package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"sync/atomic"
	"time"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
)

// serviceState is everything the request path needs to know about a service,
// immutable once published. The handler loads the current state through an
// atomic pointer, so Apply reconfigures a running service by building a new
// state and swapping it; requests in flight finish with the state they began
// with.
type serviceState struct {
	svc  model.Service // WithDefaults applied
	mode model.Mode
	up   *url.URL // nil when the service has no upstream (virtualize)
	norm *pathnorm.Normalizer
	tr   *http.Transport
	virt Virtualizer       // nil unless the mode serves a recording
	anon *anonymize.Engine // nil unless captures are anonymized

	recording bool    // the mode captures traffic (record, hybrid)
	bodyCap   int64   // bytes captured per body; 0 = bodies are not captured
	sample    float64 // fraction of requests captured, 1 = all

	// inflight counts requests using this state. A retired state is closed
	// (virtualizer, idle connections) once it drains; see acquire.
	inflight atomic.Int64
	retired  atomic.Bool
}

// derive returns a copy of st with fresh reference counters, for changes that
// replace a single component (a reloaded virtualizer, a new anonymizer).
func (st *serviceState) derive() *serviceState {
	return &serviceState{
		svc: st.svc, mode: st.mode, up: st.up, norm: st.norm, tr: st.tr,
		virt: st.virt, anon: st.anon,
		recording: st.recording, bodyCap: st.bodyCap, sample: st.sample,
	}
}

// needsVirtualizer reports whether mode serves recorded responses.
func needsVirtualizer(m model.Mode) bool {
	return m == model.ModeVirtualize || m == model.ModeHybrid
}

// effectiveProfileID returns the anonymization profile a service uses.
func effectiveProfileID(svc model.Service) string {
	if svc.ProfileID == "" {
		return model.DefaultProfileID
	}
	return svc.ProfileID
}

// sameVirtualization reports whether two service configurations produce the
// same virtual index, so a running virtualizer can be kept.
func sameVirtualization(a, b model.Service) bool {
	return a.RecordingID == b.RecordingID &&
		reflect.DeepEqual(a.Match, b.Match) &&
		a.Latency == b.Latency &&
		a.ErrorAmplification == b.ErrorAmplification &&
		a.ErrorFloor == b.ErrorFloor &&
		slices.Equal(a.EndpointPatterns, b.EndpointPatterns)
}

// buildState validates and assembles the state for svc, reusing the parts of
// prev that did not change. Resources it creates are closed again on error.
// recBytes is the recording's captured body bytes when svc captures traffic.
func (m *Manager) buildState(ctx context.Context, prev *serviceState, svc model.Service) (st *serviceState, recBytes int64, err error) {
	st = &serviceState{svc: svc, mode: svc.Mode, recording: svc.Mode.UsesUpstream() && svc.Mode.UsesRecording()}

	if prev != nil && slices.Equal(prev.svc.EndpointPatterns, svc.EndpointPatterns) {
		st.norm = prev.norm
	} else if st.norm, err = pathnorm.New(svc.EndpointPatterns); err != nil {
		return nil, 0, fmt.Errorf("%w: endpoint_patterns: %v", model.ErrValidation, err)
	}

	if svc.Upstream != "" {
		if st.up, err = url.Parse(svc.Upstream); err != nil {
			return nil, 0, fmt.Errorf("%w: upstream: %v", model.ErrValidation, err)
		}
	}
	if svc.Mode.UsesUpstream() {
		timeout := svc.Timeout.D()
		if prev != nil && prev.tr != nil && prev.svc.InsecureSkipVerify == svc.InsecureSkipVerify && prev.svc.Timeout == svc.Timeout {
			st.tr = prev.tr
		} else {
			st.tr = newTransport(svc.InsecureSkipVerify, timeout)
		}
	}

	st.sample = 1
	if svc.CaptureSample > 0 && svc.CaptureSample < 1 {
		st.sample = svc.CaptureSample
	}
	if svc.MaxBodyBytes > 0 {
		st.bodyCap = svc.MaxBodyBytes
	}

	if st.recording {
		if m.deps.Store == nil {
			return nil, 0, errors.New("proxy: no store configured")
		}
		rec, gerr := m.deps.Store.GetRecording(ctx, svc.RecordingID)
		if gerr != nil {
			return nil, 0, fmt.Errorf("recording %q: %w", svc.RecordingID, gerr)
		}
		recBytes = rec.Bytes
		if svc.AnonymizeCapture {
			if st.anon, err = m.loadAnonymizer(ctx, svc); err != nil {
				return nil, 0, err
			}
		}
	}

	if needsVirtualizer(svc.Mode) {
		if prev != nil && prev.virt != nil && sameVirtualization(prev.svc, svc) {
			st.virt = prev.virt
		} else {
			if m.deps.Virtualizers == nil {
				return nil, 0, errors.New("proxy: no virtualizer factory configured")
			}
			v, verr := m.deps.Virtualizers(ctx, svc, st.norm)
			if verr != nil {
				return nil, 0, fmt.Errorf("build virtualizer: %w", verr)
			}
			if v == nil {
				return nil, 0, errors.New("build virtualizer: factory returned nil")
			}
			st.virt = v
		}
	}
	return st, recBytes, nil
}

