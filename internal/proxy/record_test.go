package proxy

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

func gz(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRecordCapturesExchange(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	c := newClient(t)

	before := time.Now()
	status, _, respBody := do(t, c, http.MethodPost, base+"/users/42/orders?expand=items&x=1", strings.NewReader(`{"sku":"A-1"}`),
		"Content-Type", "application/json", "X-Custom", "yes")
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	exs := e.waitStored(svc.RecordingID, 1)
	if len(exs) != 1 {
		t.Fatalf("stored %d exchanges, want 1", len(exs))
	}
	ex := exs[0]

	if ex.ServiceID != svc.ID || ex.RecordingID != svc.RecordingID {
		t.Errorf("ids: service %q recording %q", ex.ServiceID, ex.RecordingID)
	}
	if ex.Endpoint != "POST /users/{id}/orders" {
		t.Errorf("Endpoint = %q", ex.Endpoint)
	}
	if ex.StartedAt.Before(before.Add(-time.Second)) || ex.StartedAt.After(time.Now().Add(time.Second)) {
		t.Errorf("StartedAt = %v (test began %v)", ex.StartedAt, before)
	}
	if ex.TTFB <= 0 || ex.Duration < ex.TTFB {
		t.Errorf("TTFB %v, Duration %v: want 0 < TTFB <= Duration", ex.TTFB, ex.Duration)
	}
	if ex.Error != "" || ex.Anonymized {
		t.Errorf("Error %q Anonymized %v", ex.Error, ex.Anonymized)
	}

	req := ex.Request
	if req.Method != "POST" || req.Path != "/users/42/orders" || req.RawQuery != "expand=items&x=1" {
		t.Errorf("request line: %s %s?%s", req.Method, req.Path, req.RawQuery)
	}
	if req.Scheme != "http" || req.Proto != "HTTP/1.1" || req.Host != strings.TrimPrefix(base, "http://") {
		t.Errorf("request scheme %q proto %q host %q", req.Scheme, req.Proto, req.Host)
	}
	if req.Header.Get("X-Custom") != "yes" || req.Header.Get("Content-Type") != "application/json" {
		t.Errorf("request headers: %v", req.Header)
	}
	if req.Header.Get("X-Forwarded-For") != "" {
		t.Errorf("stored request must be what the client sent, not what the upstream got: %v", req.Header)
	}
	if string(req.Body) != `{"sku":"A-1"}` || req.BodySize != 13 || req.BodyTruncated {
		t.Errorf("request body %q size %d truncated %v", req.Body, req.BodySize, req.BodyTruncated)
	}

	resp := ex.Response
	if resp.Status != 200 || resp.Header.Get("X-Upstream") != "echo" {
		t.Errorf("response: %d %v", resp.Status, resp.Header)
	}
	if !bytes.Equal(resp.Body, respBody) || resp.BodySize != int64(len(respBody)) || resp.BodyTruncated {
		t.Errorf("response body mismatch: stored %d bytes, client got %d", len(resp.Body), len(respBody))
	}
	if resp.Proto != "HTTP/1.1" {
		t.Errorf("response proto = %q", resp.Proto)
	}

	rec, err := e.st.GetRecording(context.Background(), svc.RecordingID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ExchangeCount != 1 {
		t.Errorf("recording ExchangeCount = %d", rec.ExchangeCount)
	}
	if st := e.mgr.Status(svc.ID); st.Captured != 1 || st.CaptureDropped != 0 || st.CaptureSkipped != 0 {
		t.Errorf("status = %+v", st)
	}
}

func TestRecordStoresRawQueryAndEscapedPath(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	do(t, newClient(t), "GET", base+"/a%2Fb/c%20d?q=%7E&z", nil)
	ex := e.waitStored(svc.RecordingID, 1)[0]
	if ex.Request.Path != "/a%2Fb/c%20d" || ex.Request.RawQuery != "q=%7E&z" {
		t.Errorf("path %q query %q", ex.Request.Path, ex.Request.RawQuery)
	}
}

func TestRecordAnonymizesBeforeStoring(t *testing.T) {
	const email, token = "jane.doe@acme.io", "tok-super-secret-123"
	var seen echoed
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = echoed{URI: r.RequestURI, Header: r.Header.Clone(), Body: string(b)}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=sess-abcdef123456; Path=/")
		io.WriteString(w, `{"user":{"email":"`+email+`"},"ok":true}`)
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.AnonymizeCapture = true })
	base := e.start(svc)

	status, _, body := do(t, newClient(t), "POST", base+"/users/"+strings.Replace(email, "@", "%40", 1)+"?api_key=KEY-1234567",
		strings.NewReader(`{"email":"`+email+`","password":"hunter2"}`),
		"Content-Type", "application/json", "Authorization", "Bearer "+token)
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	// The client gets the real thing back; only the stored copy is anonymized.
	if !strings.Contains(string(body), email) {
		t.Errorf("client response was altered: %s", body)
	}
	mu.Lock()
	if seen.Header.Get("Authorization") != "Bearer "+token || !strings.Contains(seen.Body, "hunter2") || !strings.Contains(seen.URI, "KEY-1234567") {
		t.Errorf("upstream request was altered: %+v", seen)
	}
	mu.Unlock()

	ex := e.waitStored(svc.RecordingID, 1)[0]
	if !ex.Anonymized {
		t.Error("exchange is not marked anonymized")
	}
	if ex.Endpoint != "POST /users/{email}" {
		t.Errorf("Endpoint = %q: must come from the raw path", ex.Endpoint)
	}
	full, _ := json2(ex)
	for _, secret := range []string{email, token, "hunter2", "KEY-1234567", "jane.doe%40acme.io", "sess-abcdef123456"} {
		if strings.Contains(full, secret) {
			t.Errorf("stored exchange still contains %q:\n%s", secret, full)
		}
	}
	rec, _ := e.st.GetRecording(context.Background(), svc.RecordingID)
	if !rec.Anonymized {
		t.Error("recording is not marked anonymized")
	}
}

