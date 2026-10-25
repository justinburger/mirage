package anonymize

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/justinburger/mirage/internal/model"
)

func changeAt(changes []model.AnonChange, loc string) (model.AnonChange, bool) {
	for _, c := range changes {
		if c.Location == loc {
			return c, true
		}
	}
	return model.AnonChange{}, false
}

func TestReportLocationsAndSources(t *testing.T) {
	e := mustEngine(t, testProfile(
		rl(model.TargetQuery, "token", model.StrategyRedact),
		rl(model.TargetJSON, "$.user.ssn_note", model.StrategyHash),
		rl(model.TargetForm, "card", model.StrategyMask),
	))
	ex := newExchange()
	ex.Request.Method = "POST"
	ex.Request.Path = "/users/jane%40acme.io"
	ex.Request.RawQuery = "email=jane%40acme.io&token=abc&keep=1"
	ex.Request.Header.Set("Authorization", "Bearer xyz")
	ex.Request.Header.Set("Cookie", "sid=abc; theme=dark")
	ex.Request.Header.Set("X-Note", "call 415-555-2671")
	ex.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ex.Request.Body = []byte("card=4111111111111111&name=bob")
	ex.Request.BodySize = int64(len(ex.Request.Body))
	ex.Response.Header.Set("Content-Type", "application/json")
	ex.Response.Header.Set("Set-Cookie", "sid=zzz; Path=/")
	ex.Response.Body = []byte(`{"user":{"email":"jane@acme.io","ssn_note":"n1","items":[{"ip":"1.2.3.4"}]},"x-y":{"ip":"5.6.7.8"}}`)
	ex.Response.BodySize = int64(len(ex.Response.Body))
	ex.Error = "dial tcp 203.0.113.9:443: refused"

	changes, err := e.ExchangeWithReport(ex)
	if err != nil {
		t.Fatal(err)
	}
	type want struct{ loc, source string }
	for _, w := range []want{
		{"request.header.Authorization", "redact_headers"},
		{"request.cookie.sid", "redact_headers"},
		{"request.header.X-Note", "detector:phone"},
		{"request.path", "detector:email"},
		{"request.query.email", "detector:email"},
		{"request.query.token", "rule " + "token"},
		{"request.body form.card", "rule card"},
		{"response.cookie.sid", "redact_headers"},
		{"response.body $.user.email", "detector:email"},
		{"response.body $.user.ssn_note", "rule $.user.ssn_note"},
		{"response.body $.user.items[0].ip", "detector:ipv4"},
		{"response.body $['x-y'].ip", "detector:ipv4"},
		{"error", "detector:ipv4"},
	} {
		c, ok := changeAt(changes, w.loc)
		if !ok {
			t.Errorf("no change recorded at %q; got %v", w.loc, locations(changes))
			continue
		}
		if c.Source != w.source {
			t.Errorf("%s: source = %q, want %q", w.loc, c.Source, w.source)
		}
		if c.Before == c.After {
			t.Errorf("%s: before == after (%q)", w.loc, c.Before)
		}
	}
	if _, ok := changeAt(changes, "request.query.keep"); ok {
		t.Error("an unchanged parameter was reported")
	}
	if c, _ := changeAt(changes, "request.header.Authorization"); c.Before != "Bearer xyz" || !strings.HasPrefix(c.After, "Bearer h_") {
		t.Errorf("Authorization change = %+v", c)
	}
	if c, _ := changeAt(changes, "request.query.email"); c.Before != "jane@acme.io" {
		t.Errorf("the report shows the decoded value: %+v", c)
	}
	// Order: request first, then response, then the error.
	var idx [3]int
	for i, c := range changes {
		switch {
		case strings.HasPrefix(c.Location, "request."):
			idx[0] = i
		case strings.HasPrefix(c.Location, "response."):
			if idx[1] == 0 {
				idx[1] = i
			}
		case c.Location == "error":
			idx[2] = i
		}
	}
	if !(idx[0] < idx[1] && idx[1] < idx[2]) {
		t.Errorf("changes are not ordered request, response, error: %v", locations(changes))
	}
}

func locations(cs []model.AnonChange) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Location)
	}
	return out
}

func TestReportDoesNotChangeTheResult(t *testing.T) {
	e := mustEngine(t, testProfile(rl(model.TargetJSON, "$..customer_id", model.StrategyHash), rl(model.TargetJSON, "$..notes", model.StrategyRemove)))
	a, b := sampleExchange(), sampleExchange()
	if err := e.Exchange(a); err != nil {
		t.Fatal(err)
	}
	changes, err := e.ExchangeWithReport(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Error("reporting changed the anonymized exchange")
	}
	if len(changes) == 0 {
		t.Error("no changes reported for the sample exchange")
	}
	// No changes: an empty, non-nil list (it serializes as [], not null).
	clean := newExchange()
	changes, err = e.ExchangeWithReport(clean)
	if err != nil || changes == nil || len(changes) != 0 {
		t.Errorf("changes = %#v, err = %v", changes, err)
	}
}

