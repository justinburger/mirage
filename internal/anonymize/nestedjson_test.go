package anonymize

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

// quoteJSON renders s as a JSON string literal.
func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// innerString decodes the string member path of a JSON document.
func innerString(t testing.TB, doc string, path ...string) string {
	t.Helper()
	s, ok := jsonAt(t, doc, path...).(string)
	if !ok {
		t.Fatalf("%v is not a string in %s", path, doc)
	}
	return s
}

// An envelope (a queue message, a webhook, a log event) carries a whole JSON
// document in a string: the credentials inside are found like in a body.
func TestNestedJSONEnvelopeIsRewritten(t *testing.T) {
	e := builtinEngine(t)
	const tok = "tok_live_abc"
	in := `{"id":7,"payload":"{\"user\":\"jane\",\"password\":\"hunter2\",\"access_token\":\"` + tok + `\"}"}`
	sides(t, func(t *testing.T, anon func(*Engine, string, string) string) {
		out := anon(e, "application/json", in)
		inner := innerString(t, out, "payload")
		want := `{"user":"jane","password":"[REDACTED]","access_token":"` + e.k.hashToken(tok) + `"}`
		if inner != want {
			t.Errorf("payload = %s\nwant      %s", inner, want)
		}
		if strings.Contains(out, "hunter2") || strings.Contains(out, tok) {
			t.Errorf("a secret survived: %s", out)
		}
		if got := jsonAt(t, out, "id"); got != float64(7) {
			t.Errorf("id = %v", got)
		}
		// The whole body is compact JSON with the inner document as one
		// string.
		if out != `{"id":7,"payload":`+quoteJSON(want)+`}` {
			t.Errorf("body = %s", out)
		}
	})
}

