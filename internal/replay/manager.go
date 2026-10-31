package replay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/store"
)

// ErrShutdown is returned by Start once the manager has been shut down.
var ErrShutdown = errors.New("replay: manager is shut down")

const (
	// collectorTTL is how long a finished job's collector stays in the
	// registry (so the dashboard and /metrics still show it) before removal.
	collectorTTL = 60 * time.Second
	// stopTimeout bounds how long Stop waits for a cancelled job to save.
	stopTimeout = 30 * time.Second
	// saveTimeout bounds the store write that records a job's result.
	saveTimeout = 15 * time.Second
)

// Deps are the Manager's dependencies.
type Deps struct {
	Store store.Store
	// Metrics owns the jobs' collectors and ticks them. Nil makes each job
	// tick a private collector itself (no live dashboard).
	Metrics *metrics.Registry
	Logger  *slog.Logger
	// OnChange is called after a job changes status (may be nil). It runs on
	// the manager's goroutines with no lock held, so it may call the Manager.
	OnChange func(model.ReplayJob)
}

// liveRun is a job that is running in this process.
type liveRun struct {
	id      string
	job     model.ReplayJob // the persisted "running" state; never modified
	run     *runner
	cancel  context.CancelFunc
	done    chan struct{} // closed once the result is saved and observers notified
	deleted atomic.Bool
}

// removal is a scheduled removal of a finished job's collector. gen tells a
// timer that fires late whether it has been replaced since.
type removal struct {
	gen uint64
	t   *time.Timer
}

// Manager runs replay jobs in the background.
type Manager struct {
	st       store.Store
	reg      *metrics.Registry
	log      *slog.Logger
	onChange func(model.ReplayJob)

	root   context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// txMu serialises the lifecycle transitions that write a job to the
	// store (Start, finishing, Delete, Recover) so a finishing or starting
	// job can never resurrect a deleted one.
	txMu sync.Mutex
	// deleting counts the Delete calls in progress per job, guarded by txMu.
	// Delete releases txMu while it waits for a running job to stop, and
	// Start refuses a job in this map: otherwise a Start landing in that gap
	// would register a new run that saves the job again after its row was
	// deleted.
	deleting map[string]int

	mu      sync.Mutex // guards the fields below
	runs    map[string]*liveRun
	removal map[string]removal // pending collector removals
	nextGen uint64
	closed  bool

	// Knobs for tests.
	collectorTTL time.Duration
	finalWait    time.Duration
}

// NewManager returns a manager. Its jobs run on a root context that Shutdown
// cancels.
func NewManager(deps Deps) *Manager {
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	root, cancel := context.WithCancel(context.Background())
	return &Manager{
		st: deps.Store, reg: deps.Metrics, log: log, onChange: deps.OnChange,
		root: root, cancel: cancel,
		runs: map[string]*liveRun{}, removal: map[string]removal{}, deleting: map[string]int{},
		collectorTTL: collectorTTL, finalWait: finalSeriesWait,
	}
}

func (m *Manager) notify(j model.ReplayJob) {
	if m.onChange == nil {
		return
	}
	j.Series = nil
	m.onChange(cloneJob(j))
}

// cloneJob copies the parts of a job that callers might modify (the API masks
// sensitive header values in place).
func cloneJob(j model.ReplayJob) model.ReplayJob {
	c := &j.Config
	c.Stages = slices.Clone(c.Stages)
	c.Include = slices.Clone(c.Include)
	c.Exclude = slices.Clone(c.Exclude)
	c.RemoveHeaders = slices.Clone(c.RemoveHeaders)
	c.SetHeaders = maps.Clone(c.SetHeaders)
	if j.Summary != nil {
		s := *j.Summary
		j.Summary = &s
	}
	return j
}

// maxSeed is 2^53-1, the largest integer a JSON number carries exactly in a
// browser (and any other client that parses numbers as doubles).
const maxSeed = 1<<53 - 1

// randomSeed returns a non-zero seed in [1, 2^53-1]. A 64-bit seed would be
// rounded by the web UI, so Edit as new and the config view would show and
// resubmit a different seed and the clone would draw a different schedule.
func randomSeed() uint64 {
	for {
		if s := rand.Uint64() & maxSeed; s != 0 {
			return s
		}
	}
}