// json2 flattens everything of an exchange that could hold a secret.
func json2(ex *model.Exchange) (string, error) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s?%s\n%v\n%s\n%v\n%s\n%s", ex.Request.Method, ex.Request.Path, ex.Request.RawQuery,
		ex.Request.Header, ex.Request.Body, ex.Response.Header, ex.Response.Body, ex.Error)
	return sb.String(), nil
}

func TestApplyRejectsMissingProfileAndRecording(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)

	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) {
		s.AnonymizeCapture = true
		s.ProfileID = "prof_missing"
	})
	err := e.mgr.Apply(context.Background(), svc)
	if !errors.Is(err, model.ErrNotFound) {
		t.Errorf("missing profile: err = %v, want ErrNotFound", err)
	}
	if st := e.mgr.Status(svc.ID); st.Running || st.Error == "" {
		t.Errorf("status after failed Apply: %+v", st)
	}

	svc2 := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.RecordingID = "rec_missing" })
	if err := e.mgr.Apply(context.Background(), svc2); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("missing recording: err = %v, want ErrNotFound", err)
	}

	// A stored profile with no secret is refused (it cannot be built through
	// the store, which always assigns one, so test the loader directly).
	p, _ := e.st.GetProfile(context.Background(), model.DefaultProfileID)
	if p.Secret == "" {
		t.Fatal("default profile has no secret")
	}
}

func TestApplyRejectsInvalidService(t *testing.T) {
	e := newEnv(t)
	err := e.mgr.Apply(context.Background(), model.Service{ID: "svc_bad", Name: "bad", ListenAddr: "127.0.0.1:0", Mode: model.ModeRecord, Enabled: true})
	if !errors.Is(err, model.ErrValidation) {
		t.Errorf("err = %v, want a validation error", err)
	}
	err = e.mgr.Apply(context.Background(), model.Service{Name: "noid", Enabled: true})
	if !errors.Is(err, model.ErrValidation) {
		t.Errorf("missing id: err = %v", err)
	}
}

func TestAcceptEncodingNarrowedUpstream(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	rec := e.service(up.URL, model.ModeRecord)
	pass := e.service(up.URL, model.ModePassthrough)
	recBase, passBase := e.start(rec), e.start(pass)
	c := newClient(t)

	for _, tc := range []struct {
		name, sent, want string
		noHeader         bool
	}{
		{"gzip and brotli", "gzip, br", "gzip", false},
		{"all three", "br, gzip;q=0.8, deflate", "gzip, deflate", false},
		{"only brotli", "br", "identity", false},
		{"refused gzip", "gzip;q=0, deflate", "deflate", false},
		{"star", "*", "gzip, deflate", false},
		{"nothing sent", "", "identity", true},
	} {
		hdrs := []string{}
		if !tc.noHeader {
			hdrs = []string{"Accept-Encoding", tc.sent}
		}
		do(t, c, "GET", recBase+"/ae", nil, hdrs...)
		if got := up.lastRequest().Header.Get("Accept-Encoding"); got != tc.want {
			t.Errorf("record, client sent %q (%s): upstream got %q, want %q", tc.sent, tc.name, got, tc.want)
		}
	}
	// Passthrough leaves the client's value alone.
	do(t, c, "GET", passBase+"/ae", nil, "Accept-Encoding", "br, gzip")
	if got := up.lastRequest().Header.Get("Accept-Encoding"); got != "br, gzip" {
		t.Errorf("passthrough: upstream got %q, want the client's", got)
	}
}

