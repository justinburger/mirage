// Package api is Mirage's management API (JSON over HTTP, documented in
// docs/API.md), its live Server-Sent Events stream, the Prometheus endpoint
// and the static web UI.
//
// # Overview
//
// Server.Handler returns one http.Handler that serves, in this order of
// precedence: "/api/v1/..." (the JSON API), "/metrics" (Prometheus) and
// everything else from Deps.UI (the single-page web UI, with "index.html" as
// the fallback for unknown extension-less paths). Routing uses the standard
// library's method patterns ("GET /api/v1/services/{id}"); unknown API paths
// answer a JSON 404 and known paths with the wrong method a JSON 405 with an
// Allow header.
//
// The package depends on small interfaces instead of the proxy and replay
// packages: ServiceController and ReplayController. internal/app connects
// them.
//
// # Wire conventions
//
//   - Errors are {"error": "...", "problems": [...]}. A model.ErrValidation
//     (or *model.ValidationError) maps to 400 with the problems listed,
//     model.ErrNotFound to 404, model.ErrConflict to 409, a request body over
//     the limit to 413, anything unexpected to 500 with a generic message (the
//     detail is logged).
//   - An object that a request names (a recording_id, profile_id, service_id
//     or exchange_id in its body or query) and that does not exist is a 400
//     with a problem such as `recording_id "rec_x" does not exist`, whichever
//     endpoint it is. A 404 means the object addressed by the URL itself is
//     missing. A replay that points at a recording that was deleted after the
//     replay ran, a state conflict rather than a bad request, is a 409 when
//     started or cloned.
//   - Messages never carry the names of internal packages ("store:", "ops:"),
//     and an object named by several layers of an error is named once. That
//     includes the error of a service status (status.error, also in the live
//     stream), which the controller built from such errors.
//   - A request body must be declared as JSON (Content-Type application/json
//     or a +json type; the HAR import also takes multipart/form-data): any
//     other type, or none, is a 415. See Authentication for why.
//   - Request bodies are limited to 10 MiB (HAR import: 1 GiB, streamed).
//     Unknown JSON fields are rejected with 400, except the read-only echo
//     fields "status", "fired", "has_secret", "created_at" and "updated_at",
//     which are accepted and ignored on every endpoint so a fetched object can
//     be sent back. The id in the URL wins over an id in the body.
//   - List endpoints return JSON arrays, never null.
//
// # Services
//
// Creating or changing a service applies WithDefaults and Validate, checks
// that the recording and profile it references exist, persists it, then calls
// ServiceController.Apply. When Apply fails (a port in use, say) the service
// is still saved and the response is still 2xx (200, or 201 on create): it
// carries the status with running=false and the error in status.error, so a
// client must look at status after create, update, enable and mode, not at the
// HTTP status alone. Only a clash with another Mirage service (name or listen
// address) is a 409. status.mode is the mode of the running listener; when
// nothing is running it is the saved service's mode. Changes
// to one service are serialised, so the store and the running listener
// cannot disagree about which update came last. The Apply that follows a
// successful store write is detached from the request's cancellation, so a
// client that disconnects cannot leave the two out of step.
//
// # Referential integrity
//
// A recording cannot be deleted (409) while an enabled service in a mode that
// uses recordings points at it, or a replay job on it is pending or running.
// The check and the delete are atomic with respect to saving a service and
// starting a replay: a request that enables or re-points a service at a
// recording that a concurrent delete is removing waits for it and is then
// refused (400, the recording no longer exists), and a delete that arrives
// while a service is being saved on the recording is refused (409).
// A profile cannot be deleted (409) if it is "default" or any service or
// replay job references it. Deleting a service deletes the fault rules scoped
// to it (they could never fire again), and a fault rule cannot be saved for a
// service that does not exist.
//
// # Replays
//
// Creating a replay validates its config and checks that its recording and
// profile exist (400 otherwise). Starting one, directly or with "start": true,
// first checks that its recording still exists and is not empty, and answers
// 409 at once instead of "running" followed by a failure; with "start": true a
// job that cannot start is not left behind. The values of credential
// set_headers are masked ("***") in every response by a fixed rule
// (isSensitiveHeader: Authorization, Cookie, X-Api-Key and other well-known
// names, or any name containing auth, token, secret, password, credential,
// session, ... or ending in -key), independent of the anonymization profiles.
// A job's error text is shown without internal package prefixes.
//
// # Live stream
//
// GET /api/v1/stream is a Server-Sent Events stream fed by ONE broadcaster
// goroutine, started by Server.Run. It subscribes to the metrics registry
// once, marshals each event once and hands the finished bytes to a buffered
// channel per client. A client that cannot keep up is dropped (its stream is
// closed and the browser reconnects), never allowed to slow the others.
//
// # Authentication
//
// With Deps.AdminToken set, everything under /api/ except /api/v1/health, and
// /metrics, needs the token as "Authorization: Bearer <token>", the cookie
// "mirage_token" or the query parameter "access_token" (for EventSource and
// download links). Tokens are compared in constant time. The static UI is
// public.
//
// A browser on the administrator's machine can reach the admin address, so
// web pages are kept out in every auth mode (guardBrowsers):
//
//   - A state-changing request whose Origin header names another origin (or
//     whose Sec-Fetch-Site is cross-site) is refused with 403, whatever
//     credential it carries. CORS "simple requests" (text/plain, form
//     encodings, no preflight) are otherwise delivered to the handler and
//     only their response is hidden from the page.
//   - A state-changing request with a body must be JSON (415 otherwise); the
//     simple-request content types are exactly the ones a page can send
//     without a preflight.
//   - Without an admin token, the Host header must be an IP address,
//     localhost, or one of Deps.AllowedHosts, or the request is refused with
//     403: a page whose DNS name was rebound to the admin address is "same
//     origin" to the browser and could otherwise read every recording. With
//     a token, a rebinding page has no credential to present, so names are
//     unrestricted. The health check is exempt.
//
// Clients that are not browsers (curl, scripts, the CLI) send no Origin and
// notice only the Content-Type rule: use "Content-Type: application/json".
//
// # Static UI
//
// Every UI file is served with "Cache-Control: no-cache" and a strong ETag
// (content hash, computed once at start-up), and answers If-None-Match with
// 304. All responses carry X-Content-Type-Options, Referrer-Policy,
// X-Frame-Options and a Content-Security-Policy that allows only 'self'
// (plus 'unsafe-inline' for style attributes).
package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/justinburger/mirage/internal/fault"
	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/store"
)

