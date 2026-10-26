package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
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
		method := s.checkToken(r)
		if method == authNone {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mirage"`)
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: "unauthorized: send the admin token as a Bearer token"})
			return
		}
		// A cookie is attached to cross-site requests by the browser on its
		// own; a bearer header or query token never is. For cookie
		// authentication, refuse state changes that another origin triggered.
		if method == authCookieMethod && !safeMethod(r.Method) && !sameOrigin(r) {
			writeJSON(w, http.StatusForbidden, errorBody{Error: "cross-origin request refused"})
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

// sameOrigin reports whether the request's Origin header, if any, names the
// host it was sent to. Requests without Origin (curl, same-origin GET) pass.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}
