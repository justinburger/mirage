package virtual

import (
	"net/http"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
)

func defaultMatch() model.MatchConfig {
	return model.Service{}.WithDefaults().Match
}

func TestMatchLevelsAndFallThrough(t *testing.T) {
	exs := func() []*model.Exchange {
		return []*model.Exchange{
			mkEx(1, "GET", "/users/42?expand=orders", 200, "A"),
			mkEx(2, "GET", "/users/42", 200, "B"),
			mkEx(3, "GET", "/users/7?x=1", 200, "C"),
			mkEx(4, "POST", "/users/42", 201, "D", withReqBody(`{"a":1}`)),
		}
	}
	tests := []struct {
		name      string
		templates bool
		method    string
		target    string
		body      string
		wantLevel string
		wantBody  string
	}{
		{"exact with query", true, "GET", "/users/42?expand=orders", "", LevelExact, "A"},
		{"exact without query", true, "GET", "/users/42", "", LevelExact, "B"},
		{"exact other id", true, "GET", "/users/7?x=1", "", LevelExact, "C"},
		{"path when the query differs", true, "GET", "/users/42?expand=other", "", LevelPath, "A"},
		{"path when the query is missing", true, "GET", "/users/7", "", LevelPath, "C"},
		{"template for an unseen id", true, "GET", "/users/99", "", LevelTemplate, "A"},
		{"template ignores the query", true, "GET", "/users/99?zzz=1", "", LevelTemplate, "A"},
		{"no template without the option", false, "GET", "/users/99", "", LevelMiss, ""},
		{"unknown endpoint", true, "GET", "/orders/1", "", LevelMiss, ""},
		{"method is part of every level", true, "DELETE", "/users/42", "", LevelMiss, ""},
		{"post exact body", true, "POST", "/users/42", `{"a":1}`, LevelExact, "D"},
		{"post other body falls to path", true, "POST", "/users/42", `{"a":2}`, LevelPath, "D"},
		{"post template", true, "POST", "/users/5", `{"a":2}`, LevelTemplate, "D"},
		{"lower case method", true, "get", "/users/42", "", LevelExact, "B"},
		{"trailing slash is a different path", false, "GET", "/users/42/", "", LevelMiss, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := Options{Match: defaultMatch()}
			o.Match.PathTemplates = tc.templates
			e := newTestEngine(t, o, exs()...)
			level, body := lookup(e, tc.method, tc.target, tc.body)
			if level != tc.wantLevel || body != tc.wantBody {
				t.Fatalf("got (%s, %q), want (%s, %q)", level, body, tc.wantLevel, tc.wantBody)
			}
		})
	}
}

func TestMatchEscapedPathAndEndpoint(t *testing.T) {
	// Recorded paths are escaped paths; templates use the stored endpoint.
	e := newTestEngine(t, Options{Match: model.MatchConfig{PathTemplates: true}},
		mkEx(1, "GET", "/files/a%20b/42", 200, "F"),
		mkEx(2, "GET", "/users/jane%40acme.io", 200, "U"),
	)
	if l, b := lookup(e, "GET", "/files/a%20b/42", ""); l != LevelExact || b != "F" {
		t.Fatalf("escaped path: got (%s,%q)", l, b)
	}
	if l, b := lookup(e, "GET", "/files/a%20b/43", ""); l != LevelTemplate || b != "F" {
		t.Fatalf("escaped template: got (%s,%q)", l, b)
	}
	if l, b := lookup(e, "GET", "/users/bob%40acme.io", ""); l != LevelTemplate || b != "U" {
		t.Fatalf("email template: got (%s,%q)", l, b)
	}
}

func TestTemplateUsesStoredEndpoint(t *testing.T) {
	// The stored endpoint wins over recomputing from the (anonymized) path.
	n, err := pathnorm.New([]string{"/custom/{thing}"})
	if err != nil {
		t.Fatal(err)
	}
	ex := mkEx(1, "GET", "/users/h_3f9a1c0b7d2e4a51/orders", 200, "X")
	ex.Endpoint = "GET /custom/{thing}"
	e := newTestEngine(t, Options{Match: model.MatchConfig{PathTemplates: true}, Normalizer: n}, ex)
	if l, _ := lookup(e, "GET", "/custom/1", ""); l != LevelTemplate {
		t.Fatalf("stored endpoint not used: %s", l)
	}
	if l, _ := lookup(e, "GET", "/users/55/orders", ""); l != LevelMiss {
		t.Fatalf("computed endpoint used instead of the stored one: %s", l)
	}
}