// ServiceController controls running services. *proxy.Manager implements it.
type ServiceController interface {
	Apply(ctx context.Context, svc model.Service) error
	Remove(ctx context.Context, id string) error
	Reload(ctx context.Context, id string) error
	ProfileChanged(ctx context.Context, profileID string) error
	Status(id string) model.ServiceStatus
	Statuses() []model.ServiceStatus
}

// ReplayController runs replay jobs. *replay.Manager implements it.
type ReplayController interface {
	Create(ctx context.Context, name string, cfg model.ReplayConfig) (model.ReplayJob, error)
	Start(ctx context.Context, id string) error
	Stop(id string) error
	Get(ctx context.Context, id string) (model.ReplayJob, error)
	List(ctx context.Context) ([]model.ReplayJob, error)
	Delete(ctx context.Context, id string) error
	Series(ctx context.Context, id string, sinceMs int64) ([]model.SeriesPoint, error)
	Running() []model.ReplayJob
}

// Deps are the API server's dependencies.
type Deps struct {
	// Store is required.
	Store store.Store
	// Services and Replays may be nil; the endpoints that need them then
	// answer 503.
	Services ServiceController
	Replays  ReplayController
	// Metrics and Faults default to private, empty instances when nil.
	Metrics *metrics.Registry
	Faults  *fault.Injector
	// UI is the static web UI (web.FS()); nil disables it.
	UI         fs.FS
	AdminToken string
	// AllowedHosts are the DNS names (optionally with a port) the admin server
	// may be addressed by when it has no AdminToken, in addition to IP
	// addresses and localhost, which always work. A name listed here is also
	// accepted as the Origin of state-changing requests, for a reverse proxy
	// that presents a public name and rewrites Host.
	AllowedHosts []string
	Version      string
	DataPath     string
	Logger       *slog.Logger
}

// Server is the management API.
type Server struct {
	deps  Deps
	svcs  ServiceController
	rpls  ReplayController
	log   *slog.Logger
	start time.Time

	mux     *http.ServeMux
	handler http.Handler
	ui      *staticUI
	hub     *hub

	// token is the SHA-256 of Deps.AdminToken; compared in constant time.
	token    [sha256.Size]byte
	hasToken bool
	// allowedHosts is Deps.AllowedHosts, normalised.
	allowedHosts []string

	// svcLocks serialises store+controller changes per service; faultMu and
	// profileMu do the same for the fault rule set and anonymization profiles.
	svcLocks  keyedMutex
	faultMu   sync.Mutex
	profileMu sync.Mutex

	// recordingMu orders the deletion of a recording against everything that
	// can make a service (or a replay) start using one. deleteRecording holds
	// it exclusively across "is anything using it?" and the delete, and every
	// service save (saveService) and replay start holds it shared across its
	// own "does the recording exist?" check and the write that follows.
	// Without it an enable could pass its check just before the delete, and
	// the delete its check just before the enable's write, leaving an enabled
	// service on a recording that is gone. Shared holders do not exclude each
	// other (services are serialised by svcLocks, per service); the lock is
	// always taken after the service lock, and nothing is acquired under it
	// except the store and the controller, so it cannot deadlock.
	recordingMu sync.RWMutex

	// importLimit caps the size of a HAR upload (a field so tests can lower it).
	importLimit int64

	// previewSecret keys anonymization previews of profiles without a secret,
	// so repeated previews of an unsaved profile produce stable tokens.
	previewSecret string

	runOnce sync.Once
	running atomic.Bool   // Run was called
	done    chan struct{} // closed once the broadcaster has stopped
}

