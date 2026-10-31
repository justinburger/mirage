package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"
)

// copyBufSize is the size of the buffers ReverseProxy copies bodies with.
const copyBufSize = 32 << 10

var copyBufs = sync.Pool{New: func() any { return new([copyBufSize]byte) }}

// bufferPool implements httputil.BufferPool on top of copyBufs; the default
// allocates a fresh 32 KiB buffer for every response.
type bufferPool struct{}

func (bufferPool) Get() []byte { return copyBufs.Get().(*[copyBufSize]byte)[:] }

func (bufferPool) Put(b []byte) {
	if len(b) == copyBufSize {
		copyBufs.Put((*[copyBufSize]byte)(b))
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newTransport builds the upstream transport of a service: large idle pools,
// keep-alive, HTTP/2 over TLS, and DisableCompression so content encodings
// pass through untouched. Timeout bounds the wait for the response headers;
// bodies stream until EOF or client disconnect.
func newTransport(insecure bool, timeout time.Duration) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   1024,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: timeout,
		DisableCompression:    true,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecure, MinVersion: tls.VersionTLS12},
	}
}

// reqKey is the context key under which a request's *reqState travels from
// the handler to the ReverseProxy hooks.
type reqKey struct{}

func stateFrom(ctx context.Context) *reqState {
	rs, _ := ctx.Value(reqKey{}).(*reqState)
	return rs
}

// newProxy builds the service's reverse proxy. It is stateless: every hook
// finds the request's service state in the request context, so one proxy
// serves the service across live reconfigurations.
func (s *service) newProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite:        s.rewrite,
		Transport:      roundTripFunc(s.roundTrip),
		ModifyResponse: s.modifyResponse,
		ErrorHandler:   s.errorHandler,
		BufferPool:     bufferPool{},
		ErrorLog:       s.m.httpLog,
	}
}

// rewrite points the outbound request at the upstream (joining its base path),
// sets the Host header and the X-Forwarded-* headers, and narrows
// Accept-Encoding for requests that are captured.
func (s *service) rewrite(pr *httputil.ProxyRequest) {
	rs := stateFrom(pr.In.Context())
	if rs == nil {
		return
	}
	st := rs.st
	pr.SetURL(st.up)
	if st.svc.PreserveHost {
		pr.Out.Host = pr.In.Host
	}
	pr.SetXForwarded()
	if rs.capture {
		pr.Out.Header.Set("Accept-Encoding", narrowAcceptEncoding(pr.In.Header["Accept-Encoding"]))
	}
}

// roundTrip hands the request to the service's current transport and times it.
// The clock starts when the request is handed over, after any fault delay.
func (s *service) roundTrip(req *http.Request) (*http.Response, error) {
	rs := stateFrom(req.Context())
	if rs == nil {
		return nil, errors.New("proxy: request without state")
	}
	rs.t0 = time.Now()
	res, err := rs.st.tr.RoundTrip(req)
	if err == nil {
		rs.ttfb = time.Since(rs.t0)
	}
	return res, err
}

// modifyResponse hooks the upstream response: it notes upgrades (which pass
// through untouched, the body must stay the upgraded connection) and tees the
// body of captured responses.
func (s *service) modifyResponse(res *http.Response) error {
	rs := stateFrom(res.Request.Context())
	if rs == nil {
		return nil
	}
	rs.res = res
	if res.StatusCode == http.StatusSwitchingProtocols {
		rs.upgraded = true
		rs.rw.status, rs.rw.wrote = res.StatusCode, true
		return nil
	}
	suppressSniffing(rs.rw.Header(), res.Header)
	if rs.capture {
		rs.respTee = &respTee{src: res.Body, buf: newCapBuf(rs.st.bodyCap)}
		res.Body = rs.respTee
	}
	return nil
}

