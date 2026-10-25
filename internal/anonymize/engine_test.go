package anonymize

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func TestNewRejectsBadRules(t *testing.T) {
	r := func(target model.RuleTarget, sel string) model.AnonymizationRule {
		return rl(target, sel, model.StrategyRedact)
	}
	cases := []struct {
		name string
		rule model.AnonymizationRule
		want string // substring of a problem
	}{
		{"JSONPath without $", r(model.TargetJSON, "user.email"), "JSON selector must start with $"},
		{"JSONPath unterminated bracket", r(model.TargetJSON, "$.a["), "invalid JSONPath"},
		{"JSONPath filter", r(model.TargetJSON, "$.a[?(@.x>1)]"), "invalid JSONPath"},
		{"JSONPath slice", r(model.TargetJSON, "$.a[0:2]"), "invalid JSONPath"},
		{"JSONPath negative index", r(model.TargetJSON, "$.a[-1]"), "invalid JSONPath"},
		{"JSONPath trailing dot", r(model.TargetJSON, "$.a."), "invalid JSONPath"},
		{"regex", r(model.TargetRegex, "(unclosed"), "invalid regex"},
		{"path without slash", r(model.TargetPath, "users/{id}"), "path selector must start with /"},
		{"path unbalanced brace", r(model.TargetPath, "/users/{id"), "invalid path template"},
		{"path without parameter", r(model.TargetPath, "/users/all"), "invalid path template"},
		{"path rest not last", r(model.TargetPath, "/a/{x...}/b"), "invalid path template"},
		{"path empty segment", r(model.TargetPath, "/a//{x}"), "invalid path template"},
		{"path empty name", r(model.TargetPath, "/a/{}"), "invalid path template"},
		{"unknown target", r("telepathy", "x"), "unknown target"},
		{"empty selector", r(model.TargetHeader, "  "), "selector is required"},
		{"unknown scope", withScope(r(model.TargetHeader, "X"), "sideways"), "unknown scope"},
		{"unknown strategy", rl(model.TargetHeader, "X", "scramble"), "unknown strategy"},
		{"unknown fake kind", withFake(rl(model.TargetHeader, "X", model.StrategyFake), "klingon"), "unknown fake kind"},
		{"negative keep", func() model.AnonymizationRule {
			x := rl(model.TargetHeader, "X", model.StrategyMask)
			x.KeepPrefix = -1
			return x
		}(), "must not be negative"},
		{"disabled rules are validated too", func() model.AnonymizationRule {
			x := r(model.TargetRegex, "(")
			x.Enabled = false
			return x
		}(), "invalid regex"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(testProfile(c.rule))
			if err == nil {
				t.Fatal("expected an error")
			}
			var ve *model.ValidationError
			if !errors.As(err, &ve) || !errors.Is(err, model.ErrValidation) {
				t.Fatalf("error %T %v is not a validation error", err, err)
			}
			if !strings.Contains(strings.Join(ve.Problems, "|"), c.want) {
				t.Errorf("problems %q do not mention %q", ve.Problems, c.want)
			}
			if err := Validate(testProfile(c.rule)); err == nil {
				t.Error("Validate accepted the profile")
			}
		})
	}
}

func TestNewCollectsEveryProblemOnce(t *testing.T) {
	p := testProfile(
		rl(model.TargetJSON, "no-dollar", model.StrategyRedact),
		rl(model.TargetRegex, "(", model.StrategyRedact),
		rl(model.TargetJSON, "$.a[", model.StrategyRedact),
		rl(model.TargetPath, "/x/{", model.StrategyRedact),
	)
	p.Detectors = append(p.Detectors, "mind-reader")
	_, err := New(p)
	var ve *model.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("got %v", err)
	}
	if len(ve.Problems) != 5 {
		t.Errorf("want 5 problems, got %d: %q", len(ve.Problems), ve.Problems)
	}
	seen := map[string]bool{}
	for _, pr := range ve.Problems {
		if seen[pr] {
			t.Errorf("duplicate problem %q", pr)
		}
		seen[pr] = true
	}
}

