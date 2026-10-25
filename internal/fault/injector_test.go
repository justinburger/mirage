package fault

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

func TestDecideWithoutRules(t *testing.T) {
	r := newReq("GET", "/x")
	var nilInj *Injector
	var zero Injector
	for name, inj := range map[string]*Injector{
		"nil injector":   nilInj,
		"zero injector":  &zero,
		"new injector":   NewInjector(),
		"empty rule set": newInjector(t),
	} {
		if plan := inj.Decide("svc", r); plan != nil {
			t.Errorf("%s: Decide = %+v, want nil", name, plan)
		}
		if got := inj.Counts(); got == nil || len(got) != 0 {
			t.Errorf("%s: Counts = %v, want empty non-nil map", name, got)
		}
		if got := inj.Rules(); got == nil || len(got) != 0 {
			t.Errorf("%s: Rules = %v, want empty non-nil slice", name, got)
		}
	}
	if plan := newInjector(t, errRule("a")).Decide("svc", nil); plan != nil {
		t.Errorf("Decide(nil request) = %+v, want nil", plan)
	}
}

func TestDecideMatching(t *testing.T) {
	tests := []struct {
		name    string
		rule    model.FaultRule
		service string
		req     *requestSpec
		want    bool
	}{
		{name: "no conditions", rule: errRule("a"), service: "svc", req: get("/x"), want: true},

		// Service scoping.
		{name: "service match", rule: errRule("a", svc("svc")), service: "svc", req: get("/x"), want: true},
		{name: "service mismatch", rule: errRule("a", svc("other")), service: "svc", req: get("/x"), want: false},
		{name: "empty rule service matches all", rule: errRule("a"), service: "anything", req: get("/x"), want: true},
		{name: "scoped rule vs empty service id", rule: errRule("a", svc("svc")), service: "", req: get("/x"), want: false},

		// Methods are case-insensitive on both sides.
		{name: "method match", rule: errRule("a", methods("POST")), service: "s", req: &requestSpec{"POST", "/x", nil}, want: true},
		{name: "method lower-case rule", rule: errRule("a", methods("post")), service: "s", req: &requestSpec{"POST", "/x", nil}, want: true},
		{name: "method mixed-case rule", rule: errRule("a", methods("PoSt")), service: "s", req: &requestSpec{"POST", "/x", nil}, want: true},
		{name: "method in list", rule: errRule("a", methods("GET", "put")), service: "s", req: &requestSpec{"PUT", "/x", nil}, want: true},
		{name: "method not in list", rule: errRule("a", methods("GET", "PUT")), service: "s", req: &requestSpec{"DELETE", "/x", nil}, want: false},
		{name: "method prefix is not a match", rule: errRule("a", methods("GE")), service: "s", req: get("/x"), want: false},

		// Path globs.
		{name: "glob exact", rule: errRule("a", path("/users")), service: "s", req: get("/users"), want: true},
		{name: "glob exact miss", rule: errRule("a", path("/users")), service: "s", req: get("/orders"), want: false},
		{name: "glob star", rule: errRule("a", path("/users/*/orders")), service: "s", req: get("/users/7/orders"), want: true},
		{name: "glob star miss", rule: errRule("a", path("/users/*/orders")), service: "s", req: get("/users/7/8/orders"), want: false},
		{name: "glob double star", rule: errRule("a", path("/api/**")), service: "s", req: get("/api/v1/users/7"), want: true},
		{name: "glob double star miss", rule: errRule("a", path("/api/**")), service: "s", req: get("/web/x"), want: false},
		{name: "glob ignores query string", rule: errRule("a", path("/users")), service: "s", req: get("/users?id=1&x=/orders"), want: true},
		{name: "glob matches decoded path", rule: errRule("a", path("/a b")), service: "s", req: get("/a%20b"), want: true},
		{name: "glob match everything", rule: errRule("a", path("/**")), service: "s", req: get("/anything/at/all"), want: true},

		// Regular expressions.
		{name: "regex anchored", rule: errRule("a", path(`re:^/users/\d+$`)), service: "s", req: get("/users/42"), want: true},
		{name: "regex anchored miss", rule: errRule("a", path(`re:^/users/\d+$`)), service: "s", req: get("/users/abc"), want: false},
		{name: "regex anchored trailing", rule: errRule("a", path(`re:^/users/\d+$`)), service: "s", req: get("/users/42/x"), want: false},
		{name: "regex unanchored", rule: errRule("a", path(`re:orders`)), service: "s", req: get("/x/orders/1"), want: true},
		{name: "regex case-insensitive flag", rule: errRule("a", path(`re:(?i)^/users$`)), service: "s", req: get("/USERS"), want: true},
		{name: "regex alternation", rule: errRule("a", path(`re:^/(a|b)$`)), service: "s", req: get("/b"), want: true},
		{name: "regex not matched against query", rule: errRule("a", path(`re:orders`)), service: "s", req: get("/users?x=orders"), want: false},
		{name: "empty regex matches all", rule: errRule("a", path(`re:`)), service: "s", req: get("/q"), want: true},

		// Headers: name case-insensitive, value exact.
		{name: "header match", rule: errRule("a", hdr("X-Tenant", "acme")), service: "s", req: get("/x", "X-Tenant", "acme"), want: true},
		{name: "header name case-insensitive", rule: errRule("a", hdr("x-tenant", "acme")), service: "s", req: get("/x", "X-TENANT", "acme"), want: true},
		{name: "header value case-sensitive", rule: errRule("a", hdr("X-Tenant", "acme")), service: "s", req: get("/x", "X-Tenant", "ACME"), want: false},
		{name: "header value is exact not substring", rule: errRule("a", hdr("X-Tenant", "acme")), service: "s", req: get("/x", "X-Tenant", "acme-2"), want: false},
		{name: "header absent", rule: errRule("a", hdr("X-Tenant", "acme")), service: "s", req: get("/x"), want: false},
		{name: "repeated header any value", rule: errRule("a", hdr("X-Tag", "b")), service: "s", req: get("/x", "X-Tag", "a", "X-Tag", "b"), want: true},
		{name: "empty value requires presence", rule: errRule("a", hdr("X-Flag", "")), service: "s", req: get("/x", "X-Flag", ""), want: true},
		{name: "empty value but header absent", rule: errRule("a", hdr("X-Flag", "")), service: "s", req: get("/x"), want: false},
		{name: "all headers required", rule: errRule("a", hdr("A", "1"), hdr("B", "2")), service: "s", req: get("/x", "A", "1"), want: false},
		{name: "all headers present", rule: errRule("a", hdr("A", "1"), hdr("B", "2")), service: "s", req: get("/x", "A", "1", "B", "2"), want: true},
		{name: "Host pseudo-header", rule: errRule("a", hdr("Host", "API.example.com")), service: "s", req: &requestSpec{"GET", "http://api.example.com/x", nil}, want: true},
		{name: "Host pseudo-header miss", rule: errRule("a", hdr("host", "other.example.com")), service: "s", req: &requestSpec{"GET", "http://api.example.com/x", nil}, want: false},

		// Everything together.
		{
			name:    "all conditions satisfied",
			rule:    errRule("a", svc("svc"), methods("POST"), path("/checkout/**"), hdr("X-Env", "perf")),
			service: "svc",
			req:     &requestSpec{"POST", "/checkout/cart/1", []string{"X-Env", "perf"}},
			want:    true,
		},
		{
			name:    "one condition fails",
			rule:    errRule("a", svc("svc"), methods("POST"), path("/checkout/**"), hdr("X-Env", "perf")),
			service: "svc",
			req:     &requestSpec{"POST", "/checkout/cart/1", []string{"X-Env", "prod"}},
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inj := newInjector(t, tt.rule)
			plan := inj.Decide(tt.service, tt.req.build())
			if got := plan != nil; got != tt.want {
				t.Fatalf("Decide fired = %v, want %v (plan %+v)", got, tt.want, plan)
			}
			wantCount := int64(0)
			if tt.want {
				wantCount = 1
			}
			if got := inj.Counts()["a"]; got != wantCount {
				t.Errorf("Counts[a] = %d, want %d", got, wantCount)
			}
		})
	}
}

