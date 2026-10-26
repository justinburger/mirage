package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

const fullYAML = `
server:
  admin: 127.0.0.1:8484
  data: /var/lib/mirage/mirage.db
  admin_token: s3cret
  log_level: debug
  log_format: json
recordings:
  - name: cap
    description: captured traffic
    har: captures/a.har
profiles:
  - name: strict
    detectors: [email, credit_card]
    detector_strategy: hash
    redact_headers: [Authorization]
    opaque_bodies: fill
    secret: fixed-secret
    rules:
      - name: emails
        enabled: true
        target: json
        selector: $..email
        strategy: fake
        fake: email
      - name: tail
        enabled: true
        target: json
        selector: $.card
        strategy: mask
        keep_suffix: 4
services:
  - name: web
    listen_addr: ":9001"
    upstream: http://127.0.0.1:7001
    mode: record
    enabled: true
    recording: cap
    profile: strict
    anonymize_capture: true
    endpoint_patterns: ["/users/{id}"]
    timeout: 1500
    max_body_bytes: 0x400
    capture_sample: 0.5
    tls: {self_signed: true}
    match:
      query: subset
      ignore_params: [_ts]
      selection: random
    latency:
      mode: fixed
      fixed: 250ms
      jitter: 1m30s
    error_amplification: 2.5
faults:
  - name: slow search
    enabled: true
    order: 3
    service_id: web
    methods: [GET, HEAD]
    path_pattern: /api/search
    headers:
      X-Beta: yes
    probability: 1e-1
    type: latency
    delay: 200ms
    jitter: 50
  - name: boom
    type: error
    status: 500
    probability: 0.5
    response_headers:
      Retry-After: 1
      X-Flag: true
`

// fullJSON is fullYAML as JSON (headers are strings in JSON).
const fullJSON = `{
  "server": {"admin": "127.0.0.1:8484", "data": "/var/lib/mirage/mirage.db", "admin_token": "s3cret", "log_level": "debug", "log_format": "json"},
  "recordings": [{"name": "cap", "description": "captured traffic", "har": "captures/a.har"}],
  "profiles": [{
    "name": "strict", "detectors": ["email", "credit_card"], "detector_strategy": "hash",
    "redact_headers": ["Authorization"], "opaque_bodies": "fill", "secret": "fixed-secret",
    "rules": [
      {"name": "emails", "enabled": true, "target": "json", "selector": "$..email", "strategy": "fake", "fake": "email"},
      {"name": "tail", "enabled": true, "target": "json", "selector": "$.card", "strategy": "mask", "keep_suffix": 4}
    ]}],
  "services": [{
    "name": "web", "listen_addr": ":9001", "upstream": "http://127.0.0.1:7001", "mode": "record", "enabled": true,
    "recording": "cap", "profile": "strict", "anonymize_capture": true, "endpoint_patterns": ["/users/{id}"],
    "timeout": 1500, "max_body_bytes": 1024, "capture_sample": 0.5, "tls": {"self_signed": true},
    "match": {"query": "subset", "ignore_params": ["_ts"], "selection": "random"},
    "latency": {"mode": "fixed", "fixed": "250ms", "jitter": "1m30s"}, "error_amplification": 2.5
  }],
  "faults": [
    {"name": "slow search", "enabled": true, "order": 3, "service_id": "web", "methods": ["GET", "HEAD"],
     "path_pattern": "/api/search", "headers": {"X-Beta": "yes"}, "probability": 0.1, "type": "latency",
     "delay": "200ms", "jitter": 50},
    {"name": "boom", "type": "error", "status": 500, "probability": 0.5,
     "response_headers": {"Retry-After": "1", "X-Flag": "true"}}
  ]
}`

func mustParse(t *testing.T, name, src string) *Config {
	t.Helper()
	c, err := Parse(name, []byte(src))
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return c
}