// Create validates and stores a pending job. cfg gets its defaults and, when
// Seed is 0, a random seed that is stored so re-runs and clones replay the
// same schedule. Problems with the config (including a profile that does not
// exist or does not compile) are a *model.ValidationError; a recording that
// does not exist wraps model.ErrNotFound.
func (m *Manager) Create(ctx context.Context, name string, cfg model.ReplayConfig) (model.ReplayJob, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return model.ReplayJob{}, err
	}
	rec, err := m.st.GetRecording(ctx, cfg.RecordingID)
	if err != nil {
		return model.ReplayJob{}, fmt.Errorf("recording %q: %w", cfg.RecordingID, err)
	}
	if cfg.ProfileID != "" {
		if err := m.checkProfile(ctx, cfg.ProfileID); err != nil {
			return model.ReplayJob{}, err
		}
	}
	if cfg.Seed == 0 {
		cfg.Seed = randomSeed()
	}
	if strings.TrimSpace(name) == "" {
		name = "Replay of " + rec.Name
	}
	job, err := m.st.PutReplayJob(ctx, model.ReplayJob{Name: strings.TrimSpace(name), Config: cfg, Status: model.ReplayPending})
	if err != nil {
		return model.ReplayJob{}, err
	}
	m.notify(job)
	return job, nil
}

// checkProfile makes sure the profile exists and compiles.
func (m *Manager) checkProfile(ctx context.Context, id string) error {
	prof, err := m.st.GetProfile(ctx, id)
	if errors.Is(err, model.ErrNotFound) {
		return &model.ValidationError{Problems: []string{fmt.Sprintf("profile_id %q does not exist", id)}}
	}
	if err != nil {
		return err
	}
	if _, err := anonymize.New(prof); err != nil {
		problems := []string{err.Error()}
		var ve *model.ValidationError
		if errors.As(err, &ve) {
			problems = ve.Problems
		}
		out := make([]string, len(problems))
		for i, p := range problems {
			out[i] = fmt.Sprintf("profile %q: %s", id, p)
		}
		return &model.ValidationError{Problems: out}
	}
	return nil
}

func (m *Manager) lookup(id string) *liveRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs[id]
}

// Start runs a job in the background. Starting a job that already ran runs
// it again and replaces its results; starting a running job returns an error
// wrapping model.ErrConflict.
func (m *Manager) Start(ctx context.Context, id string) error {
	job, launch, err := m.start(ctx, id)
	if err != nil {
		return err
	}
	// The job begins only after observers have heard it is running, so its
	// result can never be reported ahead of its start. The deferred launch
	// keeps a panicking OnChange from stranding a registered job.
	defer launch()
	m.notify(job)
	return nil
}

// start registers the job and persists its running state. It returns the
// function that launches the goroutine.
func (m *Manager) start(ctx context.Context, id string) (model.ReplayJob, func(), error) {
	m.txMu.Lock()
	defer m.txMu.Unlock()

	m.mu.Lock()
	closed, running := m.closed, m.runs[id] != nil
	m.mu.Unlock()
	if closed {
		return model.ReplayJob{}, nil, ErrShutdown
	}
	if running {
		return model.ReplayJob{}, nil, fmt.Errorf("replay %s is already running: %w", id, model.ErrConflict)
	}
	if m.deleting[id] > 0 {
		return model.ReplayJob{}, nil, fmt.Errorf("replay %s is being deleted: %w", id, model.ErrNotFound)
	}
	job, err := m.st.GetReplayJob(ctx, id)
	if err != nil {
		return model.ReplayJob{}, nil, err
	}

	col, tick := m.collector(id)
	jobCtx, cancel := context.WithCancel(m.root)
	lr := &liveRun{id: id, cancel: cancel, done: make(chan struct{})}
	opts := RunOptions{Collector: col, TickCollector: tick, Logger: m.log.With("replay", id), JobID: id}
	lr.run, err = newRunner(m.st, job.Config, opts)
	if err != nil {
		cancel()
		return model.ReplayJob{}, nil, err
	}
	lr.run.finalWait = m.finalWait
	lr.run.skipFinalWait = func() bool { return m.root.Err() != nil || lr.deleted.Load() }

	now := time.Now().UTC()
	job.Status, job.Error = model.ReplayRunning, ""
	job.StartedAt, job.EndedAt = &now, nil
	job.Summary, job.Series = nil, nil
	lr.job = job

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		return model.ReplayJob{}, nil, ErrShutdown
	}
	m.cancelRemoval(id)
	m.runs[id] = lr
	m.wg.Add(1)
	m.mu.Unlock()

	if _, err := m.st.PutReplayJob(ctx, job); err != nil {
		m.mu.Lock()
		delete(m.runs, id)
		m.mu.Unlock()
		m.wg.Done()
		cancel()
		return model.ReplayJob{}, nil, err
	}
	m.log.Info("replay started", "replay", id, "recording", job.Config.RecordingID, "mode", job.Config.Mode)
	return cloneJob(lr.job), func() { go m.runJob(jobCtx, lr) }, nil
}

