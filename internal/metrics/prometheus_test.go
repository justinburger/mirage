package metrics

import (
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/justinburger/mirage/internal/model"
)

var (
	metricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

type promSampleLine struct {
	name   string
	labels map[string]string
	value  float64
}

// key identifies the series: the name plus its labels in sorted order.
func (s promSampleLine) key() string {
	keys := make([]string, 0, len(s.labels))
	for k := range s.labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(s.name)
	for _, k := range keys {
		fmt.Fprintf(&b, "|%s=%q", k, s.labels[k])
	}
	return b.String()
}

type promFamilyInfo struct {
	typ     string
	help    string
	samples int
}

// parseProm is a strict reader for the text exposition format 0.0.4. It fails
// the test on anything a Prometheus server would reject or that violates the
// format's rules: invalid names or escapes, a HELP/TYPE pair repeated or
// arriving after samples, samples outside a declared family, duplicate series.
func parseProm(t *testing.T, text string) (map[string]*promFamilyInfo, []promSampleLine) {
	t.Helper()
	fams, samples, err := parsePromText(text)
	if err != nil {
		t.Fatal(err)
	}
	return fams, samples
}

// parseFail carries a format violation out of the recursive-free parser.
type parseFail struct{ msg string }

func failf(format string, args ...any) { panic(parseFail{fmt.Sprintf(format, args...)}) }

func parsePromText(text string) (fams map[string]*promFamilyInfo, samples []promSampleLine, err error) {
	defer func() {
		if r := recover(); r != nil {
			pf, ok := r.(parseFail)
			if !ok {
				panic(r)
			}
			err = errors.New(pf.msg)
		}
	}()
	if text != "" && !strings.HasSuffix(text, "\n") {
		failf("output does not end with a newline")
	}
	if !utf8.ValidString(text) {
		failf("output is not valid UTF-8")
	}
	fams = map[string]*promFamilyInfo{}
	helpSeen := map[string]int{}
	typeSeen := map[string]int{}
	seen := map[string]bool{}

	familyOf := func(name string) *promFamilyInfo {
		if f, ok := fams[name]; ok {
			return f
		}
		for _, suffix := range []string{"_sum", "_count"} {
			if base, ok := strings.CutSuffix(name, suffix); ok {
				if f, ok := fams[base]; ok && f.typ == "summary" {
					return f
				}
			}
		}
		return nil
	}

	for n, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		where := fmt.Sprintf("line %d %q", n+1, line)
		switch {
		case line == "":
			failf("%s: blank line", where)
		case strings.HasPrefix(line, "# HELP "):
			name, help, _ := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			if !metricNameRE.MatchString(name) {
				failf("%s: bad metric name", where)
			}
			helpSeen[name]++
			if fams[name] == nil {
				fams[name] = &promFamilyInfo{}
			}
			if fams[name].samples > 0 {
				failf("%s: HELP after samples", where)
			}
			fams[name].help = help
		case strings.HasPrefix(line, "# TYPE "):
			name, typ, _ := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
			if !metricNameRE.MatchString(name) {
				failf("%s: bad metric name", where)
			}
			switch typ {
			case "counter", "gauge", "summary", "histogram", "untyped":
			default:
				failf("%s: bad type", where)
			}
			typeSeen[name]++
			if fams[name] == nil {
				fams[name] = &promFamilyInfo{}
			}
			if fams[name].samples > 0 {
				failf("%s: TYPE after samples", where)
			}
			fams[name].typ = typ
		case strings.HasPrefix(line, "#"):
			failf("%s: unexpected comment", where)
		default:
			s := parseSampleLine(line, where)
			f := familyOf(s.name)
			if f == nil {
				failf("%s: sample without a declared family", where)
			}
			f.samples++
			if _, dup := seen[s.key()]; dup {
				failf("%s: duplicate series", where)
			}
			seen[s.key()] = true
			if f.typ == "summary" && s.name == strings.TrimSuffix(s.name, "_sum") && !strings.HasSuffix(s.name, "_count") {
				if _, ok := s.labels["quantile"]; !ok {
					failf("%s: summary sample without quantile label", where)
				}
			}
			samples = append(samples, s)
		}
	}
	for name := range fams {
		if helpSeen[name] != 1 || typeSeen[name] != 1 {
			failf("family %s: HELP x%d, TYPE x%d, want exactly one of each", name, helpSeen[name], typeSeen[name])
		}
	}
	return fams, samples, nil
}