// requestSpec describes a request declaratively so test tables stay readable.
type requestSpec struct {
	method string
	target string
	header []string
}

func get(target string, headerKV ...string) *requestSpec {
	return &requestSpec{method: "GET", target: target, header: headerKV}
}

func (s *requestSpec) build() *http.Request { return newReq(s.method, s.target, s.header...) }

// Rule modifiers.
func svc(id string) func(*model.FaultRule)       { return func(r *model.FaultRule) { r.ServiceID = id } }
func methods(m ...string) func(*model.FaultRule) { return func(r *model.FaultRule) { r.Methods = m } }
func path(p string) func(*model.FaultRule)       { return func(r *model.FaultRule) { r.PathPattern = p } }
func prob(p float64) func(*model.FaultRule)      { return func(r *model.FaultRule) { r.Probability = p } }
func order(o int) func(*model.FaultRule)         { return func(r *model.FaultRule) { r.Order = o } }
func name(n string) func(*model.FaultRule)       { return func(r *model.FaultRule) { r.Name = n } }
func disabled() func(*model.FaultRule)           { return func(r *model.FaultRule) { r.Enabled = false } }
func hdr(k, v string) func(*model.FaultRule) {
	return func(r *model.FaultRule) {
		if r.Headers == nil {
			r.Headers = map[string]string{}
		}
		r.Headers[k] = v
	}
}

