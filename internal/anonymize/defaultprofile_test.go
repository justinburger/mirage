package anonymize

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

// builtinEngine is an engine for the real built-in profile, rules included
// (testProfile and defaultEngine replace the rules).
func builtinEngine(t testing.TB) *Engine {
	t.Helper()
	p := model.DefaultAnonymizationProfile()
	p.Secret = testSecret
	return mustEngine(t, p)
}

// jsonAt walks a decoded JSON document.
func jsonAt(t testing.TB, doc string, path ...string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, doc)
	}
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("no object at %v in %s", path, doc)
		}
		if v, ok = m[p]; !ok {
			t.Fatalf("no %q in %s", p, doc)
		}
	}
	return v
}

// sides runs fn once for the request side and once for the response side:
// the built-in rules have scope both.
func sides(t *testing.T, fn func(t *testing.T, anon func(e *Engine, ct, body string) string)) {
	t.Run("request", func(t *testing.T) {
		fn(t, func(e *Engine, ct, body string) string { return string(anonReq(t, e, ct, body).Body) })
	})
	t.Run("response", func(t *testing.T) {
		fn(t, func(e *Engine, ct, body string) string { return anonResp(t, e, ct, body) })
	})
}

func TestBuiltinProfileLoginBody(t *testing.T) {
	e := builtinEngine(t)
	sides(t, func(t *testing.T, anon func(*Engine, string, string) string) {
		out := anon(e, "application/json", `{"email":"jane.doe@acme.io","password":"hunter2"}`)
		if pw := jsonAt(t, out, "password"); pw != "[REDACTED]" {
			t.Errorf("password = %v, want redacted", pw)
		}
		email, _ := jsonAt(t, out, "email").(string)
		if email == "jane.doe@acme.io" || strings.Contains(out, "acme.io") {
			t.Errorf("e-mail not faked: %s", out)
		}
		if want := e.k.fake(model.FakeEmail, "jane.doe@acme.io"); email != want {
			t.Errorf("e-mail = %q, want the consistent fake %q", email, want)
		}
		if !strings.Contains(email, "@example.") || strings.Contains(out, "hunter2") {
			t.Errorf("unexpected output: %s", out)
		}
	})
}

func TestBuiltinProfileTokensAreHashed(t *testing.T) {
	e := builtinEngine(t)
	sides(t, func(t *testing.T, anon func(*Engine, string, string) string) {
		out := anon(e, "application/json", `{"accessToken":"abc", "refresh_token":"def"}`)
		if got := jsonAt(t, out, "accessToken"); got != e.k.hashToken("abc") {
			t.Errorf("accessToken = %v, want %s", got, e.k.hashToken("abc"))
		}
		if got := jsonAt(t, out, "refresh_token"); got != e.k.hashToken("def") {
			t.Errorf("refresh_token = %v, want %s", got, e.k.hashToken("def"))
		}
		if strings.Contains(out, `"abc"`) || strings.Contains(out, `"def"`) {
			t.Errorf("a token survived: %s", out)
		}
	})
}

func TestBuiltinProfileNamesAreFaked(t *testing.T) {
	e := builtinEngine(t)
	sides(t, func(t *testing.T, anon func(*Engine, string, string) string) {
		out := anon(e, "application/json", `{"user":{"firstName":"Jane","lastName":"Doe"}}`)
		first, last := jsonAt(t, out, "user", "firstName"), jsonAt(t, out, "user", "lastName")
		if first == "Jane" || last == "Doe" {
			t.Errorf("names not faked: %s", out)
		}
		if first != e.k.fake(model.FakeName, "Jane") || last != e.k.fake(model.FakeName, "Doe") {
			t.Errorf("names are not the consistent fakes: %s", out)
		}
		for _, v := range []any{first, last} {
			s, _ := v.(string)
			if s == "" || strings.ContainsAny(s, "0123456789@") {
				t.Errorf("a fake name should look like a name, got %q", s)
			}
		}
		// The same person maps to the same fake everywhere.
		again := anon(e, "application/json", `[{"first_name":"Jane"},{"full_name":"Jane"}]`)
		if strings.Count(again, e.k.fake(model.FakeName, "Jane")) != 2 {
			t.Errorf("Jane is not consistent: %s", again)
		}
	})
}

