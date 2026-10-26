package proxy

import (
	"errors"
	"net/http"
)

// respWriter wraps the client's ResponseWriter to learn the status, the
// number of body bytes written and whether a write failed (the client went
// away). It implements Unwrap, so http.NewResponseController (used by
// httputil.ReverseProxy for flushing and for protocol upgrades) reaches the
// real writer.
type respWriter struct {
	http.ResponseWriter
	status int   // final status (informational 1xx codes other than 101 are ignored)
	bytes  int64 // body bytes written
	wrote  bool  // a final status was sent
	err    error // first write or flush error
}

// WriteHeader implements http.ResponseWriter.
func (w *respWriter) WriteHeader(code int) {
	if (code >= 200 || code == http.StatusSwitchingProtocols) && !w.wrote {
		w.wrote = true
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write implements http.ResponseWriter.
func (w *respWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	if err != nil && w.err == nil {
		w.err = err
	}
	return n, err
}

// Flush implements http.Flusher.
func (w *respWriter) Flush() { _ = w.FlushError() }

// FlushError flushes buffered data to the client.
func (w *respWriter) FlushError() error {
	if !w.wrote {
		w.wrote = true
		w.status = http.StatusOK
	}
	var err error
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	} else if err = http.NewResponseController(w.ResponseWriter).Flush(); errors.Is(err, http.ErrNotSupported) {
		err = nil
	}
	if err != nil && w.err == nil {
		w.err = err
	}
	return err
}

// Unwrap returns the wrapped writer for http.ResponseController.
func (w *respWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
