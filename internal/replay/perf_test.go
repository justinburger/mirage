package replay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// stampTarget is a trivial local server that timestamps arrivals.
type stampTarget struct {
	*httptest.Server
	n    atomic.Int64
	seen []int64 // arrival times in ns since base, preallocated
	base time.Time
}

func newStampTarget(t testing.TB, capacity int) *stampTarget {
	st := &stampTarget{seen: make([]int64, capacity), base: time.Now()}
	st.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if i := st.n.Add(1) - 1; int(i) < len(st.seen) {
			st.seen[i] = int64(time.Since(st.base))
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(st.Close)
	return st
}

// errors returns how far each arrival is from the ideal schedule i/rate, in
// nanoseconds, after removing the median offset (the first request's dial
// time and the network latency shift the whole schedule, which is not
// jitter).
func (st *stampTarget) errors(rate float64) []int64 {
	n := min(int(st.n.Load()), len(st.seen))
	at := slices.Clone(st.seen[:n])
	slices.Sort(at)
	out := make([]int64, n)
	for i := range at {
		out[i] = at[i] - int64(float64(i)/rate*1e9)
	}
	med := slices.Clone(out)
	slices.Sort(med)
	for i := range out {
		out[i] -= med[len(med)/2]
	}
	return out
}

func pct(sorted []int64, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return time.Duration(sorted[min(int(p*float64(len(sorted))), len(sorted)-1)])
}

// TestSchedulerAccuracy measures how well the open-model scheduler holds a
// fixed rate against a trivial local server: achieved vs target requests per
// second, the worst intended-vs-actual send delay the job itself reports, and
// the jitter of the arrivals at the target. The numbers are logged; the
// assertions only catch gross regressions (a scheduler that sleeps with timer
// granularity manages ~1000 req/s) and are skipped when the machine could not
// keep up and the job reports itself saturated.
func TestSchedulerAccuracy(t *testing.T) {
	if testing.Short() {
		t.Skip("scheduler accuracy runs for seconds")
	}
	rates := []float64{500, 2000, 10000}
	if raceEnabled {
		rates = rates[:2] // 10k req/s is beyond what the race detector leaves room for
	}
	for _, rate := range rates {
		const seconds = 1.0
		t.Run(fmt.Sprintf("rate=%.0f", rate), func(t *testing.T) {
			st := openStore(t)
			rec := seedRecording(t, st, numbered(100, time.Millisecond)...)
			target := newStampTarget(t, int(rate*seconds)+100)
			cfg := baseCfg(rec, target.URL)
			cfg.Mode = model.ReplayRate
			cfg.Rate = rate
			cfg.Duration = model.Duration(time.Duration(seconds * float64(time.Second)))
			sum, _ := mustRun(t, st, cfg)

			errs := target.errors(rate)
			abs := make([]int64, len(errs))
			for i, e := range errs {
				abs[i] = max(e, -e)
			}
			slices.Sort(abs)
			t.Logf("rate %.0f/s for %.1fs: sent %d of %d planned, achieved %.0f req/s (%.1f%% of target), max lag %v, "+
				"target-side jitter p50=%v p99=%v max=%v, corrected p99 %.2fms, saturated=%v",
				rate, seconds, sum.Sent, sum.Planned, sum.AchievedRPS, 100*sum.AchievedRPS/rate, sum.MaxLag.D(),
				pct(abs, 0.5), pct(abs, 0.99), pct(abs, 1), sum.CorrectedLatency.P99, sum.Saturated)

			if sum.Saturated {
				t.Skipf("the machine could not sustain %.0f req/s (%d of %d slots busy); numbers above are for information",
					rate, sum.MaxInFlight, cfg.WithDefaults().Concurrency)
			}
			// A request or two can be reset when a stall makes the scheduler
			// catch up with a burst of new connections (see TestMaxModeThroughput).
			if sum.Planned != int64(rate*seconds) || sum.Sent != sum.Planned || sum.Failed > sum.Planned/100 {
				t.Errorf("planned %d sent %d completed %d failed %d, want %d sent", sum.Planned, sum.Sent, sum.Completed, sum.Failed, int64(rate*seconds))
			}
			floor, lag := 0.9, 250*time.Millisecond
			if raceEnabled {
				floor, lag = 0.6, time.Second
			}
			if sum.AchievedRPS < floor*rate {
				t.Errorf("achieved %.0f req/s, want at least %.0f%% of %.0f", sum.AchievedRPS, 100*floor, rate)
			}
			if sum.MaxLag.D() > lag {
				t.Errorf("max lag %v exceeds %v", sum.MaxLag.D(), lag)
			}
		})
	}
}

// TestMaxModeThroughput measures closed-loop throughput against a trivial
// local server. The target (30k req/s at concurrency 256) is reported by
// BenchmarkRunMax; this only guards against collapse.
func TestMaxModeThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput run")
	}
	n := int64(100_000)
	if raceEnabled {
		n = 15_000
	}
	st := openStore(t)
	rec := seedRecording(t, st, numbered(1000, time.Millisecond)...)
	target := newStampTarget(t, 0)
	cfg := baseCfg(rec, target.URL)
	cfg.Mode = model.ReplayMax
	cfg.Loop = true
	cfg.MaxRequests = n
	cfg.Concurrency = 256
	sum, _ := mustRun(t, st, cfg)
	t.Logf("max mode, concurrency 256: %d requests in %v = %.0f req/s (max in flight %d, p50 %.2fms p99 %.2fms)",
		sum.Completed, sum.Elapsed.D(), sum.AchievedRPS, sum.MaxInFlight, sum.Latency.P50, sum.Latency.P99)
	// 256 workers dial at the same instant; a loopback listener's accept
	// queue (128 on some systems) resets the overflow, so a handful of
	// connection resets at start-up is expected, not a defect.
	if sum.Sent != n || sum.Completed+sum.Failed != n || sum.Failed > n/100 {
		t.Fatalf("sent %d completed %d failed %d, want %d requests with under 1%% failures", sum.Sent, sum.Completed, sum.Failed, n)
	}
	floor := 8000.0
	if raceEnabled {
		floor = 1500
	}
	if sum.AchievedRPS < floor {
		t.Errorf("%.0f req/s, want at least %.0f", sum.AchievedRPS, floor)
	}
	if sum.MaxInFlight != 256 {
		t.Errorf("max mode should keep every worker busy: max in flight %d", sum.MaxInFlight)
	}
	// Every worker busy is what a closed loop is, not a sign the client fell
	// behind, so it is not reported as saturation.
	if sum.Saturated {
		t.Error("max mode reported saturated")
	}
}

