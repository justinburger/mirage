// Package virtual serves recorded responses in place of a real upstream
// (service virtualization), with realistic latency and controllable error
// rates.
//
// CONTRACT (implemented by the virtual agent):
//
// Index. Build streams the recording (store.IterateExchanges with request
// and response bodies) into an in-memory index with three levels (see
// model.MatchConfig):
//
//	exact    method + path + query key + body key + selected header values
//	path     method + path
//	template method + endpoint template (only if Match.PathTemplates)
//
// Query key per Match.Query: exact → params sorted by name then value,
// minus IgnoreParams; ignore → empty; subset → empty at the exact level, and
// candidates found at the "path" level are then filtered to those whose
// recorded params (minus IgnoreParams) are all present with equal values in
// the request. Body key per Match.Body: ignore → empty; exact → hash of
// bytes; json → hash of canonical JSON (keys sorted, insignificant
// whitespace removed, numbers kept as literals) or of the bytes when the
// body is not JSON. Use a fast non-cryptographic hash (hash/maphash with a
// per-engine seed). Exchanges whose Error is set (no upstream response) are
// not served as normal matches but do count toward error rates.
//
// Lookup is the hot path (target ≥ 50k lookups/s per core, no allocation
// beyond key building): read-locked map lookups. Selection among candidates
// per Match.Selection: sequential keeps an atomic cursor per key and cycles;
// random picks uniformly; first always returns the first recorded.
//
// Error amplification. At Build, for each recorded endpoint compute the
// error rate e (model.Exchange.IsError) and keep pools of its error and
// success exchanges. Target rate t = min(1, max(e × ErrorAmplification,
// ErrorFloor)). When a matched response is a success and t > e, flip it to an
// error with probability (t − e) / (1 − e): serve a random recorded error
// exchange of the same endpoint, or if there is none a synthetic 503 with
// JSON body {"error":"service unavailable (injected by mirage)"}. When the
// match is an error and t < e, flip to a random recorded success with
// probability 1 − t/e (when a success exists). Mark the Match Injected.
//
// Writing a response (Respond / Serve):
//   - Latency per model.LatencyConfig: recorded → wait TTFB × Multiplier,
//     write and flush the headers, wait the rest of (Duration − TTFB) ×
//     Multiplier, then write the body; fixed → wait Fixed, then respond;
//     Jitter adds uniform ±Jitter to the total (never negative). Waits stop
//     early if the request context is cancelled (then return without writing).
//   - Copy recorded headers except hop-by-hop headers (Connection,
//     Keep-Alive, Proxy-*, Te, Trailer, Transfer-Encoding, Upgrade),
//     Content-Length (recomputed) and Date (set to now).
//   - If the recorded response had ContentEncoding "gzip" and the request
//     accepts gzip, serve it gzip-compressed with Content-Encoding: gzip,
//     compressing each exchange's body once and caching the result.
//   - Add "X-Mirage-Match: exact|path|template" and, for amplified errors,
//     "X-Mirage-Injected: error".
//   - HEAD requests get headers only.
//
// Misses: Serve returns false and writes nothing; WriteMiss writes
// Match.MissStatus with a JSON body explaining the miss
// ({"error":"no recorded response matches this request","method":…,
// "path":…,"recording_id":…}) and "X-Mirage-Match: miss".
//
// Memory. If the recording's captured bytes exceed MaxResidentBytes, keep
// only the index and request-side keys in memory and load response bodies on
// demand with store.GetExchange through an LRU cache bounded by
// MaxResidentBytes. Below the threshold everything stays resident.
//
// Hybrid mode: Add inserts a newly captured exchange into all levels and
// the endpoint pools while serving continues (write lock).
package virtual

import (
	"context"
	"net/http"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
	"github.com/justinburger/mirage/internal/store"
)

// DefaultMaxResidentBytes is the default MaxResidentBytes (512 MiB).
const DefaultMaxResidentBytes = 512 << 20

// Options configures an Engine.
type Options struct {
	RecordingID        string
	Match              model.MatchConfig
	Latency            model.LatencyConfig
	ErrorAmplification float64
	ErrorFloor         float64
	// Normalizer computes endpoint templates; nil = pathnorm.Default.
	Normalizer       *pathnorm.Normalizer
	MaxResidentBytes int64 // 0 = DefaultMaxResidentBytes
	// Seed makes random choices reproducible in tests (0 = random seed).
	Seed uint64
}

// OptionsFromService derives Options from a service (defaults applied).
func OptionsFromService(svc model.Service, n *pathnorm.Normalizer) Options {
	svc = svc.WithDefaults()
	return Options{
		RecordingID:        svc.RecordingID,
		Match:              svc.Match,
		Latency:            svc.Latency,
		ErrorAmplification: svc.ErrorAmplification,
		ErrorFloor:         svc.ErrorFloor,
		Normalizer:         n,
	}
}

// Match is the result of a lookup.
type Match struct {
	Exchange *model.Exchange
	Level    string // "exact", "path" or "template"
	Injected bool   // response swapped by error amplification
	// Synthetic is true when Injected and no recorded error existed, so a
	// synthetic 503 is served (Exchange is then nil).
	Synthetic bool
}

// Stats counts engine activity since Build.
type Stats struct {
	Indexed  int64            `json:"indexed"`
	Hits     int64            `json:"hits"`
	Misses   int64            `json:"misses"`
	Injected int64            `json:"injected"`
	ByLevel  map[string]int64 `json:"by_level"`
}

// Engine is a virtualized service backed by one recording.
type Engine struct{}

// Build loads the recording from the store.
func Build(ctx context.Context, st store.Store, opts Options) (*Engine, error) {
	panic("virtual: not implemented")
}

// NewEngine returns an empty engine (populate with Add). st may be nil when
// everything stays resident.
func NewEngine(st store.Store, opts Options) *Engine { panic("virtual: not implemented") }

// Add indexes one exchange (hybrid mode, tests).
func (e *Engine) Add(ex *model.Exchange) { panic("virtual: not implemented") }

// Lookup finds the response for a request. body is the full request body
// (already read by the caller). Returns nil on a miss.
func (e *Engine) Lookup(r *http.Request, body []byte) *Match { panic("virtual: not implemented") }

// Respond writes the matched response, simulating latency.
func (e *Engine) Respond(w http.ResponseWriter, r *http.Request, m *Match) {
	panic("virtual: not implemented")
}

// Serve is Lookup + Respond. It returns false, writing nothing, on a miss.
func (e *Engine) Serve(w http.ResponseWriter, r *http.Request, body []byte) bool {
	panic("virtual: not implemented")
}

// WriteMiss writes the miss response.
func (e *Engine) WriteMiss(w http.ResponseWriter, r *http.Request) {
	panic("virtual: not implemented")
}

// Len returns the number of indexed exchanges.
func (e *Engine) Len() int { panic("virtual: not implemented") }

// Stats returns activity counters.
func (e *Engine) Stats() Stats { panic("virtual: not implemented") }

// Close releases resources.
func (e *Engine) Close() error { panic("virtual: not implemented") }
