package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/store"
)

// route adds a test-only handler to the server's mux.
func (e *env) route(pattern string, h http.HandlerFunc) {
	e.srv.mux.HandleFunc(pattern, h)
}

func TestPanickingHandlerBecomesAJSON500(t *testing.T) {
	var (
		mu  sync.Mutex
		buf bytes.Buffer
	)
	logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, nil))
	e := newEnv(t, func(d *Deps) { d.Logger = logger })
	e.route("GET /api/v1/test/panic", func(http.ResponseWriter, *http.Request) { panic("kaboom: secret detail") })

	eb := e.errorOf("GET", "/api/v1/test/panic", nil, http.StatusInternalServerError)
	if strings.Contains(eb.Error, "kaboom") {
		t.Errorf("panic text leaked to the client: %q", eb.Error)
	}
	// The server survives and the panic was logged with its stack.
	e.call("GET", "/api/v1/health", nil, http.StatusOK, nil)
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "kaboom: secret detail") || !strings.Contains(out, "stack=") {
		t.Errorf("panic not logged:\n%s", out)
	}
}

func TestAbortHandlerPanicPassesThrough(t *testing.T) {
	e := newEnv(t)
	e.route("GET /api/v1/test/abort", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	e.route("GET /api/v1/test/halfway", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		panic("failure after the response started")
	})
	for _, path := range []string{"/api/v1/test/abort", "/api/v1/test/halfway"} {
		resp, err := e.ts.Client().Get(e.ts.URL + path)
		if err == nil {
			// For the halfway case the headers are already out; the body must fail.
			_, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		if err == nil {
			t.Errorf("GET %s: want a broken connection, got a clean response", path)
		}
	}
	e.call("GET", "/api/v1/health", nil, http.StatusOK, nil)
}

func TestHandlerErrorAfterResponseStartedAbortsTheConnection(t *testing.T) {
	e := newEnv(t)
	e.route("GET /api/v1/test/late-error", e.srv.h(func(w http.ResponseWriter, r *http.Request) error {
		_, _ = w.Write([]byte("start of a download"))
		return errors.New("disk on fire")
	}))
	resp, err := e.ts.Client().Get(e.ts.URL + "/api/v1/test/late-error")
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("a download that failed midway must not end like a complete one")
	}
}

func TestAccessLogLevels(t *testing.T) {
	record := func(level slog.Level) string {
		var (
			mu  sync.Mutex
			buf bytes.Buffer
		)
		logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, &slog.HandlerOptions{Level: level}))
		e := newEnv(t, func(d *Deps) { d.Logger = logger })
		e.call("GET", "/api/v1/services/svc_nope", nil, http.StatusNotFound, nil)
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
	if out := record(slog.LevelInfo); strings.Contains(out, "api: request") {
		t.Errorf("access log written at info level:\n%s", out)
	}
	out := record(slog.LevelDebug)
	for _, want := range []string{"api: request", "method=GET", "path=/api/v1/services/svc_nope", "status=404", "duration="} {
		if !strings.Contains(out, want) {
			t.Errorf("access log lacks %q:\n%s", want, out)
		}
	}
}

func TestNotFoundAndMethodNotAllowed(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		method, path string
		status       int
		allow        []string
	}{
		{"GET", "/api/v1/nope", 404, nil},
		{"POST", "/api/v1/nope", 404, nil},
		{"GET", "/api/v1/services/svc_x/nope", 404, nil},
		{"DELETE", "/api/v1/services", 405, []string{"GET", "POST"}},
		{"PUT", "/api/v1/services", 405, []string{"GET", "POST"}},
		{"POST", "/api/v1/services/svc_x", 405, []string{"DELETE", "GET", "PUT"}},
		{"GET", "/api/v1/services/svc_x/enable", 405, []string{"POST"}},
		{"POST", "/api/v1/health", 405, []string{"GET"}},
		{"PUT", "/api/v1/recordings/rec_x", 405, []string{"DELETE", "GET", "PATCH"}},
		{"GET", "/api/v1/stream/x", 404, nil},
		{"PATCH", "/api/v1/faults", 405, []string{"GET", "POST"}},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			eb := e.errorOf(tc.method, tc.path, nil, tc.status)
			if tc.status == 404 && !strings.Contains(eb.Error, "no such endpoint") {
				t.Errorf("error = %q", eb.Error)
			}
			resp, _ := e.do(tc.method, tc.path, nil)
			if len(tc.allow) > 0 {
				got := resp.Header.Get("Allow")
				for _, m := range tc.allow {
					if !strings.Contains(got, m) {
						t.Errorf("Allow = %q, want it to list %s", got, m)
					}
				}
			}
		})
	}
	// A path that needs cleaning is redirected by the mux, not mistaken for an endpoint.
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(e.ts.URL + "/api/v1//services")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 3 {
		t.Errorf("status = %d, want a redirect to the cleaned path", resp.StatusCode)
	}
}

