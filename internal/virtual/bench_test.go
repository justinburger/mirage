package virtual

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

const (
	benchUsers   = 2000
	benchPerUser = 5 // recorded variants (query strings) per user
)

// benchEngine indexes a realistic corpus: benchUsers users, each recorded
// under benchPerUser query variants, plus POSTs with JSON bodies.
func benchEngine(b testing.TB, sel model.Selection) *Engine {
	b.Helper()
	e := NewEngine(nil, Options{
		Latency: model.LatencyConfig{Mode: model.LatencyNone},
		Match:   model.MatchConfig{PathTemplates: true, Selection: sel, IgnoreParams: []string{"_ts"}},
	})
	seq := int64(0)
	for u := range benchUsers {
		for v := range benchPerUser {
			seq++
			ex := mkEx(seq, "GET", fmt.Sprintf("/api/v1/users/%d?expand=orders&v=%d", 1000+u, v), 200,
				`{"id":123,"name":"Jane Doe","email":"jane@example.com","roles":["a","b","c"]}`,
				withRespHeader("X-Request-Id", "abc"), withRespHeader("Cache-Control", "no-store"))
			e.Add(ex)
		}
		seq++
		e.Add(mkEx(seq, "POST", fmt.Sprintf("/api/v1/users/%d/orders", 1000+u), 201, `{"ok":true}`,
			withReqBody(fmt.Sprintf(`{"item":%d,"qty":2,"tags":["x","y"],"meta":{"a":1,"b":2}}`, u))))
	}
	b.Cleanup(func() { _ = e.Close() })
	return e
}

func benchLookup(b *testing.B, e *Engine, target, method string, body []byte, wantLevel string) {
	r := httptest.NewRequest(method, target, nil)
	if m := e.Lookup(r, body); m == nil || m.Level != wantLevel {
		b.Fatalf("setup: lookup %s = %+v, want level %s", target, m, wantLevel)
	}
	b.ReportAllocs()
	b.ResetTimer()
	var m Match
	for i := 0; i < b.N; i++ {
		if !e.lookup(r, body, &m) {
			b.Fatal("miss")
		}
	}
}

func BenchmarkLookupExactHit(b *testing.B) {
	e := benchEngine(b, model.SelectSequential)
	benchLookup(b, e, "/api/v1/users/1500?expand=orders&v=2", "GET", nil, LevelExact)
}

func BenchmarkLookupExactHitIgnoredParam(b *testing.B) {
	e := benchEngine(b, model.SelectSequential)
	benchLookup(b, e, "/api/v1/users/1500?_ts=1700000000&v=2&expand=orders", "GET", nil, LevelExact)
}

func BenchmarkLookupExactHitJSONBody(b *testing.B) {
	e := benchEngine(b, model.SelectSequential)
	// Same document as the recording for user 500, reordered and spaced out.
	body := []byte(`{ "meta": {"b": 2, "a": 1}, "tags": ["x", "y"], "qty": 2, "item": 500 }`)
	benchLookup(b, e, "/api/v1/users/1500/orders", "POST", body, LevelExact)
}

func BenchmarkLookupPathFallback(b *testing.B) {
	e := benchEngine(b, model.SelectSequential)
	benchLookup(b, e, "/api/v1/users/1500?expand=other", "GET", nil, LevelPath)
}

func BenchmarkLookupTemplateFallback(b *testing.B) {
	e := benchEngine(b, model.SelectSequential)
	benchLookup(b, e, "/api/v1/users/987654", "GET", nil, LevelTemplate)
}

func BenchmarkLookupMiss(b *testing.B) {
	e := benchEngine(b, model.SelectSequential)
	r := httptest.NewRequest("GET", "/nothing/here", nil)
	b.ReportAllocs()
	b.ResetTimer()
	var m Match
	for i := 0; i < b.N; i++ {
		if e.lookup(r, nil, &m) {
			b.Fatal("hit")
		}
	}
}

// BenchmarkLookupPublic includes the allocation of the returned *Match.
func BenchmarkLookupPublic(b *testing.B) {
	e := benchEngine(b, model.SelectSequential)
	r := httptest.NewRequest("GET", "/api/v1/users/1500?expand=orders&v=2", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if e.Lookup(r, nil) == nil {
			b.Fatal("miss")
		}
	}
}

func BenchmarkLookupSubset(b *testing.B) {
	e := NewEngine(nil, Options{Match: model.MatchConfig{Query: model.QuerySubset, Body: model.BodyIgnore}})
	for i := 1; i <= 200; i++ {
		e.Add(mkEx(int64(i), "GET", fmt.Sprintf("/s/%d?a=1", i%20), 200, "x"))
	}
	r := httptest.NewRequest("GET", "/s/3?a=1&b=2&c=3", nil)
	b.ReportAllocs()
	b.ResetTimer()
	var m Match
	for i := 0; i < b.N; i++ {
		if !e.lookup(r, nil, &m) {
			b.Fatal("miss")
		}
	}
}

func BenchmarkLookupWithAmplification(b *testing.B) {
	e := NewEngine(nil, Options{ErrorAmplification: 10, Latency: model.LatencyConfig{Mode: model.LatencyNone}})
	for i := 1; i <= 100; i++ {
		st := 200
		if i <= 2 {
			st = 503
		}
		e.Add(mkEx(int64(i), "GET", "/items", st, "x"))
	}
	r := httptest.NewRequest("GET", "/items", nil)
	b.ReportAllocs()
	b.ResetTimer()
	var m Match
	for i := 0; i < b.N; i++ {
		if !e.lookup(r, nil, &m) {
			b.Fatal("miss")
		}
	}
}

