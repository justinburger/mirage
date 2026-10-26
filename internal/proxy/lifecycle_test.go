package proxy

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
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

	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
)

// freeAddr returns a loopback address with a currently free port.
func freeAddr(t testing.TB) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestModeSwitchKeepsListenerAndServesNewMode(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModePassthrough)
	svc.RecordingID = e.recording()
	base := e.start(svc)
	addr := e.mgr.Status(svc.ID).Addr
	c := newClient(t)

	do(t, c, "GET", base+"/p", nil)
	if st := e.mgr.Status(svc.ID); st.Captured != 0 || st.Mode != model.ModePassthrough {
		t.Fatalf("passthrough: %+v", st)
	}

	svc.Mode = model.ModeRecord
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	if got := e.mgr.Status(svc.ID); got.Addr != addr || got.Mode != model.ModeRecord {
		t.Errorf("listener changed on a mode switch: %+v (was %s)", got, addr)
	}
	do(t, c, "GET", base+"/r", nil) // same keep-alive connection
	e.waitStored(svc.RecordingID, 1)

	svc.Mode = model.ModeVirtualize
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	hits := up.hits.Load()
	if status, _, _ := do(t, c, "GET", base+"/r", nil); status != 200 {
		t.Errorf("virtualized /r: %d", status)
	}
	if status, _, _ := do(t, c, "GET", base+"/unknown", nil); status != 404 {
		t.Errorf("virtualized miss: %d", status)
	}
	if up.hits.Load() != hits {
		t.Error("virtualize mode contacted the upstream")
	}
}

func TestApplyChangesUpstreamLive(t *testing.T) {
	a, b := newEcho(t), newEcho(t)
	e := newEnv(t)
	svc := e.service(a.URL, model.ModePassthrough)
	base := e.start(svc)
	c := newClient(t)
	do(t, c, "GET", base+"/x", nil)
	if a.hits.Load() != 1 || b.hits.Load() != 0 {
		t.Fatalf("hits %d/%d", a.hits.Load(), b.hits.Load())
	}
	svc.Upstream = b.URL + "/prefix"
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	do(t, c, "GET", base+"/x", nil)
	if a.hits.Load() != 1 || b.hits.Load() != 1 || b.lastRequest().URI != "/prefix/x" {
		t.Errorf("after switching upstream: hits %d/%d, uri %q", a.hits.Load(), b.hits.Load(), b.lastRequest().URI)
	}
}

func TestApplyChangesRecordingAndAnonymizationLive(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	firstRec := svc.RecordingID
	base := e.start(svc)
	c := newClient(t)

	do(t, c, "GET", base+"/one", nil, "Authorization", "Bearer secret-token-1")
	e.waitStored(firstRec, 1)

	svc.RecordingID = e.recording()
	svc.AnonymizeCapture = true
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	do(t, c, "GET", base+"/two", nil, "Authorization", "Bearer secret-token-2")
	second := e.waitStored(svc.RecordingID, 1)[0]
	if strings.Contains(fmt.Sprint(second.Request.Header), "secret-token-2") || !second.Anonymized {
		t.Errorf("second capture not anonymized: %v", second.Request.Header)
	}
	first := e.captured(firstRec)
	if len(first) != 1 || first[0].Request.Path != "/one" || first[0].Anonymized || !strings.Contains(fmt.Sprint(first[0].Request.Header), "secret-token-1") {
		t.Errorf("first recording changed: %+v", first)
	}
}

