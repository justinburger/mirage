// Package replay is Mirage's load generator: it replays a recording against a
// target at the recorded pace, faster (Speed), multiplied (Amplify), with
// more error-inducing traffic (ErrorAmplification), or at a fixed or ramped
// rate, and measures the result.
//
// CONTRACT (implemented by the replay agent):
//
// Loading. Run streams the recording with request bodies only
// (IterOptions{RequestBodies: true}) into compact prepared items: method,
// path+query joined onto the target URL (target path as a prefix), headers
// minus hop-by-hop headers, Host and Content-Length, body, offset from the
// first exchange's StartedAt, endpoint, recorded status and IsError.
// Include/Exclude regexes match "METHOD /path?query". Headers: RemoveHeaders
// are deleted, SetHeaders set, Host = HostHeader or the target host. An empty
// recording (after filtering) fails the job with a clear error.
//
// Anonymization on the fly (ProfileID): build an anonymize.Engine from the
// stored profile; with VaryClones, copy c ≥ 1 uses engine.WithSalt(
// "clone-<c>"). Requests are cloned before anonymizing (prepared items are
// shared and immutable) and anonymized in the worker goroutines.
//
// Schedule (a lazy generator; never materialise unbounded schedules):
//   - timeline: item i is due at start + offset_i / Speed. With Loop, pass k
//     adds k × (span + mean gap) / Speed.
//   - rate: arrivals follow Rate or Stages (each stage ramps linearly from
//     the previous rate, starting from 0, to its Rate over its Duration;
//     after the last stage the job ends unless Duration is longer, in which
//     case the last rate holds). Items are taken in recorded order, cycling.
//   - max: Concurrency workers send back to back (closed model).
//   - Amplify N: every scheduled request is sent N times, spreading copies
//     evenly over the gap to the next scheduled request (rate mode: over
//     1/rate), never all at once.
//   - ErrorAmplification k: an item whose recording was an error is
//     scheduled floor(k) times plus once more with probability frac(k)
//     (so k < 1 thins them out); other items once. Applied before Amplify.
//   - Stop at Duration (if set), MaxRequests (if set), or the end of a
//     single pass, whichever comes first.
//   - Summary.Planned is the exact request count when the schedule is finite
//     (single pass, MaxRequests), otherwise an estimate for Duration-bounded
//     runs, or -1 if unknown.
//
// Dispatch. One scheduler goroutine and a fixed pool of Concurrency worker
// goroutines. The scheduler sleeps until the next due time (no sleeping
// for waits under ~1ms; dispatch everything already due in one go) and
// hands work to the pool; when every worker is busy it blocks, and the
// resulting delay is measured as lag. Latency is measured two ways: service
// time (send → body fully read) and corrected time (intended send → body
// fully read). MaxLag is the largest intended-vs-actual send delay.
//
// HTTP client: a dedicated http.Transport per job (MaxIdleConnsPerHost =
// Concurrency, MaxIdleConns = 2 × Concurrency, IdleConnTimeout 90s,
// ForceAttemptHTTP2, DisableCompression, keep-alive dialer,
// InsecureSkipVerify per config); requests carry a per-request Timeout
// context; response bodies are drained (io.Copy to io.Discard with a pooled
// buffer) and counted. Errors: timeouts (deadline exceeded / net timeout)
// counted in Timeouts and Failed; other transport errors in Failed.
// CompareStatus counts status mismatches against the recording.
//
// Metrics: every response is Observe'd on the job's collector (endpoint,
// status, service time, bytes) with InFlight tracking, so the live dashboard
// and SSE stream show the job like a service. The Summary is built from the
// run's own histograms and counters plus collector.Endpoints(); Series from
// collector.Series(start).
//
// Performance target: ≥ 30k req/s against a local httptest server with
// Concurrency 256 on a laptop, scheduler error < 2ms at 10k req/s.
// Benchmark it.
//
// Manager: jobs are persisted in the store. Create validates
// (WithDefaults + Validate + the recording exists) and stores a pending job.
// Start runs it in the background on the registry collector
// metrics.ReplayCollectorName(id) (reset at start); Get returns the live
// Summary while running (refreshed at least once per second); Stop cancels
// it (status stopped, partial summary kept). On finish the job (status,
// error, summary, series, timestamps) is saved. Only one run per job at a
// time; starting a finished job re-runs it (overwriting results). Recover
// marks jobs left running by a previous process as failed ("interrupted:
// mirage restarted"). OnChange is called after every status change.
package replay

import (
	"context"
	"log/slog"

	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/store"
)

// RunOptions configures Run.
type RunOptions struct {
	// Collector receives live observations. Required.
	Collector *metrics.Collector
	// TickCollector makes Run call Collector.Tick once per second itself
	// (standalone/CLI use). Leave false when a metrics.Registry ticks it.
	TickCollector bool
	// Progress, if set, is called about once per second with the live
	// summary.
	Progress func(model.ReplaySummary)
	Logger   *slog.Logger
}

// Run executes one replay synchronously and returns its summary and
// per-second series. Cancelling ctx stops it early; the partial summary is
// still returned together with ctx.Err().
func Run(ctx context.Context, st store.Store, cfg model.ReplayConfig, opts RunOptions) (model.ReplaySummary, []model.SeriesPoint, error) {
	panic("replay: not implemented")
}

// Deps are the Manager's dependencies.
type Deps struct {
	Store   store.Store
	Metrics *metrics.Registry
	Logger  *slog.Logger
	// OnChange is called after a job changes status (may be nil).
	OnChange func(model.ReplayJob)
}

// Manager runs replay jobs in the background.
type Manager struct{}

// NewManager returns a manager.
func NewManager(deps Deps) *Manager { panic("replay: not implemented") }

// Create validates and stores a pending job.
func (m *Manager) Create(ctx context.Context, name string, cfg model.ReplayConfig) (model.ReplayJob, error) {
	panic("replay: not implemented")
}

// Start runs a job in the background.
func (m *Manager) Start(ctx context.Context, id string) error { panic("replay: not implemented") }

// Stop cancels a running job.
func (m *Manager) Stop(id string) error { panic("replay: not implemented") }

// Get returns a job, with the live summary if running.
func (m *Manager) Get(ctx context.Context, id string) (model.ReplayJob, error) {
	panic("replay: not implemented")
}

// List returns all jobs (newest first, without series), with live summaries
// for running jobs.
func (m *Manager) List(ctx context.Context) ([]model.ReplayJob, error) {
	panic("replay: not implemented")
}

// Delete removes a job (stopping it first if running).
func (m *Manager) Delete(ctx context.Context, id string) error { panic("replay: not implemented") }

// Recover marks jobs interrupted by a restart as failed.
func (m *Manager) Recover(ctx context.Context) error { panic("replay: not implemented") }

// Shutdown stops running jobs and waits for them to save (bounded by ctx).
func (m *Manager) Shutdown(ctx context.Context) error { panic("replay: not implemented") }
