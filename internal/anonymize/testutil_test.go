package anonymize

import (
	"net/http"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

const testSecret = "unit-test-secret"

// testProfile returns the default profile with a fixed secret and the given
// rules.
func testProfile(rules ...model.AnonymizationRule) model.AnonymizationProfile {
	p := model.DefaultAnonymizationProfile()
	p.Secret = testSecret
	p.Rules = append([]model.AnonymizationRule{}, rules...)
	return p
}

// bareProfile has no detectors and no redacted headers, so only the rules
// under test can change anything.
func bareProfile(rules ...model.AnonymizationRule) model.AnonymizationProfile {
	p := testProfile(rules...)
	p.Detectors = nil
	p.RedactHeaders = nil
	return p
}

func mustEngine(t testing.TB, p model.AnonymizationProfile) *Engine {
	t.Helper()
	e, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func defaultEngine(t testing.TB) *Engine { return mustEngine(t, testProfile()) }

// rl builds an enabled rule for both sides.
func rl(target model.RuleTarget, selector string, strategy model.Strategy) model.AnonymizationRule {
	return model.AnonymizationRule{
		ID: "r-" + selector, Name: "rule " + selector, Enabled: true, Target: target,
		Selector: selector, Scope: model.ScopeBoth, Strategy: strategy,
	}
}

func withFake(r model.AnonymizationRule, k model.FakeKind) model.AnonymizationRule {
	r.Fake = k
	return r
}

func withScope(r model.AnonymizationRule, s model.RuleScope) model.AnonymizationRule {
	r.Scope = s
	return r
}

// reqBody anonymizes a request carrying body with the given content type and
// returns the exchange.
func anonReq(t testing.TB, e *Engine, ct, body string) *model.Request {
	t.Helper()
	req := &model.Request{Method: "POST", Path: "/x", Header: http.Header{}, Body: []byte(body), BodySize: int64(len(body))}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if err := e.Request(req); err != nil {
		t.Fatalf("Request: %v", err)
	}
	return req
}

// anonResp anonymizes a response body and returns it as a string.
func anonResp(t testing.TB, e *Engine, ct, body string) string {
	t.Helper()
	resp := &model.Response{Status: 200, Header: http.Header{}, Body: []byte(body), BodySize: int64(len(body))}
	if ct != "" {
		resp.Header.Set("Content-Type", ct)
	}
	if err := e.Response(resp); err != nil {
		t.Fatalf("Response: %v", err)
	}
	return string(resp.Body)
}

// anonJSON anonymizes a JSON response body.
func anonJSON(t testing.TB, e *Engine, body string) string {
	t.Helper()
	return anonResp(t, e, "application/json", body)
}

func newExchange() *model.Exchange {
	return &model.Exchange{
		Request:  model.Request{Method: "GET", Path: "/", Header: http.Header{}},
		Response: model.Response{Status: 200, Header: http.Header{}},
	}
}

// matchTexts runs all detectors over s and returns "detector:text" strings in
// position order.
func matchTexts(s string) []string {
	var all uint16
	for di := range detectors {
		all |= 1 << di
	}
	st := classify(s)
	var c claims
	c.scan(s, candidates(all, &st))
	var out []string
	for _, m := range c.list() {
		out = append(out, string(detectors[m.det].name)+":"+s[m.start:m.end])
	}
	return out
}

// rawHeader builds a header map with keys exactly as given, without
// canonicalization (hand-built maps in other packages can look like this).
func rawHeader(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		k := kv[i]
		h[k] = append(h[k], kv[i+1])
	}
	return h
}

// u renders a JSON unicode escape (backslash, 'u' and four hex digits). The
// tests build escapes with it so the source never contains a literal
// escape sequence that tooling could mistake for a character.
func u(hex string) string { return "\\" + "u" + hex }
