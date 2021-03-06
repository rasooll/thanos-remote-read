// Binary thanos-remote-read provides an adapter from Prometheus remote read
// protocol to Thanos StoreAPI.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"sort"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/thanos-io/thanos/pkg/store/storepb"
	"google.golang.org/grpc"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
)

var (
	flagListen         = flag.String("listen", ":10080", "[ip]:port to serve HTTP on")
	flagStore          = flag.String("store", "localhost:10901", "Thanos Store API gRPC endpoint")
	flagIgnoreWarnings = flag.Bool("ignore-warnings", false, "Ignore warnings from Thanos")
)

var (
	httpRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "http",
			Name:      "requests_total",
		},
		[]string{"code", "method", "handler"})
)

func init() {
	prometheus.MustRegister(httpRequests)
}

func main() {
	flag.Parse()

	var err error
	conn, err := grpc.Dial(*flagStore, grpc.WithInsecure(),
		grpc.WithUnaryInterceptor(grpc_prometheus.UnaryClientInterceptor),
		grpc.WithStreamInterceptor(grpc_prometheus.StreamClientInterceptor))
	if err != nil {
		log.Fatal(err)
	}
	setup(conn)
	log.Fatal(http.ListenAndServe(*flagListen, nil))
}

func setup(conn *grpc.ClientConn) {
	api := &API{
		client: storepb.NewStoreClient(conn),
	}

	handler := func(path, name string, f http.HandlerFunc) {
		http.HandleFunc(path, promhttp.InstrumentHandlerCounter(
			httpRequests.MustCurryWith(prometheus.Labels{"handler": name}), f))
	}
	handler("/", "root", root)
	handler("/-/healthy", "health", ok)
	handler("/api/v1/read", "read", errorWrap(api.remoteRead))

	http.Handle("/metrics", promhttp.Handler())
}

type API struct {
	client storepb.StoreClient
}

func errorWrap(f func(w http.ResponseWriter, r *http.Request) error) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		err := f(w, r)
		if err != nil {
			if httpErr, ok := err.(HTTPError); ok {
				http.Error(w, httpErr.Error(), httpErr.Status)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
}

type HTTPError struct {
	error
	Status int
}

func (api *API) remoteRead(w http.ResponseWriter, r *http.Request) error {
	compressed, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return err
	}

	reqBuf, err := snappy.Decode(nil, compressed)
	if err != nil {
		return HTTPError{err, http.StatusBadRequest}
	}

	var req prompb.ReadRequest
	if err := proto.Unmarshal(reqBuf, &req); err != nil {
		return HTTPError{err, http.StatusBadRequest}
	}

	// Ignored selectors.
	ignoredSelector := make(map[string]struct{})
	if ignores, ok := r.URL.Query()["ignore"]; ok {
		for _, ignore := range ignores {
			ignoredSelector[ignore] = struct{}{}
		}
	}

	// This does not do streaming, at the time of writing Prometheus doesn't ask
	// for it anyway: https://github.com/prometheus/prometheus/issues/5926

	resp, err := api.doStoreRequest(r.Context(), &req, ignoredSelector)
	if err != nil {
		return err
	}

	data, err := proto.Marshal(resp)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Header().Set("Content-Encoding", "snappy")

	compressed = snappy.Encode(nil, data)
	if _, err := w.Write(compressed); err != nil {
		log.Printf("Error writing response: %v", err)
	}
	return nil
}

var promMatcherToThanos = map[prompb.LabelMatcher_Type]storepb.LabelMatcher_Type{
	prompb.LabelMatcher_EQ:  storepb.LabelMatcher_EQ,
	prompb.LabelMatcher_NEQ: storepb.LabelMatcher_NEQ,
	prompb.LabelMatcher_RE:  storepb.LabelMatcher_RE,
	prompb.LabelMatcher_NRE: storepb.LabelMatcher_NRE,
}

type AggrChunkByTimestamp []storepb.AggrChunk

func (c AggrChunkByTimestamp) Len() int           { return len(c) }
func (c AggrChunkByTimestamp) Swap(i, j int)      { c[i], c[j] = c[j], c[i] }
func (c AggrChunkByTimestamp) Less(i, j int) bool { return c[i].MinTime < c[j].MinTime }

func (api *API) doStoreRequest(ctx context.Context, req *prompb.ReadRequest, ignoredSelector map[string]struct{}) (*prompb.ReadResponse, error) {
	response := &prompb.ReadResponse{}

	for _, query := range req.Queries {
		storeReq := &storepb.SeriesRequest{
			MinTime: query.StartTimestampMs,
			MaxTime: query.EndTimestampMs,
			// Prometheus doesn't understand Thanos compaction, only ask for raw data.
			Aggregates: []storepb.Aggr{storepb.Aggr_RAW},
			Matchers:   make([]storepb.LabelMatcher, 0, len(query.Matchers)),
		}
		for _, matcher := range query.Matchers {
			if _, ok := ignoredSelector[matcher.Name]; ok {
				continue
			}
			storeReq.Matchers = append(storeReq.Matchers, storepb.LabelMatcher{
				Name:  matcher.Name,
				Type:  promMatcherToThanos[matcher.Type],
				Value: matcher.Value})
		}

		log.Printf("Thanos request: %v", storeReq)
		storeRes, err := api.client.Series(ctx, storeReq)
		if err != nil {
			return nil, err
		}

		result := &prompb.QueryResult{}
		iter := chunkenc.NewNopIterator()

		for {
			res, err := storeRes.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Printf("Error in recv from thanos: %v", err)
				return nil, err
			}

			switch r := res.GetResult().(type) {
			case *storepb.SeriesResponse_Series:
				t := &prompb.TimeSeries{}
				for _, label := range r.Series.Labels {
					t.Labels = append(t.Labels, prompb.Label{
						Name:  label.Name,
						Value: label.Value,
					})
				}

				sort.Sort(AggrChunkByTimestamp(r.Series.Chunks))
				for _, chunk := range r.Series.Chunks {
					if chunk.Raw == nil {
						// We only ask for and handle RAW
						err := fmt.Errorf("unexpectedly missing raw chunk data")
						log.Print(err)
						return nil, err
					}
					if chunk.Raw.Type != storepb.Chunk_XOR {
						err := fmt.Errorf("unexpected encoding type: %v", chunk.Raw.Type)
						log.Print(err)
						return nil, err
					}

					raw, err := chunkenc.FromData(chunkenc.EncXOR, chunk.Raw.Data)
					if err != nil {
						err := fmt.Errorf("reading chunk: %w", err)
						log.Print("Error ", err)
						return nil, err
					}

					iter = raw.Iterator(iter)
					for iter.Next() {
						ts, value := iter.At()
						t.Samples = append(t.Samples, prompb.Sample{
							Timestamp: ts,
							Value:     value,
						})
					}
				}

				result.Timeseries = append(result.Timeseries, t)

			case *storepb.SeriesResponse_Warning:
				if *flagIgnoreWarnings {
					log.Printf("Warning from thanos: %v", r)
				} else {
					return nil, HTTPError{fmt.Errorf("%v", r), http.StatusInternalServerError}
				}
			}
		}
		response.Results = append(response.Results, result)
	}
	return response, nil
}

func ok(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("ok"))
}

func root(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-type", "text/html")
	w.Write([]byte(`
	<p>thanos-remote-read adapter</p>
	<ul>
	  <li><a href="/-/healthy">/-/healthy</a>
	  <li><a href="/metrics">/metrics</a>
	  <li>/api/v1/read (point Prometheus here!)
	</ul>`))
}