func TestReportCapAndClipping(t *testing.T) {
	e := defaultEngine(t)
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < 700; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `"user%d@acme.io"`, i)
	}
	sb.WriteString("]")
	ex := newExchange()
	ex.Response.Header.Set("Content-Type", "application/json")
	ex.Response.Body = []byte(sb.String())
	changes, err := e.ExchangeWithReport(ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 500 {
		t.Errorf("report has %d entries, want the cap of 500", len(changes))
	}
	// ...but the exchange is still fully anonymized beyond the cap.
	if strings.Contains(string(ex.Response.Body), "acme.io") {
		t.Error("anonymization stopped at the report cap")
	}

	// Values are clipped to 200 characters.
	long := strings.Repeat("x", 300) + "@acme.io"
	ex = newExchange()
	ex.Request.Header.Set("X-Long", "mail "+long)
	ex.Request.Header.Set("X-Wide", strings.Repeat("é", 250)+" jane@acme.io")
	changes, err = e.ExchangeWithReport(ex)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		for _, v := range []string{c.Before, c.After} {
			if n := utf8.RuneCountInString(v); n > 200 {
				t.Errorf("%s: %d characters in %q", c.Location, n, v[:20])
			}
			if !utf8.ValidString(v) {
				t.Errorf("%s: clipped in the middle of a character", c.Location)
			}
		}
	}
	if c, ok := changeAt(changes, "request.header.X-Long"); !ok || !strings.HasSuffix(c.Before, "…") || utf8.RuneCountInString(c.Before) != 200 {
		t.Errorf("long value: %+v", c)
	}
	if got := clipValue("short"); got != "short" {
		t.Errorf("clipValue(short) = %q", got)
	}
	if got := clipValue(strings.Repeat("a", 200)); len(got) != 200 {
		t.Errorf("a value of exactly 200 characters must not be clipped: %d", len(got))
	}
}

func TestReportLocatorStrings(t *testing.T) {
	cases := []struct {
		l    locator
		want string
	}{
		{locator{side: "request", where: "path"}, "request.path"},
		{locator{side: "response", where: "header", name: "Set-Cookie"}, "response.header.Set-Cookie"},
		{locator{side: "request", where: "query", name: "a"}, "request.query.a"},
		{locator{side: "request", where: "cookie", name: "sid"}, "request.cookie.sid"},
		{locator{side: "request", where: "body"}, "request.body"},
		{locator{side: "request", where: "body", form: true, name: "card"}, "request.body form.card"},
		{locator{side: "response", where: "body", json: []byte("$.a[0]")}, "response.body $.a[0]"},
		{locator{side: "response", where: "body", line: 3, json: []byte("$.a")}, "response.body line 3 $.a"},
		{locator{side: "response", where: "body", line: 12}, "response.body line 12"},
		{locator{where: "error"}, "error"},
	}
	for _, c := range cases {
		if got := c.l.String(); got != c.want {
			t.Errorf("%+v = %q, want %q", c.l, got, c.want)
		}
	}
	for _, n := range []int{0, 1, 9, 10, 123456} {
		if itoa(n) != fmt.Sprint(n) {
			t.Errorf("itoa(%d) = %s", n, itoa(n))
		}
	}
}

func TestReportJSONPaths(t *testing.T) {
	var _ = appendJSONPathIndex
	cases := []struct {
		key  string
		want string
	}{
		{"name", ".name"},
		{"user_id", ".user_id"},
		{"$ref", ".$ref"},
		{"a-b", "['a-b']"},
		{"a b", "['a b']"},
		{"it's", `['it\'s']`},
		{`back\slash`, `['back\\slash']`},
		{"", "['']"},
		{"日本", ".日本"},
	}
	for _, c := range cases {
		if got := string(appendJSONPathKey([]byte("$"), []byte(c.key))); got != "$"+c.want {
			t.Errorf("key %q -> %s, want $%s", c.key, got, c.want)
		}
	}
}

func TestReportDiscardsFailedJSONPass(t *testing.T) {
	// The JSON pass records the first address before it hits the syntax error;
	// the text fallback then handles the body. Each address is reported once.
	e := defaultEngine(t)
	ex := newExchange()
	ex.Response.Header.Set("Content-Type", "application/json")
	ex.Response.Body = []byte(`{"a":"jane@acme.io","b":"bob@acme.io",,}`)
	changes, err := e.ExchangeWithReport(ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("got %d changes: %v", len(changes), changes)
	}
	for _, c := range changes {
		if c.Location != "response.body" {
			t.Errorf("fallback location = %q", c.Location)
		}
	}

	// NDJSON: the bad line is reported as text, good lines by JSON path.
	ex = newExchange()
	ex.Response.Header.Set("Content-Type", "application/x-ndjson")
	ex.Response.Body = []byte(`{"a":"jane@acme.io"}` + "\n" + `broken jane@acme.io {` + "\n")
	changes, err = e.ExchangeWithReport(ex)
	if err != nil {
		t.Fatal(err)
	}
	got := locations(changes)
	want := []string{"response.body line 1 $.a", "response.body line 2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("locations = %v, want %v", got, want)
	}
}

func TestReportRemovedMemberShowsBeforeAndEmptyAfter(t *testing.T) {
	e := mustEngine(t, bareProfile(rl(model.TargetJSON, "$.password", model.StrategyRemove)))
	ex := newExchange()
	ex.Request.Header.Set("Content-Type", "application/json")
	ex.Request.Body = []byte(`{"user":"bob","password":"hunter2"}`)
	changes, err := e.ExchangeWithReport(ex)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := changeAt(changes, "request.body $.password")
	if !ok || c.After != "" || c.Before != `"hunter2"` {
		t.Errorf("removed member: %+v (%v)", c, ok)
	}
	// A removed header and query parameter show up as well.
	e = mustEngine(t, bareProfile(rl(model.TargetHeader, "X-Secret", model.StrategyRemove), rl(model.TargetQuery, "t", model.StrategyRemove)))
	ex = newExchange()
	ex.Request.Header = http.Header{"X-Secret": {"s"}}
	ex.Request.RawQuery = "t=1&k=2"
	changes, err = e.ExchangeWithReport(ex)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := changeAt(changes, "request.header.X-Secret"); !ok {
		t.Errorf("removed header not reported: %v", locations(changes))
	}
	if c, ok := changeAt(changes, "request.query.t"); !ok || c.Before != "t=1" || c.After != "" {
		t.Errorf("removed parameter: %+v (%v)", c, ok)
	}
}
