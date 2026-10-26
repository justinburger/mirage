package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/fault"
	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
	"github.com/justinburger/mirage/internal/store"
)

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---- fake virtualizer --------------------------------------------------

// fakeVirt is a minimal Virtualizer: exact method + request URI lookup.
type fakeVirt struct {
	mu      sync.RWMutex
	entries map[string]*model.Exchange
	added   []*model.Exchange
	bodies  [][]byte // request bodies passed to Serve

	hits, misses atomic.Int64
	closed       atomic.Bool
	missStatus   int
}

func vkey(method, uri string) string { return method + " " + uri }

func newFakeVirt() *fakeVirt {
	return &fakeVirt{entries: map[string]*model.Exchange{}, missStatus: http.StatusNotFound}
}

func (f *fakeVirt) Serve(w http.ResponseWriter, r *http.Request, body []byte) bool {
	f.mu.Lock()
	if len(body) > 0 {
		f.bodies = append(f.bodies, append([]byte(nil), body...))
	}
	f.mu.Unlock()
	f.mu.RLock()
	ex, ok := f.entries[vkey(r.Method, r.URL.RequestURI())]
	f.mu.RUnlock()
	if !ok {
		f.misses.Add(1)
		return false
	}
	f.hits.Add(1)
	h := w.Header()
	for k, vv := range ex.Response.Header {
		if k == "Content-Length" || k == "Content-Encoding" {
			continue
		}
		h[k] = vv
	}
	h.Set("X-Mirage-Match", "exact")
	w.WriteHeader(ex.Response.Status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(ex.Response.Body)
	}
	return true
}

func (f *fakeVirt) WriteMiss(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Mirage-Match", "miss")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.missStatus)
	_, _ = io.WriteString(w, `{"error":"no recorded response matches this request"}`)
}

func (f *fakeVirt) Add(ex *model.Exchange) {
	if f.closed.Load() {
		return
	}
	f.mu.Lock()
	f.entries[vkey(ex.Request.Method, ex.Request.URI())] = ex
	f.added = append(f.added, ex)
	f.mu.Unlock()
}

func (f *fakeVirt) Len() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.entries)
}

func (f *fakeVirt) Stats() model.VirtualStats {
	return model.VirtualStats{Indexed: int64(f.Len()), Hits: f.hits.Load(), Misses: f.misses.Load()}
}

func (f *fakeVirt) Close() error { f.closed.Store(true); return nil }

func (f *fakeVirt) addedCount() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.added)
}

func (f *fakeVirt) seenBodies() [][]byte {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return append([][]byte(nil), f.bodies...)
}

// virtFactory builds fakeVirts from the service's recording, like the real
// engine does, and remembers them.
type virtFactory struct {
	st store.Store

	mu    sync.Mutex
	built []*fakeVirt
	fail  error
}

func (vf *virtFactory) build(ctx context.Context, svc model.Service, n *pathnorm.Normalizer) (Virtualizer, error) {
	vf.mu.Lock()
	fail := vf.fail
	vf.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	f := newFakeVirt()
	err := vf.st.IterateExchanges(ctx, svc.RecordingID, model.IterOptions{RequestBodies: true, ResponseBodies: true}, func(ex *model.Exchange) error {
		if ex.Error == "" {
			f.entries[vkey(ex.Request.Method, ex.Request.URI())] = ex
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	vf.mu.Lock()
	vf.built = append(vf.built, f)
	vf.mu.Unlock()
	return f, nil
}

func (vf *virtFactory) count() int {
	vf.mu.Lock()
	defer vf.mu.Unlock()
	return len(vf.built)
}

func (vf *virtFactory) last() *fakeVirt {
	vf.mu.Lock()
	defer vf.mu.Unlock()
	if len(vf.built) == 0 {
		return nil
	}
	return vf.built[len(vf.built)-1]
}

func (vf *virtFactory) setFail(err error) {
	vf.mu.Lock()
	vf.fail = err
	vf.mu.Unlock()
}

// ---- test environment --------------------------------------------------

type env struct {
	t     testing.TB
	st    *store.SQLite
	reg   *metrics.Registry
	inj   *fault.Injector
	virts *virtFactory
	mgr   *Manager
	seq   atomic.Int64

	lenient atomic.Bool // the test expects Shutdown to fail
}

// shutdownFailureExpected tells the cleanup not to treat a Shutdown error as a
// test failure.
func (e *env) shutdownFailureExpected() { e.lenient.Store(true) }

// newEnv builds a manager over a private in-memory store with fast capture
// flushing. tweak may adjust the dependencies.
func newEnv(t testing.TB, tweak ...func(*Deps)) *env {
	t.Helper()
	st, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	e := &env{t: t, st: st, reg: metrics.NewRegistry(), inj: fault.NewInjector()}
	e.virts = &virtFactory{st: st}
	deps := Deps{
		Store:                st,
		Metrics:              e.reg,
		Faults:               e.inj,
		Virtualizers:         e.virts.build,
		CaptureFlushInterval: 5 * time.Millisecond,
		CaptureWorkers:       2,
	}
	for _, f := range tweak {
		f(&deps)
	}
	e.mgr = NewManager(deps)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := e.mgr.Shutdown(ctx); err != nil && !e.lenient.Load() {
			t.Errorf("Shutdown: %v", err)
		}
		_ = st.Close()
	})
	return e
}

// recording creates a recording and returns its ID.
func (e *env) recording() string {
	e.t.Helper()
	rec, err := e.st.CreateRecording(context.Background(), model.Recording{Name: fmt.Sprintf("rec-%d", e.seq.Add(1)), Source: "proxy"})
	if err != nil {
		e.t.Fatalf("CreateRecording: %v", err)
	}
	return rec.ID
}

// service returns an enabled service on 127.0.0.1:0 with its own recording.
func (e *env) service(upstream string, mode model.Mode, mods ...func(*model.Service)) model.Service {
	e.t.Helper()
	n := e.seq.Add(1)
	svc := model.Service{
		ID:         fmt.Sprintf("svc_t%d", n),
		Name:       fmt.Sprintf("t%d", n),
		ListenAddr: "127.0.0.1:0",
		Upstream:   upstream,
		Mode:       mode,
		Enabled:    true,
	}
	if mode.UsesRecording() {
		svc.RecordingID = e.recording()
	}
	for _, m := range mods {
		m(&svc)
	}
	return svc
}

// start applies svc and returns its base URL.
func (e *env) start(svc model.Service) string {
	e.t.Helper()
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		e.t.Fatalf("Apply: %v", err)
	}
	return e.url(svc.ID)
}