func TestDisableStopsListenerKeepsStatusAndReenableWorks(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	addr := e.mgr.Status(svc.ID).Addr
	do(t, newClient(t), "GET", base+"/a", nil)
	e.waitStored(svc.RecordingID, 1)

	off := svc
	off.Enabled = false
	if err := e.mgr.Apply(context.Background(), off); err != nil {
		t.Fatal(err)
	}
	st := e.mgr.Status(svc.ID)
	if st.Running || st.Addr != "" || st.Mode != model.ModeRecord || st.StartedAt != nil {
		t.Errorf("status after disable: %+v", st)
	}
	if _, ok := e.mgr.Handler(svc.ID); ok {
		t.Error("Handler returned a stopped service")
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Error("the listener is still accepting connections")
	}
	if got := len(e.mgr.Statuses()); got != 1 {
		t.Errorf("Statuses has %d entries; a disabled service keeps its entry", got)
	}

	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	st = e.mgr.Status(svc.ID)
	if !st.Running || st.Captured != 0 || st.StartedAt == nil {
		t.Errorf("status after re-enable: %+v (counters count since start)", st)
	}
	do(t, newClient(t), "GET", e.url(svc.ID)+"/b", nil)
	e.waitStored(svc.RecordingID, 2)
}

func TestRemoveForgetsServiceAndMetrics(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModePassthrough)
	base := e.start(svc)
	do(t, newClient(t), "GET", base+"/", nil)
	name := metrics.ServiceCollectorName(svc.ID)
	if _, ok := e.reg.Get(name); !ok {
		t.Fatal("no collector while running")
	}

	if err := e.mgr.Remove(context.Background(), svc.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.reg.Get(name); ok {
		t.Error("collector survived Remove")
	}
	if st := e.mgr.Status(svc.ID); st.Running || st.ServiceID != svc.ID {
		t.Errorf("status of removed service: %+v", st)
	}
	if len(e.mgr.Statuses()) != 0 {
		t.Errorf("Statuses = %v", e.mgr.Statuses())
	}
	if err := e.mgr.Remove(context.Background(), svc.ID); err != nil {
		t.Errorf("removing twice: %v", err)
	}
	if err := e.mgr.Remove(context.Background(), "svc_never_existed"); err != nil {
		t.Errorf("removing an unknown service: %v", err)
	}
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatalf("Apply after Remove: %v", err)
	}
	if status, _, _ := do(t, newClient(t), "GET", e.url(svc.ID)+"/", nil); status != 200 {
		t.Errorf("status %d", status)
	}
}

func TestStatusesAreSortedAndComplete(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	for i := 0; i < 4; i++ {
		svc := e.service(up.URL, model.ModePassthrough)
		if i == 2 {
			svc.Enabled = false
		}
		if err := e.mgr.Apply(context.Background(), svc); err != nil {
			t.Fatal(err)
		}
	}
	sts := e.mgr.Statuses()
	if len(sts) != 4 {
		t.Fatalf("%d statuses", len(sts))
	}
	for i := 1; i < len(sts); i++ {
		if sts[i-1].ServiceID >= sts[i].ServiceID {
			t.Errorf("not sorted: %s before %s", sts[i-1].ServiceID, sts[i].ServiceID)
		}
	}
	running := 0
	for _, s := range sts {
		if s.Running {
			running++
		}
	}
	if running != 3 {
		t.Errorf("%d running, want 3", running)
	}
	if st := e.mgr.Status("svc_unknown"); st.Running || st.ServiceID != "svc_unknown" {
		t.Errorf("unknown service status: %+v", st)
	}
}

func TestListenAddrChangeMovesTheListenerGracefully(t *testing.T) {
	slow := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-slow
		}
		io.WriteString(w, "done")
	}))
	defer up.Close()
	e := newEnv(t)
	oldAddr, newAddr := freeAddr(t), freeAddr(t)
	svc := e.service(up.URL, model.ModePassthrough, func(s *model.Service) { s.ListenAddr = oldAddr })
	e.start(svc)

	// A request in flight on the old listener must still complete.
	result := make(chan string, 1)
	go func() {
		_, _, b := do(t, newClient(t), "GET", "http://"+oldAddr+"/slow", nil)
		result <- string(b)
	}()
	waitFor(t, "slow request to arrive", func() bool { return e.snapshot(svc.ID).InFlight == 1 })

	svc.ListenAddr = newAddr
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	if got := e.mgr.Status(svc.ID).Addr; got != newAddr {
		t.Errorf("Addr = %q, want %q", got, newAddr)
	}
	if status, _, _ := do(t, newClient(t), "GET", "http://"+newAddr+"/fast", nil); status != 200 {
		t.Errorf("new listener: %d", status)
	}
	if conn, err := net.DialTimeout("tcp", oldAddr, time.Second); err == nil {
		conn.Close()
		t.Error("the old listener still accepts connections")
	}
	close(slow)
	select {
	case got := <-result:
		if got != "done" {
			t.Errorf("in-flight request: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight request on the old listener never completed")
	}
}