// collector returns the job's registry collector, reset for a fresh run, and
// whether the run must tick it itself (no registry).
func (m *Manager) collector(id string) (*metrics.Collector, bool) {
	name := metrics.ReplayCollectorName(id)
	if m.reg == nil {
		return metrics.NewCollector(name, model.KindReplay, id), true
	}
	col := m.reg.Collector(name, model.KindReplay, id)
	col.Reset()
	return col, false
}

// runJob executes a job and saves its result.
func (m *Manager) runJob(ctx context.Context, lr *liveRun) {
	defer m.wg.Done()
	defer lr.cancel()
	sum, series, err := lr.run.execute(ctx)

	end := time.Now().UTC()
	job := lr.job
	job.EndedAt = &end
	job.Summary, job.Series = &sum, series
	switch {
	case err == nil:
		job.Status = model.ReplayCompleted
	case ctx.Err() != nil:
		job.Status = model.ReplayStopped
	default:
		job.Status, job.Error = model.ReplayFailed, err.Error()
		if sum.Sent == 0 {
			job.Summary, job.Series = nil, nil
		}
		m.log.Warn("replay failed", "replay", lr.id, "error", err)
	}

	m.save(lr, job)

	m.mu.Lock()
	delete(m.runs, lr.id)
	if !lr.deleted.Load() {
		m.armRemoval(lr.id)
	}
	m.mu.Unlock()
	if !lr.deleted.Load() {
		m.notify(job)
	}
	close(lr.done)
	m.log.Info("replay finished", "replay", lr.id, "status", job.Status, "sent", sum.Sent, "elapsed", sum.Elapsed.String())
}

// save persists the final job, or finishes the deletion if it was deleted
// meanwhile.
func (m *Manager) save(lr *liveRun, job model.ReplayJob) {
	m.txMu.Lock()
	defer m.txMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), saveTimeout)
	defer cancel()
	if lr.deleted.Load() {
		if err := m.st.DeleteReplayJob(ctx, lr.id); err != nil && !errors.Is(err, model.ErrNotFound) {
			m.log.Error("delete replay job", "replay", lr.id, "error", err)
		}
		return
	}
	if _, err := m.st.PutReplayJob(ctx, job); err != nil {
		m.log.Error("save replay job", "replay", lr.id, "error", err)
	}
}

// armRemoval schedules the removal of a finished job's collector after the
// TTL, or removes it at once when the manager is shutting down. m.mu must be
// held.
func (m *Manager) armRemoval(id string) {
	if m.reg == nil {
		return
	}
	if m.closed {
		m.reg.Remove(metrics.ReplayCollectorName(id))
		return
	}
	m.cancelRemoval(id)
	m.nextGen++
	gen := m.nextGen
	m.removal[id] = removal{gen: gen, t: time.AfterFunc(m.collectorTTL, func() { m.expire(id, gen) })}
}

// cancelRemoval drops a pending removal. m.mu must be held.
func (m *Manager) cancelRemoval(id string) {
	if r, ok := m.removal[id]; ok {
		r.t.Stop()
		delete(m.removal, id)
	}
}

func (m *Manager) expire(id string, gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.removal[id]; !ok || r.gen != gen {
		return // re-armed, cancelled by a new run, or already dropped
	}
	delete(m.removal, id)
	if m.runs[id] == nil {
		m.reg.Remove(metrics.ReplayCollectorName(id))
	}
}

// dropCollector removes the job's collector at once.
func (m *Manager) dropCollector(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cancelRemoval(id)
	if m.reg != nil && m.runs[id] == nil {
		m.reg.Remove(metrics.ReplayCollectorName(id))
	}
}

// Stop cancels a running job and waits for it to save as stopped, with the
// summary of what it did. Stopping a job that is not running (or unknown)
// returns nil.
func (m *Manager) Stop(id string) error {
	lr := m.lookup(id)
	if lr == nil {
		return nil
	}
	lr.cancel()
	select {
	case <-lr.done:
		return nil
	case <-time.After(stopTimeout):
		return fmt.Errorf("replay %s did not stop within %s", id, stopTimeout)
	}
}

// snapshot returns the in-memory view of a running job with its live summary.
func (lr *liveRun) snapshot() model.ReplayJob {
	j := cloneJob(lr.job)
	sum := lr.run.summary()
	j.Summary = &sum
	return j
}

