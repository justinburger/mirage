package virtual

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// syntheticHeaders are the headers of the synthetic 503.
var syntheticHeaders = []hdr{{key: "Content-Type", vals: []string{"application/json"}}}

// dateStamp caches the formatted Date header for one second.
type dateStamp struct {
	sec int64
	s   string
}

func (e *Engine) dateHeader() string {
	now := e.now()
	sec := now.Unix()
	if d := e.date.Load(); d != nil && d.sec == sec {
		return d.s
	}
	s := now.UTC().Format(http.TimeFormat)
	e.date.Store(&dateStamp{sec, s})
	return s
}

// bodyAllowed reports whether a response with this status carries a body.
func bodyAllowed(status int) bool {
	return status >= 200 && status != http.StatusNoContent && status != http.StatusNotModified
}

// responseStatus maps a recorded status to one net/http can write as a final
// response.
func responseStatus(status int) int {
	if status < 200 || status > 999 {
		return http.StatusBadGateway
	}
	return status
}

// Respond writes the matched response, simulating latency.
//
// With recorded latency the headers are written and flushed after the
// simulated time to first byte and the body follows after the rest of the
// duration; with fixed latency everything is written after the delay. If the
// request's context ends during a wait, Respond returns without writing the
// part that was still pending. A nil m writes nothing.
func (e *Engine) Respond(w http.ResponseWriter, r *http.Request, m *Match) {
	if m == nil {
		return
	}
	ctx := r.Context()

	var (
		status int
		hdrs   []hdr
		body   *bodyEntry
		gzOK   bool
	)
	if m.Synthetic {
		status, hdrs, body = http.StatusServiceUnavailable, syntheticHeaders, e.synth
	} else {
		it := m.it
		if it == nil {
			if m.Exchange == nil {
				return
			}
			it = newItem(m.Exchange, false)
		}
		var err error
		if body, err = e.bodyFor(ctx, it); err != nil {
			e.writeLoadError(w, r, err)
			return
		}
		if it.truncated && len(body.raw) > 0 {
			// Serving the prefix with a matching Content-Length would hand the
			// client a response it cannot tell from a complete one. A body that
			// was not kept at all (max_body_bytes -1, a HAR without content)
			// is served as recorded, empty.
			e.writeTruncated(w, r, m, it, len(body.raw))
			return
		}
		status, hdrs, gzOK = responseStatus(it.status), it.hdrs, it.gzip
	}

	payload := body.raw
	useGzip := gzOK && len(payload) > 0 && bodyAllowed(status) && acceptsGzip(r.Header)
	if useGzip {
		payload = body.gzipped(e)
	}

	first, rest := e.latency(m.ttfb, m.dur)
	if !e.wait(ctx, first) {
		return
	}

	e.writeHeader(w.Header(), m, hdrs, status, len(payload), useGzip)
	w.WriteHeader(status)
	if rest > 0 {
		// Let the client see the headers while the "upstream" is still busy.
		_ = http.NewResponseController(w).Flush()
		if !e.wait(ctx, rest) {
			return
		}
	}
	if len(payload) > 0 && bodyAllowed(status) && r.Method != http.MethodHead {
		_, _ = w.Write(payload)
	}
}

// writeHeader copies the recorded headers into dst and adds the ones Mirage
// computes. The values are copied, so nothing a writer does to dst can reach
// the index.
func (e *Engine) writeHeader(dst http.Header, m *Match, hdrs []hdr, status, bodyLen int, gzipped bool) {
	n := 5
	for _, h := range hdrs {
		n += len(h.vals)
	}
	backing := make([]string, 0, n)
	for _, h := range hdrs {
		backing = putValues(dst, backing, h.key, h.vals...)
	}
	backing = putValues(dst, backing, "Date", e.dateHeader())
	if bodyAllowed(status) {
		backing = putValues(dst, backing, "Content-Length", strconv.Itoa(bodyLen))
	}
	if gzipped {
		backing = putValues(dst, backing, "Content-Encoding", "gzip")
	}
	level := m.Level
	if m.Synthetic && level == "" {
		level = LevelExact
	}
	if level != "" {
		backing = putValues(dst, backing, HeaderMatch, level)
	}
	if m.Injected && (m.Synthetic || (m.it != nil && m.it.isErr)) {
		putValues(dst, backing, HeaderInjected, "error")
	}
}

// putValues sets dst[key] to a copy of vals carved out of backing (capacity
// limited, so appending to it reallocates) and returns the rest of backing.
func putValues(dst http.Header, backing []string, key string, vals ...string) []string {
	start := len(backing)
	backing = append(backing, vals...)
	dst[key] = backing[start:len(backing):len(backing)]
	return backing
}

