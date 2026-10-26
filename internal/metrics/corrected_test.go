package metrics

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/justinburger/mirage/internal/model"
)

func newReplayCollector() *Collector {
	return NewCollector("replay:rpl_test", model.KindReplay, "rpl_test")
}

// correctedObs is a replay observation: service time d, latency from the
// intended send time corrected.
func correctedObs(d, corrected time.Duration) Observation {
	return Observation{Endpoint: "GET /x", Status: 200, Duration: d, BytesIn: 1, BytesOut: 2, Corrected: corrected}
}

func TestCorrectedLatencyQuantiles(t *testing.T) {
	repeat := func(n int, o Observation) []Observation {
		out := make([]Observation, n)
		for i := range out {
			out[i] = o
		}
		return out
	}
	tests := []struct {
		name string
		obs  []Observation
		// Expected corrected summary (ms); Min, Max and Mean are exact.
		count                          int64
		min, max, mean                 float64
		p50, p90, p95, p99, p999       float64
		serviceMax, serviceP999, total float64 // service-time side, to prove it is untouched
	}{
		{
			name: "uniform ramp over a constant service time",
			obs: func() []Observation {
				var o []Observation
				for i := 1; i <= 100; i++ {
					o = append(o, correctedObs(time.Millisecond, time.Duration(i)*time.Millisecond))
				}
				return o
			}(),
			count: 100, min: 1, max: 100, mean: 50.5,
			p50: 50, p90: 90, p95: 95, p99: 99, p999: 100,
			serviceMax: 1, serviceP999: 1, total: 100,
		},
		{
			name: "queueing tail hidden from service time",
			obs: append(repeat(990, correctedObs(10*time.Millisecond, 10*time.Millisecond)),
				repeat(10, correctedObs(10*time.Millisecond, 2*time.Second))...),
			count: 1000, min: 10, max: 2000, mean: (990*10 + 10*2000) / 1000.0,
			p50: 10, p90: 10, p95: 10, p99: 10, p999: 2000,
			serviceMax: 10, serviceP999: 10, total: 1000,
		},
		{
			name:  "corrected equals service time",
			obs:   repeat(50, correctedObs(30*time.Millisecond, 30*time.Millisecond)),
			count: 50, min: 30, max: 30, mean: 30,
			p50: 30, p90: 30, p95: 30, p99: 30, p999: 30,
			serviceMax: 30, serviceP999: 30, total: 50,
		},
		{
			name: "only observations with Corrected set are counted",
			obs: append(repeat(6, correctedObs(5*time.Millisecond, 0)),
				repeat(4, correctedObs(5*time.Millisecond, 40*time.Millisecond))...),
			count: 4, min: 40, max: 40, mean: 40,
			p50: 40, p90: 40, p95: 40, p99: 40, p999: 40,
			serviceMax: 5, serviceP999: 5, total: 10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newReplayCollector()
			for _, o := range tt.obs {
				c.Observe(o)
			}
			h := c.CorrectedLatency()
			if h == nil {
				t.Fatal("CorrectedLatency() = nil, want a histogram")
			}
			got := h.Summary()
			if got.Count != tt.count {
				t.Errorf("Count = %d, want %d", got.Count, tt.count)
			}
			if got.Min != tt.min || got.Max != tt.max {
				t.Errorf("Min/Max = %v/%v, want exactly %v/%v", got.Min, got.Max, tt.min, tt.max)
			}
			approx(t, "Mean", got.Mean, tt.mean, 1e-9)
			for _, q := range []struct {
				name      string
				got, want float64
			}{
				{"P50", got.P50, tt.p50}, {"P90", got.P90, tt.p90}, {"P95", got.P95, tt.p95},
				{"P99", got.P99, tt.p99}, {"P999", got.P999, tt.p999},
			} {
				approx(t, q.name, q.got, q.want, 0.01)
			}

			// The same numbers reach the snapshot.
			s := c.Snapshot()
			if s.CorrectedLatency == nil || *s.CorrectedLatency != got {
				t.Errorf("Snapshot.CorrectedLatency = %+v, want %+v", s.CorrectedLatency, got)
			}
			// The service-time side is exactly what it was without Corrected.
			if s.Requests != int64(tt.total) || s.Latency.Count != int64(tt.total) {
				t.Errorf("requests = %d (latency count %d), want %v", s.Requests, s.Latency.Count, tt.total)
			}
			if s.Latency.Max != tt.serviceMax {
				t.Errorf("service Max = %v, want %v", s.Latency.Max, tt.serviceMax)
			}
			approx(t, "service P999", s.Latency.P999, tt.serviceP999, 0.01)
		})
	}
}

