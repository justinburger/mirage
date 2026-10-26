package metrics

import (
	"math"
	"sync/atomic"
)

// correctedQuantiles are the percentiles of a series point's corrected latency
// (p95, p99), ascending.
var correctedQuantiles = [...]float64{0.95, 0.99}

// correctedState is the coordinated-omission-corrected latency of one
// collector: a second cumulative histogram plus what Tick needs to cut it into
// one-second windows the same way it cuts the main one. A collector allocates it
// on the first Observation with Corrected > 0 and never replaces it.
//
// The fields before the Tick-side block are written by Observe with atomics
// only; the Tick-side fields are guarded by Collector.mu.
type correctedState struct {
	// winMax and winMinC are the extremes since the last Tick, swapped out by
	// Tick, exactly like Collector.winMax and winMinC. They sit on their own
	// line, away from the histogram's counters.
	winMax  atomic.Int64
	winMinC atomic.Int64
	_       [cacheLineSize - 16]byte

	// latency is the cumulative histogram; latency.count is the number of
	// corrected observations.
	latency Histogram

	// prev is the bucket counts as of the previous Tick and win the difference
	// to the current ones, reused across ticks so Tick does not allocate.
	prev [numBuckets]uint64
	win  sparseCounts
}

// correctedState returns the collector's corrected-latency state, creating it
// on first use. The fast path is one atomic load.
func (c *Collector) correctedState() *correctedState {
	if cs := c.corr.Load(); cs != nil {
		return cs
	}
	return c.newCorrectedState()
}

// newCorrectedState is the slow path of correctedState. The mutex keeps
// concurrent first observations from each allocating a state only to throw it
// away.
func (c *Collector) newCorrectedState() *correctedState {
	c.epMu.Lock()
	defer c.epMu.Unlock()
	if cs := c.corr.Load(); cs != nil {
		return cs
	}
	cs := new(correctedState)
	c.corr.Store(cs)
	return cs
}

// observe records one corrected latency in ns (> 0). The extremes are written
// before the histogram's bucket and count, the order Tick relies on.
func (cs *correctedState) observe(v int64) {
	atomicMax(&cs.winMax, v)
	atomicMax(&cs.winMinC, math.MaxInt64-v)
	cs.latency.observe(v)
}

// closeWindow turns the corrected observations since the previous Tick into
// the window's p95 and p99 in ms, both 0 when the window has none. Collector.mu
// must be held.
func (cs *correctedState) closeWindow() (p95, p99 float64) {
	cs.win.reset()
	cs.win.addDiff(&cs.latency.counts, &cs.prev)
	lo, hi := takeExtremes(&cs.winMax, &cs.winMinC, cs.win.total > 0)
	if cs.win.total == 0 {
		return 0, 0
	}
	var v [len(correctedQuantiles)]int64
	cs.win.walk(correctedQuantiles[:], v[:])
	return nsToMs(min(max(v[0], lo), hi)), nsToMs(min(max(v[1], lo), hi))
}

// reset clears the state in place. Collector.mu must be held. As with
// Histogram.Reset, observations racing with it may be partially kept.
func (cs *correctedState) reset() {
	cs.winMax.Store(0)
	cs.winMinC.Store(0)
	cs.latency.Reset()
	clear(cs.prev[:])
	cs.win.reset()
}
