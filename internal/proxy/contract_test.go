package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// serviceController mirrors api.ServiceController, which proxy must not
// import; *Manager has to satisfy it.
type serviceController interface {
	Apply(ctx context.Context, svc model.Service) error
	Remove(ctx context.Context, id string) error
	Reload(ctx context.Context, id string) error
	ProfileChanged(ctx context.Context, profileID string) error
	Status(id string) model.ServiceStatus
	Statuses() []model.ServiceStatus
}

var _ serviceController = (*Manager)(nil)

func TestServerAndTransportSettings(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) {
		s.Timeout = model.Duration(7 * time.Second)
		s.InsecureSkipVerify = true
	})
	e.start(svc)

	s := e.mgr.get(svc.ID)
	srv := s.lis.srv
	if srv.ReadHeaderTimeout != 10*time.Second || srv.IdleTimeout != 120*time.Second || srv.MaxHeaderBytes != 1<<20 {
		t.Errorf("server: ReadHeaderTimeout %v IdleTimeout %v MaxHeaderBytes %d", srv.ReadHeaderTimeout, srv.IdleTimeout, srv.MaxHeaderBytes)
	}
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Errorf("read/write timeouts (%v/%v) would cut streaming responses", srv.ReadTimeout, srv.WriteTimeout)
	}
	if !srv.Protocols.UnencryptedHTTP2() || !srv.Protocols.HTTP1() || srv.Protocols.HTTP2() {
		t.Errorf("plaintext protocols: %v", srv.Protocols)
	}
	tr := s.state.Load().tr
	if tr.MaxIdleConnsPerHost != 1024 || tr.IdleConnTimeout != 90*time.Second || !tr.ForceAttemptHTTP2 ||
		!tr.DisableCompression || tr.ResponseHeaderTimeout != 7*time.Second || !tr.TLSClientConfig.InsecureSkipVerify || tr.Proxy != nil {
		t.Errorf("transport: %+v", tr)
	}

	tlsSvc := e.service(up.URL, model.ModePassthrough, func(s *model.Service) { s.TLS = &model.TLSConfig{SelfSigned: true} })
	if err := e.mgr.Apply(context.Background(), tlsSvc); err != nil {
		t.Fatal(err)
	}
	if p := e.mgr.get(tlsSvc.ID).lis.srv.Protocols; !p.HTTP1() || !p.HTTP2() || p.UnencryptedHTTP2() {
		t.Errorf("TLS protocols: %v", p)
	}
}

func TestApplyContextBoundsOnlyTheCall(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	ctx, cancel := context.WithCancel(context.Background())
	if err := e.mgr.Apply(ctx, svc); err != nil {
		t.Fatal(err)
	}
	cancel() // like an API handler returning
	time.Sleep(20 * time.Millisecond)
	base := e.url(svc.ID)
	if status, _, _ := do(t, newClient(t), "GET", base+"/after-cancel", nil); status != 200 {
		t.Errorf("the service died with the Apply context: %d", status)
	}
	e.waitStored(svc.RecordingID, 1)

	// An already-cancelled context fails the call that needs the store.
	dead, cancelDead := context.WithCancel(context.Background())
	cancelDead()
	other := e.service(up.URL, model.ModeRecord)
	if err := e.mgr.Apply(dead, other); err == nil {
		t.Error("Apply with a cancelled context succeeded")
	}
}

func TestStreamingOverCleartextHTTP2(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: two\n\n")
	}))
	defer up.Close()
	defer close(release)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)

	var protos http.Protocols
	protos.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: &protos}
	defer tr.CloseIdleConnections()
	res, err := (&http.Client{Transport: tr}).Get(base + "/h2-stream")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.ProtoMajor != 2 {
		t.Fatalf("proto %s", res.Proto)
	}
	first := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(res.Body).ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if line != "data: one\n" {
			t.Errorf("first line = %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no data over HTTP/2 while the upstream is still open: the response is buffered")
	}
}

func TestFaultAbortResetsHTTP2Streams(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	setRules(t, e, model.FaultRule{Type: model.FaultAbort, PathPattern: "/abort"})

	var protos http.Protocols
	protos.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: &protos}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	if res, err := c.Get(base + "/abort"); err == nil {
		res.Body.Close()
		t.Fatalf("got %d; the stream must be reset", res.StatusCode)
	}
	// The connection is still good for other streams.
	res, err := c.Get(base + "/fine")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("follow-up request: %v %v", res, err)
	}
	res.Body.Close()
	snap := e.waitIdle(svc.ID)
	if snap.Requests != 2 || snap.Errors != 1 || snap.InFlight != 0 {
		t.Errorf("metrics: %+v", snap)
	}
}

// settledGoroutines returns the goroutine count once it stops falling.
func settledGoroutines() int {
	n := runtime.NumGoroutine()
	for i := 0; i < 40; i++ {
		runtime.GC()
		time.Sleep(25 * time.Millisecond)
		m := runtime.NumGoroutine()
		if m >= n {
			return m
		}
		n = m
	}
	return n
}

func TestNoGoroutineLeakAfterShutdown(t *testing.T) {
	base := settledGoroutines()
	func() {
		up := newEcho(t)
		e := newEnv(t)
		hold := newClient(t)

		pass := e.service(up.URL, model.ModePassthrough)
		rec := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.AnonymizeCapture = true })
		virt := e.service("", model.ModeVirtualize)
		hyb := e.service(up.URL, model.ModeHybrid)
		secure := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.TLS = &model.TLSConfig{SelfSigned: true} })
		seedExchange(t, e, virt.RecordingID, "GET", "/v", 200, "{}")
		for _, svc := range []model.Service{pass, rec, virt, hyb, secure} {
			e.start(svc)
		}
		secureClient := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsInsecure()}}
		for i := 0; i < 20; i++ {
			for _, svc := range []model.Service{pass, rec, virt, hyb} {
				do(t, hold, "GET", e.url(svc.ID)+"/v", nil)
			}
			do(t, secureClient, "GET", "https://"+e.mgr.Status(secure.ID).Addr+"/x", nil)
		}
		// Churn: reconfigure, rebind, disable, remove.
		rec.Timeout = model.Duration(9 * time.Second)
		e.mgr.Apply(context.Background(), rec)
		e.mgr.Reload(context.Background(), virt.ID)
		off := hyb
		off.Enabled = false
		e.mgr.Apply(context.Background(), off)
		e.mgr.Remove(context.Background(), pass.ID)

		hold.CloseIdleConnections()
		secureClient.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := e.mgr.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
		up.Close()
		_ = e.st.Close()
	}()
	if got := settledGoroutines(); got > base+3 {
		buf := make([]byte, 1<<16)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("goroutines: %d before, %d after Shutdown\n%s", base, got, buf)
	}
}
