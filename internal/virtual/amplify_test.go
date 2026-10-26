package virtual

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// corpus builds an endpoint /items with n exchanges, the first errs of which
// are 503s. Bodies are "ok-<i>" and "err-<i>".
func corpus(n, errs int) []*model.Exchange {
	exs := make([]*model.Exchange, 0, n)
	for i := 1; i <= n; i++ {
		if i <= errs {
			exs = append(exs, mkEx(int64(i), "GET", "/items", 503, fmt.Sprintf("err-%d", i)))
		} else {
			exs = append(exs, mkEx(int64(i), "GET", "/items", 200, fmt.Sprintf("ok-%d", i)))
		}
	}
	return exs
}

// errorShare runs n lookups and returns the share of error responses
// (synthetic ones included), injected and synthetic counts.
func errorShare(e *Engine, target string, n int) (share float64, injected, synthetic int) {
	errs := 0
	for range n {
		m := e.Lookup(mkReq("GET", target), nil)
		if m == nil {
			panic("miss")
		}
		if m.Injected {
			injected++
		}
		if m.Synthetic {
			synthetic++
			errs++
			continue
		}
		if model.IsErrorStatus(m.Exchange.Response.Status) {
			errs++
		}
	}
	return float64(errs) / float64(n), injected, synthetic
}

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s = %.4f, want %.4f ± %.4f", name, got, want, tol)
	}
}

