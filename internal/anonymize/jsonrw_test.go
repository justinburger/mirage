package anonymize

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

func TestJSONPathParse(t *testing.T) {
	good := []struct {
		sel   string
		steps int
	}{
		{"$", 0},
		{"$.a", 1},
		{"$.a.b", 2},
		{"$.a[0]", 2},
		{"$.a[*].b", 3},
		{"$['a-b']", 1},
		{`$["a b"]`, 1},
		{"$['a']['b']", 2},
		{"$..key", 1},
		{"$..['key']", 1},
		{"$..*", 1},
		{"$..[0]", 1},
		{"$.*", 1},
		{"$[*]", 1},
		{"$.a..b", 2},
		{"$.items[10].id", 3},
		{"$.user-id", 1},
		{"$['it\\'s']", 1},
		{" $.a ", 1},
		{"$[ 0 ]", 1},
		{"$.日本語", 1},
	}
	for _, c := range good {
		jp, err := parseJSONPath(c.sel)
		if err != nil {
			t.Errorf("parse(%q): %v", c.sel, err)
			continue
		}
		if len(jp.steps) != c.steps {
			t.Errorf("parse(%q): %d steps, want %d", c.sel, len(jp.steps), c.steps)
		}
	}
	bad := []string{
		"", "a.b", "$.", "$..", "$.a.", "$.[0]", "$[", "$[]", "$[0", "$['a", "$['a'", "$[-1]",
		"$[0,1]", "$[1:2]", "$[?(@.a)]", "$.a b", "$.a(1)", "$[abc]", "$.a*", "$.'a'", "$ ", "$x",
	}
	for _, sel := range bad {
		if sel == "$ " { // trailing space is trimmed: that is valid
			continue
		}
		if _, err := parseJSONPath(sel); err == nil {
			t.Errorf("parse(%q): expected an error", sel)
		}
	}
	deep := "$" + strings.Repeat(".a", maxJSONPathSteps+1)
	if _, err := parseJSONPath(deep); err == nil {
		t.Error("expected an error for an over-long path")
	}
}

// jsonCase is a golden test: input, rules, expected output.
type jsonCase struct {
	name  string
	rules []model.AnonymizationRule
	in    string
	want  string
}

func hashOf(v string) string { return testKeyer(testSecret).hashToken(v) }

func runJSONCases(t *testing.T, cases []jsonCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := mustEngine(t, bareProfile(c.rules...))
			got := anonJSON(t, e, c.in)
			if got != c.want {
				t.Errorf("\n in:   %s\n got:  %s\n want: %s", c.in, got, c.want)
			}
			if !json.Valid([]byte(got)) && json.Valid([]byte(c.in)) {
				t.Errorf("valid JSON in, invalid JSON out: %s", got)
			}
		})
	}
}

func TestJSONSelectors(t *testing.T) {
	red := func(sel string) model.AnonymizationRule { return rl(model.TargetJSON, sel, model.StrategyRedact) }
	runJSONCases(t, []jsonCase{
		{"member", []model.AnonymizationRule{red("$.a")}, `{"a":"x","b":"y"}`, `{"a":"[REDACTED]","b":"y"}`},
		{"nested member", []model.AnonymizationRule{red("$.a.b")}, `{"a":{"b":"x","c":"y"},"b":"z"}`, `{"a":{"b":"[REDACTED]","c":"y"},"b":"z"}`},
		{"array index", []model.AnonymizationRule{red("$.a[1]")}, `{"a":["x","y","z"]}`, `{"a":["x","[REDACTED]","z"]}`},
		{"array wildcard", []model.AnonymizationRule{red("$.a[*].b")}, `{"a":[{"b":1,"c":2},{"b":3}]}`, `{"a":[{"b":"[REDACTED]","c":2},{"b":"[REDACTED]"}]}`},
		{"object wildcard", []model.AnonymizationRule{red("$.a.*")}, `{"a":{"x":"1","y":"2"},"b":"3"}`, `{"a":{"x":"[REDACTED]","y":"[REDACTED]"},"b":"3"}`},
		{"root wildcard", []model.AnonymizationRule{red("$.*")}, `{"a":"1","b":"2"}`, `{"a":"[REDACTED]","b":"[REDACTED]"}`},
		{"root array wildcard", []model.AnonymizationRule{red("$[*]")}, `["a","b"]`, `["[REDACTED]","[REDACTED]"]`},
		{"root array index", []model.AnonymizationRule{red("$[0]")}, `["a","b"]`, `["[REDACTED]","b"]`},
		{"quoted name", []model.AnonymizationRule{red("$['a-b']")}, `{"a-b":"x","ab":"y"}`, `{"a-b":"[REDACTED]","ab":"y"}`},
		{"quoted name with space", []model.AnonymizationRule{red(`$["a b"].c`)}, `{"a b":{"c":"x"}}`, `{"a b":{"c":"[REDACTED]"}}`},
		{"escaped key matches", []model.AnonymizationRule{red(`$['a"b']`)}, `{"a\"b":"x","ab":"y"}`, `{"a\"b":"[REDACTED]","ab":"y"}`},
		{"unicode escaped key matches", []model.AnonymizationRule{red("$.é")},
			`{"` + u("00e9") + `":"x"}`, `{"` + u("00e9") + `":"[REDACTED]"}`},
		{"recursive descent", []model.AnonymizationRule{red("$..email")},
			`{"email":"a","u":{"email":"b","l":[{"email":"c"},{"n":{"email":"d"}}]},"e":"keep"}`,
			`{"email":"[REDACTED]","u":{"email":"[REDACTED]","l":[{"email":"[REDACTED]"},{"n":{"email":"[REDACTED]"}}]},"e":"keep"}`},
		{"recursive descent after a prefix", []model.AnonymizationRule{red("$.a..id")},
			`{"id":"top","a":{"id":"1","b":{"id":"2"}}}`, `{"id":"top","a":{"id":"[REDACTED]","b":{"id":"[REDACTED]"}}}`},
		// Element 0 of "b" is itself an array, so everything beneath it is covered.
		{"recursive descent with index", []model.AnonymizationRule{red("$..[0]")},
			`{"a":["x","y"],"b":[["p","q"],"r"]}`, `{"a":["[REDACTED]","y"],"b":[["[REDACTED]","[REDACTED]"],"r"]}`},
		{"recursive wildcard", []model.AnonymizationRule{red("$..*")}, `{"a":{"b":"x"},"c":["y"]}`, `{"a":{"b":"[REDACTED]"},"c":["[REDACTED]"]}`},
		{"root selects everything", []model.AnonymizationRule{red("$")}, `{"a":"1","b":[2,true,null,"x"]}`, `{"a":"[REDACTED]","b":["[REDACTED]",true,null,"[REDACTED]"]}`},
		{"no match leaves the document alone", []model.AnonymizationRule{red("$.zzz")}, `{ "a" : "x" }`, `{ "a" : "x" }`},
		{"wrong type of container never matches", []model.AnonymizationRule{red("$.a.b")}, `{"a":["b"]}`, `{"a":["b"]}`},
		{"index on an object never matches", []model.AnonymizationRule{red("$.a[0]")}, `{"a":{"0":"x"}}`, `{"a":{"0":"x"}}`},
		{"two rules, one document", []model.AnonymizationRule{red("$.a"), rl(model.TargetJSON, "$.b", model.StrategyHash)},
			`{"a":"1","b":"2"}`, `{"a":"[REDACTED]","b":"` + hashOf("2") + `"}`},
	})
}