// discard closes the resources of a state that was built but never published.
func (m *Manager) discard(st, prev *serviceState) {
	if st.virt != nil && (prev == nil || st.virt != prev.virt) {
		m.closeVirtualizer(st.virt)
	}
	if st.tr != nil && (prev == nil || st.tr != prev.tr) {
		st.tr.CloseIdleConnections()
	}
}

func (m *Manager) closeVirtualizer(v Virtualizer) {
	if err := v.Close(); err != nil {
		m.log.Warn("closing virtualizer", "error", err)
	}
}

// loadAnonymizer builds the anonymizer of svc's profile from the store. A
// profile without a secret is an error: tokens would not be stable across
// restarts and captures could not be correlated with earlier ones.
func (m *Manager) loadAnonymizer(ctx context.Context, svc model.Service) (*anonymize.Engine, error) {
	id := effectiveProfileID(svc)
	p, err := m.deps.Store.GetProfile(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("anonymization profile %q: %w", id, err)
	}
	if p.Secret == "" {
		return nil, fmt.Errorf("anonymization profile %q has no secret", id)
	}
	eng, err := anonymize.New(p.WithDefaults())
	if err != nil {
		return nil, fmt.Errorf("anonymization profile %q: %w", id, err)
	}
	return eng, nil
}

// acquire returns the service's current state with its in-flight count raised,
// or nil when the service is stopped. The caller must call release.
//
// A state is retired only after the pointer to it was replaced. Because
// retirement sets the flag and then waits for the count to reach zero, a
// request that raised the count before the flag is waited for, and a request
// that sees the flag undoes its increment and loads the new state.
func (s *service) acquire() *serviceState {
	for {
		st := s.state.Load()
		if st == nil {
			return nil
		}
		st.inflight.Add(1)
		if !st.retired.Load() {
			return st
		}
		st.inflight.Add(-1)
	}
}

func (st *serviceState) release() { st.inflight.Add(-1) }

// retire marks old as replaced by next (nil when the service stopped) and
// closes what next no longer uses once the last request on old is done: the
// virtualizer and the idle connections of the upstream transport.
func (m *Manager) retire(old, next *serviceState) {
	old.retired.Store(true)
	var closers []func()
	if old.virt != nil && (next == nil || old.virt != next.virt) {
		closers = append(closers, func() { m.closeVirtualizer(old.virt) })
	}
	if old.tr != nil && (next == nil || old.tr != next.tr) {
		closers = append(closers, old.tr.CloseIdleConnections)
	}
	if len(closers) == 0 {
		return
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.waitDrained(old, 2*time.Minute)
		for _, c := range closers {
			c()
		}
	}()
}

// waitDrained waits until st has no requests, giving up after limit (streaming
// requests may hold a state for a long time). Once Shutdown cancels the root
// context the requests end by themselves, so only a short grace remains.
func (m *Manager) waitDrained(st *serviceState, limit time.Duration) {
	deadline := time.Now().Add(limit)
	rootDone := m.root.Done()
	for st.inflight.Load() > 0 {
		if rootDone != nil {
			select {
			case <-rootDone:
				deadline = earlier(deadline, time.Now().Add(time.Second))
				rootDone = nil
			default:
			}
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
