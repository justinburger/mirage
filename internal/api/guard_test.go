package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The admin API with no token is reachable by whatever can talk to its port,
// and that includes web pages open in the administrator's browser. These
// tests pin the three browser-facing defences that apply in every auth mode:
// a cross-origin Origin is refused, a body must be declared as JSON, and (only
// without a token) the Host header must be one a DNS-rebinding page cannot use.

const serviceJSON = `{"name":"csrf-proxy","listen_addr":"127.0.0.1:0","upstream":"http://127.0.0.1:1","mode":"passthrough"}`

func serviceCount(t *testing.T, e *env) int {
	t.Helper()
	list, err := e.st.ListServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

// The attack of the review: a page on another origin sends a CORS "simple
// request" (text/plain, no preflight) to the loopback admin API.
func TestGuardRefusesCrossOriginSimpleRequest(t *testing.T) {
	e := newEnv(t) // no token: the default configuration
	resp, body := e.send(request{
		method: "POST", path: "/api/v1/services", body: serviceJSON,
		header: map[string]string{
			"Origin":         "http://evil.example",
			"Content-Type":   "text/plain;charset=UTF-8",
			"Sec-Fetch-Site": "cross-site",
		},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", resp.StatusCode, body)
	}
	if n := serviceCount(t, e); n != 0 {
		t.Fatalf("the refused request created %d services", n)
	}
	if e.svcs.applyCount() != 0 {
		t.Fatal("the refused request reached the service controller")
	}
}

func TestGuardOrigin(t *testing.T) {
	for _, withToken := range []bool{false, true} {
		name := "no token"
		if withToken {
			name = "token"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, func(d *Deps) {
				if withToken {
					d.AdminToken = testToken
				}
			})
			own := "http://" + strings.TrimPrefix(e.ts.URL, "http://")
			otherPort := "http://127.0.0.1:1"
			tests := []struct {
				name   string
				method string
				path   string
				body   any
				header map[string]string
				want   int
			}{
				{"POST from another origin", "POST", "/api/v1/services", serviceJSON, map[string]string{"Origin": "http://evil.example"}, 403},
				{"POST from another port of this host", "POST", "/api/v1/services", serviceJSON, map[string]string{"Origin": otherPort}, 403},
				{"POST from the null origin", "POST", "/api/v1/services", serviceJSON, map[string]string{"Origin": "null"}, 403},
				{"POST with a malformed Origin", "POST", "/api/v1/services", serviceJSON, map[string]string{"Origin": "://bad"}, 403},
				{"body-less POST from another origin", "POST", "/api/v1/services/svc_x/disable", nil, map[string]string{"Origin": "http://evil.example"}, 403},
				{"PUT from another origin", "PUT", "/api/v1/services/svc_x", serviceJSON, map[string]string{"Origin": "http://evil.example"}, 403},
				{"PATCH from another origin", "PATCH", "/api/v1/recordings/rec_x", `{"name":"x"}`, map[string]string{"Origin": "http://evil.example"}, 403},
				{"DELETE from another origin", "DELETE", "/api/v1/services/svc_x", nil, map[string]string{"Origin": "http://evil.example"}, 403},
				{"cross-site fetch metadata without Origin", "POST", "/api/v1/services/svc_x/disable", nil, map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
				{"POST from this origin reaches the handler", "POST", "/api/v1/services/svc_x/disable", nil, map[string]string{"Origin": own, "Sec-Fetch-Site": "same-origin"}, 404},
				{"POST without Origin (curl) reaches the handler", "POST", "/api/v1/services/svc_x/disable", nil, nil, 404},
				{"GET from another origin is harmless", "GET", "/api/v1/services", nil, map[string]string{"Origin": "http://evil.example"}, 200},
			}
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					resp, body := e.send(request{method: tc.method, path: tc.path, body: tc.body, header: tc.header})
					if resp.StatusCode != tc.want {
						t.Errorf("status = %d, want %d: %s", resp.StatusCode, tc.want, body)
					}
					if tc.want == http.StatusForbidden && !strings.Contains(string(body), "cross-origin") {
						t.Errorf("the refusal does not say why: %s", body)
					}
				})
			}
			if n := serviceCount(t, e); n != 0 {
				t.Errorf("refused requests created %d services", n)
			}
		})
	}
}

