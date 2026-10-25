// Package api is Mirage's management API (JSON over HTTP, documented in
// docs/API.md), its live Server-Sent Events stream, the Prometheus endpoint
// and the static web UI.
//
// CONTRACT (implemented by the api agent):
//
//   - Routing with net/http.ServeMux method patterns ("GET
//     /api/v1/services/{id}"); no router dependency. Every route in
//     docs/API.md must exist with exactly the documented request and
//     response shapes; the web UI is written against that document.
//   - JSON responses use Content-Type application/json; errors are
//     {"error": "...", "problems": [...]} with status mapped from the error:
//     model.ErrValidation (and *model.ValidationError) → 400 with problems,
//     model.ErrNotFound → 404, model.ErrConflict → 409, malformed JSON → 400,
//     anything else → 500 (logged, message generic). Request bodies are
//     limited to 10 MiB (HAR import: 1 GiB, streamed), unknown JSON fields are
//     rejected with 400.
//   - Services: create/update applies WithDefaults then Validate, checks the
//     recording and profile exist, persists via the store, then calls
//     Services.Apply. If Apply fails (e.g. port in use) the service is still
//     saved and the response includes the status with its error. The mode
//     shortcut can create a new recording (new_recording_name).
//   - Profiles: the secret is never returned (Secret "" and HasSecret true);
//     PUT with an empty secret keeps the stored one; "regenerate_secret":
//     true in the body generates a new one. After a profile changes call
//     Services.ProfileChanged.
//   - Faults: after any change reload all rules from the store into
//     Deps.Faults.SetRules; list responses include each rule's fire count.
//   - Anonymize preview builds an engine from the inline profile (or the
//     stored one, keeping its secret when the inline profile has the same ID
//     and no secret) and runs ExchangeWithReport on a copy.
//   - Long operations (anonymized copy, HAR import) run synchronously for
//     simplicity but must stream (no whole-recording buffering) and respect
//     request cancellation.
//   - SSE (/api/v1/stream): subscribes to the metrics registry and sends
//     "stats" events each second, a "services" event with all statuses
//     whenever any status changes (checked each second) and a "replays"
//     event with id/status/summary of running jobs each second while any
//     run, plus a ":" comment heartbeat every 15s. Flush after every event.
//     Close cleanly when the client goes away.
//   - Auth: when AdminToken is set, /api/* (except /api/v1/health) and
//     /metrics require the token as "Authorization: Bearer <token>", a
//     "mirage_token" cookie, or an "access_token" query parameter (for
//     EventSource and downloads). Compare in constant time. Static UI files
//     are public (the UI asks for the token on 401).
//   - Static UI: Deps.UI served at "/", with "index.html" for unknown
//     non-API paths (SPA fallback). There is no build step and no hashed
//     file names, so every UI file is served with "Cache-Control: no-cache"
//     and a strong ETag (content hash computed once at start-up) and
//     answers If-None-Match with 304. Security headers on every response:
//     X-Content-Type-Options nosniff, Referrer-Policy no-referrer,
//     X-Frame-Options DENY, and a Content-Security-Policy allowing only
//     'self' (plus 'unsafe-inline' for style attributes).
//   - Access log via slog at debug level (method, path, status, duration).
package api

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"

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
}

// Deps are the API server's dependencies.
type Deps struct {
	Store    store.Store
	Services ServiceController
	Replays  ReplayController
	Metrics  *metrics.Registry
	Faults   *fault.Injector
	// UI is the static web UI (web.FS()); nil disables it.
	UI         fs.FS
	AdminToken string
	Version    string
	DataPath   string
	Logger     *slog.Logger
}

// Server is the management API.
type Server struct{}

// New builds the API server.
func New(deps Deps) *Server { panic("api: not implemented") }

// Handler returns the root handler (API, /metrics and UI).
func (s *Server) Handler() http.Handler { panic("api: not implemented") }