func TestBindErrors(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	// A fresh service on a taken port: error, not running, error in status.
	bad := e.service(up.URL, model.ModePassthrough, func(s *model.Service) { s.ListenAddr = occupied.Addr().String() })
	if err := e.mgr.Apply(context.Background(), bad); err == nil {
		t.Fatal("Apply on an occupied port succeeded")
	}
	if st := e.mgr.Status(bad.ID); st.Running || !strings.Contains(st.Error, "listen") {
		t.Errorf("status = %+v", st)
	}

	// A running service asked to move onto a taken port keeps serving.
	good := e.service(up.URL, model.ModePassthrough)
	base := e.start(good)
	moved := good
	moved.ListenAddr = occupied.Addr().String()
	if err := e.mgr.Apply(context.Background(), moved); err == nil {
		t.Fatal("moving onto an occupied port succeeded")
	}
	st := e.mgr.Status(good.ID)
	if !st.Running || st.Error == "" {
		t.Errorf("status = %+v: the old listener must keep running", st)
	}
	if status, _, _ := do(t, newClient(t), "GET", base+"/", nil); status != 200 {
		t.Errorf("old listener broken: %d", status)
	}
	// Retry once the port is free.
	occupied.Close()
	if err := e.mgr.Apply(context.Background(), moved); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if st := e.mgr.Status(good.ID); st.Error != "" || st.Addr != moved.ListenAddr {
		t.Errorf("status after retry = %+v", st)
	}
}

func TestTLSChangeRebindsTheSamePort(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	addr := freeAddr(t)
	svc := e.service(up.URL, model.ModePassthrough, func(s *model.Service) { s.ListenAddr = addr })
	e.start(svc)
	if status, _, _ := do(t, newClient(t), "GET", "http://"+addr+"/", nil); status != 200 {
		t.Fatal(status)
	}

	svc.TLS = &model.TLSConfig{SelfSigned: true}
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatalf("enabling TLS on the same port: %v", err)
	}
	tlsClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if status, _, _ := do(t, tlsClient, "GET", "https://"+addr+"/", nil); status != 200 {
		t.Errorf("https: %d", status)
	}
	if res, err := newClient(t).Get("http://" + addr + "/"); err == nil {
		res.Body.Close()
		if res.StatusCode == 200 {
			t.Error("plain HTTP still works on a TLS listener")
		}
	}

	svc.TLS = nil
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatalf("disabling TLS: %v", err)
	}
	if status, _, _ := do(t, newClient(t), "GET", "http://"+addr+"/", nil); status != 200 {
		t.Errorf("plain after TLS off: %d", status)
	}
}

