package fault

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

func TestPlanNil(t *testing.T) {
	var p *Plan
	w := httptest.NewRecorder()
	if p.Apply(w, newReq("GET", "/x")) {
		t.Error("nil Plan.Apply reported handled")
	}
	if w.Flushed || w.Body.Len() != 0 || len(w.Header()) != 0 {
		t.Error("nil Plan.Apply wrote to the response")
	}
	if got := p.WrapWriter(w); got != http.ResponseWriter(w) {
		t.Error("nil Plan.WrapWriter changed the writer")
	}
	if got := p.WrapWriterContext(context.Background(), w); got != http.ResponseWriter(w) {
		t.Error("nil Plan.WrapWriterContext changed the writer")
	}
}

func TestPlanWithoutTerminal(t *testing.T) {
	w := httptest.NewRecorder()
	p := &Plan{Delay: 30 * time.Millisecond}
	start := time.Now()
	if p.Apply(w, newReq("GET", "/x")) {
		t.Error("a latency-only plan must not report handled")
	}
	if el := time.Since(start); el < 30*time.Millisecond {
		t.Errorf("Apply returned after %v, want at least 30ms", el)
	}
	if w.Body.Len() != 0 || w.Code != 200 || len(w.Header()) != 0 {
		t.Error("latency-only plan wrote a response")
	}
	// A plan with only a throttle does nothing in Apply.
	if (&Plan{BytesPerSecond: 100}).Apply(w, newReq("GET", "/x")) {
		t.Error("throttle-only plan reported handled")
	}
}

func TestApplyDelayWaitsFullDuration(t *testing.T) {
	inj := newInjector(t, latencyRule("a", 60*time.Millisecond, 0), latencyRule("b", 40*time.Millisecond, 0, order(1)))
	r := newReq("GET", "/x")
	p := inj.Decide("s", r)
	start := time.Now()
	if p.Apply(httptest.NewRecorder(), r) {
		t.Fatal("unexpected handled")
	}
	if el := time.Since(start); el < 100*time.Millisecond {
		t.Errorf("waited %v, want at least the 100ms sum", el)
	}
}

func TestApplyDelayHonoursCancellation(t *testing.T) {
	tests := []struct {
		name string
		plan *Plan
	}{
		{"latency only", &Plan{Delay: time.Minute}},
		{"latency then error", &Plan{Delay: time.Minute, Terminal: &model.FaultRule{Name: "e", Type: model.FaultError, Status: 500}}},
		{"latency then abort", &Plan{Delay: time.Minute, Terminal: &model.FaultRule{Name: "a", Type: model.FaultAbort}}},
		{"latency then timeout", &Plan{Delay: time.Minute, Terminal: &model.FaultRule{Name: "t", Type: model.FaultTimeout, Delay: model.Duration(time.Minute)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			r := newReq("GET", "/x").WithContext(ctx)
			w := httptest.NewRecorder()
			time.AfterFunc(30*time.Millisecond, cancel)
			defer cancel()

			start := time.Now()
			handled := tt.plan.Apply(w, r) // must not panic: the client is gone, there is nothing to abort
			if !handled {
				t.Error("Apply did not report handled after cancellation")
			}
			if el := time.Since(start); el > 5*time.Second {
				t.Errorf("Apply took %v to notice cancellation", el)
			}
			if w.Body.Len() != 0 || w.Flushed || len(w.Header()) != 0 {
				t.Error("Apply wrote a response for a cancelled request")
			}
		})
	}
}

func TestApplyAlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := newReq("GET", "/x").WithContext(ctx)
	if !(&Plan{Delay: time.Minute}).Apply(httptest.NewRecorder(), r) {
		t.Error("expected handled for an already-cancelled request")
	}
}

