package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
	"github.com/justinburger/mirage/internal/store"
	"github.com/justinburger/mirage/internal/virtual"
)

// ---- anonymized captures never keep bodies the anonymizer cannot read -----

// piiJSON builds a JSON array of n records of unique, poorly compressible
// personal data.
func piiJSON(n int) (body []byte, emails []string) {
	rnd := rand.NewChaCha8([32]byte{7})
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < n; i++ {
		var b [6]byte
		rnd.Read(b[:])
		email := fmt.Sprintf("u%x@acme-%x.io", b[:3], b[3:])
		emails = append(emails, email)
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"email":%q,"ssn":"%03d-%02d-%04d"}`, email, 100+i%800, 10+i%80, 1000+i%9000)
	}
	sb.WriteString("]")
	return []byte(sb.String()), emails
}

func TestAnonymizedCaptureNeverKeepsUninspectableCompressedBodies(t *testing.T) {
	plain, emails := piiJSON(3000)
	wire := gz(t, plain)
	const capBytes = 16 << 10
	if len(wire) <= capBytes || len(plain) <= 4*capBytes {
		t.Fatalf("test setup: wire %d plain %d", len(wire), len(plain))
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/big-gzip":
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(wire)
		case "/brotli":
			w.Header().Set("Content-Encoding", "br")
			w.Write(plain[:2000]) // pretend it is brotli: the proxy cannot decode it
		case "/corrupt":
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(append([]byte("this is not gzip: "), plain[:2000]...))
		case "/stacked":
			w.Header().Set("Content-Encoding", "gzip, br")
			w.Write(wire[:3000])
		default:
			w.Write(plain[:2000])
		}
	}))
	defer up.Close()

	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) {
		s.AnonymizeCapture = true
		s.MaxBodyBytes = capBytes
	})
	base := e.start(svc)
	c := newClient(t)
	paths := []string{"/big-gzip", "/brotli", "/corrupt", "/stacked"}
	for _, p := range paths {
		do(t, c, "GET", base+p, nil, "Accept-Encoding", "gzip, br")
	}
	// A gzip request upload over the cap.
	do(t, c, "POST", base+"/upload", bytes.NewReader(wire), "Content-Encoding", "gzip", "Content-Type", "application/json")

	byPath := map[string]*model.Exchange{}
	for _, ex := range e.waitStored(svc.RecordingID, len(paths)+1) {
		byPath[ex.Request.Path] = ex
	}
	for _, p := range append(paths, "/upload") {
		ex := byPath[p]
		if ex == nil {
			t.Fatalf("%s not stored", p)
		}
		if !ex.Anonymized {
			t.Errorf("%s: not marked anonymized", p)
		}
		for _, side := range []struct {
			name string
			h    http.Header
			body []byte
			enc  string
		}{
			{"request", ex.Request.Header, ex.Request.Body, ex.Request.ContentEncoding},
			{"response", ex.Response.Header, ex.Response.Body, ex.Response.ContentEncoding},
		} {
			// Whatever is stored must be inspected plain data: no body that is
			// still compressed.
			if ce := side.h.Get("Content-Encoding"); ce != "" && len(side.body) > 0 {
				t.Errorf("%s %s: %d bytes stored still under Content-Encoding %q", p, side.name, len(side.body), ce)
			}
			if len(side.body) >= 2 && side.body[0] == 0x1f && side.body[1] == 0x8b {
				t.Errorf("%s %s: stored body is a raw gzip stream", p, side.name)
			}
			for _, em := range emails {
				if bytes.Contains(side.body, []byte(em)) {
					t.Fatalf("%s %s: stored body leaks raw email %q", p, side.name, em)
				}
			}
			// The decoded prefix is bounded by the cap (anonymized values may
			// be a little longer than the originals).
			if len(side.body) > 2*capBytes {
				t.Errorf("%s %s: stored %d bytes, cap %d", p, side.name, len(side.body), capBytes)
			}
		}
	}

	// The big gzip response is decoded (its readable prefix is kept, anonymized),
	// the others are not stored at all.
	r := byPath["/big-gzip"].Response
	if len(r.Body) == 0 || !r.BodyTruncated || r.ContentEncoding != "gzip" || r.BodySize <= int64(len(r.Body)) {
		t.Errorf("/big-gzip: body %d truncated %v ContentEncoding %q size %d", len(r.Body), r.BodyTruncated, r.ContentEncoding, r.BodySize)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(r.Body), []byte(`[{"email"`)) {
		t.Errorf("/big-gzip: stored body is not the decoded JSON prefix: %.60q", r.Body)
	}
	for _, p := range []string{"/brotli", "/corrupt", "/stacked"} {
		if r := byPath[p].Response; len(r.Body) != 0 || !r.BodyTruncated {
			t.Errorf("%s: an uninspectable compressed body must be dropped (and marked cut): %d bytes truncated %v", p, len(r.Body), r.BodyTruncated)
		}
	}
	if r := byPath["/upload"].Request; len(r.Body) == 0 || r.ContentEncoding != "gzip" || !r.BodyTruncated {
		t.Errorf("/upload: request body %d truncated %v ContentEncoding %q", len(r.Body), r.BodyTruncated, r.ContentEncoding)
	}
}

func TestPartialGzipStaysEncodedWithoutAnonymization(t *testing.T) {
	// Documented behaviour for recordings that are not anonymized: unchanged.
	rnd := make([]byte, 40<<10)
	rand.NewChaCha8([32]byte{2}).Read(rnd)
	wire := gz(t, rnd)
	body, _, trunc, _, ok := decodeBody(http.Header{"Content-Encoding": {"gzip"}}, wire[:8<<10], int64(len(wire)), true, 1<<20)
	if ok || trunc || body != nil {
		t.Fatalf("decodeBody must still refuse a cut stream: ok=%v", ok)
	}
}

func TestDecodeBodyLenient(t *testing.T) {
	plain := []byte(strings.Repeat("lenient decoding of a cut stream ", 4000))
	rnd := rand.NewChaCha8([32]byte{3})
	mixed := make([]byte, 0, 2*len(plain))
	for len(mixed) < 2*len(plain) { // semi-compressible so the wire form is large
		var b [8]byte
		rnd.Read(b[:])
		mixed = append(mixed, fmt.Sprintf("%x ", b[:])...)
	}
	wire := gz(t, mixed)
	h := func() http.Header { return http.Header{"Content-Encoding": {"gzip"}} }

	t.Run("cut stream keeps the decoded prefix", func(t *testing.T) {
		cut := wire[:len(wire)/2]
		out, size, trunc, enc, ok := decodeBodyLenient(h(), cut, int64(len(wire)), true, 1<<20)
		if !ok || !trunc || enc != "gzip" {
			t.Fatalf("ok=%v truncated=%v enc=%q", ok, trunc, enc)
		}
		if len(out) == 0 || !bytes.HasPrefix(mixed, out) {
			t.Errorf("decoded %d bytes, prefix of the original: %v", len(out), bytes.HasPrefix(mixed, out))
		}
		if size <= int64(len(out)) {
			t.Errorf("size %d must exceed the %d bytes kept", size, len(out))
		}
	})
	t.Run("limit applies", func(t *testing.T) {
		out, size, trunc, _, ok := decodeBodyLenient(h(), wire[:len(wire)/2], int64(len(wire)), true, 100)
		if !ok || !trunc || len(out) != 100 || size < 101 {
			t.Errorf("ok=%v trunc=%v len=%d size=%d", ok, trunc, len(out), size)
		}
	})
	t.Run("complete stream behaves like decodeBody", func(t *testing.T) {
		out, size, trunc, enc, ok := decodeBodyLenient(h(), wire, int64(len(wire)), false, 1<<30)
		if !ok || trunc || enc != "gzip" || !bytes.Equal(out, mixed) || size != int64(len(mixed)) {
			t.Errorf("ok=%v trunc=%v len=%d size=%d", ok, trunc, len(out), size)
		}
	})
	t.Run("not a gzip stream", func(t *testing.T) {
		if _, _, _, _, ok := decodeBodyLenient(h(), []byte("plain text, no gzip"), 19, false, 1<<20); ok {
			t.Error("a body without a gzip header must not decode")
		}
	})
	t.Run("a stream cut before any output yields nothing", func(t *testing.T) {
		if _, _, _, _, ok := decodeBodyLenient(h(), wire[:12], int64(len(wire)), true, 1<<20); ok {
			t.Error("no decoded bytes: nothing to keep")
		}
	})
}

// ---- stored endpoints never carry raw path values -------------------------

func TestAnonymizedEndpointDoesNotLeakRawPathValues(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.AnonymizeCapture = true })
	base := e.start(svc)
	c := newClient(t)
	// Names in paths are not something the default profile detects: the stored
	// path keeps them too, so the endpoint cannot do better than the path.
	raw := []string{"123-45-6789", "4111-1111-1111-1111", "+14155552671", "203.0.113.9"}
	for _, p := range []string{
		"/people/123-45-6789/profile",
		"/pay/4111-1111-1111-1111",
		"/phone/%2B14155552671",
		"/ip/203.0.113.9/x",
		"/users/42/orders", // ids still collapse into one endpoint
		"/users/77/orders",
	} {
		do(t, c, "GET", base+p, nil)
	}
	got := e.waitStored(svc.RecordingID, 6)
	counts := map[string]int{}
	for _, ex := range got {
		for _, r := range raw {
			if strings.Contains(ex.Endpoint, r) || strings.Contains(ex.Endpoint, strings.TrimPrefix(r, "+")) {
				t.Errorf("stored endpoint %q leaks %q (path %q)", ex.Endpoint, r, ex.Request.Path)
			}
		}
		counts[ex.Endpoint]++
	}
	if counts["GET /users/{id}/orders"] != 2 {
		t.Errorf("numeric ids must still collapse into a template: %v", counts)
	}
}

func TestAnonymizedEndpointKeepsRawTemplatePlaceholders(t *testing.T) {
	n := pathnorm.Default
	for _, tc := range []struct {
		name, method, rawPath, anonPath, want string
	}{
		{"literal replaced", "GET", "/people/123-45-6789/profile", "/people/356-82-0822/profile", "GET /people/356-82-0822/profile"},
		{"template from the raw path wins", "GET", "/users/42/orders", "/users/****42/orders", "GET /users/{id}/orders"},
		{"mixed", "POST", "/a/7/b/Jane-Roe", "/a/9/b/Mia-Cole", "POST /a/{id}/b/Mia-Cole"},
		{"unchanged path", "GET", "/healthz", "/healthz", "GET /healthz"},
		{"segment count changed", "GET", "/a/b", "/a/b/c", "GET /a/b/c"},
	} {
		if got := anonymizedEndpoint(n, tc.method, tc.rawPath, tc.anonPath); got != tc.want {
			t.Errorf("%s: anonymizedEndpoint = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ---- hybrid learning --------------------------------------------------------

func TestLearnableRejectsConditionalAndTransientResponses(t *testing.T) {
	mk := func(status int, hdr ...string) *model.Exchange {
		ex := &model.Exchange{Request: model.Request{Method: "GET", Header: http.Header{}}, Response: model.Response{Status: status}}
		for i := 0; i+1 < len(hdr); i += 2 {
			ex.Request.Header.Set(hdr[i], hdr[i+1])
		}
		return ex
	}
	for _, tc := range []struct {
		name string
		ex   *model.Exchange
		want bool
	}{
		{"200", mk(200), true},
		{"204", mk(204), true},
		{"301", mk(301), true},
		{"404", mk(404), true},
		{"410", mk(410), true},
		{"100", mk(100), false},
		{"206", mk(206), false},
		{"304", mk(304), false},
		{"408", mk(408), false},
		{"412", mk(412), false},
		{"416", mk(416), false},
		{"425", mk(425), false},
		{"429", mk(429), false},
		{"503", mk(503), false},
		{"200 with Range", mk(200, "Range", "bytes=0-3"), false},
		{"200 with If-Range", mk(200, "If-Range", `"x"`), false},
		{"200 with If-None-Match", mk(200, "If-None-Match", `"x"`), false},
		{"200 with If-Modified-Since", mk(200, "If-Modified-Since", "Mon, 02 Jan 2006 15:04:05 GMT"), false},
		{"200 with If-Match", mk(200, "If-Match", `"x"`), false},
		{"200 with If-Unmodified-Since", mk(200, "If-Unmodified-Since", "Mon, 02 Jan 2006 15:04:05 GMT"), false},
		{"200 with unrelated header", mk(200, "Accept", "*/*"), true},
	} {
		if got := learnable(tc.ex); got != tc.want {
			t.Errorf("%s: learnable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestHybridDoesNotServeConditionalOrThrottledAnswersToPlainRequests(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("If-None-Match") != "":
			w.WriteHeader(http.StatusNotModified)
		case r.Header.Get("Range") != "":
			w.Header().Set("Content-Range", "bytes 0-3/10")
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte("0123"))
		case r.Header.Get("X-Busy") != "":
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte("slow down"))
		default:
			w.Write([]byte("0123456789"))
		}
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeHybrid)
	base := e.start(svc)
	c := newClient(t)
	for _, tc := range []struct {
		path, hdr, val string
		status         int
	}{
		{"/a", "If-None-Match", `"v1"`, 304},
		{"/b", "Range", "bytes=0-3", 206},
		{"/c", "X-Busy", "1", 429},
	} {
		if status, _, _ := do(t, c, "GET", base+tc.path, nil, tc.hdr, tc.val); status != tc.status {
			t.Fatalf("%s: first status %d, want %d", tc.path, status, tc.status)
		}
	}
	e.waitStored(svc.RecordingID, 3)
	// Give the (wrongly) learning pipeline time to act, then check.
	time.Sleep(50 * time.Millisecond)
	if n := e.virts.last().addedCount(); n != 0 {
		t.Fatalf("learned %d conditional or transient exchanges", n)
	}
	for _, p := range []string{"/a", "/b", "/c"} {
		status, hdr, body := do(t, c, "GET", base+p, nil)
		if status != 200 || string(body) != "0123456789" || hdr.Get("X-Mirage-Match") != "" {
			t.Errorf("%s plain follow-up: %d %q %v; the upstream would answer 200", p, status, body, hdr)
		}
	}
}

// ---- record -> hybrid switch ------------------------------------------------

func TestRecordToHybridSwitchLearnsCapturesStillQueued(t *testing.T) {
	up := newEcho(t)
	e, _, release := gatedEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	if status, _, _ := do(t, newClient(t), "GET", base+"/thing", nil); status != 200 {
		t.Fatal(status)
	}
	// The capture is queued behind the blocked store; switch to hybrid now.
	svc.Mode = model.ModeHybrid
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	vf := e.virts.last()
	if vf == nil || vf.Len() != 0 {
		t.Fatalf("the new virtualizer should start empty (the capture was not stored yet): %+v", vf)
	}
	release()
	waitFor(t, "the queued capture to be stored", func() bool { return e.mgr.Status(svc.ID).Captured == 1 })
	waitFor(t, "the queued capture to be learned by the new virtualizer", func() bool { return vf.addedCount() == 1 })
	if status, hdr, _ := do(t, newClient(t), "GET", base+"/thing", nil); status != 200 || hdr.Get("X-Mirage-Match") != "exact" {
		t.Errorf("second request should be served virtually: %d %v", status, hdr)
	}
	if up.hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", up.hits.Load())
	}
}

func TestHybridToRecordSwitchDoesNotLearn(t *testing.T) {
	up := newEcho(t)
	e, _, release := gatedEnv(t)
	svc := e.service(up.URL, model.ModeHybrid)
	base := e.start(svc)
	do(t, newClient(t), "GET", base+"/thing", nil)
	vf := e.virts.last()
	svc.Mode = model.ModeRecord
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	release()
	waitFor(t, "the capture to be stored", func() bool { return e.mgr.Status(svc.ID).Captured == 1 })
	if n := vf.addedCount(); n != 0 {
		t.Errorf("learned %d exchanges after leaving hybrid mode", n)
	}
}

// ---- client aborts during simulated latency -------------------------------

// stallVirt is a Virtualizer that, like virtual.Respond during simulated
// latency, returns without writing when the request context ends.
type stallVirt struct{ *fakeVirt }

func (s *stallVirt) Serve(w http.ResponseWriter, r *http.Request, body []byte) bool {
	<-r.Context().Done()
	return true
}

func TestVirtualClientAbortIsNotRecordedAsSuccess(t *testing.T) {
	e := newEnv(t, func(d *Deps) {
		d.Virtualizers = func(ctx context.Context, svc model.Service, n *pathnorm.Normalizer) (Virtualizer, error) {
			return &stallVirt{fakeVirt: newFakeVirt()}, nil
		}
	})
	svc := e.service("", model.ModeVirtualize)
	base := e.start(svc)
	c := &http.Client{Timeout: 100 * time.Millisecond}
	if _, err := c.Get(base + "/slow"); err == nil {
		t.Fatal("expected the client to time out")
	}
	snap := e.waitIdle(svc.ID)
	if snap.Requests != 1 || snap.Errors != 1 || snap.StatusClasses["2xx"] != 0 {
		t.Errorf("a request the client gave up on must not count as a 2xx success: requests=%d errors=%d classes=%v", snap.Requests, snap.Errors, snap.StatusClasses)
	}
}

// ---- queue accounting ---------------------------------------------------------

func TestCaptureQueueChargesDecodedBodySize(t *testing.T) {
	plain := bytes.Repeat([]byte("a"), 100<<10)
	wire := gz(t, plain)
	if len(wire) > 1<<10 {
		t.Fatalf("test setup: wire %d", len(wire))
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(wire)
	}))
	defer up.Close()
	// Room for about three decoded bodies, but for dozens of encoded ones.
	e, _, release := gatedEnv(t, func(d *Deps) {
		d.CaptureQueueBytes = 350 << 10
		d.CaptureWorkers = 1
		d.CaptureBatchSize = 100
		d.CaptureFlushInterval = time.Hour
	})
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.MaxBodyBytes = 1 << 20 })
	base := e.start(svc)
	c := newClient(t)
	const n = 20
	for i := 0; i < n; i++ {
		do(t, c, "GET", fmt.Sprintf("%s/g%d", base, i), nil, "Accept-Encoding", "gzip")
	}
	waitFor(t, "captures to be dropped once the decoded bytes exceed the bound", func() bool {
		return e.mgr.Status(svc.ID).CaptureDropped >= n-5
	})
	if q := e.mgr.pipe.queuedBytes.Load(); q > 350<<10+200<<10 {
		t.Errorf("%d bytes charged against a 350 KiB bound", q)
	}
	release()
	if err := e.mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if q := e.mgr.pipe.queuedBytes.Load(); q != 0 {
		t.Errorf("%d bytes still charged after the pipeline drained", q)
	}
	st := e.mgr.Status(svc.ID)
	if st.Captured == 0 || st.Captured+st.CaptureDropped != n {
		t.Errorf("captured %d dropped %d of %d", st.Captured, st.CaptureDropped, n)
	}
}

func TestCaptureQueueChargesHeaderOverhead(t *testing.T) {
	up := bodyUpstream(t, 0)
	e, _, release := gatedEnv(t, func(d *Deps) {
		d.CaptureQueueBytes = 6 << 10
		d.CaptureWorkers = 1
		d.CaptureBatchSize = 1
	})
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	c := newClient(t)
	// Empty bodies, but many headers: every capture weighs about 2 KiB.
	hdrs := []string{}
	for i := 0; i < 10; i++ {
		hdrs = append(hdrs, fmt.Sprintf("X-Pad-%d", i), strings.Repeat("v", 40))
	}
	const n = 30
	for i := 0; i < n; i++ {
		do(t, c, "GET", fmt.Sprintf("%s/h%d", base, i), nil, hdrs...)
	}
	waitFor(t, "most captures to be dropped by the memory bound", func() bool {
		return e.mgr.Status(svc.ID).CaptureDropped >= n-10
	})
	release()
}

func TestExchangeOverheadEstimate(t *testing.T) {
	small := &model.Exchange{Request: model.Request{Header: http.Header{}}}
	big := &model.Exchange{Request: model.Request{Header: http.Header{}}, Response: model.Response{Header: http.Header{}}}
	for i := 0; i < 20; i++ {
		big.Request.Header.Set(fmt.Sprintf("X-H-%d", i), strings.Repeat("v", 30))
		big.Response.Header.Set(fmt.Sprintf("X-R-%d", i), strings.Repeat("w", 30))
	}
	if a, b := exchangeOverhead(small), exchangeOverhead(big); a <= 0 || b < a+40*(30+8) {
		t.Errorf("overhead small=%d big=%d", a, b)
	}
}

func TestDecodeBodyAvoidsSlackForGzip(t *testing.T) {
	plain := bytes.Repeat([]byte("0123456789abcdef"), 6000)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(plain)
	zw.Close()
	out, size, trunc, _, ok := decodeBody(http.Header{"Content-Encoding": {"gzip"}}, buf.Bytes(), int64(buf.Len()), false, 1<<20)
	if !ok || trunc || size != int64(len(plain)) || !bytes.Equal(out, plain) {
		t.Fatalf("ok=%v trunc=%v size=%d", ok, trunc, size)
	}
	if cap(out) != len(out) {
		t.Errorf("decoded body keeps %d bytes of slack", cap(out)-len(out))
	}
}

// realEngineFactory builds the real virtual engine, like internal/app does.
func realEngineFactory(st store.Store) VirtualizerFactory {
	return func(ctx context.Context, svc model.Service, n *pathnorm.Normalizer) (Virtualizer, error) {
		eng, err := virtual.Build(ctx, st, virtual.OptionsFromService(svc, n))
		if err != nil {
			return nil, err
		}
		return eng, nil
	}
}

func TestRecordToHybridSwitchWithTheRealEngineIndexesEachCaptureOnce(t *testing.T) {
	up := newEcho(t)
	e, _, release := gatedEnv(t, func(d *Deps) { d.Virtualizers = realEngineFactory(d.Store) })
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.Latency.Mode = model.LatencyNone })
	base := e.start(svc)
	c := newClient(t)
	do(t, c, "GET", base+"/thing", nil)
	// Switch while the capture is queued behind the blocked store.
	svc.Mode = model.ModeHybrid
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	release()
	waitFor(t, "the capture to be stored", func() bool { return e.mgr.Status(svc.ID).Captured == 1 })
	waitFor(t, "the capture to be indexed", func() bool { return e.mgr.Status(svc.ID).IndexedExchanges == 1 })
	if status, hdr, _ := do(t, c, "GET", base+"/thing", nil); status != 200 || hdr.Get("X-Mirage-Match") == "" {
		t.Errorf("second request should be served virtually: %d %v", status, hdr)
	}
	if up.hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", up.hits.Load())
	}
	// A reload while nothing is queued does not index the stored capture twice.
	if err := e.mgr.Reload(context.Background(), svc.ID); err != nil {
		t.Fatal(err)
	}
	if n := e.mgr.Status(svc.ID).IndexedExchanges; n != 1 {
		t.Errorf("IndexedExchanges = %d after a reload, want 1", n)
	}
}