func TestSelfSignedTLSServesHTTP1AndHTTP2(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.TLS = &model.TLSConfig{SelfSigned: true} })
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	addr := e.mgr.Status(svc.ID).Addr
	base := "https://" + addr

	h2 := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		ForceAttemptHTTP2: true,
	}}
	res, err := h2.Get(base + "/h2")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.ProtoMajor != 2 || res.TLS == nil {
		t.Errorf("proto %s, tls %v: want HTTP/2 over TLS", res.Proto, res.TLS != nil)
	}
	h1 := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		TLSNextProto:    map[string]func(string, *tls.Conn) http.RoundTripper{},
	}}
	res, err = h1.Get(base + "/h1")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.ProtoMajor != 1 {
		t.Errorf("proto %s, want HTTP/1.1", res.Proto)
	}

	exs := e.waitStored(svc.RecordingID, 2)
	protos := map[string]string{}
	for _, ex := range exs {
		if ex.Request.Scheme != "https" {
			t.Errorf("%s: scheme %q", ex.Request.Path, ex.Request.Scheme)
		}
		protos[ex.Request.Path] = ex.Request.Proto
	}
	if protos["/h2"] != "HTTP/2.0" || protos["/h1"] != "HTTP/1.1" {
		t.Errorf("captured protos = %v", protos)
	}
	if got := up.lastRequest().Header.Get("X-Forwarded-Proto"); got != "https" {
		t.Errorf("X-Forwarded-Proto = %q", got)
	}

	// The certificate covers localhost, 127.0.0.1 and ::1 and uses ECDSA P-256.
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cert := conn.ConnectionState().PeerCertificates[0]
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Errorf("public key = %T", cert.PublicKey)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	for _, name := range []string{"localhost", "127.0.0.1", "::1"} {
		if err := cert.VerifyHostname(name); err != nil {
			t.Errorf("certificate does not cover %s: %v", name, err)
		}
	}
	if time.Now().Before(cert.NotBefore) || time.Now().After(cert.NotAfter) {
		t.Errorf("certificate validity %v..%v", cert.NotBefore, cert.NotAfter)
	}
}

func TestTLSCertFiles(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	cert, err := selfSignedCert("files.test")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	keyDER, _ := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)

	svc := e.service(up.URL, model.ModePassthrough, func(s *model.Service) {
		s.TLS = &model.TLSConfig{CertFile: certFile, KeyFile: keyFile}
	})
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	conn, err := tls.Dial("tcp", e.mgr.Status(svc.ID).Addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := conn.ConnectionState().PeerCertificates[0]; !got.Equal(cert.Leaf) {
		t.Error("the listener does not serve the configured certificate")
	}

	// Unreadable files fail Apply without starting anything.
	bad := e.service(up.URL, model.ModePassthrough, func(s *model.Service) {
		s.TLS = &model.TLSConfig{CertFile: certFile, KeyFile: filepath.Join(dir, "missing.pem")}
	})
	if err := e.mgr.Apply(context.Background(), bad); err == nil || e.mgr.Status(bad.ID).Running {
		t.Errorf("Apply with a missing key file: err %v running %v", err, e.mgr.Status(bad.ID).Running)
	}
}

func TestPlainListenerAcceptsCleartextHTTP2(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)

	var protos http.Protocols
	protos.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: &protos}
	defer tr.CloseIdleConnections()
	res, err := (&http.Client{Transport: tr}).Get(base + "/h2c")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.ProtoMajor != 2 {
		t.Errorf("proto = %s, want HTTP/2 without TLS", res.Proto)
	}
	if ex := e.waitStored(svc.RecordingID, 1)[0]; ex.Request.Proto != "HTTP/2.0" || ex.Request.Scheme != "http" {
		t.Errorf("captured proto %q scheme %q", ex.Request.Proto, ex.Request.Scheme)
	}
}

func TestUpstreamTLSVerification(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Proto)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	e := newEnv(t)

	strict := e.service(srv.URL, model.ModePassthrough)
	if status, _, _ := do(t, newClient(t), "GET", e.start(strict)+"/", nil); status != 502 {
		t.Errorf("an untrusted upstream certificate must fail: %d", status)
	}
	lax := e.service(srv.URL, model.ModePassthrough, func(s *model.Service) { s.InsecureSkipVerify = true })
	status, _, body := do(t, newClient(t), "GET", e.start(lax)+"/", nil)
	if status != 200 || string(body) != "HTTP/2.0" {
		t.Errorf("insecure_skip_verify: %d %q (the upstream hop should use HTTP/2)", status, body)
	}
}