func TestNewAndValidateNameHandling(t *testing.T) {
	p := testProfile()
	p.Name = ""
	if _, err := New(p); err != nil {
		t.Errorf("New should not require a name: %v", err)
	}
	if err := Validate(p); err == nil {
		t.Error("Validate should require a name (model-level validation)")
	}
	p.Name = "ok"
	if err := Validate(p); err != nil {
		t.Errorf("Validate: %v", err)
	}
	if err := Validate(model.DefaultAnonymizationProfile()); err != nil {
		t.Errorf("the built-in profile must validate: %v", err)
	}
	// Too many rules.
	big := testProfile()
	for i := 0; i < maxRules+1; i++ {
		big.Rules = append(big.Rules, rl(model.TargetHeader, fmt.Sprintf("X-%d", i), model.StrategyRedact))
	}
	if _, err := New(big); err == nil {
		t.Error("expected an error for too many rules")
	}
	if err := Validate(big); err == nil {
		t.Error("Validate: expected an error for too many rules")
	}
}

func TestNewAppliesDefaultsWithoutTouchingTheInput(t *testing.T) {
	rules := []model.AnonymizationRule{{ID: "r", Name: "n", Enabled: true, Target: model.TargetHeader, Selector: "X-A"}} // no scope, no strategy
	p := model.AnonymizationProfile{Name: "p", Secret: "s", Rules: rules}
	e, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	if rules[0].Scope != "" || rules[0].Strategy != "" {
		t.Errorf("the caller's rules were modified: %+v", rules[0])
	}
	got := e.Profile()
	if got.Rules[0].Scope != model.ScopeBoth || got.Rules[0].Strategy != model.StrategyHash || got.DetectorStrategy != model.StrategyFake {
		t.Errorf("defaults not applied: %+v", got)
	}
	// The default strategy is hash, the default scope both.
	ex := newExchange()
	ex.Request.Header.Set("X-A", "v")
	ex.Response.Header.Set("X-A", "v")
	if err := e.Exchange(ex); err != nil {
		t.Fatal(err)
	}
	if ex.Request.Header.Get("X-A") != e.k.hashToken("v") || ex.Response.Header.Get("X-A") != e.k.hashToken("v") {
		t.Errorf("default rule: %v / %v", ex.Request.Header, ex.Response.Header)
	}
	// Profile returns a copy.
	got.Rules[0].Selector = "mutated"
	got.Detectors = append(got.Detectors, "x")
	if e.Profile().Rules[0].Selector != "X-A" {
		t.Error("Profile() exposes the engine's rules")
	}
}

func TestEmptySecretGivesRandomPerEngineSecret(t *testing.T) {
	p := testProfile()
	p.Secret = ""
	a, b := mustEngine(t, p), mustEngine(t, p)
	first := a.k.hashToken("v")
	if again := a.k.hashToken("v"); first != again {
		t.Error("tokens are not stable within one engine")
	}
	if first == b.k.hashToken("v") {
		t.Error("two engines without a secret share a key")
	}
	ex := newExchange()
	ex.Request.Header.Set("X-Api-Key", "k")
	if err := a.Exchange(ex); err != nil || ex.Request.Header.Get("X-Api-Key") != a.k.hashToken("k") {
		t.Errorf("exchange: %v %v", err, ex.Request.Header)
	}
}