func TestNarrowAcceptEncoding(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, "identity"},
		{[]string{""}, "identity"},
		{[]string{"gzip"}, "gzip"},
		{[]string{"GZIP"}, "gzip"},
		{[]string{"x-gzip"}, "gzip"},
		{[]string{"deflate"}, "deflate"},
		{[]string{"deflate, gzip"}, "gzip, deflate"},
		{[]string{"gzip, deflate, br, zstd"}, "gzip, deflate"},
		{[]string{"br, zstd"}, "identity"},
		{[]string{"identity"}, "identity"},
		{[]string{"gzip;q=0"}, "identity"},
		{[]string{"gzip;q=0.0, deflate;q=0.5"}, "deflate"},
		{[]string{"gzip; q=0.001"}, "gzip"},
		{[]string{"*"}, "gzip, deflate"},
		{[]string{"*;q=0"}, "identity"},
		{[]string{"*, gzip;q=0"}, "deflate"},
		{[]string{"gzip", "br"}, "gzip"},
		{[]string{"br", "deflate"}, "deflate"},
		{[]string{" gzip ; q=1 , , deflate"}, "gzip, deflate"},
	} {
		if got := narrowAcceptEncoding(tc.in); got != tc.want {
			t.Errorf("narrowAcceptEncoding(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGzipResponsePassesThroughAndIsStoredDecoded(t *testing.T) {
	plain := []byte(strings.Repeat(`{"name":"widget","price":12.5},`, 40))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gz(t, plain))
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)

	status, hdr, wire := do(t, newClient(t), "GET", base+"/items", nil, "Accept-Encoding", "gzip")
	if status != 200 || hdr.Get("Content-Encoding") != "gzip" {
		t.Fatalf("client got status %d, Content-Encoding %q: gzip must pass through", status, hdr.Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("client body is not gzip: %v", err)
	}
	if got, _ := io.ReadAll(zr); !bytes.Equal(got, plain) {
		t.Error("client body does not decode to the original")
	}

	ex := e.waitStored(svc.RecordingID, 1)[0]
	r := ex.Response
	if !bytes.Equal(r.Body, plain) {
		t.Errorf("stored body (%d bytes) is not the decoded original (%d bytes)", len(r.Body), len(plain))
	}
	if r.ContentEncoding != "gzip" || r.BodySize != int64(len(plain)) || r.BodyTruncated {
		t.Errorf("ContentEncoding %q BodySize %d truncated %v", r.ContentEncoding, r.BodySize, r.BodyTruncated)
	}
	if r.Header.Get("Content-Encoding") != "" || r.Header.Get("Content-Length") != "" {
		t.Errorf("stored headers still carry the encoding or length: %v", r.Header)
	}
}

func TestDeflateAndGzipRequestBodiesAreDecoded(t *testing.T) {
	plain := []byte(strings.Repeat("hello deflate world ", 50))
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	c := newClient(t)

	var zbuf, fbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	zw.Write(plain)
	zw.Close()
	fw, _ := flate.NewWriter(&fbuf, flate.BestSpeed)
	fw.Write(plain)
	fw.Close()

	do(t, c, "POST", base+"/zlib", bytes.NewReader(zbuf.Bytes()), "Content-Encoding", "deflate")
	do(t, c, "POST", base+"/raw-deflate", bytes.NewReader(fbuf.Bytes()), "Content-Encoding", "deflate")
	do(t, c, "POST", base+"/gzip", bytes.NewReader(gz(t, plain)), "Content-Encoding", "gzip")
	exs := e.waitStored(svc.RecordingID, 3)
	byPath := map[string]*model.Exchange{}
	for _, ex := range exs {
		byPath[ex.Request.Path] = ex
	}
	for _, p := range []string{"/zlib", "/raw-deflate", "/gzip"} {
		ex := byPath[p]
		if ex == nil {
			t.Fatalf("%s not stored", p)
		}
		if !bytes.Equal(ex.Request.Body, plain) || ex.Request.BodySize != int64(len(plain)) {
			t.Errorf("%s: request body not decoded (%d bytes)", p, len(ex.Request.Body))
		}
		if ex.Request.ContentEncoding == "" || ex.Request.Header.Get("Content-Encoding") != "" {
			t.Errorf("%s: ContentEncoding %q header %q", p, ex.Request.ContentEncoding, ex.Request.Header.Get("Content-Encoding"))
		}
	}
	// The upstream still received the compressed bytes.
	if got := up.lastRequest(); got.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("upstream did not receive the encoding header: %v", got.Header)
	}
}

