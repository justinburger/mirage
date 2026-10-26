package proxy

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
)

// reqState is the per-request bookkeeping of the handler. It is created on the
// handler goroutine and travels to the ReverseProxy hooks through the request
// context. Everything except the request body tee (which the transport reads
// on its own goroutine) is touched only by the handler goroutine.
type reqState struct {
	s        *service
	st       *serviceState
	r        *http.Request // the request as received
	rw       respWriter
	start    time.Time
	endpoint string

	// prefix holds request body bytes the handler consumed for matching
	// (hybrid mode); the upstream receives them first.
	prefix   []byte
	bodyRead int64 // bytes consumed for matching when the body is not proxied

	capture  bool // this request is captured
	reqTee   *reqTee
	respTee  *respTee
	res      *http.Response
	upgraded bool

	t0   time.Time     // request handed to the upstream transport
	ttfb time.Duration // t0 until the response headers arrived

	upErr    error // the transport failed
	upStatus int
	upBody   []byte
	upAt     time.Time

	completed bool // ServeHTTP returned normally (no panic)
}

// ServeHTTP implements http.Handler: it serves one request of the service
// according to the service's current state.
//
// Bookkeeping (metrics, capture hand-off, state release) runs in a deferred
// call and never recovers: when a fault aborts the connection, or ReverseProxy
// aborts after a failed copy, http.ErrAbortHandler keeps propagating to
// net/http, and the request is observed with status 0 as an error.
func (s *service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st := s.acquire()
	if st == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "service is not running")
		return
	}
	s.active.Add(1)
	s.collector.InFlight(1)
	rs := &reqState{s: s, st: st, r: r, start: time.Now()}
	defer rs.done()
	rs.rw.ResponseWriter = w
	rs.endpoint = st.norm.Endpoint(r.Method, r.URL.EscapedPath())

	rs.serve()
	rs.completed = true
}

// serve runs the fault injector and then the mode's handler.
func (rs *reqState) serve() {
	r := rs.r
	var w http.ResponseWriter = &rs.rw
	if inj := rs.s.m.deps.Faults; inj != nil {
		if plan := inj.Decide(rs.s.id, r); plan != nil {
			if plan.Apply(w, r) {
				return
			}
			w = plan.WrapWriterContext(r.Context(), w)
		}
	}
	switch rs.st.mode {
	case model.ModeVirtualize:
		rs.virtualize(w, r)
	case model.ModeHybrid:
		rs.hybrid(w, r)
	case model.ModeRecord:
		rs.proxy(w, r, true)
	default:
		rs.proxy(w, r, false)
	}
}

// virtualize answers from the recording; the upstream is never contacted.
func (rs *reqState) virtualize(w http.ResponseWriter, r *http.Request) {
	v := rs.st.virt
	if v == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "virtual service is not ready")
		return
	}
	var body []byte
	if rs.st.bodyCap > 0 && hasBody(r) {
		h, _ := readPooled(r.Body, rs.st.bodyCap)
		rs.bodyRead = int64(len(h.b))
		body = h.b
		defer putBuf(h)
	}
	if !v.Serve(w, r, body) {
		v.WriteMiss(w, r)
	}
}

// hybrid serves from the recording and proxies (and captures) misses.
func (rs *reqState) hybrid(w http.ResponseWriter, r *http.Request) {
	v := rs.st.virt
	if v == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "virtual service is not ready")
		return
	}
	var body []byte
	if rs.st.bodyCap > 0 && hasBody(r) {
		// A private copy: on a miss it is replayed to the upstream by a
		// transport goroutine that may outlive this call.
		body, _ = readPrefix(r.Body, rs.st.bodyCap, r.ContentLength)
		rs.prefix = body
		rs.bodyRead = int64(len(body))
	}
	if v.Serve(w, r, body) {
		return
	}
	rs.proxy(w, r, true)
}

// proxy forwards the request to the upstream, capturing it when record is set
// and the request is sampled in.
func (rs *reqState) proxy(w http.ResponseWriter, r *http.Request, record bool) {
	if rs.st.up == nil {
		writeJSONError(w, http.StatusBadGateway, "service has no upstream")
		return
	}
	rs.capture = record && rs.decideCapture(r)
	rs.s.rp.ServeHTTP(w, rs.prepare(r))
}

// decideCapture decides, before any buffering, whether r is captured.
// Upgrades are never captured; unsampled requests and requests past the
// capture limit are counted as skipped.
func (rs *reqState) decideCapture(r *http.Request) bool {
	if isUpgrade(r) {
		return false
	}
	st, s := rs.st, rs.s
	if st.sample < 1 && rand.Float64() >= st.sample {
		s.skipped.Add(1)
		return false
	}
	if lim := st.svc.MaxCaptureBytes; lim > 0 && s.used.Load()+s.pending.Load() >= lim {
		s.noteLimit()
		s.skipped.Add(1)
		return false
	}
	return true
}

// prepare returns the request to hand to ReverseProxy: it carries rs in its
// context and its body goes through the tee that counts (and captures) it.
func (rs *reqState) prepare(r *http.Request) *http.Request {
	out := r.WithContext(context.WithValue(r.Context(), reqKey{}, rs))
	if hasBody(r) {
		t := &reqTee{src: r.Body}
		if rs.capture {
			t.buf = newCapBuf(rs.st.bodyCap)
		}
		if len(rs.prefix) > 0 {
			t.seed(rs.prefix)
			out.Body = &prefixBody{pre: rs.prefix, rest: t}
		} else {
			out.Body = t
		}
		rs.reqTee = t
	}
	return out
}