func TestTemplateComputedWhenEndpointEmpty(t *testing.T) {
	ex := mkEx(1, "GET", "/users/42", 200, "X")
	ex.Endpoint = ""
	e := newTestEngine(t, Options{Match: model.MatchConfig{PathTemplates: true}}, ex)
	if l, _ := lookup(e, "GET", "/users/43", ""); l != LevelTemplate {
		t.Fatalf("got %s, want template", l)
	}
}

func TestQueryModes(t *testing.T) {
	tests := []struct {
		name      string
		query     model.QueryMatch
		ignore    []string
		recorded  string
		request   string
		wantLevel string
	}{
		{"exact equal", model.QueryExact, nil, "a=1&b=2", "a=1&b=2", LevelExact},
		{"exact reordered", model.QueryExact, nil, "a=1&b=2", "b=2&a=1", LevelExact},
		{"exact repeated reordered", model.QueryExact, nil, "a=2&a=1", "a=1&a=2", LevelExact},
		{"exact repeated different", model.QueryExact, nil, "a=1&a=1", "a=1&a=2", LevelPath},
		{"exact extra param", model.QueryExact, nil, "a=1", "a=1&b=2", LevelPath},
		{"exact missing param", model.QueryExact, nil, "a=1&b=2", "a=1", LevelPath},
		{"exact different value", model.QueryExact, nil, "a=1", "a=2", LevelPath},
		{"exact none vs none", model.QueryExact, nil, "", "", LevelExact},
		{"exact none vs some", model.QueryExact, nil, "", "a=1", LevelPath},
		{"exact percent and plus", model.QueryExact, nil, "q=a%20b", "q=a+b", LevelExact},
		{"exact encoded name", model.QueryExact, nil, "a%5Bb%5D=1", "a[b]=1", LevelExact},
		{"exact bare flag equals empty value", model.QueryExact, nil, "flag", "flag=", LevelExact},
		{"exact empty segments skipped", model.QueryExact, nil, "a=1", "&a=1&&", LevelExact},
		{"value with equals is distinct", model.QueryExact, nil, "a=b=c", "a=b&c=", LevelPath},
		{"ambiguous join is distinct", model.QueryExact, nil, "a=1%262", "a=1&2=", LevelPath},
		{"ignore param in request only", model.QueryExact, []string{"_ts"}, "a=1", "a=1&_ts=999", LevelExact},
		{"ignore param in recording only", model.QueryExact, []string{"_ts"}, "a=1&_ts=5", "a=1", LevelExact},
		{"ignore param different values", model.QueryExact, []string{"_ts"}, "a=1&_ts=5", "_ts=6&a=1", LevelExact},
		{"ignore does not hide other params", model.QueryExact, []string{"_ts"}, "a=1&_ts=5", "a=2&_ts=5", LevelPath},
		{"ignore mode any query", model.QueryIgnore, nil, "a=1", "z=9&y=8", LevelExact},
		{"ignore mode none vs some", model.QueryIgnore, nil, "", "a=1", LevelExact},
		{"subset request has more", model.QuerySubset, nil, "a=1", "a=1&b=2", LevelExact},
		{"subset equal", model.QuerySubset, nil, "a=1&b=2", "b=2&a=1", LevelExact},
		{"subset recorded has more", model.QuerySubset, nil, "a=1&b=2", "a=1", LevelMiss},
		{"subset value differs", model.QuerySubset, nil, "a=1", "a=2&b=1", LevelMiss},
		{"subset no recorded params", model.QuerySubset, nil, "", "a=1", LevelExact},
		{"subset ignores listed params", model.QuerySubset, []string{"_ts"}, "a=1&_ts=5", "a=1", LevelExact},
		{"subset repeated value present", model.QuerySubset, nil, "a=1", "a=2&a=1", LevelExact},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := Options{Match: model.MatchConfig{Query: tc.query, IgnoreParams: tc.ignore, Body: model.BodyIgnore}}
			target := "/r"
			if tc.recorded != "" {
				target += "?" + tc.recorded
			}
			e := newTestEngine(t, o, mkEx(1, "GET", target, 200, "R"))
			req := "/r"
			if tc.request != "" {
				req += "?" + tc.request
			}
			level, _ := lookup(e, "GET", req, "")
			// With the query ignored (or a subset) the path level equals the
			// exact level, so a "path" outcome shows up as exact.
			if o.Match.Query != model.QueryExact && o.Match.Body == model.BodyIgnore && tc.wantLevel == LevelPath {
				t.Fatalf("bad test case: path level is never reported for %s", tc.query)
			}
			if level != tc.wantLevel {
				t.Fatalf("recorded %q request %q: got %s, want %s", tc.recorded, tc.request, level, tc.wantLevel)
			}
		})
	}
}