func TestParseFullYAML(t *testing.T) {
	c := mustParse(t, "mirage.yaml", fullYAML)

	if !reflect.DeepEqual(c.Server, Server{Admin: "127.0.0.1:8484", Data: "/var/lib/mirage/mirage.db", AdminToken: "s3cret", LogLevel: "debug", LogFormat: "json"}) {
		t.Errorf("server = %+v", c.Server)
	}
	if len(c.Recordings) != 1 || c.Recordings[0] != (RecordingConfig{Name: "cap", Description: "captured traffic", HAR: "captures/a.har"}) {
		t.Errorf("recordings = %+v", c.Recordings)
	}

	if len(c.Profiles) != 1 {
		t.Fatalf("profiles = %d", len(c.Profiles))
	}
	p := c.Profiles[0]
	if p.Name != "strict" || p.Secret != "fixed-secret" || p.DetectorStrategy != model.StrategyHash || p.OpaqueBodies != model.OpaqueFill {
		t.Errorf("profile = %+v", p)
	}
	if !reflect.DeepEqual(p.Detectors, []model.Detector{model.DetectEmail, model.DetectCreditCard}) {
		t.Errorf("detectors = %v", p.Detectors)
	}
	if len(p.Rules) != 2 || p.Rules[1].KeepSuffix != 4 || p.Rules[0].Fake != model.FakeEmail || !p.Rules[0].Enabled {
		t.Errorf("rules = %+v", p.Rules)
	}

	s := c.Services[0]
	if s.Name != "web" || s.ListenAddr != ":9001" || s.Mode != model.ModeRecord || !s.Enabled || s.Recording != "cap" || s.Profile != "strict" {
		t.Errorf("service = %+v", s)
	}
	if s.Timeout.D() != 1500*time.Millisecond {
		t.Errorf("timeout from a bare number = %v, want 1.5s", s.Timeout)
	}
	if s.MaxBodyBytes != 1024 || s.CaptureSample != 0.5 || s.ErrorAmplification != 2.5 {
		t.Errorf("numbers: %d %v %v", s.MaxBodyBytes, s.CaptureSample, s.ErrorAmplification)
	}
	if s.TLS == nil || !s.TLS.SelfSigned {
		t.Errorf("tls = %+v", s.TLS)
	}
	if s.Match.Query != model.QuerySubset || s.Match.Selection != model.SelectRandom || !reflect.DeepEqual(s.Match.IgnoreParams, []string{"_ts"}) {
		t.Errorf("match = %+v", s.Match)
	}
	if s.Latency.Mode != model.LatencyFixed || s.Latency.Fixed.D() != 250*time.Millisecond || s.Latency.Jitter.D() != 90*time.Second {
		t.Errorf("latency = %+v", s.Latency)
	}

	f := c.Faults[0]
	if f.Probability != 0.1 || f.Delay.D() != 200*time.Millisecond || f.Jitter.D() != 50*time.Millisecond || f.Headers["X-Beta"] != "yes" {
		t.Errorf("fault = %+v", f)
	}
	// Plain YAML numbers and booleans fill string fields as written.
	if h := c.Faults[1].ResponseHeader; h["Retry-After"] != "1" || h["X-Flag"] != "true" {
		t.Errorf("response headers = %v", h)
	}
	if f := c.Faults[0].Headers; f["X-Beta"] != "yes" {
		t.Errorf("YAML 'yes' should stay the string yes, got %v", f)
	}
}

func TestParseJSONMatchesYAML(t *testing.T) {
	y := mustParse(t, "a.yaml", fullYAML)
	j := mustParse(t, "a.json", fullJSON)
	if !reflect.DeepEqual(y, j) {
		t.Errorf("YAML and JSON decode differently:\nyaml: %+v\njson: %+v", y, j)
	}
	// Any other extension is tried as YAML, which also reads JSON.
	for _, name := range []string{"conf", "conf.txt", "", "conf.yml", "CONF.YAML"} {
		if got := mustParse(t, name, fullJSON); !reflect.DeepEqual(got, j) {
			t.Errorf("%q: differs from the JSON decode", name)
		}
	}
}