func TestBuiltinProfileQueryCredentialsAreHashed(t *testing.T) {
	e := builtinEngine(t)
	cases := []struct {
		name, raw string
		want      map[string]string // decoded values by name after anonymization
	}{
		{"api_key", "api_key=XYZ", map[string]string{"api_key": e.k.hashToken("XYZ")}},
		{"access_token", "access_token=tok-9", map[string]string{"access_token": e.k.hashToken("tok-9")}},
		{"both beside ordinary params", "page=2&api_key=XYZ&q=boots&access_token=tok-9",
			map[string]string{"page": "2", "api_key": e.k.hashToken("XYZ"), "q": "boots", "access_token": e.k.hashToken("tok-9")}},
		{"escaped value", "access_token=a%2Fb%3D%3D", map[string]string{"access_token": e.k.hashToken("a/b==")}},
		{"other credential names", "apikey=k1&signature=s1&password=p1&session=x1",
			map[string]string{"apikey": e.k.hashToken("k1"), "signature": e.k.hashToken("s1"), "password": e.k.hashToken("p1"), "session": e.k.hashToken("x1")}},
		// An empty value has nothing to hide, and stays empty.
		{"empty value", "access_token=&page=1", map[string]string{"access_token": "", "page": "1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := &model.Request{Method: "GET", Path: "/api", RawQuery: c.raw, Header: http.Header{}}
			if err := e.Request(req); err != nil {
				t.Fatal(err)
			}
			got, err := url.ParseQuery(req.RawQuery)
			if err != nil {
				t.Fatalf("output query %q: %v", req.RawQuery, err)
			}
			for k, want := range c.want {
				if got.Get(k) != want {
					t.Errorf("%s = %q, want %q (query %q)", k, got.Get(k), want, req.RawQuery)
				}
			}
			if len(got) != len(c.want) {
				t.Errorf("parameters changed: %q", req.RawQuery)
			}
			for _, secret := range []string{"XYZ", "tok-9", "k1=", "s1", "p1", "x1"} {
				if strings.Contains(req.RawQuery, secret) {
					t.Errorf("%q survived in %q", secret, req.RawQuery)
				}
			}
		})
	}
}

func TestBuiltinProfileFormCredentials(t *testing.T) {
	e := builtinEngine(t)
	req := anonReq(t, e, "application/x-www-form-urlencoded; charset=UTF-8",
		"username=jane&password=hunter2&access_token=abc&remember=on&client_secret=s3cr3t")
	got, err := url.ParseQuery(string(req.Body))
	if err != nil {
		t.Fatalf("output %q: %v", req.Body, err)
	}
	want := map[string]string{
		"username": "jane", "remember": "on",
		"password": "[REDACTED]", "client_secret": "[REDACTED]", "access_token": e.k.hashToken("abc"),
	}
	for k, w := range want {
		if got.Get(k) != w {
			t.Errorf("%s = %q, want %q (body %q)", k, got.Get(k), w, req.Body)
		}
	}
	if strings.Contains(string(req.Body), "hunter2") || strings.Contains(string(req.Body), "s3cr3t") {
		t.Errorf("a secret survived: %q", req.Body)
	}
	if req.BodySize != int64(len(req.Body)) {
		t.Errorf("BodySize %d, body %d", req.BodySize, len(req.Body))
	}
}

