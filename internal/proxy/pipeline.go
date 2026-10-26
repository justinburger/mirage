package proxy

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// captureJob is one captured exchange on its way to the store.
type captureJob struct {
	ex  *model.Exchange
	svc *service
	// st is the service state the request was handled with; it decides the
	// recording, the anonymizer and the body cap.
	st *serviceState
	// queued is the body byte count charged against the queue bounds.
	queued int64
}

// release returns the job's charge to the queue accounting. It must be called
// exactly once, when the job leaves the pipeline.
func (j *captureJob) release(p *pipeline) {
	p.queuedBytes.Add(-j.queued)
	j.svc.pending.Add(-j.queued)
}

// pipeline moves captured exchanges from the request path to the store:
//
//	enqueue (bounded by count and bytes, never blocks)
//	  -> workers: decode gzip/deflate, anonymize
//	  -> writer: batch per recording, Store.AppendExchanges, hybrid learning
//
// When the writer cannot keep up the queue fills and the request path drops
// captures (counted) instead of waiting.
type pipeline struct {
	m        *Manager
	queue    chan *captureJob
	ready    chan *captureJob
	maxBytes int64

	queuedBytes atomic.Int64
	stopped     atomic.Bool // no new jobs are accepted

	startOnce sync.Once
	started   atomic.Bool
	stopCh    chan struct{} // closed: drain what is queued and finish
	abortCh   chan struct{} // closed: give up (shutdown deadline)
	writeCtx  context.Context
	writeStop context.CancelFunc
	workers   sync.WaitGroup
	writerEnd chan struct{}
}

func newPipeline(m *Manager) *pipeline {
	ctx, cancel := context.WithCancel(context.Background())
	return &pipeline{
		m:         m,
		queue:     make(chan *captureJob, m.deps.CaptureQueueSize),
		ready:     make(chan *captureJob, max(2*m.deps.CaptureBatchSize, 64)),
		maxBytes:  m.deps.CaptureQueueBytes,
		stopCh:    make(chan struct{}),
		abortCh:   make(chan struct{}),
		writeCtx:  ctx,
		writeStop: cancel,
		writerEnd: make(chan struct{}),
	}
}

// start launches the workers and the writer, once. Services that never
// capture never start them.
func (p *pipeline) start() {
	p.startOnce.Do(func() {
		p.started.Store(true)
		for range p.m.deps.CaptureWorkers {
			p.workers.Add(1)
			go p.work()
		}
		go p.write()
	})
}

// enqueue offers a job to the queue. It never blocks: when the queue is full
// by count or by bytes, or the pipeline is stopping, the job is dropped and
// counted. A job is always admitted into an empty queue, so a single exchange
// larger than the byte bound is not lost forever.
func (p *pipeline) enqueue(j *captureJob) {
	if p.stopped.Load() {
		j.svc.dropped.Add(1)
		return
	}
	if now := p.queuedBytes.Add(j.queued); now > p.maxBytes && now != j.queued {
		p.queuedBytes.Add(-j.queued)
		j.svc.dropped.Add(1)
		return
	}
	j.svc.pending.Add(j.queued)
	select {
	case p.queue <- j:
	default:
		p.drop(j)
	}
}

// work is one worker: it takes jobs off the queue until told to stop and then
// drains what is left.
func (p *pipeline) work() {
	defer p.workers.Done()
	for {
		select {
		case j := <-p.queue:
			p.process(j)
		case <-p.stopCh:
			for {
				select {
				case j := <-p.queue:
					p.process(j)
				default:
					return
				}
			}
		case <-p.abortCh:
			return
		}
	}
}

// process decodes and anonymizes a job and hands it to the writer. An
// exchange whose anonymization fails is dropped: it is never stored raw.
func (p *pipeline) process(j *captureJob) {
	decodeExchange(j.ex, j.st.bodyCap)
	if j.st.anon != nil {
		if err := j.st.anon.Exchange(j.ex); err != nil {
			p.m.log.Error("anonymizing capture; exchange dropped", "service", j.svc.id, "error", err)
			j.svc.setRuntimeErr("capture: anonymize: " + err.Error())
			p.drop(j)
			return
		}
	}
	select {
	case p.ready <- j:
	case <-p.abortCh:
		p.drop(j)
	}
}

// batch collects jobs for one recording.
type batch struct {
	jobs []*captureJob
}

