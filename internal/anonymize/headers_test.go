package anonymize

import (
	"net/http"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func reqHeaders(t *testing.T, e *Engine, h http.Header) http.Header {
	t.Helper()
	req := &model.Request{Method: "GET", Path: "/", Header: h}
	if err := e.Request(req); err != nil {
		t.Fatal(err)
	}
	return req.Header
}

func TestRedactHeadersKeepAuthScheme(t *testing.T) {
	e := defaultEngine(t)
	tok := e.k.hashToken("xyz")
	cases := []struct{ in, want string }{
		{"Bearer xyz", "Bearer " + tok},
		{"bearer xyz", "bearer " + tok},
		{"Basic xyz", "Basic " + tok},
		{"Token xyz", "Token " + tok},
		{"Digest xyz", "Digest " + tok},
		{"Bearer   xyz", "Bearer   " + tok},
		{"xyz", tok},                                  // no scheme: the whole value is hashed
		{"Unknown xyz", e.k.hashToken("Unknown xyz")}, // not a known scheme
		{"Bearer ", e.k.hashToken("Bearer ")},         // nothing after the scheme
		{"Bearer", e.k.hashToken("Bearer")},
		{"", ""},
	}
	for _, c := range cases {
		h := reqHeaders(t, e, http.Header{"Authorization": {c.in}})
		if got := h.Get("Authorization"); got != c.want {
			t.Errorf("Authorization %q -> %q, want %q", c.in, got, c.want)
		}
	}
	// The same credential hashes the same under every scheme and header.
	h := reqHeaders(t, e, http.Header{
		"Authorization": {"Bearer xyz"}, "Proxy-Authorization": {"Token xyz"}, "X-Api-Key": {"xyz"}, "X-Auth-Token": {"xyz"},
	})
	for name, v := range h {
		if !strings.HasSuffix(v[0], tok) {
			t.Errorf("%s = %q does not end with the shared token %s", name, v[0], tok)
		}
	}
}

func TestRedactHeadersDefaultList(t *testing.T) {
	e := defaultEngine(t)
	in := http.Header{}
	for _, n := range model.DefaultAnonymizationProfile().RedactHeaders {
		if n == "Cookie" || n == "Set-Cookie" {
			in.Set(n, "c=secret-"+n+"; Path=/")
		} else {
			in.Set(n, "secret-"+n)
		}
	}
	in.Set("X-Keep", "keep")
	in.Set("Accept", "application/json")
	out := reqHeaders(t, e, in)
	for _, n := range model.DefaultAnonymizationProfile().RedactHeaders {
		if v := out.Get(n); strings.Contains(v, "secret-") || !strings.Contains(v, "h_") {
			t.Errorf("%s = %q", n, v)
		}
	}
	if out.Get("X-Keep") != "keep" || out.Get("Accept") != "application/json" {
		t.Errorf("unlisted headers changed: %v", out)
	}
}

func TestHeaderCopyOnWrite(t *testing.T) {
	e := defaultEngine(t)
	orig := []string{"k1", "k2"}
	shared := http.Header{"X-Api-Key": orig}
	copyOf := shared.Clone()
	req := &model.Request{Header: shared}
	if err := e.Request(req); err != nil {
		t.Fatal(err)
	}
	got := req.Header["X-Api-Key"]
	if len(got) != 2 || got[0] != e.k.hashToken("k1") || got[1] != e.k.hashToken("k2") {
		t.Errorf("multi-valued header: %v", got)
	}
	if orig[0] != copyOf["X-Api-Key"][0] || orig[1] != copyOf["X-Api-Key"][1] {
		t.Errorf("the caller's slice was modified in place: %v", orig)
	}
}

func TestNonCanonicalAndNilHeaders(t *testing.T) {
	e := defaultEngine(t)
	auth, key := "authori"+"zation", "x-api-"+"key"
	h := reqHeaders(t, e, rawHeader(auth, "Bearer xyz", key, "k"))
	if h[auth][0] != "Bearer "+e.k.hashToken("xyz") || h[key][0] != e.k.hashToken("k") {
		t.Errorf("non-canonical header keys were skipped: %v", h)
	}
	// nil maps and nil values must not panic
	if err := e.Request(&model.Request{}); err != nil {
		t.Fatal(err)
	}
	if err := e.Response(&model.Response{}); err != nil {
		t.Fatal(err)
	}
	h = reqHeaders(t, e, http.Header{"X-Empty": nil, "X-Blank": {""}, "Authorization": {""}})
	if len(h["X-Empty"]) != 0 || h["X-Blank"][0] != "" || h["Authorization"][0] != "" {
		t.Errorf("empty values changed: %v", h)
	}
}

func TestCookieHeader(t *testing.T) {
	e := defaultEngine(t)
	a, b := e.k.hashToken("1"), e.k.hashToken("2")
	cases := []struct{ in, want string }{
		{"a=1; b=2", "a=" + a + "; b=" + b},
		{"a=1;b=2", "a=" + a + ";b=" + b},
		{"a=1;  b=2 ;", "a=" + a + ";  b=" + b + " ;"}, // separators and a trailing ';' survive
		{`a="1"; b=2`, `a="` + a + `"; b=` + b},        // quotes stay outside the token
		{"a=; b=2", "a=; b=" + b},                      // empty value: nothing to hide
		{"a=1", "a=" + a},
		{"a=1=1", "a=" + e.k.hashToken("1=1")},              // only the first '=' splits
		{"token; b=2", e.k.hashToken("token") + "; b=" + b}, // bare token could be a credential
	}
	for _, c := range cases {
		h := reqHeaders(t, e, http.Header{"Cookie": {c.in}})
		if got := h.Get("Cookie"); got != c.want {
			t.Errorf("Cookie %q -> %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSetCookieHeader(t *testing.T) {
	e := defaultEngine(t)
	cases := []struct{ in, want string }{
		{"sid=abc; Path=/; HttpOnly", "sid=" + e.k.hashToken("abc") + "; Path=/; HttpOnly"},
		{"sid=abc", "sid=" + e.k.hashToken("abc")},
		{"sid=abc==; Max-Age=3600; Secure; SameSite=Lax", "sid=" + e.k.hashToken("abc==") + "; Max-Age=3600; Secure; SameSite=Lax"},
		{"sid=abc; Expires=Wed, 21 Oct 2015 07:28:00 GMT; Domain=example.com", "sid=" + e.k.hashToken("abc") + "; Expires=Wed, 21 Oct 2015 07:28:00 GMT; Domain=example.com"},
		{`sid="abc"; Path=/`, `sid="` + e.k.hashToken("abc") + `"; Path=/`},
		{"sid=; Path=/; Max-Age=0", "sid=; Path=/; Max-Age=0"}, // a cleared cookie
		{"HttpOnly", "HttpOnly"}, // malformed: no name=value
	}
	for _, c := range cases {
		resp := &model.Response{Header: http.Header{"Set-Cookie": {c.in}}}
		if err := e.Response(resp); err != nil {
			t.Fatal(err)
		}
		if got := resp.Header.Get("Set-Cookie"); got != c.want {
			t.Errorf("Set-Cookie %q -> %q, want %q", c.in, got, c.want)
		}
	}
	// Several Set-Cookie headers are handled one by one.
	resp := &model.Response{Header: http.Header{"Set-Cookie": {"a=1; Path=/", "b=2; Secure"}}}
	if err := e.Response(resp); err != nil {
		t.Fatal(err)
	}
	want := []string{"a=" + e.k.hashToken("1") + "; Path=/", "b=" + e.k.hashToken("2") + "; Secure"}
	if got := resp.Header["Set-Cookie"]; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCookiesNotInRedactListAreScannedByDetectors(t *testing.T) {
	p := testProfile()
	p.RedactHeaders = nil
	e := mustEngine(t, p)
	h := reqHeaders(t, e, http.Header{"Cookie": {"who=jane@acme.io; n=1"}})
	if got := h.Get("Cookie"); strings.Contains(got, "acme.io") || !strings.HasSuffix(got, "; n=1") || !strings.HasPrefix(got, "who=") {
		t.Errorf("cookie value not scanned: %q", got)
	}
}

func TestCookieRules(t *testing.T) {
	rm := rl(model.TargetCookie, "sid", model.StrategyRemove)
	cases := []struct {
		name, in, want string
		drop           bool
	}{
		{"middle", "a=1; sid=x; b=2", "a=" + hashOf("1") + "; b=" + hashOf("2"), false},
		{"first", "sid=x; a=1", "a=" + hashOf("1"), false},
		{"last", "a=1; sid=x", "a=" + hashOf("1"), false},
		{"only", "sid=x", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := mustEngine(t, testProfile(rm))
			h := reqHeaders(t, e, http.Header{"Cookie": {c.in}})
			_, present := h["Cookie"]
			if c.drop {
				if present {
					t.Errorf("the empty Cookie header was kept: %v", h)
				}
				return
			}
			if got := h.Get("Cookie"); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}

	// A cookie rule beats redact_headers and sees the original value.
	e := mustEngine(t, testProfile(withFake(rl(model.TargetCookie, "who", model.StrategyFake), model.FakeEmail)))
	h := reqHeaders(t, e, http.Header{"Cookie": {"who=jane@acme.io; sid=abc"}})
	want := "who=" + e.k.fake(model.FakeEmail, "jane@acme.io") + "; sid=" + e.k.hashToken("abc")
	if got := h.Get("Cookie"); got != want {
		t.Errorf("cookie rule: got %q, want %q", got, want)
	}

	// Rules reach Set-Cookie, and remove deletes the header when it was the cookie.
	e = mustEngine(t, testProfile(rm))
	resp := &model.Response{Header: http.Header{"Set-Cookie": {"sid=abc; Path=/", "other=1; Path=/"}}}
	if err := e.Response(resp); err != nil {
		t.Fatal(err)
	}
	if got := resp.Header["Set-Cookie"]; len(got) != 1 || !strings.HasPrefix(got[0], "other=h_") {
		t.Errorf("Set-Cookie after remove: %v", got)
	}
}

func TestHeaderRuleBeatsRedactHeaders(t *testing.T) {
	// X-Api-Key is in the default redact list; the rule must see the original.
	e := mustEngine(t, testProfile(withFake(rl(model.TargetHeader, "X-Api-Key", model.StrategyFake), model.FakeEmail)))
	h := reqHeaders(t, e, http.Header{"X-Api-Key": {"jane@acme.io"}})
	if got, want := h.Get("X-Api-Key"), e.k.fake(model.FakeEmail, "jane@acme.io"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// The scheme survives a header rule on Authorization.
	e = mustEngine(t, testProfile(rl(model.TargetHeader, "Authorization", model.StrategyHash)))
	h = reqHeaders(t, e, http.Header{"Authorization": {"Bearer xyz"}})
	if got := h.Get("Authorization"); got != "Bearer "+e.k.hashToken("xyz") {
		t.Errorf("Authorization with a hash rule: %q", got)
	}
	// Remove deletes the header, in every spelling.
	e = mustEngine(t, testProfile(rl(model.TargetHeader, "authorization", model.StrategyRemove)))
	h = reqHeaders(t, e, http.Header{"Authorization": {"Bearer xyz"}, "X-Keep": {"1"}})
	if _, ok := h["Authorization"]; ok || h.Get("X-Keep") != "1" {
		t.Errorf("remove: %v", h)
	}
}

func TestDetectorsScanOtherHeaders(t *testing.T) {
	e := defaultEngine(t)
	h := reqHeaders(t, e, http.Header{
		"X-Note":  {"call 415-555-2671 or mail jane@acme.io"},
		"Referer": {"https://shop.test/cart?email=jane%40acme.io&n=1"},
		"X-Multi": {"1.2.3.4", "plain"},
	})
	for name, v := range h {
		for _, x := range v {
			if strings.Contains(x, "acme") || strings.Contains(x, "415-555") || strings.Contains(x, "1.2.3.4") {
				t.Errorf("%s leaked: %q", name, x)
			}
		}
	}
	if !strings.Contains(h.Get("Referer"), "&n=1") || !strings.HasPrefix(h.Get("Referer"), "https://shop.test/cart?email=") {
		t.Errorf("surrounding text of the URL changed: %q", h.Get("Referer"))
	}
	if h["X-Multi"][1] != "plain" {
		t.Errorf("clean value changed: %v", h["X-Multi"])
	}
}

func TestStructuralHeadersAreNeverTouched(t *testing.T) {
	rules := []model.AnonymizationRule{rl(model.TargetRegex, `\d+`, model.StrategyRedact)}
	e := mustEngine(t, testProfile(rules...))
	h := reqHeaders(t, e, http.Header{
		"Content-Length": {"12345"}, "Content-Type": {"text/plain; charset=utf-8"}, "Host": {"10.0.0.1:8080"},
		"Content-Encoding": {"gzip"}, "Transfer-Encoding": {"chunked"}, "Connection": {"keep-alive"},
		"X-Other": {"abc 123"},
	})
	if h.Get("Content-Length") != "12345" || h.Get("Content-Type") != "text/plain; charset=utf-8" ||
		h.Get("Host") != "10.0.0.1:8080" || h.Get("Content-Encoding") != "gzip" {
		t.Errorf("structural headers changed: %v", h)
	}
	if h.Get("X-Other") != "abc [REDACTED]" {
		t.Errorf("regex rule did not apply to a normal header: %q", h.Get("X-Other"))
	}
}

func TestVersionStringsInHeadersAreNotAddresses(t *testing.T) {
	e := defaultEngine(t)
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	h := reqHeaders(t, e, http.Header{
		"User-Agent": {ua}, "Etag": {`W/"1698765432123456"`}, "If-None-Match": {`"4111111111111111"`},
		"Date": {"Wed, 21 Oct 2015 07:28:00 GMT"}, "Range": {"bytes=4111111111111111-"},
	})
	if h.Get("User-Agent") != ua || h.Get("Etag") != `W/"1698765432123456"` || h.Get("If-None-Match") != `"4111111111111111"` {
		t.Errorf("protocol metadata was rewritten: %v", h)
	}
	// ...but an explicit rule still reaches them.
	e = mustEngine(t, testProfile(rl(model.TargetHeader, "User-Agent", model.StrategyRedact)))
	h = reqHeaders(t, e, http.Header{"User-Agent": {ua}})
	if h.Get("User-Agent") != "[REDACTED]" {
		t.Errorf("rule on User-Agent: %q", h.Get("User-Agent"))
	}
}

func TestSplitAuthScheme(t *testing.T) {
	cases := []struct{ in, scheme, rest string }{
		{"Bearer abc", "Bearer ", "abc"},
		{"BEARER abc", "BEARER ", "abc"},
		{"Basic  abc", "Basic  ", "abc"},
		{"Bearer abc def", "Bearer ", "abc def"},
		{"abc", "", "abc"},
		{"Foo abc", "", "Foo abc"},
		{"Bearer ", "", "Bearer "},
		{" abc", "", " abc"},
		{"", "", ""},
	}
	for _, c := range cases {
		if s, r := splitAuthScheme(c.in); s != c.scheme || r != c.rest {
			t.Errorf("splitAuthScheme(%q) = (%q,%q), want (%q,%q)", c.in, s, r, c.scheme, c.rest)
		}
	}
}
