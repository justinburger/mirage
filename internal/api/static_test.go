package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
)

// wantSecurityHeaders checks the headers every response must carry.
func wantSecurityHeaders(t *testing.T, resp *http.Response) {
	t.Helper()
	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"X-Frame-Options":        "DENY",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'self'", "style-src 'self' 'unsafe-inline'", "object-src 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("Content-Security-Policy %q lacks %q", csp, directive)
		}
	}
	// Scripts must not get the inline exception.
	if strings.Contains(csp, "script-src") || strings.Contains(strings.ReplaceAll(csp, "style-src 'self' 'unsafe-inline'", ""), "unsafe-inline") {
		t.Errorf("Content-Security-Policy %q allows more than intended", csp)
	}
}

func TestStaticIndexAndAssets(t *testing.T) {
	e := newEnv(t)
	ui := testUI()
	tests := []struct {
		path     string
		file     string
		wantType string
	}{
		{"/", "index.html", "text/html; charset=utf-8"},
		{"/index.html", "index.html", "text/html; charset=utf-8"},
		{"/assets/app.js", "assets/app.js", "text/javascript; charset=utf-8"},
		{"/assets/js/router.js", "assets/js/router.js", "text/javascript; charset=utf-8"},
		{"/assets/app.css", "assets/app.css", "text/css; charset=utf-8"},
		{"/assets/favicon.svg", "assets/favicon.svg", "image/svg+xml"},
		{"/assets/data.unknownx", "assets/data.unknownx", "application/octet-stream"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			resp, body := e.do("GET", tc.path, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if string(body) != string(ui[tc.file].Data) {
				t.Errorf("body = %q", body)
			}
			if got := resp.Header.Get("Content-Type"); got != tc.wantType {
				t.Errorf("Content-Type = %q, want %q", got, tc.wantType)
			}
			if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
				t.Errorf("Cache-Control = %q, want no-cache", cc)
			}
			sum := sha256.Sum256(ui[tc.file].Data)
			if want := `"` + hex.EncodeToString(sum[:16]) + `"`; resp.Header.Get("ETag") != want {
				t.Errorf("ETag = %q, want the content hash %s", resp.Header.Get("ETag"), want)
			}
			wantSecurityHeaders(t, resp)
		})
	}
}

func TestStaticETagRevalidation(t *testing.T) {
	e := newEnv(t)
	first, _ := e.do("GET", "/assets/app.js", nil)
	etag := first.Header.Get("ETag")
	if etag == "" || strings.HasPrefix(etag, "W/") {
		t.Fatalf("ETag = %q, want a strong validator", etag)
	}
	other, _ := e.do("GET", "/assets/app.css", nil)
	if other.Header.Get("ETag") == etag {
		t.Error("different files share an ETag")
	}
	again, _ := e.do("GET", "/assets/app.js", nil)
	if again.Header.Get("ETag") != etag {
		t.Error("the ETag changed between requests")
	}

	tests := []struct {
		name   string
		inm    string
		status int
	}{
		{"matching", etag, 304},
		{"weak form of the same tag", "W/" + etag, 304},
		{"one of several", `"nope", ` + etag, 304},
		{"star", "*", 304},
		{"stale", `"0000"`, 200},
		{"other file's tag", other.Header.Get("ETag"), 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := e.send(request{method: "GET", path: "/assets/app.js", header: map[string]string{"If-None-Match": tc.inm}})
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if tc.status == 304 {
				if len(body) != 0 {
					t.Errorf("304 with a body: %q", body)
				}
				if resp.Header.Get("ETag") != etag || resp.Header.Get("Cache-Control") != "no-cache" {
					t.Errorf("304 lacks validators: %v", resp.Header)
				}
			}
			wantSecurityHeaders(t, resp)
		})
	}

	t.Run("the SPA fallback revalidates too", func(t *testing.T) {
		idx, _ := e.do("GET", "/", nil)
		resp, _ := e.send(request{method: "GET", path: "/recordings/rec_1", header: map[string]string{"If-None-Match": idx.Header.Get("ETag")}})
		if resp.StatusCode != 304 {
			t.Errorf("status = %d, want 304", resp.StatusCode)
		}
	})
}