// With QuerySubset and a body the path level is distinct from the exact level,
// and the subset filter applies to it too.
func TestQuerySubsetAppliesToEveryLevel(t *testing.T) {
	o := Options{Match: model.MatchConfig{Query: model.QuerySubset, Body: model.BodyExact, PathTemplates: true}}
	e := newTestEngine(t, o,
		mkEx(1, "POST", "/u/1?a=1", 200, "one", withReqBody("x")),
		mkEx(2, "POST", "/u/1?a=2", 200, "two", withReqBody("x")),
	)
	tests := []struct {
		target, body, wantLevel, wantBody string
	}{
		{"/u/1?a=2&z=1", "x", LevelExact, "two"},    // exact level, filtered
		{"/u/1?a=2", "other", LevelPath, "two"},     // path level, filtered
		{"/u/9?a=1", "other", LevelTemplate, "one"}, // template level, filtered
		{"/u/1?a=3", "x", LevelMiss, ""},            // nothing passes at any level
	}
	for _, tc := range tests {
		if l, b := lookup(e, "POST", tc.target, tc.body); l != tc.wantLevel || b != tc.wantBody {
			t.Errorf("%s body %q: got (%s,%q), want (%s,%q)", tc.target, tc.body, l, b, tc.wantLevel, tc.wantBody)
		}
	}
}

