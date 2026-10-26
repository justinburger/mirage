package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/justinburger/mirage/internal/fault"
	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
	"github.com/justinburger/mirage/internal/store"
)

var errBoom = errors.New("boom: secret internal detail")

// ---------------------------------------------------------------- fakes

// fakeServices is a ServiceController that records what it is asked to do.
type fakeServices struct {
	mu       sync.Mutex
	applied  map[string]model.Service // last applied service per ID
	status   map[string]model.ServiceStatus
	applies  []model.Service // every Apply, in order
	removed  []string
	reloaded []string
	profiles []string

	applyErr   error // returned by Apply
	silentFail bool  // when applyErr is set, leave status.Error empty
	// keepOnFail makes a failed Apply behave like proxy.Manager: the status
	// keeps what the previous listener was (mode, running) and only gains the
	// error; a service that was not running stays not running.
	keepOnFail bool
	removeErr  error
	reloadErr  error
	profileErr error
}

func newFakeServices() *fakeServices {
	return &fakeServices{applied: map[string]model.Service{}, status: map[string]model.ServiceStatus{}}
}

func (f *fakeServices) Apply(_ context.Context, svc model.Service) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applies = append(f.applies, svc)
	f.applied[svc.ID] = svc
	st := model.ServiceStatus{ServiceID: svc.ID, Mode: svc.Mode, Running: svc.Enabled, Addr: svc.ListenAddr}
	if svc.Enabled {
		now := time.Now()
		st.StartedAt = &now
	}
	if f.applyErr != nil {
		st.Running = false
		if prev, ok := f.status[svc.ID]; ok && f.keepOnFail {
			st = prev
		}
		if !f.silentFail {
			st.Error = f.applyErr.Error()
		}
		f.status[svc.ID] = st
		return f.applyErr
	}
	f.status[svc.ID] = st
	return nil
}

func (f *fakeServices) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	if f.removeErr != nil {
		return f.removeErr
	}
	delete(f.applied, id)
	delete(f.status, id)
	return nil
}

func (f *fakeServices) Reload(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloaded = append(f.reloaded, id)
	return f.reloadErr
}

func (f *fakeServices) ProfileChanged(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profiles = append(f.profiles, id)
	return f.profileErr
}

func (f *fakeServices) Status(id string) model.ServiceStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status[id]
}

func (f *fakeServices) Statuses() []model.ServiceStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.ServiceStatus, 0, len(f.status))
	for _, st := range f.status {
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b model.ServiceStatus) int { return strings.Compare(a.ServiceID, b.ServiceID) })
	return out
}

func (f *fakeServices) setStatus(st model.ServiceStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[st.ServiceID] = st
}

func (f *fakeServices) applyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.applies)
}

func (f *fakeServices) lastApplied(id string) (model.Service, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	svc, ok := f.applied[id]
	return svc, ok
}

// fakeReplays is a ReplayController over the real store: jobs persist, running
// jobs live in memory, and tests move them to a final state with finish.
type fakeReplays struct {
	st store.Store

	mu      sync.Mutex
	running map[string]model.ReplayJob
	series  map[string][]model.SeriesPoint
	created []model.ReplayConfig // configs given to Create, as received
	stopped []string

	startErr  error
	createErr error
}

func newFakeReplays(st store.Store) *fakeReplays {
	return &fakeReplays{st: st, running: map[string]model.ReplayJob{}, series: map[string][]model.SeriesPoint{}}
}

func (f *fakeReplays) Create(ctx context.Context, name string, cfg model.ReplayConfig) (model.ReplayJob, error) {
	f.mu.Lock()
	f.created = append(f.created, cfg)
	err := f.createErr
	f.mu.Unlock()
	if err != nil {
		return model.ReplayJob{}, err
	}
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return model.ReplayJob{}, err
	}
	if _, err := f.st.GetRecording(ctx, cfg.RecordingID); err != nil {
		return model.ReplayJob{}, fmt.Errorf("replay: recording: %w", err)
	}
	if cfg.ProfileID != "" {
		if _, err := f.st.GetProfile(ctx, cfg.ProfileID); err != nil {
			return model.ReplayJob{}, fmt.Errorf("replay: profile: %w", err)
		}
	}
	if cfg.Seed == 0 {
		cfg.Seed = 4242
	}
	return f.st.PutReplayJob(ctx, model.ReplayJob{Name: name, Config: cfg, Status: model.ReplayPending})
}

