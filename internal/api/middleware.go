package api

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// contentSecurityPolicy allows only same-origin resources. 'unsafe-inline' is
// there for style attributes set by the UI; scripts get no such exception.
const contentSecurityPolicy = "default-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// securityHeaders sets the response headers every reply carries, errors and
// downloads included.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		if needsAuth(r.URL.Path) || r.URL.Path == healthPath {
			// API and metrics responses are live data; the UI sets its own policy.
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// accessLog logs every request at debug level. Only the path is logged, never
// the query string, which may carry the access_token.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.log.Enabled(r.Context(), slog.LevelDebug) {
			next.ServeHTTP(w, r)
			return
		}
		rec := &recorder{ResponseWriter: w}
		start := time.Now()
		defer func() {
			s.log.Debug("api: request", "method", r.Method, "path", r.URL.Path,
				"status", rec.statusCode(), "bytes", rec.bytes, "duration", time.Since(start))
		}()
		next.ServeHTTP(rec, r)
	})
}

// recoverPanics turns a panicking handler into a 500, keeping the process
// alive. http.ErrAbortHandler is the panic net/http reserves for aborting a
// response and passes through untouched.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec, ok := w.(*recorder)
		if !ok {
			rec = &recorder{ResponseWriter: w}
			w = rec
		}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler { //nolint:errorlint // net/http compares by identity
				panic(v)
			}
			s.log.Error("api: handler panic", "method", r.Method, "path", r.URL.Path,
				"panic", v, "stack", string(debug.Stack()))
			if rec.Started() {
				panic(http.ErrAbortHandler)
			}
			writeJSON(rec, http.StatusInternalServerError, errorBody{Error: "internal server error"})
		}()
		next.ServeHTTP(w, r)
	})
}

// recorder wraps a ResponseWriter to remember whether and how the response
// started. It passes Flush and the other optional interfaces through
// (http.ResponseController finds them by Unwrap).
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *recorder) WriteHeader(code int) {
	// 1xx informational responses (other than 101) are not the final status.
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		r.ResponseWriter.WriteHeader(code)
		return
	}
	if !r.wrote {
		r.wrote, r.status = true, code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Flush() {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Started reports whether the response header has been written.
func (r *recorder) Started() bool { return r.wrote }

func (r *recorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}
