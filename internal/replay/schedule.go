package replay

import (
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// job is one scheduled request: what to send, when it was supposed to go out
// and which amplified copy of the recorded request it is.
type job struct {
	it *item
	// due is the intended send time since the run started. A negative due
	// (max mode) means "as soon as a worker is free": the closed model has no
	// intended time.
	due time.Duration
	// copy is the 0-based amplify copy index, which selects the clone
	// identity when VaryClones is set.
	copy int
}

// generator produces the schedule lazily, in non-decreasing due order. It is
// used by the scheduler goroutine only.
type generator interface {
	// next returns the next request, or ok=false at the end of the schedule.
	next() (job, bool)
	// end returns the time the schedule is supposed to end when that is later
	// than its last request (a rate curve ending in a quiet stage); 0 means
	// the schedule ends with its last request.
	end() time.Duration
}

// newRNG returns the PCG stream all of a job's randomness comes from.
func newRNG(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
}

// sampler decides how many times a recorded request is scheduled under
// ErrorAmplification k: error items floor(k) times plus once more with
// probability frac(k); every other item once.
type sampler struct {
	k     float64
	whole int
	frac  float64
	rng   *rand.Rand
}

func newSampler(k float64, seed uint64) *sampler {
	whole := int(math.Floor(k))
	return &sampler{k: k, whole: whole, frac: k - float64(whole), rng: newRNG(seed)}
}

func (s *sampler) copies(it *item) int {
	if !it.isErr() || s.k == 1 {
		return 1
	}
	n := s.whole
	if s.frac > 0 && s.rng.Float64() < s.frac {
		n++
	}
	return n
}

// timelineGen replays the recorded inter-arrival times compressed by speed.
//
// Requests recorded at the same instant form a group, due at pass shift +
// off/speed. The group's copies (error copies times amplify, summed over its
// requests) are spread evenly over the gap to the next later request: copy k
// of G is due at base + gap·k/G. Copies go round by round, copy 0 of every
// request in the group first, then copy 1, and so on, so a request's copies
// are spread over the gap and the due times never go backwards. The last
// group of a pass has the mean gap after it.
type timelineGen struct {
	items  []item
	speed  float64
	amp    int
	loop   bool
	smp    *sampler
	period time.Duration // unscaled pass length
	n      int

	pass  int64
	next0 int           // first item of the next group
	shift time.Duration // scaled start of the current pass

	// The current group.
	gs     int
	counts []int32 // copies per request of the group
	active []int32 // requests (relative to gs) that still have a copy this round
	round  int
	pos    int           // next entry of active
	k      int64         // copies emitted so far
	total  int64         // copies in the group
	base   time.Duration // scaled due of the group
	gap    time.Duration // scaled gap to the next later request
}

func newTimelineGen(p *prepared, cfg model.ReplayConfig) *timelineGen {
	return &timelineGen{
		items: p.items, speed: cfg.Speed, amp: cfg.Amplify, loop: cfg.Loop,
		smp: newSampler(cfg.ErrorAmplification, cfg.Seed), period: p.period, n: len(p.items),
	}
}

func (g *timelineGen) scaled(d time.Duration) time.Duration {
	return time.Duration(float64(d) / g.speed)
}

// copyDue is the due time of copy k of total spread over gap after base.
func copyDue(base, gap time.Duration, k, total int64) time.Duration {
	if k == 0 {
		return base
	}
	return base + time.Duration(float64(gap)*float64(k)/float64(total))
}

func (g *timelineGen) end() time.Duration { return 0 }

func (g *timelineGen) next() (job, bool) {
	for g.k >= g.total {
		if !g.advance() {
			return job{}, false
		}
	}
	if g.pos >= len(g.active) {
		g.nextRound()
	}
	rel := g.active[g.pos]
	g.pos++
	j := job{it: &g.items[g.gs+int(rel)], due: copyDue(g.base, g.gap, g.k, g.total), copy: g.round % g.amp}
	g.k++
	return j, true
}

// nextRound drops the requests whose copies are used up and starts the next
// round over the rest.
func (g *timelineGen) nextRound() {
	keep := g.active[:0]
	for _, rel := range g.active {
		if int(g.counts[rel]) > g.round+1 {
			keep = append(keep, rel)
		}
	}
	g.active = keep
	g.round++
	g.pos = 0
}

// startPass positions the generator at the first group of pass p.
func (g *timelineGen) startPass(p int64) {
	g.pass = p
	g.next0 = 0
	g.shift = g.scaled(time.Duration(float64(g.period) * float64(p)))
}

// advance moves to the next group that is scheduled at least once.
func (g *timelineGen) advance() bool {
	for {
		if g.next0 >= g.n {
			if !g.loop {
				g.k, g.total = 0, 0
				return false
			}
			g.startPass(g.pass + 1)
		}
		gs := g.next0
		off := g.items[gs].off
		ge := gs + 1
		for ge < g.n && g.items[ge].off == off {
			ge++
		}
		g.next0 = ge

		g.counts, g.active = g.counts[:0], g.active[:0]
		var total int64
		for i := gs; i < ge; i++ {
			m := int32(g.smp.copies(&g.items[i]) * g.amp)
			g.counts = append(g.counts, m)
			if m > 0 {
				g.active = append(g.active, int32(i-gs))
				total += int64(m)
			}
		}
		if total == 0 {
			continue
		}
		next := g.period
		if ge < g.n {
			next = g.items[ge].off
		}
		g.gs, g.total, g.k = gs, total, 0
		g.round, g.pos = 0, 0
		g.base = g.shift + g.scaled(off)
		g.gap = g.scaled(next - off)
		return true
	}
}

// countBefore returns how many copies of the current group are due before
// limit (limit <= 0 means all of them).
func (g *timelineGen) countBefore(limit time.Duration) int64 {
	if limit <= 0 || copyDue(g.base, g.gap, g.total-1, g.total) < limit {
		return g.total
	}
	if g.base >= limit {
		return 0
	}
	// Smallest k in [1, total) with due >= limit; copyDue is monotonic in k.
	lo, hi := int64(1), g.total-1
	for lo < hi {
		mid := (lo + hi) / 2
		if copyDue(g.base, g.gap, mid, g.total) >= limit {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// timelinePlan is what the schedule will send, for Summary.Planned and
// TargetRPS.
type timelinePlan struct {
	planned  int64 // exact for a single pass (-1 when unknown)
	perPass  int64 // requests in the first pass
	targetRP float64
}

// loopPlanned counts the requests a looping timeline sends before limit, by
// the generator's own arithmetic. Every pass before the last two is complete
// (its last request is due a whole pass before limit), so those are counted
// as perPass each and only the passes around limit are walked. It reports
// false when the passes are not alike: with a fractional ErrorAmplification
// over a recording that has errors, each pass draws its own copies, so only
// the first pass is known.
func loopPlanned(p *prepared, cfg model.ReplayConfig, perPass int64, limit time.Duration) (int64, bool) {
	k := cfg.ErrorAmplification
	if k != math.Floor(k) && slices.ContainsFunc(p.items, func(it item) bool { return it.isErr() }) {
		return 0, false
	}
	g := newTimelineGen(p, cfg)
	g.loop = false
	period := g.scaled(p.period)
	if period <= 0 {
		return 0, false
	}
	full := max(int64(limit/period)-1, 0)
	n := full * perPass
	for pass := full; ; pass++ {
		g.startPass(pass)
		if g.shift >= limit {
			return n, true
		}
		for g.advance() {
			n += g.countBefore(limit)
		}
	}
}

// planTimeline walks the first pass the way the generator will (same seed, so
// the same error-amplification draws) and counts the requests. Planned is
// exact for a single pass cut by Duration/MaxRequests, exact for a loop with a
// Duration unless fractional error amplification makes the passes differ (then
// an estimate from the pass rate), and MaxRequests for loops with no Duration.
func planTimeline(p *prepared, cfg model.ReplayConfig) timelinePlan {
	g := newTimelineGen(p, cfg)
	g.loop = false
	limit := cfg.Duration.D()
	var perPass, cut int64
	for g.advance() {
		perPass += g.total
		cut += g.countBefore(limit)
	}
	period := g.scaled(p.period)
	plan := timelinePlan{perPass: perPass}
	if period > 0 {
		plan.targetRP = float64(perPass) / period.Seconds()
	}
	switch {
	case !cfg.Loop:
		plan.planned = cut
	case cfg.Duration > 0:
		if n, ok := loopPlanned(p, cfg, perPass, limit); ok {
			plan.planned = n
		} else {
			plan.planned = int64(math.Round(plan.targetRP * limit.Seconds()))
		}
	case cfg.MaxRequests > 0:
		plan.planned = cfg.MaxRequests
	default:
		plan.planned = -1
	}
	if cfg.MaxRequests > 0 && (plan.planned < 0 || plan.planned > cfg.MaxRequests) {
		plan.planned = cfg.MaxRequests
	}
	return plan
}

// cycler walks the items in time order, repeating them (rate mode, and max
// mode with Loop), consuming one request per call with the error-amplification
// sampling applied.
type cycler struct {
	items []item
	smp   *sampler
	loop  bool
	idx   int
	rem   int
	empty int // consecutive items sampled out
}

func newCycler(items []item, k float64, seed uint64, loop bool) *cycler {
	return &cycler{items: items, smp: newSampler(k, seed), loop: loop, idx: -1}
}

func (c *cycler) next() (*item, bool) {
	for c.rem == 0 {
		c.idx++
		if c.idx >= len(c.items) {
			if !c.loop {
				c.idx = len(c.items) - 1
				return nil, false
			}
			c.idx = 0
		}
		c.rem = c.smp.copies(&c.items[c.idx])
		if c.rem == 0 {
			// A vanishing ErrorAmplification on a recording of mostly errors
			// could otherwise spin for ages; force progress.
			if c.empty++; c.empty > 4*len(c.items)+1000 {
				c.rem = 1
			}
		}
		if c.rem > 0 {
			c.empty = 0
		}
	}
	c.rem--
	return &c.items[c.idx], true
}

// rateGen sends along a rate curve, cycling the recording.
type rateGen struct {
	curve *rateCurve
	cyc   *cycler
	n     int64
	limit time.Duration // 0 = none
}

// newRateGen schedules cfg's Rate/Stages. The schedule ends at Duration, or
// at the end of the stages when Duration is 0; with a constant Rate and no
// Duration it runs until MaxRequests.
func newRateGen(p *prepared, cfg model.ReplayConfig) *rateGen {
	g := &rateGen{curve: newRateCurve(cfg), cyc: newCycler(p.items, cfg.ErrorAmplification, cfg.Seed, true)}
	g.limit = cfg.Duration.D()
	if g.limit == 0 && len(cfg.Stages) > 0 {
		g.limit = g.curve.finiteEnd()
	}
	return g
}

func (g *rateGen) end() time.Duration { return g.limit }

func (g *rateGen) next() (job, bool) {
	t, ok := g.curve.arrival(g.n)
	if !ok {
		return job{}, false
	}
	due := secondsToDuration(t)
	if g.limit > 0 && due >= g.limit {
		return job{}, false
	}
	it, ok := g.cyc.next()
	if !ok {
		return job{}, false
	}
	g.n++
	return job{it: it, due: due}, true
}

// planRate returns Planned (the integral of the rate over the run, capped by
// MaxRequests) and the average target rate.
func planRate(cfg model.ReplayConfig, g *rateGen) (planned int64, targetRPS float64) {
	if g.limit <= 0 {
		// Constant rate until MaxRequests.
		return cfg.MaxRequests, g.curve.tail
	}
	sec := g.limit.Seconds()
	area := g.curve.cumulative(sec)
	planned = int64(math.Ceil(area - 1e-9))
	if cfg.MaxRequests > 0 && planned > cfg.MaxRequests {
		planned = cfg.MaxRequests
	}
	return planned, area / sec
}

// maxGen feeds the closed-loop workers back to back.
type maxGen struct{ cyc *cycler }

func newMaxGen(p *prepared, cfg model.ReplayConfig) *maxGen {
	return &maxGen{cyc: newCycler(p.items, cfg.ErrorAmplification, cfg.Seed, cfg.Loop)}
}

func (g *maxGen) end() time.Duration { return 0 }

func (g *maxGen) next() (job, bool) {
	it, ok := g.cyc.next()
	if !ok {
		return job{}, false
	}
	return job{it: it, due: -1}, true
}

// planMax counts a single pass the way the generator will; loops are bounded
// only by MaxRequests.
func planMax(p *prepared, cfg model.ReplayConfig) int64 {
	if cfg.Loop {
		if cfg.MaxRequests > 0 {
			return cfg.MaxRequests
		}
		return -1
	}
	smp := newSampler(cfg.ErrorAmplification, cfg.Seed)
	var total int64
	for i := range p.items {
		total += int64(smp.copies(&p.items[i]))
	}
	if cfg.MaxRequests > 0 && total > cfg.MaxRequests {
		total = cfg.MaxRequests
	}
	return total
}

// newGenerator builds the generator for cfg's mode together with Planned and
// the average target rate.
func newGenerator(p *prepared, cfg model.ReplayConfig) (g generator, planned int64, targetRPS float64) {
	switch cfg.Mode {
	case model.ReplayRate:
		rg := newRateGen(p, cfg)
		planned, targetRPS = planRate(cfg, rg)
		return rg, planned, targetRPS
	case model.ReplayMax:
		return newMaxGen(p, cfg), planMax(p, cfg), 0
	default:
		plan := planTimeline(p, cfg)
		return newTimelineGen(p, cfg), plan.planned, plan.targetRP
	}
}
