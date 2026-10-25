package store

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// benchBody returns n bytes of JSON-looking, only mildly compressible text.
func benchBody(rng *rand.Rand, n int) []byte {
	var b bytes.Buffer
	b.WriteString(`{"items":[`)
	for b.Len() < n-40 {
		fmt.Fprintf(&b, `{"id":"%016x","v":%d},`, rng.Uint64(), rng.IntN(1_000_000))
	}
	b.WriteString(`{}]}`)
	return b.Bytes()[:min(b.Len(), n)]
}

// benchBatch builds a batch of n exchanges with request and response bodies
// of bodySize bytes each.
func benchBatch(n, bodySize int) []*model.Exchange {
	rng := rand.New(rand.NewPCG(1, 2))
	exs := make([]*model.Exchange, n)
	for i := range exs {
		req, resp := benchBody(rng, bodySize), benchBody(rng, bodySize)
		exs[i] = &model.Exchange{
			ServiceID: "svc_bench",
			StartedAt: baseTime.Add(time.Duration(i) * time.Millisecond),
			Duration:  model.Duration(time.Duration(5+rng.IntN(200)) * time.Millisecond),
			TTFB:      model.Duration(2 * time.Millisecond),
			Endpoint:  fmt.Sprintf("POST /v1/orders/%d/items", i%20),
			Request: model.Request{
				Method: "POST", Scheme: "https", Host: "api.example.com", Proto: "HTTP/1.1",
				Path: fmt.Sprintf("/v1/orders/%d/items", i), RawQuery: "expand=lines&limit=50",
				Header: http.Header{
					"Content-Type": {"application/json"}, "Accept": {"application/json"},
					"User-Agent": {"bench/1.0"}, "Authorization": {"Bearer h_0123456789abcdef"},
				},
				Body: req, BodySize: int64(len(req)),
			},
			Response: model.Response{
				Status: 200 + 300*(i%50/49), Proto: "HTTP/1.1",
				Header: http.Header{
					"Content-Type": {"application/json"}, "Cache-Control": {"no-store"},
					"Date": {"Sat, 14 Mar 2026 15:09:26 GMT"},
				},
				Body: resp, BodySize: int64(len(resp)),
			},
		}
	}
	return exs
}

func benchAppend(b *testing.B, s *SQLite, bodySize int) {
	rec := newRecording(b, s, "bench")
	const batch = 500
	exs := benchBatch(batch, bodySize)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.AppendExchanges(ctx(), rec.ID, exs); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N*batch)/b.Elapsed().Seconds(), "exchanges/s")
}

// BenchmarkAppendExchanges measures the hot write path in batches of 500 with
// ~1 KB request and response bodies (just under the compression threshold).
func BenchmarkAppendExchanges(b *testing.B) {
	b.Run("memory", func(b *testing.B) { benchAppend(b, openMem(b), 1000) })
	b.Run("file", func(b *testing.B) {
		s, _ := openFile(b)
		benchAppend(b, s, 1000)
	})
}

// BenchmarkAppendExchangesCompressed uses 4 KB bodies, which are gzipped.
func BenchmarkAppendExchangesCompressed(b *testing.B) {
	s, _ := openFile(b)
	benchAppend(b, s, 4096)
}

// BenchmarkAppendExchangesNoBody measures the row overhead alone.
func BenchmarkAppendExchangesNoBody(b *testing.B) {
	s, _ := openFile(b)
	benchAppend(b, s, 0)
}

// seedBench fills a recording of n exchanges (with bodies of bodySize bytes)
// in batches of 500 and returns its ID.
func seedBench(b *testing.B, s *SQLite, n, bodySize int) string {
	b.Helper()
	rec := newRecording(b, s, "seed")
	batch := benchBatch(500, bodySize)
	for done := 0; done < n; done += len(batch) {
		if err := s.AppendExchanges(ctx(), rec.ID, batch); err != nil {
			b.Fatal(err)
		}
	}
	return rec.ID
}

const benchRecordingSize = 50_000

// BenchmarkListExchanges measures list pages on a 50k-exchange recording.
func BenchmarkListExchanges(b *testing.B) {
	s, _ := openFile(b)
	rec := seedBench(b, s, benchRecordingSize, 1000)
	for _, c := range []struct {
		name string
		q    model.ExchangeQuery
	}{
		{"first page", model.ExchangeQuery{Limit: 100}},
		{"newest first", model.ExchangeQuery{Limit: 100, Desc: true}},
		{"deep offset", model.ExchangeQuery{Limit: 100, Offset: 40_000}},
		{"errors only", model.ExchangeQuery{Limit: 100, ErrorsOnly: true}},
		{"by endpoint", model.ExchangeQuery{Limit: 100, Endpoint: "POST /v1/orders/3/items"}},
		{"path search", model.ExchangeQuery{Limit: 100, PathContains: "/v1/orders/4999"}},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, _, err := s.ListExchanges(ctx(), rec, c.q); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkGetExchange measures fetching one exchange with its bodies.
func BenchmarkGetExchange(b *testing.B) {
	s, _ := openFile(b)
	rec := seedBench(b, s, 5000, 1000)
	items, _, err := s.ListExchanges(ctx(), rec, model.ExchangeQuery{Limit: 1000})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.GetExchange(ctx(), items[i%len(items)].ID); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIterateExchanges measures streaming a whole recording.
func BenchmarkIterateExchanges(b *testing.B) {
	s, _ := openFile(b)
	rec := seedBench(b, s, benchRecordingSize, 1000)
	for _, c := range []struct {
		name string
		opts model.IterOptions
	}{
		{"no bodies", model.IterOptions{}},
		{"both bodies", model.IterOptions{RequestBodies: true, ResponseBodies: true}},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			n := 0
			for i := 0; i < b.N; i++ {
				err := s.IterateExchanges(ctx(), rec, c.opts, func(*model.Exchange) error { n++; return nil })
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(n)/b.Elapsed().Seconds(), "exchanges/s")
		})
	}
}

// BenchmarkEndpointStats measures the per-endpoint aggregation of 50k exchanges.
func BenchmarkEndpointStats(b *testing.B) {
	s, _ := openFile(b)
	rec := seedBench(b, s, benchRecordingSize, 100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.EndpointStats(ctx(), rec); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAppendExchangesParallel appends from several goroutines at once;
// the single writer serialises them, so this shows the contention cost.
func BenchmarkAppendExchangesParallel(b *testing.B) {
	s, _ := openFile(b)
	recs := make([]string, 8)
	for i := range recs {
		recs[i] = newRecording(b, s, fmt.Sprint("par", i)).ID
	}
	var next atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		exs := benchBatch(100, 1000)
		rec := recs[next.Add(1)%int64(len(recs))]
		for pb.Next() {
			if err := s.AppendExchanges(ctx(), rec, exs); err != nil {
				b.Error(err)
				return
			}
		}
	})
	b.StopTimer()
	b.ReportMetric(float64(b.N*100)/b.Elapsed().Seconds(), "exchanges/s")
}