func TestBodyModes(t *testing.T) {
	tests := []struct {
		name      string
		mode      model.BodyMatch
		recorded  string
		request   string
		wantLevel string
	}{
		{"ignore anything", model.BodyIgnore, `{"a":1}`, `zzz`, LevelExact},
		{"exact same bytes", model.BodyExact, `{"a":1}`, `{"a":1}`, LevelExact},
		{"exact whitespace differs", model.BodyExact, `{"a":1}`, `{"a": 1}`, LevelPath},
		{"exact empty vs empty", model.BodyExact, ``, ``, LevelExact},
		{"exact empty vs some", model.BodyExact, ``, `x`, LevelPath},
		{"json whitespace", model.BodyJSON, `{"a":1,"b":[1,2]}`, "{\n  \"a\": 1,\n  \"b\": [ 1 , 2 ]\n}", LevelExact},
		{"json key order", model.BodyJSON, `{"a":1,"b":2}`, `{"b":2,"a":1}`, LevelExact},
		{"json nested key order", model.BodyJSON, `{"x":{"a":1,"b":{"c":3,"d":4}}}`, `{"x":{"b":{"d":4,"c":3},"a":1}}`, LevelExact},
		{"json array order matters", model.BodyJSON, `[1,2,3]`, `[3,2,1]`, LevelPath},
		{"json array of objects", model.BodyJSON, `[{"a":1,"b":2},{"c":3}]`, `[{"b":2,"a":1},{"c":3}]`, LevelExact},
		{"json value differs", model.BodyJSON, `{"a":1}`, `{"a":2}`, LevelPath},
		{"json number literals are literals", model.BodyJSON, `{"a":1}`, `{"a":1.0}`, LevelPath},
		{"json number exponent literal", model.BodyJSON, `{"a":1e3}`, `{"a":1e3}`, LevelExact},
		{"json string vs number", model.BodyJSON, `{"a":"1"}`, `{"a":1}`, LevelPath},
		{"json true vs string", model.BodyJSON, `{"a":true}`, `{"a":"true"}`, LevelPath},
		{"json null vs missing", model.BodyJSON, `{"a":null}`, `{}`, LevelPath},
		{"json extra member", model.BodyJSON, `{"a":1}`, `{"a":1,"b":2}`, LevelPath},
		{"json duplicate keys count", model.BodyJSON, `{"a":1,"a":1}`, `{"a":1}`, LevelPath},
		{"json escaped string equals plain", model.BodyJSON, `{"k":"AB"}`, `{"k":"AB"}`, LevelExact},
		{"json escaped key equals plain", model.BodyJSON, `{"k":1}`, `{"k":1}`, LevelExact},
		{"json escaped quote", model.BodyJSON, `{"k":"a\"b"}`, `{"k":"a\"b"}`, LevelExact},
		{"json empty object vs array", model.BodyJSON, `{}`, `[]`, LevelPath},
		{"json top-level scalar", model.BodyJSON, ` 42 `, `42`, LevelExact},
		{"json top-level string", model.BodyJSON, `"hi"`, ` "hi" `, LevelExact},
		{"non-json falls back to bytes", model.BodyJSON, `a=1&b=2`, `a=1&b=2`, LevelExact},
		{"non-json bytes differ", model.BodyJSON, `a=1&b=2`, `b=2&a=1`, LevelPath},
		{"invalid json is bytes", model.BodyJSON, `{"a":1,}`, `{"a": 1,}`, LevelPath},
		{"invalid json same bytes", model.BodyJSON, `{"a":1,}`, `{"a":1,}`, LevelExact},
		{"empty vs empty", model.BodyJSON, ``, ``, LevelExact},
		{"empty vs json", model.BodyJSON, ``, `{}`, LevelPath},
		{"json vs equal-looking text", model.BodyJSON, `{"a":1}`, `{"a":1} `, LevelExact}, // trailing space is insignificant JSON whitespace
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := Options{Match: model.MatchConfig{Body: tc.mode}}
			e := newTestEngine(t, o, mkEx(1, "POST", "/b", 200, "R", withReqBody(tc.recorded)))
			level, _ := lookup(e, "POST", "/b", tc.request)
			want := tc.wantLevel
			if tc.mode == model.BodyIgnore {
				want = LevelExact
			}
			if level != want {
				t.Fatalf("recorded %q request %q: got %s, want %s", tc.recorded, tc.request, level, want)
			}
		})
	}
}

func TestBodyMatchDecodesCompressedRequest(t *testing.T) {
	e := newTestEngine(t, Options{Match: model.MatchConfig{Body: model.BodyJSON}},
		mkEx(1, "POST", "/b", 200, "R", withReqBody(`{"a":1}`)))
	gz := gzipForTest(t, `{ "a": 1 }`)
	m := e.Lookup(mkReq("POST", "/b", "Content-Encoding", "gzip"), gz)
	if m == nil || m.Level != LevelExact {
		t.Fatalf("gzip request body not decoded for matching: %+v", m)
	}
	// A header that lies about the encoding falls back to the raw bytes.
	m = e.Lookup(mkReq("POST", "/b", "Content-Encoding", "gzip"), []byte(`{"a":1}`))
	if m == nil || m.Level != LevelExact {
		t.Fatalf("plain body with a gzip header should still match: %+v", m)
	}
}

