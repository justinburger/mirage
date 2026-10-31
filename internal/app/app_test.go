package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/config"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/store"
)

// lockedBuffer is a bytes.Buffer safe for the logger's concurrent use.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func testLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// startApp runs a new App until the test ends and returns it with its stop
// function (which waits for Run to return).
func startApp(t *testing.T, opts Options) (*App, func() error) {
	t.Helper()
	if opts.Admin == "" {
		opts.Admin = "127.0.0.1:0"
	}
	a, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	var once sync.Once
	var result error
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-done:
			case <-time.After(30 * time.Second):
				result = fmt.Errorf("Run did not return within 30s of cancellation")
			}
		})
		return result
	}
	t.Cleanup(func() { _ = stop() })
	return a, stop
}

func getJSON(t *testing.T, url string, out any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("GET %s: decode %q: %v", url, body, err)
		}
	}
	return resp.StatusCode
}

func eventually(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAppRecordsThroughAConfiguredService(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path":%q,"email":"jane@acme.io"}`, r.URL.Path)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cfg := writeFile(t, dir, "mirage.yaml", fmt.Sprintf(`
recordings:
  - name: wired
services:
  - name: web
    listen_addr: 127.0.0.1:0
    upstream: %s
    mode: record
    enabled: true
    recording: wired
    anonymize_capture: true
  - name: off
    listen_addr: 127.0.0.1:0
    upstream: %s
    mode: passthrough
`, upstream.URL, upstream.URL))

	logs := &lockedBuffer{}
	a, stop := startApp(t, Options{Data: filepath.Join(dir, "m.db"), ConfigPath: cfg, Logger: testLogger(logs)})
	base := a.AdminURL()

	var health struct{ Status string }
	if code := getJSON(t, base+"/api/v1/health", &health); code != 200 || health.Status != "ok" {
		t.Fatalf("health: %d %+v", code, health)
	}
	var svcs []struct {
		ID, Name string
		Status   model.ServiceStatus
	}
	getJSON(t, base+"/api/v1/services", &svcs)
	var web, off *model.ServiceStatus
	for i := range svcs {
		switch svcs[i].Name {
		case "web":
			web = &svcs[i].Status
		case "off":
			off = &svcs[i].Status
		}
	}
	if web == nil || off == nil {
		t.Fatalf("services: %+v", svcs)
	}
	if !web.Running || web.Addr == "" {
		t.Fatalf("service web should run: %+v", web)
	}
	if off.Running {
		t.Errorf("a service that is not enabled must not run: %+v", off)
	}

	// Traffic through the service reaches the upstream and is recorded.
	resp, err := http.Get("http://" + web.Addr + "/users/42")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"path":"/users/42"`) {
		t.Fatalf("proxied response: %d %s", resp.StatusCode, body)
	}
	var recs []model.Recording
	eventually(t, "the exchange to be captured", 10*time.Second, func() bool {
		getJSON(t, base+"/api/v1/recordings", &recs)
		return len(recs) == 1 && recs[0].ExchangeCount == 1
	})
	if !recs[0].Anonymized {
		t.Errorf("recording should be anonymized: %+v", recs[0])
	}
	if res := a.Applied(); len(res.Created) == 0 {
		t.Errorf("Applied() should list what the config created: %+v", res)
	}

	// The UI and metrics are served by the same listener.
	for path, want := range map[string]string{"/": "text/html", "/metrics": "text/plain"} {
		r, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), want) {
			t.Errorf("GET %s: %d %s", path, r.StatusCode, r.Header.Get("Content-Type"))
		}
	}

	start := time.Now()
	if err := stop(); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("shutdown took %v\n%s", d, logs.String())
	}
	if _, err := http.Get("http://" + web.Addr + "/"); err == nil {
		t.Error("the service listener should be closed after shutdown")
	}
	if !strings.Contains(logs.String(), "shutdown complete") {
		t.Errorf("log should end with shutdown complete:\n%s", logs.String())
	}

	// The capture was flushed to disk before the store closed.
	st, err := store.Open(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	list, err := st.ListRecordings(context.Background())
	if err != nil || len(list) != 1 || list[0].ExchangeCount != 1 {
		t.Fatalf("after shutdown: %+v, %v", list, err)
	}
}