func TestJSONContainerMatchAppliesToLeaves(t *testing.T) {
	runJSONCases(t, []jsonCase{
		{"strings and numbers, not booleans or null",
			[]model.AnonymizationRule{rl(model.TargetJSON, "$.user", model.StrategyRedact)},
			`{"user":{"name":"jane","age":31,"admin":true,"manager":null,"tags":["a",2],"meta":{"x":"y"}},"keep":"k"}`,
			`{"user":{"name":"[REDACTED]","age":"[REDACTED]","admin":true,"manager":null,"tags":["[REDACTED]","[REDACTED]"],"meta":{"x":"[REDACTED]"}},"keep":"k"}`},
		{"hash on an array",
			[]model.AnonymizationRule{rl(model.TargetJSON, "$.ids", model.StrategyHash)},
			`{"ids":["a","b"],"z":1}`,
			`{"ids":["` + hashOf("a") + `","` + hashOf("b") + `"],"z":1}`},
		// The first rule in the profile wins; the other leaves the value alone.
		{"a container rule listed first wins over a deeper rule",
			[]model.AnonymizationRule{rl(model.TargetJSON, "$.u", model.StrategyHash), rl(model.TargetJSON, "$.u.n", model.StrategyRedact)},
			`{"u":{"n":"jane","m":"x"}}`,
			`{"u":{"n":"` + hashOf("jane") + `","m":"` + hashOf("x") + `"}}`},
		{"a deeper rule listed first wins for its leaf",
			[]model.AnonymizationRule{rl(model.TargetJSON, "$.u.n", model.StrategyRedact), rl(model.TargetJSON, "$.u", model.StrategyHash)},
			`{"u":{"n":"jane","m":"x"}}`,
			`{"u":{"n":"[REDACTED]","m":"` + hashOf("x") + `"}}`},
		{"a remove listed after a rule that matches the same value does not apply",
			[]model.AnonymizationRule{rl(model.TargetJSON, "$.u", model.StrategyHash), rl(model.TargetJSON, "$.u.n", model.StrategyRemove)},
			`{"u":{"n":"jane","m":"x"}}`,
			`{"u":{"n":"` + hashOf("jane") + `","m":"` + hashOf("x") + `"}}`},
		{"a remove listed first deletes the member",
			[]model.AnonymizationRule{rl(model.TargetJSON, "$.u.n", model.StrategyRemove), rl(model.TargetJSON, "$.u", model.StrategyHash)},
			`{"u":{"n":"jane","m":"x"}}`,
			`{"u":{"m":"` + hashOf("x") + `"}}`},
		{"empty containers survive",
			[]model.AnonymizationRule{rl(model.TargetJSON, "$.a", model.StrategyRedact)},
			`{"a":{},"b":[],"c":{"d":[]}}`, `{"a":{},"b":[],"c":{"d":[]}}`},
	})
}