// errUnavailable is returned by the placeholder controllers installed when
// Deps leaves Services or Replays nil.
var errUnavailable = errors.New("this feature is not available in this server")

// New builds the API server. It starts no goroutines; call Run for the live
// stream. It panics if deps.Store is nil.
func New(deps Deps) *Server {
	if deps.Store == nil {
		panic("api: Deps.Store is required")
	}
	s := &Server{
		deps:          deps,
		svcs:          deps.Services,
		rpls:          deps.Replays,
		log:           deps.Logger,
		start:         time.Now(),
		mux:           http.NewServeMux(),
		previewSecret: model.NewSecret(),
		importLimit:   maxImportBody,
		done:          make(chan struct{}),
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.svcs == nil {
		s.svcs = unavailableServices{}
	}
	if s.rpls == nil {
		s.rpls = unavailableReplays{}
	}
	if s.deps.Metrics == nil {
		s.deps.Metrics = metrics.NewRegistry()
	}
	if s.deps.Faults == nil {
		s.deps.Faults = fault.NewInjector()
	}
	if deps.AdminToken != "" {
		s.token, s.hasToken = sha256.Sum256([]byte(deps.AdminToken)), true
	}
	s.allowedHosts = normalizeHosts(deps.AllowedHosts)
	s.ui = newStaticUI(deps.UI, s.log)
	s.hub = newHub(s)
	s.routes()
	s.handler = s.securityHeaders(s.accessLog(s.recoverPanics(s.authenticate(s.guardBrowsers(s.mux)))))
	return s
}

// Handler returns the root handler (API, /metrics and UI).
func (s *Server) Handler() http.Handler { return s.handler }

// Run starts the background work, the single SSE broadcaster, and returns at
// once; the broadcaster stops when ctx is done, closing every open stream. Call
// it once before serving (further calls do nothing). The broadcaster is already
// subscribed to the metrics registry when Run returns, so no tick that follows
// is missed. Until Run is called the stream endpoint still works but carries no
// periodic events.
func (s *Server) Run(ctx context.Context) {
	s.runOnce.Do(func() {
		s.running.Store(true)
		updates, unsubscribe := s.deps.Metrics.Subscribe(2)
		go func() {
			defer close(s.done)
			defer unsubscribe()
			s.hub.run(ctx, updates)
		}()
	})
}

// Wait blocks until the broadcaster started by Run has stopped, which happens
// after the context given to Run is done. It returns at once if Run was never
// called.
func (s *Server) Wait() {
	if s.running.Load() {
		<-s.done
	}
}

// WaitContext is Wait bounded by ctx; it reports whether the broadcaster is
// stopped (or was never started).
func (s *Server) WaitContext(ctx context.Context) bool {
	if !s.running.Load() {
		return true
	}
	select {
	case <-s.done:
		return true
	case <-ctx.Done():
		return false
	}
}

// unavailableServices is the ServiceController used when none is configured.
type unavailableServices struct{}

func (unavailableServices) Apply(context.Context, model.Service) error { return errUnavailable }
func (unavailableServices) Remove(context.Context, string) error       { return nil }
func (unavailableServices) Reload(context.Context, string) error       { return errUnavailable }
func (unavailableServices) ProfileChanged(context.Context, string) error {
	return nil
}
func (unavailableServices) Status(id string) model.ServiceStatus {
	return model.ServiceStatus{ServiceID: id}
}
func (unavailableServices) Statuses() []model.ServiceStatus { return nil }

// unavailableReplays is the ReplayController used when none is configured.
type unavailableReplays struct{}

func (unavailableReplays) Create(context.Context, string, model.ReplayConfig) (model.ReplayJob, error) {
	return model.ReplayJob{}, errUnavailable
}
func (unavailableReplays) Start(context.Context, string) error { return errUnavailable }
func (unavailableReplays) Stop(string) error                   { return errUnavailable }
func (unavailableReplays) Get(context.Context, string) (model.ReplayJob, error) {
	return model.ReplayJob{}, errUnavailable
}
func (unavailableReplays) List(context.Context) ([]model.ReplayJob, error) { return nil, nil }
func (unavailableReplays) Delete(context.Context, string) error            { return errUnavailable }
func (unavailableReplays) Series(context.Context, string, int64) ([]model.SeriesPoint, error) {
	return nil, errUnavailable
}
func (unavailableReplays) Running() []model.ReplayJob { return nil }
