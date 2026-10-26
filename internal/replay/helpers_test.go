package replay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/store"
)

// epoch is the start of every test recording.
var epoch = time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)

// rx describes one recorded exchange for seedRecording.
type rx struct {
	at       time.Duration // StartedAt offset from epoch
	method   string        // default GET
	path     string        // default /items/1
	query    string
	status   int // default 200
	body     string
	header   map[string]string
	endpoint string // default "<METHOD> <path>"
	encoding string // Request.ContentEncoding
	errMsg   string
}

func (r rx) exchange() *model.Exchange {
	if r.method == "" {
		r.method = http.MethodGet
	}
	if r.path == "" {
		r.path = "/items/1"
	}
	if r.status == 0 {
		r.status = 200
	}
	if r.endpoint == "" {
		r.endpoint = r.method + " " + r.path
	}
	h := http.Header{}
	for k, v := range r.header {
		h.Set(k, v)
	}
	var body []byte
	if r.body != "" {
		body = []byte(r.body)
	}
	return &model.Exchange{
		StartedAt: epoch.Add(r.at),
		Endpoint:  r.endpoint,
		Error:     r.errMsg,
		Request: model.Request{
			Method: r.method, Path: r.path, RawQuery: r.query, Header: h,
			Body: body, BodySize: int64(len(body)), ContentEncoding: r.encoding,
		},
		Response: model.Response{Status: r.status, Header: http.Header{}},
	}
}

func openStore(t testing.TB) store.Store {
	t.Helper()
	st, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedRecording stores a recording of the given exchanges, appended in the
// order given (which is Seq order).
func seedRecording(t testing.TB, st store.Store, exs ...rx) string {
	t.Helper()
	ctx := context.Background()
	rec, err := st.CreateRecording(ctx, model.Recording{Name: "test", Source: "proxy"})
	if err != nil {
		t.Fatalf("CreateRecording: %v", err)
	}
	batch := make([]*model.Exchange, len(exs))
	for i, e := range exs {
		batch[i] = e.exchange()
	}
	if err := st.AppendExchanges(ctx, rec.ID, batch); err != nil {
		t.Fatalf("AppendExchanges: %v", err)
	}
	return rec.ID
}

// seen is one request as the test target received it.
type seen struct {
	method, uri, host string
	header            http.Header
	body              []byte
	at                time.Time
}

// recorder is a test target that logs every request it gets.
type recorder struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []seen
}

// newRecorder starts a target. handler (may be nil: 200 "ok") produces the
// response after the request has been logged.
func newRecorder(t testing.TB, handler http.HandlerFunc) *recorder {
	t.Helper()
	rc := &recorder{}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.reqs = append(rc.reqs, seen{method: r.Method, uri: r.RequestURI, host: r.Host, header: r.Header.Clone(), body: body, at: time.Now()})
		rc.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *recorder) requests() []seen {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]seen(nil), rc.reqs...)
}

func (rc *recorder) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.reqs)
}

// baseCfg is a minimal valid config for a recording and target.
func baseCfg(recID, target string) model.ReplayConfig {
	return model.ReplayConfig{RecordingID: recID, Target: target, Timeout: model.Duration(5 * time.Second)}
}

// runCfg runs cfg with a private, self-ticking collector.
func runCfg(t testing.TB, st store.Store, cfg model.ReplayConfig) (model.ReplaySummary, []model.SeriesPoint, error) {
	t.Helper()
	return Run(context.Background(), st, cfg, RunOptions{
		Collector:     metrics.NewCollector("replay:test", model.KindReplay, "test"),
		TickCollector: true,
	})
}

// mustRun is runCfg that fails the test on error.
func mustRun(t testing.TB, st store.Store, cfg model.ReplayConfig) (model.ReplaySummary, []model.SeriesPoint) {
	t.Helper()
	sum, series, err := runCfg(t, st, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return sum, series
}

// uris returns the request URIs the target saw, in arrival order.
func (rc *recorder) uris() []string {
	var out []string
	for _, r := range rc.requests() {
		out = append(out, r.uri)
	}
	return out
}

// itemsAt builds prepared items at the given offsets (milliseconds); the
// items listed in errs (by index) are recorded errors.
func itemsAt(offsetsMs []int, errs ...int) *prepared {
	items := make([]item, len(offsetsMs))
	for i, ms := range offsetsMs {
		items[i] = item{off: time.Duration(ms) * time.Millisecond, uri: "/i", method: "GET", endpoint: "GET /i", status: 200}
	}
	for _, e := range errs {
		items[e].flags |= flagErr
		items[e].status = 500
	}
	span := items[len(items)-1].off
	return &prepared{items: items, span: span, period: passPeriod(len(items), span)}
}

// drain pulls a generator dry the way the scheduler does (honouring Duration
// and MaxRequests), up to hardCap jobs.
func drain(g generator, cfg model.ReplayConfig, hardCap int) []job {
	var out []job
	limit := cfg.Duration.D()
	for len(out) < hardCap {
		if cfg.MaxRequests > 0 && int64(len(out)) >= cfg.MaxRequests {
			break
		}
		j, ok := g.next()
		if !ok || (limit > 0 && j.due >= limit) {
			break
		}
		out = append(out, j)
	}
	return out
}

func dues(js []job) []time.Duration {
	out := make([]time.Duration, len(js))
	for i, j := range js {
		out[i] = j.due
	}
	return out
}

func ms(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func newTestCollector() *metrics.Collector {
	return metrics.NewCollector("replay:test", model.KindReplay, "test")
}