func TestParseServerAllowedHosts(t *testing.T) {
	c := mustParse(t, "c.yaml", "server:\n  allowed_hosts: [mirage.lan, \"mirage.lan:8484\"]\n")
	if !reflect.DeepEqual(c.Server.AllowedHosts, []string{"mirage.lan", "mirage.lan:8484"}) {
		t.Errorf("allowed_hosts = %q", c.Server.AllowedHosts)
	}
}

func TestParseEnvExpansion(t *testing.T) {
	t.Setenv("MIRAGE_TEST_TOKEN", "tok123")
	t.Setenv("MIRAGE_TEST_PORT", "9100")
	t.Setenv("MIRAGE_TEST_EMPTY", "")
	src := `
server:
  # admin_token: ${MIRAGE_TEST_NOT_SET}
  admin_token: ${MIRAGE_TEST_TOKEN}
  data: ${MIRAGE_TEST_UNSET:-/data/mirage.db}
  log_level: ${MIRAGE_TEST_EMPTY:-warn}
services:
  - name: web
    listen_addr: ":${MIRAGE_TEST_PORT}"
    upstream: http://x:1
`
	c := mustParse(t, "c.yaml", src)
	if c.Server.AdminToken != "tok123" || c.Server.Data != "/data/mirage.db" || c.Server.LogLevel != "warn" {
		t.Errorf("server = %+v", c.Server)
	}
	if c.Services[0].ListenAddr != ":9100" {
		t.Errorf("listen_addr = %q", c.Services[0].ListenAddr)
	}

	_, err := Parse("c.yaml", []byte("server:\n  data: ${MIRAGE_TEST_NOT_SET}\n"))
	var ce *Error
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "MIRAGE_TEST_NOT_SET") || ce.Problems[0].Line != 2 {
		t.Errorf("err = %v", err)
	}
	// JSON files expand too, and '#' is not a comment there.
	c = mustParse(t, "c.json", `{"server": {"data": "${MIRAGE_TEST_TOKEN}"}}`)
	if c.Server.Data != "tok123" {
		t.Errorf("json data = %q", c.Server.Data)
	}
}