func TestGuardContentType(t *testing.T) {
	for _, withToken := range []bool{false, true} {
		name := "no token"
		if withToken {
			name = "token"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, func(d *Deps) {
				if withToken {
					d.AdminToken = testToken
				}
			})
			ct := func(v string) map[string]string { return map[string]string{"Content-Type": v} }
			tests := []struct {
				name   string
				method string
				path   string
				body   string
				header map[string]string
				want   int
			}{
				{"text/plain is refused", "POST", "/api/v1/services", serviceJSON, ct("text/plain;charset=UTF-8"), 415},
				{"form encoding is refused", "POST", "/api/v1/services", serviceJSON, ct("application/x-www-form-urlencoded"), 415},
				{"multipart is refused outside the import", "POST", "/api/v1/services", serviceJSON, ct("multipart/form-data; boundary=x"), 415},
				{"a missing Content-Type is refused", "POST", "/api/v1/services", serviceJSON, ct(""), 415},
				{"garbage Content-Type is refused", "POST", "/api/v1/services", serviceJSON, ct("json"), 415},
				{"PUT is checked too", "PUT", "/api/v1/services/svc_x", serviceJSON, ct("text/plain"), 415},
				{"PATCH is checked too", "PATCH", "/api/v1/recordings/rec_x", `{"name":"x"}`, ct("text/plain"), 415},
				{"application/json is accepted", "POST", "/api/v1/services", serviceJSON, ct("application/json"), 201},
				{"a charset parameter is accepted", "POST", "/api/v1/recordings", `{"name":"charset"}`, ct("application/json; charset=utf-8"), 201},
				{"the media type is case-insensitive", "POST", "/api/v1/recordings", `{"name":"case"}`, ct("Application/JSON"), 201},
				{"a +json type is accepted", "POST", "/api/v1/recordings", `{"name":"suffix"}`, ct("application/vnd.api+json"), 201},
				{"the HAR import takes raw JSON", "POST", "/api/v1/recordings/import", `{"log":{"version":"1.2","creator":{"name":"t","version":"1"},"entries":[]}}`, ct("application/har+json"), 201},
				{"the HAR import refuses text/plain", "POST", "/api/v1/recordings/import", `{"log":{}}`, ct("text/plain"), 415},
				{"a body-less POST needs no Content-Type", "POST", "/api/v1/services/svc_x/disable", "", ct(""), 404},
				{"a body-less POST ignores a stray Content-Type", "POST", "/api/v1/services/svc_x/disable", "", ct("text/plain"), 404},
				{"GET needs no Content-Type", "GET", "/api/v1/services", "", ct(""), 200},
			}
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					var body any
					if tc.body != "" {
						body = tc.body
					}
					resp, got := e.send(request{method: tc.method, path: tc.path, body: body, header: tc.header})
					if resp.StatusCode != tc.want {
						t.Errorf("status = %d, want %d: %s", resp.StatusCode, tc.want, got)
					}
					if tc.want == http.StatusUnsupportedMediaType {
						var eb errorBody
						if !strings.Contains(string(got), "application/json") {
							t.Errorf("the refusal does not name the type to send: %s", got)
						}
						_ = eb
					}
				})
			}
			// Only the one valid service was created; the refused bodies were not read.
			if n := serviceCount(t, e); n != 1 {
				t.Errorf("services = %d, want 1", n)
			}
		})
	}
}

// The HAR import also takes a multipart upload (the web UI's form).
func TestGuardAllowsMultipartForImport(t *testing.T) {
	e := newEnv(t)
	boundary := "xyz"
	body := "--" + boundary + "\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.har\"\r\nContent-Type: application/json\r\n\r\n" +
		`{"log":{"version":"1.2","creator":{"name":"t","version":"1"},"entries":[]}}` + "\r\n--" + boundary + "--\r\n"
	resp, got := e.send(request{method: "POST", path: "/api/v1/recordings/import", body: body,
		header: map[string]string{"Content-Type": "multipart/form-data; boundary=" + boundary}})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, got)
	}
}