func TestUpgradePassesThroughUncaptured(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		line, _ := brw.ReadString('\n')
		brw.WriteString("echo:" + line)
		brw.Flush()
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	e.start(svc)

	conn, err := net.Dial("tcp", e.mgr.Status(svc.ID).Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET /socket HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 101 {
		t.Fatalf("status %d", res.StatusCode)
	}
	fmt.Fprintf(conn, "ping\n")
	line, err := br.ReadString('\n')
	if err != nil || line != "echo:ping\n" {
		t.Fatalf("echo = %q, %v", line, err)
	}
	conn.Close()

	snap := e.waitIdle(svc.ID)
	if snap.Requests != 1 || snap.StatusClasses["1xx"] != 1 || snap.Errors != 0 {
		t.Errorf("metrics: %d requests, %v, %d errors", snap.Requests, snap.StatusClasses, snap.Errors)
	}
	time.Sleep(30 * time.Millisecond)
	if got := e.captured(svc.RecordingID); len(got) != 0 {
		t.Errorf("an upgraded connection was captured: %d", len(got))
	}
	if st := e.mgr.Status(svc.ID); st.CaptureSkipped != 0 || st.Captured != 0 {
		t.Errorf("status = %+v", st)
	}
}

func TestRetiredTransportClosesIdleConnections(t *testing.T) {
	var closed atomic.Int64
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	up.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			closed.Add(1)
		}
	}
	up.Start()
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModePassthrough)
	base := e.start(svc)
	do(t, newClient(t), "GET", base+"/", nil)
	if closed.Load() != 0 {
		t.Fatal("setup: connection closed early")
	}
	svc.Timeout = model.Duration(30 * time.Second) // a new transport is needed
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the old transport's idle connection to be closed", func() bool { return closed.Load() == 1 })
}

// ---- profile changes ---------------------------------------------------

func TestProfileChangedReloadsAnonymizer(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.AnonymizeCapture = true })
	other := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.AnonymizeCapture = true; s.ProfileID = "prof_other" })
	if _, err := e.st.PutProfile(context.Background(), model.AnonymizationProfile{ID: "prof_other", Name: "other"}); err != nil {
		t.Fatal(err)
	}
	base, otherBase := e.start(svc), e.start(other)
	c := newClient(t)
	payload := `{"nickname":"zed-the-unique"}`

	do(t, c, "POST", base+"/n", strings.NewReader(payload), "Content-Type", "application/json")
	if got := e.waitStored(svc.RecordingID, 1)[0]; !strings.Contains(string(got.Request.Body), "zed-the-unique") {
		t.Fatalf("the default profile should not touch nickname: %s", got.Request.Body)
	}

	p, err := e.st.GetProfile(context.Background(), model.DefaultProfileID)
	if err != nil {
		t.Fatal(err)
	}
	p.Rules = append(p.Rules, model.AnonymizationRule{
		Name: "nickname", Enabled: true, Target: model.TargetJSON, Selector: "$..nickname",
		Scope: model.ScopeBoth, Strategy: model.StrategyRedact,
	})
	if _, err := e.st.PutProfile(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := e.mgr.ProfileChanged(context.Background(), model.DefaultProfileID); err != nil {
		t.Fatal(err)
	}
	do(t, c, "POST", base+"/n", strings.NewReader(payload), "Content-Type", "application/json")
	do(t, c, "POST", otherBase+"/n", strings.NewReader(payload), "Content-Type", "application/json")
	exs := e.waitStored(svc.RecordingID, 2)
	if got := string(exs[1].Request.Body); !strings.Contains(got, "[REDACTED]") || strings.Contains(got, "zed-the-unique") {
		t.Errorf("after ProfileChanged: %s", got)
	}
	if got := e.waitStored(other.RecordingID, 1)[0]; !strings.Contains(string(got.Request.Body), "zed-the-unique") {
		t.Errorf("a service on another profile must not change: %s", got.Request.Body)
	}

	// An unusable profile keeps the previous anonymizer and reports the error.
	p.Rules = append(p.Rules, model.AnonymizationRule{
		Name: "broken", Enabled: true, Target: model.TargetJSON, Selector: "$[[[", Scope: model.ScopeBoth, Strategy: model.StrategyRedact,
	})
	if _, err := e.st.PutProfile(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := e.mgr.ProfileChanged(context.Background(), ""); err == nil {
		t.Error("ProfileChanged with a broken profile returned nil")
	}
	do(t, c, "POST", base+"/n", strings.NewReader(payload), "Content-Type", "application/json")
	exs = e.waitStored(svc.RecordingID, 3)
	if strings.Contains(string(exs[2].Request.Body), "zed-the-unique") {
		t.Error("a failed reload must keep the previous anonymizer, not fall back to raw capture")
	}
}

// noSecretStore returns profiles without their secret.
type noSecretStore struct{ *failingStore }

func (n noSecretStore) GetProfile(ctx context.Context, id string) (model.AnonymizationProfile, error) {
	p, err := n.failingStore.GetProfile(ctx, id)
	p.Secret = ""
	return p, err
}

func TestProfileWithoutSecretIsRefused(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t, func(d *Deps) { d.Store = noSecretStore{&failingStore{Store: d.Store}} })
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.AnonymizeCapture = true })
	err := e.mgr.Apply(context.Background(), svc)
	if err == nil || !strings.Contains(err.Error(), "secret") || e.mgr.Status(svc.ID).Running {
		t.Errorf("Apply = %v, running %v", err, e.mgr.Status(svc.ID).Running)
	}
}

