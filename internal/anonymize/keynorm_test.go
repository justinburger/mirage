package anonymize

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func TestNormKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"api_key", "apikey"},
		{"apiKey", "apikey"},
		{"API-KEY", "apikey"},
		{"apikey", "apikey"},
		{"Api_-Key", "apikey"},
		{"", ""},
		{"___", ""},
		{"user[password]", "user[password]"},
		{"X-Amz-Signature", "xamzsignature"},
		{"Ünïcode_Key", "Ünïcodekey"}, // only ASCII letters fold
	}
	for _, c := range cases {
		if got := normKey(c.in); got != c.want {
			t.Errorf("normKey(%q) = %q, want %q", c.in, got, c.want)
		}
		if got := string(appendNormKey(nil, []byte(c.in))); got != c.want {
			t.Errorf("appendNormKey(%q) = %q, want %q", c.in, got, c.want)
		}
		var scratch []byte
		if got := string(foldKey(&scratch, []byte(c.in))); got != c.want {
			t.Errorf("foldKey(%q) = %q, want %q", c.in, got, c.want)
		}
		if !normEqual([]byte(c.in), c.want) {
			t.Errorf("normEqual(%q, %q) = false", c.in, c.want)
		}
	}
	for _, c := range []struct{ key, norm string }{
		{"apiKey", "apikeys"}, {"apiKe", "apikey"}, {"api", "apikey"}, {"apikeyx", "apikey"}, {"", "a"}, {"a", ""},
	} {
		if normEqual([]byte(c.key), c.norm) {
			t.Errorf("normEqual(%q, %q) = true", c.key, c.norm)
		}
	}
}

func TestBracketPart(t *testing.T) {
	for in, want := range map[string]string{
		"user[password]":     "password",
		"a[b][c]":            "c",
		"a[b][]":             "b",
		"passwords[]":        "",
		"password":           "",
		"a[]":                "",
		"[x]":                "x",
		"user[password":      "",
		"user]":              "",
		"":                   "",
		"user[profile][pin]": "pin",
	} {
		if got := bracketPart(in); got != want {
			t.Errorf("bracketPart(%q) = %q, want %q", in, got, want)
		}
	}
}

// credentialKeys lists spellings of credential member names that the
// built-in profile must catch, with the strategy its rule uses.
var credentialKeys = []struct {
	key  string
	hash bool // hashed; otherwise redacted
}{
	{"password", false}, {"Password", false}, {"PASSWORD", false}, {"user_password", false},
	{"new_password", false}, {"newPassword", false}, {"passphrase", false}, {"private_key", false},
	{"privateKey", false}, {"ClientSecret", false}, {"client-secret", false},
	{"AccessToken", true}, {"access-token", true}, {"ACCESS_TOKEN", true}, {"api-key", true},
	{"ApiKey", true}, {"API_KEY", true}, {"jwt", true}, {"JWT", true}, {"session", true},
	{"csrf_token", true}, {"CsrfToken", true}, {"Authorization", true}, {"refresh-token", true},
}

func TestBuiltinProfileMatchesCredentialKeysIgnoringCaseAndSeparators(t *testing.T) {
	e := builtinEngine(t)
	sides(t, func(t *testing.T, anon func(*Engine, string, string) string) {
		for _, c := range credentialKeys {
			val := "S3cr3t-" + c.key
			// At the top, nested in an object and inside an array element.
			in := `{"` + c.key + `":"` + val + `","a":{"b":[{"` + c.key + `":"` + val + `"}]},"keep":"visible"}`
			out := anon(e, "application/json", in)
			if strings.Contains(out, val) {
				t.Errorf("%s: value survived: %s", c.key, out)
			}
			want := "[REDACTED]"
			if c.hash {
				want = e.k.hashToken(val)
			}
			if got := jsonAt(t, out, c.key); got != want {
				t.Errorf("%s = %v, want %q", c.key, got, want)
			}
			if got := jsonAt(t, out, "keep"); got != "visible" {
				t.Errorf("%s: unrelated member changed: %s", c.key, out)
			}
		}
	})
}