func parseSampleLine(line, where string) promSampleLine {
	s := promSampleLine{labels: map[string]string{}}
	i := strings.IndexAny(line, "{ ")
	if i < 0 {
		failf("%s: no value", where)
	}
	s.name = line[:i]
	if !metricNameRE.MatchString(s.name) {
		failf("%s: bad metric name", where)
	}
	rest := line[i:]
	if rest[0] == '{' {
		rest = rest[1:]
		for {
			if strings.HasPrefix(rest, "}") {
				rest = rest[1:]
				break
			}
			eq := strings.Index(rest, `="`)
			if eq < 0 {
				failf("%s: bad label syntax", where)
			}
			name := rest[:eq]
			if !labelNameRE.MatchString(name) {
				failf("%s: bad label name %q", where, name)
			}
			rest = rest[eq+2:]
			var val strings.Builder
			closed := false
			for j := 0; j < len(rest); j++ {
				ch := rest[j]
				if ch == '\\' {
					j++
					if j >= len(rest) {
						failf("%s: dangling escape", where)
					}
					switch rest[j] {
					case '\\':
						val.WriteByte('\\')
					case '"':
						val.WriteByte('"')
					case 'n':
						val.WriteByte('\n')
					default:
						failf("%s: invalid escape \\%c", where, rest[j])
					}
					continue
				}
				if ch == '"' {
					rest = rest[j+1:]
					closed = true
					break
				}
				val.WriteByte(ch)
			}
			if !closed {
				failf("%s: unterminated label value", where)
			}
			if _, dup := s.labels[name]; dup {
				failf("%s: duplicate label %q", where, name)
			}
			s.labels[name] = val.String()
			if strings.HasPrefix(rest, ",") {
				rest = rest[1:]
			} else if !strings.HasPrefix(rest, "}") {
				failf("%s: expected , or } after label", where)
			}
		}
	}
	if !strings.HasPrefix(rest, " ") {
		failf("%s: expected space before value", where)
	}
	val := strings.TrimPrefix(rest, " ")
	switch val {
	case "NaN":
		s.value = math.NaN()
	case "+Inf":
		s.value = math.Inf(1)
	case "-Inf":
		s.value = math.Inf(-1)
	default:
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			failf("%s: bad value %q", where, val)
		}
		s.value = f
	}
	return s
}

