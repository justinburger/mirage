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
	"testing"
	"time"

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