func TestWithSalt(t *testing.T) {
	e := defaultEngine(t)
	if e.WithSalt("") != e {
		t.Error(`WithSalt("") must return the receiver`)
	}
	a1, a2, b := e.WithSalt("clone-1"), e.WithSalt("clone-1"), e.WithSalt("clone-2")
	if a1 == e || a1.c != e.c {
		t.Error("a salted engine shares the compiled rules and is a different engine")
	}
	for _, v := range []string{"user-1", "jane@acme.io", "4111111111111111"} {
		if a1.k.hashToken(v) != a2.k.hashToken(v) {
			t.Errorf("same salt, different token for %q", v)
		}
		if a1.k.hashToken(v) == b.k.hashToken(v) || a1.k.hashToken(v) == e.k.hashToken(v) {
			t.Errorf("salts do not separate the token of %q", v)
		}
		if a1.k.fake(model.FakeAuto, v) == b.k.fake(model.FakeAuto, v) {
			t.Errorf("salts do not separate the fake of %q", v)
		}
	}
	// Engines for a salt are reused, and the cache stays bounded.
	if e.WithSalt("clone-1") != a1 {
		t.Error("the engine for a salt should be reused")
	}
	for i := 0; i < 3*maxSaltedEngines; i++ {
		e.WithSalt(fmt.Sprint("churn-", i))
	}
	e.salted.mu.RLock()
	size := len(e.salted.m)
	e.salted.mu.RUnlock()
	if size > maxSaltedEngines {
		t.Errorf("salt cache grew to %d entries", size)
	}
	if again := e.WithSalt("clone-1"); again.k.hashToken("v") != a1.k.hashToken("v") {
		t.Error("an evicted and rebuilt salted engine must give the same tokens")
	}
	// WithSalt replaces the salt, it does not stack: it is keyed by secret+salt.
	if e.WithSalt("a").WithSalt("b").k.hashToken("v") != e.WithSalt("b").k.hashToken("v") {
		t.Error("WithSalt on a salted engine must key by the original secret and the new salt")
	}
	// The salted engine anonymizes with the same rules but its own identities.
	mk := func(en *Engine) *model.Exchange {
		ex := newExchange()
		ex.Request.Path = "/u/jane%40acme.io"
		ex.Request.Header.Set("Authorization", "Bearer tok")
		if err := en.Exchange(ex); err != nil {
			t.Fatal(err)
		}
		return ex
	}
	x, y, z := mk(a1), mk(a2), mk(e)
	if !reflect.DeepEqual(x, y) {
		t.Error("same salt, different exchange")
	}
	if x.Request.Path == z.Request.Path || x.Request.Header.Get("Authorization") == z.Request.Header.Get("Authorization") {
		t.Error("salt did not change the identities in the exchange")
	}
	if p := a1.Profile(); !reflect.DeepEqual(p, e.Profile()) {
		t.Error("a salted engine reports the same profile")
	}
}

func TestDeterminismAndSecretSeparation(t *testing.T) {
	build := func() *model.Exchange {
		ex := newExchange()
		ex.Request.Method = "POST"
		ex.Request.Path = "/users/jane%40acme.io/orders"
		ex.Request.RawQuery = "email=jane%40acme.io&card=4111111111111111"
		ex.Request.Header.Set("Authorization", "Bearer tok")
		ex.Request.Header.Set("Cookie", "sid=abc")
		ex.Request.Header.Set("Content-Type", "application/json")
		ex.Request.Body = []byte(`{"email":"jane@acme.io","phone":"415-555-2671","ip":"192.168.1.10","uuid":"550e8400-e29b-41d4-a716-446655440000"}`)
		ex.Response.Header.Set("Content-Type", "application/json")
		ex.Response.Header.Set("Set-Cookie", "sid=abc; Path=/")
		ex.Response.Body = []byte(`{"owner":"jane@acme.io","iban":"GB82 WEST 1234 5698 7654 32","ssn":"123-45-6789"}`)
		return ex
	}
	p := testProfile(rl(model.TargetJSON, "$.uuid", model.StrategyHash))
	e1, e2 := mustEngine(t, p), mustEngine(t, p)
	a, b := build(), build()
	if err := e1.Exchange(a); err != nil {
		t.Fatal(err)
	}
	if err := e2.Exchange(b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("two engines with one secret disagree:\n%+v\n%+v", a, b)
	}
	// Anonymizing again is deterministic too.
	c := build()
	if err := e1.Exchange(c); err != nil || !reflect.DeepEqual(a, c) {
		t.Errorf("repeat run differs (err %v)", err)
	}

	p2 := p
	p2.Secret = "another-secret"
	d := build()
	if err := mustEngine(t, p2).Exchange(d); err != nil {
		t.Fatal(err)
	}
	if string(d.Request.Body) == string(a.Request.Body) || d.Request.Path == a.Request.Path ||
		d.Request.Header.Get("Authorization") == a.Request.Header.Get("Authorization") {
		t.Error("a different secret produced the same anonymized output")
	}
}