func TestParseEmptyDocuments(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"a.yaml", ""},
		{"a.yaml", "# only a comment\n"},
		{"a.yaml", "---\n"},
		{"a.yaml", "null\n"},
		{"a.yaml", "{}\n"},
		{"a.json", ""},
		{"a.json", "  \n"},
		{"a.json", "null"},
		{"a.json", "{}"},
		{"a.json", "\xef\xbb\xbf{}"},
	} {
		c, err := Parse(tc.name, []byte(tc.src))
		if err != nil {
			t.Errorf("Parse(%q, %q): %v", tc.name, tc.src, err)
			continue
		}
		if len(c.Services)+len(c.Profiles)+len(c.Faults)+len(c.Recordings) != 0 {
			t.Errorf("Parse(%q, %q) = %+v, want empty", tc.name, tc.src, c)
		}
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name  string
		file  string
		src   string
		parts []string // every part must appear in the error text
		line  int      // line of the first problem, 0 = don't check
	}{
		{
			name: "unknown field names the field, path, line and a suggestion",
			file: "m.yaml",
			src: `services:
  - name: web
    listen_adr: ":9001"
`,
			parts: []string{"m.yaml", "line 3", "services[0].listen_adr", `unknown field "listen_adr"`, `did you mean "listen_addr"`},
			line:  3,
		},
		{
			name:  "unknown top-level section",
			file:  "m.yaml",
			src:   "servers: []\n",
			parts: []string{`unknown field "servers"`, `did you mean "server"`},
			line:  1,
		},
		{
			name:  "unknown field with no close match",
			file:  "m.yaml",
			src:   "server:\n  frobnicate: 1\n",
			parts: []string{"server.frobnicate", `unknown field "frobnicate"`},
		},
		{
			name:  "unknown field in embedded model section",
			file:  "m.yaml",
			src:   "services:\n  - name: a\n    match:\n      qury: exact\n",
			parts: []string{"services[0].match.qury", `did you mean "query"`},
			line:  4,
		},
		{
			name:  "unknown field in JSON",
			file:  "m.json",
			src:   "{\n  \"faults\": [\n    {\"name\": \"x\", \"prob\": 1}\n  ]\n}",
			parts: []string{"m.json", "line 3", "faults[0].prob", `did you mean "probability"`},
			line:  3,
		},
		{
			name:  "field names are case sensitive",
			file:  "m.yaml",
			src:   "services:\n  - Name: web\n",
			parts: []string{"services[0].Name", `did you mean "name"`},
		},
		{
			name:  "wrong scalar type",
			file:  "m.yaml",
			src:   "faults:\n  - name: x\n    type: error\n    probability: lots\n",
			parts: []string{"faults[0].probability", "expected a number", `string "lots"`},
			line:  4,
		},
		{
			name:  "float where integer expected",
			file:  "m.yaml",
			src:   "faults:\n  - name: x\n    type: error\n    status: 503.5\n",
			parts: []string{"faults[0].status", "expected an integer", "number 503.5"},
		},
		{
			name:  "bool expected",
			file:  "m.yaml",
			src:   "services:\n  - name: x\n    enabled: \"yes\"\n",
			parts: []string{"services[0].enabled", "expected bool"},
		},
		{
			name:  "list expected",
			file:  "m.yaml",
			src:   "services:\n  - name: x\n    endpoint_patterns: /users/{id}\n",
			parts: []string{"services[0].endpoint_patterns", "expected a list"},
		},
		{
			name:  "object expected",
			file:  "m.yaml",
			src:   "services:\n  - name: x\n    match: exact\n",
			parts: []string{"services[0].match", "expected an object"},
		},
		{
			name:  "section must be a list",
			file:  "m.yaml",
			src:   "services:\n  name: x\n",
			parts: []string{"services", "expected a list, got object"},
		},
		{
			name:  "list item must be an object",
			file:  "m.yaml",
			src:   "faults:\n  - just a string\n",
			parts: []string{"faults[0]", "expected an object"},
		},
		{
			name:  "bad duration",
			file:  "m.yaml",
			src:   "faults:\n  - name: x\n    type: latency\n    delay: soon\n",
			parts: []string{"faults[0].delay", `invalid duration "soon"`},
			line:  4,
		},
		{
			name:  "duration given as a list",
			file:  "m.yaml",
			src:   "faults:\n  - name: x\n    type: latency\n    delay: [1, 2]\n",
			parts: []string{"faults[0].delay", "invalid duration"},
		},
		{
			name:  "bad timestamp",
			file:  "m.yaml",
			src:   "profiles:\n  - name: p\n    created_at: yesterday\n",
			parts: []string{"profiles[0].created_at"},
		},
		{
			name:  "string where object key expects number inside a map value",
			file:  "m.yaml",
			src:   "faults:\n  - name: x\n    type: error\n    response_headers:\n      A: [1]\n",
			parts: []string{"faults[0].response_headers.A", "expected a string, got list"},
		},
		{
			name: "several problems are reported together",
			file: "m.yaml",
			src: `server:
  verbose: true
services:
  - name: a
    lisen_addr: ":1"
faults:
  - name: f
    probability: much
`,
			parts: []string{"3 problems", "server.verbose", "services[0].lisen_addr", "faults[0].probability"},
		},
		{
			name:  "duplicate YAML key",
			file:  "m.yaml",
			src:   "server:\n  admin: a:1\n  admin: b:2\n",
			parts: []string{"line 3", `duplicate key "admin"`, "first used on line 2"},
			line:  3,
		},
		{
			name:  "duplicate JSON key",
			file:  "m.json",
			src:   "{\"server\": {\"admin\": \"a:1\",\n \"admin\": \"b:2\"}}",
			parts: []string{"line 2", `duplicate key "admin"`},
			line:  2,
		},
		{
			name:  "second YAML document",
			file:  "m.yaml",
			src:   "server: {}\n---\nserver: {}\n",
			parts: []string{"multiple YAML documents"},
		},
		{
			name:  "YAML syntax error",
			file:  "m.yaml",
			src:   "server:\n  admin: [unclosed\nservices: []\n",
			parts: []string{"m.yaml", "line"},
		},
		{
			name:  "YAML tab indent",
			file:  "m.yaml",
			src:   "server:\n\tadmin: x\n",
			parts: []string{"m.yaml"},
		},
		{
			name:  "JSON syntax error has a line",
			file:  "m.json",
			src:   "{\n  \"server\": {\n    \"admin\": ,\n  }\n}",
			parts: []string{"m.json", "line 3"},
			line:  3,
		},
		{
			name:  "JSON trailing data",
			file:  "m.json",
			src:   "{} {}",
			parts: []string{"unexpected data after"},
		},
		{
			name:  "JSON truncated",
			file:  "m.json",
			src:   "{\"server\": {",
			parts: []string{"m.json"},
		},
		{
			name:  "top level list",
			file:  "m.yaml",
			src:   "- a\n- b\n",
			parts: []string{"top level must be an object", "got list"},
		},
		{
			name:  "top level scalar",
			file:  "m.json",
			src:   "42",
			parts: []string{"top level must be an object", "got number"},
		},
		{
			name:  "non-finite number",
			file:  "m.yaml",
			src:   "faults:\n  - name: x\n    probability: .inf\n",
			parts: []string{"not a finite number"},
		},
		{
			name:  "unsupported tag",
			file:  "m.yaml",
			src:   "server:\n  admin: !!binary aGVsbG8=\n",
			parts: []string{"unsupported YAML tag"},
		},
		{
			name:  "non-scalar key",
			file:  "m.yaml",
			src:   "? [a, b]\n: 1\n",
			parts: []string{"keys must be plain strings"},
		},
		{
			name:  "env var missing",
			file:  "m.yaml",
			src:   "server:\n  data: ${MIRAGE_DEFINITELY_NOT_SET_1}\n",
			parts: []string{"m.yaml", "line 2", "MIRAGE_DEFINITELY_NOT_SET_1"},
			line:  2,
		},
		{
			name: "validation problems from the model",
			file: "m.yaml",
			src: `server:
  log_level: loud
  log_format: xml
  admin: nonsense
services:
  - name: bad name!
    listen_addr: nope
    mode: record
    upstream: ftp://x
    recording: r
  - name: ok
    listen_addr: ":9"
    upstream: http://u
    mode: virtualize
faults:
  - name: f
    type: error
    probability: 2
`,
			parts: []string{
				"server.log_level", "server.log_format", "server.admin",
				"services[0]", "name must be 1-64 chars", "listen_addr", "upstream",
				"services[1]", "needs a recording_id",
				"faults[0]", "probability must be between 0 and 1",
			},
		},
		{
			name:  "duplicate names",
			file:  "m.yaml",
			src:   "services:\n  - {name: a, listen_addr: ':1', upstream: 'http://x', mode: passthrough}\n  - {name: a, listen_addr: ':2', upstream: 'http://x', mode: passthrough}\nprofiles:\n  - name: P\n  - name: ' p '\nfaults:\n  - {name: f, type: abort}\n  - {name: f, type: abort}\nrecordings:\n  - name: r\n  - name: r\n  - name: ' '\n",
			parts: []string{"services[1]", "duplicate name", "profiles[1]", "ignore case", "faults[1]", "recordings[1]", "recordings[2]: name is required"},
		},
		{
			name:  "duplicate listen address spelled differently",
			file:  "m.yaml",
			src:   "services:\n  - {name: a, listen_addr: ':9001', upstream: 'http://x', mode: passthrough}\n  - {name: b, listen_addr: '0.0.0.0:9001', upstream: 'http://x', mode: passthrough}\n",
			parts: []string{"services[1]", "also used by services[0]"},
		},
		{
			name:  "port zero never clashes",
			file:  "m.yaml",
			src:   "services:\n  - {name: a, listen_addr: ':0', upstream: 'http://x', mode: passthrough}\n  - {name: b, listen_addr: ':0', upstream: 'http://x', mode: passthrough}\n",
			parts: nil,
		},
		{
			name:  "blank recording reference",
			file:  "m.yaml",
			src:   "services:\n  - {name: a, listen_addr: ':1', upstream: 'http://x', mode: record, recording: '  '}\n",
			parts: []string{"recording name must not be blank"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse(tc.file, []byte(tc.src))
			if tc.parts == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error, got config %+v", c)
			}
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("error %T is not *Error: %v", err, err)
			}
			if !errors.Is(err, model.ErrValidation) {
				t.Errorf("error does not wrap model.ErrValidation")
			}
			if ce.File != tc.file {
				t.Errorf("File = %q, want %q", ce.File, tc.file)
			}
			for _, part := range tc.parts {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not contain %q", err, part)
				}
			}
			if tc.line != 0 && ce.Problems[0].Line != tc.line {
				t.Errorf("first problem on line %d, want %d (%v)", ce.Problems[0].Line, tc.line, err)
			}
		})
	}
}

func TestParseProblemCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("server:\n")
	for i := 0; i < 50; i++ {
		b.WriteString("  bad")
		b.WriteString(strings.Repeat("x", i+1))
		b.WriteString(": 1\n")
	}
	_, err := Parse("m.yaml", []byte(b.String()))
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
	if len(ce.Problems) != maxProblems {
		t.Errorf("reported %d problems, want the cap of %d", len(ce.Problems), maxProblems)
	}
}

func TestParseAnchorsAndMerge(t *testing.T) {
	src := `
defaults: &svc
  upstream: http://127.0.0.1:7001
  mode: passthrough
  enabled: true
services:
  - <<: *svc
    name: a
    listen_addr: ":9001"
  - <<: *svc
    name: b
    listen_addr: ":9002"
    mode: virtualize
    recording: r
`
	// "defaults" is not a section, so the file is rejected...
	if _, err := Parse("m.yaml", []byte(src)); err == nil || !strings.Contains(err.Error(), `unknown field "defaults"`) {
		t.Fatalf("err = %v", err)
	}
	// ...but anchors defined inside an allowed section work.
	src = `
services:
  - &svc
    name: a
    listen_addr: ":9001"
    upstream: http://127.0.0.1:7001
    mode: passthrough
    enabled: true
  - <<: *svc
    name: b
    listen_addr: ":9002"
    mode: virtualize
    recording: r
`
	c := mustParse(t, "m.yaml", src)
	a, b := c.Services[0], c.Services[1]
	if b.Upstream != a.Upstream || !b.Enabled || b.Name != "b" || b.Mode != model.ModeVirtualize || b.ListenAddr != ":9002" {
		t.Errorf("merged service = %+v", b)
	}

	// An alias bomb is refused instead of exhausting memory.
	var bomb strings.Builder
	bomb.WriteString("services:\n  - name: a\n    endpoint_patterns: &l0 [/a, /a, /a, /a, /a, /a, /a, /a, /a, /a]\n")
	prev := "l0"
	for i := 1; i <= 8; i++ {
		cur := "l" + string(rune('0'+i))
		bomb.WriteString("    x" + cur + ": &" + cur + " [")
		bomb.WriteString(strings.TrimSuffix(strings.Repeat("*"+prev+", ", 10), ", "))
		bomb.WriteString("]\n")
		prev = cur
	}
	if _, err := Parse("m.yaml", []byte(bomb.String())); err == nil || !strings.Contains(err.Error(), "too many values") {
		t.Errorf("alias bomb: err = %v", err)
	}
}

