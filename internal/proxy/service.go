package proxy

import (
	"context"
	"fmt"
	"net/http/httputil"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
)

// service is the runtime of one virtual service: its listener, its current
// state and its counters. It is also the http.Handler of the service.
type service struct {
	m  *Manager
	id string
	rp *httputil.ReverseProxy

	// lifeMu serializes lifecycle operations (Apply, Remove, Reload,
	// ProfileChanged, Shutdown). It is never held by the request path, and
	// Status never takes it, so a slow Apply cannot stall either.
	lifeMu  sync.Mutex
	removed bool          // set under lifeMu when the service was removed or the manager shut down
	cfg     model.Service // last applied configuration
	lis     *listener     // nil when stopped

	// state is the configuration requests run with; nil when stopped.
	state atomic.Pointer[serviceState]
	// collector is assigned under lifeMu before the first state is published
	// and read by requests only after loading a state.
	collector *metrics.Collector
	active    atomic.Int64 // requests in flight, for Shutdown

	statMu    sync.RWMutex
	running   bool
	addr      string
	mode      model.Mode
	startedAt time.Time
	opErr     string // last failed Apply or Reload; cleared by the next success
	rtErr     string // last runtime problem (store failure, failed anonymization); cleared by Apply
	rtWarn    string // a runtime note that leaves the service working (capture limit); cleared by Apply

	captured   atomic.Int64
	dropped    atomic.Int64
	skipped    atomic.Int64
	used       atomic.Int64 // body bytes stored in the recording (capture limit)
	pending    atomic.Int64 // body bytes of this service waiting in the capture queue
	limitNoted atomic.Bool  // "capture limit reached" is set in rtWarn
}

func newService(m *Manager, id string) *service {
	s := &service{m: m, id: id}
	s.rp = s.newProxy()
	return s
}

// listenerChanged reports whether moving from old to next needs a new socket.
func listenerChanged(old, next model.Service) bool {
	return old.ListenAddr != next.ListenAddr || !reflect.DeepEqual(old.TLS, next.TLS)
}

// apply starts, reconfigures or stops the service to match svc. lifeMu is
// held. On error a running service keeps its previous configuration (except
// when its own socket had to be released to bind the new one).
func (s *service) apply(ctx context.Context, svc model.Service) error {
	m := s.m
	if !svc.Enabled {
		s.cfg = svc
		s.stop()
		s.statMu.Lock()
		s.mode, s.opErr, s.rtErr, s.rtWarn = svc.Mode, "", "", ""
		s.statMu.Unlock()
		return nil
	}
	if err := svc.Validate(); err != nil {
		return s.fail(err)
	}
	prev := s.state.Load()
	next, recBytes, err := m.buildState(ctx, prev, svc)
	if err != nil {
		return s.fail(err)
	}
	if next.recording {
		m.pipe.start()
	}
	if s.collector == nil {
		s.collector = m.collector(s.id)
	}

	fresh := s.lis == nil
	var newLis, oldLis *listener
	switch {
	case fresh:
		if newLis, err = s.bind(svc); err != nil {
			m.discard(next, prev)
			return s.fail(err)
		}
		s.captured.Store(0)
		s.dropped.Store(0)
		s.skipped.Store(0)
	case listenerChanged(s.cfg, svc):
		oldLis = s.lis
		if sameSocket(s.cfg.ListenAddr, svc.ListenAddr) {
			// The port must be free before it can be bound again.
			m.drain(oldLis)
			s.lis, oldLis = nil, nil
			if newLis, err = s.bind(svc); err != nil {
				m.discard(next, prev)
				s.stop()
				return s.fail(err)
			}
		} else if newLis, err = s.bind(svc); err != nil {
			// Bind first: on failure the old listener keeps serving.
			m.discard(next, prev)
			return s.fail(err)
		}
	}

	if prev == nil || !prev.recording || prev.svc.RecordingID != svc.RecordingID {
		s.used.Store(recBytes)
	}
	s.limitNoted.Store(false)
	s.state.Store(next)
	if newLis != nil {
		s.lis = newLis
		s.serve(newLis)
	}
	if oldLis != nil {
		m.drain(oldLis)
	}
	if prev != nil {
		m.retire(prev, next)
	}
	s.cfg = svc
	s.statMu.Lock()
	s.running, s.addr, s.mode, s.opErr, s.rtErr, s.rtWarn = true, s.lis.addr, svc.Mode, "", "", ""
	if fresh {
		s.startedAt = time.Now()
	}
	s.statMu.Unlock()
	return nil
}