func TestStaticSPAFallback(t *testing.T) {
	e := newEnv(t)
	_, index := e.do("GET", "/", nil)
	for _, path := range []string{"/services", "/services/svc_abc", "/recordings/rec_1/exchanges", "/replays/new", "/assets", "/assets/"} {
		resp, body := e.do("GET", path, nil)
		if resp.StatusCode != 200 || string(body) != string(index) {
			t.Errorf("GET %s = %d %q, want the index page", path, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("GET %s: Content-Type = %q", path, ct)
		}
		if resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("ETag") == "" {
			t.Errorf("GET %s: caching headers = %v", path, resp.Header)
		}
	}
}

func TestStaticMissingAssetsAre404(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/assets/missing.js", "/missing.css", "/assets/js/nope.map", "/favicon.ico"} {
		resp, body := e.do("GET", path, nil)
		if resp.StatusCode != 404 {
			t.Errorf("GET %s = %d %q, want 404", path, resp.StatusCode, body)
		}
		if strings.Contains(string(body), "<!doctype") {
			t.Errorf("GET %s answered with the index page", path)
		}
		wantSecurityHeaders(t, resp)
	}
}

func TestStaticAPIPathsNeverFallBackToTheUI(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/api/v1/nope", "/api/v2/services", "/api/", "/api"} {
		resp, body := e.do("GET", path, nil)
		if resp.StatusCode != 404 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			t.Errorf("GET %s = %d %s (%q)", path, resp.StatusCode, body, resp.Header.Get("Content-Type"))
		}
		var eb errorBody
		if err := json.Unmarshal(body, &eb); err != nil || !strings.Contains(eb.Error, "no such endpoint") {
			t.Errorf("GET %s: body %s", path, body)
		}
	}
}

func TestStaticMethods(t *testing.T) {
	e := newEnv(t)
	t.Run("HEAD", func(t *testing.T) {
		resp, body := e.do("HEAD", "/assets/app.js", nil)
		if resp.StatusCode != 200 || len(body) != 0 || resp.Header.Get("ETag") == "" {
			t.Errorf("HEAD = %d, %d bytes, headers %v", resp.StatusCode, len(body), resp.Header)
		}
	})
	t.Run("POST", func(t *testing.T) {
		resp, _ := e.do("POST", "/", "x")
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, HEAD" {
			t.Errorf("POST / = %d, Allow %q", resp.StatusCode, resp.Header.Get("Allow"))
		}
	})
}

func TestStaticPathCleaning(t *testing.T) {
	e := newEnv(t)
	// The client follows the mux's redirect to the cleaned path.
	for _, path := range []string{"/assets/../index.html", "//assets//app.js", "/assets/%2e%2e/assets/app.js", "/../../etc/passwd"} {
		resp, body := e.do("GET", path, nil)
		if resp.StatusCode != 200 && resp.StatusCode != 404 {
			t.Errorf("GET %s = %d", path, resp.StatusCode)
		}
		if strings.Contains(string(body), "root:") {
			t.Errorf("GET %s leaked a file", path)
		}
	}
}

func TestStaticDisabled(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.UI = nil })
	resp, _ := e.do("GET", "/", nil)
	if resp.StatusCode != 404 {
		t.Errorf("GET / = %d", resp.StatusCode)
	}
	wantSecurityHeaders(t, resp)
	// The API is unaffected.
	e.call("GET", "/api/v1/health", nil, http.StatusOK, nil)

	empty := newEnv(t, func(d *Deps) { d.UI = fstest.MapFS{} })
	if resp, _ := empty.do("GET", "/", nil); resp.StatusCode != 404 {
		t.Errorf("empty UI: GET / = %d", resp.StatusCode)
	}
}

