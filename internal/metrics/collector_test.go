package metrics

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/justinburger/mirage/internal/model"
)

// base is a whole-second instant used as the fake clock in tests.
var base = time.Unix(1_700_000_000, 0)

func newTestCollector() *Collector {
	return NewCollector("service:svc_test", model.KindService, "svc_test")
}

// observe records n identical requests.
func observe(c *Collector, n int, endpoint string, status int, d time.Duration) {
	for range n {
		c.Observe(Observation{
			Endpoint: endpoint, Status: status, Duration: d,
			BytesIn: 10, BytesOut: 100, Error: model.IsErrorStatus(status),
		})
	}
}

func approx(t *testing.T, name string, got, want, relTol float64) {
	t.Helper()
	if relErr(got, want) > relTol {
		t.Errorf("%s = %v, want %v (±%.2f%%)", name, got, want, relTol*100)
	}
}

func TestStatusClass(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{0, "err"}, {-1, "err"}, {99, "err"}, {600, "err"}, {1000, "err"},
		{100, "1xx"}, {199, "1xx"},
		{200, "2xx"}, {204, "2xx"}, {299, "2xx"},
		{301, "3xx"}, {399, "3xx"},
		{404, "4xx"}, {429, "4xx"},
		{500, "5xx"}, {503, "5xx"}, {599, "5xx"},
	}
	for _, tt := range tests {
		if got := classNames[statusClass(tt.status)]; got != tt.want {
			t.Errorf("statusClass(%d) = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestCollectorIdentity(t *testing.T) {
	c := NewCollector("replay:rpl_1", model.KindReplay, "rpl_1")
	if c.Name() != "replay:rpl_1" || c.Kind() != model.KindReplay || c.ID() != "rpl_1" {
		t.Errorf("identity = %q %q %q", c.Name(), c.Kind(), c.ID())
	}
	s := c.Snapshot()
	if s.Name != "replay:rpl_1" || s.Kind != model.KindReplay || s.ID != "rpl_1" {
		t.Errorf("snapshot identity = %+v", s)
	}
	if time.Since(s.Since) > time.Minute || time.Since(s.Since) < -time.Minute {
		t.Errorf("Since = %v", s.Since)
	}
}

func TestSnapshotEmpty(t *testing.T) {
	s := newTestCollector().Snapshot()
	if s.Requests != 0 || s.Errors != 0 || s.RPS != 0 || s.RPS10 != 0 || s.ErrorRate10 != 0 {
		t.Errorf("non-zero snapshot: %+v", s)
	}
	if (s.Latency != model.LatencySummary{}) || (s.Recent != model.LatencySummary{}) {
		t.Errorf("non-zero latency: %+v %+v", s.Latency, s.Recent)
	}
	if len(s.StatusClasses) != 6 {
		t.Errorf("StatusClasses = %v, want all six keys", s.StatusClasses)
	}
	for _, k := range []string{"1xx", "2xx", "3xx", "4xx", "5xx", "err"} {
		if v, ok := s.StatusClasses[k]; !ok || v != 0 {
			t.Errorf("StatusClasses[%q] = %v, %v", k, v, ok)
		}
	}
	if len(s.Endpoints) != 0 {
		t.Errorf("Endpoints = %v", s.Endpoints)
	}
}

func TestObserveCumulative(t *testing.T) {
	c := newTestCollector()
	for i := 1; i <= 10; i++ {
		status := 200
		if i <= 2 {
			status = 500
		}
		c.Observe(Observation{
			Endpoint: "GET /a", Status: status, Duration: time.Duration(i) * time.Millisecond,
			BytesIn: 100, BytesOut: 1000, Error: model.IsErrorStatus(status),
		})
	}
	c.Observe(Observation{Endpoint: "POST /b", Status: 0, Duration: 50 * time.Millisecond, Error: true}) // transport error
	c.Observe(Observation{Endpoint: "POST /b", Status: 404, Duration: 2 * time.Millisecond})             // not an error
	c.Observe(Observation{Endpoint: "POST /b", Status: 302, Duration: 3 * time.Millisecond, BytesIn: -5, BytesOut: -5})
	c.Observe(Observation{Endpoint: "POST /b", Status: 101, Duration: -time.Second}) // negative clamps to 0

	s := c.Snapshot()
	if s.Requests != 14 || s.Errors != 3 {
		t.Errorf("Requests/Errors = %d/%d, want 14/3", s.Requests, s.Errors)
	}
	if s.BytesIn != 1000 || s.BytesOut != 10000 {
		t.Errorf("Bytes = %d/%d, want 1000/10000 (negatives ignored)", s.BytesIn, s.BytesOut)
	}
	want := map[string]int64{"1xx": 1, "2xx": 8, "3xx": 1, "4xx": 1, "5xx": 2, "err": 1}
	for k, v := range want {
		if s.StatusClasses[k] != v {
			t.Errorf("StatusClasses[%s] = %d, want %d", k, s.StatusClasses[k], v)
		}
	}
	if s.Latency.Count != 14 || s.Latency.Min != 0 || s.Latency.Max != 50 {
		t.Errorf("Latency = %+v", s.Latency)
	}
	approx(t, "mean", s.Latency.Mean, (55.0+50+2+3+0)/14, 1e-9)

	eps := c.Endpoints()
	if len(eps) != 2 || eps[0].Endpoint != "GET /a" || eps[1].Endpoint != "POST /b" {
		t.Fatalf("Endpoints = %+v", eps)
	}
	if eps[0].Count != 10 || eps[0].Errors != 2 || eps[0].Latency.Count != 10 || eps[0].Latency.Max != 10 {
		t.Errorf("GET /a = %+v", eps[0])
	}
	if eps[1].Count != 4 || eps[1].Errors != 1 || eps[1].Latency.Min != 0 || eps[1].Latency.Max != 50 {
		t.Errorf("POST /b = %+v", eps[1])
	}
	if len(s.Endpoints) != 2 || s.Endpoints[0].Endpoint != "GET /a" {
		t.Errorf("snapshot endpoints = %+v", s.Endpoints)
	}
}

func TestInFlightGauge(t *testing.T) {
	c := newTestCollector()
	c.InFlight(1)
	c.InFlight(1)
	c.InFlight(1)
	c.InFlight(-1)
	if got := c.Snapshot().InFlight; got != 2 {
		t.Errorf("InFlight = %d, want 2", got)
	}
	if p := c.Tick(base); p.InFlight != 2 {
		t.Errorf("point InFlight = %d, want 2", p.InFlight)
	}
	c.InFlight(-5) // a stray decrement must not show a negative gauge
	if got := c.Snapshot().InFlight; got != 0 {
		t.Errorf("InFlight = %d, want clamped to 0", got)
	}
	if p := c.Tick(base.Add(time.Second)); p.InFlight != 0 {
		t.Errorf("point InFlight = %d, want 0", p.InFlight)
	}
}

func TestTickWindowContents(t *testing.T) {
	c := newTestCollector()
	for i := 1; i <= 10; i++ {
		status := 200
		if i > 8 {
			status = 503
		}
		c.Observe(Observation{
			Endpoint: "GET /a", Status: status, Duration: time.Duration(i) * time.Millisecond,
			BytesIn: 100, BytesOut: 1000, Error: model.IsErrorStatus(status),
		})
	}
	c.InFlight(3)

	p := c.Tick(base.Add(400 * time.Millisecond))
	if wantT := (base.Unix() - 1) * 1000; p.T != wantT {
		t.Errorf("T = %d, want %d (start of the second just closed)", p.T, wantT)
	}
	if p.Requests != 10 || p.Errors != 2 || p.InFlight != 3 || p.BytesIn != 1000 || p.BytesOut != 10000 {
		t.Errorf("point = %+v", p)
	}
	// Durations 1..10ms: p50 is the 5th (5ms), p95 and p99 the 10th; max exact.
	approx(t, "P50", p.P50, 5, 0.01)
	approx(t, "P95", p.P95, 10, 0.01)
	approx(t, "P99", p.P99, 10, 0.01)
	if p.Max != 10 {
		t.Errorf("Max = %v, want exactly 10", p.Max)
	}

	// The next window is empty: zero counts and latencies, in-flight carried.
	p2 := c.Tick(base.Add(time.Second + 3*time.Millisecond))
	if p2.T != p.T+1000 {
		t.Errorf("T = %d, want %d", p2.T, p.T+1000)
	}
	if want := (model.SeriesPoint{T: p2.T, InFlight: 3}); p2 != want {
		t.Errorf("empty window = %+v, want %+v", p2, want)
	}

	// The window after counts only its own traffic.
	observe(c, 3, "GET /a", 200, 100*time.Millisecond)
	p3 := c.Tick(base.Add(2 * time.Second))
	if p3.Requests != 3 || p3.Errors != 0 || p3.BytesIn != 30 || p3.BytesOut != 300 {
		t.Errorf("third window = %+v", p3)
	}
	approx(t, "P50", p3.P50, 100, 0.01)
	if p3.Max != 100 {
		t.Errorf("Max = %v, want 100", p3.Max)
	}

	// Series holds exactly what Tick returned, oldest first.
	got := c.Series(0)
	if len(got) != 3 || got[0] != p || got[1] != p2 || got[2] != p3 {
		t.Errorf("Series = %+v", got)
	}
	// Cumulative totals are unaffected by ticking.
	if s := c.Snapshot(); s.Requests != 13 || s.Errors != 2 {
		t.Errorf("snapshot after ticks: %d/%d", s.Requests, s.Errors)
	}
}

func TestTickEmptyWindowsProduceZeroPoints(t *testing.T) {
	c := newTestCollector()
	for i := range 5 {
		p := c.Tick(base.Add(time.Duration(i) * time.Second))
		want := model.SeriesPoint{T: (base.Unix() - 1 + int64(i)) * 1000}
		if p != want {
			t.Errorf("tick %d = %+v, want %+v", i, p, want)
		}
	}
	if got := c.Series(0); len(got) != 5 {
		t.Errorf("Series length = %d, want 5", len(got))
	}
	s := c.Snapshot()
	if s.RPS != 0 || s.RPS10 != 0 || s.ErrorRate10 != 0 || (s.Recent != model.LatencySummary{}) {
		t.Errorf("snapshot over idle windows: %+v", s)
	}
}

func TestTickFillsGaps(t *testing.T) {
	c := newTestCollector()
	c.Tick(base) // closes second base-1
	observe(c, 5, "GET /a", 200, 2*time.Millisecond)
	p := c.Tick(base.Add(5 * time.Second)) // seconds base..base+3 were never ticked

	pts := c.Series(0)
	if len(pts) != 6 {
		t.Fatalf("len = %d, want 6 (1 + 4 filler + 1)", len(pts))
	}
	for i, pt := range pts {
		if want := (base.Unix() - 1 + int64(i)) * 1000; pt.T != want {
			t.Errorf("point %d T = %d, want %d", i, pt.T, want)
		}
		if i >= 1 && i <= 4 && (pt.Requests != 0 || pt.P50 != 0) {
			t.Errorf("filler point %d not zero: %+v", i, pt)
		}
	}
	if p != pts[5] || p.Requests != 5 {
		t.Errorf("real point = %+v, last in series %+v", p, pts[5])
	}
	// Fillers count as windows for the rolling rate: 5 requests / 6 windows.
	if s := c.Snapshot(); s.RPS != 5 || math.Abs(s.RPS10-5.0/6) > 1e-12 {
		t.Errorf("RPS %v RPS10 %v", s.RPS, s.RPS10)
	}
}

func TestTickHugeGapIsBounded(t *testing.T) {
	c := newTestCollector()
	c.Tick(base)
	observe(c, 1, "GET /a", 200, time.Millisecond)
	p := c.Tick(base.Add(10 * 24 * time.Hour))
	pts := c.Series(0)
	if len(pts) != SeriesCapacity {
		t.Fatalf("len = %d, want %d", len(pts), SeriesCapacity)
	}
	for i := 1; i < len(pts); i++ {
		if pts[i].T != pts[i-1].T+1000 {
			t.Fatalf("points %d,%d not consecutive: %d %d", i-1, i, pts[i-1].T, pts[i].T)
		}
	}
	if pts[len(pts)-1] != p || p.Requests != 1 {
		t.Errorf("last = %+v, returned %+v", pts[len(pts)-1], p)
	}
}

// TestTickCarriesExtremesOfInFlightObservation replays the interleaving where
// Tick runs after Observe published its window extremes but before its bucket
// and count: the extremes must reach the window that counts the request.
func TestTickCarriesExtremesOfInFlightObservation(t *testing.T) {
	c := newTestCollector()
	const d = 7 * time.Millisecond
	c.winMax.Store(int64(d))
	c.winMinC.Store(math.MaxInt64 - int64(d))

	p := c.Tick(base)
	if p != (model.SeriesPoint{T: p.T}) {
		t.Fatalf("window without traffic must be all zero, got %+v", p)
	}
	c.latency.observe(int64(d)) // the rest of that Observe lands
	p = c.Tick(base.Add(time.Second))
	if p.Requests != 1 || p.Max != 7 {
		t.Fatalf("window = %+v, want 1 request with max 7ms carried over", p)
	}
	approx(t, "P50", p.P50, 7, 0.01)
	if r := c.Snapshot().Recent; r.Min != 7 || r.Max != 7 || r.Count != 1 {
		t.Errorf("recent = %+v", r)
	}
	// Nothing is carried a second time.
	if p := c.Tick(base.Add(2 * time.Second)); p.Max != 0 {
		t.Errorf("max leaked into the following window: %+v", p)
	}
}

func TestTickJitterNearSecondBoundary(t *testing.T) {
	// A standalone ticker whose phase sits just under a second boundary can
	// land 1.0008s apart across it. That must not insert a zero point.
	c := newTestCollector()
	t0 := base.Add(-5 * time.Millisecond)              // x.995
	t1 := base.Add(time.Second + 3*time.Millisecond)   // x+1.003: next whole second, not skipped
	t2 := base.Add(3*time.Second - 4*time.Millisecond) // 1.99s later: one second really was missed
	observe(c, 2, "GET /a", 200, time.Millisecond)
	p0 := c.Tick(t0)
	observe(c, 3, "GET /a", 200, time.Millisecond)
	p1 := c.Tick(t1)
	observe(c, 4, "GET /a", 200, time.Millisecond)
	p2 := c.Tick(t2)

	if p1.T != p0.T+1000 {
		t.Errorf("jittered tick stamped %d after %d, want consecutive", p1.T, p0.T)
	}
	pts := c.Series(0)
	if len(pts) != 4 {
		t.Fatalf("series = %+v, want 4 points (one filler for the really-missed second)", pts)
	}
	if pts[2].Requests != 0 || pts[2].T != p1.T+1000 || p2.T != p1.T+2000 || p2.Requests != 4 {
		t.Errorf("series = %+v", pts)
	}
	if p0.Requests != 2 || p1.Requests != 3 {
		t.Errorf("requests %d, %d", p0.Requests, p1.Requests)
	}
}

func TestTickTimestampsStayStrictlyIncreasing(t *testing.T) {
	c := newTestCollector()
	a := c.Tick(base)
	b := c.Tick(base.Add(100 * time.Millisecond)) // same second again
	d := c.Tick(base.Add(-time.Hour))             // clock stepped back
	for _, pair := range [][2]model.SeriesPoint{{a, b}, {b, d}} {
		if pair[1].T != pair[0].T+1000 {
			t.Errorf("T %d after %d, want +1000", pair[1].T, pair[0].T)
		}
	}
}

func TestSeriesRingWrapsAndFilters(t *testing.T) {
	c := newTestCollector()
	const total = SeriesCapacity + 100
	for i := range total {
		observe(c, 1, "GET /a", 200, time.Millisecond)
		c.Tick(base.Add(time.Duration(i) * time.Second))
	}
	all := c.Series(0)
	if len(all) != SeriesCapacity {
		t.Fatalf("len = %d, want %d", len(all), SeriesCapacity)
	}
	firstT := (base.Unix() - 1 + 100) * 1000 // the first 100 points were overwritten
	for i, p := range all {
		if want := firstT + int64(i)*1000; p.T != want {
			t.Fatalf("point %d T = %d, want %d", i, p.T, want)
		}
		if p.Requests != 1 {
			t.Fatalf("point %d requests = %d", i, p.Requests)
		}
	}

	tests := []struct {
		name  string
		since int64
		want  int
	}{
		{"before everything", 0, SeriesCapacity},
		{"exactly the oldest", firstT, SeriesCapacity},
		{"just after the oldest", firstT + 1, SeriesCapacity - 1},
		{"exactly the newest", all[len(all)-1].T, 1},
		{"after the newest", all[len(all)-1].T + 1, 0},
		{"mid-ring", firstT + 1000*1800, SeriesCapacity - 1800},
	}
	for _, tt := range tests {
		got := c.Series(tt.since)
		if got == nil {
			t.Errorf("%s: Series returned nil", tt.name)
		}
		if len(got) != tt.want {
			t.Errorf("%s: len = %d, want %d", tt.name, len(got), tt.want)
		}
	}

	// The result is a copy.
	got := c.Series(0)
	got[0].Requests = 999
	if c.Series(0)[0].Requests != 1 {
		t.Error("Series exposed ring storage")
	}
}

func TestSeriesOnFreshCollector(t *testing.T) {
	got := newTestCollector().Series(0)
	if got == nil || len(got) != 0 {
		t.Errorf("Series = %#v, want empty non-nil", got)
	}
}

func TestSnapshotRatesOverLastTenWindows(t *testing.T) {
	c := newTestCollector()
	step := func(i int) {
		// window i has i requests, i%2 of them errors
		observe(c, i-i%2, "GET /a", 200, time.Millisecond)
		observe(c, i%2, "GET /a", 500, time.Millisecond)
		c.Tick(base.Add(time.Duration(i) * time.Second))
	}

	for i := 1; i <= 3; i++ {
		step(i)
	}
	s := c.Snapshot() // fewer than 10 windows: averaged over the 3 that exist
	if s.RPS != 3 || s.RPS10 != 2 {
		t.Errorf("after 3 windows RPS %v RPS10 %v, want 3 and 2", s.RPS, s.RPS10)
	}
	if want := 2.0 / 6.0; math.Abs(s.ErrorRate10-want) > 1e-12 {
		t.Errorf("ErrorRate10 = %v, want %v", s.ErrorRate10, want)
	}

	for i := 4; i <= 12; i++ {
		step(i)
	}
	s = c.Snapshot() // windows 3..12
	if s.RPS != 12 {
		t.Errorf("RPS = %v, want 12", s.RPS)
	}
	if s.RPS10 != 7.5 {
		t.Errorf("RPS10 = %v, want 7.5 (windows 3..12 hold 75 requests)", s.RPS10)
	}
	if want := 5.0 / 75.0; math.Abs(s.ErrorRate10-want) > 1e-12 {
		t.Errorf("ErrorRate10 = %v, want %v", s.ErrorRate10, want)
	}
	if s.Recent.Count != 75 {
		t.Errorf("Recent.Count = %d, want 75", s.Recent.Count)
	}
	if s.Requests != 78 {
		t.Errorf("Requests = %d, want 78 (cumulative)", s.Requests)
	}
}

func TestSnapshotRecentLatencyRollsOff(t *testing.T) {
	c := newTestCollector()
	observe(c, 100, "GET /slow", 200, time.Second)
	c.Tick(base) // window 1
	for i := 1; i < recentWindows; i++ {
		observe(c, 10, "GET /fast", 200, 5*time.Millisecond)
		c.Tick(base.Add(time.Duration(i) * time.Second))
	}
	// Windows 1..10 are all inside "recent".
	r := c.Snapshot().Recent
	if r.Count != 190 || r.Max != 1000 || r.Min != 5 {
		t.Errorf("recent with slow window = %+v", r)
	}
	approx(t, "recent p50", r.P50, 1000, 0.01) // 100 of 190 are slow
	approx(t, "recent mean", r.Mean, (100*1000.0+90*5)/190, 1e-9)

	// One more window pushes the slow one out of the last ten.
	observe(c, 10, "GET /fast", 200, 5*time.Millisecond)
	c.Tick(base.Add(time.Duration(recentWindows) * time.Second))
	s := c.Snapshot()
	if s.Recent.Count != 100 || s.Recent.Max != 5 || s.Recent.Min != 5 {
		t.Errorf("recent after roll-off = %+v", s.Recent)
	}
	approx(t, "recent p99", s.Recent.P99, 5, 0.01)
	// The cumulative view still remembers.
	if s.Latency.Max != 1000 || s.Latency.Count != 200 {
		t.Errorf("cumulative latency = %+v", s.Latency)
	}
}

func TestEndpointCapAndOther(t *testing.T) {
	c := newTestCollector()
	const extra = 50
	for i := range MaxEndpoints + extra {
		observe(c, 1, fmt.Sprintf("GET /e/%d", i), 200, time.Millisecond)
	}
	// Existing endpoints keep being tracked after the cap is reached.
	observe(c, 4, "GET /e/0", 200, time.Millisecond)
	// Naming the overflow bucket explicitly lands there, not as a new endpoint.
	observe(c, 2, OtherEndpoint, 500, time.Millisecond)

	if got := len(*c.eps.Load()); got != MaxEndpoints {
		t.Errorf("named endpoints = %d, want %d", got, MaxEndpoints)
	}
	eps := c.Endpoints()
	if len(eps) != MaxEndpoints+1 {
		t.Fatalf("Endpoints = %d, want %d", len(eps), MaxEndpoints+1)
	}
	var other *model.EndpointSummary
	seen := map[string]bool{}
	for i := range eps {
		if seen[eps[i].Endpoint] {
			t.Errorf("duplicate endpoint %q", eps[i].Endpoint)
		}
		seen[eps[i].Endpoint] = true
		if eps[i].Endpoint == OtherEndpoint {
			other = &eps[i]
		}
	}
	if other == nil || other.Count != extra+2 || other.Errors != 2 {
		t.Fatalf("other = %+v, want count %d errors 2", other, extra+2)
	}
	if eps[0].Endpoint != OtherEndpoint || eps[1].Endpoint != "GET /e/0" || eps[1].Count != 5 {
		t.Errorf("busiest = %+v then %+v, want (other) then GET /e/0", eps[0], eps[1])
	}
	var total int64
	for _, e := range eps {
		total += e.Count
	}
	if total != int64(MaxEndpoints+extra+4+2) || c.Snapshot().Requests != total {
		t.Errorf("endpoint counts sum to %d, snapshot requests %d", total, c.Snapshot().Requests)
	}
}

func TestEndpointOrderingAndLimit(t *testing.T) {
	c := newTestCollector()
	observe(c, 5, "B", 200, time.Millisecond)
	observe(c, 5, "A", 200, time.Millisecond)
	observe(c, 9, "C", 200, time.Millisecond)
	var got []string
	for _, e := range c.Endpoints() {
		got = append(got, e.Endpoint)
	}
	if strings.Join(got, ",") != "C,A,B" {
		t.Errorf("order = %v, want count desc then name asc: C,A,B", got)
	}

	// Snapshot carries only the busiest SnapshotEndpoints.
	c2 := newTestCollector()
	for i := range 30 {
		observe(c2, 30-i, fmt.Sprintf("GET /e/%02d", i), 200, time.Millisecond)
	}
	s := c2.Snapshot()
	if len(s.Endpoints) != SnapshotEndpoints {
		t.Fatalf("snapshot endpoints = %d, want %d", len(s.Endpoints), SnapshotEndpoints)
	}
	for i, e := range s.Endpoints {
		if want := fmt.Sprintf("GET /e/%02d", i); e.Endpoint != want || e.Count != int64(30-i) {
			t.Errorf("snapshot endpoint %d = %+v, want %s", i, e, want)
		}
	}
	if len(c2.Endpoints()) != 30 {
		t.Errorf("Endpoints() should not be limited")
	}
}

func TestEndpointNameHandling(t *testing.T) {
	c := newTestCollector()
	observe(c, 1, "", 200, time.Millisecond)
	long1 := strings.Repeat("x", maxEndpointName) + "AAA"
	long2 := strings.Repeat("x", maxEndpointName) + "BBB"
	observe(c, 1, long1, 200, time.Millisecond)
	observe(c, 1, long2, 200, time.Millisecond)

	// A name backed by a buffer the caller reuses must be copied.
	buf := []byte("GET /reused")
	observe(c, 1, unsafe.String(&buf[0], len(buf)), 200, time.Millisecond)
	copy(buf, "XXX /mutated")

	byName := map[string]int64{}
	for _, e := range c.Endpoints() {
		byName[e.Endpoint] = e.Count
	}
	if byName[UnknownEndpoint] != 1 {
		t.Errorf("empty endpoint should be %q: %v", UnknownEndpoint, byName)
	}
	if byName[strings.Repeat("x", maxEndpointName)] != 2 {
		t.Errorf("long names should be truncated and merged: %d entries", len(byName))
	}
	if byName["GET /reused"] != 1 {
		t.Errorf("endpoint name aliased the caller's buffer: %v", byName)
	}
}

func TestLatencyReturnsCopy(t *testing.T) {
	c := newTestCollector()
	observe(c, 3, "GET /a", 200, 4*time.Millisecond)
	h := c.Latency()
	if h.Count() != 3 || h.Max() != 4*time.Millisecond {
		t.Fatalf("copy = %d %v", h.Count(), h.Max())
	}
	h.Record(time.Hour)
	if c.Snapshot().Latency.Count != 3 {
		t.Error("mutating the copy changed the collector")
	}
	observe(c, 1, "GET /a", 200, time.Millisecond)
	if h.Count() != 4 { // 3 + the Record above, not the new observation
		t.Errorf("copy picked up later observations: %d", h.Count())
	}
}

func TestReset(t *testing.T) {
	c := newTestCollector()
	for i := range 4 {
		observe(c, 5, fmt.Sprintf("GET /e/%d", i), 500, 30*time.Millisecond)
		c.Tick(base.Add(time.Duration(i) * time.Second))
	}
	c.InFlight(2)
	before := c.Snapshot().Since
	time.Sleep(2 * time.Millisecond)

	c.Reset()
	s := c.Snapshot()
	if !s.Since.After(before) {
		t.Errorf("Since not advanced: %v -> %v", before, s.Since)
	}
	if s.Requests != 0 || s.Errors != 0 || s.BytesIn != 0 || s.BytesOut != 0 || s.RPS != 0 || s.RPS10 != 0 ||
		s.ErrorRate10 != 0 || len(s.Endpoints) != 0 || (s.Latency != model.LatencySummary{}) || (s.Recent != model.LatencySummary{}) {
		t.Errorf("not cleared: %+v", s)
	}
	for k, v := range s.StatusClasses {
		if v != 0 {
			t.Errorf("class %s = %d", k, v)
		}
	}
	if s.InFlight != 2 {
		t.Errorf("InFlight = %d: the gauge tracks live requests and must survive Reset", s.InFlight)
	}
	if got := c.Series(0); len(got) != 0 {
		t.Errorf("series not cleared: %d points", len(got))
	}
	if len(c.Endpoints()) != 0 || c.Latency().Count() != 0 {
		t.Error("endpoints or latency not cleared")
	}

	// A reset collector works as new, including window accounting.
	observe(c, 7, "GET /new", 200, 9*time.Millisecond)
	p := c.Tick(base.Add(time.Hour))
	if p.Requests != 7 || p.Errors != 0 || p.Max != 9 {
		t.Errorf("first window after Reset = %+v", p)
	}
	if want := (base.Add(time.Hour).Unix() - 1) * 1000; p.T != want {
		t.Errorf("T = %d, want %d (no gap fill across a Reset)", p.T, want)
	}
	if got := len(c.Series(0)); got != 1 {
		t.Errorf("series length after reset+tick = %d", got)
	}
}

// TestObserveRacingTickCountsOnce hammers Observe while another goroutine
// ticks as fast as it can; every observation must land in exactly one window.
func TestObserveRacingTickCountsOnce(t *testing.T) {
	const (
		writers   = 8
		perWriter = 40_000
	)
	c := newTestCollector()
	var (
		wg         sync.WaitGroup
		done       atomic.Bool
		reqs, errs int64
		bin, bout  int64
		maxMs      float64
		windows    int
		tickErr    error
	)
	accumulate := func(p model.SeriesPoint) {
		reqs += p.Requests
		errs += p.Errors
		bin += p.BytesIn
		bout += p.BytesOut
		maxMs = max(maxMs, p.Max)
		windows++
		if p.Errors < 0 || p.Requests < 0 || p.BytesIn < 0 || p.P50 < 0 {
			tickErr = fmt.Errorf("negative field: %+v", p)
		}
	}
	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		for i := 0; !done.Load(); i++ {
			accumulate(c.Tick(base.Add(time.Duration(i) * time.Second)))
		}
	}()

	var wantErrs, wantBytesIn, wantBytesOut atomic.Int64
	var wantMax atomic.Int64
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 5))
			for i := range perWriter {
				status := 200
				if i%7 == 0 {
					status = 502
				}
				d := time.Duration(1000 + r.Int64N(50_000_000))
				in, out := int64(r.IntN(500)), int64(r.IntN(5000))
				c.InFlight(1)
				c.Observe(Observation{
					Endpoint: fmt.Sprintf("GET /e/%d", i%5), Status: status, Duration: d,
					BytesIn: in, BytesOut: out, Error: model.IsErrorStatus(status),
				})
				c.InFlight(-1)
				if status == 502 {
					wantErrs.Add(1)
				}
				wantBytesIn.Add(in)
				wantBytesOut.Add(out)
				atomicMax(&wantMax, int64(d))
			}
		}()
	}
	wg.Wait()
	done.Store(true)
	<-tickerDone
	accumulate(c.Tick(base.Add(time.Hour))) // collect whatever was left

	if tickErr != nil {
		t.Fatal(tickErr)
	}
	const total = writers * perWriter
	if reqs != total {
		t.Errorf("requests across %d windows = %d, want %d", windows, reqs, total)
	}
	if errs != wantErrs.Load() {
		t.Errorf("errors across windows = %d, want %d", errs, wantErrs.Load())
	}
	if bin != wantBytesIn.Load() || bout != wantBytesOut.Load() {
		t.Errorf("bytes across windows = %d/%d, want %d/%d", bin, bout, wantBytesIn.Load(), wantBytesOut.Load())
	}
	if want := float64(wantMax.Load()) / 1e6; maxMs != want {
		t.Errorf("max across windows = %v, want %v", maxMs, want)
	}
	// Every histogram bucket was carried into some window exactly once.
	for i := range c.latency.counts {
		if got, want := c.prev[i], c.latency.counts[i].Load(); got != want {
			t.Fatalf("bucket %d: windows saw %d, histogram has %d", i, got, want)
		}
	}
	if s := c.Snapshot(); s.Requests != total || s.InFlight != 0 {
		t.Errorf("snapshot = %d requests, %d in flight", s.Requests, s.InFlight)
	}
	t.Logf("%d windows closed while %d observations raced", windows, total)
}

