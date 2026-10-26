package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
)

const testToken = "s3cr3t-admin-token"

func authEnv(t *testing.T) *env {
	t.Helper()
	return newEnv(t, func(d *Deps) { d.AdminToken = testToken })
}

func TestAuthRequired(t *testing.T) {
	e := authEnv(t)
	protected := []struct{ method, path string }{
		{"GET", "/api/v1/info"},
		{"GET", "/api/v1/services"},
		{"POST", "/api/v1/services"},
		{"DELETE", "/api/v1/services/svc_x"},
		{"GET", "/api/v1/recordings"},
		{"GET", "/api/v1/recordings/rec_x/export.har"},
		{"POST", "/api/v1/recordings/import"},
		{"GET", "/api/v1/profiles"},
		{"GET", "/api/v1/faults"},
		{"GET", "/api/v1/replays"},
		{"GET", "/api/v1/stats"},
		{"GET", "/api/v1/stream"},
		{"GET", "/metrics"},
		// Unknown API paths do not reveal whether they exist.
		{"GET", "/api/v1/nonexistent"},
		{"GET", "/api/"},
		{"GET", "/api"},
	}
	for _, tc := range protected {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			resp, body := e.send(request{method: tc.method, path: tc.path, noAuth: true})
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
				t.Errorf("Content-Type = %q", resp.Header.Get("Content-Type"))
			}
			if !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Bearer") {
				t.Errorf("WWW-Authenticate = %q", resp.Header.Get("WWW-Authenticate"))
			}
			if !strings.Contains(string(body), `"error"`) {
				t.Errorf("body = %s", body)
			}
			if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
				t.Error("security headers are missing on a 401")
			}
		})
	}
}

func TestAuthOpenPaths(t *testing.T) {
	e := authEnv(t)
	for _, path := range []string{"/api/v1/health", "/", "/index.html", "/assets/app.js", "/assets/app.css", "/services/svc_1", "/recordings"} {
		resp, body := e.send(request{method: "GET", path: path, noAuth: true})
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s without credentials = %d: %s", path, resp.StatusCode, body)
		}
	}
	// The health check reveals little: no data path, no detectors.
	_, body := e.send(request{method: "GET", path: "/api/v1/health", noAuth: true})
	if strings.Contains(string(body), "data_path") || strings.Contains(string(body), ":memory:") {
		t.Errorf("health leaks configuration: %s", body)
	}
}

func TestAuthMethods(t *testing.T) {
	e := authEnv(t)
	tests := []struct {
		name   string
		path   string
		header map[string]string
		want   int
	}{
		{"bearer", "/api/v1/services", map[string]string{"Authorization": "Bearer " + testToken}, 200},
		{"bearer scheme is case-insensitive", "/api/v1/services", map[string]string{"Authorization": "bearer " + testToken}, 200},
		{"bearer with extra space", "/api/v1/services", map[string]string{"Authorization": "Bearer   " + testToken}, 200},
		{"cookie", "/api/v1/services", map[string]string{"Cookie": "mirage_token=" + testToken}, 200},
		{"cookie among others", "/api/v1/services", map[string]string{"Cookie": "a=1; mirage_token=" + testToken + "; b=2"}, 200},
		{"access_token query", "/api/v1/services?access_token=" + testToken, nil, 200},
		{"access_token for metrics", "/metrics?access_token=" + testToken, nil, 200},
		{"wrong bearer", "/api/v1/services", map[string]string{"Authorization": "Bearer nope"}, 401},
		{"wrong cookie", "/api/v1/services", map[string]string{"Cookie": "mirage_token=nope"}, 401},
		{"wrong query", "/api/v1/services?access_token=nope", nil, 401},
		{"empty query", "/api/v1/services?access_token=", nil, 401},
		{"empty bearer", "/api/v1/services", map[string]string{"Authorization": "Bearer "}, 401},
		{"basic scheme", "/api/v1/services", map[string]string{"Authorization": "Basic " + testToken}, 401},
		{"bare token without scheme", "/api/v1/services", map[string]string{"Authorization": testToken}, 401},
		{"token prefix", "/api/v1/services", map[string]string{"Authorization": "Bearer " + testToken[:len(testToken)-1]}, 401},
		{"token with suffix", "/api/v1/services", map[string]string{"Authorization": "Bearer " + testToken + "x"}, 401},
		{"different case", "/api/v1/services", map[string]string{"Authorization": "Bearer " + strings.ToUpper(testToken)}, 401},
		{"wrong cookie name", "/api/v1/services", map[string]string{"Cookie": "token=" + testToken}, 401},
		{"wrong query name", "/api/v1/services?token=" + testToken, nil, 401},
		{"wrong bearer but valid cookie", "/api/v1/services", map[string]string{"Authorization": "Bearer nope", "Cookie": "mirage_token=" + testToken}, 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := e.send(request{method: "GET", path: tc.path, header: tc.header, noAuth: true})
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d: %s", resp.StatusCode, tc.want, body)
			}
		})
	}
}

