package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
)

func TestPassthroughForwardsRequest(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModePassthrough)
	base := e.start(svc)
	c := newClient(t)

	status, hdr, body := do(t, c, http.MethodPost, base+"/users/42?expand=orders&x=1", strings.NewReader(`{"a":1}`),
		"Content-Type", "application/json", "X-Custom", "yes", "Authorization", "Bearer tok")
	if status != 200 {
		t.Fatalf("status = %d, body %s", status, body)
	}
	if hdr.Get("X-Upstream") != "echo" {
		t.Errorf("upstream response header missing: %v", hdr)
	}
	var got echoed
	mustJSON(t, body, &got)
	if got.Method != "POST" || got.URI != "/users/42?expand=orders&x=1" || got.Body != `{"a":1}` {
		t.Errorf("upstream saw %+v", got)
	}
	if got.Header.Get("X-Custom") != "yes" || got.Header.Get("Authorization") != "Bearer tok" {
		t.Errorf("headers not forwarded: %v", got.Header)
	}
	if got.Header.Get("X-Forwarded-For") == "" || got.Header.Get("X-Forwarded-Proto") != "http" {
		t.Errorf("X-Forwarded-* missing: %v", got.Header)
	}
	upHost := strings.TrimPrefix(up.URL, "http://")
	if got.Host != upHost {
		t.Errorf("upstream Host = %q, want the upstream's own %q", got.Host, upHost)
	}
	if st := e.mgr.Status(svc.ID); st.Captured != 0 || st.CaptureSkipped != 0 {
		t.Errorf("passthrough must not capture: %+v", st)
	}
}

func TestPassthroughPreserveHost(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModePassthrough, func(s *model.Service) { s.PreserveHost = true })
	base := e.start(svc)

	req, _ := http.NewRequest("GET", base+"/h", nil)
	req.Host = "api.example.test"
	res, err := newClient(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got := up.lastRequest().Host; got != "api.example.test" {
		t.Errorf("upstream Host = %q, want the client's", got)
	}
}

func TestPassthroughUpstreamBasePathAndEscapes(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL+"/base/v1", model.ModePassthrough)
	base := e.start(svc)
	c := newClient(t)

	for _, tc := range []struct{ path, want string }{
		{"/users/1", "/base/v1/users/1"},
		{"/", "/base/v1/"},
		{"/a%2Fb/c%20d?q=%7E", "/base/v1/a%2Fb/c%20d?q=%7E"},
		{"/files/x?a=1&a=2&b", "/base/v1/files/x?a=1&a=2&b"},
	} {
		status, _, _ := do(t, c, "GET", base+tc.path, nil)
		if status != 200 {
			t.Fatalf("%s: status %d", tc.path, status)
		}
		if got := up.lastRequest().URI; got != tc.want {
			t.Errorf("%s: upstream saw %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestPassthroughMetrics(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModePassthrough)
	base := e.start(svc)
	c := newClient(t)

	for i := 0; i < 3; i++ {
		do(t, c, "GET", base+"/users/42", nil)
	}
	do(t, c, "POST", base+"/orders", strings.NewReader("12345"))

	snap := e.waitIdle(svc.ID)
	if snap.Requests != 4 || snap.Errors != 0 {
		t.Errorf("requests=%d errors=%d, want 4/0", snap.Requests, snap.Errors)
	}
	if snap.BytesIn != 5 {
		t.Errorf("BytesIn = %d, want 5", snap.BytesIn)
	}
	if snap.BytesOut == 0 {
		t.Errorf("BytesOut = 0")
	}
	eps := map[string]int64{}
	for _, ep := range snap.Endpoints {
		eps[ep.Endpoint] = ep.Count
	}
	if eps["GET /users/{id}"] != 3 || eps["POST /orders"] != 1 {
		t.Errorf("endpoints = %v", eps)
	}
	if snap.Name != metrics.ServiceCollectorName(svc.ID) {
		t.Errorf("collector name = %q", snap.Name)
	}
}

func TestEndpointPatternsAndRawPath(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModePassthrough, func(s *model.Service) {
		s.EndpointPatterns = []string{"/files/{path...}", "/v2/{kind}/items"}
	})
	base := e.start(svc)
	c := newClient(t)

	do(t, c, "GET", base+"/files/a/b/c.txt", nil)
	do(t, c, "GET", base+"/v2/blue/items", nil)
	do(t, c, "GET", base+"/users/jane%40acme.io", nil)
	snap := e.waitIdle(svc.ID)
	eps := map[string]bool{}
	for _, ep := range snap.Endpoints {
		eps[ep.Endpoint] = true
	}
	for _, want := range []string{"GET /files/{path...}", "GET /v2/{kind}/items", "GET /users/{email}"} {
		if !eps[want] {
			t.Errorf("endpoint %q missing from %v", want, eps)
		}
	}
}

func TestUpstreamStatusesAreCountedAsErrors(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/limited":
			w.WriteHeader(429)
		case "/boom":
			w.WriteHeader(500)
		case "/missing":
			w.WriteHeader(404)
		}
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModePassthrough)
	base := e.start(svc)
	c := newClient(t)
	for _, p := range []string{"/limited", "/boom", "/missing", "/ok"} {
		do(t, c, "GET", base+p, nil)
	}
	snap := e.waitIdle(svc.ID)
	if snap.Requests != 4 || snap.Errors != 2 {
		t.Errorf("requests=%d errors=%d, want 4/2 (429 and 500 are errors)", snap.Requests, snap.Errors)
	}
	if snap.StatusClasses["4xx"] != 2 || snap.StatusClasses["5xx"] != 1 || snap.StatusClasses["2xx"] != 1 {
		t.Errorf("status classes = %v", snap.StatusClasses)
	}
}

func TestHandlerEmbedding(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModePassthrough)
	e.start(svc)

	h, ok := e.mgr.Handler(svc.ID)
	if !ok {
		t.Fatal("Handler: not found for a running service")
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	status, _, body := do(t, newClient(t), "GET", srv.URL+"/embedded", nil)
	if status != 200 || !strings.Contains(string(body), "/embedded") {
		t.Errorf("status %d body %s", status, body)
	}
	if _, ok := e.mgr.Handler("svc_missing"); ok {
		t.Error("Handler for unknown service returned ok")
	}
}

func TestPassthroughLeavesContentEncodingAlone(t *testing.T) {
	// The upstream compresses whatever the client asked for. With transparent
	// decompression on, the transport would add its own Accept-Encoding and
	// strip the encoding from the answer; the client must get the bytes as
	// the upstream sent them.
	plain := []byte(strings.Repeat("compress me ", 200))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(gz(t, plain))
	}))
	defer up.Close()
	e := newEnv(t)
	for _, mode := range []model.Mode{model.ModePassthrough, model.ModeRecord} {
		svc := e.service(up.URL, mode)
		base := e.start(svc)
		status, hdr, body := do(t, newClient(t), "GET", base+"/z", nil)
		if status != 200 || hdr.Get("Content-Encoding") != "gzip" || !bytes.Equal(body, gz(t, plain)) {
			t.Errorf("%s: status %d, Content-Encoding %q, %d bytes: want the upstream's gzip bytes untouched", mode, status, hdr.Get("Content-Encoding"), len(body))
		}
	}
}
