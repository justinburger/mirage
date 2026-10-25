package metrics

import (
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"
)

// exactQuantile is the nearest-rank quantile of sorted data, the definition
// Histogram.Quantile approximates.
func exactQuantile(sorted []int64, q float64) int64 {
	n := len(sorted)
	r := int(math.Ceil(q*float64(n) - 1e-9))
	r = min(max(r, 1), n)
	return sorted[r-1]
}

func relErr(got, want float64) float64 {
	if want == 0 {
		return math.Abs(got)
	}
	return math.Abs(got-want) / want
}

func TestBucketIndexLayout(t *testing.T) {
	// Every value maps into a bucket whose [lo, lo+width) range contains it,
	// indices never decrease with the value, and the relative width is bounded.
	values := []uint64{0, 1, 2, 127, 128, 255, 256, 257, 300, 511, 512, 1000, 1024, 1<<20 - 1, 1 << 20,
		1_000_000, 123_456_789, 600e9, 1<<39 - 1, 1 << 39, maxTrackable - 1, maxTrackable}
	slices.Sort(values)
	prev := -1
	for _, v := range values {
		idx := bucketIndex(v)
		if idx < 0 || idx >= numBuckets {
			t.Fatalf("bucketIndex(%d) = %d, out of [0,%d)", v, idx, numBuckets)
		}
		if idx < prev {
			t.Errorf("bucketIndex(%d) = %d, decreased from %d", v, idx, prev)
		}
		prev = idx
		lo, w := bucketBounds(idx)
		if v < lo || v >= lo+w {
			t.Errorf("v=%d not inside bucket %d [%d,%d)", v, idx, lo, lo+w)
		}
		if lo >= subBucketCount && float64(w)/float64(lo) > 1.0/128+1e-12 {
			t.Errorf("bucket %d too wide: width %d, lo %d", idx, w, lo)
		}
	}
	if got := bucketIndex(maxTrackable); got != numBuckets-1 {
		t.Errorf("max trackable lands in bucket %d, want last (%d)", got, numBuckets-1)
	}
	if got := bucketIndex(math.MaxUint64); got != numBuckets-1 {
		t.Errorf("overflow value lands in bucket %d, want last (%d)", got, numBuckets-1)
	}
	if got := bucketIndex(0); got != 0 {
		t.Errorf("bucketIndex(0) = %d", got)
	}
}

func TestBucketsAreContiguous(t *testing.T) {
	// Buckets tile the value line without gaps or overlap.
	var next uint64
	for idx := 0; idx < numBuckets; idx++ {
		lo, w := bucketBounds(idx)
		if lo != next {
			t.Fatalf("bucket %d starts at %d, want %d", idx, lo, next)
		}
		next = lo + w
		if got := bucketIndex(lo); got != idx {
			t.Fatalf("bucketIndex(lo of %d) = %d", idx, got)
		}
		if got := bucketIndex(lo + w - 1); got != idx {
			t.Fatalf("bucketIndex(hi of %d) = %d", idx, got)
		}
	}
	if next != maxTrackable+1 {
		t.Errorf("buckets end at %d, want %d", next, uint64(maxTrackable)+1)
	}
}

func TestRankFor(t *testing.T) {
	tests := []struct {
		q    float64
		n    uint64
		want uint64
	}{
		{0, 10, 1},
		{-1, 10, 1},
		{math.NaN(), 10, 1},
		{1, 10, 10},
		{2, 10, 10},
		{0.5, 10, 5},
		{0.99, 100, 99}, // 0.99*100 = 99.00000000000001 must not round up to 100
		{0.9, 10, 9},
		{0.999, 1000, 999},
		{0.001, 10, 1},
		{0.55, 10, 6},
		{0.5, 1, 1},
	}
	for _, tt := range tests {
		if got := rankFor(tt.q, tt.n); got != tt.want {
			t.Errorf("rankFor(%v, %d) = %d, want %d", tt.q, tt.n, got, tt.want)
		}
	}
}

