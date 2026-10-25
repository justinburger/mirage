// Package metrics provides lock-free latency histograms, per-service and
// per-replay traffic collectors with one-second time series, a registry that
// ticks collectors and fans updates out to live subscribers (the web UI's
// SSE stream), and Prometheus text exposition.
//
// CONTRACT (implemented by the metrics agent):
//
//   - Hot paths (Histogram.Record, Collector.Observe, In/Out flight) must be
//     safe for concurrent use without a global mutex: atomics for counters
//     and histogram buckets, and at most a short, sharded or RWMutex-guarded
//     lookup for the per-endpoint map. Target: Observe ≤ ~150ns and zero
//     allocations for an already-seen endpoint (benchmark it).
//   - Histogram: log-linear buckets (HdrHistogram-style) covering 1µs to
//     at least 10 minutes with ≤ 1% relative error. Values outside the range
//     clamp to the edge buckets. Quantile/Summary are approximate within that
//     error; Min/Max/Mean are exact.
//   - Collector keeps cumulative totals, a cumulative histogram, the current
//     one-second window (counts + histogram) and a ring of the last
//     SeriesCapacity closed windows as model.SeriesPoint. Tick(now) closes
//     the current window into a SeriesPoint stamped with the window's start
//     second; windows with no traffic still produce a point (zeros) so charts
//     have no gaps. Snapshot's RPS is the last closed window's count; RPS10,
//     ErrorRate10 and Recent cover the last 10 closed windows.
//   - Endpoints are tracked per collector up to MaxEndpoints distinct names;
//     beyond that observations go to the endpoint "(other)".
//   - Status classes: "1xx".."5xx", and "err" for Status == 0.
//   - Registry.Run ticks every collector once per wall-clock second (aligned
//     to the second) and then publishes one Update to every subscriber.
//     Slow subscribers must never block the ticker: drop the update for that
//     subscriber if its channel is full.
//   - WritePrometheus emits text exposition format 0.0.4 with these
//     families (labels kind, id, and where relevant endpoint/class):
//     mirage_requests_total, mirage_errors_total,
//     mirage_request_duration_seconds (summary with quantiles 0.5, 0.9,
//     0.95, 0.99 plus _sum and _count), mirage_in_flight_requests,
//     mirage_bytes_in_total, mirage_bytes_out_total,
//     mirage_responses_total{class=...}. Escape label values correctly.
package metrics

import (
	"context"
	"io"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

const (
	// SeriesCapacity is how many one-second points each collector retains
	// (one hour).
	SeriesCapacity = 3600
	// MaxEndpoints caps distinct endpoints per collector.
	MaxEndpoints = 256
	// OtherEndpoint collects observations beyond MaxEndpoints.
	OtherEndpoint = "(other)"
)

// Histogram is a concurrent latency histogram.
type Histogram struct{}

// NewHistogram returns an empty histogram.
func NewHistogram() *Histogram { panic("metrics: not implemented") }

// Record adds one observation. Negative durations count as 0.
func (h *Histogram) Record(d time.Duration) { panic("metrics: not implemented") }

// Merge adds every observation in o to h.
func (h *Histogram) Merge(o *Histogram) { panic("metrics: not implemented") }

// Reset clears the histogram.
func (h *Histogram) Reset() { panic("metrics: not implemented") }

// Count returns the number of observations.
func (h *Histogram) Count() int64 { panic("metrics: not implemented") }

// Quantile returns the q-quantile (0 ≤ q ≤ 1); 0 when empty.
func (h *Histogram) Quantile(q float64) time.Duration { panic("metrics: not implemented") }

// Summary returns count, min, mean, p50, p90, p95, p99, p99.9 and max in ms.
func (h *Histogram) Summary() model.LatencySummary { panic("metrics: not implemented") }

// Observation is one completed request.
type Observation struct {
	Endpoint string        // e.g. "GET /users/{id}"
	Status   int           // 0 = no response (transport error)
	Duration time.Duration // service time
	BytesIn  int64         // request bytes
	BytesOut int64         // response bytes
	// Error marks the request as an error. Callers normally set it to
	// model.IsErrorStatus(Status).
	Error bool
}

// Collector aggregates traffic for one service or replay job.
type Collector struct{}

// NewCollector returns a standalone collector (the registry creates its own).
func NewCollector(name string, kind model.CollectorKind, id string) *Collector {
	panic("metrics: not implemented")
}

// Name returns the collector name ("service:<id>", "replay:<id>").
func (c *Collector) Name() string { panic("metrics: not implemented") }

// Observe records one completed request.
func (c *Collector) Observe(o Observation) { panic("metrics: not implemented") }

// InFlight adjusts the in-flight gauge by delta (+1 on start, -1 on finish).
func (c *Collector) InFlight(delta int64) { panic("metrics: not implemented") }

// Tick closes the current one-second window into a SeriesPoint. Normally
// called by Registry.Run; standalone users (e.g. the CLI replay) call it
// themselves once per second.
func (c *Collector) Tick(now time.Time) model.SeriesPoint { panic("metrics: not implemented") }

// Snapshot returns the collector's current state.
func (c *Collector) Snapshot() model.StatsSnapshot { panic("metrics: not implemented") }

// Series returns retained points with T >= sinceMs (Unix ms), oldest first.
func (c *Collector) Series(sinceMs int64) []model.SeriesPoint { panic("metrics: not implemented") }

// Latency returns a copy of the cumulative latency histogram.
func (c *Collector) Latency() *Histogram { panic("metrics: not implemented") }

// Endpoints returns cumulative per-endpoint summaries, most requests first.
func (c *Collector) Endpoints() []model.EndpointSummary { panic("metrics: not implemented") }

// Reset clears all counters, histograms and series and sets Since to now.
func (c *Collector) Reset() { panic("metrics: not implemented") }

// Update is published by the registry once per second.
type Update struct {
	Time time.Time `json:"time"`
	// Snapshots of every collector.
	Snapshots []model.StatsSnapshot `json:"snapshots"`
	// Points holds the SeriesPoint each collector closed on this tick, keyed
	// by collector name.
	Points map[string]model.SeriesPoint `json:"points"`
}

// Registry owns named collectors.
type Registry struct{}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { panic("metrics: not implemented") }

// Collector returns the named collector, creating it if needed.
func (r *Registry) Collector(name string, kind model.CollectorKind, id string) *Collector {
	panic("metrics: not implemented")
}

// Get returns the named collector if it exists.
func (r *Registry) Get(name string) (*Collector, bool) { panic("metrics: not implemented") }

// Remove deletes the named collector.
func (r *Registry) Remove(name string) { panic("metrics: not implemented") }

// List returns all collectors sorted by name.
func (r *Registry) List() []*Collector { panic("metrics: not implemented") }

// Run ticks all collectors once per second and publishes Updates until ctx
// is done.
func (r *Registry) Run(ctx context.Context) { panic("metrics: not implemented") }

// Subscribe returns a channel receiving every Update and a cancel func that
// unsubscribes and closes the channel. buffer is the channel capacity.
func (r *Registry) Subscribe(buffer int) (<-chan Update, func()) { panic("metrics: not implemented") }

// WritePrometheus writes every collector in Prometheus text format.
func (r *Registry) WritePrometheus(w io.Writer) error { panic("metrics: not implemented") }

// ServiceCollectorName and ReplayCollectorName build collector names.
func ServiceCollectorName(serviceID string) string { return "service:" + serviceID }
func ReplayCollectorName(jobID string) string      { return "replay:" + jobID }