func TestStaticWithoutIndex(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.UI = fstest.MapFS{"assets/app.js": {Data: []byte("x")}} })
	if resp, _ := e.do("GET", "/", nil); resp.StatusCode != 404 {
		t.Errorf("GET / = %d, want 404 without an index", resp.StatusCode)
	}
	if resp, _ := e.do("GET", "/assets/app.js", nil); resp.StatusCode != 200 {
		t.Errorf("GET /assets/app.js = %d", resp.StatusCode)
	}
}

// noSeekFS hides Seek from the files of an fs.FS.
type noSeekFS struct{ fs.FS }

func (n noSeekFS) Open(name string) (fs.File, error) {
	f, err := n.FS.Open(name)
	if err != nil {
		return nil, err
	}
	if info, err := f.Stat(); err == nil && info.IsDir() {
		return f, nil // directories keep ReadDir
	}
	return struct{ fs.File }{f}, nil // fs.File has no Seek
}

func TestStaticFilesThatCannotSeek(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.UI = noSeekFS{testUI()} })
	resp, body := e.do("GET", "/assets/app.js", nil)
	if resp.StatusCode != 200 || string(body) != "console.log('mirage');" {
		t.Errorf("GET = %d %q", resp.StatusCode, body)
	}
	resp, _ = e.send(request{method: "GET", path: "/assets/app.js", header: map[string]string{"If-None-Match": resp.Header.Get("ETag")}})
	if resp.StatusCode != 304 {
		t.Errorf("revalidation = %d", resp.StatusCode)
	}
}

func TestStaticRangeRequests(t *testing.T) {
	e := newEnv(t)
	resp, body := e.send(request{method: "GET", path: "/assets/app.js", header: map[string]string{"Range": "bytes=0-6"}})
	if resp.StatusCode != http.StatusPartialContent || string(body) != "console" {
		t.Errorf("range = %d %q", resp.StatusCode, body)
	}
}

func TestContentType(t *testing.T) {
	for name, want := range map[string]string{
		"a.js": "text/javascript; charset=utf-8", "A.JS": "text/javascript; charset=utf-8", "a.mjs": "text/javascript; charset=utf-8",
		"a.css": "text/css; charset=utf-8", "a.html": "text/html; charset=utf-8", "a.json": "application/json",
		"a.svg": "image/svg+xml", "a.png": "image/png", "a.woff2": "font/woff2", "noext": "application/octet-stream",
	} {
		if got := contentType(name); got != want {
			t.Errorf("contentType(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestSecurityHeadersOnEveryKindOfResponse(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/v1/health"},
		{"GET", "/api/v1/services/svc_nope"},
		{"POST", "/api/v1/services"},
		{"GET", "/api/v1/nope"},
		{"GET", "/metrics"},
		{"GET", "/"},
		{"GET", "/assets/nope.js"},
	} {
		resp, _ := e.do(tc.method, tc.path, nil)
		wantSecurityHeaders(t, resp)
		if t.Failed() {
			t.Fatalf("%s %s lacks security headers", tc.method, tc.path)
		}
	}
	// API responses are never cached.
	resp, _ := e.do("GET", "/api/v1/services", nil)
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("API Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
}

func TestStaticOpenReadsEachFileOnce(t *testing.T) {
	// The ETags are computed up front, from the files at start-up.
	counting := &countingFS{FS: testUI()}
	_ = newEnv(t, func(d *Deps) { d.UI = counting })
	startup := counting.opens
	if startup != len(testUI()) {
		t.Errorf("start-up opened %d files, want %d (one per file)", startup, len(testUI()))
	}
}

type countingFS struct {
	fs.FS
	opens int
}

func (c *countingFS) Open(name string) (fs.File, error) {
	f, err := c.FS.Open(name)
	if err == nil {
		if info, serr := f.Stat(); serr == nil && !info.IsDir() {
			c.opens++
		}
	}
	return f, err
}
