// Package proxy runs the service listeners: a high-performance reverse proxy
// per service that records, virtualizes and injects faults.
//
// CONTRACT (implemented by the proxy agent):
//
// Listeners. Each enabled service gets its own http.Server on ListenAddr
// (ReadHeaderTimeout 10s, IdleTimeout 120s, MaxHeaderBytes 1 MiB). Plaintext
// listeners also accept HTTP/2 cleartext (http.Server.Protocols with
// UnencryptedHTTP2); TLS listeners serve HTTP/1.1 and HTTP/2. TLS.SelfSigned
// generates an ECDSA P-256 certificate at start (SANs: localhost, 127.0.0.1,
// ::1 and the listen host). Bind errors are returned by Apply and kept in
// ServiceStatus.Error.
//
// Contexts. The ctx passed to Apply/Reload/ProfileChanged bounds only that
// call (start-up and virtualizer build); the API passes r.Context().
// Listeners, capture workers and request handling run on the Manager's own
// root context, cancelled by Shutdown.
//
// Live reconfiguration. The handler reads the service's current state
// through an atomic pointer, so Apply can change mode, recording, upstream,
// matching, latency or anonymization without dropping the listener or
// in-flight requests. Only a ListenAddr or TLS change restarts the listener
// (graceful: the old server drains with a 5s deadline). Disabled services
// are stopped but keep their status entry. A virtualizer is (re)built when
// the mode needs one and the recording or virtualization settings changed;
// the old one is closed after the swap.
//
// Request path, in order (steps 1 and 4 are one deferred function, so
// panics still finish the bookkeeping; never recover(): http.ErrAbortHandler
// from fault aborts and from httputil.ReverseProxy must propagate to
// net/http, and a panicking request is observed with Status 0, Error true):
//  1. metrics: InFlight(+1) on the service collector
//     (metrics.ServiceCollectorName), endpoint from a per-service
//     pathnorm.Normalizer built from EndpointPatterns, computed from the
//     escaped path (r.URL.EscapedPath()) before anything is anonymized.
//  2. faults: Deps.Faults.Decide(serviceID, r); plan.Apply; stop if
//     handled; wrap the writer with plan.WrapWriter.
//  3. by mode:
//     passthrough → reverse proxy.
//     record      → reverse proxy + capture.
//     virtualize  → read the request body (bounded by MaxBodyBytes for
//     matching; larger bodies match on the prefix), Virtualizer.Serve;
//     on a miss Virtualizer.WriteMiss.
//     hybrid      → Virtualizer.Serve; on a miss reverse proxy + capture.
//     Once stored, the exchange is added to the virtualizer only if it is
//     a GET, HEAD or OPTIONS with Error == "", no truncated body, a status
//     the URL alone determines (not 1xx, 206, 304, 408, 412, 416, 425, 429
//     or 5xx) and a request without Range, If-Range, If-None-Match,
//     If-Modified-Since, If-Match or If-Unmodified-Since (so writes always
//     reach the upstream and transient or conditional answers are not
//     learned). The decision is made on the service's state when the
//     exchange is stored, not on the state the request ran with.
//  4. metrics: Observe(endpoint, status, duration, bytes in/out, error =
//     model.IsErrorStatus), InFlight(-1).
//
// Reverse proxy: httputil.ReverseProxy with Rewrite (SetURL to the
// upstream, joining any upstream base path; SetXForwarded; Host = upstream
// host unless PreserveHost). One tuned http.Transport per service
// (MaxIdleConnsPerHost 1024, IdleConnTimeout 90s, ForceAttemptHTTP2,
// keep-alive dialer, InsecureSkipVerify per service, DisableCompression so
// encodings pass through untouched). Service.Timeout is the transport's
// ResponseHeaderTimeout (bodies stream until EOF or client disconnect).
// In record and hybrid modes the upstream request's Accept-Encoding is the
// client's intersected with {gzip, deflate} (identity if empty), so captured
// bodies are always decodable. Upstream failures answer 502 (504 on timeout) with a JSON body
// {"error":"upstream request failed","detail":"…"} and "X-Mirage-Error:
// upstream". Streaming responses (SSE, chunked) must stream, not buffer.
// Protocol upgrades (WebSocket) pass through and are not captured.
//
// Capture never blocks or slows the request path beyond copying bytes:
//   - Service.CaptureSample is decided first; unsampled requests are proxied
//     with no capture work (counted in CaptureSkipped). Once the recording
//     reaches Service.MaxCaptureBytes, capture stops and ServiceStatus.Warning
//     says "capture limit reached" (traffic keeps flowing, so it is not an
//     Error).
//   - Request and response bodies are teed into pooled buffers that start
//     small (4 KiB) and grow on demand up to MaxBodyBytes (-1 = headers
//     only) while they stream; buffers larger than 64 KiB are not returned
//     to the pool. BodySize counts the full size and BodyTruncated marks a
//     cut. The Exchange gets exact-size heap copies; pooled buffers are
//     released before enqueue, so nothing downstream aliases them.
//   - TTFB and Duration are measured from the hand-off to the upstream
//     transport (after fault delays), per model.Exchange.
//   - When the response body finishes (EOF or upstream error) an Exchange is
//     built (StartedAt, Duration, TTFB, Endpoint, headers, Scheme/Host/Proto,
//     Error for upstream failures) and enqueued on a queue bounded both by
//     count (Deps.CaptureQueueSize) and by queued body bytes
//     (Deps.CaptureQueueBytes). If either bound is hit the exchange is
//     dropped and ServiceStatus.CaptureDropped incremented. If the client
//     disconnects before the upstream response finished, nothing is captured
//     (CaptureSkipped).
//   - Capture workers (Deps.CaptureWorkers) decode gzip/deflate bodies that
//     are complete, stopping at MaxBodyBytes decoded bytes (then
//     BodyTruncated; protects against decompression bombs), moving the
//     encoding to ContentEncoding and dropping
//     Content-Encoding/Content-Length from the stored headers; other
//     encodings stay as captured; under AnonymizeCapture see "Anonymized
//     compressed bodies" below), apply the service's anonymizer when
//     AnonymizeCapture is on (profile ProfileID or model.DefaultProfileID,
//     loaded from the store with WithDefaults + anonymize.New and rebuilt on
//     ProfileChanged; a stored profile without a secret is an error), then hand off to
//     a writer that batches per recording (Deps.CaptureBatchSize or
//     Deps.CaptureFlushInterval, whichever first) into
//     Store.AppendExchanges. Stored exchanges in hybrid mode are then added
//     to the service's virtualizer (see hybrid above). ServiceStatus.Captured
//     counts stored exchanges. If anonymization fails the exchange is
//     dropped (never stored raw) and the error logged.
//   - Shutdown stops listeners gracefully, then drains the capture queue
//     and flushes pending batches before returning (bounded by ctx).
//
// Performance targets (benchmark with httptest upstreams): passthrough adds
// < 100µs median latency and sustains ≥ 20k req/s on a laptop; record mode
// within 20% of passthrough; virtualize ≥ 40k req/s for small responses.
// Use sync.Pool for buffers; avoid per-request allocations that scale with
// body size beyond the capped capture buffers.
//
// # IMPLEMENTATION NOTES
//
// Decisions made where the contract is silent or allows two readings. All of
// them are covered by tests.
//
//   - Request state. Everything a request needs (mode, normalizer, transport,
//     virtualizer, anonymizer, capture limits) lives in one immutable
//     serviceState behind an atomic pointer. Apply builds a new state and
//     swaps it; requests that already loaded the old one finish with it. A
//     swapped-out state is retired: its virtualizer is closed and its idle
//     upstream connections are dropped only once the last request that uses it
//     is done.
//   - Capture decisions. Sampling, the capture limit and the Accept-Encoding
//     narrowing apply to requests that are captured. An unsampled request in
//     record or hybrid mode is proxied exactly like passthrough, including the
//     client's own Accept-Encoding. Upgrade requests (WebSocket) are never
//     captured and never counted as skipped.
//   - Capture accounting. The capture limit compares MaxCaptureBytes with the
//     body bytes stored in the recording plus the body bytes still queued, so
//     a burst cannot overshoot by a whole queue. The request that crosses the
//     limit is still stored; the next ones are counted in CaptureSkipped.
//   - An exchange belongs to the service state its request was handled with
//     (recording, anonymizer, body cap). Apply and ProfileChanged therefore
//     affect captures of requests handled afterwards; captures already queued
//     keep the anonymizer they were started with and are never stored raw.
//   - Truncated encoded bodies. A gzip or deflate body whose captured bytes
//     were cut by MaxBodyBytes cannot be decoded and is stored as captured
//     (still encoded, BodyTruncated set). A body that decodes but exceeds
//     MaxBodyBytes is cut at MaxBodyBytes decoded bytes, BodyTruncated is set
//     and BodySize is the larger of the wire size and MaxBodyBytes+1. A
//     completely decoded body has BodySize equal to its decoded length.
//   - Anonymized compressed bodies. When AnonymizeCapture is on, a body that is
//     still under a content coding after the strict decode is never stored as
//     captured, whatever the profile's opaque_bodies says: the anonymizer
//     cannot read it, and it would keep raw values in a recording marked
//     anonymized. A gzip or deflate stream cut by MaxBodyBytes (or corrupt
//     after some output) is decoded as far as it goes: the decoded prefix, up
//     to MaxBodyBytes, is anonymized and stored with BodyTruncated set and
//     ContentEncoding recording the coding. A body under an unknown or
//     stacked coding, or one that yields no output, is dropped: Body is empty,
//     BodyTruncated is set and BodySize keeps the wire size.
//   - Anonymized endpoints. The stored Exchange.Endpoint of an anonymized
//     capture keeps the placeholders of the raw path's template ({id},
//     {uuid}, {hex}, {token}, {email}, {date}, or a configured
//     endpoint_patterns template) but takes its literal segments from the
//     anonymized path, so it carries no raw value the anonymizer removed from
//     the stored path. The live metrics label is still computed from the raw
//     path.
//   - Capture queue accounting. A job is charged its body bytes plus an
//     estimate of the memory around them (exchange structs, header maps) against
//     Deps.CaptureQueueBytes, and only the body bytes against the service's
//     MaxCaptureBytes. Bodies are charged at their captured (possibly
//     compressed) size when queued; once a worker has decoded and anonymized
//     them the charge follows the stored size, and a job that no longer fits
//     the byte bound is dropped and counted (a job alone in the queue is
//     always kept).
//   - A request the client abandoned before anything was written (for example
//     during simulated virtual latency) is observed as status 0, an error.
//   - Upstream failures before any response was read (502 or 504) store the
//     synthetic JSON answer as the response. A failure after the response
//     headers were sent stores the status and the partial body with Error set
//     to "upstream response interrupted: ..."; httputil.ReverseProxy aborts
//     the client connection.
//   - Hybrid mode reads at most MaxBodyBytes of the request body for matching
//     into a private buffer. On a miss the prefix is replayed in front of the
//     rest of the body, so the upstream always receives the whole body.
//   - Status counters (Captured, CaptureDropped, CaptureSkipped) count since
//     the service was last started from stopped; reconfiguring a running
//     service keeps them. CaptureDropped also counts exchanges lost to an
//     anonymization or store failure, which are logged and shown in
//     ServiceStatus.Error.
//   - ServiceStatus.Error keeps the last Apply failure or runtime capture error
//     until the next successful Apply. A failed Apply leaves a running service
//     on its previous configuration.
//   - Anonymization profile and virtualizer. A virtualize or hybrid service with
//     anonymize_capture on matches requests in their anonymized form, so its
//     virtualizer (built by Deps.Virtualizers from the service) embeds the
//     profile's anonymizer. Changing anonymize_capture, or the profile while it
//     is on, therefore rebuilds the virtualizer, and ProfileChanged rebuilds
//     the virtualizer of every running virtualize and hybrid service that uses
//     the profile with anonymize_capture on, together with the capture
//     anonymizer of record and hybrid services. What cannot be rebuilt keeps
//     its previous version and the error is returned.
//   - Request body and response overlap. A proxied request with a body puts the
//     client's HTTP/1 response writer in full duplex mode
//     (http.ResponseController.EnableFullDuplex). The upstream transport reads
//     the client's body on its own goroutine and the upstream may answer before
//     it has taken all of it; without full duplex net/http closes the rest of
//     the body as soon as the response starts, the transport's next read fails,
//     it drops the upstream connection and the response the client is reading
//     ends in an unexpected EOF. This was the cause of the CI-only failures of
//     TestLargeBodiesAreForwardedIntactAndCaptureTruncated. After the handler
//     returns, net/http still disposes of any body the transport left unread.
//   - Content-Type. A response the upstream sent without a Content-Type reaches
//     the client without one in every proxied mode: the client's header map
//     gets an empty Content-Type entry, which keeps net/http from sniffing one.
//     It is set on the client's writer, not on the upstream response, because
//     httputil.ReverseProxy copies header values one by one (an empty entry
//     never arrives) and the captured headers must stay what the upstream sent.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"github.com/justinburger/mirage/internal/fault"
	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
	"github.com/justinburger/mirage/internal/store"
)

