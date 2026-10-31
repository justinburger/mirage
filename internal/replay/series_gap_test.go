package replay

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
)

// A replay's series is one point per second for as long as the job runs: the
// charts, the stored result and the API all index it by T and assume the next
// point is one second on (the collector zero-fills seconds that were not
// ticked for exactly that reason). A tick that arrives late, as it does on a
// stalled or heavily loaded machine, must therefore leave the runner's series
// as gapless as the collector's own ring.

// seriesTimes returns the T of every point, in order.
func seriesTimes(pts []model.SeriesPoint) []int64 {
	out := make([]int64, len(pts))
	for i, p := range pts {
		out[i] = p.T
	}
	return out
}

// requireConsecutive fails unless every point is exactly one second after the
// one before it.
func requireConsecutive(t *testing.T, pts []model.SeriesPoint) {
	t.Helper()
	for i := 1; i < len(pts); i++ {
		if pts[i].T != pts[i-1].T+1000 {
			t.Errorf("point %d at %d does not follow %d by one second (all: %v)", i, pts[i].T, pts[i-1].T, seriesTimes(pts))
			return
		}
	}
}

// observeN records n completed requests on col.
func observeN(col *metrics.Collector, n int) {
	for range n {
		col.Observe(metrics.Observation{
			Endpoint: "GET /x", Status: 200, Duration: 5 * time.Millisecond, Corrected: 5 * time.Millisecond,
		})
	}
}

// TestCollectKeepsTheSecondsALateTickSkipped drives the runner's collection
// step with an explicit clock instead of waiting for real timers: the monitor
// wakes late, so the tick that follows is for a boundary two seconds after the
// last one. The skipped second must still be in the run's series, with no
// requests, in both ways a run can be ticked: by itself (CLI, a Manager
// without a registry) and by a registry that it merely reads.
func TestCollectKeepsTheSecondsALateTickSkipped(t *testing.T) {
	base := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	at := func(sec int64) time.Time { return base.Add(time.Duration(sec) * time.Second) }
	wantT := func(sec ...int64) []int64 {
		out := make([]int64, len(sec))
		for i, s := range sec {
			out[i] = at(s).UnixMilli()
		}
		return out
	}

	for _, tt := range []struct {
		name string
		tick bool // the run ticks its own collector
	}{
		{"ticked by the run", true},
		{"ticked by a registry", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, err := newRunner(openStore(t), baseCfg("rec", "http://t"), RunOptions{Collector: newTestCollector(), TickCollector: tt.tick})
			if err != nil {
				t.Fatal(err)
			}
			r.startMs = base.UnixMilli()
			r.lastT = r.startMs - 1
			// close stands in for one monitor wake-up at boundary b (seconds
			// after base): the run ticks, or a registry does and the run reads.
			closeAt := func(b int64) {
				if !tt.tick {
					r.col.Tick(at(b))
				}
				r.collect(at(b))
			}

			observeN(r.col, 5)
			closeAt(1) // closes second 0, on time
			observeN(r.col, 7)
			closeAt(3) // the wake-up for boundary 2 was a second late: second 1 was never ticked
			observeN(r.col, 2)
			closeAt(4) // back on time

			got := r.series.all()
			if want := wantT(0, 1, 2, 3); !slices.Equal(seriesTimes(got), want) {
				t.Fatalf("series stamps = %v, want %v (second 1 is missing)", seriesTimes(got), want)
			}
			requireConsecutive(t, got)
			for i, want := range []int64{5, 0, 7, 2} {
				if got[i].Requests != want {
					t.Errorf("second %d has %d requests, want %d (%+v)", i, got[i].Requests, want, got)
				}
			}
		})
	}
}

// TestRunSeriesHasNoGapWhenTheMonitorStalls makes the monitor goroutine late
// for real, deterministically: it calls Progress itself, so a Progress that
// blocks for longer than a second is a machine that paused the monitor for
// that long. Whatever phase of the second the run starts in, the second whose
// boundary the monitor slept through must come back as a point.
func TestRunSeriesHasNoGapWhenTheMonitorStalls(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(23, 100*time.Millisecond)...) // 2.2s, longer than any boundary + 1s
	target := newRecorder(t, nil)
	var calls atomic.Int32
	sum, series, err := Run(context.Background(), st, baseCfg(rec, target.URL), RunOptions{
		Collector: newTestCollector(), TickCollector: true,
		Progress: func(model.ReplaySummary) {
			if calls.Add(1) == 1 {
				time.Sleep(1100 * time.Millisecond)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() == 0 {
		t.Fatal("Progress was never called")
	}
	if len(series) < 3 {
		t.Fatalf("%d series points for a 2.2s run: %v", len(series), seriesTimes(series))
	}
	requireConsecutive(t, series)
	var total int64
	for _, p := range series {
		total += p.Requests
	}
	if total != sum.Sent {
		t.Errorf("series requests sum %d, want %d", total, sum.Sent)
	}
}

// TestMonitorFirstPointIsTheRunsFirstSecond: the monitor goroutine is started
// after the run's clock, and on a loaded machine it can first get to run in a
// later second than the run began in. Its first boundary comes from the start
// of the run, so the first point is still the run's own first second (a
// monitor that looked at the clock instead would begin a second or more in).
func TestMonitorFirstPointIsTheRunsFirstSecond(t *testing.T) {
	r, err := newRunner(openStore(t), baseCfg("rec", "http://t"), RunOptions{Collector: newTestCollector(), TickCollector: true})
	if err != nil {
		t.Fatal(err)
	}
	r.start = time.Now().Add(-1500 * time.Millisecond) // the monitor got going 1.5s into the run
	r.startMs = r.start.UnixMilli() / 1000 * 1000
	r.lastT = r.startMs - 1

	stop, done := make(chan struct{}), make(chan struct{})
	go r.monitor(stop, done)
	defer func() { close(stop); <-done }()
	waitFor(t, 3*time.Second, "the first point", func() bool { return len(r.seriesSince(0)) > 0 })
	if got := r.seriesSince(0)[0].T; got != r.startMs {
		t.Errorf("first point at %d, want the run's first second %d", got, r.startMs)
	}
}