func hasBody(r *http.Request) bool { return r.Body != nil && r.Body != http.NoBody }

// done finishes the request: hands the capture over, observes the metrics and
// releases the state. The deferred calls run even if an earlier step panics.
func (rs *reqState) done() {
	end := time.Now() // before the capture work below, which is not part of the request
	defer rs.s.active.Add(-1)
	defer rs.s.collector.InFlight(-1)
	defer rs.st.release()
	defer rs.observe(end)
	rs.finishCapture()
}

// observe records the request in the service's metrics collector.
func (rs *reqState) observe(end time.Time) {
	status, isErr := rs.rw.status, false
	switch {
	case !rs.completed:
		status, isErr = 0, true // a panic (abort) ended the request
	case !rs.rw.wrote:
		status = http.StatusOK
	}
	if !isErr {
		isErr = model.IsErrorStatus(status)
	}
	rs.s.collector.Observe(metrics.Observation{
		Endpoint: rs.endpoint,
		Status:   status,
		Duration: end.Sub(rs.start),
		BytesIn:  rs.bytesIn(),
		BytesOut: rs.rw.bytes,
		Error:    isErr,
	})
}

// bytesIn is the number of request body bytes received from the client.
func (rs *reqState) bytesIn() int64 {
	if rs.reqTee != nil {
		return rs.reqTee.n.Load()
	}
	return max(rs.bodyRead, rs.r.ContentLength)
}

// finishCapture turns a captured request into an exchange and queues it.
//
// The outcomes: a complete response, an upstream failure before the response
// (502/504), an upstream failure in the middle of the body (stored with
// Error), or a client that went away before the upstream response finished
// (nothing is stored, CaptureSkipped counts it).
func (rs *reqState) finishCapture() {
	if !rs.capture {
		return
	}
	s := rs.s
	var errMsg string
	switch {
	case rs.upgraded:
		rs.dropBuffers()
		return
	case rs.upErr != nil:
		// The transport failed. The client is gone only if it cancelled the
		// request (the error says so) or could not take the answer; a client
		// that hangs up after reading the 502 must not hide a real failure,
		// so the request context is not consulted here.
		if rs.rw.err != nil || errors.Is(rs.upErr, context.Canceled) {
			rs.skip()
			return
		}
		errMsg = "upstream request failed: " + rs.upErr.Error()
	case rs.respTee != nil:
		// Once the body was read to its end the exchange is complete, whatever
		// became of the client afterwards. Otherwise the body stopped early:
		// either the upstream failed or the client went away (a failed write
		// or a cancelled request context).
		clientGone := rs.rw.err != nil || rs.r.Context().Err() != nil
		switch {
		case rs.respTee.eof:
		case rs.respTee.err != nil && !clientGone:
			errMsg = "upstream response interrupted: " + rs.respTee.err.Error()
		default:
			rs.skip()
			return
		}
	default:
		// The request never reached the upstream (for example a panic
		// before proxying); there is nothing to store.
		rs.dropBuffers()
		return
	}
	ex := rs.buildExchange(errMsg)
	s.m.pipe.enqueue(&captureJob{
		ex: ex, svc: s, st: rs.st,
		queued: int64(len(ex.Request.Body) + len(ex.Response.Body)),
	})
}

func (rs *reqState) skip() {
	rs.dropBuffers()
	rs.s.skipped.Add(1)
}

// dropBuffers releases the capture buffers without building an exchange.
func (rs *reqState) dropBuffers() {
	if rs.reqTee != nil && rs.reqTee.buf != nil {
		rs.reqTee.buf.discard()
	}
	if rs.respTee != nil {
		rs.respTee.buf.discard()
	}
}

// buildExchange assembles the exchange from the request, the (possibly
// synthetic) response and the capture buffers, which it seals and releases.
func (rs *reqState) buildExchange(errMsg string) *model.Exchange {
	r := rs.r
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	ex := &model.Exchange{
		ServiceID: rs.s.id,
		StartedAt: rs.start,
		Endpoint:  rs.endpoint,
		Error:     errMsg,
		Request: model.Request{
			Method:   r.Method,
			Scheme:   scheme,
			Host:     r.Host,
			Path:     r.URL.EscapedPath(),
			RawQuery: r.URL.RawQuery,
			Proto:    r.Proto,
			Header:   r.Header.Clone(),
		},
	}
	if t := rs.reqTee; t != nil && t.buf != nil {
		ex.Request.Body, ex.Request.BodySize, ex.Request.BodyTruncated = t.buf.seal()
	}
	if rs.upErr != nil {
		ex.Duration = model.Duration(rs.upAt.Sub(rs.t0))
		ex.Response = model.Response{
			Status:   rs.upStatus,
			Proto:    "HTTP/1.1",
			Header:   upstreamFailureHeader(len(rs.upBody)),
			Body:     rs.upBody,
			BodySize: int64(len(rs.upBody)),
		}
		return ex
	}
	t := rs.respTee
	ex.TTFB = model.Duration(rs.ttfb)
	ex.Duration = model.Duration(t.endAt.Sub(rs.t0))
	ex.Response = model.Response{
		Status: rs.res.StatusCode,
		Proto:  rs.res.Proto,
		Header: rs.res.Header.Clone(),
	}
	ex.Response.Body, ex.Response.BodySize, ex.Response.BodyTruncated = t.buf.seal()
	return ex
}
