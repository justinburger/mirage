package anonymize

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

const nameIfEmail = `$..[?(@.email || @.phone)].name`

func TestParseSiblingFilter(t *testing.T) {
	ok := []struct {
		sel  string
		name string
		sibs []string
	}{
		{`$..[?(@.email)].name`, "name", []string{"email"}},
		{`$..[?@.email].name`, "name", []string{"email"}},
		{`$..[?(@.email || @.phone)].name`, "name", []string{"email", "phone"}},
		{`$..[?( @.email||@['phone'] || @["zip"] )].name`, "name", []string{"email", "phone", "zip"}},
		{`$..[?(@.email)]['name']`, "name", []string{"email"}},
	}
	for _, c := range ok {
		jp, err := parseJSONPath(c.sel)
		if err != nil {
			t.Errorf("%s: %v", c.sel, err)
			continue
		}
		if len(jp.steps) != 1 || !jp.steps[0].descend || jp.steps[0].name != c.name || strings.Join(jp.siblings, ",") != strings.Join(c.sibs, ",") {
			t.Errorf("%s: steps %+v siblings %v", c.sel, jp.steps, jp.siblings)
		}
	}
	bad := []string{
		`$..[?(@.email)]`,              // nothing after the filter
		`$..[?(@.email)].a.b`,          // more than one member
		`$..[?(@.email)].*`,            // wildcard
		`$..[?(@.email && @.phone)].n`, // and
		`$..[?(@.age > 3)].name`,       // comparison
		`$..[?(@.email)`,               // unterminated
		`$..[?((@.email)].name`,        // unbalanced
		`$..[?().name`,                 // empty
		`$..[?(email)].name`,           // not @.member
		`$..[?(@.a.b)].name`,           // nested
		`$.a[?(@.email)].name`,         // only the descendant form
		`$.a[?(@.email)]`,              // other filters stay unsupported
		`$..[?(@.email)][0]`,           // index after the filter
		`$..[?(@.email)].name.extra`,   // dotted tail
		`$..[?(@.e*)].name`,            // wildcard in the member name
		`$..[?(` + strings.Repeat("@.a || ", 70) + `@.b)].name`, // too many members
	}
	for _, sel := range bad {
		if _, err := parseJSONPath(sel); err == nil {
			t.Errorf("%s was accepted", sel)
		}
	}
	// The model accepts the selector and the engine compiles it.
	if _, err := New(bareProfile(withFake(rl(model.TargetJSON, nameIfEmail, model.StrategyFake), model.FakeName))); err != nil {
		t.Errorf("New: %v", err)
	}
	if _, err := New(bareProfile(rl(model.TargetJSON, `$..[?(@.x > 1)].name`, model.StrategyHash))); err == nil {
		t.Error("an unsupported filter must be a validation error")
	}
}

func siblingEngine(t *testing.T, strategy model.Strategy) *Engine {
	t.Helper()
	r := rl(model.TargetJSON, nameIfEmail, strategy)
	if strategy == model.StrategyFake {
		r.Fake = model.FakeName
	}
	return mustEngine(t, bareProfile(r))
}