func TestBuiltinProfileFormFieldsIgnoreCaseSeparatorsAndBrackets(t *testing.T) {
	e := builtinEngine(t)
	sides(t, func(t *testing.T, anon func(*Engine, string, string) string) {
		in := "user[password]=hunter2&new_password=n3w&Access-Token=tok1&API_KEY=k1&" +
			"account[profile][client_secret]=cs1&Passphrase=pp1&user[tokens][]=keep&q=boots"
		out := anon(e, "application/x-www-form-urlencoded", in)
		got, err := url.ParseQuery(out)
		if err != nil {
			t.Fatalf("output %q: %v", out, err)
		}
		want := map[string]string{
			"user[password]":                  "[REDACTED]",
			"new_password":                    "[REDACTED]",
			"Access-Token":                    e.k.hashToken("tok1"),
			"API_KEY":                         e.k.hashToken("k1"),
			"account[profile][client_secret]": "[REDACTED]",
			"Passphrase":                      "[REDACTED]",
			"user[tokens][]":                  "keep",
			"q":                               "boots",
		}
		for k, v := range want {
			if got.Get(k) != v {
				t.Errorf("field %q = %q, want %q (output %s)", k, got.Get(k), v, out)
			}
		}
		for _, secret := range []string{"hunter2", "n3w", "tok1", "cs1", "pp1"} {
			if strings.Contains(out, secret) {
				t.Errorf("%q survived: %s", secret, out)
			}
		}
	})
}

func TestBuiltinProfileQueryParametersIgnoreCaseAndSeparators(t *testing.T) {
	e := builtinEngine(t)
	cases := map[string]string{ // raw query -> decoded parameter that must be hashed
		"?API_KEY=XYZ":            "API_KEY",
		"?Api-Key=XYZ&page=2":     "Api-Key",
		"?apiKey=XYZ":             "apiKey",
		"?ACCESS_TOKEN=XYZ":       "ACCESS_TOKEN",
		"?accessToken=XYZ":        "accessToken",
		"?user[password]=XYZ":     "user[password]",
		"?page=2&CSRF-Token=XYZ":  "CSRF-Token",
		"?x=1&Session_Id=XYZ&y=2": "Session_Id",
		// Revision 3 names, which JSON and forms also cover.
		"?user_password=XYZ":         "user_password",
		"?New-Password=XYZ":          "New-Password",
		"?passphrase=XYZ":            "passphrase",
		"?API_SECRET=XYZ":            "API_SECRET",
		"?secret_key=XYZ":            "secret_key",
		"?private_key=XYZ":           "private_key",
		"?aws_secret_access_key=XYZ": "aws_secret_access_key",
		"?Authorization=XYZ":         "Authorization",
		"?oauth_token=XYZ":           "oauth_token",
		"?session_token=XYZ":         "session_token",
		"?api-token=XYZ":             "api-token",
		"?x-api-key=XYZ":             "x-api-key",
	}
	for raw, name := range cases {
		req := &model.Request{Method: "GET", Path: "/api", RawQuery: strings.TrimPrefix(raw, "?"), Header: http.Header{}}
		if err := e.Request(req); err != nil {
			t.Fatal(err)
		}
		got, err := url.ParseQuery(req.RawQuery)
		if err != nil {
			t.Fatalf("%s: output query %q: %v", raw, req.RawQuery, err)
		}
		if strings.Contains(req.RawQuery, "XYZ") {
			t.Errorf("%s: value survived: %q", raw, req.RawQuery)
		}
		if got.Get(name) != e.k.hashToken("XYZ") {
			t.Errorf("%s: %q = %q, want the hash (query %q)", raw, name, got.Get(name), req.RawQuery)
		}
	}
}

// A token that appears in a JSON member, a query parameter and a header gets
// the same output everywhere, and that output is the hash of the token once:
// the profile lists both access_token and accessToken, which normalize alike,
// and applying both would hash twice.
func TestSameTokenSameOutputEverywhere(t *testing.T) {
	e := builtinEngine(t)
	const tok = "tok_live_abc"
	want := e.k.hashToken(tok)
	twice := e.k.hashToken(want)

	ex := newExchange()
	ex.Request.Method = "POST"
	ex.Request.Path = "/v1/things"
	ex.Request.RawQuery = "access_token=" + tok + "&ACCESS-TOKEN=" + tok + "&accessToken=" + tok
	ex.Request.Header.Set("Authorization", "Bearer "+tok)
	ex.Request.Header.Set("X-Access-Token", tok)
	ex.Request.Header.Set("Content-Type", "application/json")
	ex.Request.Body = []byte(`{"access_token":"` + tok + `","accessToken":"` + tok + `","AccessToken":"` + tok + `","nested":{"access-token":"` + tok + `"}}`)
	ex.Request.BodySize = int64(len(ex.Request.Body))
	changes, err := e.ExchangeWithReport(ex)
	if err != nil {
		t.Fatal(err)
	}

	q, err := url.ParseQuery(ex.Request.RawQuery)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"access_token", "ACCESS-TOKEN", "accessToken"} {
		if q.Get(k) != want {
			t.Errorf("query %s = %q, want %q", k, q.Get(k), want)
		}
	}
	if got := ex.Request.Header.Get("Authorization"); got != "Bearer "+want {
		t.Errorf("Authorization = %q", got)
	}
	if got := ex.Request.Header.Get("X-Access-Token"); got != want {
		t.Errorf("X-Access-Token = %q", got)
	}
	body := string(ex.Request.Body)
	for _, k := range []string{"access_token", "accessToken", "AccessToken"} {
		if got := jsonAt(t, body, k); got != want {
			t.Errorf("JSON %s = %v, want %q", k, got, want)
		}
	}
	if got := jsonAt(t, body, "nested", "access-token"); got != want {
		t.Errorf("JSON nested access-token = %v, want %q", got, want)
	}
	if strings.Contains(body+ex.Request.RawQuery, twice) {
		t.Errorf("a token was hashed twice: %s %s", body, ex.Request.RawQuery)
	}
	// One change per location: no value was transformed a second time.
	seen := map[string]int{}
	for _, c := range changes {
		seen[c.Location]++
		if seen[c.Location] > 1 {
			t.Errorf("%s was changed more than once: %+v", c.Location, changes)
		}
	}
}