func TestHistogramEmpty(t *testing.T) {
	h := NewHistogram()
	if h.Count() != 0 {
		t.Errorf("Count = %d", h.Count())
	}
	for _, q := range []float64{0, 0.5, 0.99, 1} {
		if got := h.Quantile(q); got != 0 {
			t.Errorf("Quantile(%v) on empty = %v", q, got)
		}
	}
	if s := h.Summary(); s.Count != 0 || s.Max != 0 || s.P99 != 0 || s.Min != 0 || s.Mean != 0 {
		t.Errorf("empty Summary = %+v", s)
	}
	if h.Min() != 0 || h.Max() != 0 || h.Sum() != 0 {
		t.Errorf("empty Min/Max/Sum = %v/%v/%v", h.Min(), h.Max(), h.Sum())
	}
	if got := h.Quantiles(0.5, 0.9); got[0] != 0 || got[1] != 0 {
		t.Errorf("empty Quantiles = %v", got)
	}
	var zero Histogram // the zero value is usable
	zero.Record(time.Millisecond)
	if zero.Count() != 1 || zero.Min() != time.Millisecond {
		t.Errorf("zero-value histogram: count %d min %v", zero.Count(), zero.Min())
	}
}

func TestHistogramSingleValueIsExact(t *testing.T) {
	// With one distinct value every quantile is clamped to it, so the answer
	// is exact even though the bucket midpoint is not.
	for _, d := range []time.Duration{0, 1, 999, time.Microsecond, 12345, 7 * time.Millisecond, 3*time.Second + 17, 9 * time.Minute} {
		h := NewHistogram()
		for range 5 {
			h.Record(d)
		}
		for _, q := range []float64{0, 0.01, 0.5, 0.999, 1} {
			if got := h.Quantile(q); got != d {
				t.Errorf("d=%v Quantile(%v) = %v", d, q, got)
			}
		}
		s := h.Summary()
		wantMs := float64(d) / 1e6
		if s.Min != wantMs || s.Max != wantMs || s.Mean != wantMs || s.P50 != wantMs {
			t.Errorf("d=%v Summary = %+v", d, s)
		}
	}
}

func TestHistogramExtremes(t *testing.T) {
	h := NewHistogram()
	data := []time.Duration{3_141_592, 2_718_281, 1_414_213, 1_732_050, 9_999_999, 5_000_001}
	for _, d := range data {
		h.Record(d)
	}
	lo, hi := slices.Min(data), slices.Max(data)
	tests := []struct {
		name string
		q    float64
		want time.Duration
	}{
		{"q=0 is the exact minimum", 0, lo},
		{"q<0 clamps to the minimum", -3, lo},
		{"NaN is treated as 0", math.NaN(), lo},
		{"q=1 is the exact maximum", 1, hi},
		{"q>1 clamps to the maximum", 7, hi},
	}
	for _, tt := range tests {
		if got := h.Quantile(tt.q); got != tt.want {
			t.Errorf("%s: got %v want %v", tt.name, got, tt.want)
		}
	}
	if h.Min() != lo || h.Max() != hi {
		t.Errorf("Min/Max = %v/%v, want %v/%v", h.Min(), h.Max(), lo, hi)
	}
	// Interior quantiles stay within [min, max] and are monotonic.
	prev := time.Duration(0)
	for q := 0.0; q <= 1.0; q += 0.05 {
		got := h.Quantile(q)
		if got < lo || got > hi {
			t.Errorf("Quantile(%v) = %v outside [%v,%v]", q, got, lo, hi)
		}
		if got < prev {
			t.Errorf("Quantile(%v) = %v decreased from %v", q, got, prev)
		}
		prev = got
	}
}

func TestHistogramNegativeCountsAsZero(t *testing.T) {
	h := NewHistogram()
	h.Record(-5 * time.Second)
	h.Record(time.Millisecond)
	if h.Count() != 2 {
		t.Fatalf("Count = %d", h.Count())
	}
	if h.Min() != 0 {
		t.Errorf("Min = %v, want 0", h.Min())
	}
	if h.Sum() != time.Millisecond {
		t.Errorf("Sum = %v, want 1ms", h.Sum())
	}
	if got := h.Quantile(0); got != 0 {
		t.Errorf("Quantile(0) = %v", got)
	}
}

func TestHistogramRangeClamp(t *testing.T) {
	h := NewHistogram()
	huge := 2 * time.Hour // far above the 18 minute bucket range
	h.Record(0)
	h.Record(500 * time.Nanosecond)
	h.Record(time.Microsecond)
	h.Record(10 * time.Minute)
	h.Record(huge)
	if h.Count() != 5 {
		t.Fatalf("Count = %d", h.Count())
	}
	if h.Max() != huge {
		t.Errorf("Max = %v, want exact %v", h.Max(), huge)
	}
	if h.Min() != 0 {
		t.Errorf("Min = %v", h.Min())
	}
	// 10 minutes is inside the precise range.
	q := h.Quantile(0.8)
	if relErr(float64(q), float64(10*time.Minute)) > 0.01 {
		t.Errorf("Quantile(0.8) = %v, want ~10m", q)
	}
	// The out-of-range sample is clamped to the last bucket for ranking but
	// quantile output stays within [Min, Max].
	if q := h.Quantile(0.99); q < 10*time.Minute || q > huge {
		t.Errorf("Quantile(0.99) = %v", q)
	}
	wantMean := float64(0+500+1000+int64(10*time.Minute)+int64(huge)) / 5 / 1e6
	if got := h.Summary().Mean; math.Abs(got-wantMean) > 1e-6*wantMean {
		t.Errorf("Mean = %v, want %v", got, wantMean)
	}
}