func TestApplyError(t *testing.T) {
	tests := []struct {
		name        string
		rule        model.FaultRule
		method      string
		wantStatus  int
		wantType    string // "" = header must be absent
		wantBody    string
		wantHeaders map[string]string
	}{
		{
			name:       "default status, empty body",
			rule:       model.FaultRule{ID: "e", Name: "plain 503", Enabled: true, Probability: 1, Type: model.FaultError},
			wantStatus: 503, wantType: "text/plain; charset=utf-8", wantBody: "",
		},
		{
			name:       "json object",
			rule:       errRule("e", name("json"), func(r *model.FaultRule) { r.Status = 500; r.Body = `{"error":"overloaded"}` }),
			wantStatus: 500, wantType: "application/json", wantBody: `{"error":"overloaded"}`,
		},
		{
			name:       "json array with leading whitespace",
			rule:       errRule("e", name("array"), func(r *model.FaultRule) { r.Status = 502; r.Body = "\n  [1, 2, 3]" }),
			wantStatus: 502, wantType: "application/json", wantBody: "\n  [1, 2, 3]",
		},
		{
			name:       "plain text",
			rule:       errRule("e", name("text"), func(r *model.FaultRule) { r.Status = 429; r.Body = "slow down" }),
			wantStatus: 429, wantType: "text/plain; charset=utf-8", wantBody: "slow down",
		},
		{
			name:       "invalid json is text",
			rule:       errRule("e", name("broken"), func(r *model.FaultRule) { r.Body = `{"error": ` }),
			wantStatus: 503, wantType: "text/plain; charset=utf-8", wantBody: `{"error": `,
		},
		{
			name:       "json scalar is text",
			rule:       errRule("e", name("scalar"), func(r *model.FaultRule) { r.Body = `123` }),
			wantStatus: 503, wantType: "text/plain; charset=utf-8", wantBody: `123`,
		},
		{
			name: "rule content type wins",
			rule: errRule("e", name("own type"), func(r *model.FaultRule) {
				r.Body = `{"a":1}`
				r.ResponseHeader = map[string]string{"content-type": "application/problem+json"}
			}),
			wantStatus: 503, wantType: "application/problem+json", wantBody: `{"a":1}`,
		},
		{
			name: "response headers",
			rule: errRule("e", name("with headers"), func(r *model.FaultRule) {
				r.Status = 429
				r.Body = "no"
				r.ResponseHeader = map[string]string{"Retry-After": "1", "x-custom": "yes"}
			}),
			wantStatus: 429, wantType: "text/plain; charset=utf-8", wantBody: "no",
			wantHeaders: map[string]string{"Retry-After": "1", "X-Custom": "yes", "X-Mirage-Fault": "with headers"},
		},
		{
			name: "response headers cannot override the fault header or length",
			rule: errRule("e", name("real name"), func(r *model.FaultRule) {
				r.Body = "abc"
				r.ResponseHeader = map[string]string{"X-Mirage-Fault": "spoofed", "Content-Length": "999"}
			}),
			wantStatus: 503, wantType: "text/plain; charset=utf-8", wantBody: "abc",
			wantHeaders: map[string]string{"X-Mirage-Fault": "real name", "Content-Length": "3"},
		},
		{
			name: "newlines in values are neutralised",
			rule: errRule("e", name("evil\r\nSet-Cookie: x=1"), func(r *model.FaultRule) {
				r.ResponseHeader = map[string]string{"X-A": "a\r\nb"}
			}),
			wantStatus: 503, wantType: "text/plain; charset=utf-8",
			wantHeaders: map[string]string{"X-Mirage-Fault": "evil  Set-Cookie: x=1", "X-A": "a  b"},
		},
		{
			name:       "204 has no body or content type",
			rule:       errRule("e", name("no content"), func(r *model.FaultRule) { r.Status = 204; r.Body = "ignored" }),
			wantStatus: 204, wantType: "", wantBody: "",
		},
		{
			name:       "304 has no body",
			rule:       errRule("e", name("not modified"), func(r *model.FaultRule) { r.Status = 304; r.Body = "ignored" }),
			wantStatus: 304, wantType: "", wantBody: "",
		},
		{
			name: "non-error status is allowed",
			rule: errRule("e", name("redirect"), func(r *model.FaultRule) {
				r.Status = 302
				r.ResponseHeader = map[string]string{"Location": "/elsewhere"}
			}),
			wantStatus: 302, wantType: "text/plain; charset=utf-8", wantHeaders: map[string]string{"Location": "/elsewhere"},
		},
		{
			name:       "unicode body",
			rule:       errRule("e", name("unicode"), func(r *model.FaultRule) { r.Body = "défaillance ☃" }),
			wantStatus: 503, wantType: "text/plain; charset=utf-8", wantBody: "défaillance ☃",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inj := newInjector(t, tt.rule)
			r := newReq(firstNonEmpty(tt.method, "GET"), "/x")
			plan := inj.Decide("s", r)
			if plan == nil || plan.Terminal == nil {
				t.Fatalf("no terminal plan: %+v", plan)
			}
			w := httptest.NewRecorder()
			if !plan.Apply(w, r) {
				t.Fatal("Apply did not report handled for an error fault")
			}
			res := w.Result()
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)

			if res.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", res.StatusCode, tt.wantStatus)
			}
			if got := res.Header.Get("Content-Type"); got != tt.wantType {
				t.Errorf("Content-Type = %q, want %q", got, tt.wantType)
			}
			if string(body) != tt.wantBody {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
			if got, want := res.Header.Get("X-Mirage-Fault"), sanitizeHeaderValue(tt.rule.Name); got != want {
				t.Errorf("X-Mirage-Fault = %q, want %q", got, want)
			}
			for k, v := range tt.wantHeaders {
				if got := res.Header.Get(k); got != v {
					t.Errorf("header %s = %q, want %q", k, got, v)
				}
			}
			if bodyAllowed(tt.wantStatus) {
				if got := res.Header.Get("Content-Length"); got != strconv.Itoa(len(tt.wantBody)) && tt.wantHeaders["Content-Length"] == "" {
					t.Errorf("Content-Length = %q, want %d", got, len(tt.wantBody))
				}
			}
		})
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func TestApplyHandBuiltPlan(t *testing.T) {
	// Plans can be constructed without an Injector; Apply must cope with rules
	// that were never defaulted or validated.
	t.Run("error with zero status", func(t *testing.T) {
		w := httptest.NewRecorder()
		p := &Plan{Terminal: &model.FaultRule{Name: "hand", Type: model.FaultError, Body: `{"x":1}`}}
		if !p.Apply(w, newReq("GET", "/x")) {
			t.Fatal("not handled")
		}
		if w.Code != 503 || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("X-Mirage-Fault") != "hand" {
			t.Errorf("response = %d %v", w.Code, w.Header())
		}
	})
	t.Run("error with out-of-range status", func(t *testing.T) {
		w := httptest.NewRecorder()
		p := &Plan{Terminal: &model.FaultRule{Name: "hand", Type: model.FaultError, Status: 1000}}
		p.Apply(w, newReq("GET", "/x"))
		if w.Code != 503 {
			t.Errorf("status = %d, want 503", w.Code)
		}
	})
	t.Run("terminal swapped after Decide", func(t *testing.T) {
		inj := newInjector(t, errRule("a", func(r *model.FaultRule) { r.Status = 500; r.Body = "original" }))
		r := newReq("GET", "/x")
		p := inj.Decide("s", r)
		other := model.FaultRule{Name: "other", Type: model.FaultError, Status: 418, Body: "teapot"}
		cp := *p
		cp.Terminal = &other
		w := httptest.NewRecorder()
		cp.Apply(w, r)
		if w.Code != 418 || w.Body.String() != "teapot" {
			t.Errorf("response = %d %q, want the swapped-in rule", w.Code, w.Body.String())
		}
	})
	t.Run("nil request", func(t *testing.T) {
		w := httptest.NewRecorder()
		p := &Plan{Delay: time.Millisecond, Terminal: &model.FaultRule{Name: "e", Type: model.FaultError, Status: 500}}
		if !p.Apply(w, nil) || w.Code != 500 {
			t.Errorf("response = %d", w.Code)
		}
	})
	t.Run("unknown terminal type", func(t *testing.T) {
		p := &Plan{Terminal: &model.FaultRule{Name: "x", Type: "mystery"}}
		if p.Apply(httptest.NewRecorder(), newReq("GET", "/x")) {
			t.Error("unknown terminal type must not report handled")
		}
	})
}

