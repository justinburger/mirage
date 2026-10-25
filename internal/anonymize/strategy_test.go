package anonymize

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func TestMaskValue(t *testing.T) {
	cases := []struct {
		in           string
		prefix, suff int
		want         string
	}{
		{"1234567890", 0, 0, "**********"},
		{"1234567890", 2, 0, "12********"},
		{"1234567890", 0, 4, "******7890"},
		{"1234567890", 2, 2, "12******90"},
		{"abcd", 2, 2, "****"}, // kept parts cover the value: mask it all
		{"abcd", 3, 3, "****"},
		{"abc", 0, 5, "***"},
		{"", 2, 2, ""},
		{"a", 0, 0, "*"},
		{"héllo wörld", 1, 1, "h*********d"}, // one '*' per character, not per byte
		{"北京市", 1, 0, "北**"},
		{"1234567890", -1, -3, "**********"}, // negative keeps are ignored
	}
	for _, c := range cases {
		if got := maskValue(c.in, c.prefix, c.suff); got != c.want {
			t.Errorf("maskValue(%q,%d,%d) = %q, want %q", c.in, c.prefix, c.suff, got, c.want)
		}
	}
}

func TestApplyStrategies(t *testing.T) {
	k := testKeyer("s")
	v := "Secret-Value-1234"
	cases := []struct {
		name string
		a    action
		want string
	}{
		{"redact", action{strategy: model.StrategyRedact, replacement: defaultReplacement}, "[REDACTED]"},
		{"redact custom", action{strategy: model.StrategyRedact, replacement: "<x>"}, "<x>"},
		{"remove acts as redact", action{strategy: model.StrategyRemove, replacement: defaultReplacement}, "[REDACTED]"},
		{"hash", action{strategy: model.StrategyHash}, k.hashToken(v)},
		{"fake string", action{strategy: model.StrategyFake, fake: model.FakeString}, k.fake(model.FakeString, v)},
		{"mask", action{strategy: model.StrategyMask, keepPrefix: 2, keepSuffix: 2}, maskValue(v, 2, 2)},
	}
	for _, c := range cases {
		if got := k.apply(&c.a, v); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if got := k.apply(&c.a, ""); got != "" {
			t.Errorf("%s: empty value became %q", c.name, got)
		}
	}
}

// scenario places a secret value in one target and reads it back.
type scenario struct {
	target   model.RuleTarget
	selector string
	build    func(v string) *model.Exchange
	// get returns the value found after anonymization and whether the
	// carrier (header, parameter, member, ...) still exists.
	get func(ex *model.Exchange) (string, bool)
	// others checks that unrelated data survived.
	others func(t *testing.T, ex *model.Exchange)
	// removeLeavesRedact: the target cannot delete, so remove acts as redact.
	removeLeavesRedact bool
}

const secretValue = "Secret-Value-1234"