func TestCorrectedNotMeasuredValues(t *testing.T) {
	for _, corrected := range []time.Duration{0, -1, -time.Second, math.MinInt64} {
		t.Run(corrected.String(), func(t *testing.T) {
			c := newReplayCollector()
			observe := correctedObs(time.Millisecond, corrected)
			for range 10 {
				c.Observe(observe)
			}
			if c.corr.Load() != nil {
				t.Error("corrected state allocated for an observation that did not measure it")
			}
			if h := c.CorrectedLatency(); h != nil {
				t.Errorf("CorrectedLatency() = %d observations, want nil", h.Count())
			}
			if s := c.Snapshot(); s.CorrectedLatency != nil || s.Requests != 10 {
				t.Errorf("snapshot = %d requests, corrected %+v", s.Requests, s.CorrectedLatency)
			}
		})
	}
}

func TestCorrectedLatencyReturnsCopy(t *testing.T) {
	c := newReplayCollector()
	c.Observe(correctedObs(time.Millisecond, 20*time.Millisecond))
	h := c.CorrectedLatency()
	h.Record(time.Hour)
	h.Reset()
	if got := c.CorrectedLatency(); got == nil || got.Count() != 1 || got.Max() != 20*time.Millisecond {
		t.Fatalf("mutating the copy changed the collector: %v", got)
	}
	// And it is a snapshot, not a view.
	before := c.CorrectedLatency()
	c.Observe(correctedObs(time.Millisecond, 30*time.Millisecond))
	if before.Count() != 1 {
		t.Errorf("earlier copy now has %d observations, want 1", before.Count())
	}
}

func TestCorrectedLazyAllocation(t *testing.T) {
	t.Run("services never allocate it", func(t *testing.T) {
		r := NewRegistry()
		svc := r.Collector("service:svc_x", model.KindService, "svc_x")
		for i := range 2000 {
			svc.Observe(Observation{Endpoint: "GET /a", Status: 200, Duration: time.Duration(i+1) * time.Microsecond, BytesOut: 5})
		}
		svc.Tick(base)
		svc.Tick(base.Add(time.Second))
		_ = svc.Snapshot()
		_ = svc.Series(0)
		_ = svc.Endpoints()
		var sb strings.Builder
		if err := r.WritePrometheus(&sb); err != nil {
			t.Fatal(err)
		}
		svc.Reset()
		if svc.corr.Load() != nil {
			t.Fatal("service collector allocated corrected state")
		}
		if svc.CorrectedLatency() != nil {
			t.Error("CorrectedLatency() non-nil for a service")
		}
		o := Observation{Endpoint: "GET /a", Status: 200, Duration: 3 * time.Millisecond, BytesIn: 10, BytesOut: 200}
		svc.Observe(o)
		if allocs := testing.AllocsPerRun(1000, func() { svc.Observe(o) }); allocs != 0 {
			t.Errorf("service Observe allocates %v times per call, want 0", allocs)
		}
		if svc.corr.Load() != nil {
			t.Error("corrected state appeared")
		}
	})

	t.Run("allocated once by the first measured observation", func(t *testing.T) {
		c := newReplayCollector()
		o := correctedObs(time.Millisecond, 5*time.Millisecond)
		plain := o
		plain.Corrected = 0
		for range 100 {
			c.Observe(plain)
		}
		if c.corr.Load() != nil {
			t.Fatal("state exists before any measured observation")
		}
		if allocs := testing.AllocsPerRun(1000, func() { c.Observe(plain) }); allocs != 0 {
			t.Errorf("unmeasured Observe on a replay collector allocates %v times, want 0", allocs)
		}

		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		c.Observe(o)
		runtime.ReadMemStats(&m1)
		first := c.corr.Load()
		if first == nil {
			t.Fatal("no state after a measured observation")
		}
		if got, min := m1.TotalAlloc-m0.TotalAlloc, uint64(unsafe.Sizeof(correctedState{})); got < min {
			t.Errorf("first measured Observe allocated %d bytes, want at least the %d byte state", got, min)
		}

		// Steady state: zero allocations, and the state is never replaced.
		if allocs := testing.AllocsPerRun(1000, func() { c.Observe(o) }); allocs != 0 {
			t.Errorf("measured Observe allocates %v times per call, want 0", allocs)
		}
		c.Tick(base)
		c.Reset()
		c.Observe(o)
		if c.corr.Load() != first {
			t.Error("corrected state replaced; Reset must clear it in place")
		}
	})

	t.Run("concurrent first observations share one state", func(t *testing.T) {
		const workers, each = 16, 200
		c := newReplayCollector()
		var (
			wg    sync.WaitGroup
			start = make(chan struct{})
		)
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for range each {
					c.Observe(correctedObs(time.Millisecond, 7*time.Millisecond))
				}
			}()
		}
		close(start)
		wg.Wait()
		h := c.CorrectedLatency()
		if h == nil || h.Count() != workers*each {
			t.Errorf("corrected count = %v, want %d (an observation was lost to a discarded state)", h, workers*each)
		}
	})
}