func TestJSONRemove(t *testing.T) {
	rm := func(sel string) model.AnonymizationRule { return rl(model.TargetJSON, sel, model.StrategyRemove) }
	runJSONCases(t, []jsonCase{
		{"first member", []model.AnonymizationRule{rm("$.a")}, `{"a":1,"b":2,"c":3}`, `{"b":2,"c":3}`},
		{"middle member", []model.AnonymizationRule{rm("$.b")}, `{"a":1,"b":2,"c":3}`, `{"a":1,"c":3}`},
		{"last member", []model.AnonymizationRule{rm("$.c")}, `{"a":1,"b":2,"c":3}`, `{"a":1,"b":2}`},
		{"only member", []model.AnonymizationRule{rm("$.a")}, `{"a":1}`, `{}`},
		{"whole subtree", []model.AnonymizationRule{rm("$.u")}, `{"x":0,"u":{"a":[1,{"b":"c"}],"d":null},"y":1}`, `{"x":0,"y":1}`},
		{"every match", []model.AnonymizationRule{rm("$..secret")}, `{"secret":1,"a":{"secret":2,"b":3},"l":[{"secret":4,"c":5}]}`, `{"a":{"b":3},"l":[{"c":5}]}`},
		{"array element", []model.AnonymizationRule{rm("$.a[1]")}, `{"a":["x","y","z"]}`, `{"a":["x","z"]}`},
		{"all array elements", []model.AnonymizationRule{rm("$.a[*]")}, `{"a":[1,2,3],"b":1}`, `{"a":[],"b":1}`},
		{"member in every array object", []model.AnonymizationRule{rm("$.items[*].card")}, `{"items":[{"id":1,"card":"x"},{"card":"y","id":2}]}`, `{"items":[{"id":1},{"id":2}]}`},
		{"null and boolean members", []model.AnonymizationRule{rm("$.a"), rm("$.b")}, `{"a":null,"b":false,"c":true}`, `{"c":true}`},
		{"removed value is validated", []model.AnonymizationRule{rm("$.a")}, `{"a":{"x":[1,2,{"y":"z"}]},"b":1}`, `{"b":1}`},
		{"root remove falls back to redacting leaves", []model.AnonymizationRule{rm("$")}, `{"a":"x","b":[1]}`, `{"a":"[REDACTED]","b":["[REDACTED]"]}`},
	})

	// A malformed value inside a removed member still makes the whole body
	// fall back to text processing instead of producing broken JSON.
	e := mustEngine(t, bareProfile(rm("$.a")))
	if got := anonJSON(t, e, `{"a":{"x":[1,2,},"b":1}`); got != `{"a":{"x":[1,2,},"b":1}` {
		t.Errorf("malformed body changed: %s", got)
	}
}

func TestJSONRemoveValidatesSkippedValue(t *testing.T) {
	e := mustEngine(t, bareProfile(rl(model.TargetJSON, "$.a", model.StrategyRemove)))
	deep := mustEngine(t, bareProfile(rl(model.TargetJSON, "$..a", model.StrategyRemove)))
	valid := []string{
		`1`, `-0.5e+10`, `"s"`, `"es\"c\\ ` + u("00e9") + `"`, `true`, `false`, `null`, `[]`, `{}`, `[1,[2,[3,[]]]]`,
		`{"x":{"y":[{"z":null},"w",1.5]}}`, ` [ 1 , { "k" : "v" } ] `, `{"":""}`,
	}
	for _, v := range valid {
		in := `{"a":` + v + `,"b":1}`
		if got := anonJSON(t, e, in); got != `{"b":1}` {
			t.Errorf("removing %s: got %s", v, got)
		}
		in = `[{"a":` + v + `},{"a":` + v + `,"b":2}]`
		if got := anonJSON(t, deep, in); got != `[{},{"b":2}]` {
			t.Errorf("removing %s from objects in an array: got %s", v, got)
		}
	}
	invalid := []string{
		`[1,`, `[1 2]`, `{"x":}`, `{"x" 1}`, `{x:1}`, `"unterminated`, `"bad \q escape"`, `01`, `1.`, `-`, `tru`, `nul`,
		`[}`, `{]`, `[1,]`, `{"x":1,}`, `{"x":1 "y":2}`, `[[[[`, `}`, `]`, `,`, `:`, `"\u12"`, `1e`, `+1`,
	}
	for _, v := range invalid {
		in := `{"a":` + v + `,"b":1}`
		got := anonJSON(t, e, in)
		if got != in { // malformed: text fallback, which has nothing to change here
			t.Errorf("malformed removed value %s: body changed to %s", v, got)
		}
	}
	// A truncated document that ends inside the skipped value, at every cut.
	doc := `{"a":{"x":[1,"s\n",{"y":null}],"z":true},"b":1}`
	for i := 0; i < len(doc); i++ {
		resp := &model.Response{Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(doc[:i]), BodyTruncated: true, BodySize: int64(len(doc))}
		if err := e.Response(resp); err != nil {
			t.Fatalf("cut %d: %v", i, err)
		}
		if strings.Contains(string(resp.Body), `"x"`) || strings.Contains(string(resp.Body), `"z"`) {
			t.Fatalf("cut %d leaked part of the removed member: %s", i, resp.Body)
		}
	}
}