func TestDecideProbability(t *testing.T) {
	const trials = 10000
	tests := []struct {
		p        float64
		min, max int
	}{
		{0, 0, 0},
		{1, trials, trials},
		// Binomial(10000, p): sd = 30 at p=0.1, 50 at p=0.5, 31 at p=0.9.
		// The windows are +-5 sd, so a false failure is a ~1e-6 event.
		{0.1, 850, 1150},
		{0.5, 4750, 5250},
		{0.9, 8850, 9150},
		{0.01, 30, 180}, // sd = 10
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("p=%v", tt.p), func(t *testing.T) {
			inj := newInjector(t, errRule("a", prob(tt.p)))
			r := newReq("GET", "/x")
			hits := 0
			for range trials {
				if inj.Decide("svc", r) != nil {
					hits++
				}
			}
			if hits < tt.min || hits > tt.max {
				t.Errorf("%d/%d fired at p=%v, want within [%d, %d]", hits, trials, tt.p, tt.min, tt.max)
			}
			if got := inj.Counts()["a"]; got != int64(hits) {
				t.Errorf("Counts[a] = %d, want %d", got, hits)
			}
		})
	}
}

func TestDecideProbabilityIsIndependentPerRule(t *testing.T) {
	const trials = 20000
	inj := newInjector(t,
		throttleRule("a", 10, prob(0.5), name("a"), order(1)),
		throttleRule("b", 10, prob(0.5), name("b"), order(2)),
	)
	r := newReq("GET", "/x")
	var both, onlyA, onlyB, neither int
	for range trials {
		p := inj.Decide("s", r)
		switch {
		case p == nil:
			neither++
		case len(p.Fired) == 2:
			both++
		case p.Fired[0] == "a":
			onlyA++
		default:
			onlyB++
		}
	}
	// Each quadrant should be ~25% (5000, sd ~61); allow +-5 sd.
	for label, n := range map[string]int{"both": both, "onlyA": onlyA, "onlyB": onlyB, "neither": neither} {
		if n < 4700 || n > 5300 {
			t.Errorf("%s = %d, want about 5000", label, n)
		}
	}
}