// mustPanicAbort runs f and fails unless it panics with http.ErrAbortHandler.
func mustPanicAbort(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		t.Helper()
		v := recover()
		if v == nil {
			t.Fatal("no panic, want http.ErrAbortHandler")
		}
		if err, ok := v.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("panic value = %#v, want http.ErrAbortHandler", v)
		}
	}()
	f()
}

func TestApplyAbortPanics(t *testing.T) {
	inj := newInjector(t, abortRule("a"), latencyRule("l", 30*time.Millisecond, 0, order(1)))
	r := newReq("GET", "/x")
	p := inj.Decide("s", r)
	w := httptest.NewRecorder()
	start := time.Now()
	mustPanicAbort(t, func() { p.Apply(w, r) })
	if el := time.Since(start); el < 30*time.Millisecond {
		t.Errorf("abort fired after %v, want after the 30ms delay", el)
	}
	if w.Body.Len() != 0 || w.Flushed || len(w.Header()) != 0 {
		t.Error("abort wrote a response")
	}
}

func TestApplyTimeoutHoldsThenPanics(t *testing.T) {
	inj := newInjector(t, timeoutRule("t", 80*time.Millisecond))
	r := newReq("GET", "/x")
	p := inj.Decide("s", r)
	start := time.Now()
	mustPanicAbort(t, func() { p.Apply(httptest.NewRecorder(), r) })
	if el := time.Since(start); el < 80*time.Millisecond {
		t.Errorf("timeout released after %v, want at least 80ms", el)
	}
}