// codeLog is a ResponseWriter that logs the status codes written to it.
type codeLog struct {
	h     http.Header
	codes []int
	body  bytes.Buffer
	flush int
}

func (c *codeLog) Header() http.Header {
	if c.h == nil {
		c.h = http.Header{}
	}
	return c.h
}
func (c *codeLog) WriteHeader(code int)        { c.codes = append(c.codes, code) }
func (c *codeLog) Write(b []byte) (int, error) { return c.body.Write(b) }
func (c *codeLog) Flush()                      { c.flush++ }

func TestRecorderTracksTheFinalStatus(t *testing.T) {
	under := &codeLog{}
	rec := &recorder{ResponseWriter: under}
	if rec.Started() {
		t.Error("a fresh recorder has not started")
	}
	rec.WriteHeader(http.StatusEarlyHints) // informational: not the final status
	if rec.Started() {
		t.Error("1xx must not count as the response start")
	}
	rec.Flush()
	if !rec.Started() || rec.statusCode() != 200 || under.flush != 1 {
		t.Errorf("after Flush: started=%v status=%d flushes=%d", rec.Started(), rec.statusCode(), under.flush)
	}
	rec.WriteHeader(500) // the header was already sent: the recorded status stays
	if rec.statusCode() != 200 {
		t.Errorf("status = %d", rec.statusCode())
	}
	if n, err := rec.Write([]byte("abc")); n != 3 || err != nil || rec.bytes != 3 {
		t.Errorf("Write = %d, %v, bytes %d", n, err, rec.bytes)
	}
	if rec.Unwrap() != under {
		t.Error("Unwrap does not return the underlying writer")
	}
	if got := under.codes; len(got) < 2 || got[0] != http.StatusEarlyHints {
		t.Errorf("the underlying writer saw %v", got)
	}

	implicit := &recorder{ResponseWriter: &codeLog{}}
	_, _ = implicit.Write([]byte("x"))
	if !implicit.Started() || implicit.statusCode() != 200 {
		t.Errorf("implicit 200: started=%v status=%d", implicit.Started(), implicit.statusCode())
	}
}

func TestNewPanicsWithoutStore(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New without a store must panic")
		}
	}()
	New(Deps{})
}

func TestNewToleratesMissingOptionalDeps(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Metrics, d.Faults, d.Logger, d.UI = nil, nil, nil, nil })
	e.call("GET", "/api/v1/stats", nil, http.StatusOK, nil)
	e.call("GET", "/api/v1/faults", nil, http.StatusOK, nil)
	e.call("GET", "/metrics", nil, http.StatusOK, nil)
	var f FaultRuleWithCount
	e.call("POST", "/api/v1/faults", faultBody("works"), http.StatusCreated, &f)
	if len(e.srv.deps.Faults.Rules()) != 1 {
		t.Error("the default injector was not loaded")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	e := newEnv(t)
	col := e.reg.Collector("service:svc_m", "service", "svc_m")
	col.Observe(observation("GET /m", 200))
	resp, body := e.do("GET", "/metrics", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	for _, want := range []string{"mirage_requests_total", `id="svc_m"`, "# TYPE mirage_request_duration_seconds summary"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %q:\n%s", want, body)
		}
	}
}

