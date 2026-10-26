package replay

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/store"
)

var errInjected = errors.New("injected store failure")

// flakyStore fails the replay-job writes and listings it is told to.
type flakyStore struct {
	store.Store
	failPut  atomic.Bool
	failList atomic.Bool
}

func (f *flakyStore) PutReplayJob(ctx context.Context, j model.ReplayJob) (model.ReplayJob, error) {
	if f.failPut.Load() {
		return model.ReplayJob{}, errInjected
	}
	return f.Store.PutReplayJob(ctx, j)
}

func (f *flakyStore) ListReplayJobs(ctx context.Context) ([]model.ReplayJob, error) {
	if f.failList.Load() {
		return nil, errInjected
	}
	return f.Store.ListReplayJobs(ctx)
}

func TestManagerStoreFailures(t *testing.T) {
	fs := &flakyStore{Store: openStore(t)}
	m := NewManager(Deps{Store: fs})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	ctx := context.Background()
	rec := seedRecording(t, fs, numbered(3, time.Millisecond)...)
	target := newRecorder(t, nil)

	job, err := m.Create(ctx, "x", baseCfg(rec, target.URL))
	if err != nil {
		t.Fatal(err)
	}

	// A start that cannot persist its running state leaves nothing behind.
	fs.failPut.Store(true)
	if err := m.Start(ctx, job.ID); !errors.Is(err, errInjected) {
		t.Fatalf("Start: %v, want the store error", err)
	}
	if len(m.Running()) != 0 {
		t.Error("a job whose start failed must not be registered as running")
	}
	if _, err := m.Create(ctx, "y", baseCfg(rec, target.URL)); !errors.Is(err, errInjected) {
		t.Errorf("Create: %v", err)
	}
	fs.failPut.Store(false)
	if err := m.Start(ctx, job.ID); err != nil {
		t.Fatalf("Start after the store recovered: %v", err)
	}
	waitFor(t, 10*time.Second, "the job to finish", func() bool { return len(m.Running()) == 0 })

	// Listing and recovery report the store's failure.
	fs.failList.Store(true)
	if _, err := m.List(ctx); !errors.Is(err, errInjected) {
		t.Errorf("List: %v", err)
	}
	if err := m.Recover(ctx); !errors.Is(err, errInjected) {
		t.Errorf("Recover: %v", err)
	}
	fs.failList.Store(false)

	// A result that cannot be saved is logged and the job still ends.
	j2, _ := m.Create(ctx, "z", baseCfg(rec, target.URL))
	if err := m.Start(ctx, j2.ID); err != nil {
		t.Fatal(err)
	}
	fs.failPut.Store(true)
	waitFor(t, 10*time.Second, "the job to end despite the store", func() bool { return len(m.Running()) == 0 })
	fs.failPut.Store(false)
}

func TestRunnerAddPointFiltersStalePoints(t *testing.T) {
	r, err := newRunner(openStore(t), baseCfg("rec", "http://x"), RunOptions{Collector: newTestCollector()})
	if err != nil {
		t.Fatal(err)
	}
	r.startMs, r.lastT = 10_000, 9_999
	for _, p := range []model.SeriesPoint{
		{T: 8_000, Requests: 1}, // before the run started (a registry's idle ticks)
		{T: 10_000, Requests: 2},
		{T: 10_000, Requests: 3}, // seen twice
		{T: 11_000, Requests: 4},
		{T: 9_000, Requests: 5}, // out of order
	} {
		r.addPoint(p)
	}
	got := r.series.all()
	if len(got) != 2 || got[0].Requests != 2 || got[1].Requests != 4 {
		t.Errorf("series = %+v", got)
	}
}

func TestRunCancelWhileEveryWorkerIsBusy(t *testing.T) {
	// One slot, held by a request that never returns: the scheduler blocks
	// handing over the next request, and cancelling must free it.
	st := openStore(t)
	rec := seedRecording(t, st, numbered(10, time.Millisecond)...)
	target := newRecorder(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	cfg := baseCfg(rec, target.URL)
	cfg.Concurrency = 1
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	sum, _, err := Run(ctx, st, cfg, RunOptions{Collector: newTestCollector(), TickCollector: true})
	if !errors.Is(err, context.Canceled) || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
	if sum.Sent != 1 || !sum.Saturated || sum.MaxInFlight != 1 || sum.Failed != 0 {
		t.Errorf("sent=%d saturated=%v max in flight=%d failed=%d", sum.Sent, sum.Saturated, sum.MaxInFlight, sum.Failed)
	}
}

func TestRunOnASingleProc(t *testing.T) {
	// With one P the scheduler must not spin: sleeping keeps the workers (and
	// the target in this process) running.
	prev := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prev)
	st := openStore(t)
	rec := seedRecording(t, st, numbered(30, 3*time.Millisecond)...)
	target := newRecorder(t, nil)
	sum, _ := mustRun(t, st, baseCfg(rec, target.URL))
	if sum.Completed != 30 || sum.Failed != 0 {
		t.Errorf("completed %d failed %d", sum.Completed, sum.Failed)
	}
	if e := sum.Elapsed.D(); e < 85*time.Millisecond || e > time.Second {
		t.Errorf("elapsed %v for 87ms of traffic", e)
	}
}

func TestRunWithAnEmptyHeaderSet(t *testing.T) {
	// A recorded request with no headers at all still sends (a nil header map
	// would make the transport refuse it).
	st := openStore(t)
	rec := seedRecording(t, st, rx{path: "/bare"})
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, target.URL)
	cfg.RemoveHeaders = []string{"X-Mirage-Replay"}
	sum, _ := mustRun(t, st, cfg)
	if sum.Completed != 1 {
		t.Errorf("completed %d failed %d", sum.Completed, sum.Failed)
	}
}