func TestJSONNumbers(t *testing.T) {
	runJSONCases(t, []jsonCase{
		{"literals survive untouched when something else changes",
			[]model.AnonymizationRule{rl(model.TargetJSON, "$.s", model.StrategyRedact)},
			`{"a":1.0,"b":1E+2,"c":-0,"d":12345678901234567890123,"e":0.1e-5,"f":1.50,"s":"x"}`,
			`{"a":1.0,"b":1E+2,"c":-0,"d":12345678901234567890123,"e":0.1e-5,"f":1.50,"s":"[REDACTED]"}`},
		{"hash of a number is a string token",
			[]model.AnonymizationRule{rl(model.TargetJSON, "$.id", model.StrategyHash)},
			`{"id":12345}`, `{"id":"` + hashOf("12345") + `"}`},
		{"redact of a number is a string",
			[]model.AnonymizationRule{rl(model.TargetJSON, "$.id", model.StrategyRedact)},
			`{"id":12345}`, `{"id":"[REDACTED]"}`},
		{"a replacement that is a number stays a number",
			[]model.AnonymizationRule{func() model.AnonymizationRule {
				r := rl(model.TargetJSON, "$.id", model.StrategyRedact)
				r.Replacement = "0"
				return r
			}()},
			`{"id":12345}`, `{"id":0}`},
		{"mask of a number is a string",
			[]model.AnonymizationRule{func() model.AnonymizationRule {
				r := rl(model.TargetJSON, "$.id", model.StrategyMask)
				r.KeepSuffix = 2
				return r
			}()},
			`{"id":12345}`, `{"id":"***45"}`},
	})
	// fake digits keeps a number a number, with the same digit count
	e := mustEngine(t, bareProfile(withFake(rl(model.TargetJSON, "$..id", model.StrategyFake), model.FakeDigits)))
	got := anonJSON(t, e, `{"id":1234567,"x":{"id":-42.50},"y":[{"id":9}]}`)
	var m struct {
		ID json.Number `json:"id"`
		X  struct {
			ID json.Number `json:"id"`
		} `json:"x"`
		Y []struct {
			ID json.Number `json:"id"`
		} `json:"y"`
	}
	dec := json.NewDecoder(strings.NewReader(got))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("output is not valid JSON with numeric ids: %s (%v)", got, err)
	}
	if len(m.ID.String()) != 7 || len(m.X.ID.String()) != 6 || !strings.HasPrefix(m.X.ID.String(), "-") || !strings.Contains(m.X.ID.String(), ".") {
		t.Errorf("fake digits did not keep the numeric shape: %s", got)
	}
	if m.ID.String() == "1234567" || len(m.Y) != 1 || m.Y[0].ID.String() == "9" {
		t.Errorf("ids were not replaced: %s", got)
	}
	// A card-shaped number is a number the detector replaces by a card number.
	e = defaultEngine(t)
	got = anonJSON(t, e, `{"pan":4111111111111111,"ts":1698765432123,"n":1234567890}`)
	if !strings.Contains(got, `"ts":1698765432123`) || !strings.Contains(got, `"n":1234567890`) {
		t.Errorf("ordinary integers were changed: %s", got)
	}
	var out struct {
		Pan json.Number `json:"pan"`
	}
	dec = json.NewDecoder(strings.NewReader(got))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil || out.Pan.String() == "4111111111111111" || !luhnValid(out.Pan.String()) {
		t.Errorf("card number not faked as a number: %s (%v)", got, err)
	}
}

func TestJSONStructurePreserved(t *testing.T) {
	e := defaultEngine(t)
	// Member order, duplicate keys, nesting, escapes and unicode survive; only
	// the e-mail changes, and whitespace is compacted.
	in := `{
	  "z": 1,
	  "a": "jane@acme.io",
	  "m": [3, 2, 1, {"k": null}],
	  "dup": 1, "dup": 2,
	  "esc": "line\nbreak \"quoted\" \\ back \/ slash ` + u("00e9") + ` ` + u("d83d") + u("de00") + `",
	  "uni": "café 日本 😀",
	  "empty": "", "t": true, "f": false
	}`
	got := anonJSON(t, e, in)
	fake := e.k.fake(model.FakeEmail, "jane@acme.io")
	want := `{"z":1,"a":"` + fake + `","m":[3,2,1,{"k":null}],"dup":1,"dup":2,` +
		`"esc":"line\nbreak \"quoted\" \\ back \/ slash ` + u("00e9") + ` ` + u("d83d") + u("de00") + `","uni":"café 日本 😀","empty":"","t":true,"f":false}`
	if got != want {
		t.Errorf("\n got:  %s\n want: %s", got, want)
	}
}

func TestJSONUnchangedBodyIsByteIdentical(t *testing.T) {
	e := defaultEngine(t)
	in := "{ \"a\" : [ 1 , 2 ] ,\n \"b\" : \"caf\\u00e9\" }\n"
	resp := &model.Response{Header: http.Header{"Content-Type": {"application/json"}, "Content-Length": {"41"}}, Body: []byte(in), BodySize: int64(len(in))}
	if err := e.Response(resp); err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != in || resp.BodySize != int64(len(in)) || resp.Header.Get("Content-Length") != "41" {
		t.Errorf("an unchanged body was modified: %q size=%d cl=%s", resp.Body, resp.BodySize, resp.Header.Get("Content-Length"))
	}
}

