package proxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
)

// Manager owns every service listener.
type Manager struct {
	deps    Deps
	log     *slog.Logger
	httpLog *log.Logger // net/http and ReverseProxy diagnostics, at debug level

	// root bounds listeners, request handling and capture workers; Shutdown
	// cancels it once the listeners have drained.
	root   context.Context
	cancel context.CancelFunc
	pipe   *pipeline
	wg     sync.WaitGroup // listener, drain and retirement goroutines

	mu       sync.Mutex
	services map[string]*service
	closed   bool

	shutdownOnce sync.Once
	shutdownErr  error
}

// NewManager returns a manager with no services.
func NewManager(deps Deps) *Manager {
	deps = deps.withDefaults()
	m := &Manager{
		deps:     deps,
		log:      deps.Logger,
		httpLog:  slog.NewLogLogger(deps.Logger.Handler(), slog.LevelDebug),
		services: make(map[string]*service),
	}
	m.root, m.cancel = context.WithCancel(context.Background())
	m.pipe = newPipeline(m)
	return m
}

// collector returns the metrics collector of a service.
func (m *Manager) collector(id string) *metrics.Collector {
	name := metrics.ServiceCollectorName(id)
	if m.deps.Metrics == nil {
		return metrics.NewCollector(name, model.KindService, id)
	}
	return m.deps.Metrics.Collector(name, model.KindService, id)
}

// get returns the known service id, or nil.
func (m *Manager) get(id string) *service {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.services[id]
}

// serviceFor returns the service id, creating it when create is set.
func (m *Manager) serviceFor(id string, create bool) (*service, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	s := m.services[id]
	if s == nil && create {
		s = newService(m, id)
		m.services[id] = s
	}
	return s, nil
}

// lockService locks the lifecycle of service id, retrying when the entry was
// removed between lookup and lock. It returns nil when there is no such
// service (and create is false).
func (m *Manager) lockService(id string, create bool) (*service, error) {
	for {
		s, err := m.serviceFor(id, create)
		if err != nil || s == nil {
			return nil, err
		}
		s.lifeMu.Lock()
		if !s.removed {
			return s, nil
		}
		s.lifeMu.Unlock()
	}
}

// Apply starts, reconfigures or stops a service to match svc (defaults are
// applied with WithDefaults).
//
// A running service is reconfigured without dropping its listener or its
// requests in flight; only a ListenAddr or TLS change rebinds the socket. If
// the new configuration cannot be built the service keeps running with the
// old one and the error is returned (and shown in ServiceStatus.Error).
func (m *Manager) Apply(ctx context.Context, svc model.Service) error {
	svc = svc.WithDefaults()
	if svc.ID == "" {
		return fmt.Errorf("%w: service id is required", model.ErrValidation)
	}
	s, err := m.lockService(svc.ID, true)
	if err != nil {
		return err
	}
	defer s.lifeMu.Unlock()
	return s.apply(ctx, svc)
}

// Remove stops a service, forgets it and removes its metrics collector.
// Removing an unknown service is not an error.
func (m *Manager) Remove(ctx context.Context, id string) error {
	s, err := m.lockService(id, false)
	if err != nil || s == nil {
		return err
	}
	s.removed = true
	s.stop()
	s.lifeMu.Unlock()

	m.mu.Lock()
	if m.services[id] == s {
		delete(m.services, id)
	}
	m.mu.Unlock()
	if m.deps.Metrics != nil {
		m.deps.Metrics.Remove(metrics.ServiceCollectorName(id))
	}
	return nil
}

// Reload rebuilds a service's virtualizer from its recording. Services whose
// mode does not serve a recording, and stopped services, are left alone.
func (m *Manager) Reload(ctx context.Context, id string) error {
	s, err := m.lockService(id, false)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("service %q: %w", id, model.ErrNotFound)
	}
	defer s.lifeMu.Unlock()
	return s.reloadVirtualizer(ctx)
}

