package virtual

import (
	"context"
	"fmt"
	"runtime"
	"testing"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
)

// ---- template matching survives a different normalizer --------------------

func TestTemplateLevelFollowsTheCurrentEndpointPatterns(t *testing.T) {
	tmpl := model.MatchConfig{PathTemplates: true}
	t.Run("renamed placeholder", func(t *testing.T) {
		// Recorded under pathnorm.Default ("GET /v1/users/{id}"), served with
		// the pattern /v1/users/{userId}.
		n, err := pathnorm.New([]string{"/v1/users/{userId}"})
		if err != nil {
			t.Fatal(err)
		}
		ex := mkEx(1, "GET", "/v1/users/42", 200, "U")
		if ex.Endpoint != "GET /v1/users/{id}" {
			t.Fatalf("setup: stored endpoint %q", ex.Endpoint)
		}
		e := newTestEngine(t, Options{Match: tmpl, Normalizer: n}, ex)
		if l, b := lookup(e, "GET", "/v1/users/99", ""); l != LevelTemplate || b != "U" {
			t.Fatalf("got (%s,%q), want a template hit", l, b)
		}
	})
	t.Run("pattern added after recording", func(t *testing.T) {
		// Default leaves cus_abc123xyz literal; the docs' pattern covers it.
		n, err := pathnorm.New([]string{"/v1/customers/{id}"})
		if err != nil {
			t.Fatal(err)
		}
		ex := mkEx(1, "GET", "/v1/customers/cus_abc123xyz", 200, "C")
		e := newTestEngine(t, Options{Match: tmpl, Normalizer: n}, ex)
		if l, b := lookup(e, "GET", "/v1/customers/cus_other456", ""); l != LevelTemplate || b != "C" {
			t.Fatalf("got (%s,%q), want a template hit", l, b)
		}
	})
	t.Run("the same normalizer still matches", func(t *testing.T) {
		e := newTestEngine(t, Options{Match: tmpl}, mkEx(1, "GET", "/users/42", 200, "U"))
		if l, _ := lookup(e, "GET", "/users/43", ""); l != LevelTemplate {
			t.Fatalf("got %s", l)
		}
	})
	t.Run("a stored endpoint from the current patterns stays authoritative", func(t *testing.T) {
		n, err := pathnorm.New([]string{"/custom/{thing}"})
		if err != nil {
			t.Fatal(err)
		}
		ex := mkEx(1, "GET", "/users/h_3f9a1c0b7d2e4a51/orders", 200, "X")
		ex.Endpoint = "GET /custom/{thing}"
		e := newTestEngine(t, Options{Match: tmpl, Normalizer: n}, ex)
		if l, _ := lookup(e, "GET", "/custom/1", ""); l != LevelTemplate {
			t.Fatalf("stored endpoint not used: %s", l)
		}
		if l, _ := lookup(e, "GET", "/users/55/orders", ""); l != LevelMiss {
			t.Fatalf("computed endpoint used instead of the stored one: %s", l)
		}
	})
	t.Run("methods stay apart", func(t *testing.T) {
		n, _ := pathnorm.New([]string{"/v1/users/{userId}"})
		e := newTestEngine(t, Options{Match: tmpl, Normalizer: n}, mkEx(1, "GET", "/v1/users/42", 200, "U"))
		if l, _ := lookup(e, "POST", "/v1/users/99", ""); l != LevelMiss {
			t.Fatalf("got %s", l)
		}
	})
}

// ---- Stats.Injected counts injected errors only ----------------------------

func TestInjectedStatMatchesTheHeader(t *testing.T) {
	count := func(e *Engine, n int) (withHeader int) {
		for range n {
			rec, ok := serve(e, mkReq("GET", "/items"), "")
			if !ok {
				t.Fatal("miss")
			}
			if rec.Header().Get(HeaderInjected) == "error" {
				withHeader++
			}
		}
		return withHeader
	}
	t.Run("de-amplification injects nothing", func(t *testing.T) {
		e := newTestEngine(t, Options{Seed: 4, ErrorAmplification: 0.1, Match: model.MatchConfig{Selection: model.SelectRandom}}, corpus(100, 50)...)
		withHeader := count(e, 2000)
		if withHeader != 0 || e.Stats().Injected != 0 {
			t.Fatalf("header on %d responses, Stats.Injected = %d; no error was injected", withHeader, e.Stats().Injected)
		}
		if e.Suppressed() == 0 {
			t.Fatal("the error-to-success swaps are not counted anywhere")
		}
	})
	t.Run("amplification counts the errors it injects", func(t *testing.T) {
		e := newTestEngine(t, Options{Seed: 4, ErrorAmplification: 5, Match: model.MatchConfig{Selection: model.SelectRandom}}, corpus(100, 10)...)
		withHeader := count(e, 2000)
		if withHeader == 0 || int64(withHeader) != e.Stats().Injected {
			t.Fatalf("header on %d responses, Stats.Injected = %d", withHeader, e.Stats().Injected)
		}
		if e.Suppressed() != 0 {
			t.Fatalf("Suppressed = %d with amplification above 1", e.Suppressed())
		}
	})
	t.Run("synthetic errors count", func(t *testing.T) {
		e := newTestEngine(t, Options{Seed: 8, ErrorFloor: 0.2, Match: model.MatchConfig{Selection: model.SelectRandom}}, corpus(50, 0)...)
		withHeader := count(e, 2000)
		if withHeader == 0 || int64(withHeader) != e.Stats().Injected {
			t.Fatalf("header on %d responses, Stats.Injected = %d", withHeader, e.Stats().Injected)
		}
	})
}

// ---- Add ignores what Build already indexed ---------------------------------