// TestCollectorHotAtomicsShareALine guards the memory layout that keeps
// parallel Observe fast: count and sum of a histogram are written together by
// every record, so they must sit in one 64-byte line (large objects such as a
// Collector or correctedState are page-aligned, so offsets decide). Putting a
// field in front of Collector.latency once split them and cost about 15% on
// the parallel Observe benchmarks.
func TestCollectorHotAtomicsShareALine(t *testing.T) {
	var (
		c  Collector
		cs correctedState
	)
	tests := []struct {
		name         string
		hist         uintptr // offset of the Histogram in its parent
		count, sum   uintptr // offsets within Histogram
		parentIsPage bool
	}{
		{"Collector.latency", unsafe.Offsetof(c.latency), unsafe.Offsetof(c.latency.count), unsafe.Offsetof(c.latency.sum), unsafe.Sizeof(c) > 32<<10},
		{"correctedState.latency", unsafe.Offsetof(cs.latency), unsafe.Offsetof(cs.latency.count), unsafe.Offsetof(cs.latency.sum), unsafe.Sizeof(cs) > 32<<10},
	}
	for _, tt := range tests {
		if !tt.parentIsPage {
			t.Fatalf("%s: parent is no longer a large (page-aligned) object; revisit this test", tt.name)
		}
		if a, b := tt.hist+tt.count, tt.hist+tt.sum; a/64 != b/64 {
			t.Errorf("%s: count at offset %d and sum at %d are in different 64-byte lines", tt.name, a, b)
		}
	}
}