func TestJSONEscapedPIIIsDetected(t *testing.T) {
	e := defaultEngine(t)
	got := anonJSON(t, e, `{"e":"jane`+u("0040")+`acme.io","n":"caf`+u("00e9")+`"}`)
	if strings.Contains(got, "acme") || strings.Contains(got, u("0040")) {
		t.Errorf("escaped e-mail leaked: %s", got)
	}
	if !strings.Contains(got, `"n":"caf`+u("00e9")+`"`) {
		t.Errorf("untouched escape was rewritten: %s", got)
	}
	// A percent-encoded address inside a string is found too.
	got = anonJSON(t, e, `{"url":"https://x.test/cb?email=jane%40acme.io&n=1"}`)
	if strings.Contains(got, "acme") || !strings.Contains(got, "%40example.") || !strings.Contains(got, "&n=1") {
		t.Errorf("encoded e-mail: %s", got)
	}
}

func TestJSONKeysAreScanned(t *testing.T) {
	e := defaultEngine(t)
	got := anonJSON(t, e, `{"users":{"jane@acme.io":{"n":1},"bob@acme.io":{"n":2}},"plain":1}`)
	if strings.Contains(got, "acme.io") {
		t.Errorf("e-mail keys leaked: %s", got)
	}
	var m struct {
		Users map[string]map[string]int `json:"users"`
		Plain int                       `json:"plain"`
	}
	if err := json.Unmarshal([]byte(got), &m); err != nil || len(m.Users) != 2 || m.Plain != 1 {
		t.Errorf("keys lost or output invalid: %s (%v)", got, err)
	}
	// A rule still matches the original key name.
	e = mustEngine(t, testProfile(rl(model.TargetJSON, "$.users['jane@acme.io'].n", model.StrategyRedact)))
	got = anonJSON(t, e, `{"users":{"jane@acme.io":{"n":1}}}`)
	if !strings.Contains(got, `"n":"[REDACTED]"`) {
		t.Errorf("rule on an e-mail key did not match: %s", got)
	}
}

func TestJSONScalarRoots(t *testing.T) {
	e := defaultEngine(t)
	cases := map[string]string{
		`"jane@acme.io"`: "",
		`42`:             `42`,
		`null`:           `null`,
		`true`:           `true`,
		`"plain"`:        `"plain"`,
		`[]`:             `[]`,
		`{}`:             `{}`,
		"  \n 7 \n":      "  \n 7 \n",
	}
	for in, want := range cases {
		got := anonJSON(t, e, in)
		if want == "" {
			if strings.Contains(got, "acme") || !json.Valid([]byte(got)) {
				t.Errorf("%s -> %s", in, got)
			}
			continue
		}
		if got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}

func TestJSONDeepNesting(t *testing.T) {
	e := mustEngine(t, testProfile(rl(model.TargetJSON, "$..deep", model.StrategyRedact)))
	for _, depth := range []int{100, 10_000, 40_000} {
		in := strings.Repeat(`{"a":[`, depth) + `{"deep":"x","e":"jane@acme.io"}` + strings.Repeat(`]}`, depth)
		got := anonJSON(t, e, in)
		if strings.Contains(got, "acme.io") || !strings.Contains(got, `"deep":"[REDACTED]"`) {
			t.Fatalf("depth %d: not anonymized", depth)
		}
		// encoding/json refuses documents nested deeper than 10000 levels, so
		// it can only vouch for the shallow ones.
		if json.Valid([]byte(in)) && !json.Valid([]byte(got)) {
			t.Fatalf("depth %d: invalid JSON out", depth)
		}
	}
	// Beyond the nesting limit the body is processed as text: no crash, and
	// PII is still replaced.
	n := maxJSONDepth + 10
	in := strings.Repeat("[", n) + `"jane@acme.io"` + strings.Repeat("]", n)
	if got := anonJSON(t, e, in); strings.Contains(got, "acme.io") {
		t.Error("over-deep document leaked the e-mail address")
	}
	// Plain arrays of arrays, 200k deep, as a hostile body would send.
	hostile := strings.Repeat("[", 200_000)
	_ = anonJSON(t, e, hostile)
}

func TestJSONLargeDocumentIsLinear(t *testing.T) {
	e := mustEngine(t, testProfile(
		rl(model.TargetJSON, "$..id", model.StrategyHash),
		rl(model.TargetJSON, "$.items[*].card", model.StrategyMask),
		rl(model.TargetJSON, "$..secret", model.StrategyRemove),
	))
	var sb strings.Builder
	sb.WriteString(`{"items":[`)
	for i := 0; i < 20_000; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"id":%d,"card":"4111111111111111","email":"user%d@acme.io","secret":"s","note":"hello world"}`, i, i)
	}
	sb.WriteString(`]}`)
	in := sb.String()
	start := time.Now()
	got := anonJSON(t, e, in)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("%d bytes took %v", len(in), elapsed)
	}
	if strings.Contains(got, "acme.io") || strings.Contains(got, "secret") || !json.Valid([]byte(got)) {
		t.Errorf("large document not anonymized correctly")
	}
}

func TestJSONMalformedFallsBackToText(t *testing.T) {
	cases := []struct{ name, in string }{
		{"trailing comma", `{"email": "jane@acme.io",}`},
		{"unquoted key", `{email: "jane@acme.io"}`},
		{"single quotes", `{'email': 'jane@acme.io'}`},
		{"truncated without flag", `{"email": "jane@acme.io", "x": [1, 2`},
		{"garbage prefix", `>>> {"email": "jane@acme.io"}`},
		{"bad escape", `{"email": "jane@acme.io", "x": "\q"}`},
		{"raw control character", "{\"email\": \"jane@acme.io\", \"x\": \"a\tb\"}"},
		{"bad number", `{"email": "jane@acme.io", "n": 01}`},
		{"bad literal", `{"email": "jane@acme.io", "n": tru}`},
		{"missing colon", `{"email" "jane@acme.io"}`},
		{"missing comma", `{"a": 1 "email": "jane@acme.io"}`},
		{"extra closer", `{"email": "jane@acme.io"}}`},
		{"NaN", `{"email": "jane@acme.io", "n": NaN}`},
	}
	e := defaultEngine(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := anonJSON(t, e, c.in)
			if strings.Contains(got, "jane@acme.io") {
				t.Errorf("malformed JSON leaked the e-mail address: %s", got)
			}
			if !strings.Contains(got, "@example.") {
				t.Errorf("expected a faked address: %s", got)
			}
		})
	}
	// JSON rules cannot apply to a malformed body; regex rules and detectors do.
	e = mustEngine(t, testProfile(rl(model.TargetRegex, `"password":\s*"([^"]*)"`, model.StrategyRedact)))
	got := anonJSON(t, e, `{"password": "hunter2",}`)
	if got != `{"password": "[REDACTED]",}` {
		t.Errorf("regex on malformed JSON: %s", got)
	}
}