func renderProm(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	if err := r.WritePrometheus(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// find returns the one sample with this name whose labels include want.
func find(t *testing.T, samples []promSampleLine, name string, want map[string]string) float64 {
	t.Helper()
	var hit []promSampleLine
outer:
	for _, s := range samples {
		if s.name != name {
			continue
		}
		for k, v := range want {
			if s.labels[k] != v {
				continue outer
			}
		}
		hit = append(hit, s)
	}
	if len(hit) != 1 {
		t.Fatalf("%s%v matched %d samples, want 1", name, want, len(hit))
	}
	return hit[0].value
}

func TestWritePrometheusGolden(t *testing.T) {
	r := NewRegistry()
	c := r.Collector("service:svc_1", model.KindService, "svc_1")
	c.Observe(Observation{Endpoint: "GET /a", Status: 200, Duration: 1500 * time.Microsecond, BytesIn: 100, BytesOut: 2500})
	c.InFlight(2)

	want := `# HELP mirage_requests_total Requests completed.
# TYPE mirage_requests_total counter
mirage_requests_total{kind="service",id="svc_1"} 1
# HELP mirage_errors_total Requests that counted as errors (5xx, 429 or no response).
# TYPE mirage_errors_total counter
mirage_errors_total{kind="service",id="svc_1"} 0
# HELP mirage_responses_total Responses by status class; err means no response.
# TYPE mirage_responses_total counter
mirage_responses_total{kind="service",id="svc_1",class="1xx"} 0
mirage_responses_total{kind="service",id="svc_1",class="2xx"} 1
mirage_responses_total{kind="service",id="svc_1",class="3xx"} 0
mirage_responses_total{kind="service",id="svc_1",class="4xx"} 0
mirage_responses_total{kind="service",id="svc_1",class="5xx"} 0
mirage_responses_total{kind="service",id="svc_1",class="err"} 0
# HELP mirage_request_duration_seconds Request service time in seconds.
# TYPE mirage_request_duration_seconds summary
mirage_request_duration_seconds{kind="service",id="svc_1",quantile="0.5"} 0.0015
mirage_request_duration_seconds{kind="service",id="svc_1",quantile="0.9"} 0.0015
mirage_request_duration_seconds{kind="service",id="svc_1",quantile="0.95"} 0.0015
mirage_request_duration_seconds{kind="service",id="svc_1",quantile="0.99"} 0.0015
mirage_request_duration_seconds_sum{kind="service",id="svc_1"} 0.0015
mirage_request_duration_seconds_count{kind="service",id="svc_1"} 1
# HELP mirage_in_flight_requests Requests currently in flight.
# TYPE mirage_in_flight_requests gauge
mirage_in_flight_requests{kind="service",id="svc_1"} 2
# HELP mirage_bytes_in_total Request bytes received.
# TYPE mirage_bytes_in_total counter
mirage_bytes_in_total{kind="service",id="svc_1"} 100
# HELP mirage_bytes_out_total Response bytes sent.
# TYPE mirage_bytes_out_total counter
mirage_bytes_out_total{kind="service",id="svc_1"} 2500
# HELP mirage_endpoint_requests_total Requests completed, per endpoint.
# TYPE mirage_endpoint_requests_total counter
mirage_endpoint_requests_total{kind="service",id="svc_1",endpoint="GET /a"} 1
# HELP mirage_endpoint_errors_total Requests that counted as errors, per endpoint.
# TYPE mirage_endpoint_errors_total counter
mirage_endpoint_errors_total{kind="service",id="svc_1",endpoint="GET /a"} 0
# HELP mirage_endpoint_request_duration_seconds Request service time in seconds, per endpoint.
# TYPE mirage_endpoint_request_duration_seconds summary
mirage_endpoint_request_duration_seconds{kind="service",id="svc_1",endpoint="GET /a",quantile="0.5"} 0.0015
mirage_endpoint_request_duration_seconds{kind="service",id="svc_1",endpoint="GET /a",quantile="0.9"} 0.0015
mirage_endpoint_request_duration_seconds{kind="service",id="svc_1",endpoint="GET /a",quantile="0.95"} 0.0015
mirage_endpoint_request_duration_seconds{kind="service",id="svc_1",endpoint="GET /a",quantile="0.99"} 0.0015
mirage_endpoint_request_duration_seconds_sum{kind="service",id="svc_1",endpoint="GET /a"} 0.0015
mirage_endpoint_request_duration_seconds_count{kind="service",id="svc_1",endpoint="GET /a"} 1
`
	got := renderProm(t, r)
	if got != want {
		t.Errorf("output differs from golden.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	parseProm(t, got) // and the golden itself is valid
}

func TestWritePrometheusFamilies(t *testing.T) {
	r := NewRegistry()
	svc := r.Collector(ServiceCollectorName("svc_a"), model.KindService, "svc_a")
	rpl := r.Collector(ReplayCollectorName("rpl_b"), model.KindReplay, "rpl_b")
	idle := r.Collector(ServiceCollectorName("svc_idle"), model.KindService, "svc_idle")
	_ = idle

	for range 4 {
		svc.Observe(Observation{Endpoint: "GET /users/{id}", Status: 200, Duration: 250 * time.Millisecond, BytesIn: 10, BytesOut: 1000})
	}
	svc.Observe(Observation{Endpoint: "POST /orders", Status: 503, Duration: 2 * time.Second, BytesIn: 500, Error: true})
	svc.Observe(Observation{Endpoint: "POST /orders", Status: 429, Duration: time.Second, Error: true})
	svc.Observe(Observation{Endpoint: "POST /orders", Status: 404, Duration: time.Second})
	svc.Observe(Observation{Endpoint: "GET /users/{id}", Status: 0, Duration: 40 * time.Second, Error: true})
	svc.InFlight(3)
	rpl.Observe(Observation{Endpoint: "GET /x", Status: 301, Duration: time.Millisecond, BytesOut: 7})

	fams, samples := parseProm(t, renderProm(t, r))

	wantTypes := map[string]string{
		"mirage_requests_total":                    "counter",
		"mirage_errors_total":                      "counter",
		"mirage_responses_total":                   "counter",
		"mirage_request_duration_seconds":          "summary",
		"mirage_in_flight_requests":                "gauge",
		"mirage_bytes_in_total":                    "counter",
		"mirage_bytes_out_total":                   "counter",
		"mirage_endpoint_requests_total":           "counter",
		"mirage_endpoint_errors_total":             "counter",
		"mirage_endpoint_request_duration_seconds": "summary",
	}
	for name, typ := range wantTypes {
		f := fams[name]
		if f == nil {
			t.Errorf("family %s missing", name)
			continue
		}
		if f.typ != typ {
			t.Errorf("%s TYPE = %q, want %q", name, f.typ, typ)
		}
		if f.help == "" {
			t.Errorf("%s has no HELP text", name)
		}
		if f.samples == 0 {
			t.Errorf("%s has no samples", name)
		}
	}
	if len(fams) != len(wantTypes) {
		t.Errorf("%d families, want %d", len(fams), len(wantTypes))
	}

	a := map[string]string{"kind": "service", "id": "svc_a"}
	with := func(extra ...string) map[string]string {
		m := map[string]string{"kind": "service", "id": "svc_a"}
		for i := 0; i < len(extra); i += 2 {
			m[extra[i]] = extra[i+1]
		}
		return m
	}
	checks := []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"mirage_requests_total", a, 8},
		{"mirage_errors_total", a, 3},
		{"mirage_in_flight_requests", a, 3},
		{"mirage_bytes_in_total", a, 4*10 + 500},
		{"mirage_bytes_out_total", a, 4000},
		{"mirage_responses_total", with("class", "2xx"), 4},
		{"mirage_responses_total", with("class", "4xx"), 2},
		{"mirage_responses_total", with("class", "5xx"), 1},
		{"mirage_responses_total", with("class", "err"), 1},
		{"mirage_responses_total", with("class", "1xx"), 0},
		{"mirage_request_duration_seconds_count", a, 8},
		{"mirage_request_duration_seconds_sum", a, 4*0.25 + 2 + 1 + 1 + 40},
		{"mirage_endpoint_requests_total", with("endpoint", "GET /users/{id}"), 5},
		{"mirage_endpoint_requests_total", with("endpoint", "POST /orders"), 3},
		{"mirage_endpoint_errors_total", with("endpoint", "POST /orders"), 2},
		{"mirage_endpoint_errors_total", with("endpoint", "GET /users/{id}"), 1},
		{"mirage_endpoint_request_duration_seconds_count", with("endpoint", "POST /orders"), 3},
		{"mirage_endpoint_request_duration_seconds_sum", with("endpoint", "POST /orders"), 4},
		{"mirage_requests_total", map[string]string{"kind": "replay", "id": "rpl_b"}, 1},
		{"mirage_responses_total", map[string]string{"kind": "replay", "id": "rpl_b", "class": "3xx"}, 1},
		{"mirage_bytes_out_total", map[string]string{"kind": "replay", "id": "rpl_b"}, 7},
		{"mirage_requests_total", map[string]string{"id": "svc_idle"}, 0},
	}
	for _, c := range checks {
		got := find(t, samples, c.name, c.labels)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}

	// Durations are seconds: the median of 4x250ms, 2x1s, 1s, 2s, 40s is
	// 250ms-1s territory, certainly not milliseconds.
	p50 := find(t, samples, "mirage_request_duration_seconds", with("quantile", "0.5"))
	if p50 < 0.2 || p50 > 1.1 {
		t.Errorf("p50 = %v s, want between 0.2 and 1.1", p50)
	}
	// All four quantiles are present, ordered, and bounded by the extremes.
	prev := 0.0
	for _, q := range []string{"0.5", "0.9", "0.95", "0.99"} {
		v := find(t, samples, "mirage_request_duration_seconds", with("quantile", q))
		if v < prev || v > 40.0001 {
			t.Errorf("quantile %s = %v out of order/range (prev %v)", q, v, prev)
		}
		prev = v
	}

	// Summing the service-wide counter equals summing the per-endpoint one:
	// no double counting between the two families.
	var total, perEndpoint float64
	for _, s := range samples {
		switch s.name {
		case "mirage_requests_total":
			total += s.value
		case "mirage_endpoint_requests_total":
			perEndpoint += s.value
		}
	}
	if total != perEndpoint || total != 9 {
		t.Errorf("sum(requests_total)=%v sum(endpoint_requests_total)=%v, want both 9", total, perEndpoint)
	}
}

func TestWritePrometheusEscaping(t *testing.T) {
	endpoints := map[string]string{ // raw endpoint -> expected decoded label value
		`GET /quote"d`:             `GET /quote"d`,
		`GET /back\slash`:          `GET /back\slash`,
		"GET /new\nline":           "GET /new\nline",
		`GET /both\"`:              `GET /both\"`,
		"GET /café/日本":             "GET /café/日本",
		"GET /bad\xffbytes":        "GET /bad�bytes",
		`GET /braces/{id}?a=1,b=2`: `GET /braces/{id}?a=1,b=2`,
		"GET /trailing\\":          "GET /trailing\\",
	}
	r := NewRegistry()
	c := r.Collector(`service:odd"id\`, model.KindService, "odd\"id\\\n")
	for ep := range endpoints {
		c.Observe(Observation{Endpoint: ep, Status: 200, Duration: time.Millisecond})
	}
	text := renderProm(t, r)

	// Hand-written assertions on the raw bytes.
	for _, frag := range []string{
		`endpoint="GET /quote\"d"`,
		`endpoint="GET /back\\slash"`,
		`endpoint="GET /new\nline"`,
		`endpoint="GET /both\\\""`,
		`endpoint="GET /trailing\\"`,
		`id="odd\"id\\\n"`,
	} {
		if !strings.Contains(text, frag) {
			t.Errorf("output lacks %s", frag)
		}
	}
	if strings.Contains(text, "\xff") {
		t.Error("invalid UTF-8 leaked into the output")
	}

	// And the strict parser, which also validates escapes, round-trips them.
	_, samples := parseProm(t, text)
	got := map[string]bool{}
	for _, s := range samples {
		if s.name == "mirage_endpoint_requests_total" {
			got[s.labels["endpoint"]] = true
			if s.labels["id"] != "odd\"id\\\n" || s.labels["kind"] != "service" {
				t.Errorf("collector labels = %v", s.labels)
			}
		}
	}
	for _, want := range endpoints {
		if !got[want] {
			t.Errorf("endpoint %q not found after escaping round-trip; got %v", want, got)
		}
	}
	if len(got) != len(endpoints) {
		t.Errorf("%d distinct endpoints in output, want %d", len(got), len(endpoints))
	}
}

func TestEscapeLabel(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"plain", "plain"},
		{`a"b`, `a\"b`},
		{`a\b`, `a\\b`},
		{"a\nb", `a\nb`},
		{`\"`, `\\\"`},
		{"tab\there", "tab\there"},
		{"bad\xc3", "bad�"},
	}
	for _, tt := range tests {
		if got := escapeLabel(tt.in); got != tt.want {
			t.Errorf("escapeLabel(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatFloat(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0"}, {1, "1"}, {0.0015, "0.0015"}, {1e-9, "1e-09"}, {123456789, "1.23456789e+08"},
		{math.NaN(), "NaN"}, {math.Inf(1), "+Inf"}, {math.Inf(-1), "-Inf"}, {-2.5, "-2.5"},
	}
	for _, tt := range tests {
		if got := formatFloat(tt.in); got != tt.want {
			t.Errorf("formatFloat(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestWritePrometheusEmpty(t *testing.T) {
	// No collectors: still valid, every family declared once with no samples.
	text := renderProm(t, NewRegistry())
	fams, samples := parseProm(t, text)
	if len(samples) != 0 {
		t.Errorf("samples = %v", samples)
	}
	if len(fams) != 10 {
		t.Errorf("families = %d, want 10", len(fams))
	}

	// A collector that never saw traffic: zero counters, NaN quantiles.
	r := NewRegistry()
	r.Collector("service:idle", model.KindService, "idle")
	_, samples = parseProm(t, renderProm(t, r))
	id := map[string]string{"id": "idle"}
	if got := find(t, samples, "mirage_requests_total", id); got != 0 {
		t.Errorf("requests = %v", got)
	}
	if got := find(t, samples, "mirage_request_duration_seconds_count", id); got != 0 {
		t.Errorf("count = %v", got)
	}
	if got := find(t, samples, "mirage_request_duration_seconds_sum", id); got != 0 {
		t.Errorf("sum = %v", got)
	}
	for _, q := range []string{"0.5", "0.9", "0.95", "0.99"} {
		if got := find(t, samples, "mirage_request_duration_seconds", map[string]string{"id": "idle", "quantile": q}); !math.IsNaN(got) {
			t.Errorf("empty summary quantile %s = %v, want NaN", q, got)
		}
	}
	for _, s := range samples {
		if strings.HasPrefix(s.name, "mirage_endpoint_") {
			t.Errorf("endpoint series for an idle collector: %v", s)
		}
	}
}

func TestWritePrometheusIsSortedAndStable(t *testing.T) {
	r := NewRegistry()
	for _, id := range []string{"c", "a", "b"} {
		c := r.Collector(ServiceCollectorName(id), model.KindService, id)
		for _, ep := range []string{"GET /z", "GET /a", "GET /m"} {
			c.Observe(Observation{Endpoint: ep, Status: 200, Duration: time.Millisecond})
		}
	}
	first := renderProm(t, r)
	if second := renderProm(t, r); second != first {
		t.Error("output not stable across scrapes of an idle registry")
	}
	var ids, eps []string
	_, samples := parseProm(t, first)
	for _, s := range samples {
		switch s.name {
		case "mirage_requests_total":
			ids = append(ids, s.labels["id"])
		case "mirage_endpoint_requests_total":
			if s.labels["id"] == "a" {
				eps = append(eps, s.labels["endpoint"])
			}
		}
	}
	if fmt.Sprint(ids) != "[a b c]" {
		t.Errorf("collector order = %v", ids)
	}
	if fmt.Sprint(eps) != "[GET /a GET /m GET /z]" {
		t.Errorf("endpoint order = %v", eps)
	}
}

func TestWritePrometheusManyEndpoints(t *testing.T) {
	r := NewRegistry()
	c := r.Collector("service:big", model.KindService, "big")
	for i := range MaxEndpoints + 30 {
		c.Observe(Observation{Endpoint: fmt.Sprintf("GET /e/%d", i), Status: 200, Duration: time.Millisecond})
	}
	_, samples := parseProm(t, renderProm(t, r))
	n := 0
	var other float64
	for _, s := range samples {
		if s.name == "mirage_endpoint_requests_total" {
			n++
			if s.labels["endpoint"] == OtherEndpoint {
				other = s.value
			}
		}
	}
	if n != MaxEndpoints+1 || other != 30 {
		t.Errorf("endpoint series = %d (other=%v), want %d (other=30)", n, other, MaxEndpoints+1)
	}
}

type failingWriter struct {
	failAfter int // bytes accepted before failing
	written   int
	calls     int
}

var errBoom = errors.New("boom")

func (w *failingWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.written+len(p) > w.failAfter {
		return 0, errBoom
	}
	w.written += len(p)
	return len(p), nil
}

func TestWritePrometheusWriteErrors(t *testing.T) {
	r := NewRegistry()
	c := r.Collector("service:big", model.KindService, "big")
	for i := range 200 { // enough output to overflow the internal buffer
		c.Observe(Observation{Endpoint: fmt.Sprintf("GET /endpoint/number/%d", i), Status: 200, Duration: time.Millisecond})
	}
	if n := len(renderProm(t, r)); n < 64<<10 {
		t.Fatalf("test output only %d bytes; need more than the buffer", n)
	}
	for _, failAfter := range []int{0, 1000, 40 << 10} {
		w := &failingWriter{failAfter: failAfter}
		if err := r.WritePrometheus(w); !errors.Is(err, errBoom) {
			t.Errorf("failAfter=%d: err = %v, want errBoom", failAfter, err)
		}
		if w.calls > 3 {
			t.Errorf("failAfter=%d: %d write calls after the first failure", failAfter, w.calls)
		}
	}
	if err := r.WritePrometheus(io.Discard); err != nil {
		t.Errorf("io.Discard: %v", err)
	}
}

func TestWritePrometheusConcurrentWithTraffic(t *testing.T) {
	r := NewRegistry()
	c := r.Collector("service:busy", model.KindService, "busy")
	var stop atomic.Bool
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; !stop.Load(); i++ {
				c.InFlight(1)
				c.Observe(Observation{Endpoint: fmt.Sprintf("GET /e/%d", (i+g)%40), Status: 200, Duration: time.Duration(i%500) * time.Microsecond})
				c.InFlight(-1)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			r.Tick(base.Add(time.Duration(i) * time.Second))
		}
	}()
	for range 20 {
		parseProm(t, renderProm(t, r))
	}
	stop.Store(true)
	wg.Wait()
}

func TestPrometheusContentType(t *testing.T) {
	if !strings.Contains(PrometheusContentType, "version=0.0.4") || !strings.HasPrefix(PrometheusContentType, "text/plain") {
		t.Errorf("PrometheusContentType = %q", PrometheusContentType)
	}
}

func BenchmarkWritePrometheus(b *testing.B) {
	r := NewRegistry()
	names := benchEndpoints(50)
	for i := range 10 {
		id := fmt.Sprintf("svc_%d", i)
		c := r.Collector(ServiceCollectorName(id), model.KindService, id)
		for j := range 5000 {
			c.Observe(Observation{Endpoint: names[j%50], Status: 200, Duration: time.Duration(1_000_000 + j*997), BytesOut: 100})
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := r.WritePrometheus(io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRegistryTick(b *testing.B) {
	r := NewRegistry()
	names := benchEndpoints(20)
	var cols []*Collector
	for i := range 10 {
		id := fmt.Sprintf("svc_%d", i)
		cols = append(cols, r.Collector(ServiceCollectorName(id), model.KindService, id))
	}
	ch, cancel := r.Subscribe(1)
	defer cancel()
	_ = ch
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		for _, c := range cols {
			for j := range 100 {
				c.Observe(Observation{Endpoint: names[j%20], Status: 200, Duration: time.Duration(1_000_000 + j*1013)})
			}
		}
		r.Tick(base.Add(time.Duration(i) * time.Second))
	}
}

// TestPromParserRejectsBadExposition checks the checker: each input breaks a
// rule of the text format and must be refused, so the tests above prove
// something when they pass.
func TestPromParserRejectsBadExposition(t *testing.T) {
	const ok = "# HELP m_total Things.\n# TYPE m_total counter\n"
	if _, _, err := parsePromText(ok + `m_total{a="b"} 1` + "\n"); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	tests := []struct {
		name, text string
	}{
		{"no trailing newline", ok + "m_total 1"},
		{"blank line", ok + "\nm_total 1\n"},
		{"HELP twice", ok + "# HELP m_total Again.\nm_total 1\n"},
		{"TYPE twice", ok + "# TYPE m_total counter\nm_total 1\n"},
		{"TYPE missing", "# HELP m_total Things.\nm_total 1\n"},
		{"HELP missing", "# TYPE m_total counter\nm_total 1\n"},
		{"sample before declaration", "m_total 1\n" + ok},
		{"undeclared family", ok + "other_total 1\n"},
		{"duplicate series", ok + `m_total{a="b"} 1` + "\n" + `m_total{a="b"} 2` + "\n"},
		{"duplicate label", ok + `m_total{a="b",a="c"} 1` + "\n"},
		{"bad escape", ok + `m_total{a="b\x"} 1` + "\n"},
		{"unterminated value", ok + `m_total{a="b} 1` + "\n"},
		{"raw newline in label", ok + "m_total{a=\"b\nc\"} 1\n"},
		{"bad label name", ok + `m_total{1a="b"} 1` + "\n"},
		{"bad metric name", "# HELP 1m Things.\n# TYPE 1m counter\n1m 1\n"},
		{"bad type", "# HELP m Things.\n# TYPE m bogus\nm 1\n"},
		{"bad value", ok + "m_total one\n"},
		{"missing value", ok + "m_total\n"},
		{"stray comment", ok + "# something\n"},
		{"invalid UTF-8", ok + "m_total{a=\"\xff\"} 1\n"},
		{"summary without quantile", "# HELP s S.\n# TYPE s summary\ns 1\n"},
		{"_sum on a counter", ok + "m_total_sum 1\n"},
	}
	for _, tt := range tests {
		if _, _, err := parsePromText(tt.text); err == nil {
			t.Errorf("%s: accepted %q", tt.name, tt.text)
		}
	}
	// Summaries with their _sum/_count children are fine.
	good := "# HELP s S.\n# TYPE s summary\n" + `s{quantile="0.5"} NaN` + "\ns_sum 0\ns_count 0\n"
	if _, _, err := parsePromText(good); err != nil {
		t.Errorf("valid summary rejected: %v", err)
	}
}
