package replay

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// maxSafeJSONInt is 2^53-1: the largest integer every JSON client (a browser's
// JSON.parse) represents exactly.
const maxSafeJSONInt = 1<<53 - 1

// TestTimeoutsReachTheLatencySummaries guards the survivorship bias in the
// summary: requests that waited for the target and then timed out must show in
// Latency and CorrectedLatency (at least as long as the timeout), or a hung
// target would report a better p99 than a healthy one.
func TestTimeoutsReachTheLatencySummaries(t *testing.T) {
	st := openStore(t)
	exs := numbered(10, 2*time.Millisecond)
	for i := range exs {
		if i%2 == 1 {
			exs[i].path = "/slow"
		}
	}
	rec := seedRecording(t, st, exs...)
	target := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			select {
			case <-time.After(3 * time.Second):
			case <-r.Context().Done():
			}
		}
	})
	cfg := baseCfg(rec, target.URL)
	cfg.Timeout = model.Duration(200 * time.Millisecond)
	sum, _ := mustRun(t, st, cfg)

	if sum.Completed != 5 || sum.Timeouts != 5 || sum.Failed != 5 {
		t.Fatalf("completed=%d timeouts=%d failed=%d, want 5/5/5", sum.Completed, sum.Timeouts, sum.Failed)
	}
	if sum.Latency.Count != 10 || sum.CorrectedLatency.Count != 10 {
		t.Errorf("latency counts %d / %d, want every request that waited (10)", sum.Latency.Count, sum.CorrectedLatency.Count)
	}
	// A timeout waited at least Timeout, and half the requests timed out, so
	// the upper percentiles are the timeout, not the survivors' few ms.
	for name, l := range map[string]model.LatencySummary{"service": sum.Latency, "corrected": sum.CorrectedLatency} {
		if l.Max < 200 {
			t.Errorf("%s max %.1fms, want at least the 200ms timeout", name, l.Max)
		}
		if l.P99 < 190 || l.P95 < 190 {
			t.Errorf("%s p95/p99 %.1f/%.1fms, want the timeouts to show", name, l.P95, l.P99)
		}
	}
	// The summary agrees with the endpoint table, which has always had them.
	if len(sum.Endpoints) == 0 || sum.Endpoints[0].Latency.Max < 190 {
		t.Errorf("endpoints: %+v", sum.Endpoints)
	}
}

// TestHungUpRequestsReachTheLatencySummaries covers a transport error after a
// connection was made: the time waited counts. A refused connection (no
// request ever left) stays out, see TestRunTransportErrors.
func TestHungUpRequestsReachTheLatencySummaries(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(10, time.Millisecond)...)
	target := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/items/3" || r.URL.Path == "/items/7" {
			time.Sleep(30 * time.Millisecond)
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		}
	})
	sum, _ := mustRun(t, st, baseCfg(rec, target.URL))
	if sum.Failed != 2 || sum.Completed != 8 {
		t.Fatalf("completed=%d failed=%d", sum.Completed, sum.Failed)
	}
	if sum.Latency.Count != 10 || sum.Latency.Max < 25 {
		t.Errorf("latency count %d max %.1fms: the two hang-ups waited ~30ms and must be counted", sum.Latency.Count, sum.Latency.Max)
	}
}

// TestRefusedConnectionsStayOutOfTheLatencySummaries: nothing was sent, so
// there is no latency to report. The CLI gate has to treat an empty summary
// with failures as a failure (it sees Completed == 0).
func TestRefusedConnectionsStayOutOfTheLatencySummaries(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(5, time.Millisecond)...)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	sum, _ := mustRun(t, st, baseCfg(rec, "http://"+addr))
	if sum.Failed != 5 || sum.Completed != 0 || sum.Latency.Count != 0 || sum.CorrectedLatency.Count != 0 {
		t.Errorf("failed=%d completed=%d latency=%d corrected=%d", sum.Failed, sum.Completed, sum.Latency.Count, sum.CorrectedLatency.Count)
	}
}