func TestShutdownClosesOpenEventStreams(t *testing.T) {
	a, stop := startApp(t, Options{Data: store.MemoryPath})
	req, _ := http.NewRequest("GET", a.AdminURL()+"/api/v1/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("stream should deliver an event on connect: %v", err)
	}
	start := time.Now()
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("an open SSE stream delayed shutdown by %v", d)
	}
}

func TestServiceThatCannotStartDoesNotStopTheOthers(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer upstream.Close()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	dir := t.TempDir()
	cfg := writeFile(t, dir, "c.yaml", fmt.Sprintf(`
services:
  - name: a-blocked
    listen_addr: %s
    upstream: %s
    mode: passthrough
    enabled: true
  - name: b-fine
    listen_addr: 127.0.0.1:0
    upstream: %s
    mode: passthrough
    enabled: true
`, busy.Addr().String(), upstream.URL, upstream.URL))
	logs := &lockedBuffer{}
	a, _ := startApp(t, Options{Data: filepath.Join(dir, "m.db"), ConfigPath: cfg, Logger: testLogger(logs)})

	var svcs []struct {
		Name   string
		Status model.ServiceStatus
	}
	getJSON(t, a.AdminURL()+"/api/v1/services", &svcs)
	got := map[string]model.ServiceStatus{}
	for _, s := range svcs {
		got[s.Name] = s.Status
	}
	if got["a-blocked"].Running {
		t.Errorf("a-blocked listens on a busy port and cannot run: %+v", got["a-blocked"])
	}
	if got["a-blocked"].Error == "" {
		t.Errorf("the start failure should be visible in the status: %+v", got["a-blocked"])
	}
	if !got["b-fine"].Running {
		t.Errorf("b-fine should be running: %+v", got["b-fine"])
	}
	if !strings.Contains(logs.String(), "service failed to start") {
		t.Errorf("the failure should be logged:\n%s", logs.String())
	}
}

func TestNewRejectsABadConfigAndReleasesTheDatabase(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, "bad.yaml", "services:\n  - name: x\n    listen_adr: \":9\"\n")
	dbPath := filepath.Join(dir, "m.db")
	_, err := New(context.Background(), Options{Admin: "127.0.0.1:0", Data: dbPath, ConfigPath: cfg})
	if err == nil || !strings.Contains(err.Error(), "listen_adr") {
		t.Fatalf("want an error naming the bad field, got %v", err)
	}
	// The failed New must have closed the database: it can be reopened.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
}

func TestNewRejectsABadProfileSelector(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, "bad.yaml", `
profiles:
  - name: broken
    rules:
      - name: r
        enabled: true
        target: json
        selector: "$[?(("
        strategy: hash
`)
	_, err := New(context.Background(), Options{Admin: "127.0.0.1:0", Data: store.MemoryPath, ConfigPath: cfg})
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("want an error about profile broken, got %v", err)
	}
}

