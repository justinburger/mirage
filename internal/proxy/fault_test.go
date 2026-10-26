package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/fault"
	"github.com/justinburger/mirage/internal/model"
)

func setRules(t *testing.T, e *env, rules ...model.FaultRule) {
	t.Helper()
	for i := range rules {
		if rules[i].ID == "" {
			rules[i].ID = "flt_" + string(rune('a'+i))
		}
		if rules[i].Name == "" {
			rules[i].Name = rules[i].ID
		}
		rules[i].Enabled = true
		if rules[i].Probability == 0 {
			rules[i].Probability = 1
		}
	}
	if err := e.inj.SetRules(rules); err != nil {
		t.Fatalf("SetRules: %v", err)
	}
}

func TestFaultErrorAnswersWithoutTouchingUpstream(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	setRules(t, e, model.FaultRule{
		Type: model.FaultError, Status: 503, Body: `{"error":"overloaded"}`,
		ResponseHeader: map[string]string{"Retry-After": "1"}, PathPattern: "/checkout/**",
	})
	c := newClient(t)

	status, hdr, body := do(t, c, "POST", base+"/checkout/pay", strings.NewReader("x"))
	if status != 503 || bodyOf(body) != `{"error":"overloaded"}` || hdr.Get("Retry-After") != "1" || hdr.Get(fault.FaultHeader) == "" {
		t.Fatalf("status %d hdr %v body %s", status, hdr, body)
	}
	// A non-matching path is proxied (and captured) as usual.
	if status, _, _ := do(t, c, "GET", base+"/other", nil); status != 200 {
		t.Fatalf("unfaulted request: %d", status)
	}
	if up.hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want only the unfaulted request", up.hits.Load())
	}
	exs := e.waitStored(svc.RecordingID, 1)
	if len(exs) != 1 || exs[0].Request.Path != "/other" {
		t.Errorf("injected errors must not be captured: %d exchanges", len(exs))
	}
	snap := e.waitIdle(svc.ID)
	if snap.Requests != 2 || snap.Errors != 1 || snap.StatusClasses["5xx"] != 1 {
		t.Errorf("metrics: %d requests, %d errors, %v", snap.Requests, snap.Errors, snap.StatusClasses)
	}
}

func TestFaultLatencyDelaysClientButNotCapturedTiming(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	setRules(t, e, model.FaultRule{Type: model.FaultLatency, Delay: model.Duration(200 * time.Millisecond)})

	start := time.Now()
	status, _, _ := do(t, newClient(t), "GET", base+"/slow", nil)
	elapsed := time.Since(start)
	if status != 200 || elapsed < 190*time.Millisecond {
		t.Fatalf("status %d after %v: the fault must delay the request", status, elapsed)
	}
	ex := e.waitStored(svc.RecordingID, 1)[0]
	if ex.Duration.D() >= 150*time.Millisecond {
		t.Errorf("captured Duration %v includes the injected delay; it is measured from the upstream hand-off", ex.Duration)
	}
	snap := e.waitIdle(svc.ID)
	if snap.Latency.Max < 190 {
		t.Errorf("metrics latency max %.1fms must include the injected delay", snap.Latency.Max)
	}
}

func TestFaultAbortDropsConnectionAndFinishesBookkeeping(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	setRules(t, e, model.FaultRule{Type: model.FaultAbort})

	res, err := newClient(t).Get(base + "/x")
	if err == nil {
		res.Body.Close()
		t.Fatalf("got a response (%d); the connection must be dropped", res.StatusCode)
	}
	snap := e.waitIdle(svc.ID)
	if snap.Requests != 1 || snap.Errors != 1 || snap.StatusClasses["err"] != 1 {
		t.Errorf("aborted request must be observed with status 0 as an error: %d requests, %d errors, %v", snap.Requests, snap.Errors, snap.StatusClasses)
	}
	if up.hits.Load() != 0 {
		t.Error("upstream was contacted")
	}
	time.Sleep(30 * time.Millisecond)
	if got := e.captured(svc.RecordingID); len(got) != 0 {
		t.Errorf("stored %d exchanges for an aborted request", len(got))
	}
	if st := e.mgr.Status(svc.ID); st.CaptureSkipped != 0 || st.Captured != 0 {
		t.Errorf("status = %+v", st)
	}
}

func TestFaultTimeoutHoldsThenAborts(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModePassthrough)
	base := e.start(svc)
	setRules(t, e, model.FaultRule{Type: model.FaultTimeout, Delay: model.Duration(150 * time.Millisecond)})

	start := time.Now()
	res, err := newClient(t).Get(base + "/hang")
	if err == nil {
		res.Body.Close()
		t.Fatal("expected the connection to be dropped")
	}
	if d := time.Since(start); d < 140*time.Millisecond {
		t.Errorf("dropped after %v, want the 150ms hold", d)
	}
	if snap := e.waitIdle(svc.ID); snap.Errors != 1 {
		t.Errorf("errors = %d", snap.Errors)
	}
}

func TestFaultThrottlePacesTheResponse(t *testing.T) {
	payload := strings.Repeat("x", 10_000)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(payload))
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModePassthrough)
	base := e.start(svc)
	setRules(t, e, model.FaultRule{Type: model.FaultThrottle, BytesPerSecond: 20_000})

	start := time.Now()
	status, _, body := do(t, newClient(t), "GET", base+"/big", nil)
	if status != 200 || string(body) != payload {
		t.Fatalf("status %d, body intact: %v", status, string(body) == payload)
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Errorf("10 KB at 20 KB/s took only %v", d)
	}
}

func TestFaultAbortPanicPropagatesThroughHandler(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	e.start(svc)
	setRules(t, e, model.FaultRule{Type: model.FaultAbort})

	h, ok := e.mgr.Handler(svc.ID)
	if !ok {
		t.Fatal("no handler")
	}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	}()
	if recovered != http.ErrAbortHandler {
		t.Fatalf("recovered %v, want http.ErrAbortHandler: the handler must never swallow the abort", recovered)
	}
	snap := e.snapshot(svc.ID)
	if snap.InFlight != 0 || snap.Requests != 1 || snap.StatusClasses["err"] != 1 {
		t.Errorf("after the panic: in-flight %d, requests %d, classes %v", snap.InFlight, snap.Requests, snap.StatusClasses)
	}
}

func TestFaultsApplyInVirtualizeMode(t *testing.T) {
	e := newEnv(t)
	svc := e.service("", model.ModeVirtualize)
	seedExchange(t, e, svc.RecordingID, "GET", "/hello", 200, `{"hello":"world"}`)
	base := e.start(svc)
	c := newClient(t)

	if status, _, body := do(t, c, "GET", base+"/hello", nil); status != 200 || bodyOf(body) != `{"hello":"world"}` {
		t.Fatalf("status %d body %s", status, body)
	}
	setRules(t, e, model.FaultRule{Type: model.FaultError, Status: 500, Body: "boom"})
	if status, hdr, _ := do(t, c, "GET", base+"/hello", nil); status != 500 || hdr.Get(fault.FaultHeader) == "" {
		t.Errorf("fault not applied before the virtual engine: %d %v", status, hdr)
	}
	if hits := e.virts.last().hits.Load(); hits != 1 {
		t.Errorf("virtual hits = %d, want 1 (the faulted request never reached the engine)", hits)
	}
}

func TestNoFaultInjectorIsFine(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t, func(d *Deps) { d.Faults = nil })
	svc := e.service(up.URL, model.ModePassthrough)
	base := e.start(svc)
	if status, _, _ := do(t, newClient(t), "GET", base+"/", nil); status != 200 {
		t.Errorf("status %d", status)
	}
}
