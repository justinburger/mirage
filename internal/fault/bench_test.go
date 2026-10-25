package fault

import (
	"net/http"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// noMatchRules is a realistic rule set in which no rule applies to the
// benchmark request: each rule is rejected by a different condition.
func noMatchRules() []model.FaultRule {
	return []model.FaultRule{
		errRule("svc", order(1), svc("other")),
		errRule("meth", order(2), methods("DELETE")),
		errRule("glob", order(3), path("/admin/**")),
		errRule("globstar", order(4), path("/users/*/orders")),
		errRule("regex", order(5), path(`re:^/internal/\d+$`)),
		errRule("hdr", order(6), hdr("X-Chaos", "yes")),
		errRule("prob0", order(7), prob(0)),
		errRule("disabled", order(8), disabled()),
		latencyRule("lat", time.Second, time.Second, order(9), path("/slow/**")),
		throttleRule("thr", 1024, order(10), methods("PUT")),
	}
}

func BenchmarkDecideNoRules(b *testing.B) {
	inj := NewInjector()
	r := newReq("GET", "/users/42/profile")
	b.ReportAllocs()
	for b.Loop() {
		_ = inj.Decide("svc", r)
	}
}

func BenchmarkDecideNoMatch(b *testing.B) {
	inj := newInjector(b, noMatchRules()...)
	r := newReq("GET", "/users/42/profile")
	b.ReportAllocs()
	for b.Loop() {
		if inj.Decide("svc", r) != nil {
			b.Fatal("unexpected match")
		}
	}
}

func BenchmarkDecideNoMatchParallel(b *testing.B) {
	inj := newInjector(b, noMatchRules()...)
	r := newReq("GET", "/users/42/profile")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if inj.Decide("svc", r) != nil {
				b.Error("unexpected match")
				return
			}
		}
	})
}

// BenchmarkDecideMatch measures a request that gets a latency fault and an
// error fault (the common "slow and flaky" combination).
func BenchmarkDecideMatch(b *testing.B) {
	inj := newInjector(b,
		latencyRule("lat", 50*time.Millisecond, 10*time.Millisecond, order(1), path("/users/**")),
		errRule("err", order(2), path("/users/*/profile"), methods("GET")),
	)
	r := newReq("GET", "/users/42/profile")
	b.ReportAllocs()
	for b.Loop() {
		if inj.Decide("svc", r) == nil {
			b.Fatal("expected a plan")
		}
	}
}

func BenchmarkDecideMatchParallel(b *testing.B) {
	inj := newInjector(b,
		latencyRule("lat", 50*time.Millisecond, 10*time.Millisecond, order(1), path("/users/**")),
		errRule("err", order(2), path("/users/*/profile"), methods("GET")),
	)
	r := newReq("GET", "/users/42/profile")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if inj.Decide("svc", r) == nil {
				b.Error("expected a plan")
				return
			}
		}
	})
}

// BenchmarkDecideProbabilistic is the performance-test case: a 10% error rule
// on a busy path, so most requests fire nothing.
func BenchmarkDecideProbabilistic(b *testing.B) {
	inj := newInjector(b, errRule("err", prob(0.1), path("/checkout/**"), methods("POST")))
	r := newReq("POST", "/checkout/cart/1")
	b.ReportAllocs()
	for b.Loop() {
		_ = inj.Decide("svc", r)
	}
}

func BenchmarkGlobMatch(b *testing.B) {
	g, _ := compileGlob("/api/*/users/**/orders")
	const path = "/api/v2/users/42/accounts/7/orders"
	b.ReportAllocs()
	for b.Loop() {
		if !g.match(path) {
			b.Fatal("expected a match")
		}
	}
}

func BenchmarkGlobMatchMiss(b *testing.B) {
	g, _ := compileGlob("/api/*/users/**/orders")
	const path = "/api/v2/users/42/accounts/7/invoices"
	b.ReportAllocs()
	for b.Loop() {
		if g.match(path) {
			b.Fatal("unexpected match")
		}
	}
}

// discardWriter is a ResponseWriter that throws everything away.
type discardWriter struct{ h http.Header }

func (d *discardWriter) Header() http.Header         { return d.h }
func (d *discardWriter) WriteHeader(int)             {}
func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// BenchmarkApplyError measures rendering an injected error response. The
// header map is cleared between iterations to mimic a fresh response.
func BenchmarkApplyError(b *testing.B) {
	inj := newInjector(b, errRule("err", func(r *model.FaultRule) {
		r.Body = `{"error":"overloaded"}`
		r.ResponseHeader = map[string]string{"Retry-After": "1"}
	}))
	r := newReq("GET", "/x")
	plan := inj.Decide("svc", r)
	w := &discardWriter{h: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		if !plan.Apply(w, r) {
			b.Fatal("expected handled")
		}
	}
}

// BenchmarkThrottleWriter measures the per-Write overhead of the throttle at a
// rate high enough that it never sleeps.
func BenchmarkThrottleWriter(b *testing.B) {
	w := (&Plan{BytesPerSecond: 1 << 40}).WrapWriter(&discardWriter{h: http.Header{}})
	data := make([]byte, 1024)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if _, err := w.Write(data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSetRules(b *testing.B) {
	rules := noMatchRules()
	inj := NewInjector()
	b.ReportAllocs()
	for b.Loop() {
		if err := inj.SetRules(rules); err != nil {
			b.Fatal(err)
		}
	}
}