func TestJSONTruncatedBodies(t *testing.T) {
	e := mustEngine(t, testProfile(rl(model.TargetJSON, "$..password", model.StrategyRedact)))
	cases := []struct{ in, want string }{
		{`{"password":"hunter2","email":"jane@acme.io","x":[1,2`, ``},
		{`{"a":{"password":"hunter2"},"b":"jane@acme.io","c":"par`, ``},
		{`{"password":"hunt`, `{"password":"[REDACTED]`},
		{`{"password":"hunter2","k`, `{"password":"[REDACTED]","k`},
		{`{"password":"hunter2","k"`, `{"password":"[REDACTED]","k"`},
		{`{"password":"hunter2","k":`, `{"password":"[REDACTED]","k":`},
		{`{"password":"hunter2",`, `{"password":"[REDACTED]"`},
		{`{"a":-`, `{"a":-`},
		{`{"a":1.`, `{"a":1.`},
		{`{"a":tr`, `{"a":tr`},
		{`{"a":"x\`, `{"a":"x\`},
		{`{"a":"x\u00`, `{"a":"x\u00`},
		{`[1,2,{"password":"x"`, `[1,2,{"password":"[REDACTED]"`},
		{``, ``},
		{`{`, `{`},
		{`   `, `   `},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			resp := &model.Response{
				Header: http.Header{"Content-Type": {"application/json"}},
				Body:   []byte(c.in), BodySize: int64(len(c.in)) + 1000, BodyTruncated: true,
			}
			if err := e.Response(resp); err != nil {
				t.Fatal(err)
			}
			got := string(resp.Body)
			if strings.Contains(got, "hunter2") || strings.Contains(got, "jane@acme.io") {
				t.Fatalf("leaked: %s", got)
			}
			if c.want != "" && got != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
			if resp.BodySize < int64(len(got)) {
				t.Errorf("BodySize %d smaller than the body %d", resp.BodySize, len(got))
			}
		})
	}
}

func TestJSONTruncatedInsideRemovedValue(t *testing.T) {
	// The capture ends inside a value a remove rule deletes: the partial value
	// must not survive in the stored body.
	e := mustEngine(t, bareProfile(rl(model.TargetJSON, "$..secret", model.StrategyRemove)))
	cases := []struct{ in, want string }{
		{`{"a":1,"secret":"hunt`, `{"a":1`},
		{`{"a":1,"secret":{"k":[1,2`, `{"a":1`},
		{`{"secret":"hunt`, `{`},
		{`[{"a":1},{"secret":tr`, `[{"a":1},{`},
		{`{"a":1,"secret":12`, `{"a":1`},
		{`{"a":1,"secret":"x","b":2,"c":"par`, `{"a":1,"b":2,"c":"par`},
	}
	for _, c := range cases {
		resp := &model.Response{Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(c.in), BodyTruncated: true, BodySize: 999}
		if err := e.Response(resp); err != nil {
			t.Fatal(err)
		}
		if got := string(resp.Body); got != c.want {
			t.Errorf("%s\n got  %s\n want %s", c.in, got, c.want)
		}
	}
}

func TestJSONRegexRulesApplyToLeaves(t *testing.T) {
	e := mustEngine(t, bareProfile(
		rl(model.TargetRegex, `ACME-\d+`, model.StrategyRedact),
		rl(model.TargetJSON, "$.k", model.StrategyMask),
	))
	got := anonJSON(t, e, `{"a":"ref ACME-1 and ACME-22","n":12,"k":"ACME-7","ACME-9":1}`)
	// The regex rule is listed first, so it takes the value and the mask rule
	// listed after it leaves the marker alone.
	want := `{"a":"ref [REDACTED] and [REDACTED]","n":12,"k":"[REDACTED]","ACME-9":1}`
	if got != want {
		t.Errorf("\n got:  %s\n want: %s", got, want)
	}

	// Listed the other way round the mask rule takes the whole value and the
	// regex rule never sees it.
	e = mustEngine(t, bareProfile(
		rl(model.TargetJSON, "$.k", model.StrategyMask),
		rl(model.TargetRegex, `ACME-\d+`, model.StrategyRedact),
	))
	got = anonJSON(t, e, `{"a":"ref ACME-1 and ACME-22","n":12,"k":"ACME-7","ACME-9":1}`)
	want = `{"a":"ref [REDACTED] and [REDACTED]","n":12,"k":"******","ACME-9":1}`
	if got != want {
		t.Errorf("\n got:  %s\n want: %s", got, want)
	}
}

