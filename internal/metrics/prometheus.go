package metrics

import (
	"bufio"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
)

// PrometheusContentType is the Content-Type for WritePrometheus output.
const PrometheusContentType = "text/plain; version=0.0.4; charset=utf-8"

// promQuantiles are the summary quantiles exposed, ascending, with their
// label values.
var (
	promQuantiles      = [...]float64{0.5, 0.9, 0.95, 0.99}
	promQuantileLabels = [...]string{"0.5", "0.9", "0.95", "0.99"}
)

// promSummary is a latency distribution in the shape of a Prometheus summary,
// in seconds.
type promSummary struct {
	count     int64
	sum       float64
	quantiles [len(promQuantiles)]float64 // NaN when count == 0
}

func newPromSummary(h *Histogram) promSummary {
	n := h.Count()
	s := promSummary{count: n, sum: float64(h.sum.Load()) / 1e9}
	if n <= 0 {
		s.count = 0
		for i := range s.quantiles {
			s.quantiles[i] = math.NaN()
		}
		return s
	}
	var v [len(promQuantiles)]int64
	h.valuesFor(promQuantiles[:], v[:])
	for i, ns := range v {
		s.quantiles[i] = float64(ns) / 1e9
	}
	return s
}

// promEndpoint is one endpoint's exported values.
type promEndpoint struct {
	name     string
	label    string // `endpoint="..."`, already escaped
	requests int64
	errors   int64
	latency  promSummary
}

// promCollector is a point-in-time copy of what WritePrometheus exports for
// one collector, so the text is generated from a stable view.
type promCollector struct {
	labels    string // `kind="service",id="svc_x"`, already escaped
	requests  int64
	errors    int64
	classes   [numClasses]int64
	inflight  int64
	bytesIn   int64
	bytesOut  int64
	latency   promSummary
	endpoints []promEndpoint
}

func (c *Collector) promData() promCollector {
	d := promCollector{
		labels:   `kind="` + escapeLabel(string(c.kind)) + `",id="` + escapeLabel(c.id) + `"`,
		errors:   c.errors.Load(),
		inflight: max(c.inflight.Load(), 0),
		bytesIn:  c.bytesIn.Load(),
		bytesOut: c.bytesOut.Load(),
		latency:  newPromSummary(&c.latency),
	}
	d.requests = d.latency.count
	for i := range c.classes {
		d.classes[i] = c.classes[i].Load()
	}
	for _, r := range c.rankedEndpoints() {
		d.endpoints = append(d.endpoints, promEndpoint{
			name:     r.ep.name,
			label:    `endpoint="` + escapeLabel(r.ep.name) + `"`,
			requests: r.n,
			errors:   r.ep.errors.Load(),
			latency:  newPromSummary(&r.ep.latency),
		})
	}
	// Stable output: endpoints alphabetically (the table is ranked by count).
	slices.SortFunc(d.endpoints, func(a, b promEndpoint) int { return strings.Compare(a.name, b.name) })
	return d
}

// labelEscaper escapes label values per the text format: backslash, double
// quote and line feed.
var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// helpEscaper escapes HELP text: backslash and line feed.
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

// escapeLabel escapes a label value. Invalid UTF-8 (endpoint names come from
// request paths) is replaced with U+FFFD because the format requires UTF-8.
func escapeLabel(s string) string {
	return labelEscaper.Replace(strings.ToValidUTF8(s, "�"))
}

// formatFloat renders a sample value.
func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// promWriter writes exposition text through a buffer. bufio.Writer keeps the
// first error and turns later writes into no-ops, so errors are checked once,
// when flushing.
type promWriter struct {
	bw *bufio.Writer
}

func (p *promWriter) family(name, help, typ string) {
	p.bw.WriteString("# HELP ")
	p.bw.WriteString(name)
	p.bw.WriteByte(' ')
	p.bw.WriteString(helpEscaper.Replace(help))
	p.bw.WriteString("\n# TYPE ")
	p.bw.WriteString(name)
	p.bw.WriteByte(' ')
	p.bw.WriteString(typ)
	p.bw.WriteByte('\n')
}

// sample writes `name{labels[,extra]} value`. labels and extra are
// pre-escaped label lists without braces.
func (p *promWriter) sample(name, labels, extra, value string) {
	p.bw.WriteString(name)
	p.bw.WriteByte('{')
	p.bw.WriteString(labels)
	if extra != "" {
		p.bw.WriteByte(',')
		p.bw.WriteString(extra)
	}
	p.bw.WriteString("} ")
	p.bw.WriteString(value)
	p.bw.WriteByte('\n')
}

func (p *promWriter) intSample(name, labels, extra string, v int64) {
	p.sample(name, labels, extra, strconv.FormatInt(v, 10))
}