func TestNestedJSONShapes(t *testing.T) {
	e := builtinEngine(t)
	h := e.k.hashToken
	cases := []struct{ name, in, want string }{
		{"array document with spaces around it",
			`{"m":"  [ {\"password\" : \"x\"} , 1 ]\n"}`,
			`{"m":` + quoteJSON(`[{"password":"[REDACTED]"},1]`) + `}`},
		{"documents in an array of strings",
			`{"batch":["{\"token\":\"t1\"}","{\"token\":\"t2\"}","plain"]}`,
			`{"batch":[` + quoteJSON(`{"token":"`+h("t1")+`"}`) + `,` + quoteJSON(`{"token":"`+h("t2")+`"}`) + `,"plain"]}`},
		{"nested objects and arrays inside the document",
			`{"m":"{\"a\":{\"b\":[{\"apiKey\":\"k1\"},{\"x\":true,\"y\":null,\"z\":1.50}]}}"}`,
			`{"m":` + quoteJSON(`{"a":{"b":[{"apiKey":"`+h("k1")+`"},{"x":true,"y":null,"z":1.50}]}}`) + `}`},
		{"unicode and escapes survive",
			`{"m":"{\"name\":\"Ren` + u("00e9") + `e \\\"R\\\"\",\"password\":\"p\"}"}`,
			`{"m":` + quoteJSON(`{"name":"Renée \"R\"","password":"[REDACTED]"}`) + `}`},
		{"a name beside an e-mail is a person (sibling filter inside the document)",
			`{"m":"{\"name\":\"Jane Doe\",\"Email\":\"keep@example.org\"}"}`,
			""},
		{"unchanged documents are kept byte for byte",
			`{"m":"{ \"a\" : 1,\n \"b\" : [ 1, 2 ] }","n":"[1,2,3]","o":"{}","p":"[REDACTED]"}`,
			`{"m":"{ \"a\" : 1,\n \"b\" : [ 1, 2 ] }","n":"[1,2,3]","o":"{}","p":"[REDACTED]"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := anonJSON(t, e, c.in)
			if !json.Valid([]byte(got)) {
				t.Fatalf("invalid JSON: %s", got)
			}
			if c.want == "" {
				inner := innerString(t, got, "m")
				if strings.Contains(inner, "Jane Doe") || !strings.Contains(inner, `"name"`) {
					t.Errorf("name not faked inside the document: %s", inner)
				}
				return
			}
			if got != c.want {
				t.Errorf("\n in:   %s\n got:  %s\n want: %s", c.in, got, c.want)
			}
		})
	}
}

// Detectors apply inside the document, to its values and its keys.
func TestNestedJSONDetectorsApplyInside(t *testing.T) {
	e := builtinEngine(t)
	in := `{"event":"{\"contact\":\"jane.doe@acme.io\",\"by_email\":{\"bob@acme.io\":1},\"n\":[\"call 415-555-2671\"]}"}`
	out := anonJSON(t, e, in)
	inner := innerString(t, out, "event")
	for _, leaked := range []string{"jane.doe@acme.io", "bob@acme.io", "415-555-2671"} {
		if strings.Contains(inner, leaked) || strings.Contains(out, leaked) {
			t.Errorf("%q survived: %s", leaked, out)
		}
	}
	if got := innerString(t, inner, "contact"); got != e.k.fake(model.FakeEmail, "jane.doe@acme.io") {
		t.Errorf("contact = %q, want the consistent fake e-mail", got)
	}
}

// Absolute paths are relative to the outer document, recursive-descent rules
// reach into the embedded one.
func TestNestedJSONOnlyRecursiveRulesApplyInside(t *testing.T) {
	e := mustEngine(t, bareProfile(
		rl(model.TargetJSON, "$.password", model.StrategyRedact),
		rl(model.TargetJSON, "$.items[0].pin", model.StrategyRedact),
		rl(model.TargetJSON, "$..secret", model.StrategyRedact),
		rl(model.TargetJSON, "$..a.b", model.StrategyRedact),
	))
	in := `{"password":"p1","w":"{\"password\":\"p2\",\"items\":[{\"pin\":\"1234\",\"secret\":\"s1\"}],\"a\":{\"b\":\"ab\"}}"}`
	out := anonJSON(t, e, in)
	inner := innerString(t, out, "w")
	want := `{"password":"p2","items":[{"pin":"1234","secret":"[REDACTED]"}],"a":{"b":"[REDACTED]"}}`
	if inner != want {
		t.Errorf("inner = %s\n want   %s", inner, want)
	}
	if got := jsonAt(t, out, "password"); got != "[REDACTED]" {
		t.Errorf("outer password = %v", got)
	}
}

// A rule that selects the string itself takes it whole: the document inside
// is not opened.
func TestNestedJSONSelectedStringIsNotOpened(t *testing.T) {
	const doc = `{"password":"hunter2"}`
	e := mustEngine(t, bareProfile(rl(model.TargetJSON, "$..payload", model.StrategyHash)))
	out := anonJSON(t, e, `{"payload":`+quoteJSON(doc)+`}`)
	if got := innerString(t, out, "payload"); got != e.k.hashToken(doc) {
		t.Errorf("payload = %q, want the hash of the whole string", got)
	}
}

func TestNestedJSONRemoveAndRegexRulesApplyInside(t *testing.T) {
	e := mustEngine(t, bareProfile(
		rl(model.TargetJSON, "$..secret", model.StrategyRemove),
		rl(model.TargetRegex, `ACME-\d+`, model.StrategyRedact),
	))
	out := anonJSON(t, e, `{"m":"{\"secret\":\"s\",\"ref\":\"see ACME-12\",\"keep\":1}"}`)
	if want := `{"m":` + quoteJSON(`{"ref":"see [REDACTED]","keep":1}`) + `}`; out != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
}

// Content that opens like a document but is not one stays the plain string.
func TestNestedJSONMalformedInnerStaysAString(t *testing.T) {
	e := builtinEngine(t)
	for _, inner := range []string{
		`{"password":"hunter2"`, // unterminated
		`{"password":"hunter2",}`,
		`{"password":hunter2}`,
		`[1,2`,
		`{"a":1} {"password":"hunter2"}`, // two documents
		`{not json}`,
		`{"password":"hunter2"}}`,
	} {
		in := `{"payload":` + quoteJSON(inner) + `,"password":"top"}`
		out := anonJSON(t, e, in)
		if got := innerString(t, out, "payload"); got != inner {
			t.Errorf("malformed inner %q was changed to %q", inner, got)
		}
		if got := jsonAt(t, out, "password"); got != "[REDACTED]" {
			t.Errorf("outer password = %v", got)
		}
	}

	// It is a plain string, so the detectors still read it, and nothing the
	// failed attempt found is reported.
	ex := newExchange()
	ex.Response.Header.Set("Content-Type", "application/json")
	ex.Response.Body = []byte(`{"payload":"{\"email\":\"jane@acme.io\",\"password\":\"hunter2\", oops"}`)
	ex.Response.BodySize = int64(len(ex.Response.Body))
	changes, err := e.ExchangeWithReport(ex)
	if err != nil {
		t.Fatal(err)
	}
	if got := innerString(t, string(ex.Response.Body), "payload"); strings.Contains(got, "jane@acme.io") || !strings.Contains(got, "hunter2") || !strings.HasSuffix(got, ", oops") {
		t.Errorf("payload = %q", got)
	}
	if len(changes) != 1 || changes[0].Location != "response.body $.payload" || changes[0].Source != "detector:email" {
		t.Errorf("changes = %+v", changes)
	}
}

// nestedDoc wraps doc in n envelopes: n=1 is {"p":"<doc>"}.
func nestedDoc(doc string, n int) string {
	for i := 0; i < n; i++ {
		doc = `{"p":` + quoteJSON(doc) + `}`
	}
	return doc
}

// unwrap returns the document n envelopes down.
func unwrap(t testing.TB, doc string, n int) string {
	t.Helper()
	for i := 0; i < n; i++ {
		doc = innerString(t, doc, "p")
	}
	return doc
}

func TestNestedJSONDepthLimit(t *testing.T) {
	e := builtinEngine(t)
	const secret = `{"password":"hunter2"}`
	for depth := 1; depth <= maxNestedJSON+2; depth++ {
		out := anonJSON(t, e, nestedDoc(secret, depth))
		got := unwrap(t, out, depth)
		if depth <= maxNestedJSON {
			if want := `{"password":"[REDACTED]"}`; got != want {
				t.Errorf("depth %d: got %s, want %s", depth, got, want)
			}
			continue
		}
		// Deeper than the limit: the string is left as it is.
		if got != secret {
			t.Errorf("depth %d: got %s, want it untouched (past the limit)", depth, got)
		}
	}
	if maxNestedJSON != 3 {
		t.Errorf("maxNestedJSON = %d, want 3", maxNestedJSON)
	}
	// Past the limit the string is still a string for the detectors.
	out := anonJSON(t, e, nestedDoc(`{"e":"jane@acme.io"}`, maxNestedJSON+1))
	if strings.Contains(out, "jane@acme.io") {
		t.Errorf("e-mail survived past the depth limit: %s", out)
	}
}

func TestNestedJSONReportLocations(t *testing.T) {
	e := builtinEngine(t)
	ex := newExchange()
	ex.Request.Method = "POST"
	ex.Request.Header.Set("Content-Type", "application/json")
	ex.Request.Body = []byte(`{"payload":"{\"user\":\"jane\",\"password\":\"hunter2\"}","items":["x","{\"a\":[{\"access_token\":\"t\"}]}"],"w":{"q":"{\"m\":\"{\\\"password\\\":\\\"deep\\\"}\"}"}}`)
	ex.Request.BodySize = int64(len(ex.Request.Body))
	changes, err := e.ExchangeWithReport(ex)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []struct{ loc, source, before, after string }{
		{"request.body $.payload <json> $.password", "JSON password", "hunter2", "[REDACTED]"},
		{"request.body $.items[1] <json> $.a[0].access_token", "JSON access_token", "t", e.k.hashToken("t")},
		{"request.body $.w.q <json> $.m <json> $.password", "JSON password", "deep", "[REDACTED]"},
	} {
		ch, ok := changeAt(changes, w.loc)
		if !ok || ch.Source != w.source || ch.Before != w.before || ch.After != w.after {
			t.Errorf("%s: %+v (found=%v), want source %q %q -> %q", w.loc, ch, ok, w.source, w.before, w.after)
		}
	}
	if len(changes) != 3 {
		t.Errorf("changes = %+v", changes)
	}
}

// A capture cut short is processed as far as it goes; a string that was
// completed before the cut is opened, the one that was cut is not.
func TestNestedJSONInTruncatedBody(t *testing.T) {
	e := builtinEngine(t)
	body := `{"payload":"{\"password\":\"hunter2\"}","cut":"{\"password\":\"hunter3\"`
	resp := &model.Response{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body), BodySize: 9999, BodyTruncated: true}
	if err := e.Response(resp); err != nil {
		t.Fatal(err)
	}
	got := string(resp.Body)
	if strings.Contains(got, "hunter2") {
		t.Errorf("the completed string was not opened: %s", got)
	}
	if !strings.Contains(got, `[REDACTED]`) {
		t.Errorf("got %s", got)
	}
}

// NDJSON lines and arrays of envelopes use the same path.
func TestNestedJSONInNDJSONAndRequestBodies(t *testing.T) {
	e := builtinEngine(t)
	line := `{"Message":"{\"password\":\"hunter2\"}"}`
	req := anonReq(t, e, "application/x-ndjson", line+"\n"+line+"\n")
	if strings.Contains(string(req.Body), "hunter2") {
		t.Errorf("ndjson: %s", req.Body)
	}
	// JSON sent under a form content type is still found.
	req = anonReq(t, e, "application/x-www-form-urlencoded", `[`+line+`]`)
	if strings.Contains(string(req.Body), "hunter2") {
		t.Errorf("form-typed JSON: %s", req.Body)
	}
}