func TestTickCorrectedSeriesFields(t *testing.T) {
	type window struct {
		name string
		obs  []Observation
		// Expected corrected p95/p99 of the closed window, ms; 0 = omitted.
		p95, p99 float64
		// Expected service-time P95 of the window, ms, proving the main
		// window is unaffected by Corrected.
		serviceP95 float64
	}
	var ramp []Observation
	for i := 1; i <= 10; i++ {
		ramp = append(ramp, correctedObs(time.Millisecond, time.Duration(i)*10*time.Millisecond))
	}
	three := func(corrected time.Duration) []Observation {
		return []Observation{
			correctedObs(time.Millisecond, corrected), correctedObs(time.Millisecond, corrected), correctedObs(time.Millisecond, corrected),
		}
	}
	windows := []window{
		{name: "ramp of ten", obs: ramp, p95: 100, p99: 100, serviceP95: 1},
		{name: "unmeasured traffic only", obs: three(0), p95: 0, p99: 0, serviceP95: 1},
		{name: "single observation is exact", obs: []Observation{correctedObs(2*time.Millisecond, 250*time.Millisecond)}, p95: 250, p99: 250, serviceP95: 2},
		{name: "empty window", obs: nil, p95: 0, p99: 0, serviceP95: 0},
		{name: "own values, not cumulative", obs: three(5 * time.Millisecond), p95: 5, p99: 5, serviceP95: 1},
		{
			name: "p95 and p99 differ",
			obs: func() []Observation {
				o := make([]Observation, 0, 200)
				for range 190 {
					o = append(o, correctedObs(time.Millisecond, 10*time.Millisecond))
				}
				for range 9 {
					o = append(o, correctedObs(time.Millisecond, 100*time.Millisecond))
				}
				return append(o, correctedObs(time.Millisecond, 900*time.Millisecond))
			}(),
			// 200 observations: rank 190 (p95) is 10ms, rank 198 (p99) is 100ms.
			p95: 10, p99: 100, serviceP95: 1,
		},
	}

	c := newReplayCollector()
	var points []model.SeriesPoint
	for i, w := range windows {
		for _, o := range w.obs {
			c.Observe(o)
		}
		p := c.Tick(base.Add(time.Duration(i) * time.Second))
		points = append(points, p)
		approx(t, w.name+": CorrectedP95", p.CorrectedP95, w.p95, 0.01)
		approx(t, w.name+": CorrectedP99", p.CorrectedP99, w.p99, 0.01)
		approx(t, w.name+": P95", p.P95, w.serviceP95, 0.01)
		if int(p.Requests) != len(w.obs) {
			t.Errorf("%s: Requests = %d, want %d", w.name, p.Requests, len(w.obs))
		}
	}
	if got := c.Series(0); len(got) != len(points) {
		t.Fatalf("Series has %d points, want %d", len(got), len(points))
	} else {
		for i := range got {
			if got[i] != points[i] {
				t.Errorf("Series[%d] = %+v, Tick returned %+v", i, got[i], points[i])
			}
		}
	}

	// Skipped seconds are zero points without corrected data; the window after
	// them carries only its own.
	c.Observe(correctedObs(time.Millisecond, 60*time.Millisecond))
	last := c.Tick(base.Add(time.Duration(len(windows)+3) * time.Second))
	approx(t, "after a gap: CorrectedP95", last.CorrectedP95, 60, 0.01)
	series := c.Series(0)
	for _, gap := range series[len(series)-4 : len(series)-1] {
		if gap.CorrectedP95 != 0 || gap.CorrectedP99 != 0 {
			t.Errorf("gap-filled point T=%d has corrected data: %+v", gap.T, gap)
		}
	}
}

func TestTickCorrectedNeverSetOnServices(t *testing.T) {
	c := newTestCollector()
	for i := range 3 {
		observe(c, 20, "GET /a", 200, 15*time.Millisecond)
		if p := c.Tick(base.Add(time.Duration(i) * time.Second)); p.CorrectedP95 != 0 || p.CorrectedP99 != 0 || p.P95 == 0 {
			t.Errorf("point = %+v", p)
		}
	}
	raw, err := json.Marshal(c.Series(0))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "corrected") {
		t.Errorf("service series JSON mentions corrected data: %s", raw)
	}
}

func TestTickCorrectedCarriesExtremesOfInFlightObservation(t *testing.T) {
	c := newReplayCollector()
	const d = 7 * time.Millisecond
	cs := c.correctedState()
	// Observe has published the extremes but not yet the bucket and count.
	cs.winMax.Store(int64(d))
	cs.winMinC.Store(math.MaxInt64 - int64(d))

	if p := c.Tick(base); p.CorrectedP95 != 0 || p.CorrectedP99 != 0 {
		t.Fatalf("window without a corrected observation has %+v", p)
	}
	cs.latency.observe(int64(d)) // the rest of that Observe lands
	p := c.Tick(base.Add(time.Second))
	if p.CorrectedP95 != 7 || p.CorrectedP99 != 7 {
		t.Fatalf("window = %+v, want the carried extremes to make p95 and p99 exactly 7ms", p)
	}
	if p := c.Tick(base.Add(2 * time.Second)); p.CorrectedP95 != 0 || p.CorrectedP99 != 0 {
		t.Errorf("extremes leaked into the following window: %+v", p)
	}
}