func TestSameValueMapsToTheSameTokenEverywhere(t *testing.T) {
	// "jane@acme.io" appears in the path, query, cookie, header, request body
	// and response body; every occurrence must become the same fake address.
	e := defaultEngine(t)
	ex := newExchange()
	ex.Request.Path = "/users/jane%40acme.io"
	ex.Request.RawQuery = "email=jane%40acme.io"
	ex.Request.Header.Set("X-Note", "from jane@acme.io")
	ex.Request.Header.Set("Content-Type", "application/json")
	ex.Request.Body = []byte(`{"email":"jane@acme.io"}`)
	ex.Response.Header.Set("Content-Type", "text/plain")
	ex.Response.Body = []byte("owner: jane@acme.io")
	if err := e.Exchange(ex); err != nil {
		t.Fatal(err)
	}
	fake := e.k.fake(model.FakeEmail, "jane@acme.io")
	for name, v := range map[string]string{
		"path": ex.Request.Path, "query": ex.Request.RawQuery, "header": ex.Request.Header.Get("X-Note"),
		"request body": string(ex.Request.Body), "response body": string(ex.Response.Body),
	} {
		if !strings.Contains(v, fake) {
			t.Errorf("%s = %q does not contain the shared fake %q", name, v, fake)
		}
	}

	// Same for a hash rule: a customer ID in path, query, header and bodies.
	hashRules := []model.AnonymizationRule{
		rl(model.TargetPath, "/customers/{id}", model.StrategyHash),
		rl(model.TargetQuery, "customer", model.StrategyHash),
		rl(model.TargetHeader, "X-Customer", model.StrategyHash),
		rl(model.TargetJSON, "$..customer", model.StrategyHash),
		rl(model.TargetForm, "customer", model.StrategyHash),
	}
	e = mustEngine(t, bareProfile(hashRules...))
	ex = newExchange()
	ex.Request.Path = "/customers/C-42"
	ex.Request.RawQuery = "customer=C-42"
	ex.Request.Header.Set("X-Customer", "C-42")
	ex.Request.Header.Set("Content-Type", "application/json")
	ex.Request.Body = []byte(`{"customer":"C-42"}`)
	ex.Response.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ex.Response.Body = []byte("customer=C-42")
	if err := e.Exchange(ex); err != nil {
		t.Fatal(err)
	}
	tok := e.k.hashToken("C-42")
	for name, v := range map[string]string{
		"path": ex.Request.Path, "query": ex.Request.RawQuery, "header": ex.Request.Header.Get("X-Customer"),
		"json": string(ex.Request.Body), "form": string(ex.Response.Body),
	} {
		if !strings.Contains(v, tok) || strings.Contains(v, "C-42") {
			t.Errorf("%s = %q", name, v)
		}
	}
}

func TestExchangeBookkeeping(t *testing.T) {
	e := defaultEngine(t)
	ex := newExchange()
	ex.Error = "dial tcp 203.0.113.9:443: connect: connection refused"
	ex.Endpoint = "GET /users/{id}"
	ex.ID, ex.Seq, ex.RecordingID = 7, 3, "rec_x"
	if err := e.Exchange(ex); err != nil {
		t.Fatal(err)
	}
	if !ex.Anonymized {
		t.Error("Anonymized not set")
	}
	if strings.Contains(ex.Error, "203.0.113.9") || !strings.HasSuffix(ex.Error, ":443: connect: connection refused") {
		t.Errorf("Error = %q", ex.Error)
	}
	if ex.Endpoint != "GET /users/{id}" || ex.ID != 7 || ex.Seq != 3 || ex.RecordingID != "rec_x" {
		t.Errorf("identity fields changed: %+v", ex)
	}

	// Request and Response alone do not claim the whole exchange is anonymized.
	ex = newExchange()
	if err := e.Request(&ex.Request); err != nil || e.Response(&ex.Response) != nil || ex.Anonymized {
		t.Error("Request/Response must not set Anonymized")
	}

	// nil arguments are errors, not panics.
	for name, err := range map[string]error{
		"Exchange": e.Exchange(nil), "Request": e.Request(nil), "Response": e.Response(nil),
	} {
		if !errors.Is(err, model.ErrValidation) {
			t.Errorf("%s(nil) = %v", name, err)
		}
	}
	if _, err := e.ExchangeWithReport(nil); err == nil {
		t.Error("ExchangeWithReport(nil) should fail")
	}
}

