package proxy

import (
	"bytes"
	"crypto/sha256"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func TestHybridMissIsProxiedCapturedLearnedAndThenServedVirtually(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeHybrid)
	base := e.start(svc)
	c := newClient(t)

	status, hdr, first := do(t, c, "GET", base+"/items/1?x=1", nil)
	if status != 200 || hdr.Get("X-Upstream") != "echo" || hdr.Get("X-Mirage-Match") != "" {
		t.Fatalf("first request should be proxied: %d %v", status, hdr)
	}
	vf := e.virts.last()
	waitFor(t, "the exchange to be learned", func() bool { return vf.addedCount() == 1 })

	status, hdr, second := do(t, c, "GET", base+"/items/1?x=1", nil)
	if status != 200 || hdr.Get("X-Mirage-Match") != "exact" {
		t.Fatalf("second request should be served virtually: %d %v", status, hdr)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("virtual response differs from the proxied one:\n%s\n%s", first, second)
	}
	if up.hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", up.hits.Load())
	}
	if got := e.captured(svc.RecordingID); len(got) != 1 {
		t.Errorf("a virtual hit must not be captured: %d exchanges", len(got))
	}
	st := e.mgr.Status(svc.ID)
	if st.Captured != 1 || st.VirtualHits != 1 || st.VirtualMisses != 1 || st.IndexedExchanges != 1 {
		t.Errorf("status = %+v", st)
	}
}

func TestHybridServesWhatTheRecordingAlreadyHolds(t *testing.T) {
	up := failIfHit(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeHybrid)
	seedExchange(t, e, svc.RecordingID, "GET", "/known", 200, `{"known":true}`)
	base := e.start(svc)
	status, hdr, body := do(t, newClient(t), "GET", base+"/known", nil)
	if status != 200 || bodyOf(body) != `{"known":true}` || hdr.Get("X-Mirage-Match") != "exact" {
		t.Errorf("%d %v %s", status, hdr, body)
	}
}

func TestHybridOnlyLearnsSafeSuccessfulExchanges(t *testing.T) {
	bigBody := strings.Repeat("b", 5000)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/err500":
			w.WriteHeader(500)
		case "/missing":
			w.WriteHeader(404)
		case "/big":
			io.WriteString(w, bigBody)
		default:
			io.WriteString(w, "ok")
		}
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeHybrid, func(s *model.Service) { s.MaxBodyBytes = 100 })
	base := e.start(svc)
	c := newClient(t)

	type req struct {
		method, path string
		learned      bool
	}
	reqs := []req{
		{"GET", "/ok", true},
		{"POST", "/ok", false},    // writes always reach the upstream
		{"PUT", "/ok", false},     //
		{"DELETE", "/ok", false},  //
		{"GET", "/err500", false}, // transient failures are not learned
		{"GET", "/missing", true}, // a 404 is a legitimate answer
		{"GET", "/big", false},    // truncated bodies cannot be replayed
		{"HEAD", "/ok", true},     //
		{"OPTIONS", "/ok", true},  //
		{"GET", "/ok?q=2", true},  //
	}
	want := 0
	for _, r := range reqs {
		do(t, c, r.method, base+r.path, nil)
		if r.learned {
			want++
		}
	}
	e.waitStored(svc.RecordingID, len(reqs))
	vf := e.virts.last()
	waitFor(t, "learned exchanges", func() bool { return vf.addedCount() >= want })
	if got := vf.addedCount(); got != want {
		t.Errorf("learned %d exchanges, want %d", got, want)
	}
	vf.mu.RLock()
	defer vf.mu.RUnlock()
	for _, ex := range vf.added {
		switch ex.Request.Method {
		case "GET", "HEAD", "OPTIONS":
		default:
			t.Errorf("learned a %s", ex.Request.Method)
		}
		if ex.Response.Status >= 500 || ex.Error != "" || ex.Response.BodyTruncated {
			t.Errorf("learned an unsafe exchange: %+v", ex.Summary())
		}
		if ex.ID == 0 || ex.Seq == 0 || ex.RecordingID != svc.RecordingID {
			t.Errorf("a learned exchange is exactly what was stored (ids assigned): %+v", ex.Summary())
		}
	}
}