func TestUndecodableBodiesAreStoredAsCaptured(t *testing.T) {
	corrupt := []byte("this is not gzip at all")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/corrupt":
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(corrupt)
		case "/brotli":
			w.Header().Set("Content-Encoding", "br")
			w.Write(corrupt)
		case "/stacked":
			w.Header().Set("Content-Encoding", "gzip, br")
			w.Write(gz(t, corrupt))
		}
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	c := newClient(t)
	for _, p := range []string{"/corrupt", "/brotli", "/stacked"} {
		do(t, c, "GET", base+p, nil, "Accept-Encoding", "gzip, br")
	}
	for _, ex := range e.waitStored(svc.RecordingID, 3) {
		if ex.Response.Header.Get("Content-Encoding") == "" || ex.Response.ContentEncoding != "" {
			t.Errorf("%s: should stay encoded: header %q ContentEncoding %q", ex.Request.Path, ex.Response.Header.Get("Content-Encoding"), ex.Response.ContentEncoding)
		}
		if len(ex.Response.Body) == 0 {
			t.Errorf("%s: body lost", ex.Request.Path)
		}
	}
}

func TestDecompressionBombIsCut(t *testing.T) {
	// 8 MiB of zeros compresses to a few KiB.
	huge := make([]byte, 8<<20)
	wire := gz(t, huge)
	if len(wire) > 100<<10 {
		t.Fatalf("test setup: compressed size %d", len(wire))
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(wire)
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.MaxBodyBytes = 64 << 10 })
	base := e.start(svc)
	do(t, newClient(t), "GET", base+"/bomb", nil, "Accept-Encoding", "gzip")
	r := e.waitStored(svc.RecordingID, 1)[0].Response
	if len(r.Body) != 64<<10 || !r.BodyTruncated || r.BodySize <= int64(len(r.Body)) {
		t.Errorf("body %d bytes, truncated %v, BodySize %d", len(r.Body), r.BodyTruncated, r.BodySize)
	}
	if r.ContentEncoding != "gzip" || r.Header.Get("Content-Encoding") != "" {
		t.Errorf("ContentEncoding %q header %q", r.ContentEncoding, r.Header.Get("Content-Encoding"))
	}
}

func TestTruncatedEncodedBodyStaysEncoded(t *testing.T) {
	// Incompressible content: the compressed copy is larger than the cap.
	rnd := make([]byte, 40<<10)
	rand.NewChaCha8([32]byte{1}).Read(rnd)
	wire := gz(t, rnd)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(wire)
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.MaxBodyBytes = 8 << 10 })
	base := e.start(svc)
	_, _, got := do(t, newClient(t), "GET", base+"/x", nil, "Accept-Encoding", "gzip")
	if !bytes.Equal(got, wire) {
		t.Fatal("client did not receive the full compressed body")
	}
	r := e.waitStored(svc.RecordingID, 1)[0].Response
	if !r.BodyTruncated || len(r.Body) != 8<<10 || r.BodySize != int64(len(wire)) {
		t.Errorf("body %d truncated %v size %d (wire %d)", len(r.Body), r.BodyTruncated, r.BodySize, len(wire))
	}
	if r.Header.Get("Content-Encoding") != "gzip" || r.ContentEncoding != "" {
		t.Errorf("header %q ContentEncoding %q: an undecodable body must keep its encoding header", r.Header.Get("Content-Encoding"), r.ContentEncoding)
	}
}

func TestStreamingReachesClientWhileCaptured(t *testing.T) {
	for _, tc := range []struct {
		name, contentType string
	}{
		{"server-sent events", "text/event-stream"},
		{"chunked", "application/octet-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			var once = func() func() {
				done := false
				return func() {
					if !done {
						done = true
						close(release)
					}
				}
			}()
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				fl := w.(http.Flusher)
				io.WriteString(w, "data: one\n\n")
				fl.Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				io.WriteString(w, "data: two\n\n")
				fl.Flush()
			}))
			defer up.Close()
			defer once()
			e := newEnv(t)
			svc := e.service(up.URL, model.ModeRecord)
			base := e.start(svc)

			res, err := newClient(t).Get(base + "/stream")
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			// The first chunk must arrive while the upstream is still blocked.
			first := make(chan string, 1)
			br := bufio.NewReader(res.Body)
			go func() {
				line, _ := br.ReadString('\n')
				first <- line
			}()
			select {
			case line := <-first:
				if line != "data: one\n" {
					t.Fatalf("first chunk = %q", line)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the first chunk did not reach the client before the upstream finished: response is buffered")
			}
			once()
			rest, err := io.ReadAll(br)
			if err != nil || string(rest) != "\ndata: two\n\n" {
				t.Fatalf("rest = %q, err %v", rest, err)
			}

			ex := e.waitStored(svc.RecordingID, 1)[0]
			if string(ex.Response.Body) != "data: one\n\ndata: two\n\n" || ex.Error != "" {
				t.Errorf("stored body %q error %q", ex.Response.Body, ex.Error)
			}
			if ex.Duration <= ex.TTFB {
				t.Errorf("Duration %v must exceed TTFB %v for a streamed response", ex.Duration, ex.TTFB)
			}
		})
	}
}