// url is the base URL of a running service.
func (e *env) url(id string) string {
	e.t.Helper()
	s := e.mgr.Status(id)
	if !s.Running || s.Addr == "" {
		e.t.Fatalf("service %s is not running: %+v", id, s)
	}
	return "http://" + strings.Replace(s.Addr, "[::]", "127.0.0.1", 1)
}

// captured returns the exchanges stored in a recording, in Seq order.
func (e *env) captured(recID string) []*model.Exchange {
	e.t.Helper()
	var out []*model.Exchange
	err := e.st.IterateExchanges(context.Background(), recID, model.IterOptions{RequestBodies: true, ResponseBodies: true}, func(ex *model.Exchange) error {
		out = append(out, ex.Clone())
		return nil
	})
	if err != nil {
		e.t.Fatalf("IterateExchanges: %v", err)
	}
	return out
}

// waitStored waits until the recording holds n exchanges and returns them.
func (e *env) waitStored(recID string, n int) []*model.Exchange {
	e.t.Helper()
	var got []*model.Exchange
	waitFor(e.t, fmt.Sprintf("%d stored exchanges", n), func() bool {
		got = e.captured(recID)
		return len(got) >= n
	})
	return got
}

// counter is the service's metrics snapshot.
func (e *env) snapshot(id string) model.StatsSnapshot {
	e.t.Helper()
	c, ok := e.reg.Get(metrics.ServiceCollectorName(id))
	if !ok {
		e.t.Fatalf("no collector for %s", id)
	}
	return c.Snapshot()
}

// waitIdle waits until no request is in flight on the service.
func (e *env) waitIdle(id string) model.StatsSnapshot {
	e.t.Helper()
	var snap model.StatsSnapshot
	waitFor(e.t, "in-flight requests to finish", func() bool {
		snap = e.snapshot(id)
		return snap.InFlight == 0
	})
	return snap
}

// ---- HTTP helpers ------------------------------------------------------

// newClient returns a client that does not decode or add encodings itself.
func newClient(t testing.TB) *http.Client {
	t.Helper()
	tr := &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 64}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 20 * time.Second}
}

// do sends a request and returns status, headers and the whole body.
func do(t testing.TB, c *http.Client, method, url string, body io.Reader, hdr ...string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading %s %s: %v", method, url, err)
	}
	return res.StatusCode, res.Header, b
}

// echoUpstream answers every request with a JSON description of it and
// counts the requests it served.
type echoUpstream struct {
	*httptest.Server
	hits atomic.Int64

	mu   sync.Mutex
	last echoed
}

type echoed struct {
	Method string      `json:"method"`
	URI    string      `json:"uri"`
	Host   string      `json:"host"`
	Header http.Header `json:"header"`
	Body   string      `json:"body"`
}

func newEcho(t testing.TB) *echoUpstream {
	t.Helper()
	u := &echoUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		e := echoed{Method: r.Method, URI: r.RequestURI, Host: r.Host, Header: r.Header.Clone(), Body: string(b)}
		u.mu.Lock()
		u.last = e
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "echo")
		_ = json.NewEncoder(w).Encode(e)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *echoUpstream) lastRequest() echoed {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last
}

// mustJSON decodes b into v.
func mustJSON(t testing.TB, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decoding %q: %v", b, err)
	}
}

// failingStore wraps a store and makes AppendExchanges slow, blocked or
// failing, so the capture pipeline can be pushed to its limits.
type failingStore struct {
	store.Store
	gate    chan struct{} // when non-nil, AppendExchanges waits for it (or ctx)
	err     error
	appends atomic.Int64
}

func (f *failingStore) AppendExchanges(ctx context.Context, recID string, exs []*model.Exchange) error {
	f.appends.Add(1)
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.err != nil {
		return f.err
	}
	return f.Store.AppendExchanges(ctx, recID, exs)
}

var errTest = errors.New("test failure")

func bodyOf(b []byte) string { return string(bytes.TrimSpace(b)) }

// tlsInsecure is a client TLS config that trusts the self-signed listeners.
func tlsInsecure() *tls.Config { return &tls.Config{InsecureSkipVerify: true} }