// Get returns a job with its series: the live summary and the points so far
// while it runs, the stored result once finished. A client that opens a
// running job seeds its charts from this.
func (m *Manager) Get(ctx context.Context, id string) (model.ReplayJob, error) {
	if lr := m.lookup(id); lr != nil {
		j := lr.snapshot()
		j.Series = lr.run.seriesSince(0)
		return j, nil
	}
	return m.st.GetReplayJob(ctx, id)
}

// List returns all jobs, newest first and without series, with live
// summaries for the running ones.
func (m *Manager) List(ctx context.Context) ([]model.ReplayJob, error) {
	jobs, err := m.st.ListReplayJobs(ctx)
	if err != nil {
		return nil, err
	}
	for i := range jobs {
		if lr := m.lookup(jobs[i].ID); lr != nil {
			jobs[i] = lr.snapshot()
		}
	}
	return jobs, nil
}

// Series returns the per-second points with T >= sinceMs: from the live run
// while the job runs, from the stored job once finished.
func (m *Manager) Series(ctx context.Context, id string, sinceMs int64) ([]model.SeriesPoint, error) {
	if lr := m.lookup(id); lr != nil {
		return lr.run.seriesSince(sinceMs), nil
	}
	job, err := m.st.GetReplayJob(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]model.SeriesPoint, 0, len(job.Series))
	for _, p := range job.Series {
		if p.T >= sinceMs {
			out = append(out, p)
		}
	}
	return out, nil
}

// Running returns the jobs running now, newest start first, with live
// summaries and without series. It never touches the store.
func (m *Manager) Running() []model.ReplayJob {
	m.mu.Lock()
	runs := make([]*liveRun, 0, len(m.runs))
	for _, lr := range m.runs {
		runs = append(runs, lr)
	}
	m.mu.Unlock()
	out := make([]model.ReplayJob, len(runs))
	for i, lr := range runs {
		out[i] = lr.snapshot()
	}
	slices.SortFunc(out, func(a, b model.ReplayJob) int {
		if c := b.StartedAt.Compare(*a.StartedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// Delete removes a job, stopping it first if it runs (its result is then not
// saved) and dropping its collector at once.
func (m *Manager) Delete(ctx context.Context, id string) error {
	m.txMu.Lock()
	m.deleting[id]++ // Start refuses the job from here on
	lr := m.lookup(id)
	if lr != nil {
		lr.deleted.Store(true)
		lr.cancel()
	}
	m.txMu.Unlock()
	if lr != nil {
		select {
		case <-lr.done:
		case <-ctx.Done():
			// The run still deletes the row when it finishes (save).
			m.txMu.Lock()
			m.doneDeleting(id)
			m.txMu.Unlock()
			return ctx.Err()
		}
	}

	m.txMu.Lock()
	defer m.txMu.Unlock()
	defer m.doneDeleting(id)
	err := m.st.DeleteReplayJob(ctx, id)
	if lr != nil && errors.Is(err, model.ErrNotFound) {
		err = nil // the finishing job already removed it
	}
	m.dropCollector(id)
	return err
}

// doneDeleting ends one Delete of id. txMu must be held.
func (m *Manager) doneDeleting(id string) {
	if m.deleting[id]--; m.deleting[id] <= 0 {
		delete(m.deleting, id)
	}
}

// Recover marks jobs a previous process left running as failed.
func (m *Manager) Recover(ctx context.Context) error {
	m.txMu.Lock()
	jobs, err := m.st.ListReplayJobs(ctx)
	if err != nil {
		m.txMu.Unlock()
		return err
	}
	var changed []model.ReplayJob
	for _, j := range jobs {
		if j.Status != model.ReplayRunning || m.lookup(j.ID) != nil {
			continue
		}
		now := time.Now().UTC()
		j.Status, j.Error, j.EndedAt = model.ReplayFailed, "interrupted: mirage restarted", &now
		if _, err = m.st.PutReplayJob(ctx, j); err != nil {
			m.txMu.Unlock()
			return err
		}
		changed = append(changed, j)
	}
	m.txMu.Unlock()
	for _, j := range changed {
		m.log.Warn("replay interrupted by restart", "replay", j.ID)
		m.notify(j)
	}
	return nil
}

// Shutdown stops running jobs and waits for them to save, bounded by ctx.
// Jobs it stops are saved as stopped with their partial results.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	for id := range m.removal {
		m.cancelRemoval(id)
		if m.reg != nil && m.runs[id] == nil {
			m.reg.Remove(metrics.ReplayCollectorName(id))
		}
	}
	m.mu.Unlock()
	m.cancel()

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