func TestHybridDoesNotLearnUpstreamFailures(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	e := newEnv(t)
	svc := e.service(url, model.ModeHybrid)
	base := e.start(svc)
	if status, _, _ := do(t, newClient(t), "GET", base+"/x", nil); status != 502 {
		t.Fatalf("status %d", status)
	}
	e.waitStored(svc.RecordingID, 1)
	if n := e.virts.last().addedCount(); n != 0 {
		t.Errorf("learned %d upstream failures", n)
	}
}

func TestHybridLargeRequestBodyReachesUpstreamIntact(t *testing.T) {
	const size, capBytes = 200 << 10, 4096
	data := make([]byte, size)
	rand.NewChaCha8([32]byte{9}).Read(data)
	want := sha256.Sum256(data)

	var gotSum atomic.Value
	var gotLen atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := sha256.New()
		n, _ := io.Copy(h, r.Body)
		gotLen.Store(n)
		var s [32]byte
		copy(s[:], h.Sum(nil))
		gotSum.Store(s)
		io.WriteString(w, "stored")
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeHybrid, func(s *model.Service) { s.MaxBodyBytes = capBytes })
	base := e.start(svc)

	small := []byte("short body")
	if status, _, _ := do(t, newClient(t), "POST", base+"/small", bytes.NewReader(small)); status != 200 {
		t.Fatal(status)
	}
	if gotLen.Load() != int64(len(small)) {
		t.Errorf("small body: upstream got %d bytes", gotLen.Load())
	}
	if status, _, body := do(t, newClient(t), "POST", base+"/large", bytes.NewReader(data)); status != 200 || string(body) != "stored" {
		t.Fatalf("%d %s", status, body)
	}
	if gotLen.Load() != size || gotSum.Load().([32]byte) != want {
		t.Errorf("upstream got %d bytes (intact: %v), want %d", gotLen.Load(), gotSum.Load().([32]byte) == want, size)
	}

	bodies := e.virts.last().seenBodies()
	if len(bodies) != 2 || string(bodies[0]) != string(small) || !bytes.Equal(bodies[1], data[:capBytes]) {
		t.Errorf("engine bodies: %d entries, lengths %v", len(bodies), lens(bodies))
	}
	byPath := map[string]*model.Exchange{}
	for _, ex := range e.waitStored(svc.RecordingID, 2) {
		byPath[ex.Request.Path] = ex
	}
	l := byPath["/large"]
	if l == nil || len(l.Request.Body) != capBytes || l.Request.BodySize != size || !l.Request.BodyTruncated || !bytes.Equal(l.Request.Body, data[:capBytes]) {
		t.Errorf("large request capture: %+v", l)
	}
	if s := byPath["/small"]; s == nil || string(s.Request.Body) != string(small) || s.Request.BodySize != int64(len(small)) || s.Request.BodyTruncated {
		t.Errorf("small request capture: %+v", s)
	}
	if st := e.waitIdle(svc.ID); st.BytesIn != size+int64(len(small)) {
		t.Errorf("BytesIn = %d", st.BytesIn)
	}
}

func lens(bs [][]byte) []int {
	out := make([]int, len(bs))
	for i, b := range bs {
		out[i] = len(b)
	}
	return out
}

func TestHybridUnsampledMissIsProxiedButNotStoredOrLearned(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeHybrid, func(s *model.Service) { s.CaptureSample = 1e-12 })
	base := e.start(svc)
	if status, _, _ := do(t, newClient(t), "GET", base+"/s", nil); status != 200 {
		t.Fatal(status)
	}
	st := e.mgr.Status(svc.ID)
	if st.CaptureSkipped != 1 || st.Captured != 0 {
		t.Errorf("status = %+v", st)
	}
	if got := e.virts.last().addedCount(); got != 0 {
		t.Errorf("learned %d unsampled exchanges", got)
	}
	// The client's Accept-Encoding is left alone for an unsampled request.
	do(t, newClient(t), "GET", base+"/s2", nil, "Accept-Encoding", "br")
	if got := up.lastRequest().Header.Get("Accept-Encoding"); got != "br" {
		t.Errorf("Accept-Encoding = %q: unsampled requests are plain passthrough", got)
	}
}

func TestHybridIgnoresUpgrades(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeHybrid)
	base := e.start(svc)
	// A plain request carrying an Upgrade header that the upstream ignores.
	do(t, newClient(t), "GET", base+"/ws", nil, "Connection", "Upgrade", "Upgrade", "websocket")
	st := e.mgr.Status(svc.ID)
	if st.Captured != 0 || st.CaptureSkipped != 0 {
		t.Errorf("upgrade requests are neither captured nor counted: %+v", st)
	}
}