func TestErrorAmplificationStatistics(t *testing.T) {
	const n = 20000
	sel := []model.Selection{model.SelectRandom, model.SelectSequential}
	for _, s := range sel {
		t.Run("2% recorded x10 -> 20% "+string(s), func(t *testing.T) {
			e := newTestEngine(t, Options{Seed: 11, ErrorAmplification: 10, Match: model.MatchConfig{Selection: s}}, corpus(100, 2)...)
			share, injected, _ := errorShare(e, "/items", n)
			near(t, "error share", share, 0.20, 0.015)
			if int64(injected) != e.Stats().Injected {
				t.Fatalf("Stats.Injected = %d, counted %d", e.Stats().Injected, injected)
			}
			if injected == 0 {
				t.Fatal("nothing injected")
			}
		})
	}
	t.Run("injected errors are the recorded ones", func(t *testing.T) {
		e := newTestEngine(t, Options{Seed: 3, ErrorAmplification: 10, Match: model.MatchConfig{Selection: model.SelectRandom}}, corpus(100, 2)...)
		seen := map[string]int{}
		for range 5000 {
			m := e.Lookup(mkReq("GET", "/items"), nil)
			if m.Synthetic {
				t.Fatal("synthetic error despite recorded errors")
			}
			if model.IsErrorStatus(m.Exchange.Response.Status) {
				seen[string(m.Exchange.Response.Body)]++
			}
		}
		if len(seen) != 2 || seen["err-1"] == 0 || seen["err-2"] == 0 {
			t.Fatalf("injected errors = %v, want both recorded errors", seen)
		}
		near(t, "err-1 vs err-2 balance", float64(seen["err-1"])/float64(seen["err-1"]+seen["err-2"]), 0.5, 0.06)
	})
	t.Run("amplification 1 changes nothing", func(t *testing.T) {
		e := newTestEngine(t, Options{Seed: 1, ErrorAmplification: 1}, corpus(100, 20)...)
		share, injected, _ := errorShare(e, "/items", 1000)
		if share != 0.2 || injected != 0 {
			t.Fatalf("share=%v injected=%d, want the recorded 20%% and no injection", share, injected)
		}
	})
	t.Run("amplification 0 means as recorded", func(t *testing.T) {
		e := newTestEngine(t, Options{Seed: 1}, corpus(100, 20)...)
		share, injected, _ := errorShare(e, "/items", 1000)
		if share != 0.2 || injected != 0 {
			t.Fatalf("share=%v injected=%d", share, injected)
		}
	})
	t.Run("amplification beyond 100% is capped", func(t *testing.T) {
		e := newTestEngine(t, Options{Seed: 1, ErrorAmplification: 10}, corpus(100, 20)...)
		share, _, _ := errorShare(e, "/items", 2000)
		if share != 1 {
			t.Fatalf("share = %v, want every response an error", share)
		}
	})
	t.Run("amplification below 1 removes errors", func(t *testing.T) {
		e := newTestEngine(t, Options{Seed: 5, ErrorAmplification: 0.5, Match: model.MatchConfig{Selection: model.SelectRandom}}, corpus(100, 20)...)
		share, injected, synthetic := errorShare(e, "/items", n)
		near(t, "error share", share, 0.10, 0.012)
		if injected == 0 || synthetic != 0 {
			t.Fatalf("injected=%d synthetic=%d", injected, synthetic)
		}
		// Injected responses replaced errors by recorded successes.
		e2 := newTestEngine(t, Options{Seed: 5, ErrorAmplification: 0.001, Match: model.MatchConfig{Selection: model.SelectRandom}}, corpus(100, 20)...)
		for range 500 {
			m := e2.Lookup(mkReq("GET", "/items"), nil)
			if m.Injected && model.IsErrorStatus(m.Exchange.Response.Status) {
				t.Fatal("an injected response is an error while reducing errors")
			}
		}
	})
	t.Run("only errors recorded and amplification below 1", func(t *testing.T) {
		e := newTestEngine(t, Options{Seed: 5, ErrorAmplification: 0.1}, corpus(10, 10)...)
		share, injected, _ := errorShare(e, "/items", 500)
		if share != 1 || injected != 0 {
			t.Fatalf("share=%v injected=%d: no success exists to swap in", share, injected)
		}
	})
	t.Run("floor on an endpoint with no recorded errors serves a synthetic 503", func(t *testing.T) {
		e := newTestEngine(t, Options{Seed: 8, ErrorFloor: 0.1, Match: model.MatchConfig{Selection: model.SelectRandom}}, corpus(50, 0)...)
		share, injected, synthetic := errorShare(e, "/items", n)
		near(t, "error share", share, 0.10, 0.012)
		if injected != synthetic || synthetic == 0 {
			t.Fatalf("injected=%d synthetic=%d: every injected error must be synthetic", injected, synthetic)
		}
		if e.Stats().Injected != int64(injected) {
			t.Fatalf("Stats.Injected = %d, want %d", e.Stats().Injected, injected)
		}
	})
	t.Run("floor raises a low rate to the floor", func(t *testing.T) {
		e := newTestEngine(t, Options{Seed: 8, ErrorFloor: 0.3, Match: model.MatchConfig{Selection: model.SelectRandom}}, corpus(100, 2)...)
		share, _, _ := errorShare(e, "/items", n)
		near(t, "error share", share, 0.30, 0.015)
	})
	t.Run("floor below the amplified rate has no effect", func(t *testing.T) {
		e := newTestEngine(t, Options{Seed: 8, ErrorAmplification: 10, ErrorFloor: 0.05, Match: model.MatchConfig{Selection: model.SelectRandom}}, corpus(100, 2)...)
		share, _, _ := errorShare(e, "/items", n)
		near(t, "error share", share, 0.20, 0.015)
	})
	t.Run("floor of 1 makes everything fail", func(t *testing.T) {
		e := newTestEngine(t, Options{Seed: 8, ErrorFloor: 1}, corpus(10, 0)...)
		share, _, synthetic := errorShare(e, "/items", 300)
		if share != 1 || synthetic != 300 {
			t.Fatalf("share=%v synthetic=%d", share, synthetic)
		}
	})
}

func TestErrorAmplificationIsPerEndpoint(t *testing.T) {
	var exs []*model.Exchange
	for i := 1; i <= 100; i++ { // /a: no errors
		exs = append(exs, mkEx(int64(i), "GET", "/a", 200, "a"))
	}
	for i := 1; i <= 100; i++ { // /b: 40% errors
		st := 200
		if i <= 40 {
			st = 500
		}
		exs = append(exs, mkEx(int64(100+i), "GET", "/b", st, "b"))
	}
	e := newTestEngine(t, Options{Seed: 2, ErrorAmplification: 2, Match: model.MatchConfig{Selection: model.SelectRandom}}, exs...)
	a, _, _ := errorShare(e, "/a", 5000)
	b, _, _ := errorShare(e, "/b", 20000)
	if a != 0 {
		t.Fatalf("/a error share = %v, want 0 (0%% x 2)", a)
	}
	near(t, "/b error share", b, 0.80, 0.015)
}