func TestBuiltinProfileFormCredentialsInMultipart(t *testing.T) {
	e := builtinEngine(t)
	in := buildMP("bb", "\r\n",
		tpart{name: "username", body: "jane"},
		tpart{name: "password", body: "hunter2"},
		tpart{name: "access_token", body: "abc"},
	)
	req := mpRequest(t, e, mpCT("bb"), in, false, 0)
	parts := readMP(t, string(req.Body), "bb")
	pw, _ := mpByName(parts, "password")
	tok, _ := mpByName(parts, "access_token")
	user, _ := mpByName(parts, "username")
	if pw.body != "[REDACTED]" || tok.body != e.k.hashToken("abc") || user.body != "jane" {
		t.Errorf("multipart credentials: %+v", parts)
	}
}

// TestBuiltinProfileEveryRuleWorks checks each rule of the built-in profile
// on its own: a rule that compiled to nothing, or that never matches the
// shape it advertises, fails here.
func TestBuiltinProfileEveryRuleWorks(t *testing.T) {
	const val = "Zorbaxian"
	e := builtinEngine(t)
	p := model.DefaultAnonymizationProfile()
	if len(p.Rules) == 0 {
		t.Fatal("the built-in profile has no rules")
	}
	if err := Validate(p); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	counts := map[model.RuleTarget]int{}
	for _, r := range p.Rules {
		if ids[r.ID] {
			t.Errorf("duplicate rule id %q", r.ID)
		}
		ids[r.ID] = true
		counts[r.Target]++
	}
	// Every rule is enabled for both sides and reached the compiled form.
	for _, sd := range []*sideRules{&e.c.req, &e.c.resp} {
		got := map[model.RuleTarget]int{
			model.TargetJSON: len(sd.json), model.TargetQuery: 0, model.TargetForm: 0,
		}
		for _, rs := range sd.query {
			got[model.TargetQuery] += len(rs)
		}
		for _, rs := range sd.form {
			got[model.TargetForm] += len(rs)
		}
		for _, tg := range []model.RuleTarget{model.TargetJSON, model.TargetQuery, model.TargetForm} {
			if got[tg] != counts[tg] {
				t.Errorf("%s rules compiled: %d, profile has %d", tg, got[tg], counts[tg])
			}
		}
	}

	expect := func(t *testing.T, r model.AnonymizationRule, got string) {
		t.Helper()
		switch r.Strategy {
		case model.StrategyRedact:
			if got != defaultReplacement {
				t.Errorf("%s: got %q, want redacted", r.Name, got)
			}
		case model.StrategyHash:
			if got != e.k.hashToken(val) {
				t.Errorf("%s: got %q, want a hash token", r.Name, got)
			}
		case model.StrategyFake:
			if got == val || got != e.k.fake(r.Fake, val) {
				t.Errorf("%s: got %q, want the %s fake of %q", r.Name, got, r.Fake, val)
			}
		default:
			t.Errorf("%s: unexpected strategy %s", r.Name, r.Strategy)
		}
	}
	for _, r := range p.Rules {
		switch r.Target {
		case model.TargetJSON:
			key := strings.TrimPrefix(r.Selector, "$..")
			if key == r.Selector || key == "" {
				t.Errorf("%s: unexpected selector %q", r.Name, r.Selector)
				continue
			}
			for name, body := range map[string]string{
				"root member":      `{"` + key + `":"` + val + `"}`,
				"nested member":    `{"a":{"b":[{"` + key + `":"` + val + `","other":"keep"}]}}`,
				"beside siblings":  `{"id":7,"` + key + `":"` + val + `","note":"hello"}`,
				"inside an array":  `[{"x":1},{"` + key + `":"` + val + `"}]`,
				"after a big text": `{"pad":"` + strings.Repeat("p", 300) + `","` + key + `":"` + val + `"}`,
			} {
				for side, anon := range map[string]func() string{
					"request":  func() string { return string(anonReq(t, e, "application/json", body).Body) },
					"response": func() string { return anonResp(t, e, "application/json", body) },
				} {
					out := anon()
					if strings.Contains(out, val) {
						t.Errorf("%s (%s, %s): %q survived: %s", r.Name, side, name, val, out)
						continue
					}
					var doc any
					if err := json.Unmarshal([]byte(out), &doc); err != nil {
						t.Errorf("%s (%s, %s): output is not JSON: %v", r.Name, side, name, err)
						continue
					}
					var found []string
					var walk func(v any)
					walk = func(v any) {
						switch x := v.(type) {
						case map[string]any:
							for k, c := range x {
								if s, ok := c.(string); ok && k == key {
									found = append(found, s)
								}
								walk(c)
							}
						case []any:
							for _, c := range x {
								walk(c)
							}
						}
					}
					walk(doc)
					if len(found) != 1 {
						t.Errorf("%s (%s, %s): %d values for %q in %s", r.Name, side, name, len(found), key, out)
						continue
					}
					expect(t, r, found[0])
				}
			}
		case model.TargetForm:
			req := anonReq(t, e, "application/x-www-form-urlencoded", "a=1&"+r.Selector+"="+val+"&b=2")
			q, err := url.ParseQuery(string(req.Body))
			if err != nil {
				t.Errorf("%s: %v", r.Name, err)
				continue
			}
			if q.Get("a") != "1" || q.Get("b") != "2" {
				t.Errorf("%s: neighbours changed: %q", r.Name, req.Body)
			}
			expect(t, r, q.Get(r.Selector))
		case model.TargetQuery:
			req := &model.Request{Method: "GET", Path: "/x", RawQuery: "a=1&" + r.Selector + "=" + val + "&b=2", Header: http.Header{}}
			if err := e.Request(req); err != nil {
				t.Fatal(err)
			}
			q, err := url.ParseQuery(req.RawQuery)
			if err != nil {
				t.Errorf("%s: %v", r.Name, err)
				continue
			}
			if q.Get("a") != "1" || q.Get("b") != "2" {
				t.Errorf("%s: neighbours changed: %q", r.Name, req.RawQuery)
			}
			expect(t, r, q.Get(r.Selector))
		default:
			t.Errorf("%s: unexpected target %s", r.Name, r.Target)
		}
	}
}

