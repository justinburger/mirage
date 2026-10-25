// Package fault injects latency, errors, dropped connections, hangs and
// bandwidth limits into service traffic for chaos and resilience testing.
//
// CONTRACT (implemented by the fault agent):
//
//   - Injector holds the compiled rule set behind an atomic pointer:
//     SetRules compiles (validating with model.FaultRule.Validate after
//     WithDefaults) and swaps atomically; Decide never takes a lock that
//     SetRules holds for long. Disabled rules are kept but never fire.
//   - Decide is the hot path: zero allocations when no rule matches. Rule
//     matching: ServiceID (empty = all), Methods (case-insensitive), path
//     glob ("*" = one segment, "**" = zero or more segments, matched against
//     r.URL.Path; compile globs to a segment matcher, not a regex), "re:"
//     regex, and exact header values. Probability uses a fast per-goroutine
//     safe RNG (math/rand/v2).
//   - Combination (see model.FaultRule): latency delays add (each rule's
//     Delay ± uniform Jitter, never negative), throttle takes the minimum
//     BytesPerSecond, the first terminal fault (error, abort, timeout) in
//     rule order wins.
//   - Plan.Apply: waits for the total delay (returning early, and reporting
//     handled, if the request context is cancelled), then performs the
//     terminal fault if any:
//     error   → writes ResponseHeader + Status + Body (Content-Type
//     application/json if Body looks like JSON, else text/plain) and
//     sets "X-Mirage-Fault: <rule name>";
//     abort   → panic(http.ErrAbortHandler) after the delay (net/http
//     resets the connection/stream and does not log it);
//     timeout → blocks for Delay or until the client goes away, then
//     panic(http.ErrAbortHandler).
//     Returns handled=true when the caller must not continue.
//   - Plan.WrapWriter returns a ResponseWriter that paces Write calls to
//     BytesPerSecond (token bucket, ~10 refills per second, flushing as it
//     goes) and still implements http.Flusher and Unwrap() so
//     http.ResponseController works. Returns w unchanged if no throttle.
//   - Counts reports how many times each rule fired (by rule ID) since
//     SetRules last included that rule; counts survive SetRules for rules
//     whose ID is unchanged.
//   - A nil *Plan is valid: Apply returns false immediately and WrapWriter
//     returns w.
package fault

import (
	"net/http"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// Injector evaluates fault rules.
type Injector struct{}

// NewInjector returns an injector with no rules.
func NewInjector() *Injector { panic("fault: not implemented") }

// SetRules replaces the rule set. On error the previous rules stay active.
func (i *Injector) SetRules(rules []model.FaultRule) error { panic("fault: not implemented") }

// Rules returns the current rules (copies), in evaluation order.
func (i *Injector) Rules() []model.FaultRule { panic("fault: not implemented") }

// Counts returns fire counts by rule ID.
func (i *Injector) Counts() map[string]int64 { panic("fault: not implemented") }

// Decide evaluates the rules for one request on a service and returns the
// faults to apply, or nil when none fire.
func (i *Injector) Decide(serviceID string, r *http.Request) *Plan {
	panic("fault: not implemented")
}

// Plan is the set of faults chosen for one request.
type Plan struct {
	Delay          time.Duration    // total added latency
	BytesPerSecond int64            // 0 = no throttle
	Terminal       *model.FaultRule // error, abort or timeout rule; nil if none
	Fired          []string         // names of the rules that fired, in order
}

// Apply performs the delay and any terminal fault. It returns true if the
// request has been fully handled (or aborted) and the caller must stop.
func (p *Plan) Apply(w http.ResponseWriter, r *http.Request) (handled bool) {
	panic("fault: not implemented")
}

// WrapWriter applies the throttle, if any, to w.
func (p *Plan) WrapWriter(w http.ResponseWriter) http.ResponseWriter {
	panic("fault: not implemented")
}