// ---- live reconfiguration under load ----------------------------------

func TestApplyUnderLoad(t *testing.T) {
	a, b := newEcho(t), newEcho(t)
	e := newEnv(t)
	// Capture a small sample only: the point is the reconfiguration, and a
	// recording that grows by thousands of exchanges a second would make every
	// virtualizer rebuild slower than the last.
	svc := e.service(a.URL, model.ModeRecord, func(s *model.Service) { s.CaptureSample = 0.01 })
	seedExchange(t, e, svc.RecordingID, "GET", "/hot", 200, `{"hot":true}`)
	base := e.start(svc)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var ok, failed atomic.Int64
	var firstFailure atomic.Value
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := newClient(t)
			for {
				select {
				case <-stop:
					return
				default:
				}
				res, err := c.Get(base + "/hot")
				if err != nil {
					failed.Add(1)
					firstFailure.CompareAndSwap(nil, err.Error())
					continue
				}
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
				if res.StatusCode != 200 {
					failed.Add(1)
					firstFailure.CompareAndSwap(nil, fmt.Sprintf("status %d", res.StatusCode))
					continue
				}
				ok.Add(1)
			}
		}()
	}

	variants := []func(*model.Service){
		func(s *model.Service) { s.Mode = model.ModePassthrough },
		func(s *model.Service) { s.Mode = model.ModeRecord },
		func(s *model.Service) { s.Mode = model.ModeVirtualize },
		func(s *model.Service) { s.Mode = model.ModeHybrid },
		func(s *model.Service) { s.Mode = model.ModeRecord; s.AnonymizeCapture = true },
		func(s *model.Service) { s.Mode = model.ModeHybrid; s.Match.Selection = model.SelectFirst }, // rebuilds the virtualizer
		func(s *model.Service) { s.Mode = model.ModeVirtualize; s.Upstream = b.URL },
		func(s *model.Service) { s.Mode = model.ModeRecord; s.Timeout = model.Duration(31 * time.Second) }, // new transport
		func(s *model.Service) { s.Mode = model.ModePassthrough; s.PreserveHost = true },
		func(s *model.Service) { s.Mode = model.ModeRecord; s.CaptureSample = 0.05; s.MaxBodyBytes = 10 },
	}
	cur := svc
	deadline := time.Now().Add(1500 * time.Millisecond)
	applies := 0
	for time.Now().Before(deadline) || applies < 30 {
		cur = svc
		variants[applies%len(variants)](&cur)
		if err := e.mgr.Apply(context.Background(), cur); err != nil {
			t.Fatalf("Apply #%d: %v", applies, err)
		}
		applies++
		time.Sleep(3 * time.Millisecond)
	}
	close(stop)
	wg.Wait()

	if failed.Load() != 0 {
		t.Errorf("%d of %d requests failed during %d reconfigurations; first: %v", failed.Load(), ok.Load()+failed.Load(), applies, firstFailure.Load())
	}
	if ok.Load() < 100 {
		t.Errorf("only %d requests completed", ok.Load())
	}
	snap := e.waitIdle(svc.ID)
	if snap.Requests != ok.Load() {
		t.Errorf("metrics saw %d requests, clients completed %d", snap.Requests, ok.Load())
	}
	if snap.Errors != 0 {
		t.Errorf("metrics counted %d errors", snap.Errors)
	}
	// Every virtualizer but the current one must end up closed.
	e.virts.mu.Lock()
	built := append([]*fakeVirt(nil), e.virts.built...)
	e.virts.mu.Unlock()
	for i, v := range built[:max(len(built)-1, 0)] {
		waitFor(t, fmt.Sprintf("virtualizer #%d to be closed", i), v.closed.Load)
	}
	t.Logf("%d requests, %d reconfigurations, %d virtualizers built", ok.Load(), applies, len(built))
}

