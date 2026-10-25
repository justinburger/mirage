package metrics

import (
	"math"
	"sort"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// windowQuantiles are the series point percentiles (p50, p95, p99).
var windowQuantiles = [...]float64{0.5, 0.95, 0.99}

// tickJitter is how long after the previous Tick the next one still counts as
// "the next second" rather than a missed one.
const tickJitter = 1500 * time.Millisecond

// Tick closes the current one-second window into a SeriesPoint, appends it to
// the ring and returns it. Normally called by Registry.Run; standalone users
// (e.g. the CLI replay) call it themselves once per second.
//
// The window is the second that just ended: the point is stamped with the
// start of second floor(now)-1, so a Tick at 12:00:05.003 closes the window
// stamped 12:00:04. Points are strictly consecutive seconds:
//
//   - A Tick less than 1.5s after the previous one (or with a clock that
//     stepped back) closes the very next second after the previous point, so
//     a standalone ticker with sub-second jitter near a second boundary never
//     skips or repeats a stamp, and two calls within one second still get
//     distinct stamps.
//   - A later Tick first appends zero points for the seconds it skipped (at
//     most SeriesCapacity of them), so charts have no gaps, then closes the
//     traffic seen since the last Tick into its own second.
//
// The window is computed as the difference between the cumulative counters and
// histogram now and at the previous Tick, so a concurrent Observe is counted in
// exactly one window. The fields of one observation (count, error, bytes,
// latency bucket) are separate atomic updates, so an observation straddling a
// Tick can have its fields split across two adjacent windows; the totals over
// all windows always add up exactly.
func (c *Collector) Tick(now time.Time) model.SeriesPoint {
	c.mu.Lock()
	defer c.mu.Unlock()

	sec := now.Unix() - 1
	if c.ticked {
		if now.Sub(c.lastNow) < tickJitter || sec <= c.lastSec {
			sec = c.lastSec + 1
		}
		gap := min(sec-c.lastSec-1, SeriesCapacity)
		for i := gap; i >= 1; i-- {
			c.appendEmptyWindow(sec - i)
		}
	}
	p := c.closeWindow(sec)
	c.ticked = true
	c.lastSec = sec
	c.lastNow = now
	return p
}

// nextWindow returns the slot for the window about to be closed, first
// removing the window it replaces from the recent-latency sum.
func (c *Collector) nextWindow() *windowStats {
	w := &c.wins[c.winPos]
	if c.winLen == len(c.wins) {
		w.addTo(c.recent, true)
	} else {
		c.winLen++
	}
	c.winPos = (c.winPos + 1) % len(c.wins)
	w.reset()
	return w
}

// appendEmptyWindow adds a zero point for a second the collector never ticked.
func (c *Collector) appendEmptyWindow(sec int64) {
	c.nextWindow()
	c.pushPoint(model.SeriesPoint{T: sec * 1000, InFlight: c.lastInFlight})
}

// closeWindow turns everything observed since the previous Tick into the
// window for second sec.
func (c *Collector) closeWindow(sec int64) model.SeriesPoint {
	w := c.nextWindow()

	// Read in the reverse of the order Observe writes (errors and bytes, then
	// the window extremes, then latency max/min, bucket, sum and finally
	// count). Everything an already-counted observation wrote is then visible
	// here, so a counted request is never missing its error, bytes, latency
	// bucket or extremes; at worst a window also carries the early fields of a
	// request that is counted in the next one.
	w.requests = delta(c.latency.count.Load(), &c.prevCount)
	w.sum = delta(c.latency.sum.Load(), &c.prevSum)
	for i := range c.latency.counts {
		v := c.latency.counts[i].Load()
		p := c.prev[i]
		if v == p {
			continue
		}
		c.prev[i] = v
		if v > p { // v < p only after a Reset raced the scan
			w.add(i, v-p)
		}
	}
	mn := c.winMinC.Swap(0)
	w.max = c.winMax.Swap(0)
	if w.total == 0 && w.requests == 0 {
		// Extremes without an observation: Observe publishes them before its
		// bucket and count, so a request is mid-flight. Hand them to the next
		// window, where the request will be counted, instead of losing them.
		if w.max > 0 {
			atomicMax(&c.winMax, w.max)
		}
		if mn != 0 {
			atomicMax(&c.winMinC, mn)
		}
		w.min, w.max = 0, 0
	} else {
		if mn != 0 {
			w.min = math.MaxInt64 - mn
		}
		if mn == 0 || w.min > w.max {
			w.min = w.max
		}
	}
	w.bytesOut = delta(c.bytesOut.Load(), &c.prevOut)
	w.bytesIn = delta(c.bytesIn.Load(), &c.prevIn)
	w.errors = delta(c.errors.Load(), &c.prevErrs)
	w.addTo(c.recent, false)

	p := model.SeriesPoint{
		T:        sec * 1000,
		Requests: w.requests,
		Errors:   w.errors,
		InFlight: max(c.inflight.Load(), 0),
		BytesIn:  w.bytesIn,
		BytesOut: w.bytesOut,
	}
	if w.total > 0 {
		var v [len(windowQuantiles)]int64
		w.walk(windowQuantiles[:], v[:])
		p.P50 = nsToMs(min(max(v[0], w.min), w.max))
		p.P95 = nsToMs(min(max(v[1], w.min), w.max))
		p.P99 = nsToMs(min(max(v[2], w.min), w.max))
	}
	p.Max = nsToMs(w.max)
	c.lastInFlight = p.InFlight
	c.pushPoint(p)
	return p
}

// delta returns cur-*prev (never negative) and advances *prev to cur.
func delta(cur int64, prev *int64) int64 {
	d := cur - *prev
	*prev = cur
	return max(d, 0)
}

// pushPoint appends p to the ring, overwriting the oldest once it is full.
func (c *Collector) pushPoint(p model.SeriesPoint) {
	if len(c.ring) < SeriesCapacity {
		c.ring = append(c.ring, p)
		return
	}
	c.ring[c.head] = p
	c.head = (c.head + 1) % SeriesCapacity
}

// pointAt returns the i-th retained point, oldest first.
func (c *Collector) pointAt(i int) model.SeriesPoint {
	return c.ring[(c.head+i)%len(c.ring)]
}

// Series returns retained points with T >= sinceMs (Unix ms), oldest first.
// The result is a copy and never nil.
func (c *Collector) Series(sinceMs int64) []model.SeriesPoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.ring)
	first := sort.Search(n, func(i int) bool { return c.pointAt(i).T >= sinceMs })
	out := make([]model.SeriesPoint, n-first)
	for i := range out {
		out[i] = c.pointAt(first + i)
	}
	return out
}

