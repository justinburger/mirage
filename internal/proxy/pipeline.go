package proxy

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
)

// captureJob is one captured exchange on its way to the store.
type captureJob struct {
	ex  *model.Exchange
	svc *service
	// st is the service state the request was handled with; it decides the
	// recording, the anonymizer and the body cap.
	st *serviceState
	// queued is the body byte count charged against the queue bounds. It
	// starts as the size of the captured (possibly still compressed) bodies
	// and follows the exchange as the workers decode and anonymize it.
	queued int64
	// overhead is the estimated memory of everything that is not a body
	// (header maps, strings, the exchange itself). It counts against the
	// queue's byte bound but not against the service's capture limit, which
	// is about stored body bytes.
	overhead int64
	// conditional marks a request that carried conditional or range headers
	// (decided on the request as received, before anonymization).
	conditional bool
}

// release returns the job's charge to the queue accounting. It must be called
// exactly once, when the job leaves the pipeline.
func (j *captureJob) release(p *pipeline) {
	p.queuedBytes.Add(-(j.queued + j.overhead))
	j.svc.pending.Add(-j.queued)
}

// Estimated memory costs, in bytes, of an exchange that its bodies do not
// cover: the structs and strings of the exchange itself, and per header value
// the map slot, the slice and the string headers. Measured on captured
// traffic (5 request and 2 response headers: about 1.8 KiB).
const (
	exchangeFixedBytes = 1024
	headerEntryBytes   = 96
)

// exchangeOverhead estimates the memory an exchange holds besides its bodies.
func exchangeOverhead(ex *model.Exchange) int64 {
	n := int64(exchangeFixedBytes)
	n += int64(len(ex.Request.Path) + len(ex.Request.RawQuery) + len(ex.Request.Host) + len(ex.Endpoint) + len(ex.Error))
	n += headerBytes(ex.Request.Header) + headerBytes(ex.Response.Header)
	return n
}