// ---- shutdown ----------------------------------------------------------

func TestShutdownDrainsCaptureQueue(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t, func(d *Deps) {
		d.CaptureFlushInterval = time.Hour // only Shutdown can flush
		d.CaptureBatchSize = 100000
	})
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	c := newClient(t)
	const n = 120
	for i := 0; i < n; i++ {
		do(t, c, "GET", fmt.Sprintf("%s/item/%d", base, i), nil)
	}
	e.waitIdle(svc.ID)
	if got := len(e.captured(svc.RecordingID)); got != 0 {
		t.Fatalf("%d exchanges stored before Shutdown; the test needs them still queued", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.mgr.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	got := e.captured(svc.RecordingID)
	if len(got) != n {
		t.Fatalf("%d exchanges in the store after Shutdown, want %d", len(got), n)
	}
	seen := map[string]bool{}
	for _, ex := range got {
		seen[ex.Request.Path] = true
	}
	if len(seen) != n {
		t.Errorf("%d distinct paths stored, want %d", len(seen), n)
	}
	if st := e.mgr.Status(svc.ID); st.Running || st.Captured != n {
		t.Errorf("status after Shutdown = %+v", st)
	}
}

func TestShutdownDrainsABackedUpPipeline(t *testing.T) {
	// A stuck store backs everything up: the writer holds a batch, the ready
	// channel is full, the worker is blocked handing over, and the rest of the
	// captures wait in the queue. Shutdown must carry all of it to the store
	// once the store recovers.
	up := newEcho(t)
	e, fs, release := gatedEnv(t, func(d *Deps) {
		d.CaptureWorkers = 1
		d.CaptureBatchSize = 5
		d.CaptureFlushInterval = time.Hour
	})
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	const n = 200
	for i := 0; i < n; i++ {
		do(t, newClient(t), "GET", fmt.Sprintf("%s/bk/%d", base, i), nil)
	}
	e.waitIdle(svc.ID)
	waitFor(t, "the writer to block", func() bool { return fs.appends.Load() >= 1 })
	// 5 in the writer's batch, 64 in ready, 1 in the worker: the queue holds the rest.
	waitFor(t, "the queue to back up", func() bool { return len(e.mgr.pipe.queue) > 100 })

	go func() {
		time.Sleep(50 * time.Millisecond)
		release()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := e.mgr.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	st := e.mgr.Status(svc.ID)
	if got := len(e.captured(svc.RecordingID)); got != n || st.Captured != n || st.CaptureDropped != 0 {
		t.Errorf("%d stored, status %+v: want all %d", got, st, n)
	}
}

func TestShutdownWaitsForRequestsInFlightAndStoresTheirCaptures(t *testing.T) {
	gate := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		io.WriteString(w, "finished")
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)

	result := make(chan string, 1)
	go func() {
		_, _, b := do(t, newClient(t), "GET", base+"/inflight", nil)
		result <- string(b)
	}()
	waitFor(t, "request in flight", func() bool { return e.snapshot(svc.ID).InFlight == 1 })

	shut := make(chan error, 1)
	go func() { shut <- e.mgr.Shutdown(context.Background()) }()
	select {
	case err := <-shut:
		t.Fatalf("Shutdown returned (%v) while a request was in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	if got := <-result; got != "finished" {
		t.Errorf("in-flight request: %q", got)
	}
	if err := <-shut; err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if got := e.captured(svc.RecordingID); len(got) != 1 || string(got[0].Response.Body) != "finished" {
		t.Errorf("capture of the request in flight: %+v", got)
	}
}

func TestShutdownIsBoundedByContext(t *testing.T) {
	// A stuck store with a backed-up pipeline: the writer holds a batch, the
	// hand-off channel is full and more captures wait in the queue. When the
	// context ends, all of them must be abandoned and counted.
	up := newEcho(t)
	fs := &failingStore{gate: make(chan struct{})}
	e := newEnv(t, func(d *Deps) {
		fs.Store = d.Store
		d.Store = fs
		d.CaptureWorkers = 1
		d.CaptureBatchSize = 5
	})
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	const n = 150
	for i := 0; i < n; i++ {
		do(t, newClient(t), "GET", fmt.Sprintf("%s/x%d", base, i), nil)
	}
	e.waitIdle(svc.ID)
	waitFor(t, "the writer to block on the store", func() bool { return fs.appends.Load() > 0 })
	waitFor(t, "the queue to back up", func() bool { return len(e.mgr.pipe.queue) > 40 })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := e.mgr.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown = %v, want a deadline error", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Shutdown took %v with a 150ms context", d)
	}
	if st := e.mgr.Status(svc.ID); st.CaptureDropped != n || st.Captured != 0 {
		t.Errorf("abandoned captures must all be counted as dropped: %+v", st)
	}
	if q := e.mgr.pipe.queuedBytes.Load(); q != 0 {
		t.Errorf("%d queued bytes still charged after the abort", q)
	}
	// The shutdown error is remembered; the env cleanup calls Shutdown again.
	if err := e.mgr.Shutdown(context.Background()); err == nil {
		t.Error("second Shutdown returned nil")
	}
	e.shutdownFailureExpected()
}

