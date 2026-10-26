package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

func TestYAMLMergeKeys(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string // JSON of the "services" section's first entry, or an error part
		err  string
	}{
		{
			name: "explicit key wins over a merge, wherever it is written",
			src: `services:
  - &a {name: from-a, upstream: "http://a:1"}
  - <<: *a
    name: explicit
`,
		},
		{
			name: "merge of a list: earlier mapping wins",
			src: `services:
  - &a {name: a, upstream: "http://a:1", listen_addr: ":1"}
  - &b {name: b, upstream: "http://b:1", listen_addr: ":2", mode: virtualize}
  - <<: [*a, *b]
    recording: r
`,
		},
		{
			name: "merge needs a mapping",
			src:  "services:\n  - <<: 5\n    name: x\n",
			err:  "needs a mapping",
		},
		{
			name: "merge list needs mappings",
			src:  "services:\n  - <<: [1, 2]\n    name: x\n",
			err:  "needs a mapping",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, err := parseYAML([]byte(tc.src))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			svcs, _ := root.get("services")
			last := svcs.items[len(svcs.items)-1]
			get := func(k string) string {
				v, ok := last.get(k)
				if !ok {
					return ""
				}
				return v.text
			}
			switch tc.name {
			case "explicit key wins over a merge, wherever it is written":
				if get("name") != "explicit" || get("upstream") != "http://a:1" {
					t.Errorf("merged = %s", last.appendJSON(nil))
				}
			default:
				if get("upstream") != "http://a:1" || get("mode") != "virtualize" || get("listen_addr") != ":1" || get("recording") != "r" || get("name") != "a" {
					t.Errorf("merged = %s", last.appendJSON(nil))
				}
			}
		})
	}
}

func TestYAMLScalarForms(t *testing.T) {
	src := `
server:
  admin: ":8484"
  data: 123
  log_level: true
`
	// Numbers and booleans fill string fields as written...
	c := mustParse(t, "c.yaml", strings.Replace(src, "log_level: true", "log_format: json", 1))
	if c.Server.Data != "123" || c.Server.Admin != ":8484" {
		t.Errorf("server = %+v", c.Server)
	}
	// ...and then fail validation by value, not by type.
	_, err := Parse("c.yaml", []byte(src))
	if err == nil || !strings.Contains(err.Error(), `log_level "true"`) {
		t.Errorf("err = %v", err)
	}

	// Octal, hex, exponent and underscore forms are numbers.
	c = mustParse(t, "c.yaml", "services:\n  - {name: a, listen_addr: ':1', upstream: 'http://x', max_body_bytes: 0x10, capture_sample: 5e-1, max_capture_bytes: 1_000}\n")
	if s := c.Services[0]; s.MaxBodyBytes != 16 || s.CaptureSample != 0.5 || s.MaxCaptureBytes != 1000 {
		t.Errorf("service = %+v", s)
	}
}

func TestYAMLTimestampField(t *testing.T) {
	c := mustParse(t, "c.yaml", "profiles:\n  - name: p\n    created_at: 2026-10-25T12:00:00Z\n")
	if !c.Profiles[0].CreatedAt.Equal(time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("created_at = %v", c.Profiles[0].CreatedAt)
	}
}

func TestJSONTreeErrors(t *testing.T) {
	for _, src := range []string{
		`{"a": [1, 2`,
		`{"a": {"b": 1`,
		`{"a" 1}`,
		`{1: 2}`,
		`[1, 2,]`,
		`{"a": tru}`,
		`{"a": 1}}`,
	} {
		if _, err := parseJSON([]byte(src)); err == nil {
			t.Errorf("parseJSON(%q) should fail", src)
		} else if pe, ok := err.(*parseError); !ok || pe.line < 1 {
			t.Errorf("parseJSON(%q): %v (%T) lacks a line", src, err, err)
		}
	}
	n, err := parseJSON([]byte("{\n\"a\": [1,\n2],\n\"b\": null}"))
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := n.get("a"); a.line != 2 || a.items[1].line != 3 {
		t.Errorf("lines: a=%d item1=%d", a.line, a.items[1].line)
	}
	if got := string(n.appendJSON(nil)); got != `{"a":[1,2],"b":null}` {
		t.Errorf("re-encoded = %s", got)
	}
}

func TestTreeStringsAndNames(t *testing.T) {
	for k, want := range map[nodeKind]string{kindNull: "null", kindBool: "bool", kindNumber: "number", kindString: "string", kindArray: "list", kindObject: "object"} {
		if k.String() != want {
			t.Errorf("%d = %q, want %q", k, k.String(), want)
		}
	}
	if got := (&parseError{4, "oops"}).Error(); got != "line 4: oops" {
		t.Errorf("parseError = %q", got)
	}
	for typ, want := range map[reflect.Type]string{
		reflect.TypeFor[bool]():     "bool",
		reflect.TypeFor[int64]():    "an integer",
		reflect.TypeFor[float64]():  "a number",
		reflect.TypeFor[string]():   "a string",
		reflect.TypeFor[[]string](): "a list",
		reflect.TypeFor[Server]():   "an object",
	} {
		if got := typeName(typ); got != want {
			t.Errorf("typeName(%v) = %q, want %q", typ, got, want)
		}
	}
	if got := truncate(strings.Repeat("x", 100), 10); got != "xxxxxxxxxx..." {
		t.Errorf("truncate = %q", got)
	}
}

func TestSuggest(t *testing.T) {
	names := []string{"name", "listen_addr", "upstream", "probability", "path_pattern"}
	for key, want := range map[string]string{
		"nmae": "name", "listen_adr": "listen_addr", "upstrem": "upstream", "prob": "probability",
		"Name": "name", "path": "path_pattern", "zzzzzzzz": "", "n": "",
	} {
		if got := suggest(key, names); got != want {
			t.Errorf("suggest(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestJSONFieldsFlattenEmbedded(t *testing.T) {
	fs := jsonFields(reflect.TypeFor[ServiceConfig]())
	for _, name := range []string{"name", "listen_addr", "upstream", "recording", "profile", "match", "latency", "id"} {
		if _, ok := fs.byName[name]; !ok {
			t.Errorf("field %q missing from %v", name, fs.names)
		}
	}
	if _, ok := fs.byName["Service"]; ok {
		t.Error("the embedded struct itself must not be a field")
	}
	cfg := jsonFields(reflect.TypeFor[Config]())
	if _, ok := cfg.byName["Dir"]; ok {
		t.Error("json:\"-\" field Dir must be hidden")
	}
	if _, ok := cfg.byName["importHAR"]; ok {
		t.Error("unexported field must be hidden")
	}
	if got := fs.byName["timeout"]; got != reflect.TypeFor[model.Duration]() {
		t.Errorf("timeout type = %v", got)
	}
}