// TestMaxModeIsNotSaturated: a closed loop keeps every worker busy by
// definition, and has no intended send times to lag behind, so "saturated"
// (and the CLI's "raise --concurrency") would be noise on every run.
func TestMaxModeIsNotSaturated(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(20, time.Millisecond)...)
	target := newRecorder(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(5 * time.Millisecond) })
	cfg := baseCfg(rec, target.URL)
	cfg.Mode = model.ReplayMax
	cfg.Concurrency = 4
	cfg.Loop = true
	cfg.MaxRequests = 60
	sum, _ := mustRun(t, st, cfg)
	if sum.MaxInFlight != 4 {
		t.Fatalf("max in flight %d, want all 4 workers busy", sum.MaxInFlight)
	}
	if sum.Saturated {
		t.Error("max mode reported saturated")
	}
}

// TestRateModeStillReportsSaturation keeps the open-loop meaning intact.
func TestRateModeStillReportsSaturation(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(10, time.Millisecond)...)
	target := newRecorder(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(80 * time.Millisecond) })
	cfg := baseCfg(rec, target.URL)
	cfg.Mode = model.ReplayRate
	cfg.Rate = 100
	cfg.Duration = model.Duration(300 * time.Millisecond)
	cfg.Concurrency = 2
	sum, _ := mustRun(t, st, cfg)
	if !sum.Saturated {
		t.Errorf("rate mode with 2 slots at 100 rps against an 80ms target must saturate: %+v", sum)
	}
}

// TestClosedLoopDoesNotDialMoreConnectionsThanWorkers: a worker that starts
// its next request before the transport has put the previous connection back
// used to dial a fresh one, and the surplus was thrown away. At a few
// thousand workers that exhausts the ephemeral ports.
func TestClosedLoopDoesNotDialMoreConnectionsThanWorkers(t *testing.T) {
	var dialed atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			dialed.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	const workers = 128
	n := int64(60_000)
	if raceEnabled {
		n = 15_000
	}
	st := openStore(t)
	rec := seedRecording(t, st, numbered(1000, time.Millisecond)...)
	cfg := baseCfg(rec, srv.URL)
	cfg.Mode = model.ReplayMax
	cfg.Loop = true
	cfg.MaxRequests = n
	cfg.Concurrency = workers
	sum, _ := mustRun(t, st, cfg)
	if sum.Sent != n || sum.Failed > n/100 {
		t.Fatalf("sent %d failed %d", sum.Sent, sum.Failed)
	}
	got := dialed.Load()
	t.Logf("%d requests on %d workers dialed %d connections", sum.Sent, workers, got)
	if got > workers+workers/8 {
		t.Errorf("dialed %d connections for %d workers", got, workers)
	}
}

// TestTransportCapsConnectionsAtConcurrency pins the setting that does it.
func TestTransportCapsConnectionsAtConcurrency(t *testing.T) {
	tr := newTransport(model.ReplayConfig{Concurrency: 300})
	if tr.MaxConnsPerHost != 300 || tr.MaxIdleConnsPerHost != 300 || tr.MaxIdleConns < 300 {
		t.Errorf("MaxConnsPerHost=%d MaxIdleConnsPerHost=%d MaxIdleConns=%d", tr.MaxConnsPerHost, tr.MaxIdleConnsPerHost, tr.MaxIdleConns)
	}
}

// TestAchievedRPSIsMeasuredOverTheSendWindow: the drain after the last
// request goes out is not part of the rate. A run that hit its schedule
// exactly must not be reported below it because a few requests were slow.
func TestAchievedRPSIsMeasuredOverTheSendWindow(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(50, time.Millisecond)...)
	var n atomic.Int64
	target := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1)%5 == 0 {
			time.Sleep(400 * time.Millisecond) // a long tail: the run drains for ~0.4s
		}
	})
	cfg := baseCfg(rec, target.URL)
	cfg.Mode = model.ReplayRate
	cfg.Rate = 100
	cfg.Duration = model.Duration(time.Second)
	sum, _ := mustRun(t, st, cfg)
	if sum.Sent != 100 || sum.Completed != 100 {
		t.Fatalf("sent %d completed %d, want 100", sum.Sent, sum.Completed)
	}
	if e := sum.Elapsed.D(); e < 1300*time.Millisecond {
		t.Fatalf("elapsed %v: the test needs a drain of ~0.4s", e)
	}
	if sum.AchievedRPS < 93 || sum.AchievedRPS > 107 {
		t.Errorf("achieved %.1f rps for a 100 rps schedule that delivered every request in 1s (elapsed %v)", sum.AchievedRPS, sum.Elapsed.D())
	}
}