func TestNewReportsAnAdminAddressInUse(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	dbPath := filepath.Join(t.TempDir(), "m.db")
	_, err = New(context.Background(), Options{Admin: busy.Addr().String(), Data: dbPath})
	if err == nil || !strings.Contains(err.Error(), "admin listener") {
		t.Fatalf("want an admin listener error, got %v", err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("database should be released after a failed New: %v", err)
	}
	st.Close()
}

func TestCloseWithoutRun(t *testing.T) {
	a, err := New(context.Background(), Options{Admin: "127.0.0.1:0", Data: store.MemoryPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := a.Run(context.Background()); err == nil {
		// Run after Close would serve on a closed store; it must not hang.
		t.Log("Run after Close returned nil")
	}
}

// Without an admin token the API answers only to IP addresses, localhost and
// the names the operator lists (--allowed-host), so a rebound DNS name cannot
// reach it.
func TestAllowedHostsReachTheAdminAPI(t *testing.T) {
	get := func(a *App, host string) int {
		t.Helper()
		req, _ := http.NewRequest("GET", a.AdminURL()+"/api/v1/services", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	plain, _ := startApp(t, Options{Data: store.MemoryPath})
	if got := get(plain, "mirage.lan:8484"); got != http.StatusForbidden {
		t.Errorf("an unlisted name: %d, want 403", got)
	}
	if got := get(plain, "localhost:8484"); got != http.StatusOK {
		t.Errorf("localhost: %d, want 200", got)
	}

	listed, _ := startApp(t, Options{Data: store.MemoryPath, AllowedHosts: []string{"mirage.lan"}})
	if got := get(listed, "mirage.lan:8484"); got != http.StatusOK {
		t.Errorf("a listed name: %d, want 200", got)
	}
	if got := get(listed, "evil.example"); got != http.StatusForbidden {
		t.Errorf("another name: %d, want 403", got)
	}
}

func TestAdminTokenIsEnforced(t *testing.T) {
	a, _ := startApp(t, Options{Data: store.MemoryPath, AdminToken: "s3cret"})
	for _, c := range []struct {
		path, auth string
		want       int
	}{
		{"/api/v1/health", "", 200},
		{"/api/v1/services", "", 401},
		{"/api/v1/services", "Bearer s3cret", 200},
		{"/api/v1/services", "Bearer wrong", 401},
	} {
		req, _ := http.NewRequest("GET", a.AdminURL()+c.path, nil)
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("GET %s with %q: %d, want %d", c.path, c.auth, resp.StatusCode, c.want)
		}
	}
}

// The virtualizer factory must hand the proxy an untyped nil on failure: a
// typed nil *virtual.Engine inside the interface would pass the proxy's
// "v != nil" check and crash on first use.
func TestVirtualizerFactoryReturnsUntypedNilOnError(t *testing.T) {
	a, err := New(context.Background(), Options{Admin: "127.0.0.1:0", Data: store.MemoryPath})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	v, err := a.virtualizer(context.Background(), model.Service{RecordingID: "rec_missing", Mode: model.ModeVirtualize}, nil)
	if err == nil {
		t.Fatal("want an error for a recording that does not exist")
	}
	if v != nil {
		t.Fatalf("factory returned a non-nil interface (%T) together with an error", v)
	}
}

// Starting a virtualize service for a recording that was filled by an earlier
// record run: the stored service comes back after a restart without the
// config file.
func TestStoredServicesStartAgain(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "hello "+r.URL.Path)
	}))
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "m.db")
	cfg := writeFile(t, dir, "c.yaml", fmt.Sprintf(`
recordings:
  - name: r
services:
  - name: s
    listen_addr: 127.0.0.1:0
    upstream: %s
    mode: record
    enabled: true
    recording: r
`, upstream.URL))

	a, stop := startApp(t, Options{Data: dbPath, ConfigPath: cfg})
	var svcs []struct {
		ID     string
		Status model.ServiceStatus
	}
	getJSON(t, a.AdminURL()+"/api/v1/services", &svcs)
	if len(svcs) != 1 || !svcs[0].Status.Running {
		t.Fatalf("services: %+v", svcs)
	}
	resp, err := http.Get("http://" + svcs[0].Status.Addr + "/greet")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	eventually(t, "capture", 10*time.Second, func() bool {
		var recs []model.Recording
		getJSON(t, a.AdminURL()+"/api/v1/recordings", &recs)
		return len(recs) == 1 && recs[0].ExchangeCount == 1
	})
	// Switch to virtualize over the API, then stop the upstream and restart.
	switchBody := fmt.Sprintf(`{"mode":"virtualize","recording_id":%q}`, mustRecordingID(t, a))
	r, err := http.Post(a.AdminURL()+"/api/v1/services/"+svcs[0].ID+"/mode", "application/json", strings.NewReader(switchBody))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("mode switch: %d", r.StatusCode)
	}
	upstream.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	b, _ := startApp(t, Options{Data: dbPath})
	var again []struct{ Status model.ServiceStatus }
	getJSON(t, b.AdminURL()+"/api/v1/services", &again)
	if len(again) != 1 || !again[0].Status.Running || again[0].Status.Mode != model.ModeVirtualize {
		t.Fatalf("after restart: %+v", again)
	}
	resp, err = http.Get("http://" + again[0].Status.Addr + "/greet")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(got) != "hello /greet" {
		t.Fatalf("virtualized response after restart: %d %q", resp.StatusCode, got)
	}
}