func TestErrorAmplificationWithUnservableErrors(t *testing.T) {
	// Two of ten exchanges failed without a response: they count toward the
	// 20% rate but cannot be served, so amplified errors are synthetic.
	exs := corpus(8, 0)
	exs = append(exs,
		mkEx(100, "GET", "/items", 504, "", withError("timeout")),
		mkEx(101, "GET", "/items", 502, "", withError("refused")),
	)
	e := newTestEngine(t, Options{Seed: 4, ErrorAmplification: 2, Match: model.MatchConfig{Selection: model.SelectRandom}}, exs...)
	share, injected, synthetic := errorShare(e, "/items", 20000)
	// e=0.2 and t=0.4, so a matched success flips with probability
	// (0.4-0.2)/(1-0.2). Every match is a success (the failed exchanges are
	// never matched), hence 25%, not the 40% target: the formula follows the
	// contract and counts the unservable failures in e.
	near(t, "error share", share, 0.25, 0.015)
	if injected != synthetic {
		t.Fatalf("injected=%d synthetic=%d", injected, synthetic)
	}
	e2 := newTestEngine(t, Options{Seed: 4, ErrorAmplification: 1}, exs...)
	if share, injected, _ := errorShare(e2, "/items", 100); share != 0 || injected != 0 {
		t.Fatalf("unamplified: share=%v injected=%d", share, injected)
	}
}