// DNS rebinding: a page on evil.example.com whose name now resolves to the
// admin address reaches the API as "same origin", so Origin cannot catch it;
// the Host header still names the attacker's domain.
func TestGuardHostValidationWithoutToken(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.AllowedHosts = []string{"Mirage.Internal", "admin.example.com:8484"} })
	tests := []struct {
		host string
		want int
	}{
		{"", 200}, // the test server's own address (127.0.0.1:port)
		{"127.0.0.1:8484", 200},
		{"127.0.0.1", 200},
		{"localhost:8484", 200},
		{"LOCALHOST:8484", 200},
		{"localhost.:8484", 200},
		{"app.localhost:8484", 200},
		{"[::1]:8484", 200},
		{"192.168.1.20:8484", 200},
		{"10.0.0.5", 200},
		{"mirage.internal:8484", 200},
		{"mirage.internal", 200},
		{"admin.example.com:8484", 200},
		{"evil.example.com:8484", 403},
		{"evil.example.com", 403},
		{"localhost.evil.example.com:8484", 403},
		{"evil-localhost:8484", 403},
		{"admin.example.com:9999", 403}, // an entry with a port pins the port
		{"mirage.internal.evil.example:8484", 403},
		{"127.0.0.1.evil.example:8484", 403},
		{"2130706433:8484", 403}, // an integer is a name to resolvers, not an address
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			for _, path := range []string{"/api/v1/services", "/api/v1/recordings", "/metrics", "/"} {
				resp, body := e.send(request{method: "GET", path: path, host: tc.host})
				if resp.StatusCode != tc.want {
					t.Errorf("GET %s with Host %q = %d, want %d: %s", path, tc.host, resp.StatusCode, tc.want, body)
				}
			}
		})
	}

	// The refusal explains itself and leaks nothing.
	resp, body := e.send(request{method: "GET", path: "/api/v1/recordings", host: "evil.example.com:8484"})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "host") || strings.Contains(string(body), "[]") {
		t.Errorf("rebinding refusal = %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("security headers are missing on the refusal")
	}

	// The health check stays open for probes that use a service name.
	if resp, body := e.send(request{method: "GET", path: "/api/v1/health", host: "evil.example.com:8484"}); resp.StatusCode != 200 {
		t.Errorf("health with a foreign Host = %d: %s", resp.StatusCode, body)
	}
}

// A rebinding page can make "same-origin" writes too: Host and Origin both name
// its domain. The Host check stops those as well.
func TestGuardRebindingWritesAreRefused(t *testing.T) {
	e := newEnv(t)
	resp, body := e.send(request{
		method: "POST", path: "/api/v1/services", body: serviceJSON, host: "evil.example.com:8484",
		header: map[string]string{"Origin": "http://evil.example.com:8484"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if n := serviceCount(t, e); n != 0 {
		t.Fatalf("created %d services", n)
	}
}

// With a token, a rebinding page has nothing to present: the token is not a
// cookie of the attacker's domain. Names stay usable (reverse proxies, DNS
// aliases) without configuration.
func TestGuardHostIsNotCheckedWithToken(t *testing.T) {
	e := authEnv(t)
	resp, body := e.send(request{method: "GET", path: "/api/v1/services", host: "mirage.example.org:8484"})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	resp, _ = e.send(request{method: "GET", path: "/api/v1/services", host: "evil.example.com:8484", noAuth: true})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without the token: status = %d, want 401", resp.StatusCode)
	}
}

// Behind a reverse proxy the Origin names the public name while Host names the
// backend. Listing the public name accepts that Origin.
func TestGuardAllowedHostAcceptsItsOrigin(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.AllowedHosts = []string{"mirage.example.com"} })
	resp, body := e.send(request{
		method: "POST", path: "/api/v1/recordings", body: `{"name":"via proxy"}`,
		header: map[string]string{"Origin": "https://mirage.example.com"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("listed origin: status = %d: %s", resp.StatusCode, body)
	}
	resp, _ = e.send(request{
		method: "POST", path: "/api/v1/recordings", body: `{"name":"x"}`,
		header: map[string]string{"Origin": "https://evil.example"},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unlisted origin: status = %d", resp.StatusCode)
	}
}

func TestHostAllowed(t *testing.T) {
	s := New(Deps{Store: newEnv(t).st, AllowedHosts: []string{" Example.COM ", "", "[::1]:9000", "named.test:81"}})
	tests := map[string]bool{
		"":                       true,
		"localhost":              true,
		"localhost:1":            true,
		"x.localhost":            true,
		"127.0.0.1:80":           true,
		"[::1]":                  true,
		"[2001:db8::1]:8080":     true,
		"[fe80::1%25en0]:80":     true,  // an address with a zone
		"127.0.0.1/x":            false, // not a host[:port] at all
		"evil.example@127.0.0.1": false,
		"evil.example?:80":       false,
		"example.com":            true,
		"EXAMPLE.com:5":          true,
		"named.test:81":          true,
		"named.test:82":          false,
		"named.test":             false,
		"sub.example.com":        false,
		"evil.example":           false,
		"localhost.evil":         false,
		"[bad":                   false,
	}
	for host, want := range tests {
		if got := s.hostAllowed(host); got != want {
			t.Errorf("hostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// The guard sits behind authentication: an unauthenticated cross-origin write
// is still told to authenticate, and the refusal never touches the handler.
func TestGuardRunsAfterAuthentication(t *testing.T) {
	e := authEnv(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/services", strings.NewReader(serviceJSON))
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Content-Type", "text/plain")
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