func headerBytes(h http.Header) int64 {
	var n int64
	for k, vals := range h {
		n += int64(len(k))
		for _, v := range vals {
			n += headerEntryBytes + int64(len(v))
		}
	}
	return n
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
	charge := j.queued + j.overhead
	if now := p.queuedBytes.Add(charge); now > p.maxBytes && now != charge {
		p.queuedBytes.Add(-charge)
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
//
// The bodies were charged at their encoded size when the job was queued; once
// decoded they can be hundreds of times larger, so the charge follows them
// (see recharge) and a job that no longer fits the queue's byte bound is
// dropped here.
func (p *pipeline) process(j *captureJob) {
	st := j.st
	if st.anon != nil {
		decodeExchangeForAnon(j.ex, st.bodyCap)
	} else {
		decodeExchange(j.ex, st.bodyCap)
	}
	if !p.recharge(j, true) {
		p.drop(j)
		return
	}
	if st.anon != nil {
		ex := j.ex
		rawPath := ex.Request.Path
		if err := st.anon.Exchange(ex); err != nil {
			p.m.log.Error("anonymizing capture; exchange dropped", "service", j.svc.id, "error", err)
			j.svc.setRuntimeErr("capture: anonymize: " + err.Error())
			p.drop(j)
			return
		}
		// The endpoint was computed from the raw path; it must not carry what
		// the anonymizer just removed from the stored path.
		ex.Endpoint = anonymizedEndpoint(st.norm, ex.Request.Method, rawPath, ex.Request.Path)
		p.recharge(j, false)
	}
	select {
	case p.ready <- j:
	case <-p.abortCh:
		p.drop(j)
	}
}

// recharge brings the job's queue charge in line with the bodies it holds now.
// With enforce set it reports false when growth pushed the queue past its byte
// bound (a job alone in the queue is always kept, like at admission); the
// caller then drops the job, which returns the whole charge.
func (p *pipeline) recharge(j *captureJob, enforce bool) bool {
	body := int64(len(j.ex.Request.Body) + len(j.ex.Response.Body))
	delta := body - j.queued
	if delta == 0 {
		return true
	}
	j.queued = body
	now := p.queuedBytes.Add(delta)
	j.svc.pending.Add(delta)
	return !(enforce && delta > 0 && now > p.maxBytes && now != j.queued+j.overhead)
}

// anonymizedEndpoint returns the endpoint to store with an anonymized
// exchange. The endpoint of the raw path is the stable grouping ("/users/{id}"
// whatever the id became), but its literal segments are raw values the
// anonymizer rewrote in the stored path ("/people/123-45-6789"). So the
// result takes the segments of the endpoint of the anonymized path, except
// where the raw path was templated: a placeholder carries no data and keeps
// the grouping stable. A template the user configured (endpoint_patterns)
// is kept as it is.
func anonymizedEndpoint(n *pathnorm.Normalizer, method, rawPath, anonPath string) string {
	rawEP := n.Endpoint(method, rawPath)
	anonEP := n.Endpoint(method, anonPath)
	if rawEP == anonEP {
		return rawEP
	}
	_, rawT, ok1 := strings.Cut(rawEP, " ")
	prefix, anonT, ok2 := strings.Cut(anonEP, " ")
	if !ok1 || !ok2 {
		return anonEP
	}
	if slices.Contains(n.Patterns(), rawT) {
		return prefix + " " + rawT
	}
	rs, as := strings.Split(rawT, "/"), strings.Split(anonT, "/")
	if len(rs) != len(as) {
		return anonEP
	}
	for i := range as {
		if isBuiltinPlaceholder(rs[i]) {
			as[i] = rs[i]
		}
	}
	return prefix + " " + strings.Join(as, "/")
}

// isBuiltinPlaceholder reports whether seg is one of the placeholders the
// path normalizer substitutes for ids and other opaque values.
func isBuiltinPlaceholder(seg string) bool {
	switch seg {
	case "{id}", "{uuid}", "{hex}", "{token}", "{email}", "{date}":
		return true
	}
	return false
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
		if j.ex.Response.BodyTruncated && j.st.bodyCap > 0 {
			// max_body_bytes -1 drops bodies on purpose; only a real cap
			// counts. A capture queued before the cap or recording changed
			// would bring back a warning the user has just acted on.
			if cur := j.svc.state.Load(); cur != nil && cur.bodyCap == j.st.bodyCap && cur.svc.RecordingID == j.st.svc.RecordingID {
				j.svc.truncated.Add(1)
			}
		}
		j.svc.used.Add(stored)
		j.release(p)
		p.learn(j) // hands the exchange over: do not touch j.ex afterwards
	}
}

// learn adds a stored exchange to the service's virtualizer in hybrid mode,
// unless it is something a replay should not reproduce: writes, transient
// failures, conditional requests, upstream errors and truncated bodies.
//
// Whether to learn is decided on the service's state now, not on the state the
// request was handled with: a capture still queued when the mode changes from
// record to hybrid belongs in the new virtualizer, which was built from a
// store that did not hold it yet. An exchange that the new virtualizer did read
// from the store is recognized by its ID (Virtualizer.Add ignores it).
func (p *pipeline) learn(j *captureJob) {
	if j.conditional || !learnable(j.ex) {
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

// conditionalHeaders change the response without being part of the virtual
// key: what they provoke (304, 206, 412, 416) says nothing about the resource.
var conditionalHeaders = []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since", "If-Match", "If-Unmodified-Since"}

// hasConditionalHeader reports whether h carries a conditional or range header.
func hasConditionalHeader(h http.Header) bool {
	for _, k := range conditionalHeaders {
		if _, ok := h[k]; ok {
			return true
		}
	}
	return false
}

// learnable reports whether a captured exchange may be added to the live
// index: a GET, HEAD or OPTIONS that completed with a status the URL alone
// determines. Writes, upstream failures, truncated bodies, requests that
// carried conditional or range headers and the answers that depend on
// something other than the URL (1xx, 206, 304, 408, 412, 416, 425, 429, 5xx)
// are not learned; the upstream is asked again.
func learnable(ex *model.Exchange) bool {
	switch ex.Request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		return false
	}
	if ex.Error != "" || ex.Request.BodyTruncated || ex.Response.BodyTruncated {
		return false
	}
	switch st := ex.Response.Status; {
	case st < 200, st >= 500:
		return false
	case st == http.StatusPartialContent, st == http.StatusNotModified, st == http.StatusRequestTimeout,
		st == http.StatusPreconditionFailed, st == http.StatusRequestedRangeNotSatisfiable,
		st == http.StatusTooEarly, st == http.StatusTooManyRequests:
		return false
	}
	return !hasConditionalHeader(ex.Request.Header)
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
