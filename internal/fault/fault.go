// Package fault injects latency, errors, dropped connections, hangs and
// bandwidth limits into service traffic for chaos and resilience testing.
//
// An [Injector] holds a set of [model.FaultRule]s. For every request on a
// service listener the proxy asks it to [Injector.Decide], which returns a
// [Plan] (or nil when no fault fires). The proxy then calls [Plan.Apply] to
// perform the delay and any terminal fault, and [Plan.WrapWriter] to pace the
// response body:
//
//	plan := inj.Decide(serviceID, r)
//	if plan.Apply(w, r) {
//		return // an error was written, or the client went away
//	}
//	w = plan.WrapWriter(w) // plan may be nil
//
// # Rule matching
//
// A rule is in scope for a request when all of its conditions hold:
//
//   - ServiceID is empty or equal to the service's ID.
//   - Methods is empty or contains the request method (case-insensitive).
//   - PathPattern is empty, a glob, or a "re:" regular expression matched
//     against r.URL.Path. In a glob "*" matches exactly one non-empty path
//     segment, "**" matches zero or more segments and every other segment is a
//     case-sensitive literal ("/users/*/orders", "/api/**"). A single
//     trailing slash on the pattern or the path is ignored. A "re:" pattern
//     uses RE2 syntax and is unanchored, like regexp.MatchString.
//   - Every entry of Headers is present on the request with exactly that
//     value (header names are case-insensitive, values are compared exactly;
//     if a header is repeated, any of its values may match). The pseudo-header
//     "Host" is compared against r.Host, ignoring case.
//
// A rule that is in scope fires with its Probability: 0 never fires, 1
// always fires, values in between are sampled independently for each request
// with math/rand/v2. Disabled rules are kept (and returned by
// [Injector.Rules]) but never fire.
//
// # Combining faults
//
// Rules are evaluated in ascending (Order, Name) order. Every rule that fires
// contributes to the request's [Plan]:
//
//   - latency delays add up (each rule's Delay ± a uniform Jitter, never
//     negative);
//   - throttles take the lowest BytesPerSecond;
//   - the first terminal fault (error, abort or timeout) wins. Later terminal
//     rules are not evaluated, so they neither fire nor count for that
//     request. Latency and throttle rules ordered after the winning terminal
//     rule still contribute: a request that is aborted after 200 ms of added
//     latency is a legitimate fault scenario.
//
// # Concurrency and performance
//
// The compiled rule set lives behind an atomic pointer. [Injector.SetRules]
// compiles a new immutable set off to the side and swaps it in; [Injector.Decide]
// never takes a lock and allocates nothing when no rule fires. Fire counters
// are atomics shared between successive rule sets for rules whose ID is
// unchanged, so reconfiguring a running service does not lose counts.
//
// # Aborts and panics
//
// Abort and timeout faults end by panicking with http.ErrAbortHandler, the
// panic value net/http reserves for aborting a response. The server recovers
// it, closes the HTTP/1.1 connection (or resets the HTTP/2 stream) without
// writing a response, and does not log it. Any middleware around a handler
// that calls [Plan.Apply] (metrics, access logging, capture) and uses recover
// MUST re-panic http.ErrAbortHandler and let it propagate unchanged. Deferred
// cleanup is fine; swallowing the panic would turn an injected connection drop
// into an empty 200 response.
package fault

import (
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/justinburger/mirage/internal/model"
)

// Injector evaluates fault rules. The zero value is ready to use and has no
// rules. An Injector is safe for concurrent use.
type Injector struct {
	mu  sync.Mutex // serialises SetRules; never taken by Decide
	set atomic.Pointer[ruleSet]
}

// NewInjector returns an injector with no rules.
func NewInjector() *Injector { return &Injector{} }

// SetRules replaces the rule set. Each rule is defaulted (model.FaultRule.WithDefaults)
// and validated (model.FaultRule.Validate), plus a few checks the injector
// depends on: rule IDs must be unique, header and method names must be valid
// HTTP tokens, probabilities must not be NaN and error statuses must be 200
// or above. On error nothing changes: the previous rules stay active, and the
// returned error is a *model.ValidationError listing every problem.
//
// Fire counts are kept for rules whose ID also exists in the new set. The rules
// are copied, so the caller may reuse its slice.
func (i *Injector) SetRules(rules []model.FaultRule) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	next, err := compileRules(rules, i.set.Load())
	if err != nil {
		return err
	}
	i.set.Store(next)
	return nil
}

// Rules returns copies of the current rules, in evaluation order (ascending
// Order, then Name), including disabled rules. Defaults have been applied.
func (i *Injector) Rules() []model.FaultRule {
	if i == nil {
		return []model.FaultRule{}
	}
	set := i.set.Load()
	if set == nil {
		return []model.FaultRule{}
	}
	out := make([]model.FaultRule, len(set.all))
	for n, c := range set.all {
		out[n] = cloneRule(c.rule)
	}
	return out
}

// Counts returns how many times each rule has fired, keyed by rule ID. Every
// rule with a non-empty ID appears, with 0 if it has not fired. A count
// starts when SetRules first includes a rule's ID and survives later SetRules
// calls that still include it; a rule that is removed and added again starts
// from zero.
func (i *Injector) Counts() map[string]int64 {
	out := make(map[string]int64)
	if i == nil {
		return out
	}
	set := i.set.Load()
	if set == nil {
		return out
	}
	for id, c := range set.byID {
		out[id] = c.fired.Load()
	}
	return out
}

// planAlloc lets Decide allocate a Plan and room for a few fired rule names in
// one allocation.
type planAlloc struct {
	Plan
	names [4]string
}

// Decide evaluates the rules for one request on a service and returns the
// faults to apply, or nil when none fire. It takes no locks and performs no
// allocation when nothing fires.
//
// The returned Plan (including Plan.Terminal) is shared, immutable
// configuration and must not be modified.
func (i *Injector) Decide(serviceID string, r *http.Request) *Plan {
	if i == nil || r == nil {
		return nil
	}
	set := i.set.Load()
	if set == nil || len(set.active) == 0 {
		return nil
	}
	var pa *planAlloc
	for _, c := range set.active {
		if c.terminal && pa != nil && pa.Terminal != nil {
			continue // first terminal fault wins
		}
		if !c.matches(serviceID, r) || !c.roll() {
			continue
		}
		if pa == nil {
			pa = new(planAlloc)
			pa.Fired = pa.names[:0]
		}
		c.fired.Add(1)
		pa.Fired = append(pa.Fired, c.rule.Name)
		switch c.kind {
		case kindLatency:
			pa.Delay = addDelay(pa.Delay, c.sampleDelay())
		case kindThrottle:
			if pa.BytesPerSecond == 0 || c.bps < pa.BytesPerSecond {
				pa.BytesPerSecond = c.bps
			}
		default: // error, abort, timeout
			pa.Terminal = &c.rule
			pa.resp = c.resp
		}
	}
	if pa == nil {
		return nil
	}
	return &pa.Plan
}