// TestAchievedRPSOfAFinishedClosedLoopRun keeps completed / elapsed for runs
// that end by running out of requests rather than by time.
func TestAchievedRPSOfAFinishedClosedLoopRun(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(40, time.Millisecond)...)
	target := newRecorder(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(10 * time.Millisecond) })
	cfg := baseCfg(rec, target.URL)
	cfg.Mode = model.ReplayMax
	cfg.Concurrency = 4
	sum, _ := mustRun(t, st, cfg)
	want := float64(sum.Completed) / sum.Elapsed.D().Seconds()
	if sum.Completed != 40 || sum.AchievedRPS < 0.97*want || sum.AchievedRPS > 1.03*want {
		t.Errorf("completed %d achieved %.1f, want about %.1f", sum.Completed, sum.AchievedRPS, want)
	}
}

// TestRandomSeedsRoundTripThroughJSONNumbers: a browser parses JSON numbers
// as doubles, so a seed above 2^53 comes back changed and Edit-as-new would
// replay a different schedule.
func TestRandomSeedsRoundTripThroughJSONNumbers(t *testing.T) {
	for i := 0; i < 20_000; i++ {
		s := randomSeed()
		if s == 0 || s > maxSafeJSONInt {
			t.Fatalf("randomSeed() = %d, want 1..2^53-1", s)
		}
		if f := float64(s); uint64(f) != s {
			t.Fatalf("seed %d does not survive a double", s)
		}
	}
}

func TestCreatedJobsGetABrowserSafeSeed(t *testing.T) {
	e := newEnv(t)
	rec := seedRecording(t, e.st, numbered(3, time.Millisecond)...)
	for i := 0; i < 20; i++ {
		job := e.create(t, baseCfg(rec, "http://127.0.0.1:1"))
		if job.Config.Seed == 0 || job.Config.Seed > maxSafeJSONInt {
			t.Fatalf("seed %d", job.Config.Seed)
		}
		b, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		var generic struct {
			Config struct {
				Seed float64 `json:"seed"`
			} `json:"config"`
		}
		if err := json.Unmarshal(b, &generic); err != nil || uint64(generic.Config.Seed) != job.Config.Seed {
			t.Fatalf("seed %d came back as %v (%v)", job.Config.Seed, generic.Config.Seed, err)
		}
	}
}

// TestLoopPlannedMatchesTheSchedule: for a looping timeline with a Duration,
// Planned counts the requests that are due before Duration instead of
// estimating them from the pass rate, so Sent never exceeds it.
func TestLoopPlannedMatchesTheSchedule(t *testing.T) {
	p := itemsAt([]int{0, 100, 200, 300, 300, 450}, 1, 4)
	tests := []struct {
		name string
		mod  func(*model.ReplayConfig)
	}{
		{"speed 7 for 4s", func(c *model.ReplayConfig) { c.Speed = 7; c.Duration = model.Duration(4 * time.Second) }},
		{"speed 3 for 1s", func(c *model.ReplayConfig) { c.Speed = 3; c.Duration = model.Duration(time.Second) }},
		{"speed 1 for 1.3s", func(c *model.ReplayConfig) { c.Duration = model.Duration(1300 * time.Millisecond) }},
		{"speed 13.7 for 2.9s", func(c *model.ReplayConfig) { c.Speed = 13.7; c.Duration = model.Duration(2900 * time.Millisecond) }},
		{"duration inside the first pass", func(c *model.ReplayConfig) { c.Duration = model.Duration(250 * time.Millisecond) }},
		{"amplify 3", func(c *model.ReplayConfig) {
			c.Speed = 5
			c.Amplify = 3
			c.Duration = model.Duration(1700 * time.Millisecond)
		}},
		{"integer error amplification", func(c *model.ReplayConfig) {
			c.Speed = 9
			c.ErrorAmplification = 3
			c.Duration = model.Duration(2300 * time.Millisecond)
		}},
		{"error amplification below 1 is not random for one pass only", func(c *model.ReplayConfig) {
			c.Speed = 4
			c.ErrorAmplification = 0
			c.Duration = model.Duration(1900 * time.Millisecond)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tlCfg(func(c *model.ReplayConfig) { c.Loop = true; tt.mod(c) })
			g, planned, _ := newGenerator(p, cfg)
			got := int64(len(drain(g, cfg, 5_000_000)))
			if planned != got {
				t.Errorf("Planned = %d, the schedule sends %d requests before Duration", planned, got)
			}
		})
	}
}

