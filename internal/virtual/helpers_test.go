package virtual

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
)

var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// exOpt customises an exchange built by mkEx.
type exOpt func(*model.Exchange)

// mkEx builds a recorded exchange. Seq and ID are seq, and StartedAt is seq
// seconds after base. target is a path with an optional query.
func mkEx(seq int64, method, target string, status int, respBody string, opts ...exOpt) *model.Exchange {
	u, err := url.Parse(target)
	if err != nil {
		panic(err)
	}
	ex := &model.Exchange{
		ID:          seq,
		RecordingID: "rec_t",
		Seq:         seq,
		StartedAt:   base.Add(time.Duration(seq) * time.Second),
		Request: model.Request{
			Method:   method,
			Path:     u.EscapedPath(),
			RawQuery: u.RawQuery,
			Header:   http.Header{},
		},
		Response: model.Response{
			Status:   status,
			Header:   http.Header{"Content-Type": {"application/json"}},
			Body:     []byte(respBody),
			BodySize: int64(len(respBody)),
		},
	}
	ex.Endpoint = pathnorm.Default.Endpoint(method, ex.Request.Path)
	for _, o := range opts {
		o(ex)
	}
	return ex
}

func withReqBody(s string) exOpt {
	return func(e *model.Exchange) { e.Request.Body, e.Request.BodySize = []byte(s), int64(len(s)) }
}

func withReqHeader(k string, v ...string) exOpt {
	return func(e *model.Exchange) { e.Request.Header[http.CanonicalHeaderKey(k)] = v }
}

func withRespHeader(k string, v ...string) exOpt {
	return func(e *model.Exchange) { e.Response.Header[k] = v }
}

func withLatency(ttfb, dur time.Duration) exOpt {
	return func(e *model.Exchange) { e.TTFB, e.Duration = model.Duration(ttfb), model.Duration(dur) }
}

func withError(msg string) exOpt {
	return func(e *model.Exchange) { e.Error = msg }
}

func withGzip() exOpt {
	return func(e *model.Exchange) { e.Response.ContentEncoding = "gzip" }
}

func withStart(t time.Time) exOpt {
	return func(e *model.Exchange) { e.StartedAt = t }
}

// noLatency returns opts with latency simulation off unless a mode is set.
func noLatency(o Options) Options {
	if o.Latency.Mode == "" {
		o.Latency.Mode = model.LatencyNone
	}
	return o
}

// newTestEngine builds an engine (no store) holding exs. Latency is off unless
// opts set a mode.
func newTestEngine(t testing.TB, opts Options, exs ...*model.Exchange) *Engine {
	t.Helper()
	e := NewEngine(nil, noLatency(opts))
	for _, ex := range exs {
		e.Add(ex)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// mkReq builds a server-style request. hdr is key, value, key, value, ...
func mkReq(method, target string, hdr ...string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Add(hdr[i], hdr[i+1])
	}
	return r
}

// lookup runs Lookup for a request and reports the level ("miss" on a miss)
// and the recorded body of the match.
func lookup(e *Engine, method, target, body string, hdr ...string) (level, respBody string) {
	m := e.Lookup(mkReq(method, target, hdr...), []byte(body))
	if m == nil {
		return LevelMiss, ""
	}
	if m.Exchange == nil {
		return m.Level, "<synthetic>"
	}
	return m.Level, string(m.Exchange.Response.Body)
}

// serve runs Serve against a recorder.
func serve(e *Engine, r *http.Request, body string) (*httptest.ResponseRecorder, bool) {
	rec := httptest.NewRecorder()
	ok := e.Serve(rec, r, []byte(body))
	return rec, ok
}

func bodyOf(rec *httptest.ResponseRecorder) string {
	b, _ := io.ReadAll(rec.Result().Body)
	return string(b)
}

// fakeClock records simulated waits instead of sleeping.
type fakeClock struct {
	mu    sync.Mutex
	waits []time.Duration
	log   *eventLog // optional: waits are logged here too
}

func (f *fakeClock) sleep(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	f.mu.Lock()
	f.waits = append(f.waits, d)
	f.mu.Unlock()
	if f.log != nil {
		f.log.add("wait " + d.String())
	}
	return true
}

func (f *fakeClock) get() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.waits...)
}

// install makes e use the fake clock for waits.
func (f *fakeClock) install(e *Engine) { e.sleep = f.sleep }

// eventLog is an ordered list of events shared by a fake clock and a writer.
type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (l *eventLog) add(s string) {
	l.mu.Lock()
	l.ev = append(l.ev, s)
	l.mu.Unlock()
}

func (l *eventLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.ev, ",")
}

// logWriter is a ResponseWriter that logs WriteHeader, Flush and Write.
type logWriter struct {
	h       http.Header
	log     *eventLog
	code    int
	body    bytes.Buffer
	onFlush func() // optional, called after the flush is logged
}

func newLogWriter(log *eventLog) *logWriter { return &logWriter{h: http.Header{}, log: log} }

func (w *logWriter) Header() http.Header { return w.h }
func (w *logWriter) WriteHeader(c int) {
	w.code = c
	w.log.add("header")
}
func (w *logWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.WriteHeader(200)
	}
	w.log.add("body")
	return w.body.Write(b)
}
func (w *logWriter) Flush() {
	w.log.add("flush")
	if w.onFlush != nil {
		w.onFlush()
	}
}

// virtualizer is a copy of proxy.Virtualizer (the proxy package is built
// separately and must not be imported here): *Engine has to satisfy it
// exactly.
type virtualizer interface {
	Serve(w http.ResponseWriter, r *http.Request, body []byte) bool
	WriteMiss(w http.ResponseWriter, r *http.Request)
	Add(ex *model.Exchange)
	Len() int
	Stats() model.VirtualStats
	Close() error
}

var _ virtualizer = (*Engine)(nil)

// gzipForTest compresses s.
func gzipForTest(t testing.TB, s string) []byte {
	t.Helper()
	e := NewEngine(nil, Options{})
	return e.compress([]byte(s))
}

func httptestRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

// newTestServer serves e over a real listener (misses get the miss response)
// and returns its base URL.
func newTestServer(t testing.TB, e *Engine) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !e.Serve(w, r, body) {
			e.WriteMiss(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func readAll(t testing.TB, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