func TestDefaultProfileRulesWithTheSameKeyAgree(t *testing.T) {
	// Rules whose keys normalize alike select the same values; the first one
	// wins, so the others must not do something different from it.
	type k struct {
		target model.RuleTarget
		name   string
	}
	first := map[k]model.AnonymizationRule{}
	for _, r := range model.DefaultAnonymizationProfile().Rules {
		var name string
		switch r.Target {
		case model.TargetJSON:
			jp, err := parseJSONPath(r.Selector)
			if err != nil || !jp.steps[0].descend || len(jp.steps) != 1 || jp.siblings != nil {
				continue
			}
			name = jp.steps[0].norm
		case model.TargetQuery, model.TargetForm, model.TargetCookie:
			name = normKey(r.Selector)
		default:
			continue
		}
		key := k{r.Target, name}
		if f, ok := first[key]; ok {
			if f.Strategy != r.Strategy || f.Fake != r.Fake {
				t.Errorf("%s %q (%s/%s) and %q (%s/%s) select the same key but differ", r.Target, r.Selector, r.Strategy, r.Fake, f.Selector, f.Strategy, f.Fake)
			}
			continue
		}
		first[key] = r
	}
}

func TestFirstRuleWinsInEveryTarget(t *testing.T) {
	hash := func(v string) string { return hashOf(v) }
	t.Run("query", func(t *testing.T) {
		e := mustEngine(t, bareProfile(
			rl(model.TargetQuery, "api_key", model.StrategyHash),
			rl(model.TargetQuery, "apiKey", model.StrategyRedact),
		))
		req := &model.Request{Method: "GET", Path: "/", RawQuery: "API-KEY=v", Header: http.Header{}}
		if err := e.Request(req); err != nil {
			t.Fatal(err)
		}
		if want := "API-KEY=" + hash("v"); req.RawQuery != want {
			t.Errorf("query = %q, want %q", req.RawQuery, want)
		}
	})
	t.Run("query remove after hash", func(t *testing.T) {
		e := mustEngine(t, bareProfile(
			rl(model.TargetQuery, "token", model.StrategyHash),
			rl(model.TargetQuery, "TOKEN", model.StrategyRemove),
		))
		req := &model.Request{Method: "GET", Path: "/", RawQuery: "a=1&Token=v", Header: http.Header{}}
		if err := e.Request(req); err != nil {
			t.Fatal(err)
		}
		if want := "a=1&Token=" + hash("v"); req.RawQuery != want {
			t.Errorf("query = %q, want %q", req.RawQuery, want)
		}
	})
	t.Run("form", func(t *testing.T) {
		e := mustEngine(t, bareProfile(
			rl(model.TargetForm, "access_token", model.StrategyHash),
			rl(model.TargetForm, "accessToken", model.StrategyHash),
		))
		req := anonReq(t, e, "application/x-www-form-urlencoded", "x=1&AccessToken=v")
		if want := "x=1&AccessToken=" + hash("v"); string(req.Body) != want {
			t.Errorf("form = %q, want %q", req.Body, want)
		}
	})
	t.Run("cookie", func(t *testing.T) {
		e := mustEngine(t, bareProfile(
			rl(model.TargetCookie, "session_id", model.StrategyHash),
			rl(model.TargetCookie, "sessionId", model.StrategyRedact),
		))
		ex := newExchange()
		ex.Request.Header.Set("Cookie", "theme=dark; Session-ID=v")
		if err := e.Exchange(ex); err != nil {
			t.Fatal(err)
		}
		if want := "theme=dark; Session-ID=" + hash("v"); ex.Request.Header.Get("Cookie") != want {
			t.Errorf("Cookie = %q, want %q", ex.Request.Header.Get("Cookie"), want)
		}
	})
	t.Run("path", func(t *testing.T) {
		e := mustEngine(t, bareProfile(
			rl(model.TargetPath, "/users/{id}", model.StrategyRedact),
			rl(model.TargetPath, "/users/{name}", model.StrategyHash),
		))
		ex := newExchange()
		ex.Request.Path = "/users/42"
		if err := e.Exchange(ex); err != nil {
			t.Fatal(err)
		}
		if want := "/users/%5BREDACTED%5D"; ex.Request.Path != want {
			t.Errorf("path = %q, want %q", ex.Request.Path, want)
		}
	})
	t.Run("header and regex", func(t *testing.T) {
		// The regex rule is listed first: it takes its stretch of the value
		// and the header rule after it leaves the value alone.
		e := mustEngine(t, bareProfile(
			rl(model.TargetRegex, `ACME-\d+`, model.StrategyRedact),
			rl(model.TargetHeader, "X-A", model.StrategyHash),
			rl(model.TargetHeader, "X-B", model.StrategyHash),
		))
		ex := newExchange()
		ex.Request.Header.Set("X-A", "ref ACME-5")
		ex.Request.Header.Set("X-B", "plain")
		if err := e.Exchange(ex); err != nil {
			t.Fatal(err)
		}
		if got := ex.Request.Header.Get("X-A"); got != "ref [REDACTED]" {
			t.Errorf("X-A = %q", got)
		}
		if got := ex.Request.Header.Get("X-B"); got != hash("plain") {
			t.Errorf("X-B = %q, want the hash (no regex match, so the header rule applies)", got)
		}
	})
	t.Run("multipart part name", func(t *testing.T) {
		e := mustEngine(t, bareProfile(
			rl(model.TargetForm, "user_password", model.StrategyRedact),
			rl(model.TargetForm, "userPassword", model.StrategyHash),
		))
		body := "--b\r\nContent-Disposition: form-data; name=\"User-Password\"\r\n\r\nhunter2\r\n--b--\r\n"
		req := anonReq(t, e, "multipart/form-data; boundary=b", body)
		if want := "--b\r\nContent-Disposition: form-data; name=\"User-Password\"\r\n\r\n[REDACTED]\r\n--b--\r\n"; string(req.Body) != want {
			t.Errorf("body = %q, want %q", req.Body, want)
		}
	})
}