// Snapshot returns the collector's current state. RPS is the request count of
// the last closed window; RPS10, ErrorRate10 and Recent cover the last 10
// closed windows (or fewer, averaged over the windows that exist, while the
// collector is younger than that). Endpoints carries the SnapshotEndpoints
// busiest endpoints.
func (c *Collector) Snapshot() model.StatsSnapshot {
	s := model.StatsSnapshot{
		Name:     c.name,
		Kind:     c.kind,
		ID:       c.id,
		Since:    time.Unix(0, c.since.Load()),
		Errors:   c.errors.Load(),
		BytesIn:  c.bytesIn.Load(),
		BytesOut: c.bytesOut.Load(),
		InFlight: max(c.inflight.Load(), 0),

		StatusClasses: make(map[string]int64, numClasses),
	}
	for i := range c.classes {
		s.StatusClasses[classNames[i]] = c.classes[i].Load()
	}
	s.Latency = c.latency.Summary()
	s.Requests = s.Latency.Count

	c.mu.Lock()
	s.RPS, s.RPS10, s.ErrorRate10, s.Recent = c.recentStats()
	c.mu.Unlock()

	s.Endpoints = c.topEndpoints(SnapshotEndpoints)
	return s
}

// recentStats derives the rates and the latency summary over the closed
// windows retained in c.wins. c.mu must be held.
func (c *Collector) recentStats() (rps, rps10, errRate float64, recent model.LatencySummary) {
	if c.winLen == 0 {
		return
	}
	newest := &c.wins[(c.winPos-1+len(c.wins))%len(c.wins)]
	rps = float64(newest.requests)

	var (
		reqs, errs, sum int64
		total           uint64
		lo              int64 = math.MaxInt64
		hi              int64
	)
	for i := 0; i < c.winLen; i++ {
		w := &c.wins[i]
		reqs += w.requests
		errs += w.errors
		sum += w.sum
		total += w.total
		if w.total > 0 || w.requests > 0 {
			lo = min(lo, w.min)
			hi = max(hi, w.max)
		}
	}
	rps10 = float64(reqs) / float64(c.winLen)
	if reqs > 0 {
		errRate = min(float64(errs)/float64(reqs), 1)
	}
	if total == 0 && reqs == 0 {
		return
	}
	lo = min(lo, hi)

	var v [len(summaryQuantiles)]int64
	if total > 0 {
		walkDense(&c.recent.counts, total, summaryQuantiles[:], v[:])
	}
	n := max(reqs, int64(total))
	recent = model.LatencySummary{
		Count: n,
		Min:   nsToMs(lo),
		Mean:  float64(sum) / float64(n) / 1e6,
		P50:   nsToMs(min(max(v[0], lo), hi)),
		P90:   nsToMs(min(max(v[1], lo), hi)),
		P95:   nsToMs(min(max(v[2], lo), hi)),
		P99:   nsToMs(min(max(v[3], lo), hi)),
		P999:  nsToMs(min(max(v[4], lo), hi)),
		Max:   nsToMs(hi),
	}
	return
}