func TestRecoverInternal(t *testing.T) {
	f := func() (err error) {
		defer recoverInternal(&err)
		panic("boom")
	}
	if err := f(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v", err)
	}
	g := func() (err error) {
		defer recoverInternal(&err)
		return nil
	}
	if g() != nil {
		t.Error("recoverInternal invented an error")
	}
}

func TestDetectorSelectionAndStrategies(t *testing.T) {
	const in = `{"e":"mail jane@acme.io now","p":"call 415-555-2671"}`
	eml, ph := "jane@acme.io", "415-555-2671"

	// Detectors that are not enabled do nothing.
	p := testProfile()
	p.Detectors = []model.Detector{model.DetectPhone}
	e := mustEngine(t, p)
	if got := anonJSON(t, e, in); !strings.Contains(got, eml) || strings.Contains(got, ph) {
		t.Errorf("only the phone detector was enabled: %s", got)
	}
	p.Detectors = nil
	if got := anonJSON(t, mustEngine(t, p), in); got != in {
		t.Errorf("no detectors, yet the body changed: %s", got)
	}

	// Each detector strategy.
	cases := []struct {
		strategy model.Strategy
		want     string
	}{
		{model.StrategyRedact, `{"e":"mail [REDACTED] now","p":"call [REDACTED]"}`},
		{model.StrategyRemove, `{"e":"mail [REDACTED] now","p":"call [REDACTED]"}`}, // cannot delete part of a string
		{model.StrategyHash, `{"e":"mail ` + hashOf(eml) + ` now","p":"call ` + hashOf(ph) + `"}`},
		{model.StrategyMask, `{"e":"mail ************ now","p":"call ************"}`},
	}
	for _, c := range cases {
		p := testProfile()
		p.DetectorStrategy = c.strategy
		if got := anonJSON(t, mustEngine(t, p), in); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.strategy, got, c.want)
		}
	}
	e = defaultEngine(t) // fake
	got := anonJSON(t, e, in)
	if !strings.Contains(got, e.k.fake(model.FakeEmail, eml)) || !strings.Contains(got, e.k.fake(model.FakePhone, ph)) {
		t.Errorf("fake strategy: %s", got)
	}
}

func TestEveryDetectorFakeStaysDetectable(t *testing.T) {
	// A faked value keeps its shape, so a second pass over already
	// anonymized data (replay with per-clone identities) still finds it.
	e := defaultEngine(t)
	samples := map[model.Detector]string{
		model.DetectEmail: "jane@acme.io", model.DetectCreditCard: "4111 1111 1111 1111", model.DetectSSN: "123-45-6789",
		model.DetectPhone: "(415) 555-2671", model.DetectIPv4: "192.168.1.10", model.DetectIPv6: "2001:db8::8a2e:370:7334",
		model.DetectJWT: sampleJWT, model.DetectAWSKey: "AKIAIOSFODNN7EXAMPLE", model.DetectIBAN: "GB82 WEST 1234 5698 7654 32",
	}
	for det, in := range samples {
		out := anonResp(t, e, "text/plain", "value: "+in+" end")
		if strings.Contains(out, in) {
			t.Errorf("%s: not anonymized: %q", det, out)
			continue
		}
		fakeValue := strings.TrimSuffix(strings.TrimPrefix(out, "value: "), " end")
		if got := matchesOf(det, fakeValue); len(got) != 1 || got[0] != fakeValue {
			t.Errorf("%s: fake %q is not detected as %s (%v)", det, fakeValue, det, got)
		}
	}
	// UUIDs are off by default.
	const u = "550e8400-e29b-41d4-a716-446655440000"
	if got := anonResp(t, e, "text/plain", u); got != u {
		t.Errorf("the uuid detector is off by default: %q", got)
	}
	p := testProfile()
	p.Detectors = []model.Detector{model.DetectUUID}
	if got := anonResp(t, mustEngine(t, p), "text/plain", u); got == u || !isUUID(got) {
		t.Errorf("uuid detector: %q", got)
	}
}

