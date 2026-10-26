package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// smallJSON is a typical small API response.
const smallJSON = `{"id":42,"name":"widget","price":12.5,"tags":["a","b","c"],"stock":{"warehouse":"eu-1","count":130}}`

// benchUpstream answers every request with smallJSON.
func benchUpstream(b testing.TB) *httptest.Server {
	b.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, smallJSON)
	}))
	b.Cleanup(srv.Close)
	return srv
}

// benchClient is a client that keeps plenty of connections alive.
func benchClient(b testing.TB) *http.Client {
	b.Helper()
	tr := &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 512, MaxIdleConns: 512}
	b.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// hammer sends b.N GET requests to url from parallel clients and reports the
// request rate.
func hammer(b *testing.B, url string) {
	b.Helper()
	c := benchClient(b)
	b.ReportAllocs()
	b.SetParallelism(4)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			res, err := c.Get(url)
			if err != nil {
				b.Error(err)
				return
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				b.Errorf("status %d", res.StatusCode)
				return
			}
		}
	})
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "req/s")
}

// BenchmarkDirect is the upstream alone: the baseline the proxy modes are
// compared with.
func BenchmarkDirect(b *testing.B) {
	hammer(b, benchUpstream(b).URL+"/users/42")
}

func BenchmarkPassthrough(b *testing.B) {
	up := benchUpstream(b)
	e := newEnv(b)
	svc := e.service(up.URL, model.ModePassthrough)
	hammer(b, e.start(svc)+"/users/42")
}

func BenchmarkRecord(b *testing.B) {
	up := benchUpstream(b)
	e := newEnv(b, func(d *Deps) { d.CaptureWorkers = 4 })
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	hammer(b, base+"/users/42")
	waitFor(b, "captures to settle", func() bool {
		s := e.mgr.Status(svc.ID)
		return s.Captured+s.CaptureDropped >= int64(b.N)
	})
	st := e.mgr.Status(svc.ID)
	b.ReportMetric(float64(st.CaptureDropped)/float64(b.N)*100, "dropped%")
}

func BenchmarkRecordAnonymized(b *testing.B) {
	up := benchUpstream(b)
	e := newEnv(b, func(d *Deps) { d.CaptureWorkers = 4 })
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.AnonymizeCapture = true })
	base := e.start(svc)
	hammer(b, base+"/users/42")
	waitFor(b, "captures to settle", func() bool {
		s := e.mgr.Status(svc.ID)
		return s.Captured+s.CaptureDropped >= int64(b.N)
	})
	st := e.mgr.Status(svc.ID)
	b.ReportMetric(float64(st.CaptureDropped)/float64(b.N)*100, "dropped%")
}

// BenchmarkRecordUnsampled is record mode with sampling so low that no
// request is captured: the cost of the decision alone.
func BenchmarkRecordUnsampled(b *testing.B) {
	up := benchUpstream(b)
	e := newEnv(b)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.CaptureSample = 1e-9 })
	hammer(b, e.start(svc)+"/users/42")
}

func BenchmarkVirtualize(b *testing.B) {
	e := newEnv(b)
	svc := e.service("", model.ModeVirtualize, func(s *model.Service) { s.Latency.Mode = model.LatencyNone })
	seedExchange(b, e, svc.RecordingID, "GET", "/users/42", 200, smallJSON)
	hammer(b, e.start(svc)+"/users/42")
}

func BenchmarkHybridHit(b *testing.B) {
	up := benchUpstream(b)
	e := newEnv(b)
	svc := e.service(up.URL, model.ModeHybrid)
	seedExchange(b, e, svc.RecordingID, "GET", "/users/42", 200, smallJSON)
	hammer(b, e.start(svc)+"/users/42")
}

// BenchmarkVirtualizeHandler measures the request path without sockets:
// metrics, fault lookup, state handling and the (fake) engine.
func BenchmarkVirtualizeHandler(b *testing.B) {
	e := newEnv(b)
	svc := e.service("", model.ModeVirtualize)
	seedExchange(b, e, svc.RecordingID, "GET", "/users/42", 200, smallJSON)
	e.start(svc)
	h, _ := e.mgr.Handler(svc.ID)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		req := httptest.NewRequest("GET", "/users/42", nil)
		for pb.Next() {
			rec := &discardWriter{}
			h.ServeHTTP(rec, req)
		}
	})
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "req/s")
}

// discardWriter is a ResponseWriter that keeps nothing.
type discardWriter struct{ hdr http.Header }

func (d *discardWriter) Header() http.Header {
	if d.hdr == nil {
		d.hdr = make(http.Header, 4)
	}
	return d.hdr
}
func (d *discardWriter) WriteHeader(int)             {}
func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// BenchmarkLatencyOverhead compares the latency of sequential requests made
// directly to the upstream and through each mode, and reports the median
// added by the proxy.
func BenchmarkLatencyOverhead(b *testing.B) {
	up := benchUpstream(b)
	e := newEnv(b)
	pass := e.service(up.URL, model.ModePassthrough)
	rec := e.service(up.URL, model.ModeRecord)
	targets := []struct {
		name, url string
	}{
		{"direct", up.URL + "/users/42"},
		{"passthrough", e.start(pass) + "/users/42"},
		{"record", e.start(rec) + "/users/42"},
	}
	c := benchClient(b)
	one := func(url string) time.Duration {
		start := time.Now()
		res, err := c.Get(url)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return time.Since(start)
	}
	for _, tg := range targets {
		for range 200 {
			one(tg.url) // warm the connections
		}
	}
	b.ReportAllocs()
	samples := make(map[string][]time.Duration)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, tg := range targets { // interleaved, so drift hits all alike
			samples[tg.name] = append(samples[tg.name], one(tg.url))
		}
	}
	b.StopTimer()
	median := func(name string) float64 {
		s := samples[name]
		slices.Sort(s)
		return float64(s[len(s)/2]) / float64(time.Microsecond)
	}
	direct := median("direct")
	b.ReportMetric(direct, "direct-p50-µs")
	b.ReportMetric(median("passthrough")-direct, "passthrough-added-p50-µs")
	b.ReportMetric(median("record")-direct, "record-added-p50-µs")
}

// ---- micro benchmarks --------------------------------------------------

func BenchmarkCapBufWrite4K(b *testing.B) {
	chunk := bytes.Repeat([]byte("x"), 4<<10)
	b.ReportAllocs()
	b.SetBytes(int64(len(chunk)) * 4)
	for i := 0; i < b.N; i++ {
		c := newCapBuf(1 << 20)
		for range 4 {
			c.write(chunk)
		}
		c.discard()
	}
}

func BenchmarkCapBufSeal(b *testing.B) {
	chunk := bytes.Repeat([]byte("x"), 2<<10)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c := newCapBuf(1 << 20)
		c.write(chunk)
		c.seal()
	}
}

func BenchmarkNarrowAcceptEncoding(b *testing.B) {
	in := []string{"gzip, deflate, br, zstd"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = narrowAcceptEncoding(in)
	}
}

func BenchmarkDecodeGzip(b *testing.B) {
	plain := []byte(strings.Repeat(smallJSON, 20))
	body := gz(b, plain)
	h := http.Header{"Content-Encoding": {"gzip"}}
	b.ReportAllocs()
	b.SetBytes(int64(len(plain)))
	for i := 0; i < b.N; i++ {
		if _, _, _, _, ok := decodeBody(h, body, int64(len(body)), false, 1<<20); !ok {
			b.Fatal("not decoded")
		}
	}
}