func (f *fakeReplays) Start(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	if _, ok := f.running[id]; ok {
		return fmt.Errorf("replay: job %s is running: %w", id, model.ErrConflict)
	}
	job, err := f.st.GetReplayJob(ctx, id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	job.Status, job.StartedAt, job.EndedAt = model.ReplayRunning, &now, nil
	job.Summary = &model.ReplaySummary{Planned: 100, Sent: 10}
	f.running[id] = job
	_, err = f.st.PutReplayJob(ctx, job)
	return err
}

func (f *fakeReplays) Stop(id string) error {
	f.mu.Lock()
	f.stopped = append(f.stopped, id)
	f.mu.Unlock()
	f.finish(id, model.ReplayStopped, nil)
	return nil
}

// finish moves a running job to a final state, as the manager does when a run ends.
func (f *fakeReplays) finish(id string, status model.ReplayStatus, summary *model.ReplaySummary) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.running[id]
	if !ok {
		return
	}
	delete(f.running, id)
	now := time.Now().UTC()
	job.Status, job.EndedAt = status, &now
	if summary != nil {
		job.Summary = summary
	}
	job.Series = f.series[id]
	_, _ = f.st.PutReplayJob(context.Background(), job)
}

func (f *fakeReplays) Get(ctx context.Context, id string) (model.ReplayJob, error) {
	f.mu.Lock()
	job, ok := f.running[id]
	f.mu.Unlock()
	if ok {
		return job, nil
	}
	return f.st.GetReplayJob(ctx, id)
}