func TestBuiltinProfileLeavesOrdinaryTrafficAlone(t *testing.T) {
	e := builtinEngine(t)
	body := `{"id":7,"name":"Widget","tokens_remaining":5,"passport":"X","keyboard":"qwerty","items":[{"sku":"A-1","qty":2}]}`
	if got := anonResp(t, e, "application/json", body); got != body {
		t.Errorf("ordinary JSON changed:\n in: %s\nout: %s", body, got)
	}
	req := &model.Request{Method: "GET", Path: "/search", RawQuery: "q=boots&page=2&sort=price&keyword=a", Header: http.Header{}}
	if err := e.Request(req); err != nil {
		t.Fatal(err)
	}
	if req.RawQuery != "q=boots&page=2&sort=price&keyword=a" {
		t.Errorf("ordinary query changed: %q", req.RawQuery)
	}
	if got := string(anonReq(t, e, "application/x-www-form-urlencoded", "q=boots&qty=2").Body); got != "q=boots&qty=2" {
		t.Errorf("ordinary form changed: %q", got)
	}
}

func TestBuiltinProfileInAFullExchange(t *testing.T) {
	e := builtinEngine(t)
	ex := newExchange()
	ex.Request.Method = "POST"
	ex.Request.Path = "/login"
	ex.Request.RawQuery = "api_key=XYZ&next=%2Fhome"
	ex.Request.Header.Set("Authorization", "Bearer abc")
	ex.Request.Header.Set("Content-Type", "application/json; charset=utf-8")
	ex.Request.Body = []byte(`{"email":"jane.doe@acme.io","password":"hunter2","profile":{"firstName":"Jane","lastName":"Doe"}}`)
	ex.Request.BodySize = int64(len(ex.Request.Body))
	ex.Request.Header.Set("Content-Length", "99")
	ex.Response.Header.Set("Content-Type", "application/json")
	ex.Response.Body = []byte(`{"accessToken":"abc","refresh_token":"def","user":{"first_name":"Jane"}}`)
	ex.Response.BodySize = int64(len(ex.Response.Body))
	changes, err := e.ExchangeWithReport(ex)
	if err != nil {
		t.Fatal(err)
	}
	all := string(ex.Request.Body) + string(ex.Response.Body) + ex.Request.RawQuery
	for _, secret := range []string{"hunter2", "acme.io", `"abc"`, `"def"`, "XYZ", "Jane", "Doe"} {
		if strings.Contains(all, secret) {
			t.Errorf("%q survived: %s", secret, all)
		}
	}
	// The same token is the same hash in a header and in a body, so sessions
	// stay linkable.
	if got := ex.Request.Header.Get("Authorization"); got != "Bearer "+e.k.hashToken("abc") {
		t.Errorf("Authorization = %q", got)
	}
	if jsonAt(t, string(ex.Response.Body), "accessToken") != e.k.hashToken("abc") {
		t.Errorf("accessToken does not match the bearer hash: %s", ex.Response.Body)
	}
	for _, w := range []struct{ loc, source string }{
		{"request.body $.password", "JSON password"},
		{"request.body $.profile.firstName", "JSON firstName"},
		{"request.query.api_key", "Query api_key"},
		{"response.body $.accessToken", "JSON accessToken"},
		{"response.body $.refresh_token", "JSON refresh_token"},
		{"response.body $.user.first_name", "JSON first_name"},
		{"request.body $.email", "detector:email"},
	} {
		ch, ok := changeAt(changes, w.loc)
		if !ok || ch.Source != w.source {
			t.Errorf("%s: %+v (found=%v), want source %q", w.loc, ch, ok, w.source)
		}
	}
	if !ex.Anonymized || ex.Request.Header.Get("Content-Length") != itoa(len(ex.Request.Body)) {
		t.Errorf("bookkeeping: anonymized=%v CL=%q body=%d", ex.Anonymized, ex.Request.Header.Get("Content-Length"), len(ex.Request.Body))
	}
}

