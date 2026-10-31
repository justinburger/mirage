package virtual

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

// A recorded response without a Content-Type is served without one. Without
// the header key net/http would sniff "text/plain; charset=utf-8" and the
// client would see a header the upstream never sent.
func TestNoContentTypeIsNotSniffed(t *testing.T) {
	noCT := func(e *model.Exchange) { delete(e.Response.Header, "Content-Type") }
	e := newTestEngine(t, Options{},
		mkEx(1, "GET", "/bare", 200, "hello, this looks like plain text", noCT),
		mkEx(2, "GET", "/html", 200, "<html><body>hi</body></html>", noCT),
		mkEx(3, "GET", "/typed", 200, `{"a":1}`),
		mkEx(4, "GET", "/gone", 204, "", noCT),
	)
	base := newTestServer(t, e)

	for _, p := range []string{"/bare", "/html", "/gone"} {
		resp, err := http.Get(base + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = readAll(t, resp)
		if _, has := resp.Header["Content-Type"]; has {
			t.Errorf("GET %s: Content-Type = %q, the recording has none", p, resp.Header.Get("Content-Type"))
		}
	}
	resp, err := http.Get(base + "/typed")
	if err != nil {
		t.Fatal(err)
	}
	_ = readAll(t, resp)
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("recorded Content-Type = %q", got)
	}

	// The writer's header map is never shared with the index.
	rec, _ := serve(e, mkReq("GET", "/bare"), "")
	if ct, has := rec.Header()["Content-Type"]; has && len(ct) != 0 {
		t.Errorf("Content-Type = %v", ct)
	}
}

// Misses and other generated answers keep their own Content-Type.
func TestGeneratedAnswersKeepContentType(t *testing.T) {
	e := newTestEngine(t, Options{ErrorFloor: 1}, mkEx(1, "GET", "/s", 200, "fine", func(e *model.Exchange) { delete(e.Response.Header, "Content-Type") }))
	rec, _ := serve(e, mkReq("GET", "/s"), "")
	if rec.Header().Get("Content-Type") != "application/json" || rec.Code != http.StatusServiceUnavailable {
		t.Errorf("synthetic 503: code=%d hdr=%v", rec.Code, rec.Header())
	}
}

func headEx(seq int64, target string, status int, contentLength string) *model.Exchange {
	ex := mkEx(seq, "HEAD", target, status, "")
	if contentLength != "" {
		ex.Response.Header.Set("Content-Length", contentLength)
	}
	return ex
}

// A HEAD answered from a recorded HEAD exchange announces the length the
// upstream announced, not the zero bytes of its empty recorded body.
func TestHeadAnswersWithRecordedContentLength(t *testing.T) {
	e := newTestEngine(t, Options{},
		headEx(1, "/file", 200, "1234"),
		headEx(2, "/nolen", 200, ""),
		headEx(3, "/bad", 200, "12x"),
		mkEx(4, "GET", "/file", 200, "short"),
		mkEx(5, "GET", "/only-get", 200, "twelve bytes"),
	)
	base := newTestServer(t, e)

	req := func(method, path string) *http.Response {
		r, err := http.NewRequest(method, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = readAll(t, resp)
		return resp
	}

	head := req("HEAD", "/file")
	if head.StatusCode != 200 || head.ContentLength != 1234 || head.Header.Get("Content-Length") != "1234" {
		t.Errorf("HEAD /file: code=%d length=%d hdr=%v", head.StatusCode, head.ContentLength, head.Header)
	}
	// The GET recording of the same URL still answers GET with its own body.
	if get := req("GET", "/file"); get.ContentLength != 5 {
		t.Errorf("GET /file: length = %d, want 5", get.ContentLength)
	}
	// A HEAD answered from a GET recording announces the GET body length.
	if head := req("HEAD", "/only-get"); head.ContentLength != 12 {
		t.Errorf("HEAD /only-get: length = %d, want 12", head.ContentLength)
	}
	// Without a usable recorded length the payload length (0) stays.
	for _, p := range []string{"/nolen", "/bad"} {
		if head := req("HEAD", p); head.ContentLength != 0 {
			t.Errorf("HEAD %s: length = %d, want 0", p, head.ContentLength)
		}
	}
}

// The length of a recorded HEAD also survives lazy loading (the index keeps
// no response headers then).
func TestHeadContentLengthLazy(t *testing.T) {
	st := openStore(t)
	rec := fill(t, st,
		headEx(1, "/file", 200, "4096"),
		mkEx(2, "GET", "/big", 200, bigBody(1, 4096)),
	)
	e := build(t, st, rec, Options{MaxResidentBytes: 16})
	if !e.Lazy() {
		t.Fatal("engine is not lazy")
	}
	r, _ := serve(e, mkReq("HEAD", "/file"), "")
	if got := r.Header().Get("Content-Length"); got != "4096" || r.Body.Len() != 0 {
		t.Errorf("HEAD /file: Content-Length = %q body=%d", got, r.Body.Len())
	}
	r, _ = serve(e, mkReq("HEAD", "/big"), "")
	if got := r.Header().Get("Content-Length"); got != strconv.Itoa(4096) {
		t.Errorf("HEAD /big: Content-Length = %q", got)
	}
}

// HEAD never announces a recorded length on a status without a body, and a
// recorded Content-Length on a GET exchange is still recomputed.
func TestRecordedContentLengthIsRecomputedForGet(t *testing.T) {
	e := newTestEngine(t, Options{},
		mkEx(1, "GET", "/g", 200, "abc", withRespHeader("Content-Length", "999")),
		func() *model.Exchange { x := headEx(2, "/nm", 304, "77"); return x }(),
	)
	if rec, _ := serve(e, mkReq("GET", "/g"), ""); rec.Header().Get("Content-Length") != "3" {
		t.Errorf("GET: Content-Length = %q", rec.Header().Get("Content-Length"))
	}
	if rec, _ := serve(e, mkReq("HEAD", "/g"), ""); rec.Header().Get("Content-Length") != "3" {
		t.Errorf("HEAD from GET: Content-Length = %q", rec.Header().Get("Content-Length"))
	}
	rec, _ := serve(e, mkReq("HEAD", "/nm"), "")
	if rec.Code != 304 || strings.TrimSpace(rec.Header().Get("Content-Length")) != "" {
		t.Errorf("304: code=%d Content-Length=%q", rec.Code, rec.Header().Get("Content-Length"))
	}
}