func TestTTFBAndDurationAreMeasuredAroundTheUpstream(t *testing.T) {
	// Headers after 120ms, the last byte another 120ms later.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "head;")
		w.(http.Flusher).Flush()
		time.Sleep(120 * time.Millisecond)
		io.WriteString(w, "tail")
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	if status, _, body := do(t, newClient(t), "GET", base+"/timed", nil); status != 200 || string(body) != "head;tail" {
		t.Fatalf("status %d body %q", status, body)
	}
	ex := e.waitStored(svc.RecordingID, 1)[0]
	ttfb, dur := ex.TTFB.D(), ex.Duration.D()
	if ttfb < 100*time.Millisecond || ttfb > 200*time.Millisecond {
		t.Errorf("TTFB = %v, want about 120ms (until the response headers arrived)", ttfb)
	}
	if dur < 220*time.Millisecond || dur-ttfb < 100*time.Millisecond {
		t.Errorf("Duration = %v with TTFB %v, want about 240ms (until the last body byte)", dur, ttfb)
	}
}

func TestLargeBodiesAreForwardedIntactAndCaptureTruncated(t *testing.T) {
	const reqSize, respSize, capBytes = 3 << 20, 2 << 20, 1024
	reqData := make([]byte, reqSize)
	respData := make([]byte, respSize)
	r := rand.NewChaCha8([32]byte{7})
	r.Read(reqData)
	r.Read(respData)
	wantReq := sha256.Sum256(reqData)

	type seen struct {
		n   int64
		sum [32]byte
	}
	got := make(chan seen, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := sha256.New()
		n, _ := io.Copy(h, r.Body)
		var s seen
		s.n = n
		copy(s.sum[:], h.Sum(nil))
		got <- s
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(respData)
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.MaxBodyBytes = capBytes })
	base := e.start(svc)

	status, _, body := do(t, newClient(t), "POST", base+"/upload", bytes.NewReader(reqData), "Content-Type", "application/octet-stream")
	if status != 200 || !bytes.Equal(body, respData) {
		t.Fatalf("status %d, response intact: %v", status, bytes.Equal(body, respData))
	}
	if s := <-got; s.n != reqSize || s.sum != wantReq {
		t.Errorf("upstream received %d bytes (sum ok: %v), want %d intact", s.n, s.sum == wantReq, reqSize)
	}
	ex := e.waitStored(svc.RecordingID, 1)[0]
	if len(ex.Request.Body) != capBytes || ex.Request.BodySize != reqSize || !ex.Request.BodyTruncated {
		t.Errorf("request: stored %d, size %d, truncated %v", len(ex.Request.Body), ex.Request.BodySize, ex.Request.BodyTruncated)
	}
	if !bytes.Equal(ex.Request.Body, reqData[:capBytes]) {
		t.Error("stored request body is not the prefix of the sent body")
	}
	if len(ex.Response.Body) != capBytes || ex.Response.BodySize != respSize || !ex.Response.BodyTruncated {
		t.Errorf("response: stored %d, size %d, truncated %v", len(ex.Response.Body), ex.Response.BodySize, ex.Response.BodyTruncated)
	}
	if !bytes.Equal(ex.Response.Body, respData[:capBytes]) {
		t.Error("stored response body is not the prefix")
	}
	if st := e.waitIdle(svc.ID); st.BytesIn != reqSize {
		t.Errorf("BytesIn = %d, want %d", st.BytesIn, reqSize)
	}
}