func scenarios() []scenario {
	return []scenario{
		{
			target: model.TargetHeader, selector: "x-custom", // case-insensitive
			build: func(v string) *model.Exchange {
				ex := newExchange()
				ex.Request.Header.Set("X-Custom", v)
				ex.Request.Header.Set("X-Keep", "keep")
				return ex
			},
			get: func(ex *model.Exchange) (string, bool) {
				vs, ok := ex.Request.Header["X-Custom"]
				if !ok || len(vs) == 0 {
					return "", false
				}
				return vs[0], true
			},
			others: func(t *testing.T, ex *model.Exchange) {
				if ex.Request.Header.Get("X-Keep") != "keep" {
					t.Error("unrelated header changed")
				}
			},
		},
		{
			target: model.TargetQuery, selector: "token",
			build: func(v string) *model.Exchange {
				ex := newExchange()
				ex.Request.RawQuery = "keep=1&token=" + url.QueryEscape(v) + "&z=two"
				return ex
			},
			get: func(ex *model.Exchange) (string, bool) {
				q, _ := url.ParseQuery(ex.Request.RawQuery)
				if _, ok := q["token"]; !ok {
					return "", false
				}
				return q.Get("token"), true
			},
			others: func(t *testing.T, ex *model.Exchange) {
				if !strings.HasPrefix(ex.Request.RawQuery, "keep=1&") || !strings.Contains(ex.Request.RawQuery, "z=two") {
					t.Errorf("unrelated parameters changed: %q", ex.Request.RawQuery)
				}
			},
		},
		{
			target: model.TargetCookie, selector: "sid",
			build: func(v string) *model.Exchange {
				ex := newExchange()
				ex.Request.Header.Set("Cookie", "a=1; sid="+v+"; b=2")
				return ex
			},
			get: func(ex *model.Exchange) (string, bool) {
				for _, part := range strings.Split(ex.Request.Header.Get("Cookie"), ";") {
					if n, val, _ := strings.Cut(strings.TrimSpace(part), "="); n == "sid" {
						return val, true
					}
				}
				return "", false
			},
			others: func(t *testing.T, ex *model.Exchange) {
				c := ex.Request.Header.Get("Cookie")
				if !strings.HasPrefix(c, "a=1; ") || !strings.HasSuffix(c, "b=2") {
					t.Errorf("unrelated cookies changed: %q", c)
				}
			},
		},
		{
			target: model.TargetJSON, selector: "$.token",
			build: func(v string) *model.Exchange {
				ex := newExchange()
				ex.Request.Header.Set("Content-Type", "application/json")
				b, _ := json.Marshal(map[string]any{"keep": "k"})
				_ = b
				ex.Request.Body = []byte(`{"keep":"k","token":` + mustJSON(v) + `,"z":1}`)
				ex.Request.BodySize = int64(len(ex.Request.Body))
				return ex
			},
			get: func(ex *model.Exchange) (string, bool) {
				var m map[string]any
				if err := json.Unmarshal(ex.Request.Body, &m); err != nil {
					return "INVALID JSON: " + err.Error(), false
				}
				s, ok := m["token"]
				if !ok {
					return "", false
				}
				str, _ := s.(string)
				return str, true
			},
			others: func(t *testing.T, ex *model.Exchange) {
				if !strings.HasPrefix(string(ex.Request.Body), `{"keep":"k",`) || !strings.HasSuffix(string(ex.Request.Body), `"z":1}`) {
					t.Errorf("unrelated members changed: %s", ex.Request.Body)
				}
			},
		},
		{
			target: model.TargetForm, selector: "token",
			build: func(v string) *model.Exchange {
				ex := newExchange()
				ex.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				ex.Request.Body = []byte("keep=1&token=" + url.QueryEscape(v) + "&z=two")
				ex.Request.BodySize = int64(len(ex.Request.Body))
				return ex
			},
			get: func(ex *model.Exchange) (string, bool) {
				q, _ := url.ParseQuery(string(ex.Request.Body))
				if _, ok := q["token"]; !ok {
					return "", false
				}
				return q.Get("token"), true
			},
			others: func(t *testing.T, ex *model.Exchange) {
				if !strings.HasPrefix(string(ex.Request.Body), "keep=1&") || !strings.HasSuffix(string(ex.Request.Body), "z=two") {
					t.Errorf("unrelated fields changed: %q", ex.Request.Body)
				}
			},
		},
		{
			target: model.TargetRegex, selector: `Secret-Value-\d+`, removeLeavesRedact: true,
			build: func(v string) *model.Exchange {
				ex := newExchange()
				ex.Request.Header.Set("Content-Type", "text/plain")
				ex.Request.Body = []byte("prefix " + v + " suffix")
				ex.Request.BodySize = int64(len(ex.Request.Body))
				return ex
			},
			get: func(ex *model.Exchange) (string, bool) {
				s := string(ex.Request.Body)
				if !strings.HasPrefix(s, "prefix ") || !strings.HasSuffix(s, " suffix") {
					return "BAD FRAMING: " + s, true
				}
				return strings.TrimSuffix(strings.TrimPrefix(s, "prefix "), " suffix"), true
			},
		},
		{
			target: model.TargetPath, selector: "/users/{id}/profile", removeLeavesRedact: true,
			build: func(v string) *model.Exchange {
				ex := newExchange()
				ex.Request.Path = "/users/" + url.PathEscape(v) + "/profile"
				return ex
			},
			get: func(ex *model.Exchange) (string, bool) {
				segs := strings.Split(ex.Request.Path, "/")
				if len(segs) != 4 || segs[1] != "users" || segs[3] != "profile" {
					return "BAD PATH: " + ex.Request.Path, true
				}
				v, err := url.PathUnescape(segs[2])
				if err != nil {
					return "BAD ESCAPE: " + segs[2], true
				}
				return v, true
			},
		},
	}
}

func mustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestEveryStrategyOnEveryTarget(t *testing.T) {
	strategies := []struct {
		name string
		rule func(r model.AnonymizationRule) model.AnonymizationRule
		want func(e *Engine, v string) string
	}{
		{"redact", func(r model.AnonymizationRule) model.AnonymizationRule { return r },
			func(e *Engine, v string) string { return "[REDACTED]" }},
		{"redact custom replacement", func(r model.AnonymizationRule) model.AnonymizationRule { r.Replacement = "<hidden>"; return r },
			func(e *Engine, v string) string { return "<hidden>" }},
		{"hash", func(r model.AnonymizationRule) model.AnonymizationRule { return r },
			func(e *Engine, v string) string { return e.k.hashToken(v) }},
		{"fake string", func(r model.AnonymizationRule) model.AnonymizationRule { r.Fake = model.FakeString; return r },
			func(e *Engine, v string) string { return e.k.fake(model.FakeString, v) }},
		{"fake email", func(r model.AnonymizationRule) model.AnonymizationRule { r.Fake = model.FakeEmail; return r },
			func(e *Engine, v string) string { return e.k.fake(model.FakeEmail, v) }},
		{"fake auto", func(r model.AnonymizationRule) model.AnonymizationRule { r.Fake = model.FakeAuto; return r },
			func(e *Engine, v string) string { return e.k.fake(model.FakeAuto, v) }},
		{"mask keep 2+2", func(r model.AnonymizationRule) model.AnonymizationRule { r.KeepPrefix, r.KeepSuffix = 2, 2; return r },
			func(e *Engine, v string) string { return maskValue(v, 2, 2) }},
		{"mask all", func(r model.AnonymizationRule) model.AnonymizationRule { return r },
			func(e *Engine, v string) string { return maskValue(v, 0, 0) }},
		{"remove", func(r model.AnonymizationRule) model.AnonymizationRule { return r },
			nil}, // target dependent, handled below
	}
	strategyOf := map[string]model.Strategy{
		"redact": model.StrategyRedact, "redact custom replacement": model.StrategyRedact,
		"hash": model.StrategyHash, "fake string": model.StrategyFake, "fake email": model.StrategyFake,
		"fake auto": model.StrategyFake, "mask keep 2+2": model.StrategyMask, "mask all": model.StrategyMask,
		"remove": model.StrategyRemove,
	}
	for _, sc := range scenarios() {
		for _, st := range strategies {
			t.Run(string(sc.target)+"/"+st.name, func(t *testing.T) {
				r := rl(sc.target, sc.selector, strategyOf[st.name])
				r = st.rule(r)
				e := mustEngine(t, bareProfile(r))
				ex := sc.build(secretValue)
				if err := e.Exchange(ex); err != nil {
					t.Fatal(err)
				}
				got, exists := sc.get(ex)
				switch {
				case st.name == "remove" && !sc.removeLeavesRedact:
					if exists {
						t.Fatalf("remove left the carrier in place (value %q)", got)
					}
				case st.name == "remove":
					if !exists || got != "[REDACTED]" {
						t.Fatalf("remove on %s = %q (exists=%v), want [REDACTED]", sc.target, got, exists)
					}
				default:
					if !exists {
						t.Fatal("carrier disappeared")
					}
					if want := st.want(e, secretValue); got != want {
						t.Fatalf("got %q, want %q", got, want)
					}
					if got == secretValue {
						t.Fatal("value not anonymized")
					}
				}
				if sc.others != nil {
					sc.others(t, ex)
				}
			})
		}
	}
}