func TestAddIgnoresExchangesTheBuildRead(t *testing.T) {
	st := openStore(t)
	rec := fill(t, st, mkEx(1, "GET", "/a", 200, "A"), mkEx(2, "GET", "/b", 200, "B"), mkEx(3, "GET", "/c", 200, "C"))
	e := build(t, st, rec, Options{})
	if e.Len() != 3 {
		t.Fatalf("Len = %d", e.Len())
	}
	// The same stored exchange arrives again (a capture flushed while the
	// engine was being built).
	var stored []*model.Exchange
	if err := st.IterateExchanges(context.Background(), rec.ID, model.IterOptions{ResponseBodies: true}, func(ex *model.Exchange) error {
		stored = append(stored, ex.Clone())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, ex := range stored {
		e.Add(ex)
	}
	if e.Len() != 3 {
		t.Fatalf("Len = %d after re-adding what Build read; want 3", e.Len())
	}
	// A capture stored after the build (higher ID) is added; so is an
	// exchange without an ID (never stored).
	later := mkEx(10, "GET", "/d", 200, "D")
	later.ID = stored[len(stored)-1].ID + 1
	e.Add(later)
	noID := mkEx(11, "GET", "/e", 200, "E")
	noID.ID = 0
	e.Add(noID)
	if e.Len() != 5 {
		t.Fatalf("Len = %d, want 5", e.Len())
	}
	if l, b := lookup(e, "GET", "/d", ""); l != LevelExact || b != "D" {
		t.Errorf("/d: (%s,%q)", l, b)
	}
}

// ---- the index keeps a slim exchange ------------------------------------------

func TestIndexedExchangeIsSlimAndShared(t *testing.T) {
	mk := func(seq int64, target string) *model.Exchange {
		return mkEx(seq, "GET", target, 200, "body",
			withReqHeader("Accept", "application/json"), withReqHeader("User-Agent", "load-test"),
			withReqBody("{}"),
			withRespHeader("Cache-Control", "no-store"), withRespHeader("X-Served-By", "edge-1"))
	}
	a, b := mk(1, "/a"), mk(2, "/b")
	e := newTestEngine(t, Options{Match: model.MatchConfig{Body: model.BodyExact}}, a, b)

	ma, mb := e.Lookup(mkReq("GET", "/a"), []byte("{}")), e.Lookup(mkReq("GET", "/b"), []byte("{}"))
	if ma == nil || mb == nil {
		t.Fatal("miss")
	}
	if ma.Exchange.Request.Header != nil || ma.Exchange.Request.Body != nil || ma.Exchange.Response.Header != nil {
		t.Errorf("indexed exchange keeps request headers/body or response headers: %+v", ma.Exchange)
	}
	if string(ma.Exchange.Response.Body) != "body" || ma.Exchange.Response.Status != 200 || ma.Exchange.Seq != 1 {
		t.Errorf("slim exchange lost what Match promises: %+v", ma.Exchange)
	}
	// The caller's exchange is not touched.
	if a.Request.Header == nil || a.Response.Header == nil || a.Request.Body == nil {
		t.Error("Add modified the exchange it was given")
	}
	// Identical header sets are held once.
	if len(ma.it.hdrs) == 0 || &ma.it.hdrs[0] != &mb.it.hdrs[0] {
		t.Error("identical response header sets are not shared")
	}
	// Responses still carry their recorded headers.
	rec, ok := serve(e, mkReq("GET", "/a"), "{}")
	if !ok || rec.Header().Get("X-Served-By") != "edge-1" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("served headers: %v", rec.Header())
	}
}

func TestHeaderInterningIsBounded(t *testing.T) {
	e := NewEngine(nil, noLatency(Options{}))
	t.Cleanup(func() { _ = e.Close() })
	for i := 0; i < 3*maxInternedHeaderSets; i++ {
		e.Add(mkEx(int64(i+1), "GET", fmt.Sprintf("/p/%d", i), 200, "x", withRespHeader("X-Request-Id", fmt.Sprint(i))))
	}
	if n := len(e.interned); n > maxInternedHeaderSets {
		t.Fatalf("%d interned header sets, limit %d", n, maxInternedHeaderSets)
	}
}

func TestIndexMemoryPerExchange(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a large index")
	}
	const n = 20000
	e := NewEngine(nil, noLatency(Options{}))
	t.Cleanup(func() { _ = e.Close() })
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < n; i++ {
		// The exchange is garbage once handed over; the index keeps its own.
		e.Add(mkEx(int64(i+1), "GET", fmt.Sprintf("/api/users/%d", i), 200, `{"id":1,"name":"Ava Johnson"}`,
			withReqHeader("Accept", "application/json"), withReqHeader("User-Agent", "load-test/1.0"),
			withReqHeader("Accept-Encoding", "gzip"), withReqHeader("X-Request-Id", fmt.Sprint("req-", i)),
			withReqHeader("Authorization", "Bearer abcdef"), withReqHeader("Accept-Language", "en"),
			withRespHeader("Cache-Control", "no-store"), withRespHeader("X-Served-By", "edge-1"),
			withRespHeader("Server", "demo"), withRespHeader("Vary", "Accept"),
			withRespHeader("X-Frame-Options", "DENY"), withRespHeader("X-Content-Type-Options", "nosniff"),
			withRespHeader("Strict-Transport-Security", "max-age=63072000")))
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	per := (int64(after.HeapAlloc) - int64(before.HeapAlloc)) / n
	t.Logf("heap per indexed exchange: %d bytes", per)
	if per > 1500 {
		t.Errorf("%d bytes of heap per indexed exchange, want at most 1500", per)
	}
	if e.Len() != n {
		t.Fatalf("Len = %d", e.Len())
	}
}