func TestParseDeepNesting(t *testing.T) {
	deep := strings.Repeat("[", 300) + strings.Repeat("]", 300)
	for _, tc := range []struct{ name, src string }{
		{"a.json", `{"x": ` + deep + `}`},
		{"a.yaml", "x: " + deep},
	} {
		_, err := Parse(tc.name, []byte(tc.src))
		if err == nil {
			t.Errorf("%s: expected an error for deeply nested input", tc.name)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf", "mirage.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fullYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Dir != filepath.Dir(path) {
		t.Errorf("Dir = %q, want %q", c.Dir, filepath.Dir(path))
	}
	if c.harPath("captures/a.har") != filepath.Join(filepath.Dir(path), "captures", "a.har") {
		t.Errorf("harPath = %q", c.harPath("captures/a.har"))
	}
	if c.harPath("/abs/x.har") != "/abs/x.har" {
		t.Errorf("absolute har path was rewritten: %q", c.harPath("/abs/x.har"))
	}

	// A relative path gets an absolute Dir.
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	c, err = Load(filepath.Join("conf", "mirage.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.Dir) {
		t.Errorf("Dir %q is not absolute", c.Dir)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()

	_, err := Load(filepath.Join(dir, "missing.yaml"))
	if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "missing.yaml") {
		t.Errorf("missing file: err = %v", err)
	}

	if _, err := Load(dir); err == nil {
		t.Error("loading a directory should fail")
	}

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("server:\n  nope: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(bad)
	var ce *Error
	if !errors.As(err, &ce) || ce.File != bad || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("bad file: err = %v", err)
	}
}

func TestLoadExampleConfig(t *testing.T) {
	c, err := Load("../../examples/mirage.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Recordings) != 1 || len(c.Profiles) != 1 || len(c.Services) != 1 || len(c.Faults) != 2 {
		t.Fatalf("sections: %d recordings, %d profiles, %d services, %d faults", len(c.Recordings), len(c.Profiles), len(c.Services), len(c.Faults))
	}
	s := c.Services[0]
	if s.Name != "demo" || s.ListenAddr != ":9001" || s.Upstream != "http://127.0.0.1:7001" || s.Mode != model.ModeRecord ||
		s.Recording != "demo-capture" || !s.AnonymizeCapture || s.Profile != "strict" || !s.Enabled {
		t.Errorf("service = %+v", s)
	}
	if c.Profiles[0].Name != "strict" || len(c.Profiles[0].Rules) < 3 {
		t.Errorf("profile = %+v", c.Profiles[0])
	}
	for _, r := range c.Profiles[0].Rules {
		if !r.Enabled {
			t.Errorf("rule %q is not enabled", r.Name)
		}
	}
	for _, f := range c.Faults {
		if f.Enabled {
			t.Errorf("fault %q should be disabled", f.Name)
		}
	}
	if f := c.Faults[0]; f.Probability != 0.1 || f.Status != 503 || f.PathPattern != "/api/orders/**" || f.Type != model.FaultError {
		t.Errorf("fault 0 = %+v", f)
	}
	if f := c.Faults[1]; f.Type != model.FaultLatency || f.Delay.D() != 200*time.Millisecond || f.PathPattern != "/api/search" {
		t.Errorf("fault 1 = %+v", f)
	}
	if c.Server.Admin != "127.0.0.1:8484" || c.Server.AdminToken != "" {
		t.Errorf("server = %+v", c.Server)
	}
}

func TestDocsExampleJSONRoundTrip(t *testing.T) {
	// The API form of a service (docs/API.md) is accepted as a config entry.
	src := `{"services":[{"id":"svc_ab12cd34ef56gh78","name":"payments","listen_addr":":9001",
	  "upstream":"https://payments.internal:8443","mode":"passthrough","enabled":true,
	  "match":{"query":"exact","ignore_params":["_ts"],"body":"json","headers":[],"path_templates":true,"selection":"sequential","miss_status":404},
	  "latency":{"mode":"recorded","multiplier":1,"fixed":"0s","jitter":"0s"},
	  "created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-02T00:00:00Z"}]}`
	c := mustParse(t, "svc.json", src)
	if c.Services[0].ID != "svc_ab12cd34ef56gh78" || c.Services[0].CreatedAt.Year() != 2026 {
		t.Errorf("service = %+v", c.Services[0])
	}
}