type dist struct {
	name string
	gen  func(r *rand.Rand) int64 // ns
}

var distributions = []dist{
	{"uniform-1us-1s", func(r *rand.Rand) int64 { return 1000 + r.Int64N(1_000_000_000) }},
	{"uniform-1ms-50ms", func(r *rand.Rand) int64 { return 1_000_000 + r.Int64N(49_000_000) }},
	{"lognormal-20ms", func(r *rand.Rand) int64 { return int64(20e6 * math.Exp(0.8*r.NormFloat64())) }},
	{"exponential-5ms", func(r *rand.Rand) int64 { return int64(r.ExpFloat64() * 5e6) }},
	{"bimodal", func(r *rand.Rand) int64 {
		if r.Float64() < 0.9 {
			return int64(2e6 * math.Exp(0.3*r.NormFloat64()))
		}
		return int64(800e6 * math.Exp(0.4*r.NormFloat64()))
	}},
	{"loguniform-1us-10min", func(r *rand.Rand) int64 {
		return int64(math.Exp(math.Log(1e3) + r.Float64()*(math.Log(600e9)-math.Log(1e3))))
	}},
}

func TestHistogramAccuracy(t *testing.T) {
	const n = 200_000
	qs := []float64{0.001, 0.01, 0.1, 0.25, 0.5, 0.75, 0.9, 0.95, 0.99, 0.999, 0.9999}
	for _, d := range distributions {
		t.Run(d.name, func(t *testing.T) {
			r := rand.New(rand.NewPCG(1, 2))
			h := NewHistogram()
			data := make([]int64, n)
			var sum int64
			for i := range data {
				v := max(d.gen(r), 1000) // stay inside the 1µs..10min range
				v = min(v, int64(10*time.Minute))
				data[i] = v
				sum += v
				h.Record(time.Duration(v))
			}
			slices.Sort(data)

			worst := 0.0
			for _, q := range qs {
				want := exactQuantile(data, q)
				got := int64(h.Quantile(q))
				e := relErr(float64(got), float64(want))
				worst = max(worst, e)
				if e > 0.01 {
					t.Errorf("q=%v: got %d want %d (%.3f%% off)", q, got, want, e*100)
				}
			}
			t.Logf("worst relative error: %.4f%%", worst*100)

			s := h.Summary()
			if s.Count != n {
				t.Errorf("Count = %d", s.Count)
			}
			if s.Min != float64(data[0])/1e6 || s.Max != float64(data[n-1])/1e6 {
				t.Errorf("Min/Max = %v/%v, want %v/%v", s.Min, s.Max, float64(data[0])/1e6, float64(data[n-1])/1e6)
			}
			if want := float64(sum) / n / 1e6; math.Abs(s.Mean-want) > 1e-9*want {
				t.Errorf("Mean = %v want %v", s.Mean, want)
			}
			for _, c := range []struct {
				name string
				got  float64
				q    float64
			}{{"p50", s.P50, .5}, {"p90", s.P90, .9}, {"p95", s.P95, .95}, {"p99", s.P99, .99}, {"p999", s.P999, .999}} {
				want := float64(exactQuantile(data, c.q)) / 1e6
				if relErr(c.got, want) > 0.01 {
					t.Errorf("Summary %s = %v want %v", c.name, c.got, want)
				}
			}
			if !(s.Min <= s.P50 && s.P50 <= s.P90 && s.P90 <= s.P95 && s.P95 <= s.P99 && s.P99 <= s.P999 && s.P999 <= s.Max) {
				t.Errorf("summary not monotonic: %+v", s)
			}
		})
	}
}