// A regex rule takes only the stretches it matches; a later regex rule sees
// the original text, so it cannot rewrite what the first one wrote and leaves
// the stretches the first one took alone, while it still handles the rest.
func TestRegexRulesDoNotTransformEachOthersOutput(t *testing.T) {
	e := mustEngine(t, bareProfile(
		rl(model.TargetRegex, `tok_[a-z]+`, model.StrategyHash),
		rl(model.TargetRegex, `[a-z_]+`, model.StrategyRedact),
	))
	want := "[REDACTED] " + hashOf("tok_abc") + " [REDACTED]"
	req := anonReq(t, e, "text/plain", "x tok_abc y")
	if string(req.Body) != want {
		t.Errorf("text = %q, want %q", req.Body, want)
	}
	if got := anonJSON(t, e, `{"a":"x tok_abc y"}`); got != `{"a":"`+want+`"}` {
		t.Errorf("JSON leaf = %s", got)
	}
	ex := newExchange()
	ex.Request.Header.Set("X-Note", "x tok_abc y")
	if err := e.Exchange(ex); err != nil {
		t.Fatal(err)
	}
	if got := ex.Request.Header.Get("X-Note"); got != want {
		t.Errorf("header = %q, want %q", got, want)
	}

	// Two different elements of one document are two different values: both
	// rules get to work on a body that has a stretch for each.
	e = mustEngine(t, bareProfile(
		rl(model.TargetRegex, `<password>([^<]+)</`, model.StrategyRedact),
		rl(model.TargetRegex, `<token>([^<]+)</`, model.StrategyHash),
	))
	req = anonReq(t, e, "text/xml", "<a><password>p1</password><token>t1</token></a>")
	if want := "<a><password>[REDACTED]</password><token>" + hashOf("t1") + "</token></a>"; string(req.Body) != want {
		t.Errorf("xml = %q, want %q", req.Body, want)
	}
}