func TestDecideCombination(t *testing.T) {
	t.Run("latency delays add", func(t *testing.T) {
		inj := newInjector(t,
			latencyRule("a", 100*time.Millisecond, 0, order(1)),
			latencyRule("b", 50*time.Millisecond, 0, order(2)),
			latencyRule("c", 25*time.Millisecond, 0, order(3)),
		)
		p := inj.Decide("s", newReq("GET", "/x"))
		if p == nil {
			t.Fatal("no plan")
		}
		if p.Delay != 175*time.Millisecond {
			t.Errorf("Delay = %v, want 175ms", p.Delay)
		}
		if p.Terminal != nil || p.BytesPerSecond != 0 {
			t.Errorf("unexpected terminal/throttle: %+v", p)
		}
		if want := []string{"rule a", "rule b", "rule c"}; !reflect.DeepEqual(p.Fired, want) {
			t.Errorf("Fired = %v, want %v", p.Fired, want)
		}
	})

	t.Run("jitter stays within delay +- jitter and varies", func(t *testing.T) {
		inj := newInjector(t, latencyRule("a", 100*time.Millisecond, 30*time.Millisecond))
		r := newReq("GET", "/x")
		lo, hi := time.Hour, time.Duration(0)
		for range 2000 {
			d := inj.Decide("s", r).Delay
			if d < 70*time.Millisecond || d > 130*time.Millisecond {
				t.Fatalf("delay %v outside [70ms, 130ms]", d)
			}
			lo, hi = min(lo, d), max(hi, d)
		}
		if hi-lo < 40*time.Millisecond {
			t.Errorf("jitter range [%v, %v] suspiciously narrow", lo, hi)
		}
	})

	t.Run("jitter larger than delay never goes negative", func(t *testing.T) {
		inj := newInjector(t, latencyRule("a", 5*time.Millisecond, 50*time.Millisecond))
		r := newReq("GET", "/x")
		zeros := 0
		for range 2000 {
			d := inj.Decide("s", r).Delay
			if d < 0 || d > 55*time.Millisecond {
				t.Fatalf("delay %v outside [0, 55ms]", d)
			}
			if d == 0 {
				zeros++
			}
		}
		if zeros == 0 {
			t.Error("expected some delays clamped to zero")
		}
	})

	t.Run("jitter only", func(t *testing.T) {
		inj := newInjector(t, latencyRule("a", 0, 20*time.Millisecond))
		r := newReq("GET", "/x")
		for range 500 {
			if d := inj.Decide("s", r).Delay; d < 0 || d > 20*time.Millisecond {
				t.Fatalf("delay %v outside [0, 20ms]", d)
			}
		}
	})

	t.Run("huge delays saturate instead of overflowing", func(t *testing.T) {
		huge := time.Duration(1<<63 - 1)
		inj := newInjector(t,
			latencyRule("a", huge, huge, order(1)),
			latencyRule("b", huge, huge, order(2)),
			latencyRule("c", huge, huge, order(3)),
		)
		d := inj.Decide("s", newReq("GET", "/x")).Delay
		if d <= 0 {
			t.Errorf("Delay = %v, want a large positive duration", d)
		}
	})

	t.Run("throttle takes the minimum", func(t *testing.T) {
		inj := newInjector(t,
			throttleRule("a", 5000, order(1)),
			throttleRule("b", 1000, order(2)),
			throttleRule("c", 3000, order(3)),
		)
		p := inj.Decide("s", newReq("GET", "/x"))
		if p.BytesPerSecond != 1000 {
			t.Errorf("BytesPerSecond = %d, want 1000", p.BytesPerSecond)
		}
		// The minimum must not depend on rule order.
		inj = newInjector(t,
			throttleRule("a", 1000, order(1)),
			throttleRule("b", 5000, order(2)),
		)
		if got := inj.Decide("s", newReq("GET", "/x")).BytesPerSecond; got != 1000 {
			t.Errorf("BytesPerSecond = %d, want 1000", got)
		}
	})

	t.Run("first terminal wins by order", func(t *testing.T) {
		inj := newInjector(t,
			errRule("late", order(5), name("late-error")),
			abortRule("early", order(1), name("early-abort")),
		)
		p := inj.Decide("s", newReq("GET", "/x"))
		if p.Terminal == nil || p.Terminal.ID != "early" || p.Terminal.Type != model.FaultAbort {
			t.Fatalf("Terminal = %+v, want the abort rule", p.Terminal)
		}
		if want := []string{"early-abort"}; !reflect.DeepEqual(p.Fired, want) {
			t.Errorf("Fired = %v, want %v", p.Fired, want)
		}
		// The losing terminal rule is not evaluated, so it does not count.
		counts := inj.Counts()
		if counts["early"] != 1 || counts["late"] != 0 {
			t.Errorf("Counts = %v, want early=1 late=0", counts)
		}
	})

	t.Run("a terminal that does not fire does not block a later one", func(t *testing.T) {
		inj := newInjector(t,
			abortRule("early", order(1), prob(0)),
			errRule("late", order(2)),
		)
		p := inj.Decide("s", newReq("GET", "/x"))
		if p == nil || p.Terminal == nil || p.Terminal.ID != "late" {
			t.Fatalf("plan = %+v, want the late error rule", p)
		}
	})

	t.Run("ties sort by name", func(t *testing.T) {
		inj := newInjector(t,
			errRule("z", order(1), name("zebra")),
			errRule("a", order(1), name("aardvark")),
		)
		if got := inj.Decide("s", newReq("GET", "/x")).Terminal.ID; got != "a" {
			t.Errorf("winner = %q, want a (aardvark sorts first)", got)
		}
		rules := inj.Rules()
		if rules[0].ID != "a" || rules[1].ID != "z" {
			t.Errorf("Rules order = %s, %s", rules[0].ID, rules[1].ID)
		}
	})

	t.Run("latency and throttle ordered after the terminal still apply", func(t *testing.T) {
		inj := newInjector(t,
			errRule("e", order(1)),
			latencyRule("l", 40*time.Millisecond, 0, order(2)),
			throttleRule("t", 2000, order(3)),
		)
		p := inj.Decide("s", newReq("GET", "/x"))
		if p.Terminal == nil || p.Delay != 40*time.Millisecond || p.BytesPerSecond != 2000 {
			t.Errorf("plan = %+v", p)
		}
		if len(p.Fired) != 3 {
			t.Errorf("Fired = %v, want 3 rules", p.Fired)
		}
	})

	t.Run("mixed plan", func(t *testing.T) {
		inj := newInjector(t,
			latencyRule("l", 10*time.Millisecond, 0, order(1)),
			throttleRule("t", 4096, order(2)),
			timeoutRule("to", 3*time.Second, order(3)),
			errRule("e", order(4)),
		)
		p := inj.Decide("s", newReq("GET", "/x"))
		if p.Delay != 10*time.Millisecond || p.BytesPerSecond != 4096 {
			t.Errorf("plan = %+v", p)
		}
		if p.Terminal == nil || p.Terminal.ID != "to" || p.Terminal.Delay.D() != 3*time.Second {
			t.Errorf("Terminal = %+v, want the timeout rule", p.Terminal)
		}
		if len(p.Fired) != 3 {
			t.Errorf("Fired = %v", p.Fired)
		}
	})

	t.Run("more than four fired rules", func(t *testing.T) {
		rules := make([]model.FaultRule, 9)
		for n := range rules {
			rules[n] = latencyRule(fmt.Sprint("r", n), time.Millisecond, 0, order(n))
		}
		p := newInjector(t, rules...).Decide("s", newReq("GET", "/x"))
		if len(p.Fired) != 9 || p.Delay != 9*time.Millisecond {
			t.Errorf("plan = %+v", p)
		}
	})
}