// ProfileChanged applies a changed anonymization profile to every running
// service that uses profileID with anonymize_capture on: its capture
// anonymizer is reloaded (record and hybrid) and its virtualizer is rebuilt
// (virtualize and hybrid), because the virtualizer matches requests in their
// anonymized form, with the profile's anonymizer. A service whose profile
// cannot be loaded keeps its previous anonymizer and virtualizer; the errors
// are joined.
func (m *Manager) ProfileChanged(ctx context.Context, profileID string) error {
	if profileID == "" {
		profileID = model.DefaultProfileID
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	all := make([]*service, 0, len(m.services))
	for _, s := range m.services {
		all = append(all, s)
	}
	m.mu.Unlock()

	var errs []error
	for _, s := range all {
		s.lifeMu.Lock()
		if !s.removed {
			if st := s.state.Load(); st != nil && st.svc.AnonymizeCapture && effectiveProfileID(st.svc) == profileID {
				if err := s.reloadProfile(ctx); err != nil {
					errs = append(errs, fmt.Errorf("service %s: %w", s.id, err))
				}
			}
		}
		s.lifeMu.Unlock()
	}
	return errors.Join(errs...)
}

// Status returns a service's runtime status (zero Running if unknown).
func (m *Manager) Status(id string) model.ServiceStatus {
	if s := m.get(id); s != nil {
		return s.status()
	}
	return model.ServiceStatus{ServiceID: id}
}

// Statuses returns every known service's status, sorted by service ID.
func (m *Manager) Statuses() []model.ServiceStatus {
	m.mu.Lock()
	all := make([]*service, 0, len(m.services))
	for _, s := range m.services {
		all = append(all, s)
	}
	m.mu.Unlock()
	out := make([]model.ServiceStatus, len(all))
	for i, s := range all {
		out[i] = s.status()
	}
	slices.SortFunc(out, func(a, b model.ServiceStatus) int {
		switch {
		case a.ServiceID < b.ServiceID:
			return -1
		case a.ServiceID > b.ServiceID:
			return 1
		}
		return 0
	})
	return out
}

// Handler returns the request handler of a running service (tests, embedding).
func (m *Manager) Handler(id string) (http.Handler, bool) {
	s := m.get(id)
	if s == nil || s.state.Load() == nil {
		return nil, false
	}
	return s, true
}

// Shutdown stops every listener gracefully (requests in flight finish), then
// drains the capture queue and flushes pending batches before returning. All
// of it is bounded by ctx: when ctx ends first, the remaining work is
// abandoned (listeners are closed, queued captures are dropped and counted)
// and the error wraps ctx's. Shutdown is idempotent; later calls return the
// first call's result.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.shutdownOnce.Do(func() { m.shutdownErr = m.shutdown(ctx) })
	return m.shutdownErr
}

func (m *Manager) shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	all := make([]*service, 0, len(m.services))
	for _, s := range m.services {
		all = append(all, s)
	}
	m.mu.Unlock()

	// 1. Stop accepting and let requests in flight finish.
	var listeners []*listener
	for _, s := range all {
		s.lifeMu.Lock()
		s.removed = true
		if s.lis != nil {
			s.lis.beginStop()
			listeners = append(listeners, s.lis)
			s.lis = nil
		}
		s.lifeMu.Unlock()
	}
	var firstErr error
	var mu sync.Mutex
	var lw sync.WaitGroup
	for _, l := range listeners {
		lw.Add(1)
		go func() {
			defer lw.Done()
			if err := l.shutdown(ctx); err != nil {
				_ = l.srv.Close()
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("shutting down %s: %w", l.addr, err)
				}
				mu.Unlock()
			}
			<-l.done
		}()
	}
	lw.Wait()

	// 2. End what is left (hung faults, upgraded connections) and wait for
	// the handlers, which may still queue their captures.
	m.cancel()
	if err := m.waitIdle(ctx, all); err != nil && firstErr == nil {
		firstErr = err
	}

	// 3. Drain the capture queue into the store.
	if err := m.pipe.stop(ctx); err != nil && firstErr == nil {
		firstErr = err
	}

	// 4. Release virtualizers and upstream connections.
	for _, s := range all {
		s.statMu.Lock()
		s.running, s.addr = false, ""
		s.statMu.Unlock()
		if st := s.state.Swap(nil); st != nil {
			m.retire(st, nil)
		}
	}
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		if firstErr == nil {
			firstErr = fmt.Errorf("waiting for background work: %w", ctx.Err())
		}
	}
	return firstErr
}

// waitIdle waits until no request is in flight on any of the services.
func (m *Manager) waitIdle(ctx context.Context, all []*service) error {
	busy := func() bool {
		for _, s := range all {
			if s.active.Load() > 0 {
				return true
			}
		}
		return false
	}
	for busy() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for requests in flight: %w", ctx.Err())
		case <-time.After(2 * time.Millisecond):
		}
	}
	return nil
}
