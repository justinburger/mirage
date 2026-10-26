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
// is still saved and the response carries the status with its error. Changes
// to one service are serialised, so the store and the running listener
// cannot disagree about which update came last. The Apply that follows a
// successful store write is detached from the request's cancellation, so a
// client that disconnects cannot leave the two out of step.
//
// # Referential integrity
//
// A recording cannot be deleted (409) while an enabled service in a mode that
// uses recordings points at it, or a replay job on it is pending or running.
// A profile cannot be deleted (409) if it is "default" or any service or
// replay job references it.
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
// public. A state-changing request authenticated only by the cookie is
// refused when its Origin header names another host (cross-site request
// forgery).
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
	Version    string
	DataPath   string
	Logger     *slog.Logger
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

	// svcLocks serialises store+controller changes per service; faultMu and
	// profileMu do the same for the fault rule set and anonymization profiles.
	svcLocks  keyedMutex
	faultMu   sync.Mutex
	profileMu sync.Mutex

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
	s.ui = newStaticUI(deps.UI, s.log)
	s.hub = newHub(s)
	s.routes()
	s.handler = s.securityHeaders(s.accessLog(s.recoverPanics(s.authenticate(s.mux))))
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