func TestRuleScopeAndEnabled(t *testing.T) {
	build := func() *model.Exchange {
		ex := newExchange()
		ex.Request.Header.Set("X-Custom", secretValue)
		ex.Response.Header.Set("X-Custom", secretValue)
		return ex
	}
	cases := []struct {
		name                       string
		rule                       model.AnonymizationRule
		wantReqChanged, wantRespCh bool
	}{
		{"both", rl(model.TargetHeader, "X-Custom", model.StrategyRedact), true, true},
		{"request only", withScope(rl(model.TargetHeader, "X-Custom", model.StrategyRedact), model.ScopeRequest), true, false},
		{"response only", withScope(rl(model.TargetHeader, "X-Custom", model.StrategyRedact), model.ScopeResponse), false, true},
		{"disabled", func() model.AnonymizationRule {
			r := rl(model.TargetHeader, "X-Custom", model.StrategyRedact)
			r.Enabled = false
			return r
		}(), false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := mustEngine(t, bareProfile(c.rule))
			ex := build()
			if err := e.Exchange(ex); err != nil {
				t.Fatal(err)
			}
			if got := ex.Request.Header.Get("X-Custom") != secretValue; got != c.wantReqChanged {
				t.Errorf("request changed = %v, want %v", got, c.wantReqChanged)
			}
			if got := ex.Response.Header.Get("X-Custom") != secretValue; got != c.wantRespCh {
				t.Errorf("response changed = %v, want %v", got, c.wantRespCh)
			}
		})
	}
}

func TestRulesRunInOrder(t *testing.T) {
	// redact then hash: the hash sees the redaction marker.
	e := mustEngine(t, bareProfile(
		rl(model.TargetHeader, "X-A", model.StrategyRedact),
		rl(model.TargetHeader, "X-A", model.StrategyHash),
	))
	ex := newExchange()
	ex.Request.Header.Set("X-A", "v")
	if err := e.Exchange(ex); err != nil {
		t.Fatal(err)
	}
	if got, want := ex.Request.Header.Get("X-A"), e.k.hashToken("[REDACTED]"); got != want {
		t.Errorf("redact,hash: got %q, want %q", got, want)
	}

	// hash then mask: the mask sees the token.
	m := rl(model.TargetHeader, "X-A", model.StrategyMask)
	m.KeepPrefix = 2
	e = mustEngine(t, bareProfile(rl(model.TargetHeader, "X-A", model.StrategyHash), m))
	ex = newExchange()
	ex.Request.Header.Set("X-A", "v")
	if err := e.Exchange(ex); err != nil {
		t.Fatal(err)
	}
	if got, want := ex.Request.Header.Get("X-A"), maskValue(e.k.hashToken("v"), 2, 0); got != want {
		t.Errorf("hash,mask: got %q, want %q", got, want)
	}

	// A remove anywhere in the chain wins.
	e = mustEngine(t, bareProfile(
		rl(model.TargetHeader, "X-A", model.StrategyHash),
		rl(model.TargetHeader, "X-A", model.StrategyRemove),
	))
	ex = newExchange()
	ex.Request.Header.Set("X-A", "v")
	if err := e.Exchange(ex); err != nil {
		t.Fatal(err)
	}
	if _, ok := ex.Request.Header["X-A"]; ok {
		t.Error("remove did not delete the header")
	}
}