func TestHistogramQuantilesMatchQuantile(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 7))
	h := NewHistogram()
	for range 10_000 {
		h.Record(time.Duration(1000 + r.Int64N(50_000_000)))
	}
	// Deliberately unsorted, with duplicates and out-of-range values.
	qs := []float64{0.99, 0.5, 1, 0, 0.5, 0.9, -2, 3, math.NaN()}
	got := h.Quantiles(qs...)
	if len(got) != len(qs) {
		t.Fatalf("len = %d", len(got))
	}
	for i, q := range qs {
		if want := h.Quantile(q); got[i] != want {
			t.Errorf("Quantiles[%d] (q=%v) = %v, Quantile = %v", i, q, got[i], want)
		}
	}
	if got := h.Quantiles(); len(got) != 0 {
		t.Errorf("Quantiles() = %v", got)
	}
}

func TestHistogramMerge(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	a, b, all := NewHistogram(), NewHistogram(), NewHistogram()
	for i := range 50_000 {
		d := time.Duration(2000 + r.Int64N(300_000_000))
		all.Record(d)
		if i%3 == 0 {
			a.Record(d)
		} else {
			b.Record(d)
		}
	}
	a.Merge(b)
	assertSameHistogram(t, a, all)

	before := a.Clone()
	a.Merge(nil)
	a.Merge(NewHistogram()) // empty
	assertSameHistogram(t, a, before)

	// Merging into an empty histogram carries min/max across.
	c := NewHistogram()
	c.Merge(all)
	assertSameHistogram(t, c, all)

	// Self-merge doubles every count.
	d := all.Clone()
	d.Merge(d)
	if d.Count() != 2*all.Count() || d.Sum() != 2*all.Sum() || d.Min() != all.Min() || d.Max() != all.Max() {
		t.Errorf("self merge: count %d/%d sum %v/%v", d.Count(), all.Count(), d.Sum(), all.Sum())
	}
}

func assertSameHistogram(t *testing.T, got, want *Histogram) {
	t.Helper()
	if got.Count() != want.Count() || got.Sum() != want.Sum() || got.Min() != want.Min() || got.Max() != want.Max() {
		t.Fatalf("totals differ: count %d/%d sum %v/%v min %v/%v max %v/%v",
			got.Count(), want.Count(), got.Sum(), want.Sum(), got.Min(), want.Min(), got.Max(), want.Max())
	}
	for i := range want.counts {
		if g, w := got.counts[i].Load(), want.counts[i].Load(); g != w {
			t.Fatalf("bucket %d: got %d want %d", i, g, w)
		}
	}
}

func TestHistogramCloneIsIndependent(t *testing.T) {
	h := NewHistogram()
	h.Record(time.Millisecond)
	c := h.Clone()
	h.Record(time.Second)
	if c.Count() != 1 || c.Max() != time.Millisecond {
		t.Errorf("clone changed: count %d max %v", c.Count(), c.Max())
	}
	c.Record(time.Hour)
	if h.Count() != 2 || h.Max() != time.Second {
		t.Errorf("original changed: count %d max %v", h.Count(), h.Max())
	}
}

func TestHistogramReset(t *testing.T) {
	h := NewHistogram()
	for _, d := range []time.Duration{time.Microsecond, time.Millisecond, time.Second} {
		h.Record(d)
	}
	h.Reset()
	assertSameHistogram(t, h, NewHistogram())
	if h.Quantile(0.5) != 0 || h.Summary().Count != 0 {
		t.Errorf("not empty after Reset: %+v", h.Summary())
	}
	// min/max must re-arm: a later, larger value is the new minimum.
	h.Record(5 * time.Millisecond)
	if h.Min() != 5*time.Millisecond || h.Max() != 5*time.Millisecond {
		t.Errorf("after Reset+Record: min %v max %v", h.Min(), h.Max())
	}
}

func TestHistogramConcurrent(t *testing.T) {
	const (
		writers   = 16
		perWriter = 20_000
	)
	h := NewHistogram()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 3 { // readers hammer every read path while writers run
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := h.Summary()
				if s.Count > 0 && (s.Min > s.Max || s.P50 > s.Max+1e-9 || s.P50+1e-9 < s.Min) {
					t.Errorf("inconsistent summary under load: %+v", s)
					return
				}
				_ = h.Quantile(0.99)
				_ = h.Clone()
			}
		}()
	}
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 99))
			for i := range perWriter {
				h.Record(time.Duration(1000 + r.Int64N(10_000_000) + int64(i%7)))
			}
		}()
	}
	wg.Wait()
	close(stop)
	readers.Wait()

	if got := h.Count(); got != writers*perWriter {
		t.Fatalf("Count = %d, want %d", got, writers*perWriter)
	}
	var bucketTotal uint64
	for i := range h.counts {
		bucketTotal += h.counts[i].Load()
	}
	if bucketTotal != writers*perWriter {
		t.Errorf("bucket total %d, want %d", bucketTotal, writers*perWriter)
	}
}