// writeLoadError answers when a lazily loaded body cannot be read from the
// store.
func (e *Engine) writeLoadError(w http.ResponseWriter, r *http.Request, err error) {
	body, _ := json.Marshal(map[string]string{
		"error":  "recorded response unavailable",
		"detail": err.Error(),
	})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set(HeaderError, "store")
	w.WriteHeader(http.StatusInternalServerError)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// truncatedBody is the JSON document answered for a truncated recording.
type truncatedBody struct {
	Error       string `json:"error"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	RecordingID string `json:"recording_id"`
	ExchangeID  int64  `json:"exchange_id,omitempty"`
	// RecordedBytes is the size of the original response body. When the body
	// was cut while being decompressed it is only a lower bound and
	// RecordedBytesExact is false.
	RecordedBytes      int64 `json:"recorded_bytes"`
	RecordedBytesExact bool  `json:"recorded_bytes_exact"`
	KeptBytes          int64 `json:"kept_bytes"` // bytes kept in the recording
}

// truncatedMessage is the error text of a truncated-recording answer.
const truncatedMessage = "only part of this response body was recorded (it was larger than the service's max_body_bytes when it was captured, or it was cut in an imported HAR), so it cannot be served faithfully; raise max_body_bytes and record this request again"

// writeTruncated answers a request whose matched recording holds only part of
// its response body: 502 with a JSON explanation, the match level, and
// "X-Mirage-Error: truncated". No latency is simulated.
func (e *Engine) writeTruncated(w http.ResponseWriter, r *http.Request, m *Match, it *item, kept int) {
	e.truncs.Add(1)
	path := "/"
	if r.URL != nil {
		if path = r.URL.EscapedPath(); path == "" {
			path = "/"
		}
	}
	doc := truncatedBody{
		Error:              truncatedMessage,
		Method:             r.Method,
		Path:               path,
		RecordingID:        e.opts.RecordingID,
		KeptBytes:          int64(kept),
		RecordedBytesExact: true,
	}
	if ex := it.ex; ex != nil {
		doc.ExchangeID = ex.ID
		doc.RecordedBytes = ex.Response.BodySize
		// A body cut while being decoded only knows it was bigger than the
		// cap (the capture records max_body_bytes+1 or the wire size).
		doc.RecordedBytesExact = ex.Response.ContentEncoding == ""
	}
	body, _ := json.Marshal(doc)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set(HeaderError, ErrorTruncated)
	if m.Level != "" {
		h.Set(HeaderMatch, m.Level)
	}
	w.WriteHeader(http.StatusBadGateway)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// latency returns the simulated wait before the response headers and the wait
// between headers and body.
func (e *Engine) latency(ttfb, dur time.Duration) (first, rest time.Duration) {
	c := &e.cfg
	switch c.latMode {
	case model.LatencyRecorded:
		total := max(dur, ttfb, 0)
		first = scale(max(ttfb, 0), c.multiplier)
		total = scale(total, c.multiplier)
		if c.jitter > 0 {
			total += e.jitterOffset()
		}
		total = max(total, 0)
		first = min(first, total)
		return first, total - first
	case model.LatencyFixed:
		d := c.fixed
		if c.jitter > 0 {
			d += e.jitterOffset()
		}
		return max(d, 0), 0
	}
	return 0, 0
}

// jitterOffset returns a uniform duration in [-Jitter, +Jitter].
func (e *Engine) jitterOffset() time.Duration {
	j := int64(e.cfg.jitter)
	if j <= 0 {
		return 0
	}
	span := 2*j + 1
	if span <= 0 { // overflow: absurd jitter
		return 0
	}
	hi := e.rng.uint64() % uint64(span)
	return time.Duration(int64(hi) - j)
}

func scale(d time.Duration, f float64) time.Duration {
	if f == 1 {
		return d
	}
	return time.Duration(float64(d) * f)
}

// wait sleeps for d and reports false if ctx ended first.
func (e *Engine) wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	if e.sleep != nil {
		return e.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// acceptsGzip reports whether the Accept-Encoding header allows gzip: gzip
// named with a non-zero quality, or "*" when gzip is not named.
func acceptsGzip(h http.Header) bool {
	vals := h["Accept-Encoding"]
	if len(vals) == 0 {
		return false
	}
	star, starSeen, gz, gzSeen := false, false, false, false
	for _, v := range vals {
		for v != "" {
			var tok string
			tok, v, _ = strings.Cut(v, ",")
			name, params, _ := strings.Cut(tok, ";")
			name = strings.TrimSpace(name)
			ok := qualityNonZero(params)
			switch {
			case strings.EqualFold(name, "gzip") || strings.EqualFold(name, "x-gzip"):
				gz, gzSeen = ok, true
			case name == "*":
				star, starSeen = ok, true
			}
		}
	}
	if gzSeen {
		return gz
	}
	return starSeen && star
}

// qualityNonZero reports whether the parameter list of an Accept-Encoding
// element has a q value other than 0 (absent means 1).
func qualityNonZero(params string) bool {
	for params != "" {
		var p string
		p, params, _ = strings.Cut(params, ";")
		p = strings.TrimSpace(p)
		if len(p) >= 2 && (p[0] == 'q' || p[0] == 'Q') && p[1] == '=' {
			q, err := strconv.ParseFloat(strings.TrimSpace(p[2:]), 64)
			return err != nil || q > 0
		}
	}
	return true
}

// missBody is the JSON document of a miss response.
type missBody struct {
	Error       string `json:"error"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	RecordingID string `json:"recording_id"`
}

// WriteMiss writes the miss response: Match.MissStatus (default 404) with a
// JSON body explaining the miss and "X-Mirage-Match: miss".
func (e *Engine) WriteMiss(w http.ResponseWriter, r *http.Request) {
	path := "/"
	if r.URL != nil {
		if path = r.URL.EscapedPath(); path == "" {
			path = "/"
		}
	}
	body, _ := json.Marshal(missBody{
		Error:       "no recorded response matches this request",
		Method:      r.Method,
		Path:        path,
		RecordingID: e.opts.RecordingID,
	})
	status := e.cfg.missStatus
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set(HeaderMatch, LevelMiss)
	allowed := bodyAllowed(status)
	if allowed {
		h.Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(status)
	if allowed && r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}