// BenchmarkRunMax is the end-to-end throughput of the load generator against
// a trivial httptest server in the same process: ns/op is the cost of one
// request (client and server side), req/s the rate. The target is at least
// 30k req/s at concurrency 256.
func BenchmarkRunMax(b *testing.B) {
	for _, conc := range []int{64, 256} {
		b.Run(fmt.Sprintf("concurrency=%d", conc), func(b *testing.B) {
			st := openStore(b)
			rec := seedRecording(b, st, numbered(1000, time.Millisecond)...)
			target := newStampTarget(b, 0)
			cfg := baseCfg(rec, target.URL)
			cfg.Mode = model.ReplayMax
			cfg.Loop = true
			cfg.MaxRequests = int64(max(b.N, 1))
			cfg.Concurrency = conc
			b.ReportAllocs()
			b.ResetTimer()
			sum, _, err := Run(context.Background(), st, cfg, RunOptions{Collector: newTestCollector(), TickCollector: true})
			b.StopTimer()
			if err != nil {
				b.Fatal(err)
			}
			// Start-up connection resets are expected (see TestMaxModeThroughput).
			if sum.Failed > max(sum.Sent/100, int64(conc)) {
				b.Fatalf("%d of %d requests failed", sum.Failed, sum.Sent)
			}
			b.ReportMetric(sum.AchievedRPS, "req/s")
		})
	}
}

// BenchmarkRunRate is the open-model scheduler at a fixed high rate: ns/op
// is wall time per scheduled request (1/rate), and lag-ms the worst delay
// between intended and actual send.
func BenchmarkRunRate(b *testing.B) {
	const rate = 10000
	st := openStore(b)
	rec := seedRecording(b, st, numbered(1000, time.Millisecond)...)
	target := newStampTarget(b, 0)
	cfg := baseCfg(rec, target.URL)
	cfg.Mode = model.ReplayRate
	cfg.Rate = rate
	cfg.MaxRequests = int64(max(b.N, 1))
	b.ReportAllocs()
	b.ResetTimer()
	sum, _, err := Run(context.Background(), st, cfg, RunOptions{Collector: newTestCollector(), TickCollector: true})
	b.StopTimer()
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(sum.AchievedRPS, "req/s")
	b.ReportMetric(sum.MaxLag.D().Seconds()*1000, "maxlag-ms")
}