func TestJSONManyRulesStayCorrect(t *testing.T) {
	var rules []model.AnonymizationRule
	for i := 0; i < 200; i++ {
		rules = append(rules, rl(model.TargetJSON, fmt.Sprintf("$.f%d", i), model.StrategyRedact))
	}
	rules = append(rules, rl(model.TargetJSON, "$..any", model.StrategyRemove))
	e := mustEngine(t, bareProfile(rules...))
	got := anonJSON(t, e, `{"f0":"a","f199":"b","f200":"c","n":{"any":1,"x":2}}`)
	want := `{"f0":"[REDACTED]","f199":"[REDACTED]","f200":"c","n":{"x":2}}`
	if got != want {
		t.Errorf("\n got:  %s\n want: %s", got, want)
	}
}

func TestUnescapeAndAppendJSONString(t *testing.T) {
	cases := []struct{ raw, want string }{
		{`plain`, "plain"},
		{`a\nb\tc\r\b\f`, "a\nb\tc\r\b\f"},
		{`\"q\" \\ \/`, `"q" \ /`},
		{u("00e9"), "é"},
		{u("0041") + u("0042"), "AB"},
		{u("d83d") + u("de00"), "😀"},
		{u("d83d"), "�"},              // lone high surrogate
		{u("de00"), "�"},              // lone low surrogate
		{u("d83d") + "x", "�x"},       // high surrogate then text
		{u("d83d") + u("0041"), "�A"}, // high surrogate then a non-surrogate escape
		{u("d83d") + u("d83d"), "��"}, // two high surrogates
		{`trailing\`, `trailing\`},    // truncated escapes are copied through
		{`bad` + u("12"), `bad` + u("12")},
		{`\q`, `\q`},
	}
	for _, c := range cases {
		if got := string(unescapeJSON(nil, []byte(c.raw))); got != c.want {
			t.Errorf("unescape(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
	enc := []struct{ in, want string }{
		{"plain", `"plain"`},
		{"a\"b\\c", `"a\"b\\c"`},
		{"\n\r\t", `"\n\r\t"`},
		{"\x01\x1f", `"\u0001\u001f"`},
		{"é😀", `"é😀"`},
		{"", `""`},
	}
	for _, c := range enc {
		got := string(appendJSONString(nil, c.in))
		if got != c.want {
			t.Errorf("appendJSONString(%q) = %s, want %s", c.in, got, c.want)
		}
		var back string
		if err := json.Unmarshal([]byte(got), &back); err != nil || back != c.in {
			t.Errorf("round trip of %q failed: %q (%v)", c.in, back, err)
		}
	}
}

func TestValidJSONNumber(t *testing.T) {
	good := []string{"0", "-0", "1", "-1", "10", "1.5", "-1.5", "1e5", "1E+5", "1e-5", "0.1", "12345678901234567890", "1.5e300"}
	bad := []string{"", "-", "01", "1.", ".5", "+1", "1e", "1e+", "0x10", "1 ", " 1", "NaN", "Infinity", "1.5.5", "[REDACTED]"}
	for _, s := range good {
		if !validJSONNumber(s) {
			t.Errorf("validJSONNumber(%q) = false", s)
		}
	}
	for _, s := range bad {
		if validJSONNumber(s) {
			t.Errorf("validJSONNumber(%q) = true", s)
		}
	}
}

// Keep the lenient and strict walkers consistent: for every prefix of a valid
// document, lenient processing must not panic and must not change a prefix
// that contains nothing sensitive.
func TestJSONEveryPrefix(t *testing.T) {
	e := mustEngine(t, bareProfile(rl(model.TargetJSON, "$..x", model.StrategyRedact)))
	doc := `{"a":[1,2.5e3,"s` + u("00e9") + `\n",{"b":null,"c":true}],"d":{"e":"f"},"g":[]}`
	for i := 0; i <= len(doc); i++ {
		resp := &model.Response{Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(doc[:i]), BodySize: int64(len(doc)), BodyTruncated: true}
		if err := e.Response(resp); err != nil {
			t.Fatalf("prefix %d: %v", i, err)
		}
		if got := string(resp.Body); got != doc[:i] {
			// compact input in, compact input out, nothing sensitive: identical.
			t.Fatalf("prefix %d changed:\n got  %q\n want %q", i, got, doc[:i])
		}
	}
}

// slowJSONMatcher returns a copy of e that matches every json rule through
// the automaton, without the member-name index for $..name rules.
func slowJSONMatcher(e *Engine) *Engine {
	c := *e.c
	c.req.keyDescent, c.resp.keyDescent = nil, nil
	s := *e
	s.c = &c
	return &s
}

func TestKeyDescentRulesCompileToTheIndex(t *testing.T) {
	e := mustEngine(t, bareProfile(
		rl(model.TargetJSON, "$..email", model.StrategyHash),
		rl(model.TargetJSON, "$..['odd key']", model.StrategyHash),
		rl(model.TargetJSON, "$.user.email", model.StrategyHash),
		rl(model.TargetJSON, "$..user.email", model.StrategyHash),
		rl(model.TargetJSON, "$..*", model.StrategyHash),
		rl(model.TargetJSON, "$..[0]", model.StrategyHash),
		rl(model.TargetJSON, "$.email", model.StrategyHash),
		withScope(rl(model.TargetJSON, "$..resp", model.StrategyHash), model.ScopeResponse),
	))
	wantReq := map[string][]int32{"email": {0}, "odd key": {1}}
	if !reflect.DeepEqual(e.c.req.keyDescent, wantReq) {
		t.Errorf("request index = %v, want %v", e.c.req.keyDescent, wantReq)
	}
	if got := e.c.resp.keyDescent["resp"]; !reflect.DeepEqual(got, []int32{int32(len(e.c.resp.json) - 1)}) {
		t.Errorf("response index for resp = %v (json rules %d)", got, len(e.c.resp.json))
	}
	var keyOnly []string
	for _, r := range e.c.req.json {
		if r.keyOnly {
			keyOnly = append(keyOnly, r.jp.src)
		}
	}
	if !reflect.DeepEqual(keyOnly, []string{"$..email", "$..['odd key']"}) {
		t.Errorf("keyOnly rules = %v", keyOnly)
	}
	if mustEngine(t, bareProfile(rl(model.TargetJSON, "$.a.b", model.StrategyHash))).c.req.keyDescent != nil {
		t.Error("an engine without $..name rules should have no index")
	}
}

// TestKeyDescentMatchesTheAutomaton compares the member-name index with the
// plain automaton on random documents and rule sets.
func TestKeyDescentMatchesTheAutomaton(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	keys := []string{"a", "b", "c", "email", "odd key", "x"}
	strategies := []model.Strategy{model.StrategyHash, model.StrategyRedact, model.StrategyRemove, model.StrategyMask}
	selectors := []string{"$..a", "$..b", "$..email", "$..['odd key']", "$.a.b", "$..a.b", "$.x[*]", "$..c[0]", "$.*", "$..x", "$.a..b", "$..*"}

	var genVal func(depth int) string
	genVal = func(depth int) string {
		switch n := rnd.Intn(7); {
		case n == 0 && depth < 5, n == 1 && depth < 5:
			var sb strings.Builder
			sb.WriteByte('{')
			for i, m := 0, rnd.Intn(4); i < m; i++ {
				if i > 0 {
					sb.WriteByte(',')
				}
				fmt.Fprintf(&sb, "%q:%s", keys[rnd.Intn(len(keys))], genVal(depth+1))
			}
			sb.WriteByte('}')
			return sb.String()
		case n == 2 && depth < 5:
			var sb strings.Builder
			sb.WriteByte('[')
			for i, m := 0, rnd.Intn(4); i < m; i++ {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString(genVal(depth + 1))
			}
			sb.WriteByte(']')
			return sb.String()
		case n == 3:
			return strconv.Itoa(rnd.Intn(1000))
		case n == 4:
			return "null"
		}
		return fmt.Sprintf("%q", fmt.Sprintf("v%d jane@acme.io", rnd.Intn(50)))
	}

	changedDocs, docs := 0, 0
	defer func() {
		if changedDocs < docs/4 {
			t.Errorf("only %d of %d random documents were changed: the comparison is too weak", changedDocs, docs)
		}
	}()
	for iter := 0; iter < 400; iter++ {
		var rules []model.AnonymizationRule
		for i, n := 0, 1+rnd.Intn(6); i < n; i++ {
			sel := selectors[rnd.Intn(len(selectors))]
			rules = append(rules, rl(model.TargetJSON, sel, strategies[rnd.Intn(len(strategies))]))
		}
		if rnd.Intn(3) == 0 {
			rules = append(rules, rl(model.TargetRegex, `v1\d`, model.StrategyHash))
		}
		fast := mustEngine(t, testProfile(rules...))
		slow := slowJSONMatcher(fast)
		for d := 0; d < 6; d++ {
			doc := genVal(0)
			if doc[0] != '{' && doc[0] != '[' {
				doc = "{" + `"a":` + doc + "}"
			}
			run := func(e *Engine) (string, []model.AnonChange) {
				ex := &model.Exchange{
					Request: model.Request{Header: http.Header{}},
					Response: model.Response{
						Header: http.Header{"Content-Type": {"application/json"}},
						Body:   []byte(doc), BodySize: int64(len(doc)),
					},
				}
				changes, err := e.ExchangeWithReport(ex)
				if err != nil {
					t.Fatal(err)
				}
				return string(ex.Response.Body), changes
			}
			gotOut, gotCh := run(fast)
			wantOut, wantCh := run(slow)
			docs++
			if gotOut != doc {
				changedDocs++
			}
			if gotOut != wantOut {
				t.Fatalf("rules %v\n doc: %s\nfast: %s\nslow: %s", selectorsOf(rules), doc, gotOut, wantOut)
			}
			if !reflect.DeepEqual(gotCh, wantCh) {
				t.Fatalf("rules %v\n doc: %s\nfast report: %v\nslow report: %v", selectorsOf(rules), doc, gotCh, wantCh)
			}
		}
	}
}

func selectorsOf(rules []model.AnonymizationRule) []string {
	var out []string
	for _, r := range rules {
		out = append(out, r.Selector+"/"+string(r.Strategy))
	}
	return out
}