// TestResponseIsNotCutOffWhileTheRequestBodyIsStillBeingSent pins the cause of
// the CI-only "unexpected EOF" flake of
// TestLargeBodiesAreForwardedIntactAndCaptureTruncated.
//
// net/http's HTTP/1 server closes the request body of a handler that starts
// its response before it has read the whole body, unless the handler asked
// for full duplex (http.ResponseController.EnableFullDuplex). The proxy hands
// the client's body to the upstream transport, which keeps reading it on its
// own goroutine, so that close can land in the middle of the transport's
// read: the transport fails with "invalid Read on closed Body", drops the
// upstream connection and the response the client is reading stops with an
// error. That happens whenever the upstream answers before the upload has
// finished (an early answer, a streaming service) and, as a race, when the
// transport is only a few microseconds behind a fast upstream, as in the
// flaky test.
//
// Here the upstream answers after the first kilobyte and the client sends the
// rest of its upload once the answer is under way, so the answer is always
// ahead of the request body. Nothing may be cut: the client gets the whole
// response and the upstream gets the whole request.
func TestResponseIsNotCutOffWhileTheRequestBodyIsStillBeingSent(t *testing.T) {
	const respSize = 3 << 20
	rnd := rand.NewChaCha8([32]byte{9})
	head, tail, respData := make([]byte, 1<<10), make([]byte, 128<<10), make([]byte, respSize)
	rnd.Read(head)
	rnd.Read(tail)
	rnd.Read(respData)
	wantReq := sha256.Sum256(append(append([]byte(nil), head...), tail...))

	answering := make(chan struct{})
	gotReq := make(chan [32]byte, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.EnableFullDuplex(); err != nil { // answer while the body is still arriving
			t.Errorf("upstream EnableFullDuplex: %v", err)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(respData[:1<<10])
		rc.Flush()
		close(answering)
		w.Write(respData[1<<10:])
		h := sha256.New()
		io.Copy(h, r.Body)
		var sum [32]byte
		copy(sum[:], h.Sum(nil))
		gotReq <- sum
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.MaxBodyBytes = 1024 })
	base := e.start(svc)

	pr, pw := io.Pipe()
	defer pr.Close()
	go func() {
		pw.Write(head)
		select {
		case <-answering:
		case <-time.After(10 * time.Second):
		}
		// Let the proxy start copying the answer to the client; a proxy that
		// is too slow only makes this test pass vacuously, never fail.
		time.Sleep(50 * time.Millisecond)
		pw.Write(tail)
		pw.Close()
	}()
	req, err := http.NewRequest("POST", base+"/stream", pr)
	if err != nil {
		t.Fatal(err)
	}
	res, err := newClient(t).Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the response: %v (got %d of %d bytes)", err, len(body), respSize)
	}
	if res.StatusCode != 200 || !bytes.Equal(body, respData) {
		t.Fatalf("status %d, response intact: %v", res.StatusCode, bytes.Equal(body, respData))
	}
	select {
	case sum := <-gotReq:
		if sum != wantReq {
			t.Error("the upstream did not receive the request body intact")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the upstream never finished reading the request body")
	}
	ex := e.waitStored(svc.RecordingID, 1)[0]
	if ex.Response.BodySize != respSize || ex.Error != "" {
		t.Errorf("stored response size %d (want %d), error %q", ex.Response.BodySize, respSize, ex.Error)
	}
}

func TestBodyCaptureDisabled(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.MaxBodyBytes = -1 })
	base := e.start(svc)
	status, _, body := do(t, newClient(t), "POST", base+"/nobody", strings.NewReader("sensitive payload"))
	if status != 200 || len(body) == 0 {
		t.Fatalf("status %d", status)
	}
	ex := e.waitStored(svc.RecordingID, 1)[0]
	if len(ex.Request.Body) != 0 || len(ex.Response.Body) != 0 {
		t.Errorf("bodies were captured: %d / %d bytes", len(ex.Request.Body), len(ex.Response.Body))
	}
	if ex.Request.BodySize != int64(len("sensitive payload")) || ex.Response.BodySize != int64(len(body)) {
		t.Errorf("sizes %d / %d still count the bytes", ex.Request.BodySize, ex.Response.BodySize)
	}
	if !ex.Request.BodyTruncated {
		t.Error("a request body that was not stored must be marked truncated")
	}
}

func TestChunkedRequestBody(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)

	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < 5; i++ {
			fmt.Fprintf(pw, "part-%d;", i)
		}
		pw.Close()
	}()
	status, _, _ := do(t, newClient(t), "PUT", base+"/chunked", io.NopCloser(pr))
	if status != 200 {
		t.Fatal(status)
	}
	if got := up.lastRequest().Body; got != "part-0;part-1;part-2;part-3;part-4;" {
		t.Errorf("upstream body = %q", got)
	}
	ex := e.waitStored(svc.RecordingID, 1)[0]
	if string(ex.Request.Body) != "part-0;part-1;part-2;part-3;part-4;" || ex.Request.BodySize != 35 {
		t.Errorf("stored %q size %d", ex.Request.Body, ex.Request.BodySize)
	}
}