func (f *fakeReplays) List(ctx context.Context) ([]model.ReplayJob, error) {
	jobs, err := f.st.ListReplayJobs(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, j := range jobs {
		if live, ok := f.running[j.ID]; ok {
			jobs[i] = live
		}
	}
	return jobs, nil
}

func (f *fakeReplays) Delete(ctx context.Context, id string) error {
	f.mu.Lock()
	delete(f.running, id)
	f.mu.Unlock()
	return f.st.DeleteReplayJob(ctx, id)
}

func (f *fakeReplays) Series(ctx context.Context, id string, since int64) ([]model.SeriesPoint, error) {
	if _, err := f.Get(ctx, id); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.SeriesPoint
	for _, p := range f.series[id] {
		if p.T >= since {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeReplays) Running() []model.ReplayJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.ReplayJob, 0, len(f.running))
	for _, j := range f.running {
		out = append(out, j)
	}
	slices.SortFunc(out, func(a, b model.ReplayJob) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func (f *fakeReplays) setSeries(id string, pts []model.SeriesPoint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.series[id] = pts
}

// ----------------------------------------------------------- environment

// testUI is the file system the tests serve as the web UI.
func testUI() fstest.MapFS {
	return fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><title>Mirage</title><script type=module src=/assets/app.js></script>")},
		"assets/app.js":        {Data: []byte("console.log('mirage');")},
		"assets/app.css":       {Data: []byte("body{margin:0}")},
		"assets/favicon.svg":   {Data: []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")},
		"assets/js/router.js":  {Data: []byte("export const r = 1;")},
		"assets/data.unknownx": {Data: []byte("zzz")},
	}
}

// env bundles a server under test with everything the tests poke at.
type env struct {
	t     testing.TB
	srv   *Server
	ts    *httptest.Server
	st    *store.SQLite
	reg   *metrics.Registry
	inj   *fault.Injector
	svcs  *fakeServices
	rpls  *fakeReplays
	token string
}

// newEnv builds a server over a private in-memory store and fakes. tweak may
// change the dependencies before the server is built.
func newEnv(t testing.TB, tweak ...func(*Deps)) *env {
	t.Helper()
	st, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := &env{
		t: t, st: st,
		reg:  metrics.NewRegistry(),
		inj:  fault.NewInjector(),
		svcs: newFakeServices(),
		rpls: newFakeReplays(st),
	}
	deps := Deps{
		Store: st, Services: e.svcs, Replays: e.rpls, Metrics: e.reg, Faults: e.inj,
		UI: testUI(), Version: "v0.0.0-test", DataPath: ":memory:",
	}
	for _, fn := range tweak {
		fn(&deps)
	}
	e.token = deps.AdminToken
	e.srv = New(deps)
	e.ts = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.ts.Close)
	return e
}

// run starts the broadcaster and stops it, waiting for it, at the end of the test.
func (e *env) run() context.CancelFunc {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.srv.Run(ctx)
	e.t.Cleanup(func() {
		cancel()
		wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer wcancel()
		if !e.srv.WaitContext(wctx) {
			e.t.Error("broadcaster did not stop after its context was cancelled")
		}
	})
	return cancel
}

// request is one HTTP call to the test server.
type request struct {
	method, path string
	body         any               // []byte/string sent as is, io.Reader streamed, anything else JSON-encoded
	header       map[string]string // extra headers
	host         string            // Host header, when not the test server's own
	noAuth       bool
}

// do performs the request and returns the response with its body read.
func (e *env) do(method, path string, body any) (*http.Response, []byte) {
	e.t.Helper()
	return e.send(request{method: method, path: path, body: body})
}

func (e *env) send(rq request) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	ctype := ""
	switch b := rq.body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	case io.Reader:
		rd = b
	default:
		data, err := json.Marshal(b)
		if err != nil {
			e.t.Fatalf("marshal request body: %v", err)
		}
		rd, ctype = bytes.NewReader(data), "application/json"
	}
	req, err := http.NewRequest(rq.method, e.ts.URL+rq.path, rd)
	if err != nil {
		e.t.Fatalf("NewRequest: %v", err)
	}
	if rq.host != "" {
		req.Host = rq.host
	}
	// State-changing requests with a body must declare JSON (or multipart for
	// the HAR import); raw bodies default to JSON unless a test says otherwise.
	if ctype == "" && rd != nil {
		ctype = "application/json"
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if e.token != "" && !rq.noAuth {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	for k, v := range rq.header {
		req.Header.Set(k, v)
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", rq.method, rq.path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("read %s %s: %v", rq.method, rq.path, err)
	}
	return resp, data
}

// call performs a request, checks the status and decodes the JSON response
// into out (when not nil).
func (e *env) call(method, path string, in any, wantStatus int, out any) {
	e.t.Helper()
	resp, body := e.do(method, path, in)
	if resp.StatusCode != wantStatus {
		e.t.Fatalf("%s %s = %d, want %d; body: %s", method, path, resp.StatusCode, wantStatus, body)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, body, err)
		}
	}
}

// errorOf performs a request that must fail with wantStatus and returns the
// decoded error body.
func (e *env) errorOf(method, path string, in any, wantStatus int) errorBody {
	e.t.Helper()
	resp, body := e.do(method, path, in)
	if resp.StatusCode != wantStatus {
		e.t.Fatalf("%s %s = %d, want %d; body: %s", method, path, resp.StatusCode, wantStatus, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		e.t.Fatalf("%s %s: error Content-Type = %q", method, path, ct)
	}
	var eb errorBody
	if err := json.Unmarshal(body, &eb); err != nil || eb.Error == "" {
		e.t.Fatalf("%s %s: not an error body: %q (%v)", method, path, body, err)
	}
	return eb
}

// ---------------------------------------------------------------- seeding

// validService returns a service that passes validation; mutate it as needed.
func validService(name, addr string) map[string]any {
	return map[string]any{"name": name, "listen_addr": addr, "upstream": "http://upstream.example.com:8080"}
}

// seedService stores a service directly (bypassing the API) and registers
// nothing with the controller.
func (e *env) seedService(svc model.Service) model.Service {
	e.t.Helper()
	if svc.Name == "" {
		svc.Name = "svc-" + fmt.Sprint(time.Now().UnixNano()%1_000_000)
	}
	if svc.ListenAddr == "" {
		svc.ListenAddr = ":0"
	}
	if svc.Upstream == "" {
		svc.Upstream = "http://upstream.example.com"
	}
	stored, err := e.st.PutService(context.Background(), svc.WithDefaults())
	if err != nil {
		e.t.Fatalf("PutService: %v", err)
	}
	return stored
}

var baseTime = time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)

