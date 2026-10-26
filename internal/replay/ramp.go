package replay

import (
	"math"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// segment is a stretch of the arrival-rate curve over which the rate moves
// linearly from r0 to r1 requests per second.
type segment struct {
	t0     float64 // seconds before the segment starts
	dur    float64 // seconds
	r0, r1 float64
	n0     float64 // arrivals before the segment starts
	area   float64 // arrivals inside the segment
}

// rateCurve is the arrival-rate function of a rate-mode job: a list of
// linear segments (from Stages) followed by a tail at a constant rate that
// holds forever. Arrival n (0-based) happens when the cumulative arrival
// count N(t) reaches n, so a constant rate r gives arrivals at exactly
// n/r seconds, and a ramp gives as many arrivals as the area under it.
type rateCurve struct {
	segs     []segment
	total    float64 // arrivals through the last segment
	finiteAt float64 // seconds at which the last segment ends
	tail     float64 // rate after the last segment
	cur      int     // segment cursor for the monotonic arrival lookup
}

// newRateCurve builds the curve for cfg: Stages if present (each ramping
// linearly from the previous stage's rate, starting at 0, or stepping to its
// rate with Hold), otherwise the constant Rate.
func newRateCurve(cfg model.ReplayConfig) *rateCurve {
	c := &rateCurve{}
	if len(cfg.Stages) == 0 {
		c.tail = cfg.Rate
		return c
	}
	prev := 0.0
	for _, s := range cfg.Stages {
		d := s.Duration.D().Seconds()
		r0 := prev
		if s.Hold {
			r0 = s.Rate
		}
		seg := segment{t0: c.finiteAt, dur: d, r0: r0, r1: s.Rate, n0: c.total, area: (r0 + s.Rate) / 2 * d}
		c.segs = append(c.segs, seg)
		c.total += seg.area
		c.finiteAt += d
		prev = s.Rate
	}
	c.tail = prev
	return c
}

// finiteEnd is where the staged part of the curve ends.
func (c *rateCurve) finiteEnd() time.Duration { return secondsToDuration(c.finiteAt) }

// cumulative returns N(t): the arrivals expected in the first t seconds.
func (c *rateCurve) cumulative(t float64) float64 {
	if t <= 0 {
		return 0
	}
	for i := range c.segs {
		s := &c.segs[i]
		if t < s.dur {
			a := (s.r1 - s.r0) / s.dur
			return s.n0 + s.r0*t + a*t*t/2
		}
		t -= s.dur
	}
	return c.total + c.tail*t
}

// arrival returns the time in seconds of arrival n (0-based), or ok=false
// when the curve never produces that many arrivals (a zero tail rate).
// Successive calls with non-decreasing n are O(1).
func (c *rateCurve) arrival(n int64) (t float64, ok bool) {
	x := float64(n)
	if x >= c.total {
		if c.tail <= 0 {
			return 0, false
		}
		return c.finiteAt + (x-c.total)/c.tail, true
	}
	// The cursor only moves forward unless the caller rewinds.
	if c.cur >= len(c.segs) || x < c.segs[c.cur].n0 {
		c.cur = 0
	}
	for x >= c.segs[c.cur].n0+c.segs[c.cur].area {
		c.cur++
	}
	s := &c.segs[c.cur]
	return s.t0 + solveSegment(s, x-s.n0), true
}

// solveSegment returns the time into the segment at which x arrivals have
// happened: the root of r0·τ + a·τ²/2 = x, computed in the form that stays
// stable when the rate starts at (or crosses) zero.
func solveSegment(s *segment, x float64) float64 {
	if x <= 0 {
		return 0
	}
	a := (s.r1 - s.r0) / s.dur
	disc := s.r0*s.r0 + 2*a*x
	if disc < 0 {
		disc = 0
	}
	den := s.r0 + math.Sqrt(disc)
	if den <= 0 {
		return s.dur
	}
	return min(2*x/den, s.dur)
}

// secondsToDuration converts without overflowing.
func secondsToDuration(s float64) time.Duration {
	if s >= float64(math.MaxInt64)/1e9 {
		return time.Duration(math.MaxInt64)
	}
	if s <= 0 {
		return 0
	}
	return time.Duration(s * 1e9)
}