func TestRuleBeatsDetectorAndAvoidsDoubleTransform(t *testing.T) {
	p := testProfile(rl(model.TargetJSON, "$.email", model.StrategyHash))
	e := mustEngine(t, p)
	got := anonJSON(t, e, `{"email":"jane@acme.io","other":"bob@acme.io"}`)
	var m map[string]string
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatal(err)
	}
	if m["email"] != e.k.hashToken("jane@acme.io") {
		t.Errorf("rule-handled value = %q, want the hash token", m["email"])
	}
	if !strings.HasSuffix(m["other"], "@example.com") && !strings.HasSuffix(m["other"], "@example.net") && !strings.HasSuffix(m["other"], "@example.org") {
		t.Errorf("detector did not fake the other address: %q", m["other"])
	}
}

func TestRegexRuleGroupAndOrder(t *testing.T) {
	// With a capture group only the group is replaced.
	e := mustEngine(t, bareProfile(rl(model.TargetRegex, `password=(\w+)`, model.StrategyRedact)))
	got := anonResp(t, e, "text/plain", "user=bob&password=hunter2&x=1 password=abc")
	if got != "user=bob&password=[REDACTED]&x=1 password=[REDACTED]" {
		t.Errorf("group replacement: %q", got)
	}
	// Without a group the whole match is replaced.
	e = mustEngine(t, bareProfile(rl(model.TargetRegex, `SECRET-\d+`, model.StrategyMask)))
	got = anonResp(t, e, "text/plain", "a SECRET-123 b SECRET-45678")
	if got != "a ********** b ************" {
		t.Errorf("whole-match mask: %q", got)
	}
	// Optional group that did not participate: nothing is replaced.
	e = mustEngine(t, bareProfile(rl(model.TargetRegex, `id=(\d+)?x`, model.StrategyRedact)))
	if got := anonResp(t, e, "text/plain", "id=x"); got != "id=x" {
		t.Errorf("non-participating group changed the text: %q", got)
	}
	// Empty matches are ignored.
	e = mustEngine(t, bareProfile(rl(model.TargetRegex, `a*`, model.StrategyRedact)))
	if got := anonResp(t, e, "text/plain", "bbb"); got != "bbb" {
		t.Errorf("empty matches changed the text: %q", got)
	}
}

func TestRegexRuleReachesEveryTextualValue(t *testing.T) {
	e := mustEngine(t, bareProfile(rl(model.TargetRegex, `ACME-\d+`, model.StrategyHash)))
	tok := e.k.hashToken("ACME-123")
	ex := newExchange()
	ex.Request.Path = "/acct/ACME-123/x"
	ex.Request.RawQuery = "a=ACME-123&b=1"
	ex.Request.Header.Set("X-Ref", "see ACME-123")
	ex.Request.Header.Set("Content-Length", "12")
	ex.Request.Header.Set("Cookie", "c=ACME-123")
	ex.Request.Header.Set("Content-Type", "application/json")
	ex.Request.Body = []byte(`{"k":"ACME-123","n":["x ACME-123 y"]}`)
	ex.Request.BodySize = int64(len(ex.Request.Body))
	if err := e.Exchange(ex); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{
		"path": ex.Request.Path, "query": ex.Request.RawQuery, "header": ex.Request.Header.Get("X-Ref"),
		"cookie": ex.Request.Header.Get("Cookie"), "body": string(ex.Request.Body),
	} {
		if strings.Contains(v, "ACME-123") || !strings.Contains(v, tok) {
			t.Errorf("%s: %q not anonymized", name, v)
		}
	}
	if ex.Request.Header.Get("Content-Type") != "application/json" {
		t.Error("Content-Type touched by a regex rule")
	}
}

