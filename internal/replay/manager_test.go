package replay

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/store"
)

type env struct {
	st      store.Store
	reg     *metrics.Registry
	m       *Manager
	mu      sync.Mutex
	changes []string // "<id>:<status>"
}

// newEnv builds a manager over a fresh store and a running registry.
func newEnv(t *testing.T, tweak ...func(*Manager)) *env {
	t.Helper()
	e := &env{st: openStore(t), reg: metrics.NewRegistry()}
	e.m = NewManager(Deps{Store: e.st, Metrics: e.reg, OnChange: func(j model.ReplayJob) {
		e.mu.Lock()
		e.changes = append(e.changes, j.ID+":"+string(j.Status))
		e.mu.Unlock()
	}})
	for _, f := range tweak {
		f(e.m)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.reg.Run(ctx); close(done) }()
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		if err := e.m.Shutdown(sctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		cancel()
		<-done
	})
	return e
}

func (e *env) events(id string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, c := range e.changes {
		if len(c) > len(id) && c[:len(id)] == id {
			out = append(out, c[len(id)+1:])
		}
	}
	return out
}

func (e *env) create(t *testing.T, cfg model.ReplayConfig) model.ReplayJob {
	t.Helper()
	j, err := e.m.Create(context.Background(), "job", cfg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return j
}

// slowRecording is 10s of traffic at 10 req/s: long enough to be stopped.
func slowRecording(t *testing.T, e *env) string {
	return seedRecording(t, e.st, numbered(100, 100*time.Millisecond)...)
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitDone(t *testing.T, e *env, id string) model.ReplayJob {
	t.Helper()
	var job model.ReplayJob
	waitFor(t, 20*time.Second, "job "+id+" to finish", func() bool {
		var err error
		job, err = e.m.Get(context.Background(), id)
		return err == nil && job.Status.Done()
	})
	return job
}

func TestManagerCreate(t *testing.T) {
	e := newEnv(t)
	rec := seedRecording(t, e.st, numbered(3, time.Millisecond)...)
	ctx := context.Background()

	t.Run("stores a pending job with defaults and a seed", func(t *testing.T) {
		job, err := e.m.Create(ctx, "  10x checkout ", model.ReplayConfig{RecordingID: rec, Target: "http://staging:8080"})
		if err != nil {
			t.Fatal(err)
		}
		if len(job.ID) < 5 || job.ID[:4] != "rpl_" || job.Name != "10x checkout" || job.Status != model.ReplayPending {
			t.Errorf("job: %+v", job)
		}
		c := job.Config
		if c.Mode != model.ReplayTimeline || c.Speed != 1 || c.Amplify != 1 || c.Concurrency != model.DefaultReplayConcurrency || c.Seed == 0 {
			t.Errorf("config not defaulted: %+v", c)
		}
		got, err := e.st.GetReplayJob(ctx, job.ID)
		if err != nil || got.Status != model.ReplayPending || got.Config.Seed != c.Seed {
			t.Errorf("stored job: %+v (%v)", got, err)
		}
		if ev := e.events(job.ID); len(ev) != 1 || ev[0] != "pending" {
			t.Errorf("OnChange: %v", ev)
		}
	})
	t.Run("keeps an explicit seed and draws different random ones", func(t *testing.T) {
		cfg := model.ReplayConfig{RecordingID: rec, Target: "http://x"}
		a, _ := e.m.Create(ctx, "a", cfg)
		b, _ := e.m.Create(ctx, "b", cfg)
		if a.Config.Seed == b.Config.Seed {
			t.Error("two jobs got the same random seed")
		}
		cfg.Seed = 77
		c, _ := e.m.Create(ctx, "c", cfg)
		if c.Config.Seed != 77 {
			t.Errorf("seed = %d, want 77", c.Config.Seed)
		}
	})
	t.Run("default name", func(t *testing.T) {
		job, err := e.m.Create(ctx, "", model.ReplayConfig{RecordingID: rec, Target: "http://x"})
		if err != nil || job.Name != "Replay of test" {
			t.Errorf("name = %q, %v", job.Name, err)
		}
	})
	t.Run("validation errors", func(t *testing.T) {
		_, err := e.m.Create(ctx, "bad", model.ReplayConfig{RecordingID: rec, Target: "ftp://x", Mode: model.ReplayRate, Amplify: 5})
		var ve *model.ValidationError
		if !errors.As(err, &ve) || !errors.Is(err, model.ErrValidation) || len(ve.Problems) < 3 {
			t.Errorf("err = %v", err)
		}
		// Nothing stored for a rejected config.
		jobs, _ := e.st.ListReplayJobs(ctx)
		for _, j := range jobs {
			if j.Name == "bad" {
				t.Error("a rejected job was stored")
			}
		}
	})
	t.Run("missing recording", func(t *testing.T) {
		_, err := e.m.Create(ctx, "x", model.ReplayConfig{RecordingID: "rec_nope", Target: "http://x"})
		var ve *model.ValidationError
		if !errors.Is(err, model.ErrNotFound) || errors.As(err, &ve) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
	t.Run("profile must exist and compile", func(t *testing.T) {
		var ve *model.ValidationError
		_, err := e.m.Create(ctx, "x", model.ReplayConfig{RecordingID: rec, Target: "http://x", ProfileID: "prof_nope"})
		if !errors.As(err, &ve) || !contains(ve.Problems[0], "profile_id") {
			t.Errorf("missing profile: %v", err)
		}
		bad, err := e.st.PutProfile(ctx, model.AnonymizationProfile{Name: "broken", Rules: []model.AnonymizationRule{
			{Name: "r", Enabled: true, Target: model.TargetRegex, Selector: "(", Strategy: model.StrategyRedact}}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.m.Create(ctx, "x", model.ReplayConfig{RecordingID: rec, Target: "http://x", ProfileID: bad.ID})
		if !errors.As(err, &ve) || !contains(ve.Problems[0], bad.ID) {
			t.Errorf("uncompilable profile: %v", err)
		}
		if _, err := e.m.Create(ctx, "x", model.ReplayConfig{RecordingID: rec, Target: "http://x", ProfileID: model.DefaultProfileID}); err != nil {
			t.Errorf("default profile: %v", err)
		}
	})
}

func TestManagerLifecycleStartGetStop(t *testing.T) {
	e := newEnv(t)
	rec := slowRecording(t, e)
	target := newRecorder(t, nil)
	ctx := context.Background()
	job := e.create(t, baseCfg(rec, target.URL))

	if err := e.m.Start(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	got, err := e.m.Get(ctx, job.ID)
	// Get carries the live series (empty this early); the listings do not.
	if err != nil || got.Status != model.ReplayRunning || got.StartedAt == nil || got.EndedAt != nil || got.Summary == nil {
		t.Fatalf("running job: %+v %v", got, err)
	}
	// Starting a running job is a conflict.
	if err := e.m.Start(ctx, job.ID); !errors.Is(err, model.ErrConflict) {
		t.Errorf("second Start: %v, want ErrConflict", err)
	}

	waitFor(t, 5*time.Second, "live summary to advance", func() bool {
		j, _ := e.m.Get(ctx, job.ID)
		return j.Summary != nil && j.Summary.Completed >= 4
	})
	live, _ := e.m.Get(ctx, job.ID)
	if live.Summary.Planned != 100 || live.Summary.Elapsed <= 0 || live.Summary.AchievedRPS <= 0 {
		t.Errorf("live summary: %+v", live.Summary)
	}
	list, err := e.m.List(ctx)
	if err != nil || len(list) != 1 || list[0].Status != model.ReplayRunning || list[0].Summary == nil || list[0].Summary.Sent == 0 || list[0].Series != nil {
		t.Errorf("List: %+v %v", list, err)
	}
	if run := e.m.Running(); len(run) != 1 || run[0].ID != job.ID || run[0].Summary == nil || run[0].Series != nil {
		t.Errorf("Running: %+v", run)
	}
	// The store holds the running state, not the live numbers.
	stored, _ := e.st.GetReplayJob(ctx, job.ID)
	if stored.Status != model.ReplayRunning || stored.Summary != nil {
		t.Errorf("stored while running: %+v", stored)
	}

	// Live series comes from the run (one point per second).
	waitFor(t, 5*time.Second, "a live series point", func() bool {
		pts, err := e.m.Series(ctx, job.ID, 0)
		return err == nil && len(pts) > 0
	})

	if err := e.m.Stop(job.ID); err != nil {
		t.Fatal(err)
	}
	done, err := e.m.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != model.ReplayStopped || done.EndedAt == nil || done.Error != "" {
		t.Fatalf("stopped job: status=%s ended=%v err=%q", done.Status, done.EndedAt, done.Error)
	}
	s := done.Summary
	if s == nil || s.Sent < 4 || s.Sent >= 100 || s.Planned != 100 || s.Failed != 0 {
		t.Errorf("partial summary: %+v", s)
	}
	if len(done.Series) == 0 {
		t.Error("a stopped job keeps the series of what it did")
	}
	if got := int64(target.count()); got != s.Sent {
		t.Errorf("target saw %d requests, summary says %d sent", got, s.Sent)
	}
	if len(e.m.Running()) != 0 {
		t.Error("Running must be empty after Stop")
	}
	if evs := e.events(job.ID); len(evs) != 3 || evs[0] != "pending" || evs[1] != "running" || evs[2] != "stopped" {
		t.Errorf("OnChange: %v", evs)
	}
	// Stop on a job that is not running is a no-op.
	if err := e.m.Stop(job.ID); err != nil {
		t.Errorf("Stop on a stopped job: %v", err)
	}
	if err := e.m.Stop("rpl_unknown"); err != nil {
		t.Errorf("Stop on an unknown job: %v", err)
	}

	// Stored series match what Series returns once finished, and filter.
	pts, err := e.m.Series(ctx, job.ID, 0)
	if err != nil || len(pts) != len(done.Series) {
		t.Errorf("Series after stop: %d points, %v; job has %d", len(pts), err, len(done.Series))
	}
	last := done.Series[len(done.Series)-1]
	if pts, _ := e.m.Series(ctx, job.ID, last.T); len(pts) != 1 || pts[0].T != last.T {
		t.Errorf("since filter: %+v", pts)
	}
	if pts, err := e.m.Series(ctx, job.ID, last.T+1); err != nil || pts == nil || len(pts) != 0 {
		t.Errorf("since after the end: %#v %v", pts, err)
	}
}

func TestManagerCompletionPersistsSummaryAndSeries(t *testing.T) {
	e := newEnv(t, func(m *Manager) { m.collectorTTL = 300 * time.Millisecond })
	rec := seedRecording(t, e.st, numbered(10, 20*time.Millisecond)...)
	target := newRecorder(t, nil)
	ctx := context.Background()
	cfg := baseCfg(rec, target.URL)
	cfg.CompareStatus = true
	job := e.create(t, cfg)
	if err := e.m.Start(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	done := waitDone(t, e, job.ID)
	if done.Status != model.ReplayCompleted || done.Error != "" || done.StartedAt == nil || done.EndedAt == nil || done.EndedAt.Before(*done.StartedAt) {
		t.Fatalf("job: %+v", done)
	}
	s := done.Summary
	if s == nil || s.Sent != 10 || s.Completed != 10 || s.Planned != 10 || s.StatusCounts["200"] != 10 || s.Latency.Count != 10 {
		t.Fatalf("summary: %+v", s)
	}
	var total int64
	for _, p := range done.Series {
		total += p.Requests
	}
	if total != 10 {
		t.Errorf("the series covers %d of 10 requests: the last partial second must be included", total)
	}
	stored, err := e.st.GetReplayJob(ctx, job.ID)
	if err != nil || stored.Status != model.ReplayCompleted || stored.Summary == nil || stored.Summary.Sent != 10 || len(stored.Series) != len(done.Series) {
		t.Errorf("stored: %+v %v", stored, err)
	}
	if evs := e.events(job.ID); len(evs) != 3 || evs[2] != "completed" {
		t.Errorf("OnChange: %v", evs)
	}
	for _, r := range target.requests() {
		if r.header.Get("X-Mirage-Replay") != job.ID {
			t.Errorf("X-Mirage-Replay = %q, want the job id", r.header.Get("X-Mirage-Replay"))
		}
	}

	// The collector lives on for the dashboard, then goes away.
	name := metrics.ReplayCollectorName(job.ID)
	col, ok := e.reg.Get(name)
	if !ok || col.Snapshot().Requests != 10 || col.Kind() != model.KindReplay || col.ID() != job.ID {
		t.Fatalf("collector after finish: ok=%v", ok)
	}
	waitFor(t, 5*time.Second, "collector removal", func() bool { _, ok := e.reg.Get(name); return !ok })

	// A re-run replaces the results and starts the collector afresh.
	if err := e.m.Start(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	again := waitDone(t, e, job.ID)
	if again.Status != model.ReplayCompleted || again.Summary.Sent != 10 || !again.StartedAt.After(*done.StartedAt) {
		t.Errorf("re-run: %+v", again)
	}
	col, ok = e.reg.Get(name)
	if !ok || col.Snapshot().Requests != 10 {
		t.Errorf("collector was not reset for the re-run: ok=%v", ok)
	}
	if target.count() != 20 {
		t.Errorf("target saw %d requests over two runs", target.count())
	}
}

func TestManagerRerunWithinTTLKeepsTheCollector(t *testing.T) {
	// A re-run must cancel the pending removal of the previous run's
	// collector; otherwise the live run would vanish from the dashboard.
	e := newEnv(t, func(m *Manager) { m.collectorTTL = 400 * time.Millisecond; m.finalWait = 0 })
	rec := seedRecording(t, e.st, numbered(3, time.Millisecond)...)
	target := newRecorder(t, nil)
	job := e.create(t, baseCfg(rec, target.URL))
	ctx := context.Background()
	name := metrics.ReplayCollectorName(job.ID)

	_ = e.m.Start(ctx, job.ID)
	waitDone(t, e, job.ID)
	time.Sleep(200 * time.Millisecond)
	// Start the second run just before the first removal would fire, with a
	// recording slow enough to still be running when it would have.
	slow := seedRecording(t, e.st, numbered(10, 100*time.Millisecond)...)
	j2 := e.create(t, baseCfg(slow, target.URL))
	_ = e.m.Start(ctx, j2.ID)
	waitDone(t, e, j2.ID)
	if _, ok := e.reg.Get(metrics.ReplayCollectorName(j2.ID)); !ok {
		t.Fatal("a fresh job's collector must be registered")
	}
	waitFor(t, 5*time.Second, "first collector removal", func() bool { _, ok := e.reg.Get(name); return !ok })

	// Same job again within the TTL: the old timer must not remove it.
	_ = e.m.Start(ctx, job.ID)
	waitDone(t, e, job.ID)
	time.Sleep(250 * time.Millisecond)
	if _, ok := e.reg.Get(name); !ok {
		t.Error("collector vanished before its TTL elapsed after the re-run")
	}
}

func TestManagerStartErrors(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.m.Start(ctx, "rpl_nope"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown job: %v", err)
	}
	if _, err := e.m.Get(ctx, "rpl_nope"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("Get unknown: %v", err)
	}
	if _, err := e.m.Series(ctx, "rpl_nope", 0); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("Series unknown: %v", err)
	}
}

func TestManagerJobThatCannotLoadFails(t *testing.T) {
	e := newEnv(t)
	rec := seedRecording(t, e.st, numbered(3, time.Millisecond)...)
	target := newRecorder(t, nil)
	ctx := context.Background()
	job := e.create(t, baseCfg(rec, target.URL))
	if err := e.st.DeleteRecording(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Start(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	done := waitDone(t, e, job.ID)
	if done.Status != model.ReplayFailed || !contains(done.Error, "not found") || done.Summary != nil || done.EndedAt == nil {
		t.Errorf("job: status=%s err=%q summary=%v", done.Status, done.Error, done.Summary)
	}
	if evs := e.events(job.ID); evs[len(evs)-1] != "failed" {
		t.Errorf("OnChange: %v", evs)
	}
	if target.count() != 0 {
		t.Error("nothing should be sent")
	}
}

func TestManagerUnreachableTargetCompletesWithFailures(t *testing.T) {
	// A dead target is a result, not a job failure.
	e := newEnv(t)
	rec := seedRecording(t, e.st, numbered(5, time.Millisecond)...)
	dead := newRecorder(t, nil)
	url := dead.URL
	dead.Close()
	job := e.create(t, baseCfg(rec, url))
	_ = e.m.Start(context.Background(), job.ID)
	done := waitDone(t, e, job.ID)
	if done.Status != model.ReplayCompleted || done.Summary.Failed != 5 || done.Summary.ErrorRate != 1 {
		t.Errorf("job: %s %+v", done.Status, done.Summary)
	}
}

func TestManagerDeleteWhileRunning(t *testing.T) {
	e := newEnv(t)
	rec := slowRecording(t, e)
	target := newRecorder(t, nil)
	ctx := context.Background()
	job := e.create(t, baseCfg(rec, target.URL))
	other := e.create(t, baseCfg(rec, target.URL))
	_ = e.m.Start(ctx, job.ID)
	waitFor(t, 5*time.Second, "first requests", func() bool { return target.count() >= 3 })
	name := metrics.ReplayCollectorName(job.ID)
	if _, ok := e.reg.Get(name); !ok {
		t.Fatal("collector missing while running")
	}

	if err := e.m.Delete(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Get(ctx, job.ID); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("Get after Delete: %v", err)
	}
	if _, ok := e.reg.Get(name); ok {
		t.Error("the collector must be removed at once by Delete")
	}
	if len(e.m.Running()) != 0 {
		t.Error("deleted job still running")
	}
	list, _ := e.m.List(ctx)
	if len(list) != 1 || list[0].ID != other.ID {
		t.Errorf("List after Delete: %+v", list)
	}
	n := target.count()
	time.Sleep(400 * time.Millisecond)
	if target.count() != n {
		t.Errorf("requests kept flowing after Delete: %d -> %d", n, target.count())
	}
	// The job must not be resurrected by the finishing goroutine.
	if _, err := e.st.GetReplayJob(ctx, job.ID); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("deleted job is back in the store: %v", err)
	}
	if evs := e.events(job.ID); evs[len(evs)-1] == "stopped" || evs[len(evs)-1] == "completed" {
		t.Errorf("a deleted job must not report a result: %v", evs)
	}
	if err := e.m.Delete(ctx, job.ID); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("second Delete: %v", err)
	}
}

func TestManagerDeleteFinishedJob(t *testing.T) {
	e := newEnv(t, func(m *Manager) { m.finalWait = 0 })
	rec := seedRecording(t, e.st, numbered(2, time.Millisecond)...)
	target := newRecorder(t, nil)
	job := e.create(t, baseCfg(rec, target.URL))
	_ = e.m.Start(context.Background(), job.ID)
	waitDone(t, e, job.ID)
	if err := e.m.Delete(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.reg.Get(metrics.ReplayCollectorName(job.ID)); ok {
		t.Error("collector should be gone")
	}
	if _, err := e.m.Get(context.Background(), job.ID); !errors.Is(err, model.ErrNotFound) {
		t.Error("job should be gone")
	}
}

func TestManagerStartDeleteRace(t *testing.T) {
	// Whatever order Start and Delete land in, a deleted job never comes back.
	e := newEnv(t, func(m *Manager) { m.finalWait = 0 })
	rec := seedRecording(t, e.st, numbered(20, 5*time.Millisecond)...)
	target := newRecorder(t, nil)
	for i := 0; i < 20; i++ {
		job := e.create(t, baseCfg(rec, target.URL))
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = e.m.Start(context.Background(), job.ID) }()
		go func() { defer wg.Done(); _ = e.m.Delete(context.Background(), job.ID) }()
		wg.Wait()
		waitFor(t, 5*time.Second, "no live runs", func() bool { return len(e.m.Running()) == 0 })
		if _, err := e.st.GetReplayJob(context.Background(), job.ID); !errors.Is(err, model.ErrNotFound) {
			// Delete may have lost the race to Start only if Start was first
			// and Delete then stopped it: either way the row must be gone.
			t.Fatalf("iteration %d: job survived Delete: %v", i, err)
		}
	}
}

func TestManagerRecover(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	rec := seedRecording(t, e.st, numbered(2, time.Millisecond)...)
	put := func(name string, st model.ReplayStatus) model.ReplayJob {
		now := time.Now()
		j, err := e.st.PutReplayJob(ctx, model.ReplayJob{Name: name, Status: st, StartedAt: &now,
			Config: baseCfg(rec, "http://127.0.0.1:1").WithDefaults()})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	running := put("orphan", model.ReplayRunning)
	pending := put("pending", model.ReplayPending)
	completed := put("done", model.ReplayCompleted)
	if err := e.m.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := e.st.GetReplayJob(ctx, running.ID)
	if got.Status != model.ReplayFailed || got.Error != "interrupted: mirage restarted" || got.EndedAt == nil {
		t.Errorf("orphan: %+v", got)
	}
	for _, j := range []model.ReplayJob{pending, completed} {
		now, _ := e.st.GetReplayJob(ctx, j.ID)
		if now.Status != j.Status {
			t.Errorf("job %s changed from %s to %s", j.Name, j.Status, now.Status)
		}
	}
	if evs := e.events(running.ID); len(evs) != 1 || evs[0] != "failed" {
		t.Errorf("OnChange: %v", evs)
	}
	// Idempotent.
	if err := e.m.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if evs := e.events(running.ID); len(evs) != 1 {
		t.Errorf("second Recover notified again: %v", evs)
	}
}

func TestManagerRecoverLeavesLiveJobsAlone(t *testing.T) {
	e := newEnv(t)
	rec := slowRecording(t, e)
	target := newRecorder(t, nil)
	job := e.create(t, baseCfg(rec, target.URL))
	_ = e.m.Start(context.Background(), job.ID)
	if err := e.m.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := e.m.Get(context.Background(), job.ID)
	if got.Status != model.ReplayRunning {
		t.Errorf("a job running in this process was recovered: %s", got.Status)
	}
	_ = e.m.Stop(job.ID)
}

func TestManagerConcurrentAccessWhileRunning(t *testing.T) {
	e := newEnv(t)
	rec := slowRecording(t, e)
	target := newRecorder(t, nil)
	ctx := context.Background()
	job := e.create(t, baseCfg(rec, target.URL))
	_ = e.m.Start(ctx, job.ID)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var reads atomic.Int64
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				switch i % 4 {
				case 0:
					j, err := e.m.Get(ctx, job.ID)
					if err != nil || j.Summary == nil {
						t.Errorf("Get: %v", err)
						return
					}
					// Callers may modify what they get (the API masks headers).
					j.Config.SetHeaders = map[string]string{"X": "y"}
					j.Summary.Sent = -1
				case 1:
					if _, err := e.m.List(ctx); err != nil {
						t.Errorf("List: %v", err)
						return
					}
				case 2:
					if _, err := e.m.Series(ctx, job.ID, 0); err != nil {
						t.Errorf("Series: %v", err)
						return
					}
				case 3:
					_ = e.m.Running()
					if _, err := e.m.Create(ctx, "extra", baseCfg(rec, target.URL)); err != nil {
						t.Errorf("Create: %v", err)
						return
					}
				}
				reads.Add(1)
			}
		}(i)
	}
	time.Sleep(1300 * time.Millisecond)
	close(stop)
	wg.Wait()
	if reads.Load() < 100 {
		t.Errorf("only %d reads completed", reads.Load())
	}
	if err := e.m.Stop(job.ID); err != nil {
		t.Fatal(err)
	}
	done, _ := e.m.Get(ctx, job.ID)
	if done.Status != model.ReplayStopped || done.Summary.Sent < 1 || done.Summary.Sent == -1 || len(done.Config.SetHeaders) != 0 {
		t.Errorf("job after concurrent access: %s %+v %v", done.Status, done.Summary, done.Config.SetHeaders)
	}
}

func TestManagerShutdownStopsAndSavesRunningJobs(t *testing.T) {
	e := newEnv(t)
	rec := slowRecording(t, e)
	target := newRecorder(t, nil)
	ctx := context.Background()
	a, b := e.create(t, baseCfg(rec, target.URL)), e.create(t, baseCfg(rec, target.URL))
	_ = e.m.Start(ctx, a.ID)
	_ = e.m.Start(ctx, b.ID)
	waitFor(t, 5*time.Second, "traffic", func() bool { return target.count() >= 6 })

	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	if err := e.m.Shutdown(sctx); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Shutdown took %v", d)
	}
	for _, id := range []string{a.ID, b.ID} {
		j, err := e.st.GetReplayJob(ctx, id)
		if err != nil || j.Status != model.ReplayStopped || j.Summary == nil || j.Summary.Sent == 0 || j.EndedAt == nil {
			t.Errorf("job %s after Shutdown: %+v (%v)", id, j, err)
		}
	}
	if len(e.m.Running()) != 0 {
		t.Error("jobs still running after Shutdown")
	}
	if err := e.m.Start(ctx, a.ID); !errors.Is(err, ErrShutdown) {
		t.Errorf("Start after Shutdown: %v", err)
	}
	// Shutdown is idempotent.
	if err := e.m.Shutdown(ctx); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
	// ... and it leaves no collector behind to be removed later.
	for _, id := range []string{a.ID, b.ID} {
		if _, ok := e.reg.Get(metrics.ReplayCollectorName(id)); ok {
			t.Errorf("collector of %s survived Shutdown", id)
		}
	}
}

func TestManagerShutdownSkipsTheFinalSecondWait(t *testing.T) {
	// A registry nobody ticks never closes the last second, so a finishing
	// job would wait for it (up to finalWait). Shutdown must not.
	st := openStore(t)
	m := NewManager(Deps{Store: st, Metrics: metrics.NewRegistry()})
	rec := seedRecording(t, st, numbered(100, 50*time.Millisecond)...)
	target := newRecorder(t, nil)
	ctx := context.Background()
	job, err := m.Create(ctx, "x", baseCfg(rec, target.URL))
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Start(ctx, job.ID)
	waitFor(t, 5*time.Second, "requests", func() bool { return target.count() >= 3 })
	start := time.Now()
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := m.Shutdown(sctx); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 800*time.Millisecond {
		t.Errorf("Shutdown took %v; it must not wait for the registry", d)
	}
	got, _ := st.GetReplayJob(ctx, job.ID)
	if got.Status != model.ReplayStopped {
		t.Errorf("status %s", got.Status)
	}
}

func TestManagerStopWaitsForTheRegistryToCloseTheLastSecond(t *testing.T) {
	// Stop is not a shutdown: the job keeps the series of its last second,
	// which costs up to a second while the registry catches up.
	e := newEnv(t)
	rec := slowRecording(t, e)
	target := newRecorder(t, nil)
	job := e.create(t, baseCfg(rec, target.URL))
	_ = e.m.Start(context.Background(), job.ID)
	waitFor(t, 5*time.Second, "requests", func() bool { return target.count() >= 3 })
	if err := e.m.Stop(job.ID); err != nil {
		t.Fatal(err)
	}
	done, _ := e.m.Get(context.Background(), job.ID)
	var total int64
	for _, p := range done.Series {
		total += p.Requests
	}
	if total != done.Summary.Completed+done.Summary.Failed {
		t.Errorf("series covers %d requests, the summary %d", total, done.Summary.Completed+done.Summary.Failed)
	}
}

func TestManagerWithoutRegistry(t *testing.T) {
	st := openStore(t)
	m := NewManager(Deps{Store: st})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	rec := seedRecording(t, st, numbered(8, 10*time.Millisecond)...)
	target := newRecorder(t, nil)
	ctx := context.Background()
	job, err := m.Create(ctx, "solo", baseCfg(rec, target.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	var done model.ReplayJob
	waitFor(t, 10*time.Second, "completion", func() bool {
		done, _ = m.Get(ctx, job.ID)
		return done.Status.Done()
	})
	var total int64
	for _, p := range done.Series {
		total += p.Requests
	}
	if done.Status != model.ReplayCompleted || done.Summary.Sent != 8 || total != 8 {
		t.Errorf("job: %s sent=%d series total=%d", done.Status, done.Summary.Sent, total)
	}
}

func TestManagerListIsNewestFirstWithLiveSummaries(t *testing.T) {
	e := newEnv(t)
	rec := slowRecording(t, e)
	target := newRecorder(t, nil)
	ctx := context.Background()
	first := e.create(t, baseCfg(rec, target.URL))
	time.Sleep(5 * time.Millisecond)
	second := e.create(t, baseCfg(rec, target.URL))
	_ = e.m.Start(ctx, second.ID)
	waitFor(t, 5*time.Second, "traffic", func() bool { return target.count() >= 2 })
	list, err := e.m.List(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("List: %v %v", list, err)
	}
	if list[0].ID != second.ID || list[1].ID != first.ID {
		t.Errorf("order: %s, %s; want newest first", list[0].ID, list[1].ID)
	}
	if list[0].Status != model.ReplayRunning || list[0].Summary == nil || list[0].Summary.Sent < 2 {
		t.Errorf("running entry: %+v", list[0])
	}
	if list[1].Status != model.ReplayPending || list[1].Summary != nil {
		t.Errorf("pending entry: %+v", list[1])
	}
	_ = e.m.Stop(second.ID)
}

func TestManagerRunsManyJobsAtOnce(t *testing.T) {
	e := newEnv(t, func(m *Manager) { m.finalWait = 0 })
	rec := seedRecording(t, e.st, numbered(30, 10*time.Millisecond)...)
	target := newRecorder(t, nil)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 5; i++ {
		j := e.create(t, baseCfg(rec, target.URL))
		ids = append(ids, j.ID)
		if err := e.m.Start(ctx, j.ID); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(e.m.Running()); n < 1 {
		t.Errorf("Running() = %d", n)
	}
	for _, id := range ids {
		j := waitDone(t, e, id)
		if j.Status != model.ReplayCompleted || j.Summary.Sent != 30 {
			t.Errorf("job %s: %s %+v", id, j.Status, j.Summary)
		}
	}
	if target.count() != 150 {
		t.Errorf("target saw %d requests, want 150", target.count())
	}
}

func TestManagerStopWaitsForTheSave(t *testing.T) {
	e := newEnv(t)
	rec := slowRecording(t, e)
	target := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {})
	job := e.create(t, baseCfg(rec, target.URL))
	_ = e.m.Start(context.Background(), job.ID)
	waitFor(t, 5*time.Second, "traffic", func() bool { return target.count() >= 2 })
	if err := e.m.Stop(job.ID); err != nil {
		t.Fatal(err)
	}
	// Stop has returned: the stopped job is already in the store.
	stored, err := e.st.GetReplayJob(context.Background(), job.ID)
	if err != nil || stored.Status != model.ReplayStopped {
		t.Errorf("stored after Stop: %v %v", stored.Status, err)
	}
}

func TestManagerDefaults(t *testing.T) {
	m := NewManager(Deps{Store: openStore(t)})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	if m.collectorTTL != 60*time.Second {
		t.Errorf("collectors are removed %v after a job finishes, want 60s", m.collectorTTL)
	}
	if m.finalWait <= 0 {
		t.Error("finalWait must be positive")
	}
}

func TestManagerOnChangeIsOrdered(t *testing.T) {
	// Even for jobs that finish at once, observers hear pending, running,
	// then the result, never the result first.
	e := newEnv(t, func(m *Manager) { m.finalWait = 0 })
	rec := seedRecording(t, e.st, numbered(2, time.Millisecond)...)
	target := newRecorder(t, nil)
	dead := newRecorder(t, nil)
	deadURL := dead.URL
	dead.Close()
	var ids []string
	for i := 0; i < 12; i++ {
		url := target.URL
		if i%3 == 0 {
			url = deadURL
		}
		j := e.create(t, baseCfg(rec, url))
		ids = append(ids, j.ID)
		if err := e.m.Start(context.Background(), j.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		waitDone(t, e, id)
		evs := e.events(id)
		if len(evs) != 3 || evs[0] != "pending" || evs[1] != "running" || evs[2] != "completed" {
			t.Errorf("job %s events: %v", id, evs)
		}
	}
}