// write is the single writer: it batches jobs per recording and appends each
// batch in one store transaction when it reaches CaptureBatchSize or when the
// flush interval elapses, whichever comes first.
func (p *pipeline) write() {
	defer close(p.writerEnd)
	batches := make(map[string]*batch)
	tick := time.NewTicker(p.m.deps.CaptureFlushInterval)
	defer tick.Stop()
	flushAll := func() {
		for id, b := range batches {
			if len(b.jobs) > 0 {
				p.flush(id, b.jobs)
				b.jobs = b.jobs[:0]
			}
		}
	}
	for {
		select {
		case j, ok := <-p.ready:
			if !ok {
				flushAll()
				return
			}
			id := j.st.svc.RecordingID
			b := batches[id]
			if b == nil {
				b = &batch{}
				batches[id] = b
			}
			b.jobs = append(b.jobs, j)
			if len(b.jobs) >= p.m.deps.CaptureBatchSize {
				p.flush(id, b.jobs)
				b.jobs = b.jobs[:0]
			}
		case <-tick.C:
			flushAll()
		case <-p.abortCh:
			for _, b := range batches {
				for _, j := range b.jobs {
					p.drop(j)
				}
			}
			return
		}
	}
}

// flush stores one batch. After a successful append each exchange is counted
// and, in hybrid mode, learned by the virtualizer.
func (p *pipeline) flush(recordingID string, jobs []*captureJob) {
	exs := make([]*model.Exchange, len(jobs))
	for i, j := range jobs {
		exs[i] = j.ex
	}
	if err := p.m.deps.Store.AppendExchanges(p.writeCtx, recordingID, exs); err != nil {
		p.m.log.Error("storing capture batch; exchanges dropped", "recording", recordingID, "count", len(jobs), "error", err)
		for _, j := range jobs {
			j.svc.setRuntimeErr(fmt.Sprintf("capture: store: %v", err))
			p.drop(j)
		}
		return
	}
	for _, j := range jobs {
		stored := int64(len(j.ex.Request.Body) + len(j.ex.Response.Body))
		j.svc.captured.Add(1)
		j.svc.used.Add(stored)
		j.release(p)
		p.learn(j) // hands the exchange over: do not touch j.ex afterwards
	}
}

// learn adds a stored exchange to the service's virtualizer in hybrid mode,
// unless it is something a replay should not reproduce: writes, transient
// failures, upstream errors and truncated bodies.
func (p *pipeline) learn(j *captureJob) {
	if j.st.mode != model.ModeHybrid || !learnable(j.ex) {
		return
	}
	cur := j.svc.acquire()
	if cur == nil {
		return
	}
	defer cur.release()
	if cur.mode == model.ModeHybrid && cur.virt != nil && cur.svc.RecordingID == j.st.svc.RecordingID {
		cur.virt.Add(j.ex)
	}
}

// learnable reports whether a captured exchange may be added to the live index.
func learnable(ex *model.Exchange) bool {
	switch ex.Request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		return false
	}
	return ex.Error == "" && ex.Response.Status < 500 &&
		!ex.Request.BodyTruncated && !ex.Response.BodyTruncated
}

// stop finishes the pipeline: no new jobs are accepted, the queue is drained
// and every pending batch is flushed. If ctx ends first the rest is
// abandoned (counted as dropped) and ctx's error is returned.
func (p *pipeline) stop(ctx context.Context) error {
	p.stopped.Store(true)
	if !p.started.Load() {
		return nil
	}
	close(p.stopCh)
	done := make(chan struct{})
	go func() {
		p.workers.Wait()
		close(p.ready)
		<-p.writerEnd
		close(done)
	}()
	select {
	case <-done:
		p.writeStop()
		return nil
	case <-ctx.Done():
		close(p.abortCh)
		p.writeStop()
		<-done
		p.abandon()
		return fmt.Errorf("draining capture queue: %w", ctx.Err())
	}
}

// abandon counts as dropped every job still in the hand-off to the writer or
// in the queue after an aborted stop. All pipeline goroutines have exited and
// ready is closed, so nothing else touches the channels.
func (p *pipeline) abandon() {
	for j := range p.ready {
		p.drop(j)
	}
	for {
		select {
		case j := <-p.queue:
			p.drop(j)
		default:
			return
		}
	}
}

// drop counts a job as lost and returns its queue charge.
func (p *pipeline) drop(j *captureJob) {
	j.svc.dropped.Add(1)
	j.release(p)
}