func TestExchangeIsSafeForConcurrentUse(t *testing.T) {
	p := testProfile(
		rl(model.TargetJSON, "$..customer_id", model.StrategyHash),
		rl(model.TargetRegex, `ORDER-\d+`, model.StrategyMask),
		rl(model.TargetPath, "/users/{id}", model.StrategyHash),
	)
	e := mustEngine(t, p)
	salted := e.WithSalt("clone")
	template := sampleExchange()
	wantPlain, wantSalted := template.Clone(), template.Clone()
	if _, err := e.ExchangeWithReport(wantPlain); err != nil {
		t.Fatal(err)
	}
	if err := salted.Exchange(wantSalted); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				ex, want, en := template.Clone(), wantPlain, e
				if (g+i)%2 == 1 {
					want, en = wantSalted, salted
				}
				if err := en.Exchange(ex); err != nil {
					errs <- err.Error()
					return
				}
				if !reflect.DeepEqual(ex, want) {
					errs <- fmt.Sprintf("goroutine %d iteration %d: result differs", g, i)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
}

// sampleExchange is a realistic exchange of roughly 2 KB of JSON, shared by
// tests and benchmarks.
func sampleExchange() *model.Exchange {
	var users strings.Builder
	users.WriteString(`{"data":{"page":1,"total":4,"users":[`)
	for i := 0; i < 4; i++ {
		if i > 0 {
			users.WriteByte(',')
		}
		fmt.Fprintf(&users, `{"id":%d,"customer_id":"cust-%04d","name":"User Number %d","email":"user%d@acme-corp.io",`+
			`"phone":"(415) 555-%04d","active":true,"created_at":"2024-03-%02dT10:15:30Z","last_ip":"10.20.%d.%d",`+
			`"address":{"street":"%d Main Street","city":"Springfield","zip":"9%04d","country":"US"},`+
			`"preferences":{"newsletter":false,"theme":"dark","locale":"en-US","tags":["a","b","c"]},`+
			`"balance":%d.%02d,"notes":"Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor"}`,
			1000+i, i, i, i, 2000+i, 10+i, i, 40+i, 100+i, 1000+i, 5000+i*17, i*7)
	}
	users.WriteString(`],"links":{"self":"/v1/users?page=1","next":"/v1/users?page=2"}},"meta":{"request_id":"req-8f3a","elapsed_ms":12}}`)
	reqBody := `{"filter":{"email":"user1@acme-corp.io","status":"active"},"fields":["id","name","email"],"page":1,"per_page":50}`
	return &model.Exchange{
		Request: model.Request{
			Method: "POST", Scheme: "https", Host: "api.acme-corp.io", Path: "/v1/users/search", RawQuery: "expand=address&include=prefs&page=1",
			Proto: "HTTP/1.1",
			Header: http.Header{
				"Accept": {"application/json"}, "Accept-Encoding": {"gzip"}, "Authorization": {"Bearer eyJhbGciOiJIUzI1NiJ9.e30.abc"},
				"Content-Type": {"application/json"}, "Content-Length": {fmt.Sprint(len(reqBody))},
				"User-Agent": {"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36"},
				"Cookie":     {"sid=abcdef123456; theme=dark"}, "X-Request-Id": {"req-8f3a"},
			},
			Body: []byte(reqBody), BodySize: int64(len(reqBody)),
		},
		Response: model.Response{
			Status: 200, Proto: "HTTP/1.1",
			Header: http.Header{
				"Content-Type": {"application/json; charset=utf-8"}, "Content-Length": {fmt.Sprint(users.Len())},
				"Cache-Control": {"no-store"}, "Date": {"Mon, 02 Jan 2026 15:04:05 GMT"}, "Server": {"nginx/1.25.3"},
				"Set-Cookie": {"sid=abcdef123456; Path=/; HttpOnly; Secure"}, "X-Request-Id": {"req-8f3a"}, "Vary": {"Accept-Encoding"},
			},
			Body: []byte(users.String()), BodySize: int64(users.Len()),
		},
	}
}