func TestSnapshotCorrectedLatency(t *testing.T) {
	tests := []struct {
		name  string
		setup func(c *Collector)
		want  *model.LatencySummary // nil: not measured
	}{
		{name: "service", setup: func(c *Collector) { observe(c, 5, "GET /a", 200, time.Millisecond) }},
		{name: "idle replay", setup: func(*Collector) {}},
		{name: "replay without corrected", setup: func(c *Collector) { observe(c, 5, "GET /a", 200, time.Millisecond) }},
		{
			name: "replay with corrected",
			setup: func(c *Collector) {
				for _, ms := range []time.Duration{10, 20, 30, 40} {
					c.Observe(correctedObs(time.Millisecond, ms*time.Millisecond))
				}
			},
			want: &model.LatencySummary{Count: 4, Min: 10, Mean: 25, P50: 20, P90: 40, P95: 40, P99: 40, P999: 40, Max: 40},
		},
		{
			name: "measured then reset",
			setup: func(c *Collector) {
				c.Observe(correctedObs(time.Millisecond, 10*time.Millisecond))
				c.Reset()
			},
		},
		{
			name: "measured, reset, measured again",
			setup: func(c *Collector) {
				c.Observe(correctedObs(time.Millisecond, 10*time.Millisecond))
				c.Reset()
				c.Observe(correctedObs(time.Millisecond, 3*time.Millisecond))
			},
			want: &model.LatencySummary{Count: 1, Min: 3, Mean: 3, P50: 3, P90: 3, P95: 3, P99: 3, P999: 3, Max: 3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newReplayCollector()
			if tt.name == "service" {
				c = newTestCollector()
			}
			tt.setup(c)
			s := c.Snapshot()
			js, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			hasKey := strings.Contains(string(js), `"corrected_latency"`)
			h := c.CorrectedLatency()

			if tt.want == nil {
				if s.CorrectedLatency != nil || hasKey || h != nil {
					t.Errorf("not measured, but snapshot has %+v (json key %v), histogram %v", s.CorrectedLatency, hasKey, h)
				}
				return
			}
			if s.CorrectedLatency == nil || !hasKey || h == nil {
				t.Fatalf("measured, but snapshot has %+v (json key %v), histogram %v", s.CorrectedLatency, hasKey, h)
			}
			g, w := *s.CorrectedLatency, *tt.want
			if g.Count != w.Count || g.Min != w.Min || g.Max != w.Max {
				t.Errorf("Count/Min/Max = %d/%v/%v, want %d/%v/%v", g.Count, g.Min, g.Max, w.Count, w.Min, w.Max)
			}
			approx(t, "Mean", g.Mean, w.Mean, 1e-9)
			approx(t, "P50", g.P50, w.P50, 0.01)
			approx(t, "P90", g.P90, w.P90, 0.01)
			approx(t, "P95", g.P95, w.P95, 0.01)
			approx(t, "P99", g.P99, w.P99, 0.01)
			approx(t, "P999", g.P999, w.P999, 0.01)
			if h.Summary() != g {
				t.Errorf("CorrectedLatency().Summary() = %+v, snapshot has %+v", h.Summary(), g)
			}
		})
	}
}

func TestResetClearsCorrectedWindows(t *testing.T) {
	for _, tickAfterReset := range []bool{true, false} {
		t.Run(fmt.Sprintf("tick after reset=%v", tickAfterReset), func(t *testing.T) {
			c := newReplayCollector()
			for range 500 {
				c.Observe(correctedObs(time.Millisecond, 800*time.Millisecond))
			}
			if p := c.Tick(base); p.CorrectedP99 != 800 {
				t.Fatalf("setup: CorrectedP99 = %v", p.CorrectedP99)
			}
			c.Observe(correctedObs(time.Millisecond, 900*time.Millisecond)) // left unticked
			c.Reset()
			now := base
			if tickAfterReset {
				now = now.Add(time.Second)
				if p := c.Tick(now); p.CorrectedP95 != 0 || p.CorrectedP99 != 0 {
					t.Errorf("point after Reset has stale corrected data: %+v", p)
				}
			}

			// Fewer observations than before Reset, in the very same bucket,
			// must still show up: the copy of the buckets from the previous
			// Tick was cleared too.
			for range 3 {
				c.Observe(correctedObs(time.Millisecond, 800*time.Millisecond))
			}
			now = now.Add(time.Second)
			if p := c.Tick(now); p.CorrectedP95 != 800 || p.CorrectedP99 != 800 {
				t.Errorf("point after re-measuring a pre-Reset bucket = %+v, want exactly 800ms", p)
			}
			c.Observe(correctedObs(time.Millisecond, 5*time.Millisecond))
			now = now.Add(time.Second)
			if p := c.Tick(now); p.CorrectedP95 != 5 || p.CorrectedP99 != 5 {
				t.Errorf("point after re-measuring = %+v, want exactly 5ms (no leftovers from before Reset)", p)
			}
			if h := c.CorrectedLatency(); h == nil || h.Count() != 4 {
				t.Errorf("CorrectedLatency after Reset = %v, want 4 observations", h)
			}
		})
	}
}