func TestDecideDisabledRules(t *testing.T) {
	inj := newInjector(t,
		errRule("off", disabled(), order(1)),
		errRule("on", order(2)),
	)
	p := inj.Decide("s", newReq("GET", "/x"))
	if p == nil || p.Terminal.ID != "on" {
		t.Fatalf("plan = %+v, want the enabled rule", p)
	}
	rules := inj.Rules()
	if len(rules) != 2 || rules[0].ID != "off" || rules[0].Enabled {
		t.Errorf("Rules = %+v, want the disabled rule kept first", rules)
	}
	counts := inj.Counts()
	if v, ok := counts["off"]; !ok || v != 0 {
		t.Errorf("Counts = %v, want off=0 present", counts)
	}

	only := newInjector(t, errRule("off", disabled()))
	if p := only.Decide("s", newReq("GET", "/x")); p != nil {
		t.Errorf("disabled rule fired: %+v", p)
	}
}

func TestSetRulesDefaultsApplied(t *testing.T) {
	inj := newInjector(t,
		model.FaultRule{ID: "e", Name: "e", Enabled: true, Probability: 1, Type: model.FaultError}, // no status
		model.FaultRule{ID: "t", Name: "t", Enabled: true, Probability: 1, Type: model.FaultTimeout, Delay: 0, Order: 1},
	)
	rules := inj.Rules()
	if rules[0].Status != 503 {
		t.Errorf("default status = %d, want 503", rules[0].Status)
	}
	if rules[1].Delay.D() != 60*time.Second {
		t.Errorf("default timeout delay = %v, want 60s", rules[1].Delay)
	}
}

