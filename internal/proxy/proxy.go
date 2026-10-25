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
//     a GET, HEAD or OPTIONS with Error == "", status < 500 and no
//     truncated body (so writes always reach the upstream and transient
//     failures are not learned).
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
//     reaches Service.MaxCaptureBytes, capture stops and ServiceStatus.Error
//     says "capture limit reached" (traffic keeps flowing).
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
//     encodings stay as captured), apply the service's anonymizer when
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
package proxy

import (
	"context"
	"log/slog"
	"net/http"
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
	// without writing on a miss.
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

// Manager owns every service listener.
type Manager struct{}

// NewManager returns a manager with no services.
func NewManager(deps Deps) *Manager { panic("proxy: not implemented") }

// Apply starts, reconfigures or stops a service to match svc (defaults are
// applied with WithDefaults).
func (m *Manager) Apply(ctx context.Context, svc model.Service) error {
	panic("proxy: not implemented")
}

// Remove stops a service, forgets it and removes its metrics collector.
func (m *Manager) Remove(ctx context.Context, id string) error { panic("proxy: not implemented") }

// Reload rebuilds a service's virtualizer from its recording.
func (m *Manager) Reload(ctx context.Context, id string) error { panic("proxy: not implemented") }

// ProfileChanged reloads the anonymizer of every service using profileID.
func (m *Manager) ProfileChanged(ctx context.Context, profileID string) error {
	panic("proxy: not implemented")
}

// Status returns a service's runtime status (zero Running if unknown).
func (m *Manager) Status(id string) model.ServiceStatus { panic("proxy: not implemented") }

// Statuses returns every known service's status.
func (m *Manager) Statuses() []model.ServiceStatus { panic("proxy: not implemented") }

// Handler returns the request handler of a running service (tests, embedding).
func (m *Manager) Handler(id string) (http.Handler, bool) { panic("proxy: not implemented") }

// Shutdown stops every listener and flushes captures.
func (m *Manager) Shutdown(ctx context.Context) error { panic("proxy: not implemented") }