// TestCorrectedObserveRacingTick hammers measured Observes while another
// goroutine ticks; every corrected observation must land in exactly one window
// and no window may report values outside what was observed.
func TestCorrectedObserveRacingTick(t *testing.T) {
	const (
		writers   = 8
		perWriter = 30_000
		lo, hi    = 40 * time.Millisecond, 60 * time.Millisecond
	)
	c := newReplayCollector()
	var (
		wg      sync.WaitGroup
		done    atomic.Bool
		windows int
		badMu   sync.Mutex
		bad     []model.SeriesPoint
	)
	check := func(p model.SeriesPoint) {
		windows++
		okVal := func(v float64) bool { return v == 0 || (v >= 40 && v <= 60) }
		if !okVal(p.CorrectedP95) || !okVal(p.CorrectedP99) || p.CorrectedP95 > p.CorrectedP99 {
			badMu.Lock()
			bad = append(bad, p)
			badMu.Unlock()
		}
	}
	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		for i := 0; !done.Load(); i++ {
			check(c.Tick(base.Add(time.Duration(i) * time.Second)))
		}
	}()
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 11))
			for range perWriter {
				corrected := lo + time.Duration(r.Int64N(int64(hi-lo)+1))
				c.Observe(correctedObs(time.Millisecond, corrected))
			}
		}()
	}
	wg.Wait()
	done.Store(true)
	<-tickerDone
	check(c.Tick(base.Add(time.Hour)))

	if len(bad) > 0 {
		t.Fatalf("%d of %d windows outside [40ms, 60ms] or with p95 > p99, e.g. %+v", len(bad), windows, bad[0])
	}
	const total = writers * perWriter
	if h := c.CorrectedLatency(); h == nil || h.Count() != total {
		t.Fatalf("corrected count = %v, want %d", h, total)
	}
	// Every bucket was carried into some window exactly once.
	cs := c.corr.Load()
	for i := range cs.latency.counts {
		if got, want := cs.prev[i], cs.latency.counts[i].Load(); got != want {
			t.Fatalf("bucket %d: windows saw %d, histogram has %d", i, got, want)
		}
	}
	t.Logf("%d windows closed while %d corrected observations raced", windows, total)
}

// TestCorrectedStress runs every operation that touches corrected data
// concurrently, Reset included, for the race detector and for panics.
func TestCorrectedStress(t *testing.T) {
	r := NewRegistry()
	c := r.Collector(ReplayCollectorName("rpl_stress"), model.KindReplay, "rpl_stress")
	var wg sync.WaitGroup
	var stop atomic.Bool
	run := func(f func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; !stop.Load(); i++ {
				f(i)
			}
		}()
	}
	for g := range 4 {
		run(func(i int) {
			d := time.Duration(i%1000+1) * time.Microsecond
			o := Observation{Endpoint: fmt.Sprintf("GET /e/%d", (i+g)%7), Status: 200, Duration: d}
			if i%3 != 0 {
				o.Corrected = d * time.Duration(1+i%5)
			}
			c.Observe(o)
		})
	}
	var tick atomic.Int64
	run(func(int) { c.Tick(base.Add(time.Duration(tick.Add(1)) * time.Second)) })
	run(func(int) { _ = c.Snapshot() })
	run(func(int) { _ = c.CorrectedLatency(); _ = c.Series(0) })
	run(func(int) { var sb strings.Builder; _ = r.WritePrometheus(&sb) })
	run(func(i int) {
		if i%50 == 0 {
			c.Reset()
		}
		time.Sleep(50 * time.Microsecond)
	})
	time.Sleep(150 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
}

