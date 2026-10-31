package replay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/store"
)

const (
	// spinWindow is how close to its due time the scheduler stops sleeping:
	// runtime timers fire up to about a millisecond late (the netpoller waits
	// in whole milliseconds on Linux), so the last stretch is spent yielding
	// in a loop instead. A shorter window would save CPU at moderate rates but
	// lets that lateness through as send-time error.
	spinWindow = time.Millisecond
	// finalSeriesWait bounds how long a registry-ticked run waits for the
	// registry to close the last second of the run.
	finalSeriesWait = 1500 * time.Millisecond
	// registrySlack is how long after a second boundary a registry-ticked run
	// polls the collector, so the registry has closed that second by then.
	registrySlack = 150 * time.Millisecond
	// numStatuses sizes the per-status counters (HTTP status 0..999).
	numStatuses = 1000
)

// runner is one execution of a replay: it loads the recording, drives the
// schedule, and accumulates the numbers. All fields touched by workers are
// atomics; the summary can be read at any time from any goroutine.
type runner struct {
	st    store.Store
	cfg   model.ReplayConfig
	col   *metrics.Collector
	opts  RunOptions
	log   *slog.Logger
	label string // X-Mirage-Replay value

	// Test and Manager hooks.
	maxPreload    int64
	finalWait     time.Duration
	skipFinalWait func() bool

	// Prepared by execute before the clock starts.
	tgt     target
	ov      overlay
	engines []*anonymize.Engine // nil without ProfileID; one per clone otherwise
	timeout time.Duration
	conc    int

	// The run clock and hot-path state, set before any worker starts.
	start   time.Time
	t0      atomic.Pointer[time.Time]
	endNs   atomic.Int64 // elapsed at the end of the run; 0 while running
	tr      *http.Transport
	work    chan job
	wg      sync.WaitGroup
	spawned int // scheduler goroutine only
	timer   *time.Timer
	spin    bool
	reqCtx  context.Context

	planned atomic.Int64
	// plannedEstimate marks a Planned that is an estimate (a looping timeline
	// whose later passes may draw differently from the first), so the summary
	// never shows more requests sent than planned.
	plannedEstimate atomic.Bool
	targetRPS       atomic.Uint64

	// sendEndNs is the run clock when the scheduler stopped sending (0 while
	// it still does), and windowDone the completions up to that moment (-1
	// when the rate is over the whole run instead). AchievedRPS is measured
	// over the send window, not over the drain that follows it.
	sendEndNs  atomic.Int64
	windowDone atomic.Int64

	sent, completed, failed, timeouts atomic.Int64
	errStatus, mismatches             atomic.Int64
	bytesSent, bytesRecv              atomic.Int64
	maxLag                            atomic.Int64 // ns
	busy, maxBusy                     atomic.Int64
	statuses                          [numStatuses]atomic.Int64
	lat, corr                         *metrics.Histogram

	series    *seriesBuf
	collectMu sync.Mutex
	startMs   int64
	lastT     int64
}