func TestHeaderMatching(t *testing.T) {
	o := Options{Match: model.MatchConfig{Headers: []string{"accept-language", "X-Tenant"}}}
	e := newTestEngine(t, o,
		mkEx(1, "GET", "/h", 200, "en-acme", withReqHeader("Accept-Language", "en"), withReqHeader("X-Tenant", "acme")),
		mkEx(2, "GET", "/h", 200, "fr-acme", withReqHeader("Accept-Language", "fr"), withReqHeader("X-Tenant", "acme")),
		mkEx(3, "GET", "/h", 200, "none"),
	)
	tests := []struct {
		name      string
		hdr       []string
		wantLevel string
		wantBody  string
	}{
		{"first", []string{"Accept-Language", "en", "X-Tenant", "acme"}, LevelExact, "en-acme"},
		{"second", []string{"Accept-Language", "fr", "X-Tenant", "acme"}, LevelExact, "fr-acme"},
		{"neither header", nil, LevelExact, "none"},
		{"unmatched tenant falls to path", []string{"Accept-Language", "en", "X-Tenant", "other"}, LevelPath, "en-acme"},
		{"unlisted headers are ignored", []string{"User-Agent", "x", "X-Other", "y"}, LevelExact, "none"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Fresh cursor state per case.
			e2 := newTestEngine(t, o,
				mkEx(1, "GET", "/h", 200, "en-acme", withReqHeader("Accept-Language", "en"), withReqHeader("X-Tenant", "acme")),
				mkEx(2, "GET", "/h", 200, "fr-acme", withReqHeader("Accept-Language", "fr"), withReqHeader("X-Tenant", "acme")),
				mkEx(3, "GET", "/h", 200, "none"),
			)
			level, body := lookup(e2, "GET", "/h", "", tc.hdr...)
			if level != tc.wantLevel || body != tc.wantBody {
				t.Fatalf("got (%s,%q), want (%s,%q)", level, body, tc.wantLevel, tc.wantBody)
			}
		})
	}
	_ = e
}

func TestHeaderMatchingMultiValue(t *testing.T) {
	o := Options{Match: model.MatchConfig{Headers: []string{"X-Multi"}}}
	e := newTestEngine(t, o, mkEx(1, "GET", "/h", 200, "ab", withReqHeader("X-Multi", "a", "b")))
	r := mkReq("GET", "/h")
	r.Header.Add("X-Multi", "a")
	r.Header.Add("X-Multi", "b")
	if m := e.Lookup(r, nil); m == nil || m.Level != LevelExact {
		t.Fatalf("multi-value header: %+v", m)
	}
	r = mkReq("GET", "/h", "X-Multi", "ab")
	if m := e.Lookup(r, nil); m == nil || m.Level != LevelPath {
		t.Fatalf("one value \"ab\" must not equal two values a,b: %+v", m)
	}
}

func TestSelection(t *testing.T) {
	mk := func(sel model.Selection, seed uint64) *Engine {
		return newTestEngine(t, Options{Match: model.MatchConfig{Selection: sel}, Seed: seed},
			mkEx(1, "GET", "/s", 200, "1"),
			mkEx(2, "GET", "/s", 200, "2"),
			mkEx(3, "GET", "/s", 200, "3"),
			mkEx(4, "GET", "/t", 200, "x"),
			mkEx(5, "GET", "/t", 200, "y"),
		)
	}
	t.Run("sequential cycles per key", func(t *testing.T) {
		e := mk(model.SelectSequential, 0)
		var got, gotT string
		for range 7 {
			_, b := lookup(e, "GET", "/s", "")
			got += b
		}
		if got != "1231231" {
			t.Fatalf("/s sequence = %s", got)
		}
		// /t has its own cursor, untouched by /s.
		for range 3 {
			_, b := lookup(e, "GET", "/t", "")
			gotT += b
		}
		if gotT != "xyx" {
			t.Fatalf("/t sequence = %s", gotT)
		}
	})
	t.Run("sequential cursor is per level key", func(t *testing.T) {
		o := Options{Match: model.MatchConfig{Selection: model.SelectSequential, PathTemplates: true}}
		e := newTestEngine(t, o,
			mkEx(1, "GET", "/u/1", 200, "a"),
			mkEx(2, "GET", "/u/2", 200, "b"),
		)
		var got string
		// Unseen ids all land on the template entry and cycle through it.
		for _, p := range []string{"/u/8", "/u/9", "/u/10", "/u/11"} {
			_, b := lookup(e, "GET", p, "")
			got += b
		}
		if got != "abab" {
			t.Fatalf("template sequence = %s", got)
		}
	})
	t.Run("first", func(t *testing.T) {
		e := mk(model.SelectFirst, 0)
		for range 5 {
			if _, b := lookup(e, "GET", "/s", ""); b != "1" {
				t.Fatalf("first returned %q", b)
			}
		}
	})
	t.Run("random is uniform and reproducible", func(t *testing.T) {
		seq := func(seed uint64) string {
			e := mk(model.SelectRandom, seed)
			var s string
			for range 3000 {
				_, b := lookup(e, "GET", "/s", "")
				s += b
			}
			return s
		}
		a, b := seq(42), seq(42)
		if a != b {
			t.Fatal("same seed gave different sequences")
		}
		if a == seq(43) {
			t.Fatal("different seeds gave the same sequence")
		}
		for _, c := range "123" {
			n := 0
			for _, x := range a {
				if x == c {
					n++
				}
			}
			if n < 850 || n > 1150 {
				t.Fatalf("candidate %c chosen %d/3000 times, want about 1000", c, n)
			}
		}
	})
	t.Run("random without a seed varies", func(t *testing.T) {
		e := mk(model.SelectRandom, 0)
		seen := map[string]bool{}
		for range 200 {
			_, b := lookup(e, "GET", "/s", "")
			seen[b] = true
		}
		if len(seen) != 3 {
			t.Fatalf("saw %v", seen)
		}
	})
	t.Run("subset sequential cycles the matching candidates", func(t *testing.T) {
		o := Options{Match: model.MatchConfig{Query: model.QuerySubset, Body: model.BodyIgnore, Selection: model.SelectSequential}}
		e := newTestEngine(t, o,
			mkEx(1, "GET", "/q?a=1", 200, "a1"),
			mkEx(2, "GET", "/q?b=1", 200, "b1"),
			mkEx(3, "GET", "/q", 200, "none"),
		)
		var got string
		for range 4 {
			_, b := lookup(e, "GET", "/q?a=1", "")
			got += b + ","
		}
		if got != "a1,none,a1,none," {
			t.Fatalf("got %s", got)
		}
	})
}