func TestWritePrometheusCorrected(t *testing.T) {
	const family = "mirage_corrected_request_duration_seconds"
	quantiles := []string{"0.5", "0.9", "0.95", "0.99"}

	measure := func(r *Registry, id string, corrected ...time.Duration) *Collector {
		c := r.Collector(ReplayCollectorName(id), model.KindReplay, id)
		for _, d := range corrected {
			c.Observe(correctedObs(10*time.Millisecond, d))
		}
		return c
	}
	tests := []struct {
		name  string
		build func(r *Registry)
		// measured maps the collector ids that must have the corrected
		// summary to their observation count and sum (seconds).
		measured map[string][2]float64
	}{
		{
			name: "services only",
			build: func(r *Registry) {
				r.Collector("service:svc_a", model.KindService, "svc_a").Observe(Observation{Endpoint: "GET /a", Status: 200, Duration: time.Millisecond})
			},
		},
		{
			name: "replay without corrected data",
			build: func(r *Registry) {
				measure(r, "rpl_plain").Observe(Observation{Endpoint: "GET /a", Status: 200, Duration: time.Millisecond})
			},
		},
		{
			name: "one measured replay among others",
			build: func(r *Registry) {
				r.Collector("service:svc_a", model.KindService, "svc_a").Observe(Observation{Endpoint: "GET /a", Status: 200, Duration: time.Millisecond})
				measure(r, "rpl_plain").Observe(Observation{Endpoint: "GET /a", Status: 200, Duration: time.Millisecond})
				measure(r, "rpl_a", 250*time.Millisecond, 250*time.Millisecond)
			},
			measured: map[string][2]float64{"rpl_a": {2, 0.5}},
		},
		{
			name: "two measured replays",
			build: func(r *Registry) {
				measure(r, "rpl_a", 100*time.Millisecond)
				measure(r, "rpl_b", 2*time.Second, 4*time.Second)
			},
			measured: map[string][2]float64{"rpl_a": {1, 0.1}, "rpl_b": {2, 6}},
		},
		{
			name: "measured then reset",
			build: func(r *Registry) {
				measure(r, "rpl_a", 100*time.Millisecond).Reset()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistry()
			tt.build(r)
			out := renderProm(t, r)
			fams, samples := parseProm(t, out)

			f := fams[family]
			if len(tt.measured) == 0 {
				if f != nil || strings.Contains(out, "corrected") {
					t.Fatalf("family present although nothing was measured:\n%s", out)
				}
				return
			}
			if f == nil {
				t.Fatalf("family missing:\n%s", out)
			}
			if f.typ != "summary" || f.help == "" {
				t.Errorf("family = %+v, want a summary with help text", f)
			}
			if want := len(tt.measured) * (len(quantiles) + 2); f.samples != want {
				t.Errorf("%d samples, want %d (quantiles, _sum and _count for %d collectors)", f.samples, want, len(tt.measured))
			}
			for id, w := range tt.measured {
				lbl := map[string]string{"kind": "replay", "id": id}
				for _, q := range quantiles {
					if v := find(t, samples, family, map[string]string{"kind": "replay", "id": id, "quantile": q}); v <= 0 {
						t.Errorf("%s quantile %s = %v, want a positive value", id, q, v)
					}
				}
				if got := find(t, samples, family+"_count", lbl); got != w[0] {
					t.Errorf("%s count = %v, want %v", id, got, w[0])
				}
				if got := find(t, samples, family+"_sum", lbl); math.Abs(got-w[1]) > 1e-9 {
					t.Errorf("%s sum = %v, want %v", id, got, w[1])
				}
			}
			for _, s := range samples {
				if strings.HasPrefix(s.name, family) {
					if _, ok := tt.measured[s.labels["id"]]; !ok || s.labels["kind"] != "replay" {
						t.Errorf("unexpected corrected sample %s%v", s.name, s.labels)
					}
				}
			}
			// The families a service would have are still all there, and the
			// corrected one sits right after the service-time summary.
			if fams["mirage_request_duration_seconds"] == nil {
				t.Error("service-time summary missing")
			}
			iDur := strings.Index(out, "# HELP mirage_request_duration_seconds ")
			iCor := strings.Index(out, "# HELP "+family+" ")
			iFlight := strings.Index(out, "# HELP mirage_in_flight_requests ")
			if !(iDur >= 0 && iDur < iCor && iCor < iFlight) {
				t.Errorf("family order wrong: duration %d, corrected %d, in-flight %d", iDur, iCor, iFlight)
			}
		})
	}
}