// BenchmarkLookupParallel spreads lookups over many keys from all CPUs; the
// per-op time should stay close to the single-goroutine figure (no global
// lock). BenchmarkLookupParallelHotKey hammers a single key, which still
// contends on that key's shard lock and cursor.
func BenchmarkLookupParallel(b *testing.B) {
	e := benchEngine(b, model.SelectSequential)
	const nreq = 4096
	reqs := make([]*http.Request, nreq)
	for i := range reqs {
		switch i % 3 {
		case 0:
			reqs[i] = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/users/%d?expand=orders&v=%d", 1000+i%benchUsers, i%benchPerUser), nil)
		case 1:
			reqs[i] = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/users/%d?expand=none", 1000+i%benchUsers), nil)
		default:
			reqs[i] = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/users/%d", 50000+i), nil)
		}
	}
	var next atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := int(next.Add(977)) // start each goroutine somewhere else
		var m Match
		for pb.Next() {
			i++
			if !e.lookup(reqs[i%nreq], nil, &m) {
				b.Error("miss")
				return
			}
		}
	})
}

func BenchmarkLookupParallelHotKey(b *testing.B) {
	e := benchEngine(b, model.SelectSequential)
	r := httptest.NewRequest("GET", "/api/v1/users/1500?expand=orders&v=2", nil)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var m Match
		for pb.Next() {
			if !e.lookup(r, nil, &m) {
				b.Error("miss")
				return
			}
		}
	})
}

// BenchmarkLookupParallelWithAdds measures readers while a writer keeps adding.
func BenchmarkLookupParallelWithAdds(b *testing.B) {
	e := benchEngine(b, model.SelectSequential)
	r := httptest.NewRequest("GET", "/api/v1/users/1500?expand=orders&v=2", nil)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := int64(1_000_000); ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			e.Add(mkEx(i, "GET", fmt.Sprintf("/added/%d", i%5000), 200, "x"))
		}
	}()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var m Match
		for pb.Next() {
			if !e.lookup(r, nil, &m) {
				b.Error("miss")
				return
			}
		}
	})
	b.StopTimer()
	close(stop)
	<-done
}

// nopWriter is a ResponseWriter that discards everything and reuses its
// header map, so the benchmark shows the engine's own allocations.
type nopWriter struct{ h http.Header }

func (w *nopWriter) Header() http.Header         { return w.h }
func (w *nopWriter) WriteHeader(int)             {}
func (w *nopWriter) Write(b []byte) (int, error) { return len(b), nil }

// BenchmarkServe is end to end into a fresh httptest.ResponseRecorder per
// iteration (the recorder's own allocations dominate).
func BenchmarkServe(b *testing.B) {
	e := benchEngine(b, model.SelectSequential)
	r := httptest.NewRequest("GET", "/api/v1/users/1500?expand=orders&v=2", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		if !e.Serve(rec, r, nil) {
			b.Fatal("miss")
		}
	}
}

// BenchmarkServeNopWriter is Serve into a writer that allocates nothing.
func BenchmarkServeNopWriter(b *testing.B) {
	e := benchEngine(b, model.SelectSequential)
	r := httptest.NewRequest("GET", "/api/v1/users/1500?expand=orders&v=2", nil)
	w := &nopWriter{h: make(http.Header, 16)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clear(w.h)
		if !e.Serve(w, r, nil) {
			b.Fatal("miss")
		}
	}
}

func BenchmarkServeParallel(b *testing.B) {
	e := benchEngine(b, model.SelectSequential)
	var next atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := int(next.Add(977))
		w := &nopWriter{h: make(http.Header, 16)}
		reqs := make([]*http.Request, 64)
		for k := range reqs {
			reqs[k] = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/users/%d?expand=orders&v=%d", 1000+(i+k*31)%benchUsers, k%benchPerUser), nil)
		}
		for pb.Next() {
			i++
			clear(w.h)
			if !e.Serve(w, reqs[i%len(reqs)], nil) {
				b.Error("miss")
				return
			}
		}
	})
}

func BenchmarkServeGzipCached(b *testing.B) {
	e := NewEngine(nil, Options{Latency: model.LatencyConfig{Mode: model.LatencyNone}})
	e.Add(mkEx(1, "GET", "/z", 200, fmt.Sprintf("%02000d", 7), withGzip()))
	r := httptest.NewRequest("GET", "/z", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := &nopWriter{h: make(http.Header, 16)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clear(w.h)
		e.Serve(w, r, nil)
	}
}

func BenchmarkWriteMiss(b *testing.B) {
	e := NewEngine(nil, Options{RecordingID: "rec_x"})
	r := httptest.NewRequest("GET", "/missing/path", nil)
	w := &nopWriter{h: make(http.Header, 16)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clear(w.h)
		e.WriteMiss(w, r)
	}
}

func BenchmarkAdd(b *testing.B) {
	e := NewEngine(nil, Options{Match: model.MatchConfig{PathTemplates: true}})
	exs := make([]*model.Exchange, b.N)
	for i := range exs {
		exs[i] = mkEx(int64(i+1), "GET", fmt.Sprintf("/api/v1/users/%d?x=%d", i%5000, i%7), 200, `{"ok":true}`)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Add(exs[i])
	}
}

func BenchmarkJSONBodyKey(b *testing.B) {
	e := NewEngine(nil, Options{})
	body := []byte(`{"item":42,"qty":2,"tags":["x","y","z"],"meta":{"a":1,"b":2,"c":{"d":[1,2,3],"e":null}},"name":"Jane Doe","active":true}`)
	var buf [64]byte
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = e.appendBodyKey(buf[:0], body)
	}
}