// Virtualizer serves recorded responses. *virtual.Engine implements it.
type Virtualizer interface {
	// Serve writes the recorded response and returns true, or returns false
	// without writing on a miss. body holds at most Service.MaxBodyBytes of the
	// request body (nil when the request has none or bodies are not captured)
	// and is only valid during the call: implementations must not retain it.
	Serve(w http.ResponseWriter, r *http.Request, body []byte) bool
	// WriteMiss writes the configured miss response.
	WriteMiss(w http.ResponseWriter, r *http.Request)
	// Add indexes a newly captured exchange (hybrid mode).
	Add(ex *model.Exchange)
	// Len is the number of indexed exchanges.
	Len() int
	// Stats feeds ServiceStatus.VirtualHits/VirtualMisses/VirtualInjected/
	// IndexedExchanges.
	Stats() model.VirtualStats
	Close() error
}

// VirtualizerFactory builds a virtualizer for a service. On error it must
// return a nil interface (not a typed nil pointer).
type VirtualizerFactory func(ctx context.Context, svc model.Service, n *pathnorm.Normalizer) (Virtualizer, error)

// Deps are the Manager's dependencies.
type Deps struct {
	Store   store.Store
	Metrics *metrics.Registry
	// Faults may be nil (no fault injection).
	Faults *fault.Injector
	// Virtualizers is required for virtualize and hybrid modes.
	Virtualizers VirtualizerFactory
	Logger       *slog.Logger

	CaptureQueueSize     int           // default 65536 exchanges
	CaptureQueueBytes    int64         // default 256 MiB of queued body bytes
	CaptureBatchSize     int           // default 512
	CaptureFlushInterval time.Duration // default 250ms
	CaptureWorkers       int           // default runtime.GOMAXPROCS(0)
}

// ErrClosed is returned by Manager methods that start or change services after
// Shutdown.
var ErrClosed = errors.New("proxy: manager is shut down")

// Default capture pipeline sizes (see Deps).
const (
	defaultCaptureQueueSize     = 65536
	defaultCaptureQueueBytes    = 256 << 20
	defaultCaptureBatchSize     = 512
	defaultCaptureFlushInterval = 250 * time.Millisecond
)

// withDefaults returns d with zero-valued tunables replaced by their defaults.
func (d Deps) withDefaults() Deps {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.CaptureQueueSize <= 0 {
		d.CaptureQueueSize = defaultCaptureQueueSize
	}
	if d.CaptureQueueBytes <= 0 {
		d.CaptureQueueBytes = defaultCaptureQueueBytes
	}
	if d.CaptureBatchSize <= 0 {
		d.CaptureBatchSize = defaultCaptureBatchSize
	}
	if d.CaptureFlushInterval <= 0 {
		d.CaptureFlushInterval = defaultCaptureFlushInterval
	}
	if d.CaptureWorkers <= 0 {
		d.CaptureWorkers = runtime.GOMAXPROCS(0)
	}
	return d
}
