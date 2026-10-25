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
//     rejected with 400 EXCEPT read-only echo fields, which are accepted and
//     ignored so the UI can PUT back what it fetched: "status" (services),
//     "fired" (faults), "has_secret", "created_at", "updated_at" (all). The
//     id in the URL path wins over an id in the body.
//   - Handlers may pass r.Context() to the controllers; they never own
//     background work with it.
//   - Services: create/update applies WithDefaults then Validate, checks the
//     recording and profile exist, persists via the store, then calls
//     Services.Apply. If Apply fails (e.g. port in use) the service is still
//     saved and the response includes the status with its error. The mode
//     shortcut can create a new recording (new_recording_name).
//   - Profiles: every output goes through model.AnonymizationProfile.Redacted
//     (the secret is never returned); PUT with an empty secret keeps the
//     stored one; "regenerate_secret": true in the body generates a new one
//     (new captures then produce different tokens than older recordings).
//     After a profile changes call Services.ProfileChanged.
//   - Referential integrity: DELETE recording → 409 if any enabled service in
//     a mode that uses recordings has RecordingID == id, or a replay job on it
//     is pending or running. DELETE profile → 409 for "default" or when any
//     service ProfileID or replay config ProfileID references it. DELETE
//     service → Services.Remove (also drops its collector), then
//     store.DeleteService; recordings it wrote are kept.
//   - Stats and series: GET /services/{id}/stats and /series read
//     Metrics.Get(metrics.ServiceCollectorName(id)); 404 only when the service
//     does not exist (a service with no collector yet returns an empty
//     snapshot / empty list); series default since = now − 15 min. Replay
//     series come from Replays.Series. POST /replays/{id}/clone = Get +
//     Create with the stored config (and name "<name> (copy)" by default).
//   - Replay responses mask values of set_headers whose name is in the
//     default profile's redact list (Authorization, Cookie, X-Api-Key, …) as
//     "***"; clone copies the stored (unmasked) config server-side.
//   - profile_id absent means "default" for POST /anonymize/preview and
//     /recordings/{id}/anonymize, and "no anonymization" for export.har and
//     import.
//   - Faults: after any change reload all rules from the store into
//     Deps.Faults.SetRules; list responses include each rule's fire count.
//   - Anonymize preview builds an engine from the inline profile (or the
//     stored one, keeping its secret when the inline profile has the same ID
//     and no secret) and runs ExchangeWithReport on a copy.
//   - Long operations (anonymized copy, HAR import) run synchronously for
//     simplicity but must stream (no whole-recording buffering; multipart
//     uploads read with r.MultipartReader, never ParseMultipartForm) and
//     respect request cancellation.
//   - SSE (/api/v1/stream): ONE broadcaster goroutine subscribes to the
//     metrics registry once, marshals each event once to []byte, and fans it
//     out to per-client buffered channels (a slow client is dropped, never
//     blocking others). Events: "stats" every second; "services" every second
//     with all statuses; "replays" every second while Replays.Running() is
//     non-empty, plus once more with the final job (from Replays.Get) when a
//     job leaves the running set; a ": ping" comment every 15s. Flush after
//     every event. A new client gets "services" immediately. Close cleanly
//     when the client goes away; the broadcaster stops when the server's
//     context ends.
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
	Series(ctx context.Context, id string, sinceMs int64) ([]model.SeriesPoint, error)
	Running() []model.ReplayJob
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

// Run starts background work (the SSE broadcaster) until ctx is done. Call it
// once before serving.
func (s *Server) Run(ctx context.Context) { panic("api: not implemented") }
