package metrics

import (
	"math"
	"math/bits"
	"slices"
	"sync/atomic"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// cacheLineSize is the padding unit that keeps independently written atomics
// on separate cache lines. 128 bytes covers Apple silicon and POWER as well as
// the 64-byte lines of amd64 and most arm64.
const cacheLineSize = 128

// Bucket layout (HdrHistogram style, unit = 1ns).
//
// Values below subBucketCount (256ns) each have their own bucket. Above that,
// every power-of-two range [2^k, 2^(k+1)) is split into subBucketCount/2 = 128
// equal sub-buckets, so a bucket is never wider than 1/128 of its lower bound.
// Reporting a bucket's midpoint therefore bounds the relative error by
// 1/256 = 0.39%, well inside the 1% target, across the whole range.
//
// Values above maxTrackable (2^40-1 ns, about 18.3 minutes) are counted in the
// last bucket; Min, Max, Sum and Mean stay exact regardless.
const (
	subBucketBits    = 8
	subBucketCount   = 1 << subBucketBits // 256
	subHalfBits      = subBucketBits - 1  // 7
	maxTrackableBits = 40
	maxTrackable     = 1<<maxTrackableBits - 1 // ns

	// numBuckets is the index of the last bucket plus one: 32 shifted ranges
	// of 128 sub-buckets plus the 256 linear buckets below 256ns.
	numBuckets = (maxTrackableBits-subBucketBits)<<subHalfBits + subBucketCount // 4352
)

// bucketIndex maps a value in nanoseconds to its bucket.
func bucketIndex(v uint64) int {
	if v > maxTrackable {
		v = maxTrackable
	}
	shift := bits.Len64(v) - subBucketBits
	if shift < 0 {
		shift = 0
	}
	return shift<<subHalfBits + int(v>>uint(shift))
}

// bucketBounds returns the lowest value held by bucket idx and the bucket's
// width, both in nanoseconds.
func bucketBounds(idx int) (lo, width uint64) {
	if idx < subBucketCount {
		return uint64(idx), 1
	}
	shift := uint(idx>>subHalfBits - 1)
	sub := uint64(idx) - uint64(shift)<<subHalfBits
	return sub << shift, 1 << shift
}

// bucketValue is the representative (midpoint) value of bucket idx in ns.
func bucketValue(idx int) int64 {
	lo, w := bucketBounds(idx)
	return int64(lo + w/2)
}

// rankFor converts quantile q into a 1-based rank among n observations using
// the nearest-rank definition: ceil(q*n), clamped to [1, n].
func rankFor(q float64, n uint64) uint64 {
	if !(q > 0) { // also catches NaN
		return 1
	}
	if q >= 1 {
		return n
	}
	// The epsilon keeps products like 0.99*100 = 99.00000000000001 from
	// rounding up to rank 100.
	r := uint64(math.Ceil(q*float64(n) - 1e-9))
	return min(max(r, 1), n)
}

// atomicMax raises a to v if v is larger.
func atomicMax(a *atomic.Int64, v int64) {
	for {
		cur := a.Load()
		if v <= cur || a.CompareAndSwap(cur, v) {
			return
		}
	}
}

// Histogram is a concurrent latency histogram: log-linear buckets with atomic
// counts covering 0 to about 18 minutes at ≤0.4% relative error (1µs to 10
// minutes comfortably included). Record, Merge, Reset and every reader are
// safe for concurrent use and never block. Readers see a view that may lag
// writers that are in flight at that moment; once writers stop the totals are
// exact.
//
// The zero value is an empty histogram ready to use. A Histogram must not be
// copied after first use; use Clone.
type Histogram struct {
	// count and sum are written by every Record, so they get a cache line to
	// themselves; max and minC are almost only read (a Record writes them
	// only when it sets a new extreme) and must not share a line with
	// counters other cores are hammering, or every read would miss.
	count atomic.Int64
	sum   atomic.Int64 // ns; wraps only after ~292 years of accumulated latency
	_     [cacheLineSize - 16]byte

	max atomic.Int64 // ns
	// minC stores math.MaxInt64 - min so that the zero value means "no
	// minimum yet" and both extremes can use the same atomicMax loop.
	minC atomic.Int64
	_    [cacheLineSize - 16]byte

	counts [numBuckets]atomic.Uint64
}

// NewHistogram returns an empty histogram.
func NewHistogram() *Histogram { return new(Histogram) }

// Record adds one observation. Negative durations count as 0.
func (h *Histogram) Record(d time.Duration) {
	h.observe(max(int64(d), 0))
}

// observe records a non-negative value in ns. The count is written last so a
// reader that sees it also sees the min, max, sum and bucket.
func (h *Histogram) observe(v int64) {
	atomicMax(&h.max, v)
	atomicMax(&h.minC, math.MaxInt64-v)
	h.counts[bucketIndex(uint64(v))].Add(1)
	h.sum.Add(v)
	h.count.Add(1)
}

// Merge adds every observation in o to h. It is safe to call while either
// histogram is being recorded into. A nil o is ignored.
func (h *Histogram) Merge(o *Histogram) {
	if o == nil {
		return
	}
	n := o.count.Load()
	if n <= 0 {
		return
	}
	sum := o.sum.Load()
	atomicMax(&h.max, o.max.Load())
	atomicMax(&h.minC, o.minC.Load())
	for i := range o.counts {
		if v := o.counts[i].Load(); v != 0 {
			h.counts[i].Add(v)
		}
	}
	h.sum.Add(sum)
	h.count.Add(n)
}

// Clone returns an independent copy of the histogram.
func (h *Histogram) Clone() *Histogram {
	c := NewHistogram()
	c.Merge(h)
	return c
}

// Reset clears the histogram. Records racing with Reset may be partially
// kept; callers that need a clean cut stop recording first.
func (h *Histogram) Reset() {
	h.count.Store(0)
	h.sum.Store(0)
	h.max.Store(0)
	h.minC.Store(0)
	for i := range h.counts {
		h.counts[i].Store(0)
	}
}

// Count returns the number of observations.
func (h *Histogram) Count() int64 { return h.count.Load() }

// Sum returns the exact sum of all observations.
func (h *Histogram) Sum() time.Duration { return time.Duration(h.sum.Load()) }

// Min returns the exact smallest observation, 0 when empty.
func (h *Histogram) Min() time.Duration {
	if h.count.Load() <= 0 {
		return 0
	}
	lo, _ := h.bounds()
	return time.Duration(lo)
}

// Max returns the exact largest observation, 0 when empty.
func (h *Histogram) Max() time.Duration {
	if h.count.Load() <= 0 {
		return 0
	}
	_, hi := h.bounds()
	return time.Duration(hi)
}

// bounds returns the exact min and max in ns, repairing the transient state
// in which a concurrent writer has not yet published its min.
func (h *Histogram) bounds() (lo, hi int64) {
	hi = h.max.Load()
	lo = math.MaxInt64 - h.minC.Load()
	if lo > hi {
		lo = hi
	}
	return lo, hi
}

// Quantile returns the q-quantile (0 ≤ q ≤ 1); 0 when empty. Values of q
// outside [0, 1] are clamped and NaN is treated as 0. q=0 returns the exact
// minimum and q=1 the exact maximum; anything in between is the midpoint of
// the bucket holding the nearest-rank observation, within 0.4% of the true
// value and never outside [Min, Max].
func (h *Histogram) Quantile(q float64) time.Duration {
	qs := [1]float64{q}
	var out [1]int64
	h.valuesFor(qs[:], out[:])
	return time.Duration(out[0])
}

// Quantiles returns Quantile(q) for each q, computed in a single pass over
// the buckets. The result has the same length and order as qs.
func (h *Histogram) Quantiles(qs ...float64) []time.Duration {
	res := make([]time.Duration, len(qs))
	if len(qs) == 0 {
		return res
	}
	type iq struct {
		i int
		q float64
	}
	order := make([]iq, len(qs))
	for i, q := range qs {
		if math.IsNaN(q) {
			q = 0
		}
		order[i] = iq{i, q}
	}
	slices.SortStableFunc(order, func(a, b iq) int {
		switch {
		case a.q < b.q:
			return -1
		case a.q > b.q:
			return 1
		}
		return 0
	})
	sorted := make([]float64, len(qs))
	for k, o := range order {
		sorted[k] = o.q
	}
	out := make([]int64, len(qs))
	h.valuesFor(sorted, out)
	for k, o := range order {
		res[o.i] = time.Duration(out[k])
	}
	return res
}

// valuesFor fills out[i] with the value in ns at quantile qs[i]. qs must be
// ascending and the same length as out. An empty histogram yields zeros.
func (h *Histogram) valuesFor(qs []float64, out []int64) {
	n := h.count.Load()
	if n <= 0 {
		clear(out)
		return
	}
	lo, hi := h.bounds()
	h.valuesWith(uint64(n), lo, hi, qs, out)
}

// valuesWith is valuesFor with the count and exact extremes supplied, so a
// caller that also reports them (Summary) uses one consistent reading.
func (h *Histogram) valuesWith(n uint64, lo, hi int64, qs []float64, out []int64) {
	walkDense(&h.counts, n, qs, out)
	for i, q := range qs {
		out[i] = finishQuantile(q, out[i], lo, hi)
	}
}

// finishQuantile pins the extremes to the exact min and max and keeps every
// other estimate inside [lo, hi].
func finishQuantile(q float64, v, lo, hi int64) int64 {
	switch {
	case !(q > 0):
		return lo
	case q >= 1:
		return hi
	}
	return min(max(v, lo), hi)
}

// walkDense finds, for each ascending quantile in qs, the representative value
// of the bucket holding that quantile's rank among n observations.
func walkDense(counts *[numBuckets]atomic.Uint64, n uint64, qs []float64, out []int64) {
	var buf [8]uint64
	ranks := buf[:0]
	if len(qs) > len(buf) {
		ranks = make([]uint64, 0, len(qs))
	}
	for _, q := range qs {
		ranks = append(ranks, rankFor(q, n))
	}
	var (
		j    int
		cum  uint64
		last int64
	)
	for i := 0; i < numBuckets && j < len(ranks); i++ {
		c := counts[i].Load()
		if c == 0 {
			continue
		}
		cum += c
		last = bucketValue(i)
		for j < len(ranks) && cum >= ranks[j] {
			out[j] = last
			j++
		}
	}
	// Writers in flight can leave the buckets short of n; use the top bucket.
	for ; j < len(ranks); j++ {
		out[j] = last
	}
}

// summaryQuantiles are the quantiles reported by Summary, in order.
var summaryQuantiles = [...]float64{0.5, 0.9, 0.95, 0.99, 0.999}

// nsToMs converts nanoseconds to fractional milliseconds.
func nsToMs(ns int64) float64 { return float64(ns) / 1e6 }

// Summary returns count, min, mean, p50, p90, p95, p99, p99.9 and max in ms.
func (h *Histogram) Summary() model.LatencySummary {
	n := h.count.Load()
	if n <= 0 {
		return model.LatencySummary{}
	}
	lo, hi := h.bounds()
	var v [len(summaryQuantiles)]int64
	h.valuesWith(uint64(n), lo, hi, summaryQuantiles[:], v[:])
	return model.LatencySummary{
		Count: n,
		Min:   nsToMs(lo),
		Mean:  float64(h.sum.Load()) / float64(n) / 1e6,
		P50:   nsToMs(v[0]),
		P90:   nsToMs(v[1]),
		P95:   nsToMs(v[2]),
		P99:   nsToMs(v[3]),
		P999:  nsToMs(v[4]),
		Max:   nsToMs(hi),
	}
}

// sparseCounts is a compact, immutable-after-build list of non-empty buckets
// in ascending index order. Collectors keep one per closed one-second window.
type sparseCounts struct {
	idx   []uint16
	cnt   []uint64
	total uint64
}

func (s *sparseCounts) reset() {
	s.idx = s.idx[:0]
	s.cnt = s.cnt[:0]
	s.total = 0
}

func (s *sparseCounts) add(idx int, n uint64) {
	s.idx = append(s.idx, uint16(idx))
	s.cnt = append(s.cnt, n)
	s.total += n
}

// walk is walkDense for a sparse bucket list; qs must be ascending.
func (s *sparseCounts) walk(qs []float64, out []int64) {
	var buf [8]uint64
	ranks := buf[:0]
	if len(qs) > len(buf) {
		ranks = make([]uint64, 0, len(qs))
	}
	for _, q := range qs {
		ranks = append(ranks, rankFor(q, s.total))
	}
	var (
		j    int
		cum  uint64
		last int64
	)
	for k, c := range s.cnt {
		cum += c
		last = bucketValue(int(s.idx[k]))
		for j < len(ranks) && cum >= ranks[j] {
			out[j] = last
			j++
		}
		if j == len(ranks) {
			return
		}
	}
	for ; j < len(ranks); j++ {
		out[j] = last
	}
}

// addTo adds the sparse counts into dense (sign +1) or removes them (-1).
func (s *sparseCounts) addTo(dense *Histogram, remove bool) {
	for k, i := range s.idx {
		if remove {
			dense.counts[i].Add(^(s.cnt[k] - 1)) // atomic subtract
		} else {
			dense.counts[i].Add(s.cnt[k])
		}
	}
}