func TestPathRuleVariants(t *testing.T) {
	cases := []struct {
		name, selector, in string
		hit                []string // decoded segments that must be anonymized
		keep               []string // decoded segments that must survive
	}{
		{"two params", "/users/{id}/orders/{order}", "/users/42/orders/9001", []string{"42", "9001"}, []string{"users", "orders"}},
		{"trailing slash", "/users/{id}", "/users/42/", []string{"42"}, []string{"users"}},
		{"prefix and suffix", "/files/v{version}", "/files/v12", []string{"12"}, []string{"files", "v"}},
		{"suffix only", "/files/{name}.json", "/files/report.json", []string{"report"}, []string{"files", ".json"}},
		{"rest", "/blob/{path...}", "/blob/a/b/c", []string{"a", "b", "c"}, []string{"blob"}},
		{"encoded value", "/u/{id}", "/u/jane%40acme.io", []string{"jane@acme.io"}, []string{"u"}},
		{"no match: more segments", "/users/{id}", "/users/42/extra", nil, []string{"users", "42", "extra"}},
		{"no match: literal differs", "/users/{id}", "/people/42", nil, []string{"people", "42"}},
		{"no match: empty param", "/files/v{version}", "/files/v", nil, []string{"files"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := mustEngine(t, bareProfile(rl(model.TargetPath, c.selector, model.StrategyHash)))
			ex := newExchange()
			ex.Request.Path = c.in
			if err := e.Exchange(ex); err != nil {
				t.Fatal(err)
			}
			got := ex.Request.Path
			for _, h := range c.hit {
				if !strings.Contains(got, e.k.hashToken(h)) {
					t.Errorf("segment %q not hashed in %q", h, got)
				}
			}
			for _, k := range c.keep {
				if !strings.Contains(got, k) {
					t.Errorf("segment %q lost in %q", k, got)
				}
			}
			if strings.HasSuffix(c.in, "/") != strings.HasSuffix(got, "/") {
				t.Errorf("trailing slash changed: %q -> %q", c.in, got)
			}
		})
	}
}

func TestPathSegmentsAreURLSafe(t *testing.T) {
	for _, st := range []model.Strategy{model.StrategyRedact, model.StrategyHash, model.StrategyFake, model.StrategyMask} {
		r := rl(model.TargetPath, "/u/{id}", st)
		r.Fake = model.FakeName
		e := mustEngine(t, bareProfile(r))
		ex := newExchange()
		ex.Request.Path = "/u/Jane Q Smith"
		if err := e.Exchange(ex); err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(ex.Request.Path)
		if err != nil || u.EscapedPath() != ex.Request.Path {
			t.Errorf("%s: path %q is not a valid escaped path (err %v)", st, ex.Request.Path, err)
		}
		if strings.ContainsAny(ex.Request.Path, " []{}|\\^`<>\"") {
			t.Errorf("%s: path %q contains unsafe characters", st, ex.Request.Path)
		}
		if strings.Count(ex.Request.Path, "/") != 2 {
			t.Errorf("%s: path %q gained or lost a segment", st, ex.Request.Path)
		}
	}
}

func TestHeaderNamesCaseInsensitiveButQueryExact(t *testing.T) {
	e := mustEngine(t, bareProfile(
		rl(model.TargetHeader, "X-API-KEY", model.StrategyRedact),
		rl(model.TargetQuery, "Token", model.StrategyRedact),
	))
	ex := newExchange()
	lowerKey := "x-api-" + "key" // non-canonical, as hand-built headers have
	ex.Request.Header[lowerKey] = []string{"abc"}
	ex.Request.RawQuery = "token=a&Token=b"
	if err := e.Exchange(ex); err != nil {
		t.Fatal(err)
	}
	if got := ex.Request.Header[lowerKey]; len(got) != 1 || got[0] != "[REDACTED]" {
		t.Errorf("header with a non-canonical key: %v", ex.Request.Header)
	}
	if ex.Request.RawQuery != "token=a&Token=%5BREDACTED%5D" {
		t.Errorf("query parameter names are case sensitive: %q", ex.Request.RawQuery)
	}
}

var _ = http.Header{}