// TestPlannedIsNeverBelowSentForAnEstimate: with fractional error
// amplification the later passes draw differently from the first, so Planned
// is an estimate and the summary must not report more sent than planned.
func TestPlannedIsNeverBelowSentForAnEstimate(t *testing.T) {
	r, err := newRunner(openStore(t), baseCfg("rec", "http://t"), RunOptions{Collector: newTestCollector()})
	if err != nil {
		t.Fatal(err)
	}
	r.planned.Store(28)
	r.sent.Store(29)
	r.plannedEstimate.Store(true)
	if s := r.summary(); s.Planned != 29 || s.Sent != 29 {
		t.Errorf("estimate: planned %d sent %d, want 29/29", s.Planned, s.Sent)
	}
	r.plannedEstimate.Store(false)
	if s := r.summary(); s.Planned != 28 {
		t.Errorf("an exact plan must not be adjusted: planned %d", s.Planned)
	}
}

func TestLoopedTimelineRunNeverSendsMoreThanPlanned(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(4, 100*time.Millisecond)...)
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, target.URL)
	cfg.Loop = true
	cfg.Speed = 7
	cfg.Duration = model.Duration(1500 * time.Millisecond)
	sum, _ := mustRun(t, st, cfg)
	if sum.Planned <= 0 || sum.Sent != sum.Planned {
		t.Errorf("planned %d sent %d", sum.Planned, sum.Sent)
	}
}

// TestGetOfARunningJobHasTheSeries: docs/API.md promises `series` on
// GET /replays/{id}; a UI opened mid-run seeds its charts from it.
func TestGetOfARunningJobHasTheSeries(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rec := seedRecording(t, e.st, numbered(200, 20*time.Millisecond)...)
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, target.URL)
	cfg.Mode = model.ReplayRate
	cfg.Rate = 50
	cfg.Duration = model.Duration(6 * time.Second)
	job := e.create(t, cfg)
	if err := e.m.Start(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.m.Stop(job.ID) })

	var live []model.SeriesPoint
	waitFor(t, 10*time.Second, "two series points", func() bool {
		live, _ = e.m.Series(ctx, job.ID, 0)
		return len(live) >= 2
	})
	got, err := e.m.Get(ctx, job.ID)
	if err != nil || got.Status != model.ReplayRunning {
		t.Fatalf("Get: %+v %v", got, err)
	}
	if len(got.Series) < len(live) || got.Series[0].T != live[0].T {
		t.Errorf("Get returned %d series points (first %v), Series had %d (first %v)", len(got.Series), got.Series, len(live), live)
	}
	// The listings stay light.
	list, err := e.m.List(ctx)
	if err != nil || len(list) != 1 || list[0].Series != nil {
		t.Errorf("List: %+v %v", list, err)
	}
	if run := e.m.Running(); len(run) != 1 || run[0].Series != nil {
		t.Errorf("Running: %+v", run)
	}
}

// A scheduler that wakes late finds its timer and the soft (Duration) context
// both ready, and select picks either. A request that was due before Duration
// must still be sent; only a request that is not yet due is cut by it.
func TestSoftEndedKeepsWhatWasDueAndCutsWhatIsNot(t *testing.T) {
	r, err := newRunner(openStore(t), baseCfg("rec", "http://t"), RunOptions{Collector: newTestCollector()})
	if err != nil {
		t.Fatal(err)
	}
	r.start = time.Now().Add(-time.Second)
	hard := context.Background()
	if !r.softEnded(hard, 500*time.Millisecond) {
		t.Error("due 0.5s ago: the late scheduler must still send it")
	}
	if r.softEnded(hard, 10*time.Second) {
		t.Error("due in 9s: soft ending cuts it")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if r.softEnded(cancelled, 500*time.Millisecond) {
		t.Error("a cancelled run sends nothing more, due or not")
	}

	// waitUntil gives up on a request that is not due once soft has ended, on
	// both the sleeping and the spinning path.
	soft, cancelSoft := context.WithCancel(hard)
	cancelSoft()
	for _, spin := range []bool{false, true} {
		r.spin = spin
		for _, ahead := range []time.Duration{800 * time.Microsecond, 5 * time.Second} {
			if r.waitUntil(hard, soft, r.now()+ahead) {
				t.Errorf("spin=%v: due in %v with soft ended: the wait must give up", spin, ahead)
			}
		}
	}
}