// suppressSniffing keeps net/http from inventing a Content-Type for a
// response the upstream sent none for. When the header map of the client's
// ResponseWriter has no Content-Type key, net/http sniffs the body and adds
// "text/plain; charset=utf-8" or similar, which clients then see although the
// upstream never sent it. A key with no values stops that and writes nothing.
//
// It must be set on the client's header map, not on upstream (res.Header):
// httputil.ReverseProxy copies upstream into dst value by value, so an empty
// entry there never reaches dst, and the captured response headers (a clone of
// res.Header) must stay what the upstream sent.
func suppressSniffing(dst, upstream http.Header) {
	if _, sent := upstream["Content-Type"]; sent {
		return
	}
	if _, set := dst["Content-Type"]; !set {
		dst["Content-Type"] = nil
	}
}

// errorHandler answers an upstream failure: 502, or 504 for a timeout, with
// a small JSON document and "X-Mirage-Error: upstream".
func (s *service) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	status, body := upstreamFailure(err)
	if rs := stateFrom(r.Context()); rs != nil {
		rs.upErr, rs.upStatus, rs.upBody, rs.upAt = err, status, body, time.Now()
	}
	writeUpstreamFailure(w, status, body)
}

// upstreamFailure maps a transport error to the status and JSON body Mirage
// answers with.
func upstreamFailure(err error) (status int, body []byte) {
	status = http.StatusBadGateway
	if isTimeout(err) {
		status = http.StatusGatewayTimeout
	}
	body, _ = json.Marshal(struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}{"upstream request failed", err.Error()})
	return status, body
}

// isTimeout reports whether err is a deadline or a network timeout (the
// response header timeout, a dial or TLS handshake timeout).
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

func writeUpstreamFailure(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Mirage-Error", "upstream")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// upstreamFailureHeader is the header stored with the synthetic response of a
// failed exchange (what the client saw).
func upstreamFailureHeader(n int) http.Header {
	return http.Header{
		"Content-Type":   {"application/json"},
		"X-Mirage-Error": {"upstream"},
		"Content-Length": {strconv.Itoa(n)},
	}
}

// writeJSONError answers with {"error": msg} and the given status.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	body, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{msg})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// narrowAcceptEncoding returns the Accept-Encoding to send upstream for a
// captured request: the codings the client accepts intersected with gzip and
// deflate, the only ones the capture pipeline can decode, or "identity" when
// nothing is left (including when the client sent no header). Quality values
// of zero exclude a coding; "*" admits both.
func narrowAcceptEncoding(values []string) string {
	var gzipQ, deflateQ, starQ = -1, -1, -1 // -1 = not mentioned, 0 = refused, 1 = accepted
	for _, v := range values {
		for part := range strings.SplitSeq(v, ",") {
			tok, params, _ := strings.Cut(part, ";")
			tok = strings.ToLower(strings.TrimSpace(tok))
			if tok == "" {
				continue
			}
			q := 1
			if params != "" && qIsZero(params) {
				q = 0
			}
			switch tok {
			case "gzip", "x-gzip":
				gzipQ = q
			case "deflate":
				deflateQ = q
			case "*":
				starQ = q
			}
		}
	}
	if gzipQ < 0 {
		gzipQ = starQ
	}
	if deflateQ < 0 {
		deflateQ = starQ
	}
	switch {
	case gzipQ > 0 && deflateQ > 0:
		return "gzip, deflate"
	case gzipQ > 0:
		return "gzip"
	case deflateQ > 0:
		return "deflate"
	}
	return "identity"
}

// qIsZero reports whether the parameter list holds q=0, q=0.0, q=0.000.
func qIsZero(params string) bool {
	for p := range strings.SplitSeq(params, ";") {
		k, v, ok := strings.Cut(p, "=")
		if !ok || strings.ToLower(strings.TrimSpace(k)) != "q" {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return err == nil && f <= 0
	}
	return false
}

// isUpgrade reports whether r asks for a protocol upgrade (WebSocket).
func isUpgrade(r *http.Request) bool {
	return r.Header.Get("Upgrade") != ""
}