// Candidate lists follow (StartedAt, Seq), not insertion order.
func TestCandidateOrderIsStartedAtThenSeq(t *testing.T) {
	// Seq is completion order: exchange with Seq 1 started last.
	exs := func() []*model.Exchange {
		return []*model.Exchange{
			mkEx(1, "GET", "/o", 200, "late", withStart(base.Add(30*time.Second))),
			mkEx(2, "GET", "/o", 200, "early", withStart(base.Add(10*time.Second))),
			mkEx(3, "GET", "/o", 200, "mid", withStart(base.Add(20*time.Second))),
			mkEx(5, "GET", "/o", 200, "tie-b", withStart(base.Add(40*time.Second))),
			mkEx(4, "GET", "/o", 200, "tie-a", withStart(base.Add(40*time.Second))),
		}
	}
	want := "early,mid,late,tie-a,tie-b,early"
	cycle := func(e *Engine) string {
		var s string
		for i := range 6 {
			_, b := lookup(e, "GET", "/o", "")
			if i > 0 {
				s += ","
			}
			s += b
		}
		return s
	}
	t.Run("added one by one in arrival order", func(t *testing.T) {
		e := newTestEngine(t, Options{}, exs()...)
		if got := cycle(e); got != want {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("added in reverse order", func(t *testing.T) {
		in := exs()
		for i, j := 0, len(in)-1; i < j; i, j = i+1, j-1 {
			in[i], in[j] = in[j], in[i]
		}
		e := newTestEngine(t, Options{}, in...)
		if got := cycle(e); got != want {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("bulk build sorts once", func(t *testing.T) {
		e := NewEngine(nil, noLatency(Options{}))
		for _, ex := range exs() {
			e.insert(ex, true)
		}
		e.finalize()
		if got := cycle(e); got != want {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("live add keeps order while cycling", func(t *testing.T) {
		e := newTestEngine(t, Options{}, mkEx(1, "GET", "/o", 200, "b", withStart(base.Add(20*time.Second))))
		e.Add(mkEx(2, "GET", "/o", 200, "c", withStart(base.Add(30*time.Second))))
		e.Add(mkEx(3, "GET", "/o", 200, "a", withStart(base.Add(10*time.Second))))
		var s string
		for range 3 {
			_, b := lookup(e, "GET", "/o", "")
			s += b
		}
		if s != "abc" {
			t.Fatalf("got %s", s)
		}
	})
}

func TestErrorExchangesAreNotServed(t *testing.T) {
	e := newTestEngine(t, Options{},
		mkEx(1, "GET", "/e", 504, "", withError("upstream timeout")),
	)
	if l, _ := lookup(e, "GET", "/e", ""); l != LevelMiss {
		t.Fatalf("a failed exchange was served: %s", l)
	}
	if e.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (failed exchanges are indexed for rates)", e.Len())
	}
	e.Add(mkEx(2, "GET", "/e", 200, "ok"))
	for range 5 {
		if _, b := lookup(e, "GET", "/e", ""); b != "ok" {
			t.Fatalf("got %q", b)
		}
	}
}

func TestRecordedErrorStatusesAreServed(t *testing.T) {
	// 5xx and 429 responses are real responses: they are served like any other.
	e := newTestEngine(t, Options{Match: model.MatchConfig{Selection: model.SelectFirst}},
		mkEx(1, "GET", "/x", 503, "down"),
	)
	rec, ok := serve(e, mkReq("GET", "/x"), "")
	if !ok || rec.Code != 503 || bodyOf(rec) != "down" {
		t.Fatalf("ok=%v code=%d body=%q", ok, rec.Code, bodyOf(rec))
	}
}

func TestHeadFallsBackToGet(t *testing.T) {
	e := newTestEngine(t, Options{Match: model.MatchConfig{Selection: model.SelectFirst}},
		mkEx(1, "GET", "/h", 200, "get-body"),
		mkEx(2, "HEAD", "/hh", 200, ""),
		mkEx(3, "GET", "/hh", 200, "should-not-win"),
	)
	if l, b := lookup(e, "HEAD", "/h", ""); l != LevelExact || b != "get-body" {
		t.Fatalf("HEAD without recorded HEAD: (%s,%q)", l, b)
	}
	if l, b := lookup(e, "HEAD", "/hh", ""); l != LevelExact || b != "" {
		t.Fatalf("recorded HEAD must win: (%s,%q)", l, b)
	}
	if l, _ := lookup(e, "HEAD", "/nope", ""); l != LevelMiss {
		t.Fatalf("HEAD miss: %s", l)
	}
	if l, _ := lookup(e, "GET", "/hh", ""); l != LevelExact {
		t.Fatalf("GET unaffected: %s", l)
	}
}

func TestLenAndStats(t *testing.T) {
	e := newTestEngine(t, Options{Match: model.MatchConfig{PathTemplates: true}},
		mkEx(1, "GET", "/users/1?x=1", 200, "a"),
		mkEx(2, "GET", "/users/2", 200, "b"),
	)
	if e.Len() != 2 {
		t.Fatalf("Len = %d", e.Len())
	}
	lookup(e, "GET", "/users/1?x=1", "") // exact
	lookup(e, "GET", "/users/2", "")     // exact
	lookup(e, "GET", "/users/2?z=1", "") // path
	lookup(e, "GET", "/users/3", "")     // template
	lookup(e, "GET", "/nothing", "")     // miss
	lookup(e, "POST", "/users/1", "")    // miss
	s := e.Stats()
	if s.Indexed != 2 || s.Hits != 4 || s.Misses != 2 || s.Injected != 0 {
		t.Fatalf("stats = %+v", s)
	}
	want := map[string]int64{LevelExact: 2, LevelPath: 1, LevelTemplate: 1}
	for k, v := range want {
		if s.ByLevel[k] != v {
			t.Fatalf("ByLevel[%s] = %d, want %d (%v)", k, s.ByLevel[k], v, s.ByLevel)
		}
	}
	if len(s.ByLevel) != 3 {
		t.Fatalf("ByLevel = %v", s.ByLevel)
	}
	// Serve counts like Lookup.
	serve(e, mkReq("GET", "/users/2"), "")
	serve(e, mkReq("GET", "/missing"), "")
	if s2 := e.Stats(); s2.Hits != 5 || s2.Misses != 3 {
		t.Fatalf("stats after Serve = %+v", s2)
	}
}

func TestDefaultsAppliedToZeroOptions(t *testing.T) {
	// NewEngine fills in defaults like Service.WithDefaults, so a bare Options
	// matches with query=exact, body=json, sequential and miss status 404.
	e := NewEngine(nil, Options{Latency: model.LatencyConfig{Mode: model.LatencyNone}})
	t.Cleanup(func() { _ = e.Close() })
	e.Add(mkEx(1, "POST", "/d?b=2&a=1", 200, "x", withReqBody(`{"k":1,"j":2}`)))
	if m := e.Lookup(mkReq("POST", "/d?a=1&b=2"), []byte(`{"j":2,"k":1}`)); m == nil || m.Level != LevelExact {
		t.Fatalf("defaults not applied: %+v", m)
	}
	rec := httpRecorderMiss(e)
	if rec != 404 {
		t.Fatalf("default miss status = %d", rec)
	}
}

func httpRecorderMiss(e *Engine) int {
	rec, _ := serve(e, mkReq("GET", "/none"), "")
	if rec.Code != 200 { // not served: a miss writes nothing
		return rec.Code
	}
	e.WriteMiss(rec, mkReq("GET", "/none"))
	return rec.Code
}

func TestOptionsFromService(t *testing.T) {
	svc := model.Service{
		RecordingID:        "rec_1",
		Match:              model.MatchConfig{Query: model.QuerySubset, IgnoreParams: []string{"_ts"}, MissStatus: 410},
		Latency:            model.LatencyConfig{Mode: model.LatencyFixed, Fixed: model.Duration(5 * time.Millisecond)},
		ErrorAmplification: 4,
		ErrorFloor:         0.1,
	}
	o := OptionsFromService(svc, nil)
	if o.RecordingID != "rec_1" || o.ErrorAmplification != 4 || o.ErrorFloor != 0.1 {
		t.Fatalf("options = %+v", o)
	}
	if o.Match.Query != model.QuerySubset || o.Match.Body != model.BodyJSON || o.Match.Selection != model.SelectSequential || o.Match.MissStatus != 410 {
		t.Fatalf("match defaults not applied: %+v", o.Match)
	}
	if o.Latency.Multiplier != 1 || o.Latency.Mode != model.LatencyFixed {
		t.Fatalf("latency = %+v", o.Latency)
	}
	if o2 := OptionsFromService(model.Service{}, nil); o2.ErrorAmplification != 1 || o2.Latency.Mode != model.LatencyRecorded {
		t.Fatalf("zero service options = %+v", o2)
	}
}

func TestAddAfterCloseIsNoOp(t *testing.T) {
	e := newTestEngine(t, Options{}, mkEx(1, "GET", "/a", 200, "a"))
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	e.Add(mkEx(2, "GET", "/b", 200, "b"))
	e.Add(nil)
	if e.Len() != 1 {
		t.Fatalf("Len = %d after Add on a closed engine", e.Len())
	}
	if l, _ := lookup(e, "GET", "/b", ""); l != LevelMiss {
		t.Fatalf("exchange added after Close was indexed: %s", l)
	}
}

func TestAddHybridMiss(t *testing.T) {
	e := newTestEngine(t, Options{})
	if l, _ := lookup(e, "GET", "/new", ""); l != LevelMiss {
		t.Fatal("empty engine matched")
	}
	e.Add(mkEx(1, "GET", "/new", 200, "fresh"))
	if l, b := lookup(e, "GET", "/new", ""); l != LevelExact || b != "fresh" {
		t.Fatalf("after Add: (%s,%q)", l, b)
	}
}

func TestLookupRequestShapes(t *testing.T) {
	e := newTestEngine(t, Options{}, mkEx(1, "GET", "/", 200, "root"), mkEx(2, "OPTIONS", "*", 200, "star"))
	if l, b := lookup(e, "GET", "/", ""); l != LevelExact || b != "root" {
		t.Fatalf("root: (%s,%q)", l, b)
	}
	// A request with no URL path at all is the root.
	r := &http.Request{Method: "GET", URL: mkReq("GET", "http://x").URL, Header: http.Header{}}
	if m := e.Lookup(r, nil); m == nil {
		t.Fatal("empty path should match /")
	}
	// A request without a URL must not panic.
	if m := e.Lookup(&http.Request{Method: "GET", Header: http.Header{}}, nil); m == nil {
		t.Fatal("nil URL should be treated as /")
	}
}