func mustRecordingID(t *testing.T, a *App) string {
	t.Helper()
	recs, err := a.Store().ListRecordings(context.Background())
	if err != nil || len(recs) != 1 {
		t.Fatalf("recordings: %+v, %v", recs, err)
	}
	return recs[0].ID
}

// A client that stalls in the middle of an admin upload keeps its connection
// active, so the admin server cannot drain. That must not eat the time the
// replay jobs and the capture queue need: the job stops and is saved at once,
// and every capture the service acknowledged is on disk.
func TestStalledAdminRequestDoesNotStarveTheOtherShutdownPhases(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":1}`)
	}))
	defer upstream.Close()
	db := filepath.Join(t.TempDir(), "m.db")
	logs := &lockedBuffer{}
	a, err := New(context.Background(), Options{Data: db, Admin: "127.0.0.1:0", Logger: testLogger(logs)})
	if err != nil {
		t.Fatal(err)
	}
	// Short budgets keep the test fast; the proportions are what matter. With
	// one shared deadline the admin phase alone would use all 3 s.
	a.shutdownTimeout = 3 * time.Second
	a.adminGrace = 300 * time.Millisecond

	ctx := context.Background()
	rec, err := a.Store().CreateRecording(ctx, model.Recording{Name: "captured"})
	if err != nil {
		t.Fatal(err)
	}
	seed, err := a.Store().CreateRecording(ctx, model.Recording{Name: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Store().AppendExchanges(ctx, seed.ID, []*model.Exchange{{
		ServiceID: "x", StartedAt: time.Now(),
		Request:  model.Request{Method: "GET", Path: "/a"},
		Response: model.Response{Status: 200},
	}}); err != nil {
		t.Fatal(err)
	}
	svc, err := a.Store().PutService(ctx, model.Service{
		ID: "svc_stall", Name: "stall", ListenAddr: "127.0.0.1:0", Upstream: upstream.URL,
		Mode: model.ModeRecord, Enabled: true, RecordingID: rec.ID,
	}.WithDefaults())
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(runCtx) }()
	eventually(t, "the service to listen", 10*time.Second, func() bool { return a.proxy.Status(svc.ID).Addr != "" })
	svcAddr := a.proxy.Status(svc.ID).Addr

	job, err := a.replay.Create(ctx, "long", model.ReplayConfig{
		RecordingID: seed.ID, Target: upstream.URL, Mode: model.ReplayRate, Rate: 200,
		Duration: model.Duration(5 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.replay.Start(ctx, job.ID); err != nil {
		t.Fatal(err)
	}

	// Steady traffic through the service until shutdown has finished.
	var acked atomic.Int64
	stopLoad := make(chan struct{})
	loadDone := make(chan struct{})
	go func() {
		defer close(loadDone)
		c := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
		for {
			select {
			case <-stopLoad:
				return
			default:
			}
			if resp, err := c.Get("http://" + svcAddr + "/x"); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 {
					acked.Add(1)
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	eventually(t, "some captured traffic", 10*time.Second, func() bool { return acked.Load() > 20 })

	// Headers and the start of a body, then silence: this request stays active.
	stalled, err := net.Dial("tcp", a.AdminAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	fmt.Fprint(stalled, "POST /api/v1/recordings/import HTTP/1.1\r\nHost: x\r\nContent-Length: 100000\r\nContent-Type: application/json\r\n\r\n{")
	time.Sleep(200 * time.Millisecond)

	signalled := time.Now()
	cancel()
	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return")
	}
	took := time.Since(signalled)
	close(stopLoad)
	<-loadDone

	if runErr != nil {
		t.Errorf("Run returned %v; a connection cut after the grace period is not a failed shutdown\n%s", runErr, logs.String())
	}
	if took > 2*time.Second {
		t.Errorf("shutdown took %v with a 300 ms admin grace and a 3 s budget\n%s", took, logs.String())
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	got, err := st.GetReplayJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ReplayStopped {
		t.Errorf("replay job is %s (%q), want stopped", got.Status, got.Error)
	}
	if got.EndedAt == nil || got.EndedAt.Sub(signalled) > time.Second {
		t.Errorf("the replay job kept running after the signal: ended %v, signalled %v", got.EndedAt, signalled)
	}

	// The cut import cleaned up after itself before the store was closed.
	if recs, err := st.ListRecordings(ctx); err != nil || len(recs) != 2 {
		t.Errorf("recordings after shutdown: %d, %v (want only captured and seed)\n%s", len(recs), err, logs.String())
	}
	if !strings.Contains(logs.String(), "connections were closed") {
		t.Errorf("cutting a stalled request should be logged:\n%s", logs.String())
	}

	r2, err := st.GetRecording(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r2.ExchangeCount != acked.Load() {
		t.Errorf("client was acknowledged %d requests but %d exchanges were stored", acked.Load(), r2.ExchangeCount)
	}
}

// The example configuration is what people copy, and trafficgen logs in both as
// JSON and as a form: whatever the example's "strict" profile is labelled, no
// credential the demo sends may be stored in clear.
func TestExampleStrictProfileKeepsDemoCredentialsOutOfTheRecording(t *testing.T) {
	var gotForm, gotJSON atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("Set-Cookie", "session=0123456789abcdef; Path=/; HttpOnly")
		io.WriteString(w, `{"token":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2ln","token_type":"Bearer"}`)
	}))
	defer upstream.Close()

	cfg, err := config.Load("../../examples/mirage.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Services) != 1 {
		t.Fatalf("the example defines %d services, want 1", len(cfg.Services))
	}
	cfg.Services[0].ListenAddr = "127.0.0.1:0"
	cfg.Services[0].Upstream = upstream.URL
	cfg.Faults = nil

	a, stop := startApp(t, Options{Data: filepath.Join(t.TempDir(), "m.db"), Config: cfg})
	var svcAddr string
	eventually(t, "the demo service to listen", 10*time.Second, func() bool {
		svcs, _ := a.Store().ListServices(context.Background())
		for _, s := range svcs {
			if s.Name == "demo" {
				svcAddr = a.proxy.Status(s.ID).Addr
			}
		}
		return svcAddr != ""
	})

	const secret = "password123"
	logins := []struct {
		contentType, body string
		flag              *atomic.Bool
	}{
		{"application/json", `{"email":"judy.lee@example.com","password":"` + secret + `"}`, &gotJSON},
		{"application/x-www-form-urlencoded", "email=judy.lee%40example.com&password=" + secret, &gotForm},
	}
	for _, l := range logins {
		req, _ := http.NewRequest("POST", "http://"+svcAddr+"/api/login", strings.NewReader(l.body))
		req.Header.Set("Content-Type", l.contentType)
		req.Header.Set("Authorization", "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2ln")
		req.Header.Set("Cookie", "session=0123456789abcdef")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		l.flag.Store(true)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !gotJSON.Load() || !gotForm.Load() {
		t.Fatal("both logins should have been sent")
	}

	st, err := store.Open(a.opts.Data)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	recs, err := st.ListRecordings(context.Background())
	if err != nil || len(recs) != 1 {
		t.Fatalf("recordings: %v, %v", recs, err)
	}
	items, total, err := st.ListExchanges(context.Background(), recs[0].ID, model.ExchangeQuery{})
	if err != nil || total != 2 {
		t.Fatalf("exchanges: %d, %v", total, err)
	}
	for _, it := range items {
		ex, err := st.GetExchange(context.Background(), it.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !ex.Anonymized {
			t.Errorf("exchange %d is not marked anonymized", ex.ID)
		}
		var all strings.Builder
		all.Write(ex.Request.Body)
		all.Write(ex.Response.Body)
		for _, h := range []http.Header{ex.Request.Header, ex.Response.Header} {
			for k, vs := range h {
				all.WriteString(k + ": " + strings.Join(vs, ",") + "\n")
			}
		}
		for _, raw := range []string{secret, "judy.lee", "0123456789abcdef", "eyJhbGciOiJIUzI1NiJ9"} {
			if strings.Contains(all.String(), raw) {
				t.Errorf("exchange %d (%s) keeps %q in clear:\nrequest body: %s\n%s", ex.ID, ex.Request.Header.Get("Content-Type"), raw, ex.Request.Body, all.String())
			}
		}
	}
}

// A service that records with anonymize_capture stores anonymized paths, queries
// and bodies. Switched to virtualize or hybrid, it must serve those recordings
// to the very requests that were recorded (raw emails in the path, the query
// and the body), each with the response recorded for it and at match level
// "exact", not collapse them onto one response or miss them.
func TestAnonymizedRecordingIsServedToTheRawRequests(t *testing.T) {
	// The upstream answers with a marker that carries no PII, so the recorded
	// response is what it said and the test can tell the responses apart.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		who := "nobody"
		for _, name := range []string{"jane", "bob"} {
			if strings.Contains(r.URL.Path, name) || strings.Contains(string(body), name) {
				who = name
			}
		}
		kind := "profile"
		if r.Method == http.MethodPost {
			kind = "order"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"answer":%q}`, kind+"-"+who)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cfg := writeFile(t, dir, "mirage.yaml", fmt.Sprintf(`
recordings:
  - name: anon
services:
  - name: anon
    listen_addr: 127.0.0.1:0
    upstream: %s
    mode: record
    enabled: true
    recording: anon
    anonymize_capture: true
`, upstream.URL))
	a, _ := startApp(t, Options{Data: filepath.Join(dir, "m.db"), ConfigPath: cfg})
	base := a.AdminURL()

	var svcs []struct {
		ID     string
		Status model.ServiceStatus
	}
	getJSON(t, base+"/api/v1/services", &svcs)
	if len(svcs) != 1 || !svcs[0].Status.Running {
		t.Fatalf("services: %+v", svcs)
	}
	svcID, addr := svcs[0].ID, svcs[0].Status.Addr

	type call struct {
		method, path, query, body string
		want                      string
	}
	calls := []call{
		{"GET", "/users/jane@acme.io/profile", "email=jane@acme.io", "", "profile-jane"},
		{"GET", "/users/bob@acme.io/profile", "email=bob@acme.io", "", "profile-bob"},
		{"POST", "/orders", "", `{"email":"jane@acme.io","item":"book"}`, "order-jane"},
		{"POST", "/orders", "", `{"email":"bob@acme.io","item":"book"}`, "order-bob"},
	}
	send := func(c call) (int, string, string) {
		t.Helper()
		u := "http://" + addr + c.path
		if c.query != "" {
			u += "?" + c.query
		}
		var body io.Reader
		if c.body != "" {
			body = strings.NewReader(c.body)
		}
		req, err := http.NewRequest(c.method, u, body)
		if err != nil {
			t.Fatal(err)
		}
		if c.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, u, err)
		}
		defer resp.Body.Close()
		got, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(got), resp.Header.Get("X-Mirage-Match")
	}

	// Record.
	for _, c := range calls {
		if code, body, _ := send(c); code != 200 || !strings.Contains(body, c.want) {
			t.Fatalf("recording %s %s: %d %s", c.method, c.path, code, body)
		}
	}
	var recs []model.Recording
	eventually(t, "the exchanges to be captured", 10*time.Second, func() bool {
		getJSON(t, base+"/api/v1/recordings", &recs)
		return len(recs) == 1 && recs[0].ExchangeCount == int64(len(calls))
	})
	recID := recs[0].ID
	if !recs[0].Anonymized {
		t.Fatalf("recording should be anonymized: %+v", recs[0])
	}
	// The premise of the test: nothing raw was stored, so a raw request can
	// only match through the anonymizer.
	err := a.Store().IterateExchanges(context.Background(), recID, model.IterOptions{RequestBodies: true}, func(ex *model.Exchange) error {
		for _, field := range []string{ex.Request.Path, ex.Request.RawQuery, string(ex.Request.Body)} {
			if strings.Contains(field, "acme.io") {
				t.Errorf("exchange %d still holds a raw email: %q", ex.Seq, field)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The upstream is gone from here on: every answer comes from the recording.
	upstream.Close()

	for _, mode := range []model.Mode{model.ModeVirtualize, model.ModeHybrid} {
		t.Run(string(mode), func(t *testing.T) {
			body := fmt.Sprintf(`{"mode":%q,"recording_id":%q}`, mode, recID)
			resp, err := http.Post(base+"/api/v1/services/"+svcID+"/mode", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			var out struct{ Status model.ServiceStatus }
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err := json.Unmarshal(raw, &out); err != nil || resp.StatusCode != 200 || !out.Status.Running || out.Status.Error != "" || out.Status.Mode != mode {
				t.Fatalf("switch to %s: %d %s", mode, resp.StatusCode, raw)
			}
			addr = out.Status.Addr

			// Each request twice: the second answer must not move on to
			// another recorded response either.
			for round := 0; round < 2; round++ {
				for _, c := range calls {
					code, got, level := send(c)
					if code != 200 || !strings.Contains(got, c.want) {
						t.Errorf("%s %s?%s %s round %d: %d %s, want the response recorded for it (%s)", c.method, c.path, c.query, c.body, round, code, got, c.want)
					}
					if level != "exact" {
						t.Errorf("%s %s?%s %s: X-Mirage-Match = %q, want exact", c.method, c.path, c.query, c.body, level)
					}
				}
			}
			// A request nobody recorded does not pass for a recorded one.
			if _, _, level := send(call{"GET", "/users/carol@acme.io/profile", "email=carol@acme.io", "", ""}); level == "exact" {
				t.Errorf("a request that was never recorded matched exactly")
			}
		})
	}
}

// The factory builds the matching anonymizer only for a service that captures
// anonymized, and a profile it cannot load fails the build (with an untyped
// nil, never an engine with a nil anonymizer inside).
func TestVirtualizerFactoryAnonymizerProfile(t *testing.T) {
	a, err := New(context.Background(), Options{Admin: "127.0.0.1:0", Data: store.MemoryPath})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	rec, err := a.Store().CreateRecording(context.Background(), model.Recording{Name: "r", Source: "proxy"})
	if err != nil {
		t.Fatal(err)
	}
	svc := model.Service{Mode: model.ModeVirtualize, RecordingID: rec.ID, ProfileID: "prof_missing"}

	// Not anonymizing: the profile is irrelevant and is not looked up.
	v, err := a.virtualizer(context.Background(), svc, nil)
	if err != nil || v == nil {
		t.Fatalf("service without anonymize_capture: %v, %v", v, err)
	}
	_ = v.Close()

	svc.AnonymizeCapture = true
	v, err = a.virtualizer(context.Background(), svc, nil)
	if err == nil || !strings.Contains(err.Error(), "prof_missing") {
		t.Fatalf("missing profile: err = %v", err)
	}
	if v != nil {
		t.Fatalf("factory returned a non-nil interface (%T) together with an error", v)
	}

	// The built-in profile (no ProfileID) loads.
	svc.ProfileID = ""
	if v, err = a.virtualizer(context.Background(), svc, nil); err != nil || v == nil {
		t.Fatalf("default profile: %v, %v", v, err)
	}
	_ = v.Close()
}