func TestAfterShutdown(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeVirtualize)
	svc.Mode = model.ModePassthrough
	addr := func() string { e.start(svc); return e.mgr.Status(svc.ID).Addr }()
	if err := e.mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.mgr.Shutdown(context.Background()); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
	if err := e.mgr.Apply(context.Background(), svc); !errors.Is(err, ErrClosed) {
		t.Errorf("Apply after Shutdown = %v, want ErrClosed", err)
	}
	if err := e.mgr.ProfileChanged(context.Background(), "default"); !errors.Is(err, ErrClosed) {
		t.Errorf("ProfileChanged after Shutdown = %v", err)
	}
	if st := e.mgr.Status(svc.ID); st.Running {
		t.Errorf("still running after Shutdown: %+v", st)
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Error("listener still open after Shutdown")
	}
	if _, ok := e.mgr.Handler(svc.ID); ok {
		t.Error("Handler returned a service after Shutdown")
	}
}

func TestShutdownRightAfterStartIsClean(t *testing.T) {
	// Shutdown races with the accept loop that has just started: the listener
	// Mirage closed itself may still be tracked by net/http, whose Shutdown
	// then reports the close error. That must not surface as a failure.
	up := newEcho(t)
	st := newEnv(t).st
	for i := 0; i < 100; i++ {
		m := NewManager(Deps{Store: st})
		svc := model.Service{ID: fmt.Sprintf("svc_quick%d", i), Name: "quick", ListenAddr: "127.0.0.1:0", Upstream: up.URL, Mode: model.ModePassthrough, Enabled: true}
		if err := m.Apply(context.Background(), svc); err != nil {
			t.Fatal(err)
		}
		if err := m.Shutdown(context.Background()); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
}