func TestInjectedResponseWriting(t *testing.T) {
	t.Run("synthetic 503", func(t *testing.T) {
		e := newTestEngine(t, Options{ErrorFloor: 1}, mkEx(1, "GET", "/s", 200, "fine", withRespHeader("X-Recorded", "yes")))
		rec, ok := serve(e, mkReq("GET", "/s"), "")
		if !ok || rec.Code != 503 {
			t.Fatalf("ok=%v code=%d", ok, rec.Code)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] != "service unavailable (injected by mirage)" {
			t.Fatalf("body = %q (%v)", rec.Body.String(), err)
		}
		h := rec.Header()
		if h.Get(HeaderInjected) != "error" || h.Get("Content-Type") != "application/json" || h.Get(HeaderMatch) != "exact" {
			t.Fatalf("headers = %v", h)
		}
		if h.Get("X-Recorded") != "" {
			t.Fatal("recorded headers leaked into the synthetic response")
		}
		if h.Get("Content-Length") != fmt.Sprint(rec.Body.Len()) {
			t.Fatalf("Content-Length = %s", h.Get("Content-Length"))
		}
		m := e.Lookup(mkReq("GET", "/s"), nil)
		if !m.Injected || !m.Synthetic || m.Exchange != nil || m.Level != LevelExact {
			t.Fatalf("match = %+v", m)
		}
	})
	t.Run("recorded error", func(t *testing.T) {
		e := newTestEngine(t, Options{ErrorFloor: 1, Latency: model.LatencyConfig{Mode: model.LatencyRecorded}},
			mkEx(1, "GET", "/s", 200, "fine", withLatency(time.Millisecond, 2*time.Millisecond)),
			mkEx(2, "GET", "/s", 500, "boom", withRespHeader("X-Err", "1"), withLatency(30*time.Millisecond, 90*time.Millisecond)),
		)
		clk := &fakeClock{}
		clk.install(e)
		var got *Match
		for range 50 { // the sequential cursor reaches the success exchange
			m := e.Lookup(mkReq("GET", "/s"), nil)
			if !m.Synthetic && m.Exchange.Response.Status == 500 {
				got = m
				break
			}
		}
		if got == nil {
			t.Fatal("never saw the recorded error")
		}
		if !got.Injected || got.Level != LevelExact {
			t.Fatalf("match = %+v", got)
		}
		rec := httptestRecorder()
		e.Respond(rec, mkReq("GET", "/s"), got)
		if rec.Code != 500 || bodyOf(rec) != "boom" || rec.Header().Get("X-Err") != "1" {
			t.Fatalf("code=%d body=%q hdr=%v", rec.Code, bodyOf(rec), rec.Header())
		}
		if rec.Header().Get(HeaderInjected) != "error" {
			t.Fatalf("X-Mirage-Injected = %q", rec.Header().Get(HeaderInjected))
		}
		// The error's own recorded latency is simulated (30ms then 60ms).
		if w := clk.get(); len(w) != 2 || w[0] != 30*time.Millisecond || w[1] != 60*time.Millisecond {
			t.Fatalf("waits = %v", w)
		}
	})
	t.Run("synthetic keeps the matched exchange's latency", func(t *testing.T) {
		e := newTestEngine(t, Options{ErrorFloor: 1, Latency: model.LatencyConfig{Mode: model.LatencyRecorded}},
			mkEx(1, "GET", "/s", 200, "fine", withLatency(4*time.Millisecond, 10*time.Millisecond)))
		clk := &fakeClock{}
		clk.install(e)
		serve(e, mkReq("GET", "/s"), "")
		if w := clk.get(); len(w) != 2 || w[0] != 4*time.Millisecond || w[1] != 6*time.Millisecond {
			t.Fatalf("waits = %v", w)
		}
	})
	t.Run("de-amplified response is not marked as an error", func(t *testing.T) {
		e := newTestEngine(t, Options{ErrorAmplification: 0.0001, Seed: 1, Match: model.MatchConfig{Selection: model.SelectRandom}}, corpus(10, 5)...)
		for range 200 {
			rec, _ := serve(e, mkReq("GET", "/items"), "")
			if rec.Code == 200 && rec.Header().Get(HeaderInjected) != "" {
				t.Fatalf("success carries %s=%q", HeaderInjected, rec.Header().Get(HeaderInjected))
			}
		}
		if e.Stats().Injected == 0 {
			t.Fatal("swaps were not counted")
		}
	})
	t.Run("level is preserved", func(t *testing.T) {
		e := newTestEngine(t, Options{ErrorFloor: 1, Match: model.MatchConfig{PathTemplates: true}},
			mkEx(1, "GET", "/u/1", 200, "x"), mkEx(2, "GET", "/u/2", 500, "e"))
		injected := 0
		for range 20 {
			m := e.Lookup(mkReq("GET", "/u/99"), nil)
			if m.Level != LevelTemplate || m.Exchange.Response.Status != 500 {
				t.Fatalf("match = %+v", m)
			}
			if m.Injected {
				injected++
			}
		}
		// The sequential cursor alternates success and error: only the
		// successes are swapped.
		if injected != 10 {
			t.Fatalf("injected %d of 20, want 10", injected)
		}
	})
}

func TestAmplificationSeesAddedExchanges(t *testing.T) {
	e := newTestEngine(t, Options{Seed: 6, ErrorAmplification: 2, Match: model.MatchConfig{Selection: model.SelectRandom}}, corpus(20, 0)...)
	if share, _, _ := errorShare(e, "/items", 500); share != 0 {
		t.Fatalf("share = %v before any error was recorded", share)
	}
	for i := 21; i <= 40; i++ { // 20 errors: the rate becomes 50%, target 100%
		e.Add(mkEx(int64(i), "GET", "/items", 500, "late-err"))
	}
	if share, _, _ := errorShare(e, "/items", 500); share != 1 {
		t.Fatalf("share = %v after the error rate reached 50%% x2", share)
	}
}

func TestAmplificationSeededDeterminism(t *testing.T) {
	run := func(seed uint64) string {
		e := newTestEngine(t, Options{Seed: seed, ErrorAmplification: 5, Match: model.MatchConfig{Selection: model.SelectRandom}}, corpus(50, 3)...)
		var s []byte
		for range 500 {
			m := e.Lookup(mkReq("GET", "/items"), nil)
			if m.Injected {
				s = append(s, 'I')
			} else {
				s = append(s, '.')
			}
		}
		return string(s)
	}
	if a, b := run(99), run(99); a != b {
		t.Fatal("same seed, different injection pattern")
	}
	if run(99) == run(100) {
		t.Fatal("different seeds, same injection pattern")
	}
}
