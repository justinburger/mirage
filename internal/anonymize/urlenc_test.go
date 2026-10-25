package anonymize

import (
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func queryOf(t *testing.T, e *Engine, raw string) string {
	t.Helper()
	req := &model.Request{Path: "/", RawQuery: raw}
	if err := e.Request(req); err != nil {
		t.Fatal(err)
	}
	return req.RawQuery
}

func TestQueryPreservesOrderAndEncoding(t *testing.T) {
	e := defaultEngine(t)
	fake := e.k.fake(model.FakeEmail, "jane@acme.io")
	cases := []struct{ in, want string }{
		{"b=2&a=jane%40acme.io&c=x%2Fy+z&d", "b=2&a=" + fake + "&c=x%2Fy+z&d"},
		{"a=1&&b=2&", "a=1&&b=2&"},
		{"&a=1", "&a=1"},
		{"a=%zz&e=jane@acme.io", "a=%zz&e=" + fake},
		{"q=a+b&r=%20", "q=a+b&r=%20"},
		{"a=b=c&d=jane%40acme.io", "a=b=c&d=" + fake},
		{"jane@acme.io", fake},          // PII in a key-only parameter
		{"jane@acme.io=1", fake + "=1"}, // PII in a parameter name
		{"x=" + strings.Repeat("a", 3000), "x=" + strings.Repeat("a", 3000)},
		{"", ""},
		{"=", "="},
		{"a", "a"},
	}
	for _, c := range cases {
		if got := queryOf(t, e, c.in); got != c.want {
			t.Errorf("query %q\n got  %q\n want %q", c.in, got, c.want)
		}
	}
}

func TestQueryDetectorsDecodeFirst(t *testing.T) {
	e := defaultEngine(t)
	// A phone number whose + was sent as %2B, a card with + for spaces.
	got := queryOf(t, e, "p=%2B14155552671&c=4111+1111+1111+1111&ip=10.1.2.3")
	for _, leak := range []string{"14155552671", "4111", "10.1.2.3"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q leaked in %q", leak, got)
		}
	}
	if !strings.HasPrefix(got, "p=%2B") {
		t.Errorf("the plus sign of the phone number lost its encoding: %q", got)
	}
}

func TestQueryRemove(t *testing.T) {
	e := mustEngine(t, bareProfile(rl(model.TargetQuery, "token", model.StrategyRemove)))
	cases := []struct{ in, want string }{
		{"a=1&token=x&b=2", "a=1&b=2"},
		{"token=x&a=1", "a=1"},
		{"a=1&token=x", "a=1"},
		{"token=x", ""},
		{"token=x&token=y&a=1", "a=1"},
		{"a=1&token&b=2", "a=1&b=2"}, // a value-less parameter is removed as well
		{"a=1&&token=x", "a=1&"},
	}
	for _, c := range cases {
		if got := queryOf(t, e, c.in); got != c.want {
			t.Errorf("remove token from %q = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPathDetectors(t *testing.T) {
	e := defaultEngine(t)
	fake := e.k.fake(model.FakeEmail, "jane@acme.io")
	cases := []struct{ in, want string }{
		{"/users/jane%40acme.io/profile", "/users/" + fake + "/profile"},
		{"/ip/10.1.2.3/x", "/ip/" + e.k.fake(model.FakeIPv4, "10.1.2.3") + "/x"},
		{"/a/b/c", "/a/b/c"},
		{"//a//b/", "//a//b/"},
		{"/", "/"},
		{"", ""},
		{"/files/report%20final.pdf", "/files/report%20final.pdf"},
		{"/v1/orders/4111111111111111", "/v1/orders/" + e.k.fake(model.FakeCreditCard, "4111111111111111")},
		{"/time/2024-01-15T10:30:00Z", "/time/2024-01-15T10:30:00Z"},
	}
	for _, c := range cases {
		req := &model.Request{Path: c.in}
		if err := e.Request(req); err != nil {
			t.Fatal(err)
		}
		if req.Path != c.want {
			t.Errorf("path %q\n got  %q\n want %q", c.in, req.Path, c.want)
		}
	}
}

func TestPathRuleSegmentIsNotRescannedByDetectors(t *testing.T) {
	r := withFake(rl(model.TargetPath, "/u/{id}", model.StrategyFake), model.FakeEmail)
	e := mustEngine(t, testProfile(r))
	req := &model.Request{Path: "/u/jane@acme.io"}
	if err := e.Request(req); err != nil {
		t.Fatal(err)
	}
	want := "/u/" + e.k.fake(model.FakeEmail, "jane@acme.io")
	if req.Path != want {
		t.Errorf("got %q, want %q (a faked e-mail must not be faked again)", req.Path, want)
	}
}

func TestPathRuleAppliesToRequestsOnly(t *testing.T) {
	e := mustEngine(t, bareProfile(rl(model.TargetPath, "/users/{id}", model.StrategyHash)))
	ex := newExchange()
	ex.Request.Path = "/users/42"
	ex.Response.Header.Set("Location", "/users/42")
	if err := e.Exchange(ex); err != nil {
		t.Fatal(err)
	}
	if ex.Request.Path != "/users/"+e.k.hashToken("42") {
		t.Errorf("request path = %q", ex.Request.Path)
	}
	if ex.Response.Header.Get("Location") != "/users/42" {
		t.Errorf("a path rule must not touch response headers: %q", ex.Response.Header.Get("Location"))
	}
}

func TestEscapeHelpers(t *testing.T) {
	cases := []struct{ in, query, path string }{
		{"abc-._~", "abc-._~", "abc-._~"},
		{"a b", "a%20b", "a%20b"},
		{"a&b=c+d", "a%26b%3Dc%2Bd", "a&b=c+d"},
		{"a/b?c#d", "a/b?c%23d", "a%2Fb%3Fc%23d"},
		{"[REDACTED]", "%5BREDACTED%5D", "%5BREDACTED%5D"},
		{"jane@acme.io", "jane@acme.io", "jane@acme.io"},
		{"é", "%C3%A9", "%C3%A9"},
		{"100%", "100%25", "100%25"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := escapeQuery(c.in); got != c.query {
			t.Errorf("escapeQuery(%q) = %q, want %q", c.in, got, c.query)
		}
		if got := escapePathSegment(c.in); got != c.path {
			t.Errorf("escapePathSegment(%q) = %q, want %q", c.in, got, c.path)
		}
	}
	for _, s := range []string{"a%20b", "a+b", "%zz", "100%", "%", "plain", "%e9", "%C3%A9"} {
		_ = decodeQuery(s)
		_ = decodePath(s)
	}
	if decodeQuery("a+b%20c") != "a b c" || decodePath("a+b%20c") != "a+b c" || decodeQuery("%zz") != "%zz" {
		t.Error("decoding")
	}
}

func TestCleanRequestAllocatesNothing(t *testing.T) {
	e := defaultEngine(t)
	req := &model.Request{Method: "GET", Path: "/v1/orders/list", RawQuery: "page=2&sort=name", Header: map[string][]string{"Accept": {"application/json"}}}
	allocs := testing.AllocsPerRun(100, func() {
		if err := e.Request(req); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("anonymizing a request with nothing to change allocates %.0f times", allocs)
	}
}