// TestCollectorStress runs every operation concurrently, for the race detector
// and for panics, including Reset.
func TestCollectorStress(t *testing.T) {
	c := newTestCollector()
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
			c.InFlight(1)
			c.Observe(Observation{
				Endpoint: fmt.Sprintf("GET /e/%d", (i+g)%(MaxEndpoints+20)), Status: 200 + 100*(i%4),
				Duration: time.Duration(i%1000) * time.Microsecond, BytesIn: 1, BytesOut: 2, Error: i%9 == 0,
			})
			c.InFlight(-1)
		})
	}
	var tick atomic.Int64
	run(func(int) { c.Tick(base.Add(time.Duration(tick.Add(1)) * time.Second)) })
	run(func(int) { _ = c.Snapshot() })
	run(func(int) { _ = c.Series(0); _ = c.Endpoints(); _ = c.Latency() })
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

func TestObserveDoesNotAllocate(t *testing.T) {
	c := newTestCollector()
	o := Observation{Endpoint: "GET /users/{id}", Status: 200, Duration: 3 * time.Millisecond, BytesIn: 10, BytesOut: 200}
	c.Observe(o) // first sight of the endpoint allocates; later ones must not
	if allocs := testing.AllocsPerRun(1000, func() { c.Observe(o) }); allocs != 0 {
		t.Errorf("Observe allocates %v times per call, want 0", allocs)
	}
	// Overflow endpoints are allocation-free once the overflow bucket exists.
	for i := range MaxEndpoints + 1 {
		c.Observe(Observation{Endpoint: fmt.Sprintf("e%d", i)})
	}
	o.Endpoint = "never seen before"
	if allocs := testing.AllocsPerRun(1000, func() { c.Observe(o) }); allocs != 0 {
		t.Errorf("overflow Observe allocates %v times per call, want 0", allocs)
	}
}