func TestApplyTimeoutEndsWhenClientGoesAway(t *testing.T) {
	inj := newInjector(t, timeoutRule("t", time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	r := newReq("GET", "/x").WithContext(ctx)
	p := inj.Decide("s", r)
	time.AfterFunc(40*time.Millisecond, cancel)
	defer cancel()
	start := time.Now()
	mustPanicAbort(t, func() { p.Apply(httptest.NewRecorder(), r) })
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("timeout took %v to notice the client leaving", el)
	}
}

func TestApplyTimeoutDefaultsToSixtySecondsWhenHandBuilt(t *testing.T) {
	// A hand-built timeout plan with no delay must not return instantly. Cancel
	// the context to prove it is waiting (and to keep the test fast).
	ctx, cancel := context.WithCancel(context.Background())
	r := newReq("GET", "/x").WithContext(ctx)
	p := &Plan{Terminal: &model.FaultRule{Name: "t", Type: model.FaultTimeout}}
	released := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		defer func() { _ = recover(); released <- time.Since(start) }()
		p.Apply(httptest.NewRecorder(), r)
	}()
	select {
	case d := <-released:
		t.Fatalf("hand-built timeout returned after %v, want it to hold", d)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout did not release on cancellation")
	}
}

// faultServer starts a server whose handler follows the proxy's documented
// request path: Decide, Apply, then a normal response. Panics propagate, as
// they must through real middleware. The returned buffer captures the
// server's error log, which must stay empty for http.ErrAbortHandler.
func faultServer(t *testing.T, inj *Injector, exit chan<- struct{}, mutate func(*httptest.Server)) (*httptest.Server, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if exit != nil {
			defer func() {
				select {
				case exit <- struct{}{}:
				default:
				}
			}()
		}
		if inj.Decide("svc", r).Apply(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ErrorLog = log.New(logs, "", 0)
	if mutate != nil {
		mutate(srv)
	}
	if srv.TLS != nil || srv.EnableHTTP2 {
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv, logs
}

func newClient(srv *httptest.Server) *http.Client {
	c := srv.Client()
	if tr, ok := c.Transport.(*http.Transport); ok {
		tr = tr.Clone()
		tr.DisableKeepAlives = true
		c.Transport = tr
	}
	return c
}

func isConnectionError(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) ||
		strings.Contains(err.Error(), "stream error") || strings.Contains(err.Error(), "connection reset")
}

func TestServerAbortClosesConnectionWithoutResponse(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		name := "http1"
		if h2 {
			name = "http2"
		}
		t.Run(name, func(t *testing.T) {
			inj := newInjector(t, abortRule("a"))
			var proto atomic.Int64
			srv, logs := faultServer(t, inj, nil, func(s *httptest.Server) {
				s.EnableHTTP2 = h2
				wrapped := s.Config.Handler
				s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					proto.Store(int64(r.ProtoMajor))
					wrapped.ServeHTTP(w, r)
				})
			})
			resp, err := newClient(srv).Get(srv.URL + "/x")
			if err == nil {
				resp.Body.Close()
				t.Fatalf("client received a response (%s), want a connection error", resp.Status)
			}
			if !isConnectionError(err) {
				t.Errorf("error = %v, want EOF / reset / stream error", err)
			}
			wantProto := int64(1)
			if h2 {
				wantProto = 2
			}
			if got := proto.Load(); got != wantProto {
				t.Errorf("handler saw HTTP/%d, want HTTP/%d", got, wantProto)
			}
			// net/http must treat the panic as a deliberate abort: no log output.
			time.Sleep(20 * time.Millisecond)
			if got := logs.String(); got != "" {
				t.Errorf("server logged %q for an injected abort", got)
			}
		})
	}
}