// newRunner applies defaults, validates cfg and prepares an idle runner.
func newRunner(st store.Store, cfg model.ReplayConfig, opts RunOptions) (*runner, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if opts.Collector == nil {
		return nil, errors.New("replay: RunOptions.Collector is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	label := opts.JobID
	if label == "" {
		label = "cli"
	}
	r := &runner{
		st: st, cfg: cfg, col: opts.Collector, opts: opts, log: log, label: label,
		maxPreload: maxPreloadBytes, finalWait: finalSeriesWait,
		lat: metrics.NewHistogram(), corr: metrics.NewHistogram(),
		series: newSeriesBuf(model.MaxReplaySeries),
		conc:   cfg.Concurrency, timeout: cfg.Timeout.D(),
	}
	r.planned.Store(-1)
	return r, nil
}

// execute runs the replay to completion. It returns the final summary and
// series; when ctx ends early they are the partial results and the error is
// ctx.Err().
func (r *runner) execute(ctx context.Context) (model.ReplaySummary, []model.SeriesPoint, error) {
	cfg := r.cfg
	if _, err := r.st.GetRecording(ctx, cfg.RecordingID); err != nil {
		return r.summary(), nil, fmt.Errorf("recording %q: %w", cfg.RecordingID, err)
	}
	if err := r.buildEngines(ctx); err != nil {
		return r.summary(), nil, err
	}
	prep, err := r.load(ctx)
	if err != nil {
		return r.summary(), nil, err
	}
	r.log.Info("replay loaded", "recording", cfg.RecordingID, "requests", len(prep.items),
		"excluded", prep.skipped, "bytes", prep.bytes, "mode", cfg.Mode)

	gen, planned, targetRPS := newGenerator(prep, cfg)
	r.planned.Store(planned)
	r.plannedEstimate.Store(cfg.Mode == model.ReplayTimeline && cfg.Loop && cfg.Duration > 0)
	r.targetRPS.Store(math.Float64bits(targetRPS))

	r.runLoad(ctx, gen)

	sum := r.summary()
	series := r.series.all()
	if err := ctx.Err(); err != nil {
		return sum, series, err
	}
	return sum, series, nil
}

// buildEngines compiles the anonymization profile, with one salted engine per
// amplify copy when VaryClones is set.
func (r *runner) buildEngines(ctx context.Context) error {
	cfg := r.cfg
	if cfg.ProfileID == "" {
		return nil
	}
	prof, err := r.st.GetProfile(ctx, cfg.ProfileID)
	if err != nil {
		return fmt.Errorf("profile %q: %w", cfg.ProfileID, err)
	}
	eng, err := anonymize.New(prof)
	if err != nil {
		return fmt.Errorf("profile %q: %w", cfg.ProfileID, err)
	}
	r.engines = []*anonymize.Engine{eng}
	if cfg.VaryClones && cfg.Amplify > 1 {
		r.engines = make([]*anonymize.Engine, cfg.Amplify)
		r.engines[0] = eng
		for c := 1; c < cfg.Amplify; c++ {
			r.engines[c] = eng.WithSalt("clone-" + strconv.Itoa(c))
		}
	}
	return nil
}

// load streams the recording into prepared items.
func (r *runner) load(ctx context.Context) (*prepared, error) {
	cfg := r.cfg
	tgt, err := parseTarget(cfg.Target)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}
	r.tgt = tgt
	r.ov = newOverlay(cfg, r.label, tgt)
	l, err := newLoader(cfg, tgt, r.ov, cfg.ProfileID == "", r.maxPreload)
	if err != nil {
		return nil, err
	}
	err = r.st.IterateExchanges(ctx, cfg.RecordingID, model.IterOptions{RequestBodies: true}, l.add)
	if err != nil {
		return nil, fmt.Errorf("load recording: %w", err)
	}
	var maxGap time.Duration
	if cfg.Mode == model.ReplayTimeline {
		maxGap = cfg.MaxGap.D()
	}
	return l.finish(maxGap)
}

// newTransport builds the job's dedicated HTTP transport.
//
// MaxConnsPerHost is the number of workers: a worker that finishes a response
// and starts its next request before the transport has put the connection back
// in the idle pool would otherwise dial a new one, and the surplus connection
// is closed when the pool is full. At a couple of thousand workers that churn
// leaves tens of thousands of sockets in TIME_WAIT, exhausts the ephemeral
// ports and shows up as multi-second latencies and failures that are the
// client's. With the cap the request waits for the returning connection (the
// transport hands it over directly), so each worker keeps one.
func newTransport(cfg model.ReplayConfig) *http.Transport {
	return &http.Transport{
		DialContext:         limitDials((&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext, maxConcurrentDials),
		ForceAttemptHTTP2:   true,
		MaxConnsPerHost:     cfg.Concurrency,
		MaxIdleConns:        cfg.Concurrency,
		MaxIdleConnsPerHost: cfg.Concurrency,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableCompression:  true,
		// Skipping verification is an explicit per-job option for staging
		// targets with private CAs.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify, MinVersion: tls.VersionTLS12},
	}
}

// runLoad starts the clock and runs the scheduler (on the calling goroutine)
// and the workers until the schedule is exhausted, a stop condition is met or
// ctx ends, then drains the in-flight requests.
func (r *runner) runLoad(ctx context.Context, gen generator) {
	r.tr = newTransport(r.cfg)
	defer r.tr.CloseIdleConnections()
	r.work = make(chan job)
	r.reqCtx = ctx
	r.spin = runtime.GOMAXPROCS(0) > 1

	r.start = time.Now()
	start := r.start
	r.t0.Store(&start)
	r.startMs = start.UnixMilli() / 1000 * 1000
	r.lastT = r.startMs - 1

	sctx := ctx
	if r.cfg.Duration > 0 {
		var cancel context.CancelFunc
		sctx, cancel = context.WithTimeout(ctx, r.cfg.Duration.D())
		defer cancel()
	}

	stopMon, monDone := make(chan struct{}), make(chan struct{})
	go r.monitor(stopMon, monDone)

	r.schedule(ctx, sctx, gen)
	r.noteSendEnd(sctx)
	r.wg.Wait()
	if r.timer != nil {
		r.timer.Stop()
	}

	r.endNs.Store(max(1, int64(time.Since(start))))
	close(stopMon)
	<-monDone
	r.finalSeries()
}

func (r *runner) now() time.Duration { return time.Since(r.start) }

// noteSendEnd closes the send window: the moment the scheduler stopped
// handing out requests, which is where the schedule ends however long the
// in-flight requests then take to drain. A closed loop that simply ran out of
// requests has no such window (its workers are still busy), so its rate stays
// over the whole run; one cut by Duration or a cancel has, and counts the
// completions inside it.
func (r *runner) noteSendEnd(sctx context.Context) {
	done := int64(-1)
	if r.cfg.Mode == model.ReplayMax {
		if sctx.Err() == nil {
			return
		}
		done = r.completed.Load()
	}
	r.windowDone.Store(done)
	r.sendEndNs.Store(max(1, int64(r.now())))
}

// schedule feeds the workers until the schedule ends. It closes r.work.
//
// hard is the caller's context: cancelling it stops everything at once. soft
// additionally expires at Duration: it ends waits for requests that are not
// due yet and the wait for a free worker, but a scheduler that is merely late
// (a stalled machine, not a saturated target) still sends the requests that
// were due before Duration, so Sent reaches Planned.
func (r *runner) schedule(hard, soft context.Context, gen generator) {
	defer close(r.work)
	limit := r.cfg.Duration.D()
	maxReq := r.cfg.MaxRequests
	var n int64
	for {
		if maxReq > 0 && n >= maxReq {
			return
		}
		j, ok := gen.next()
		if !ok {
			if end := gen.end(); end > 0 {
				r.waitUntil(hard, soft, end)
			}
			return
		}
		if limit > 0 && j.due >= limit {
			r.waitUntil(hard, soft, limit)
			return
		}
		if j.due < 0 { // closed model: no intended time
			if soft.Err() != nil {
				return
			}
		} else if !r.waitUntil(hard, soft, j.due) {
			return
		}
		if !r.dispatch(soft, j) {
			return
		}
		n++
	}
}

// waitUntil blocks until the run clock reaches due, sleeping while it is far
// off and yielding in a loop for the last stretch. It reports false when hard
// was cancelled, or soft ended before due.
//
// A scheduler that wakes late can find its timer and soft.Done both ready,
// and select then picks either one. Soft ending only counts when the run clock
// has really not reached due (see softEnded), so a request that was due before
// Duration is sent whichever case won.
func (r *runner) waitUntil(hard, soft context.Context, due time.Duration) bool {
	done := soft.Done()
	for {
		wait := due - r.now()
		if wait <= 0 {
			return hard.Err() == nil
		}
		if wait > spinWindow || !r.spin {
			d := wait
			if r.spin {
				d -= spinWindow
			}
			if !r.sleep(done, d) {
				return r.softEnded(hard, due)
			}
			continue
		}
		select {
		case <-done:
			return r.softEnded(hard, due)
		default:
		}
		runtime.Gosched()
	}
}

// softEnded is waitUntil's answer once the soft context has ended: the request
// is still sent when its due time has already passed (and hard is alive), which
// is a late scheduler, not a run that was cut before the request was due.
func (r *runner) softEnded(hard context.Context, due time.Duration) bool {
	return r.now() >= due && hard.Err() == nil
}

// sleep waits d or until done is closed, reusing one timer.
func (r *runner) sleep(done <-chan struct{}, d time.Duration) bool {
	if r.timer == nil {
		r.timer = time.NewTimer(d)
	} else {
		r.timer.Reset(d)
	}
	select {
	case <-r.timer.C:
		return true
	case <-done:
		r.timer.Stop()
		return false
	}
}

// dispatch hands j to an idle worker, starts a new worker while fewer than
// Concurrency exist, and otherwise blocks until one frees up; that blocking
// is the client-side queueing the lag and corrected latency report.
func (r *runner) dispatch(ctx context.Context, j job) bool {
	select {
	case r.work <- j:
		return true
	default:
	}
	if r.spawned < r.conc {
		r.spawned++
		r.wg.Add(1)
		go r.worker(j)
		return true
	}
	select {
	case r.work <- j:
		return true
	case <-ctx.Done():
		return false
	}
}

func (r *runner) worker(first job) {
	defer r.wg.Done()
	r.exec(first)
	for j := range r.work {
		r.exec(j)
	}
}

func noteMax(a *atomic.Int64, v int64) {
	for {
		cur := a.Load()
		if v <= cur || a.CompareAndSwap(cur, v) {
			return
		}
	}
}

// exec sends one request and records the outcome.
func (r *runner) exec(j job) {
	it := j.it
	noteMax(&r.maxBusy, r.busy.Add(1))
	r.sent.Add(1)
	r.col.InFlight(1)
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("replay worker panic", "panic", p)
			r.failed.Add(1)
		}
		r.col.InFlight(-1)
		r.busy.Add(-1)
	}()

	ctx, cancel := context.WithTimeout(r.reqCtx, r.timeout)
	defer cancel()
	req, sentBytes, err := r.build(ctx, j)
	if err != nil {
		r.fail(it, err, 0, 0)
		return
	}

	sent := r.now()
	due := j.due
	if due < 0 {
		due = sent
	}
	if lag := sent - due; lag > 0 {
		noteMax(&r.maxLag, int64(lag))
	}
	r.bytesSent.Add(sentBytes)

	resp, err := r.tr.RoundTrip(req)
	var n int64
	if err == nil {
		n, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	done := r.now()
	svc, corr := done-sent, max(done-due, done-sent, 1)

	if err != nil {
		if r.reqCtx.Err() != nil {
			return // aborted by Stop or Shutdown, not a target failure
		}
		if errors.Is(err, context.DeadlineExceeded) {
			// The request's clock started before it was built, so a timeout
			// waited at least Timeout.
			svc = max(svc, r.timeout)
			corr = max(corr, svc)
		}
		r.fail(it, err, svc, corr)
		return
	}

	status := resp.StatusCode
	r.completed.Add(1)
	if status >= 0 && status < numStatuses {
		r.statuses[status].Add(1)
	}
	isErr := model.IsErrorStatus(status)
	if isErr {
		r.errStatus.Add(1)
	}
	if r.cfg.CompareStatus && status != int(it.status) {
		r.mismatches.Add(1)
	}
	r.bytesRecv.Add(n)
	r.lat.Record(svc)
	r.corr.Record(corr)
	r.col.Observe(metrics.Observation{
		Endpoint: it.endpoint, Status: status, Duration: svc,
		BytesIn: sentBytes, BytesOut: n, Error: isErr, Corrected: corr,
	})
}

// maxLoggedFailures bounds how many failed requests a job logs, so a dead
// target does not flood the log but the cause of the failures is visible.
const maxLoggedFailures = 5

// fail records a request that got no response.
//
// A request that waited for the target and then failed (a timeout, or a
// connection that broke after it was made) is part of the latency summaries,
// with the time it actually spent: leaving it out would make the percentiles
// better the worse the target gets, since only the survivors would count. One
// that never got as far as the target (build error, refused or unresolvable
// connection, failed certificate check) waited for nothing and is left out.
func (r *runner) fail(it *item, err error, svc, corr time.Duration) {
	if r.failed.Add(1) <= maxLoggedFailures {
		r.log.Warn("replay request failed", "endpoint", it.endpoint, "error", err)
	}
	timeout := isTimeout(err)
	if timeout {
		r.timeouts.Add(1)
	}
	if corr <= 0 {
		corr = max(svc, 1)
	}
	if svc > 0 && (timeout || reachedTarget(err)) {
		r.lat.Record(svc)
		r.corr.Record(corr)
	}
	r.col.Observe(metrics.Observation{Endpoint: it.endpoint, Status: 0, Duration: svc, Error: true, Corrected: corr})
}

// reachedTarget reports whether a failed request got as far as a connection
// to the target: false for errors from dialing, name resolution and
// certificate verification, where nothing was ever sent.
func reachedTarget(err error) bool {
	var (
		op   *net.OpError
		dns  *net.DNSError
		cert *tls.CertificateVerificationError
	)
	switch {
	case errors.As(err, &dns), errors.As(err, &cert):
		return false
	case errors.As(err, &op):
		return op.Op != "dial"
	}
	return true
}

// isTimeout reports whether err is a deadline or network timeout.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// build assembles the HTTP request for j and returns the number of body bytes
// it carries. Without a profile the prepared item is used as is; with one it
// is cloned, anonymized with the copy's engine and then given the job's
// header overrides, so SetHeaders (an Authorization valid for the target)
// are never themselves anonymized.
func (r *runner) build(ctx context.Context, j job) (*http.Request, int64, error) {
	it := j.it
	req := http.Request{
		Method: it.method, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Host: r.ov.host,
	}
	body := it.body
	if r.engines == nil {
		u, err := r.tgt.url(it.uri)
		if err != nil {
			return nil, 0, err
		}
		req.URL, req.Header = u, it.hdr.h
	} else {
		var err error
		var hdr http.Header
		if hdr, body, req.URL, err = r.anonymize(j); err != nil {
			return nil, 0, err
		}
		req.Header = hdr
	}
	if len(body) > 0 {
		req.ContentLength = int64(len(body))
		req.Body = newBody(body)
		req.GetBody = func() (io.ReadCloser, error) { return newBody(body), nil }
	}
	return req.WithContext(ctx), int64(len(body)), nil
}

// anonymize prepares the request of j for sending with its copy's engine.
func (r *runner) anonymize(j job) (http.Header, []byte, *url.URL, error) {
	it := j.it
	eng := r.engines[j.copy%len(r.engines)]
	path, rawQuery := it.splitURI(len(r.tgt.prefix))
	mr := model.Request{
		Method: it.method, Path: path, RawQuery: rawQuery,
		Header: it.hdr.h.Clone(), BodySize: int64(len(it.body)),
	}
	if len(it.body) > 0 {
		mr.Body = append([]byte(nil), it.body...)
	}
	if err := eng.Request(&mr); err != nil {
		return nil, nil, nil, fmt.Errorf("anonymize: %w", err)
	}
	hdr, body := mr.Header, mr.Body
	if hdr == nil {
		hdr = http.Header{}
	}
	delete(hdr, "Content-Length")
	if it.flags&flagGzip != 0 && len(body) > 0 {
		body = gzipFast(body)
		hdr["Content-Encoding"] = []string{"gzip"}
	}
	r.ov.apply(hdr)
	uri := r.tgt.prefix + mr.Path
	if mr.RawQuery != "" {
		uri += "?" + mr.RawQuery
	}
	u, err := r.tgt.url(uri)
	if err != nil {
		return nil, nil, nil, err
	}
	return hdr, body, u, nil
}

// monitor collects the series and reports progress once a second, on the
// second boundaries (as the registry does), so the points a run ticks itself
// carry exactly the stamps a registry would give them.
//
// A wake-up that comes a second or more late (a stalled or overloaded
// machine) sleeps through a boundary, because the next one is taken from the
// clock. That costs a Progress call, but not a point: the second it skipped is
// zero-filled by the collector and picked up by collect.
func (r *runner) monitor(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	t := time.NewTimer(0)
	defer t.Stop()
	// The first boundary is the one after the run started, not after this
	// goroutine did: if the start of the run is a scheduling delay behind it,
	// the run's first second must not be the one that goes missing.
	boundary := nextSecond(r.start)
	for {
		wait := time.Until(boundary)
		if !r.opts.TickCollector {
			wait += registrySlack // let the registry close the second first
		}
		t.Reset(wait)
		select {
		case <-stop:
			return
		case <-t.C:
			r.collect(boundary)
			if r.opts.Progress != nil {
				r.opts.Progress(r.summary())
			}
			boundary = nextSecond(time.Now())
		}
	}
}

// nextSecond returns the first whole second after t.
func nextSecond(t time.Time) time.Time { return t.Truncate(time.Second).Add(time.Second) }

// collect appends the points the collector has closed since the last call
// (ticking it first, as of the second boundary now, when the run owns the
// ticking).
//
// It always reads them back from the collector's series rather than taking
// what Tick returns: a Tick that comes more than a second after the previous
// one first appends zero points for the seconds in between and then closes its
// own, and returns only the last. Those fill points are what keeps the series
// one point per second for the charts and the stored result, so they must be
// collected too.
func (r *runner) collect(now time.Time) {
	r.collectMu.Lock()
	defer r.collectMu.Unlock()
	if r.opts.TickCollector {
		r.col.Tick(now)
	}
	for _, p := range r.col.Series(r.lastT + 1) {
		r.addPoint(p)
	}
}

func (r *runner) addPoint(p model.SeriesPoint) {
	if p.T < r.startMs || p.T <= r.lastT {
		return
	}
	r.lastT = p.T
	r.series.add(p)
}

// finalSeries adds the last, partial second. A run that ticks its collector
// ticks once more, as of the boundary that ends the current second; otherwise
// it waits (bounded) for the registry to close the second the last response
// fell in.
func (r *runner) finalSeries() {
	if r.opts.TickCollector || r.finalWait <= 0 || (r.skipFinalWait != nil && r.skipFinalWait()) {
		r.collect(nextSecond(time.Now()))
		return
	}
	need := time.Now().Unix() * 1000
	deadline := time.Now().Add(r.finalWait)
	for {
		r.collect(time.Time{})
		r.collectMu.Lock()
		got := r.lastT >= need
		r.collectMu.Unlock()
		if got || time.Now().After(deadline) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// seriesSince returns the live series points with T >= sinceMs.
func (r *runner) seriesSince(sinceMs int64) []model.SeriesPoint { return r.series.since(sinceMs) }

// achievedRPS is the rate the target actually saw: the responses that came
// back for requests sent inside the send window, over that window. The drain
// that follows (the in-flight requests finishing, up to Timeout) is not part
// of the schedule, so a run that sent everything on time is not reported below
// its target because a few responses were slow. While the scheduler still
// sends, and for closed-loop runs that end by running out of requests, it is
// completed over the run so far.
func (r *runner) achievedRPS(completed int64, elapsed time.Duration) float64 {
	window := elapsed
	if end := time.Duration(r.sendEndNs.Load()); end > 0 {
		window = end
		if done := r.windowDone.Load(); done >= 0 {
			completed = done
		}
	}
	if window <= 0 {
		return 0
	}
	return float64(completed) / window.Seconds()
}

// summary builds the Summary from the run's counters and histograms; it is
// safe to call at any time.
func (r *runner) summary() model.ReplaySummary {
	var elapsed time.Duration
	if end := r.endNs.Load(); end > 0 {
		elapsed = time.Duration(end)
	} else if t0 := r.t0.Load(); t0 != nil {
		elapsed = time.Since(*t0)
	}
	completed, failed := r.completed.Load(), r.failed.Load()
	s := model.ReplaySummary{
		Planned:          r.planned.Load(),
		Sent:             r.sent.Load(),
		Completed:        completed,
		Failed:           failed,
		Timeouts:         r.timeouts.Load(),
		Errors:           failed + r.errStatus.Load(),
		Mismatches:       r.mismatches.Load(),
		StatusCounts:     map[string]int64{},
		Latency:          r.lat.Summary(),
		CorrectedLatency: r.corr.Summary(),
		TargetRPS:        math.Float64frombits(r.targetRPS.Load()),
		BytesSent:        r.bytesSent.Load(),
		BytesReceived:    r.bytesRecv.Load(),
		Elapsed:          model.Duration(elapsed),
		MaxLag:           model.Duration(r.maxLag.Load()),
		MaxInFlight:      r.maxBusy.Load(),
		// A closed loop keeps every worker busy by definition and has no
		// intended send times to fall behind, so only the open-loop modes can
		// be saturated.
		Saturated: r.cfg.Mode != model.ReplayMax && r.maxBusy.Load() >= int64(r.conc),
		Endpoints: r.col.Endpoints(),
	}
	if s.Planned >= 0 && s.Sent > s.Planned && r.plannedEstimate.Load() {
		s.Planned = s.Sent
	}
	for code := range r.statuses {
		if n := r.statuses[code].Load(); n > 0 {
			s.StatusCounts[strconv.Itoa(code)] = n
		}
	}
	s.AchievedRPS = r.achievedRPS(completed, elapsed)
	if done := completed + failed; done > 0 {
		s.ErrorRate = float64(s.Errors) / float64(done)
	}
	if s.Endpoints == nil {
		s.Endpoints = []model.EndpointSummary{}
	}
	return s
}