func benchEndpoints(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("GET /api/v1/resource%d/{id}", i)
	}
	return names
}

func BenchmarkCollectorObserve(b *testing.B) {
	c := newTestCollector()
	names := benchEndpoints(8)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		c.Observe(Observation{
			Endpoint: names[i&7], Status: 200, Duration: time.Duration(1_000_000 + i&0xfffff),
			BytesIn: 120, BytesOut: 2048,
		})
	}
}

func BenchmarkCollectorObserveParallel(b *testing.B) {
	c := newTestCollector()
	names := benchEndpoints(8)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Observe(Observation{
				Endpoint: names[i&7], Status: 200, Duration: time.Duration(1_000_000 + i&0xfffff),
				BytesIn: 120, BytesOut: 2048, Error: i%50 == 0,
			})
			i++
		}
	})
}

func BenchmarkCollectorObserveParallelOneEndpoint(b *testing.B) {
	c := newTestCollector()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Observe(Observation{Endpoint: "GET /hot", Status: 200, Duration: time.Duration(1_000_000 + i&0xfffff), BytesOut: 512})
			i++
		}
	})
}

func populatedCollector(endpoints int) *Collector {
	c := newTestCollector()
	r := rand.New(rand.NewPCG(1, 2))
	names := benchEndpoints(endpoints)
	for i := range 200_000 {
		c.Observe(Observation{
			Endpoint: names[r.IntN(len(names))], Status: 200, BytesIn: 100, BytesOut: 1000,
			Duration: time.Duration(float64(20*time.Millisecond) * math.Exp(0.8*r.NormFloat64())),
			Error:    i%40 == 0,
		})
	}
	for i := range 12 {
		c.Tick(base.Add(time.Duration(i) * time.Second))
	}
	return c
}

func BenchmarkCollectorSnapshot(b *testing.B) {
	c := populatedCollector(100)
	b.ReportAllocs()
	for b.Loop() {
		_ = c.Snapshot()
	}
}

func BenchmarkCollectorSnapshotFewEndpoints(b *testing.B) {
	c := populatedCollector(5)
	b.ReportAllocs()
	for b.Loop() {
		_ = c.Snapshot()
	}
}

func BenchmarkCollectorEndpoints(b *testing.B) {
	c := populatedCollector(100)
	b.ReportAllocs()
	for b.Loop() {
		_ = c.Endpoints()
	}
}

func BenchmarkCollectorTick(b *testing.B) {
	c := populatedCollector(20)
	names := benchEndpoints(20)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		for j := range 50 {
			c.Observe(Observation{Endpoint: names[j%20], Status: 200, Duration: time.Duration(1_000_000 + j*37_000)})
		}
		c.Tick(base.Add(time.Duration(100+i) * time.Second))
	}
}