func TestSetRulesValidation(t *testing.T) {
	tests := []struct {
		name  string
		rules []model.FaultRule
		want  string // substring of the error
	}{
		{"missing name", []model.FaultRule{errRule("a", name(""))}, "name is required"},
		{"probability above 1", []model.FaultRule{errRule("a", prob(1.5))}, "probability must be between 0 and 1"},
		{"probability below 0", []model.FaultRule{errRule("a", prob(-0.1))}, "probability must be between 0 and 1"},
		{"probability NaN", []model.FaultRule{errRule("a", prob(nan()))}, "probability must be a number"},
		{"unknown type", []model.FaultRule{errRule("a", func(r *model.FaultRule) { r.Type = "explode" })}, "type"},
		{"error status too low", []model.FaultRule{errRule("a", func(r *model.FaultRule) { r.Status = 99 })}, "status must be a valid HTTP status"},
		{"error status too high", []model.FaultRule{errRule("a", func(r *model.FaultRule) { r.Status = 600 })}, "status must be a valid HTTP status"},
		{"error status informational", []model.FaultRule{errRule("a", func(r *model.FaultRule) { r.Status = 101 })}, "1xx"},
		{"latency without delay", []model.FaultRule{latencyRule("a", 0, 0)}, "needs delay or jitter"},
		{"negative delay", []model.FaultRule{latencyRule("a", -time.Second, time.Second)}, "must not be negative"},
		{"throttle without rate", []model.FaultRule{throttleRule("a", 0)}, "bytes_per_second"},
		{"timeout negative delay", []model.FaultRule{timeoutRule("a", -time.Second)}, "delay"},
		{"glob without slash", []model.FaultRule{errRule("a", path("users"))}, "must start with /"},
		{"bad regex", []model.FaultRule{errRule("a", path("re:(unclosed"))}, "regex"},
		{"empty method", []model.FaultRule{errRule("a", methods(""))}, "invalid method"},
		{"method with space", []model.FaultRule{errRule("a", methods("GE T"))}, "invalid method"},
		{"bad header name", []model.FaultRule{errRule("a", hdr("Bad Header", "x"))}, "invalid header name"},
		{"empty header name", []model.FaultRule{errRule("a", hdr("", "x"))}, "invalid header name"},
		{"bad response header name", []model.FaultRule{errRule("a", func(r *model.FaultRule) { r.ResponseHeader = map[string]string{"X:Y": "1"} })}, "response_headers"},
		{"duplicate ids", []model.FaultRule{errRule("a"), errRule("a", name("other"))}, "duplicate id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inj := NewInjector()
			err := inj.SetRules(tt.rules)
			if err == nil {
				t.Fatal("SetRules succeeded, want an error")
			}
			if !errors.Is(err, model.ErrValidation) {
				t.Errorf("error %v does not wrap model.ErrValidation", err)
			}
			var ve *model.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error is %T, want *model.ValidationError", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func nan() float64 { var z float64; return z / z }

func TestSetRulesErrorKeepsOldRules(t *testing.T) {
	inj := newInjector(t, errRule("keep", name("keeper")))
	r := newReq("GET", "/x")
	if inj.Decide("s", r) == nil {
		t.Fatal("precondition: rule should fire")
	}
	before := inj.Counts()["keep"]

	err := inj.SetRules([]model.FaultRule{errRule("good"), errRule("bad", prob(2))})
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if got := inj.Rules(); len(got) != 1 || got[0].ID != "keep" {
		t.Errorf("Rules after failed SetRules = %+v, want the old rule", got)
	}
	p := inj.Decide("s", r)
	if p == nil || p.Terminal.ID != "keep" {
		t.Fatalf("old rule no longer fires: %+v", p)
	}
	if got := inj.Counts()["keep"]; got != before+1 {
		t.Errorf("count after failed SetRules = %d, want %d", got, before+1)
	}
}

func TestSetRulesReportsEveryProblem(t *testing.T) {
	err := NewInjector().SetRules([]model.FaultRule{
		errRule("a", name("first"), prob(3)),
		errRule("b", name("second"), path("nope")),
	})
	var ve *model.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v", err)
	}
	if len(ve.Problems) != 2 {
		t.Fatalf("problems = %q, want 2", ve.Problems)
	}
	if !strings.Contains(ve.Problems[0], `rules[0] "first" (a)`) || !strings.Contains(ve.Problems[1], `rules[1] "second" (b)`) {
		t.Errorf("problems do not identify their rules: %q", ve.Problems)
	}
}

func TestSetRulesEmptyIDsAllowed(t *testing.T) {
	inj := newInjector(t, errRule("", name("one")), errRule("", name("two"), order(1)))
	if p := inj.Decide("s", newReq("GET", "/x")); p == nil || p.Terminal.Name != "one" {
		t.Fatalf("plan = %+v", p)
	}
	if got := inj.Counts(); len(got) != 0 {
		t.Errorf("Counts = %v, want rules without IDs omitted", got)
	}
}

func TestSetRulesCopiesInput(t *testing.T) {
	in := errRule("a", methods("GET"), hdr("X-A", "1"), func(r *model.FaultRule) {
		r.ResponseHeader = map[string]string{"Retry-After": "1"}
	})
	inj := newInjector(t, in)

	// Mutate every reference field of the caller's rule after SetRules.
	in.Methods[0] = "DELETE"
	in.Headers["X-A"] = "changed"
	in.ResponseHeader["Retry-After"] = "999"

	if inj.Decide("s", newReq("GET", "/x", "X-A", "1")) == nil {
		t.Error("rule stopped matching after the caller mutated its input")
	}
	got := inj.Rules()[0]
	if got.Methods[0] != "GET" || got.Headers["X-A"] != "1" || got.ResponseHeader["Retry-After"] != "1" {
		t.Errorf("stored rule changed: %+v", got)
	}
}

func TestRulesReturnsCopies(t *testing.T) {
	inj := newInjector(t, errRule("a", methods("GET"), hdr("X-A", "1")))
	got := inj.Rules()
	got[0].Name = "mutated"
	got[0].Methods[0] = "DELETE"
	got[0].Headers["X-A"] = "changed"
	got[0].Enabled = false

	again := inj.Rules()[0]
	if again.Name != "rule a" || again.Methods[0] != "GET" || again.Headers["X-A"] != "1" || !again.Enabled {
		t.Errorf("Rules exposed internal state: %+v", again)
	}
	if inj.Decide("s", newReq("GET", "/x", "X-A", "1")) == nil {
		t.Error("rule stopped matching after mutating the copy")
	}
}

func TestRulesPreserveTimestamps(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	inj := newInjector(t, errRule("a", func(r *model.FaultRule) { r.CreatedAt, r.UpdatedAt = created, created.Add(time.Hour) }))
	got := inj.Rules()[0]
	if !got.CreatedAt.Equal(created) || !got.UpdatedAt.Equal(created.Add(time.Hour)) {
		t.Errorf("timestamps lost: %+v", got)
	}
}

func TestCounts(t *testing.T) {
	r := newReq("GET", "/x")
	inj := newInjector(t, errRule("a"), errRule("b", path("/never")), errRule("c", order(1), prob(0)))
	for range 7 {
		inj.Decide("s", r)
	}
	want := map[string]int64{"a": 7, "b": 0, "c": 0}
	if got := inj.Counts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Counts = %v, want %v", got, want)
	}

	// Same IDs survive an update, even when the rule's content changes.
	if err := inj.SetRules([]model.FaultRule{
		errRule("a", name("renamed")),
		errRule("d", order(1), path("/other")),
	}); err != nil {
		t.Fatal(err)
	}
	want = map[string]int64{"a": 7, "d": 0}
	if got := inj.Counts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Counts after SetRules = %v, want %v", got, want)
	}
	inj.Decide("s", r)
	if got := inj.Counts()["a"]; got != 8 {
		t.Errorf("Counts[a] = %d, want 8", got)
	}

	// Dropping a rule discards its count; re-adding starts from zero.
	if err := inj.SetRules([]model.FaultRule{errRule("d")}); err != nil {
		t.Fatal(err)
	}
	if err := inj.SetRules([]model.FaultRule{errRule("a"), errRule("d", order(1))}); err != nil {
		t.Fatal(err)
	}
	if got := inj.Counts()["a"]; got != 0 {
		t.Errorf("Counts[a] after remove and re-add = %d, want 0", got)
	}

	// An empty rule set clears everything.
	if err := inj.SetRules(nil); err != nil {
		t.Fatal(err)
	}
	if got := inj.Counts(); len(got) != 0 {
		t.Errorf("Counts after SetRules(nil) = %v", got)
	}
	if inj.Decide("s", r) != nil {
		t.Error("rules still fire after SetRules(nil)")
	}
}