func TestSiblingFilterAppliesPerObject(t *testing.T) {
	e := siblingEngine(t, model.StrategyRedact)
	cases := []struct {
		name, in, want string
	}{
		{"sibling after", `{"name":"Ava","email":"a@b.c"}`, `{"name":"[REDACTED]","email":"a@b.c"}`},
		{"sibling before", `{"email":"a@b.c","name":"Ava"}`, `{"email":"a@b.c","name":"[REDACTED]"}`},
		{"sibling in the middle", `{"id":1,"name":"Ava","x":[1,{"deep":true}],"phone":"1","tail":2}`, `{"id":1,"name":"[REDACTED]","x":[1,{"deep":true}],"phone":"1","tail":2}`},
		{"product", `{"name":"Widget","sku":"w-1"}`, `{"name":"Widget","sku":"w-1"}`},
		{"empty object", `{"name":"Widget"}`, `{"name":"Widget"}`},
		{"sibling only in a child is not a sibling", `{"name":"Widget","owner":{"email":"a@b.c"}}`, `{"name":"Widget","owner":{"email":"a@b.c"}}`},
		{"sibling only in the parent is not a sibling", `{"email":"a@b.c","item":{"name":"Widget"}}`, `{"email":"a@b.c","item":{"name":"Widget"}}`},
		{"array of mixed objects",
			`[{"name":"Ava","email":"a@b.c"},{"name":"Widget","sku":"1"},{"phone":"5","name":"Bo"}]`,
			`[{"name":"[REDACTED]","email":"a@b.c"},{"name":"Widget","sku":"1"},{"phone":"5","name":"[REDACTED]"}]`},
		{"nested", `{"order":{"id":1,"items":[{"name":"Widget"}],"customer":{"name":"Ava","email":"a@b.c"}}}`,
			`{"order":{"id":1,"items":[{"name":"Widget"}],"customer":{"name":"[REDACTED]","email":"a@b.c"}}}`},
		{"name is an object", `{"name":{"first":"Ava","last":"L"},"email":"a@b.c"}`, `{"name":{"first":"[REDACTED]","last":"[REDACTED]"},"email":"a@b.c"}`},
		{"escaped sibling key", `{"name":"Ava","em` + u("0061") + `il":"a@b.c"}`, `{"name":"[REDACTED]","email":"a@b.c"}`},
		{"escaped key on a different member", `{"na` + u("006d") + `e":"Ava","phone":"1"}`, `{"name":"[REDACTED]","phone":"1"}`},
		{"string values never count", `{"name":"Widget","note":"email"}`, `{"name":"Widget","note":"email"}`},
		{"empty-key and odd members around", `{"":1,"name":"Ava","x\"y":"z","email":null}`, `{"":1,"name":"[REDACTED]","x\"y":"z","email":null}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := anonJSON(t, e, c.in)
			if !json.Valid([]byte(got)) {
				t.Fatalf("output is not JSON: %s", got)
			}
			if got != c.want && got != c.in && !(c.in == c.want) {
				// The rewriter writes compact JSON; compare decoded when the input was not compact.
			}
			if !sameJSON(got, c.want) {
				t.Errorf("\n in:  %s\n got: %s\nwant: %s", c.in, got, c.want)
			}
		})
	}
}

func sameJSON(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}

func TestSiblingFilterRemoveAndReport(t *testing.T) {
	e := siblingEngine(t, model.StrategyRemove)
	got := anonJSON(t, e, `{"id":1,"name":"Ava","email":"a@b.c"}`)
	if !sameJSON(got, `{"id":1,"email":"a@b.c"}`) {
		t.Errorf("remove: %s", got)
	}
	if got := anonJSON(t, e, `{"id":1,"name":"Widget"}`); got != `{"id":1,"name":"Widget"}` {
		t.Errorf("remove touched a product: %s", got)
	}
	// The preview reports the change at the member's path.
	e = siblingEngine(t, model.StrategyFake)
	ex := newExchange()
	ex.Response.Header.Set("Content-Type", "application/json")
	ex.Response.Body = []byte(`{"user":{"name":"Ava Johnson","email":"ava@example.org"},"product":{"name":"Widget"}}`)
	ex.Response.BodySize = int64(len(ex.Response.Body))
	changes, err := e.ExchangeWithReport(ex)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, c := range changes {
		paths = append(paths, c.Location)
	}
	if len(paths) != 1 || paths[0] != "response.body $.user.name" {
		t.Errorf("changes at %v, want only $.user.name", paths)
	}
}

// A truncated capture may stop before the sibling: not knowing, anonymize.
func TestSiblingFilterTruncatedFailsClosed(t *testing.T) {
	e := siblingEngine(t, model.StrategyRedact)
	resp := newExchange().Response
	resp.Header.Set("Content-Type", "application/json")
	resp.Body = []byte(`{"name":"Ava","note":"a long note that is cut o`)
	resp.BodySize = 5000
	resp.BodyTruncated = true
	if err := e.Response(&resp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp.Body), `"name":"[REDACTED]"`) {
		t.Errorf("truncated body: %s", resp.Body)
	}
	// A complete object without a sibling is left alone even when the capture
	// is flagged truncated later in the document.
	resp = newExchange().Response
	resp.Header.Set("Content-Type", "application/json")
	resp.Body = []byte(`[{"name":"Widget","sku":"1"},{"name":"Gadget","sk`)
	resp.BodySize = 5000
	resp.BodyTruncated = true
	if err := e.Response(&resp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(resp.Body), `[{"name":"Widget","sku":"1"},`) {
		t.Errorf("complete product object changed: %s", resp.Body)
	}
}

// Reading each object ahead must not make a deeply nested document quadratic.
func TestSiblingFilterLookaheadIsBounded(t *testing.T) {
	e := siblingEngine(t, model.StrategyRedact)
	const depth = 20000
	var sb strings.Builder
	for i := 0; i < depth; i++ {
		sb.WriteString(`{"name":"n","k":`)
	}
	sb.WriteString(`1`)
	for i := 0; i < depth; i++ {
		sb.WriteString(`}`)
	}
	in := sb.String() // ~340 KB: unbounded lookahead would read ~3.4 GB
	got := anonJSON(t, e, in)
	// Past the budget the answer is yes, so names deep inside are redacted;
	// the point of the test is that it finishes and stays valid JSON.
	// (encoding/json refuses this depth, so check the shape by hand.)
	if !strings.HasPrefix(got, `{"name":`) || strings.Count(got, "{") != depth || !strings.HasSuffix(got, strings.Repeat("}", depth)) {
		t.Fatalf("output has the wrong shape (%d bytes)", len(got))
	}
	if !strings.Contains(got, `"name":"[REDACTED]"`) {
		t.Error("expected the over-budget names to be anonymized")
	}
}