func TestHealth(t *testing.T) {
	e := newEnv(t)
	var h struct {
		Status  string  `json:"status"`
		Version string  `json:"version"`
		Uptime  float64 `json:"uptime_s"`
	}
	e.call("GET", "/api/v1/health", nil, http.StatusOK, &h)
	if h.Status != "ok" || h.Version != "v0.0.0-test" || h.Uptime < 0 {
		t.Errorf("health = %+v", h)
	}
}

// iterFailStore delivers a few exchanges and then fails, like a database that
// breaks while a long export is under way.
type iterFailStore struct {
	store.Store
	after int
}

func (s iterFailStore) IterateExchanges(ctx context.Context, id string, opts model.IterOptions, fn func(*model.Exchange) error) error {
	n := 0
	err := s.Store.IterateExchanges(ctx, id, opts, func(ex *model.Exchange) error {
		if n >= s.after {
			return errors.New("disk on fire")
		}
		n++
		return fn(ex)
	})
	return err
}

func TestExportFailingMidwayAbortsTheDownload(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Store = iterFailStore{Store: d.Store, after: 3} })
	rec := e.seedRecording("big", 10)

	resp, err := e.ts.Client().Get(e.ts.URL + "/api/v1/recordings/" + rec.ID + "/export.har")
	var body []byte
	if err == nil {
		body, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err == nil {
		t.Fatalf("a failed export ended like a complete one (%d bytes)", len(body))
	}
	// Whatever arrived is not a complete HAR document.
	if json.Valid(body) {
		t.Error("the truncated export is valid JSON")
	}
	e.call("GET", "/api/v1/health", nil, http.StatusOK, nil)
}

func TestUnavailableControllers(t *testing.T) {
	ctx := context.Background()
	s := unavailableServices{}
	if s.Remove(ctx, "x") != nil || s.ProfileChanged(ctx, "x") != nil || s.Statuses() != nil {
		t.Error("unavailableServices should be inert for removal, profile changes and status")
	}
	if !errors.Is(s.Apply(ctx, model.Service{}), errUnavailable) || !errors.Is(s.Reload(ctx, "x"), errUnavailable) {
		t.Error("Apply and Reload must report unavailability")
	}
	if st := s.Status("x"); st.ServiceID != "x" || st.Running {
		t.Errorf("Status = %+v", st)
	}
	r := unavailableReplays{}
	if jobs, err := r.List(ctx); err != nil || len(jobs) != 0 || len(r.Running()) != 0 {
		t.Errorf("List/Running = %v, %v", jobs, err)
	}
	if _, err := r.Create(ctx, "x", model.ReplayConfig{}); !errors.Is(err, errUnavailable) {
		t.Errorf("Create: %v", err)
	}
	if _, err := r.Get(ctx, "x"); !errors.Is(err, errUnavailable) {
		t.Errorf("Get: %v", err)
	}
	if _, err := r.Series(ctx, "x", 0); !errors.Is(err, errUnavailable) {
		t.Errorf("Series: %v", err)
	}
	if !errors.Is(r.Start(ctx, "x"), errUnavailable) || !errors.Is(r.Stop("x"), errUnavailable) || !errors.Is(r.Delete(ctx, "x"), errUnavailable) {
		t.Error("Start, Stop and Delete must report unavailability")
	}
}

func TestWaitContext(t *testing.T) {
	e := newEnv(t)
	if !e.srv.WaitContext(context.Background()) {
		t.Error("never started: nothing to wait for")
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.srv.Run(ctx)
	short, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if e.srv.WaitContext(short) {
		t.Error("the broadcaster is still running; WaitContext must time out")
	}
	cancel()
	long, stop2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop2()
	if !e.srv.WaitContext(long) {
		t.Error("the broadcaster did not stop")
	}
}

func TestByteSize(t *testing.T) {
	for n, want := range map[int64]string{512: "512 bytes", 2048: "2 KiB", 10 << 20: "10 MiB", 1 << 30: "1 GiB", 1500: "1500 bytes"} {
		if got := byteSize(n); got != want {
			t.Errorf("byteSize(%d) = %q, want %q", n, got, want)
		}
	}
}