func TestServerAbortAfterLatency(t *testing.T) {
	inj := newInjector(t, latencyRule("l", 80*time.Millisecond, 0), abortRule("a", order(1)))
	srv, _ := faultServer(t, inj, nil, nil)
	start := time.Now()
	resp, err := newClient(srv).Get(srv.URL + "/x")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("got a response %s", resp.Status)
	}
	if el := time.Since(start); el < 80*time.Millisecond {
		t.Errorf("connection dropped after %v, want after the 80ms latency", el)
	}
}

func TestServerTimeoutHoldsThenDropsConnection(t *testing.T) {
	inj := newInjector(t, timeoutRule("t", 150*time.Millisecond))
	srv, logs := faultServer(t, inj, nil, nil)
	start := time.Now()
	resp, err := newClient(srv).Get(srv.URL + "/x")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("got a response %s", resp.Status)
	}
	if !isConnectionError(err) {
		t.Errorf("error = %v, want a connection error", err)
	}
	if el := time.Since(start); el < 150*time.Millisecond {
		t.Errorf("connection dropped after %v, want after the 150ms hold", el)
	}
	time.Sleep(20 * time.Millisecond)
	if got := logs.String(); got != "" {
		t.Errorf("server logged %q for an injected timeout", got)
	}
}

func TestServerTimeoutReleasesWhenClientGivesUp(t *testing.T) {
	inj := newInjector(t, timeoutRule("t", time.Hour))
	exit := make(chan struct{}, 1)
	srv, _ := faultServer(t, inj, exit, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/x", nil)
	resp, err := newClient(srv).Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("got a response %s", resp.Status)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want deadline exceeded", err)
	}
	select {
	case <-exit:
	case <-time.After(5 * time.Second):
		t.Fatal("server handler is still holding the request after the client disconnected")
	}
}

func TestServerLatencyThenOK(t *testing.T) {
	inj := newInjector(t, latencyRule("l", 100*time.Millisecond, 0))
	srv, _ := faultServer(t, inj, nil, nil)
	start := time.Now()
	resp, err := srv.Client().Get(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if el := time.Since(start); el < 100*time.Millisecond {
		t.Errorf("response after %v, want at least 100ms", el)
	}
	if string(body) != "ok" || resp.StatusCode != 200 {
		t.Errorf("response = %d %q", resp.StatusCode, body)
	}
}

func TestServerErrorResponse(t *testing.T) {
	inj := newInjector(t, errRule("e", name("checkout 503"), func(r *model.FaultRule) {
		r.Body = `{"error":"overloaded"}`
		r.ResponseHeader = map[string]string{"Retry-After": "1"}
	}))
	srv, _ := faultServer(t, inj, nil, nil)

	t.Run("GET", func(t *testing.T) {
		resp, err := srv.Client().Get(srv.URL + "/x")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 503 || string(body) != `{"error":"overloaded"}` {
			t.Errorf("response = %d %q", resp.StatusCode, body)
		}
		if resp.Header.Get("Content-Type") != "application/json" ||
			resp.Header.Get("Retry-After") != "1" ||
			resp.Header.Get("X-Mirage-Fault") != "checkout 503" {
			t.Errorf("headers = %v", resp.Header)
		}
		if resp.ContentLength != int64(len(body)) {
			t.Errorf("ContentLength = %d, want %d", resp.ContentLength, len(body))
		}
	})
	t.Run("HEAD", func(t *testing.T) {
		resp, err := srv.Client().Head(srv.URL + "/x")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 503 || resp.ContentLength != int64(len(`{"error":"overloaded"}`)) {
			t.Errorf("HEAD response = %d, content length %d", resp.StatusCode, resp.ContentLength)
		}
	})
}