func TestHistogramConcurrentMergeAndReset(t *testing.T) {
	// Pure race-detector coverage: merge, record, clone and reset together.
	a, b := NewHistogram(), NewHistogram()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for i := range 2000 {
				a.Record(time.Duration(i) * time.Microsecond)
			}
		}()
		go func() {
			defer wg.Done()
			for range 200 {
				b.Merge(a)
				_ = a.Clone()
			}
		}()
		go func() {
			defer wg.Done()
			for range 20 {
				b.Reset()
			}
		}()
	}
	wg.Wait()
}

// TestHistogramTransientStates covers the states a reader can see while a
// writer is between its individual atomic updates.
func TestHistogramTransientStates(t *testing.T) {
	t.Run("count published before min", func(t *testing.T) {
		h := NewHistogram()
		h.count.Store(1)
		h.max.Store(5000)
		// minC still 0: no minimum yet. Readers must not report a bogus min.
		if h.Min() != 5000 || h.Max() != 5000 {
			t.Errorf("Min/Max = %v/%v", h.Min(), h.Max())
		}
		s := h.Summary()
		if s.Min > s.Max || s.P50 < s.Min || s.P50 > s.Max {
			t.Errorf("summary = %+v", s)
		}
	})
	t.Run("buckets lag the count", func(t *testing.T) {
		h := NewHistogram()
		h.Record(2 * time.Millisecond)
		h.count.Store(10) // nine more are counted but not yet in a bucket
		if got := h.Quantile(0.99); got != 2*time.Millisecond {
			t.Errorf("Quantile = %v, want the last bucket's value", got)
		}
		walkOut := make([]int64, 2)
		walkDense(&h.counts, 10, []float64{0.5, 0.99}, walkOut)
		if walkOut[0] != walkOut[1] || walkOut[0] == 0 {
			t.Errorf("walkDense = %v", walkOut)
		}
	})
	t.Run("sparse total exceeds buckets", func(t *testing.T) {
		var s sparseCounts
		s.add(bucketIndex(3_000_000), 3)
		s.total = 10
		out := make([]int64, 3)
		s.walk([]float64{0.1, 0.5, 0.99}, out)
		for _, v := range out {
			if v == 0 || relErr(float64(v), 3e6) > 0.01 {
				t.Errorf("walk = %v", out)
			}
		}
	})
	t.Run("many quantiles", func(t *testing.T) {
		// More than the stack buffer's eight ranks.
		qs := []float64{.1, .2, .3, .4, .5, .6, .7, .8, .9, .99}
		h := NewHistogram()
		var s sparseCounts
		for i := 1; i <= 100; i++ {
			h.Record(time.Duration(i) * time.Millisecond)
			s.add(bucketIndex(uint64(i)*1_000_000), 1)
		}
		out := make([]int64, len(qs))
		h.valuesFor(qs, out)
		out2 := make([]int64, len(qs))
		s.walk(qs, out2)
		for i, q := range qs {
			want := q * 100 * 1e6
			if relErr(float64(out[i]), want) > 0.01 || relErr(float64(out2[i]), want) > 0.01 {
				t.Errorf("q=%v dense %d sparse %d want ~%v", q, out[i], out2[i], want)
			}
		}
	})
}

func BenchmarkHistogramRecord(b *testing.B) {
	h := NewHistogram()
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		h.Record(time.Duration(1_000_000 + i&0xffff))
	}
}

func BenchmarkHistogramRecordParallel(b *testing.B) {
	h := NewHistogram()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			h.Record(time.Duration(1_000_000 + i&0xffff))
			i++
		}
	})
}

func BenchmarkHistogramQuantile(b *testing.B) {
	h := populatedHistogram()
	b.ReportAllocs()
	for b.Loop() {
		_ = h.Quantile(0.99)
	}
}

func BenchmarkHistogramSummary(b *testing.B) {
	h := populatedHistogram()
	b.ReportAllocs()
	for b.Loop() {
		_ = h.Summary()
	}
}

func populatedHistogram() *Histogram {
	r := rand.New(rand.NewPCG(1, 1))
	h := NewHistogram()
	for range 100_000 {
		h.Record(time.Duration(float64(20*time.Millisecond) * math.Exp(0.8*r.NormFloat64())))
	}
	return h
}
