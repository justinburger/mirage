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
//
// # Implementation notes
//
// Histogram uses 1ns resolution below 256ns and 128 sub-buckets per power of
// two above, so every bucket is at most 1/128 as wide as its lower bound and a
// reported midpoint is within 0.4% of the true value, from 1µs (and below) to
// about 18 minutes. Quantile(0) and Quantile(1) return the exact minimum and
// maximum, and quantiles never leave [Min, Max]. Values beyond the range are
// counted in the last bucket.
//
// A Collector never swaps or allocates a window on the request path. Observe
// only increments cumulative atomics; Tick takes the difference between the
// cumulative state and its copy from the previous Tick. That makes the window
// accounting exact under any interleaving (an observation is in exactly one
// window) and keeps Observe to a handful of atomic operations. See
// Collector.Tick for the timestamp, gap-filling and skew rules.
//
// Per-endpoint stats live in a copy-on-write map behind an atomic pointer, so
// looking up an endpoint that already exists takes no lock and allocates
// nothing; new endpoints (at most MaxEndpoints per collector) take a mutex.
//
// # Corrected latency
//
// Replay jobs measure latency twice: Observation.Duration is service time and
// Observation.Corrected is the latency from the request's intended send time,
// which keeps a slow target from hiding its own queueing delay (coordinated
// omission). Corrected is optional, and 0 means not measured, so services never
// set it. The first observation with Corrected > 0 allocates a second
// cumulative histogram and per-window state (about 70 KiB) behind an atomic
// pointer; before that, and for every service collector, nothing is allocated
// and Observe pays one branch. Afterwards Observe adds one more lock-free
// histogram record and stays allocation-free. Tick cuts the corrected data into
// the same one-second windows as the rest (difference against the previous
// Tick, window extremes clamping the quantiles) and fills
// model.SeriesPoint.CorrectedP95 and CorrectedP99 for windows that have
// corrected observations; other points leave them 0 (omitted from JSON).
// Snapshot sets model.StatsSnapshot.CorrectedLatency, cumulative since Since,
// once something was measured and leaves it nil otherwise.
// Collector.CorrectedLatency returns a copy of the histogram (nil when not
// measured) for the replay summary, and WritePrometheus adds
// mirage_corrected_request_duration_seconds for collectors that measured it.
// Reset clears the corrected data along with the rest. There is no corrected
// per-endpoint or "recent" view.
//
// Deviations and additions to the contract, all deliberate:
//
//   - Statuses outside 100-599 (and 0) are counted under the "err" class, not
//     only Status == 0, so StatusClasses always sums to Requests.
//   - Snapshot.StatusClasses always carries all six keys, zero or not, and
//     Snapshot.Endpoints is limited to the SnapshotEndpoints busiest;
//     Collector.Endpoints returns every endpoint.
//   - Tick does not swap a window struct; it diffs against the previous Tick
//     (see Collector.Tick for stamps, jitter and gap filling). Window
//     percentiles are bucket midpoints within 0.4%; window Max, and the Min
//     and Max of Snapshot.Recent, are exact.
//   - Collector.Reset leaves the in-flight gauge alone (it tracks live
//     requests whose completion will decrement it).
//   - WritePrometheus adds per-endpoint families next to the ones in the
//     contract, and mirage_corrected_request_duration_seconds for replay
//     collectors that measured corrected latency; see WritePrometheus.
//   - Observation.Corrected, SeriesPoint.CorrectedP95/P99,
//     StatsSnapshot.CorrectedLatency and Collector.CorrectedLatency carry
//     coordinated-omission-corrected latency for replay jobs; see "Corrected
//     latency" above.
package metrics

const (
	// SeriesCapacity is how many one-second points each collector retains
	// (one hour).
	SeriesCapacity = 3600
	// MaxEndpoints caps distinct endpoints per collector. Observations for
	// further endpoints are aggregated under OtherEndpoint, which is reported
	// in addition to the MaxEndpoints named ones.
	MaxEndpoints = 256
	// OtherEndpoint collects observations beyond MaxEndpoints.
	OtherEndpoint = "(other)"
)

// ServiceCollectorName and ReplayCollectorName build collector names.
func ServiceCollectorName(serviceID string) string { return "service:" + serviceID }
func ReplayCollectorName(jobID string) string      { return "replay:" + jobID }