func TestEngineProfileNeverExposesTheSecret(t *testing.T) {
	p := testProfile(rl(model.TargetHeader, "X-A", model.StrategyHash))
	e := mustEngine(t, p)
	got := e.Profile()
	if got.Secret != "" || !got.HasSecret {
		t.Errorf("Profile(): Secret %q, HasSecret %v", got.Secret, got.HasSecret)
	}
	if blob, _ := json.Marshal(got); strings.Contains(string(blob), testSecret) {
		t.Errorf("the secret is in the profile JSON: %s", blob)
	}
	if want := cloneProfile(p).WithDefaults().Redacted(); !reflect.DeepEqual(got, want) {
		t.Errorf("Profile() differs from the redacted profile:\n got %+v\nwant %+v", got, want)
	}
	if p.Secret != testSecret {
		t.Error("New modified the caller's profile")
	}
	salted := e.WithSalt("clone")
	if sp := salted.Profile(); sp.Secret != "" || !sp.HasSecret {
		t.Errorf("salted Profile(): Secret %q, HasSecret %v", sp.Secret, sp.HasSecret)
	}
	got.Secret = "mutated"
	if e.Profile().Secret != "" {
		t.Error("Profile() exposes the engine's profile")
	}

	// No secret given: the engine picks a random one; the profile has none.
	p.Secret = ""
	if np := mustEngine(t, p).Profile(); np.Secret != "" || np.HasSecret {
		t.Errorf("no-secret profile: Secret %q, HasSecret %v", np.Secret, np.HasSecret)
	}
	// The built-in profile comes back with its rules.
	d := model.DefaultAnonymizationProfile()
	d.Secret = testSecret
	if dp := mustEngine(t, d).Profile(); len(dp.Rules) != len(d.Rules) || dp.Secret != "" || dp.OpaqueBodies != model.OpaqueKeep {
		t.Errorf("built-in Profile(): %d rules, secret %q, opaque %q", len(dp.Rules), dp.Secret, dp.OpaqueBodies)
	}
}
