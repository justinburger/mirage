package metrics

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// Update is published by the registry once per second.
type Update struct {
	// Time is the tick (a whole second), always in UTC.
	Time time.Time `json:"time"`
	// Snapshots of every collector.
	Snapshots []model.StatsSnapshot `json:"snapshots"`
	// Points holds the SeriesPoint each collector closed on this tick, keyed
	// by collector name.
	Points map[string]model.SeriesPoint `json:"points"`
}

// Registry owns named collectors, ticks them once per second and fans the
// result out to subscribers.
//
// All methods are safe for concurrent use. The zero value is not ready for
// use; call NewRegistry.
type Registry struct {
	mu   sync.RWMutex
	cols map[string]*Collector

	subMu   sync.Mutex
	subs    map[uint64]chan Update
	nextSub uint64

	// interval is the tick period; always time.Second outside tests.
	interval time.Duration
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		cols:     make(map[string]*Collector),
		subs:     make(map[uint64]chan Update),
		interval: time.Second,
	}
}

// Collector returns the named collector, creating it if needed. When the
// collector already exists it is returned as is, whatever kind and id are
// passed.
func (r *Registry) Collector(name string, kind model.CollectorKind, id string) *Collector {
	r.mu.RLock()
	c, ok := r.cols[name]
	r.mu.RUnlock()
	if ok {
		return c
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.cols[name]; ok {
		return c
	}
	c = NewCollector(name, kind, id)
	r.cols[name] = c
	return c
}

// Get returns the named collector if it exists.
func (r *Registry) Get(name string) (*Collector, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.cols[name]
	return c, ok
}

// Remove deletes the named collector. Holders of the collector can keep using
// it, but it is no longer ticked or exposed.
func (r *Registry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cols, name)
}

// List returns all collectors sorted by name.
func (r *Registry) List() []*Collector {
	r.mu.RLock()
	out := make([]*Collector, 0, len(r.cols))
	for _, c := range r.cols {
		out = append(out, c)
	}
	r.mu.RUnlock()
	slices.SortFunc(out, func(a, b *Collector) int { return strings.Compare(a.name, b.name) })
	return out
}

// Tick ticks every collector with now, publishes the resulting Update to all
// subscribers and returns it. Run calls it once per second; it is exported so
// callers and tests can drive the registry with their own clock.
//
// The Update is shared by every subscriber and must be treated as read-only.
func (r *Registry) Tick(now time.Time) Update {
	cols := r.List()
	u := Update{
		Time:      now.UTC(),
		Snapshots: make([]model.StatsSnapshot, 0, len(cols)),
		Points:    make(map[string]model.SeriesPoint, len(cols)),
	}
	for _, c := range cols {
		u.Points[c.name] = c.Tick(now)
		u.Snapshots = append(u.Snapshots, c.Snapshot())
	}
	r.publish(u)
	return u
}

// Run ticks all collectors once per second and publishes Updates until ctx
// is done. Ticks are aligned to wall-clock seconds, and the time handed to the
// collectors is the whole second itself, so every collector closes the same
// second regardless of timer jitter. Call Run once per registry.
func (r *Registry) Run(ctx context.Context) {
	interval := r.interval
	if interval <= 0 {
		interval = time.Second
	}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		next := nextBoundary(time.Now(), interval)
		timer.Reset(time.Until(next))
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		at := time.Now()
		if at.Before(next) {
			at = next
		}
		r.Tick(at.Truncate(interval))
	}
}

// nextBoundary returns the first multiple of interval (since the zero time,
// so whole seconds align to the Unix epoch) strictly after now.
func nextBoundary(now time.Time, interval time.Duration) time.Time {
	return now.Truncate(interval).Add(interval)
}

// Subscribe returns a channel receiving every Update and a cancel func that
// unsubscribes and closes the channel. buffer is the channel capacity (values
// below 1 are treated as 1). A subscriber that falls behind misses updates
// rather than slowing the ticker: when its channel is full the update is
// dropped for that subscriber only.
//
// cancel is safe to call any number of times and from any goroutine; the
// channel is closed exactly once and never receives another update after
// cancel returns.
func (r *Registry) Subscribe(buffer int) (<-chan Update, func()) {
	ch := make(chan Update, max(buffer, 1))
	r.subMu.Lock()
	id := r.nextSub
	r.nextSub++
	r.subs[id] = ch
	r.subMu.Unlock()

	cancel := func() {
		r.subMu.Lock()
		defer r.subMu.Unlock()
		if c, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(c)
		}
	}
	return ch, cancel
}

// publish hands u to every subscriber without blocking. It holds subMu while
// sending so cancel cannot close a channel mid-send; sends never block, so
// the critical section is short.
func (r *Registry) publish(u Update) {
	r.subMu.Lock()
	defer r.subMu.Unlock()
	for _, ch := range r.subs {
		select {
		case ch <- u:
		default:
		}
	}
}

// Subscribers returns the number of active subscriptions.
func (r *Registry) Subscribers() int {
	r.subMu.Lock()
	defer r.subMu.Unlock()
	return len(r.subs)
}