// stop closes the listener (in-flight requests drain in the background) and
// drops the state. lifeMu is held.
func (s *service) stop() {
	if s.lis != nil {
		s.m.drain(s.lis)
		s.lis = nil
	}
	if st := s.state.Swap(nil); st != nil {
		s.m.retire(st, nil)
	}
	s.statMu.Lock()
	s.running, s.addr = false, ""
	s.statMu.Unlock()
}

// fail records err as the result of the last lifecycle operation and returns
// it.
func (s *service) fail(err error) error {
	s.statMu.Lock()
	s.opErr = err.Error()
	s.statMu.Unlock()
	return err
}

// setRuntimeErr records a problem found while serving (a store failure, a
// failed anonymization).
func (s *service) setRuntimeErr(msg string) {
	s.statMu.Lock()
	s.rtErr = msg
	s.statMu.Unlock()
}

// noteLimit reports "capture limit reached" once per configuration. It is a
// warning, not an error: the service keeps serving, only capturing stopped.
func (s *service) noteLimit() {
	if s.limitNoted.CompareAndSwap(false, true) {
		s.statMu.Lock()
		s.rtWarn = "capture limit reached"
		s.statMu.Unlock()
	}
}

// listenerFailed records that l stopped serving unexpectedly.
func (s *service) listenerFailed(l *listener, err error) {
	s.statMu.Lock()
	if s.addr == l.addr {
		s.running = false
	}
	s.rtErr = fmt.Sprintf("listener failed: %v", err)
	s.statMu.Unlock()
}

// reloadVirtualizer rebuilds the virtualizer from the recording. lifeMu is held.
func (s *service) reloadVirtualizer(ctx context.Context) error {
	st := s.state.Load()
	if st == nil || !needsVirtualizer(st.mode) {
		return nil
	}
	if s.m.deps.Virtualizers == nil {
		return s.fail(fmt.Errorf("proxy: no virtualizer factory configured"))
	}
	v, err := s.m.deps.Virtualizers(ctx, st.svc, st.norm)
	if err != nil {
		return s.fail(fmt.Errorf("build virtualizer: %w", err))
	}
	if v == nil {
		return s.fail(fmt.Errorf("build virtualizer: factory returned nil"))
	}
	next := st.derive()
	next.virt = v
	s.state.Store(next)
	s.m.retire(st, next)
	s.statMu.Lock()
	s.opErr = ""
	s.statMu.Unlock()
	return nil
}

// reloadAnonymizer rebuilds the anonymizer from the stored profile. lifeMu is
// held.
func (s *service) reloadAnonymizer(ctx context.Context) error {
	st := s.state.Load()
	if st == nil || st.anon == nil {
		return nil
	}
	eng, err := s.m.loadAnonymizer(ctx, st.svc)
	if err != nil {
		return s.fail(err)
	}
	next := st.derive()
	next.anon = eng
	s.state.Store(next)
	s.m.retire(st, next)
	return nil
}

// status returns the runtime status of the service.
func (s *service) status() model.ServiceStatus {
	s.statMu.RLock()
	out := model.ServiceStatus{
		ServiceID: s.id,
		Running:   s.running,
		Addr:      s.addr,
		Mode:      s.mode,
		Error:     s.opErr,
	}
	if out.Error == "" {
		out.Error = s.rtErr
	}
	if s.running {
		out.Warning = s.rtWarn
	}
	if s.running && !s.startedAt.IsZero() {
		t := s.startedAt
		out.StartedAt = &t
	}
	s.statMu.RUnlock()
	out.Captured = s.captured.Load()
	out.CaptureDropped = s.dropped.Load()
	out.CaptureSkipped = s.skipped.Load()
	if st := s.state.Load(); st != nil && st.virt != nil {
		vs := st.virt.Stats()
		out.VirtualHits, out.VirtualMisses = vs.Hits, vs.Misses
		out.VirtualInjected, out.IndexedExchanges = vs.Injected, vs.Indexed
	}
	return out
}