func TestCountsFollowEnabledState(t *testing.T) {
	inj := newInjector(t, errRule("a"))
	r := newReq("GET", "/x")
	inj.Decide("s", r)
	if err := inj.SetRules([]model.FaultRule{errRule("a", disabled())}); err != nil {
		t.Fatal(err)
	}
	inj.Decide("s", r) // disabled: must not count
	if err := inj.SetRules([]model.FaultRule{errRule("a")}); err != nil {
		t.Fatal(err)
	}
	inj.Decide("s", r)
	if got := inj.Counts()["a"]; got != 2 {
		t.Errorf("Counts[a] = %d, want 2 (disabled rule keeps its count but does not fire)", got)
	}
}

func TestConcurrentDecideAndSetRules(t *testing.T) {
	const (
		deciders  = 8
		perWorker = 5000
	)
	inj := newInjector(t, errRule("hot"))
	stop := make(chan struct{})

	var swappers sync.WaitGroup
	swappers.Add(1)
	go func() {
		defer swappers.Done()
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			// Keep "hot" in every set (its counter must be carried over each
			// time) and add churn around it.
			rules := []model.FaultRule{
				errRule("hot", name(fmt.Sprint("v", n))),
				latencyRule(fmt.Sprint("churn", n%5), time.Millisecond, 0, order(1)),
			}
			if err := inj.SetRules(rules); err != nil {
				panic(err)
			}
			if n%7 == 0 {
				_ = inj.SetRules([]model.FaultRule{errRule("bad", prob(5))}) // rejected
			}
			_ = inj.Rules()
			_ = inj.Counts()
		}
	}()

	var fired atomic.Int64
	var wg sync.WaitGroup
	for range deciders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := newReq("GET", "/x")
			for range perWorker {
				if p := inj.Decide("s", r); p != nil && p.Terminal != nil {
					fired.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	swappers.Wait()

	// "hot" is in every set and always fires, so no increment may be lost
	// across swaps.
	if got, want := inj.Counts()["hot"], fired.Load(); got != want || want != deciders*perWorker {
		t.Errorf("hot count = %d, observed fires = %d, want both %d", got, want, deciders*perWorker)
	}
}

func TestDecideZeroAllocsWhenNothingFires(t *testing.T) {
	inj := newInjector(t,
		errRule("svc", order(1), svc("other")),
		errRule("meth", order(2), methods("DELETE")),
		errRule("glob", order(3), path("/admin/**")),
		errRule("globstar", order(4), path("/users/*/orders")),
		errRule("regex", order(5), path(`re:^/internal/\d+$`)),
		errRule("hdr", order(6), hdr("X-Chaos", "yes")),
		errRule("hdrhost", order(7), hdr("Host", "nope.example.com")),
		errRule("prob0", order(8), prob(0)),
		errRule("disabled", order(9), disabled()),
		errRule("lowprob", order(10), prob(1e-12)),
		latencyRule("lat", time.Second, time.Second, order(11), path("/slow/**")),
	)
	r := newReq("GET", "/users/42/profile")
	if allocs := testing.AllocsPerRun(1000, func() {
		if inj.Decide("svc", r) != nil {
			t.Fatal("unexpected fault")
		}
	}); allocs != 0 {
		t.Errorf("Decide with no matching rule allocated %v times per call, want 0", allocs)
	}

	// And with an empty injector.
	empty := NewInjector()
	if allocs := testing.AllocsPerRun(1000, func() { empty.Decide("svc", r) }); allocs != 0 {
		t.Errorf("Decide on an empty injector allocated %v times, want 0", allocs)
	}
}

func TestDecideMatchAllocatesOnce(t *testing.T) {
	inj := newInjector(t, errRule("a"), latencyRule("b", time.Millisecond, 0, order(1)))
	r := newReq("GET", "/x")
	if allocs := testing.AllocsPerRun(1000, func() { _ = inj.Decide("svc", r) }); allocs > 1 {
		t.Errorf("Decide with a match allocated %v times per call, want at most 1", allocs)
	}
}