func TestJSONRecursiveDescentNamesIgnoreCaseAndSeparators(t *testing.T) {
	runJSONCases(t, []jsonCase{
		{"key-only rule", []model.AnonymizationRule{rl(model.TargetJSON, "$..api_key", model.StrategyRedact)},
			`{"apiKey":"1","API-KEY":"2","a":{"Api_Key":"3","apikey":["x"]},"api":"4","keys":"5"}`,
			`{"apiKey":"[REDACTED]","API-KEY":"[REDACTED]","a":{"Api_Key":"[REDACTED]","apikey":["[REDACTED]"]},"api":"4","keys":"5"}`},
		{"remove", []model.AnonymizationRule{rl(model.TargetJSON, "$..Secret_Key", model.StrategyRemove)},
			`{"secretKey":1,"b":{"SECRET-KEY":2,"c":3}}`, `{"b":{"c":3}}`},
		{"a name after .. in a longer path", []model.AnonymizationRule{rl(model.TargetJSON, "$..user_data.pin", model.StrategyRedact)},
			`{"x":{"UserData":{"pin":"1","PIN":"2"}},"userdata":{"pin":"3"}}`,
			`{"x":{"UserData":{"pin":"[REDACTED]","PIN":"2"}},"userdata":{"pin":"[REDACTED]"}}`},
		// Absolute paths keep their exact matching.
		{"absolute members are exact", []model.AnonymizationRule{rl(model.TargetJSON, "$.api_key", model.StrategyRedact)},
			`{"api_key":"1","apiKey":"2","API_KEY":"3"}`, `{"api_key":"[REDACTED]","apiKey":"2","API_KEY":"3"}`},
		{"a member after an absolute prefix is exact", []model.AnonymizationRule{rl(model.TargetJSON, "$.a..api_key", model.StrategyRedact)},
			`{"a":{"x":{"apiKey":"1"}},"b":{"apiKey":"2"}}`, `{"a":{"x":{"apiKey":"[REDACTED]"}},"b":{"apiKey":"2"}}`},
		{"sibling filter names are folded too",
			[]model.AnonymizationRule{withFake(rl(model.TargetJSON, "$..[?(@.email || @.phone_number)].name", model.StrategyFake), model.FakeName)},
			`{"Name":"Jane","E-Mail":"j"}`, `{"Name":"` + testKeyer(testSecret).fake(model.FakeName, "Jane") + `","E-Mail":"j"}`},
		{"sibling filter without a sibling",
			[]model.AnonymizationRule{withFake(rl(model.TargetJSON, "$..[?(@.email || @.phone_number)].name", model.StrategyFake), model.FakeName)},
			`{"Name":"Jane","Mail":"j"}`, `{"Name":"Jane","Mail":"j"}`},
	})
}

func TestCookieNamesIgnoreCaseAndSeparators(t *testing.T) {
	e := mustEngine(t, bareProfile(rl(model.TargetCookie, "session_id", model.StrategyHash)))
	ex := newExchange()
	ex.Request.Header.Set("Cookie", "SessionId=a; theme=dark; session-ID=b")
	ex.Response.Header.Set("Set-Cookie", "SESSION_ID=c; Path=/; HttpOnly")
	if err := e.Exchange(ex); err != nil {
		t.Fatal(err)
	}
	if want := "SessionId=" + hashOf("a") + "; theme=dark; session-ID=" + hashOf("b"); ex.Request.Header.Get("Cookie") != want {
		t.Errorf("Cookie = %q, want %q", ex.Request.Header.Get("Cookie"), want)
	}
	if want := "SESSION_ID=" + hashOf("c") + "; Path=/; HttpOnly"; ex.Response.Header.Get("Set-Cookie") != want {
		t.Errorf("Set-Cookie = %q, want %q", ex.Response.Header.Get("Set-Cookie"), want)
	}
}

func TestBuiltinProfileHashesWebhookSignatureHeaders(t *testing.T) {
	e := builtinEngine(t)
	names := []string{"Stripe-Signature", "X-Slack-Signature", "X-Gitlab-Token", "X-Twilio-Signature", "X-Hub-Signature-256", "X-Vault-Token"}
	req := &model.Request{Method: "POST", Path: "/hook", Header: http.Header{}}
	for _, n := range names {
		req.Header.Set(n, "t=1,v1=XYZsecretXYZ")
	}
	if err := e.Request(req); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if v := req.Header.Get(n); v == "" || strings.Contains(v, "XYZsecretXYZ") {
			t.Errorf("%s = %q, want a hash", n, v)
		}
	}
}