// mkExchange builds exchange i of a test recording (PII included).
func mkExchange(i int) *model.Exchange {
	reqBody := fmt.Sprintf(`{"customer_id":"cust-%d","email":"jane%d@acme.io","password":"Pa$$w0rd!x"}`, i, i)
	status := 200
	if i%10 == 0 {
		status = 503
	}
	respBody := fmt.Sprintf(`{"id":%d,"email":"jane%d@acme.io"}`, i, i)
	path := fmt.Sprintf("/users/%d/orders", 1000+i)
	return &model.Exchange{
		ServiceID: "svc_test",
		StartedAt: baseTime.Add(time.Duration(i) * 100 * time.Millisecond),
		Duration:  model.Duration(time.Duration(i%50+1) * time.Millisecond),
		TTFB:      model.Duration(time.Duration(i%50+1) * time.Millisecond / 2),
		Endpoint:  pathnorm.Default.Endpoint("POST", path),
		Request: model.Request{
			Method: "POST", Scheme: "http", Host: "localhost:9001", Path: path, RawQuery: "expand=items", Proto: "HTTP/1.1",
			Header:   http.Header{"Content-Type": {"application/json"}, "Authorization": {fmt.Sprintf("Bearer secret-token-%d", i)}},
			Body:     []byte(reqBody),
			BodySize: int64(len(reqBody)),
		},
		Response: model.Response{
			Status: status, Proto: "HTTP/1.1",
			Header:   http.Header{"Content-Type": {"application/json"}},
			Body:     []byte(respBody),
			BodySize: int64(len(respBody)),
		},
	}
}

// seedRecording stores a recording holding n exchanges.
func (e *env) seedRecording(name string, n int) model.Recording {
	e.t.Helper()
	ctx := context.Background()
	rec, err := e.st.CreateRecording(ctx, model.Recording{Name: name, Source: "proxy"})
	if err != nil {
		e.t.Fatalf("CreateRecording: %v", err)
	}
	if n > 0 {
		batch := make([]*model.Exchange, n)
		for i := range batch {
			batch[i] = mkExchange(i)
		}
		if err := e.st.AppendExchanges(ctx, rec.ID, batch); err != nil {
			e.t.Fatalf("AppendExchanges: %v", err)
		}
	}
	rec, err = e.st.GetRecording(ctx, rec.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return rec
}

// seedReplay stores a replay job over a recording.
func (e *env) seedReplay(name, recID string, status model.ReplayStatus, tweak ...func(*model.ReplayConfig)) model.ReplayJob {
	e.t.Helper()
	cfg := model.ReplayConfig{RecordingID: recID, Target: "http://staging.example.com", Mode: model.ReplayTimeline}.WithDefaults()
	for _, fn := range tweak {
		fn(&cfg)
	}
	job, err := e.st.PutReplayJob(context.Background(), model.ReplayJob{Name: name, Config: cfg, Status: status})
	if err != nil {
		e.t.Fatalf("PutReplayJob: %v", err)
	}
	return job
}

// asMap decodes a JSON object.
func asMap(t testing.TB, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	return m
}

// observation is a minimal metrics observation.
func observation(endpoint string, status int) metrics.Observation {
	return metrics.Observation{Endpoint: endpoint, Status: status, Duration: time.Millisecond, Error: model.IsErrorStatus(status)}
}

// benchRequest is httptest.NewRequest addressed the way a browser or client on
// the admin address would: to a loopback Host (the default, example.com, is
// refused by the DNS-rebinding guard) and with a JSON body type.
func benchRequest(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Host = "127.0.0.1:8484"
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}