func TestWritePrometheusCorrectedGolden(t *testing.T) {
	r := NewRegistry()
	c := r.Collector("replay:rpl_1", model.KindReplay, "rpl_1")
	c.Observe(correctedObs(10*time.Millisecond, 250*time.Millisecond))

	const want = `# HELP mirage_corrected_request_duration_seconds Request latency in seconds from the intended send time (coordinated-omission corrected).
# TYPE mirage_corrected_request_duration_seconds summary
mirage_corrected_request_duration_seconds{kind="replay",id="rpl_1",quantile="0.5"} 0.25
mirage_corrected_request_duration_seconds{kind="replay",id="rpl_1",quantile="0.9"} 0.25
mirage_corrected_request_duration_seconds{kind="replay",id="rpl_1",quantile="0.95"} 0.25
mirage_corrected_request_duration_seconds{kind="replay",id="rpl_1",quantile="0.99"} 0.25
mirage_corrected_request_duration_seconds_sum{kind="replay",id="rpl_1"} 0.25
mirage_corrected_request_duration_seconds_count{kind="replay",id="rpl_1"} 1
`
	out := renderProm(t, r)
	start := strings.Index(out, "# HELP mirage_corrected_request_duration_seconds ")
	end := strings.Index(out, "# HELP mirage_in_flight_requests ")
	if start < 0 || end < start {
		t.Fatalf("corrected family not found:\n%s", out)
	}
	if got := out[start:end]; got != want {
		t.Errorf("corrected family differs from golden.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	// The service-time family next to it is the one the replay already had.
	if !strings.Contains(out, `mirage_request_duration_seconds_count{kind="replay",id="rpl_1"} 1`) {
		t.Errorf("service-time count missing:\n%s", out)
	}
}

// TestWritePrometheusServicesUnchangedByCorrected pins the output of a
// registry without measured collectors: identical to what it was before the
// corrected family existed.
func TestWritePrometheusServicesUnchangedByCorrected(t *testing.T) {
	r := NewRegistry()
	r.Collector("service:svc_1", model.KindService, "svc_1").Observe(Observation{Endpoint: "GET /a", Status: 200, Duration: time.Millisecond})
	r.Collector("replay:rpl_1", model.KindReplay, "rpl_1").Observe(Observation{Endpoint: "GET /a", Status: 200, Duration: time.Millisecond})
	fams, _ := parseProm(t, renderProm(t, r))
	if len(fams) != 10 {
		names := make([]string, 0, len(fams))
		for n := range fams {
			names = append(names, n)
		}
		t.Errorf("%d families %v, want the 10 that existed before", len(fams), names)
	}
}

func BenchmarkCollectorObserveCorrected(b *testing.B) {
	c := newReplayCollector()
	names := benchEndpoints(8)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		d := time.Duration(1_000_000 + i&0xfffff)
		c.Observe(Observation{Endpoint: names[i&7], Status: 200, Duration: d, BytesIn: 120, BytesOut: 2048, Corrected: d + 5_000_000})
	}
}

func BenchmarkCollectorObserveCorrectedParallel(b *testing.B) {
	c := newReplayCollector()
	names := benchEndpoints(8)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			d := time.Duration(1_000_000 + i&0xfffff)
			c.Observe(Observation{Endpoint: names[i&7], Status: 200, Duration: d, BytesIn: 120, BytesOut: 2048, Error: i%50 == 0, Corrected: d + 5_000_000})
			i++
		}
	})
}

func BenchmarkCollectorTickCorrected(b *testing.B) {
	c := populatedCollector(20)
	names := benchEndpoints(20)
	c.Observe(correctedObs(time.Millisecond, 2*time.Millisecond)) // allocate the state up front
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		for j := range 50 {
			d := time.Duration(1_000_000 + j*37_000)
			c.Observe(Observation{Endpoint: names[j%20], Status: 200, Duration: d, Corrected: d + time.Duration(j)*100_000})
		}
		c.Tick(base.Add(time.Duration(100+i) * time.Second))
	}
}