func TestAuthDisabledWithoutToken(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/api/v1/services", "/metrics", "/api/v1/info"} {
		if resp, body := e.do("GET", path, nil); resp.StatusCode != 200 {
			t.Errorf("GET %s = %d: %s", path, resp.StatusCode, body)
		}
	}
	var info struct {
		Auth bool `json:"auth"`
	}
	e.call("GET", "/api/v1/info", nil, http.StatusOK, &info)
	if info.Auth {
		t.Error("info.auth = true without a token")
	}
	// A bogus Authorization header is simply ignored.
	resp, _ := e.send(request{method: "GET", path: "/api/v1/services", header: map[string]string{"Authorization": "Bearer whatever"}})
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestInfoReportsAuth(t *testing.T) {
	e := authEnv(t)
	var info map[string]any
	e.call("GET", "/api/v1/info", nil, http.StatusOK, &info)
	if info["auth"] != true || info["version"] != "v0.0.0-test" || info["data_path"] != ":memory:" {
		t.Errorf("info = %v", info)
	}
	if _, ok := info["token"]; ok {
		t.Error("the token must never be echoed")
	}
	for _, k := range []string{"go_version", "started_at", "detectors"} {
		if _, ok := info[k]; !ok {
			t.Errorf("info lacks %q", k)
		}
	}
}

// A cookie is sent by the browser on cross-site requests too, so a request that
// only has a cookie must come from this site to change anything. Since the
// guard applies in every auth mode (see guard_test.go), a request with another
// origin is refused whichever credential it presents.
func TestAuthCookieRefusesCrossOriginWrites(t *testing.T) {
	e := authEnv(t)
	host := strings.TrimPrefix(e.ts.URL, "http://")
	sameHostOtherPort := "http://" + strings.Split(host, ":")[0] + ":1"
	cookie := "mirage_token=" + testToken
	tests := []struct {
		name         string
		method, path string
		header       map[string]string
		want         int
	}{
		{"POST from another origin", "POST", "/api/v1/services", map[string]string{"Cookie": cookie, "Origin": "https://evil.example"}, 403},
		{"DELETE from another origin", "DELETE", "/api/v1/services/svc_x", map[string]string{"Cookie": cookie, "Origin": "https://evil.example"}, 403},
		{"PUT from another origin", "PUT", "/api/v1/services/svc_x", map[string]string{"Cookie": cookie, "Origin": "https://evil.example"}, 403},
		{"PATCH from another origin", "PATCH", "/api/v1/recordings/rec_x", map[string]string{"Cookie": cookie, "Origin": "https://evil.example"}, 403},
		{"POST from another port of this host", "POST", "/api/v1/services", map[string]string{"Cookie": cookie, "Origin": sameHostOtherPort}, 403},
		{"malformed Origin", "POST", "/api/v1/services", map[string]string{"Cookie": cookie, "Origin": "://bad"}, 403},
		{"POST from this origin reaches the handler", "POST", "/api/v1/services", map[string]string{"Cookie": cookie, "Origin": "http://" + host}, 400},
		{"DELETE from this origin reaches the handler", "DELETE", "/api/v1/services/svc_x", map[string]string{"Cookie": cookie, "Origin": "http://" + host}, 404},
		{"POST without Origin (curl)", "POST", "/api/v1/services", map[string]string{"Cookie": cookie}, 400},
		{"GET from another origin is harmless", "GET", "/api/v1/services", map[string]string{"Cookie": cookie, "Origin": "https://evil.example"}, 200},
		{"bearer from another origin is refused too", "POST", "/api/v1/services", map[string]string{"Authorization": "Bearer " + testToken, "Origin": "https://evil.example"}, 403},
		{"query token from another origin is refused too", "POST", "/api/v1/services?access_token=" + testToken, map[string]string{"Origin": "https://evil.example"}, 403},
		{"bearer from this origin reaches the handler", "POST", "/api/v1/services", map[string]string{"Authorization": "Bearer " + testToken, "Origin": "http://" + host}, 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := e.send(request{method: tc.method, path: tc.path, header: tc.header, noAuth: true})
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d: %s", resp.StatusCode, tc.want, body)
			}
		})
	}
	if list, _ := e.st.ListServices(context.Background()); len(list) != 0 {
		t.Errorf("a refused request created %d services", len(list))
	}
}

func TestTokenMatches(t *testing.T) {
	s := New(Deps{Store: newEnv(t).st, AdminToken: "abc"})
	for token, want := range map[string]bool{"abc": true, "abd": false, "ab": false, "abcd": false, "": false, "ABC": false} {
		if got := s.tokenMatches(token); got != want {
			t.Errorf("tokenMatches(%q) = %v, want %v", token, got, want)
		}
	}
}

func TestAccessLogNeverContainsTheToken(t *testing.T) {
	var (
		mu  sync.Mutex
		buf bytes.Buffer
	)
	logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := newEnv(t, func(d *Deps) { d.AdminToken = testToken; d.Logger = logger })

	e.send(request{method: "GET", path: "/api/v1/services?access_token=" + testToken, noAuth: true})
	e.send(request{method: "GET", path: "/api/v1/services", header: map[string]string{"Authorization": "Bearer " + testToken}, noAuth: true})
	e.send(request{method: "GET", path: "/api/v1/services?access_token=wrong-guess", noAuth: true})

	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "path=/api/v1/services") || !strings.Contains(out, "status=200") || !strings.Contains(out, "status=401") {
		t.Fatalf("access log is missing entries:\n%s", out)
	}
	for _, secret := range []string{testToken, "wrong-guess", "access_token"} {
		if strings.Contains(out, secret) {
			t.Errorf("access log contains %q:\n%s", secret, out)
		}
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestNeedsAuth(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/v1/health":   false,
		"/api/v1/health/x": true,
		"/api/v1/info":     true,
		"/api":             true,
		"/api/":            true,
		"/metrics":         true,
		"/metrics/x":       false,
		"/":                false,
		"/assets/app.js":   false,
		"/apiary":          false,
	} {
		if got := needsAuth(path); got != want {
			t.Errorf("needsAuth(%q) = %v, want %v", path, got, want)
		}
	}
}