func TestHeadAndNoContentResponses(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/nc":
			w.WriteHeader(http.StatusNoContent)
		case "/nm":
			w.WriteHeader(http.StatusNotModified)
		default:
			w.Header().Set("Content-Length", "1234")
		}
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	c := newClient(t)
	do(t, c, "HEAD", base+"/h", nil)
	do(t, c, "GET", base+"/nc", nil)
	do(t, c, "GET", base+"/nm", nil)
	exs := e.waitStored(svc.RecordingID, 3)
	statuses := map[string]int{}
	for _, ex := range exs {
		statuses[ex.Request.Path] = ex.Response.Status
		if len(ex.Response.Body) != 0 || ex.Error != "" {
			t.Errorf("%s: body %d bytes, error %q", ex.Request.Path, len(ex.Response.Body), ex.Error)
		}
	}
	if statuses["/h"] != 200 || statuses["/nc"] != 204 || statuses["/nm"] != 304 {
		t.Errorf("statuses = %v", statuses)
	}
}

func TestUpstreamDownAnswers502AndIsCaptured(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()

	e := newEnv(t)
	svc := e.service(url, model.ModeRecord)
	base := e.start(svc)
	status, hdr, body := do(t, newClient(t), "GET", base+"/users/7", nil)
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	if hdr.Get("X-Mirage-Error") != "upstream" || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Errorf("headers: %v", hdr)
	}
	var doc struct{ Error, Detail string }
	mustJSON(t, body, &doc)
	if doc.Error != "upstream request failed" || doc.Detail == "" {
		t.Errorf("body = %s", body)
	}

	ex := e.waitStored(svc.RecordingID, 1)[0]
	if ex.Error == "" || ex.Response.Status != 502 || ex.TTFB != 0 {
		t.Errorf("Error %q, status %d, TTFB %v", ex.Error, ex.Response.Status, ex.TTFB)
	}
	if ex.Endpoint != "GET /users/{id}" || ex.Request.Path != "/users/7" {
		t.Errorf("endpoint %q path %q", ex.Endpoint, ex.Request.Path)
	}
	if !bytes.Equal(ex.Response.Body, body) {
		t.Errorf("stored response body %q differs from what the client saw %q", ex.Response.Body, body)
	}
	if !ex.IsError() {
		t.Error("exchange must count as an error")
	}
	snap := e.waitIdle(svc.ID)
	if snap.Errors != 1 || snap.StatusClasses["5xx"] != 1 {
		t.Errorf("metrics: errors %d classes %v", snap.Errors, snap.StatusClasses)
	}
}

func TestCaptureSurvivesClientsThatHangUpAtOnce(t *testing.T) {
	// A client sending "Connection: close" drops the connection as soon as it
	// has the answer, which can cancel the request context before the handler
	// has handed its capture over. Complete exchanges and genuine upstream
	// failures must be stored regardless.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	up := newEcho(t)
	e := newEnv(t)
	ok := e.service(up.URL, model.ModeRecord)
	bad := e.service(deadURL, model.ModeRecord)
	okBase, badBase := e.start(ok), e.start(bad)
	tr := &http.Transport{DisableKeepAlives: true, DisableCompression: true}
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	const n = 60
	for i := 0; i < n; i++ {
		if status, _, _ := do(t, c, "GET", okBase+"/ok", nil); status != 200 {
			t.Fatal(status)
		}
		if status, _, _ := do(t, c, "GET", badBase+"/down", nil); status != 502 {
			t.Fatal(status)
		}
	}
	e.waitStored(ok.RecordingID, n)
	e.waitStored(bad.RecordingID, n)
	for _, id := range []string{ok.ID, bad.ID} {
		if st := e.mgr.Status(id); st.CaptureSkipped != 0 || st.CaptureDropped != 0 {
			t.Errorf("%s: %+v", id, st)
		}
	}
}

// cancelOnWrite cancels the request context as soon as the response body has
// been written, like a client that hangs up the moment it has its answer.
type cancelOnWrite struct {
	http.ResponseWriter
	cancel context.CancelFunc
}

func (c cancelOnWrite) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.cancel()
	return n, err
}

func TestCaptureIgnoresAClientThatHangsUpAfterTheAnswer(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	up := newEcho(t)
	e := newEnv(t)
	for name, upstream := range map[string]string{"complete response": up.URL, "upstream failure": deadURL} {
		t.Run(name, func(t *testing.T) {
			svc := e.service(upstream, model.ModeRecord)
			e.start(svc)
			h, _ := e.mgr.Handler(svc.ID)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest("GET", "/hangup", nil).WithContext(ctx)
			h.ServeHTTP(cancelOnWrite{httptest.NewRecorder(), cancel}, req)
			if ctx.Err() == nil {
				t.Fatal("setup: the context was not cancelled")
			}
			ex := e.waitStored(svc.RecordingID, 1)[0]
			if (name == "upstream failure") != (ex.Error != "") {
				t.Errorf("Error = %q", ex.Error)
			}
			if st := e.mgr.Status(svc.ID); st.CaptureSkipped != 0 {
				t.Errorf("a finished exchange was skipped: %+v", st)
			}
		})
	}
}