// summary writes the quantile, _sum and _count samples of one summary.
func (p *promWriter) summary(name, labels, extra string, s promSummary) {
	for i, q := range s.quantiles {
		lbl := `quantile="` + promQuantileLabels[i] + `"`
		if extra != "" {
			lbl = extra + "," + lbl
		}
		p.sample(name, labels, lbl, formatFloat(q))
	}
	p.sample(name+"_sum", labels, extra, formatFloat(s.sum))
	p.intSample(name+"_count", labels, extra, s.count)
}

// WritePrometheus writes every collector in Prometheus text exposition format
// 0.0.4 (see PrometheusContentType).
//
// Families, all labelled kind (service or replay) and id:
//
//	mirage_requests_total                     counter
//	mirage_errors_total                       counter
//	mirage_responses_total{class}             counter, class 1xx..5xx or err
//	mirage_request_duration_seconds           summary, quantiles 0.5 0.9 0.95 0.99
//	mirage_in_flight_requests                 gauge
//	mirage_bytes_in_total, mirage_bytes_out_total  counters
//
// and, per endpoint (an additional endpoint label, at most MaxEndpoints+1
// values per collector):
//
//	mirage_endpoint_requests_total            counter
//	mirage_endpoint_errors_total              counter
//	mirage_endpoint_request_duration_seconds  summary
//
// The endpoint series live in their own families so that summing
// mirage_requests_total never double counts, and so the service-wide latency
// quantiles, which cannot be rebuilt from per-endpoint ones, stay available.
// Durations are in seconds. A summary with no observations reports NaN
// quantiles, as Prometheus clients do.
//
// Output is sorted (collectors by name, endpoints by name) and each family's
// HELP and TYPE lines appear exactly once, even when the family has no
// samples yet.
func (r *Registry) WritePrometheus(w io.Writer) error {
	cols := r.List()
	data := make([]promCollector, len(cols))
	for i, c := range cols {
		data[i] = c.promData()
	}

	p := &promWriter{bw: bufio.NewWriterSize(w, 32<<10)}

	p.family("mirage_requests_total", "Requests completed.", "counter")
	for i := range data {
		d := &data[i]
		p.intSample("mirage_requests_total", d.labels, "", d.requests)
	}

	p.family("mirage_errors_total", "Requests that counted as errors (5xx, 429 or no response).", "counter")
	for i := range data {
		d := &data[i]
		p.intSample("mirage_errors_total", d.labels, "", d.errors)
	}

	p.family("mirage_responses_total", "Responses by status class; err means no response.", "counter")
	// Classes in natural order: 1xx..5xx, then err.
	order := [numClasses]int{1, 2, 3, 4, 5, classErr}
	for i := range data {
		d := &data[i]
		for _, slot := range order {
			p.intSample("mirage_responses_total", d.labels, `class="`+classNames[slot]+`"`, d.classes[slot])
		}
	}

	p.family("mirage_request_duration_seconds", "Request service time in seconds.", "summary")
	for i := range data {
		d := &data[i]
		p.summary("mirage_request_duration_seconds", d.labels, "", d.latency)
	}

	p.family("mirage_in_flight_requests", "Requests currently in flight.", "gauge")
	for i := range data {
		d := &data[i]
		p.intSample("mirage_in_flight_requests", d.labels, "", d.inflight)
	}

	p.family("mirage_bytes_in_total", "Request bytes received.", "counter")
	for i := range data {
		d := &data[i]
		p.intSample("mirage_bytes_in_total", d.labels, "", d.bytesIn)
	}

	p.family("mirage_bytes_out_total", "Response bytes sent.", "counter")
	for i := range data {
		d := &data[i]
		p.intSample("mirage_bytes_out_total", d.labels, "", d.bytesOut)
	}

	p.family("mirage_endpoint_requests_total", "Requests completed, per endpoint.", "counter")
	for i := range data {
		d := &data[i]
		for _, e := range d.endpoints {
			p.intSample("mirage_endpoint_requests_total", d.labels, e.label, e.requests)
		}
	}

	p.family("mirage_endpoint_errors_total", "Requests that counted as errors, per endpoint.", "counter")
	for i := range data {
		d := &data[i]
		for _, e := range d.endpoints {
			p.intSample("mirage_endpoint_errors_total", d.labels, e.label, e.errors)
		}
	}

	p.family("mirage_endpoint_request_duration_seconds", "Request service time in seconds, per endpoint.", "summary")
	for i := range data {
		d := &data[i]
		for _, e := range d.endpoints {
			p.summary("mirage_endpoint_request_duration_seconds", d.labels, e.label, e.latency)
		}
	}

	return p.bw.Flush()
}
