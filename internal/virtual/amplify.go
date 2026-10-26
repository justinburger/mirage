package virtual

import (
	"math"
	"sync"
	"sync/atomic"
)

// syntheticBody is the body of the 503 served when error amplification asks
// for an error on an endpoint that never recorded one.
const syntheticBody = `{"error":"service unavailable (injected by mirage)"}`

// epool tracks one endpoint's recorded error rate and keeps its servable
// error and success exchanges, so error amplification costs no lock on the
// request path: counters are atomics and the pools are immutable snapshots.
type epool struct {
	total, errs atomic.Int64

	mu       sync.Mutex // guards ok and bad
	ok, bad  []*item
	snapshot atomic.Pointer[pools]
}

// pools is an immutable view of an epool's exchanges.
type pools struct{ ok, bad []*item }

// endpointPool returns the pool of the endpoint, creating it on first use.
func (e *Engine) endpointPool(endpoint string) *epool {
	e.epMu.Lock()
	ep := e.eps[endpoint]
	if ep == nil {
		ep = new(epool)
		e.eps[endpoint] = ep
	}
	e.epMu.Unlock()
	return ep
}

// record counts one exchange of the endpoint. it is nil for exchanges that
// are not servable (no upstream response). With publish set the new pool view
// becomes visible at once; Build publishes once at the end instead.
func (p *epool) record(it *item, isErr, publish bool) {
	p.mu.Lock()
	p.total.Add(1)
	if isErr {
		p.errs.Add(1)
	}
	if it != nil {
		// Appending is safe for readers of an earlier snapshot, which never
		// look beyond their own length.
		if isErr {
			p.bad = append(p.bad, it)
		} else {
			p.ok = append(p.ok, it)
		}
	}
	if publish {
		p.snapshot.Store(&pools{ok: p.ok, bad: p.bad})
	}
	p.mu.Unlock()
}

func (p *epool) publish() {
	p.mu.Lock()
	p.snapshot.Store(&pools{ok: p.ok, bad: p.bad})
	p.mu.Unlock()
}

// amplify applies error amplification to a fresh match: a success may become
// a recorded (or synthetic) error, an error may become a recorded success.
// The endpoint's error rate is e; the target is t = min(1, max(e×amp, floor)).
func (e *Engine) amplify(m *Match) {
	it := m.it
	if it == nil || it.ep == nil {
		return
	}
	total := it.ep.total.Load()
	if total <= 0 {
		return
	}
	rate := float64(it.ep.errs.Load()) / float64(total)
	target := math.Min(1, math.Max(rate*e.cfg.amp, e.cfg.floor))

	if !it.isErr {
		if target <= rate || rate >= 1 {
			return
		}
		if e.rng.float64() >= (target-rate)/(1-rate) {
			return
		}
		var bad []*item
		if ps := it.ep.snapshot.Load(); ps != nil {
			bad = ps.bad
		}
		if len(bad) == 0 {
			// Keep the matched exchange's latency for the synthetic error.
			m.Exchange, m.it = nil, nil
			m.Injected, m.Synthetic = true, true
			return
		}
		pick := bad[e.rng.intn(len(bad))]
		*m = Match{Exchange: pick.ex, Level: m.Level, Injected: true, it: pick, ttfb: pick.ttfb, dur: pick.dur}
		return
	}

	if target >= rate {
		return
	}
	if e.rng.float64() >= 1-target/rate {
		return
	}
	var ok []*item
	if ps := it.ep.snapshot.Load(); ps != nil {
		ok = ps.ok
	}
	if len(ok) == 0 {
		return
	}
	pick := ok[e.rng.intn(len(ok))]
	*m = Match{Exchange: pick.ex, Level: m.Level, Injected: true, it: pick, ttfb: pick.ttfb, dur: pick.dur}
}