func TestUpstreamTimeoutAnswers504(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer up.Close()
	defer close(release)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.Timeout = model.Duration(150 * time.Millisecond) })
	base := e.start(svc)

	start := time.Now()
	status, hdr, body := do(t, newClient(t), "GET", base+"/slow", nil)
	if status != http.StatusGatewayTimeout || hdr.Get("X-Mirage-Error") != "upstream" {
		t.Fatalf("status %d headers %v body %s", status, hdr, body)
	}
	if d := time.Since(start); d < 100*time.Millisecond || d > 3*time.Second {
		t.Errorf("answered after %v, want about the 150ms timeout", d)
	}
	ex := e.waitStored(svc.RecordingID, 1)[0]
	if ex.Response.Status != 504 || ex.Error == "" || ex.Duration.D() < 100*time.Millisecond {
		t.Errorf("status %d error %q duration %v", ex.Response.Status, ex.Error, ex.Duration)
	}
}

func TestSlowBodyIsNotCutByTimeout(t *testing.T) {
	// Timeout bounds the wait for headers only; bodies stream until EOF.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		io.WriteString(w, "late body")
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.Timeout = model.Duration(100 * time.Millisecond) })
	base := e.start(svc)
	status, _, body := do(t, newClient(t), "GET", base+"/slowbody", nil)
	if status != 200 || string(body) != "late body" {
		t.Errorf("status %d body %q", status, body)
	}
	ex := e.waitStored(svc.RecordingID, 1)[0]
	if ex.Error != "" || string(ex.Response.Body) != "late body" {
		t.Errorf("error %q body %q", ex.Error, ex.Response.Body)
	}
}

func TestUpstreamInterruptedMidBody(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, bw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		bw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 100\r\n\r\n0123456789")
		bw.Flush()
		conn.Close()
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)

	res, err := newClient(t).Get(base + "/cut")
	if err == nil {
		_, err = io.ReadAll(res.Body)
		res.Body.Close()
	}
	if err == nil {
		t.Fatal("the client must see the truncated response as an error")
	}
	ex := e.waitStored(svc.RecordingID, 1)[0]
	if ex.Response.Status != 200 || string(ex.Response.Body) != "0123456789" {
		t.Errorf("stored status %d body %q", ex.Response.Status, ex.Response.Body)
	}
	if !strings.Contains(ex.Error, "interrupted") {
		t.Errorf("Error = %q, want an interruption", ex.Error)
	}
	snap := e.waitIdle(svc.ID)
	if snap.Errors != 1 || snap.StatusClasses["err"] != 1 {
		t.Errorf("an aborted request is observed with status 0 as an error: errors %d classes %v", snap.Errors, snap.StatusClasses)
	}
}

func TestClientDisconnectMidResponseSkipsCapture(t *testing.T) {
	upDone := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upDone)
		io.WriteString(w, "first chunk\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/hang", nil)
	res, err := newClient(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(res.Body).ReadString('\n')
	if line != "first chunk\n" {
		t.Fatalf("first chunk = %q", line)
	}
	cancel()
	res.Body.Close()
	<-upDone

	snap := e.waitIdle(svc.ID)
	if snap.InFlight != 0 || snap.Requests != 1 {
		t.Errorf("in-flight %d requests %d", snap.InFlight, snap.Requests)
	}
	waitFor(t, "CaptureSkipped", func() bool { return e.mgr.Status(svc.ID).CaptureSkipped == 1 })
	time.Sleep(40 * time.Millisecond) // several flush intervals
	if got := e.captured(svc.RecordingID); len(got) != 0 {
		t.Errorf("a response the client abandoned was stored: %d exchanges", len(got))
	}
	if st := e.mgr.Status(svc.ID); st.Captured != 0 || st.CaptureDropped != 0 {
		t.Errorf("status = %+v", st)
	}
}

func TestClientDisconnectBeforeResponseSkipsCapture(t *testing.T) {
	arrived := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-r.Context().Done()
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/never", nil)
	go func() {
		<-arrived
		cancel()
	}()
	if res, err := newClient(t).Do(req); err == nil {
		res.Body.Close()
		t.Fatal("expected the request to be cancelled")
	}
	e.waitIdle(svc.ID)
	waitFor(t, "CaptureSkipped", func() bool { return e.mgr.Status(svc.ID).CaptureSkipped == 1 })
	if got := e.captured(svc.RecordingID); len(got) != 0 {
		t.Errorf("stored %d exchanges for an abandoned request", len(got))
	}
}
