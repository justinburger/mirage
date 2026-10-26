package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// authCookie is the cookie that may carry the admin token.
const authCookie = "mirage_token"

// healthPath is the one API path that never needs the token.
const healthPath = "/api/v1/health"

// needsAuth reports whether the path belongs to the protected part of the
// server: the API (except the health check) and the metrics endpoint. The
// static UI is public; it asks for the token itself when the API answers 401.
func needsAuth(path string) bool {
	switch {
	case path == healthPath:
		return false
	case path == "/metrics", path == "/api", strings.HasPrefix(path, "/api/"):
		return true
	}
	return false
}

// authMethod says how a request presented the token.
type authMethod int

const (
	authNone authMethod = iota
	authBearer
	authCookieMethod
	authQuery
)

// importPath is the one endpoint that also takes a multipart upload.
const importPath = "/api/v1/recordings/import"

// authenticate enforces Deps.AdminToken. Without a token it is a no-op.
func (s *Server) authenticate(next http.Handler) http.Handler {
	if !s.hasToken {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !needsAuth(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// A cookie is attached to cross-site requests by the browser on its
		// own; guardBrowsers, which runs next, refuses the state changes that
		// another origin triggered, whatever credential they carry.
		if s.checkToken(r) == authNone {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mirage"`)
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: "unauthorized: send the admin token as a Bearer token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkToken returns how r proved knowledge of the token, or authNone. All
// presented tokens are compared, in constant time, as SHA-256 digests so that
// neither content nor length of the real token leaks through timing.
func (s *Server) checkToken(r *http.Request) authMethod {
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		if s.tokenMatches(strings.TrimSpace(h[7:])) {
			return authBearer
		}
	}
	if c, err := r.Cookie(authCookie); err == nil && s.tokenMatches(c.Value) {
		return authCookieMethod
	}
	if q := r.URL.Query().Get("access_token"); q != "" && s.tokenMatches(q) {
		return authQuery
	}
	return authNone
}

func (s *Server) tokenMatches(presented string) bool {
	sum := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(sum[:], s.token[:]) == 1
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// guardBrowsers protects the admin API from web pages, in every auth mode. A
// browser is a confused deputy here: it sits on the administrator's machine,
// can reach a loopback or LAN admin address, and runs whatever page it was
// sent to. Three checks, none of which a plain HTTP client (curl, scripts,
// the CLI) notices:
//
//   - Host. Without an admin token, whoever can reach the port is the
//     administrator, so a page on evil.example whose DNS name was rebound to
//     the admin address would be one too (and, being "same origin", could read
//     every recording). Such a page can only use a DNS name, so a request is
//     refused unless its Host is an IP address, localhost, or listed in
//     Deps.AllowedHosts. With a token a rebinding page has nothing to present
//     (the cookie belongs to the name the administrator used), so names stay
//     free there.
//   - Origin. State-changing requests that name another origin are refused,
//     whatever credential they carry. Browsers send Origin on every
//     cross-origin POST, including the CORS "simple requests" that need no
//     preflight and so cannot be stopped by the missing CORS headers.
//   - Content-Type. A request body must be declared as JSON (a multipart
//     upload is also accepted by the HAR import). text/plain, form encodings
//     and an absent type are the only types a page can send without a
//     preflight, and none of them is a legitimate API request.
//
// It runs behind authenticate, so unauthenticated callers are asked for
// credentials first. The health check is exempt.
func (s *Server) guardBrowsers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == healthPath {
			next.ServeHTTP(w, r)
			return
		}
		if !s.hasToken && !s.hostAllowed(r.Host) {
			writeJSON(w, http.StatusForbidden, errorBody{Error: fmt.Sprintf(
				"request refused: the Host %q is not allowed (use localhost or an IP address, set an admin token, or start the server with --allowed-host)", r.Host)})
			return
		}
		if !safeMethod(r.Method) {
			if !s.originAllowed(r) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				writeJSON(w, http.StatusForbidden, errorBody{Error: "cross-origin request refused"})
				return
			}
			if !contentTypeOK(r) {
				writeJSON(w, http.StatusUnsupportedMediaType, errorBody{
					Error: fmt.Sprintf("unsupported Content-Type %q: send the body as application/json", r.Header.Get("Content-Type"))})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// normalizeHosts lower-cases and trims the allowed host entries, dropping
// blanks. An entry is a name or address, with a port to pin one.
func normalizeHosts(in []string) []string {
	var out []string
	for _, h := range in {
		h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
		if h != "" {
			out = append(out, h)
		}
	}
	return out
}

// hostName returns the lower-case host of a "host[:port]" string without
// brackets, port or trailing dot. ok is false when it cannot be parsed.
func hostName(hostport string) (name string, ok bool) {
	u, err := url.Parse("//" + hostport)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), "."), true
}

// listedHost reports whether hostport matches an allowed-host entry: by name,
// or by name and port when the entry has a port.
func (s *Server) listedHost(hostport string) bool {
	name, ok := hostName(hostport)
	if !ok {
		return false
	}
	full := strings.TrimSuffix(strings.ToLower(hostport), ".")
	for _, entry := range s.allowedHosts {
		if entry == name || entry == full {
			return true
		}
	}
	return false
}

// hostAllowed reports whether a Host header names this server in a way a DNS
// rebinding page cannot: an IP address, localhost (and *.localhost, which
// browsers pin to the loopback interface) or an allowed host. An empty Host
// (HTTP/1.0) is not a browser.
func (s *Server) hostAllowed(host string) bool {
	if host == "" {
		return true
	}
	name, ok := hostName(host)
	if !ok {
		return false
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return true
	}
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return true
	}
	return s.listedHost(host)
}

// originAllowed reports whether the request's Origin header, if any, names the
// host it was sent to or an allowed host (a reverse proxy's public name).
// Requests without Origin (curl, same-origin GET) pass.
func (s *Server) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false // includes the opaque origin "null"
	}
	return strings.EqualFold(u.Host, r.Host) || s.listedHost(u.Host)
}

// contentTypeOK reports whether the body of a state-changing request, if it
// has one, is declared as JSON (or as a multipart upload for the HAR import).
func contentTypeOK(r *http.Request) bool {
	if r.ContentLength == 0 {
		return true
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	switch {
	case mt == "application/json", strings.HasSuffix(mt, "+json"):
		return true
	case mt == "multipart/form-data":
		return r.URL.Path == importPath
	}
	return false
}
