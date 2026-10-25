package metrics

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

const (
	// UnknownEndpoint is the endpoint recorded for observations whose
	// Endpoint is empty.
	UnknownEndpoint = "(unknown)"

	// SnapshotEndpoints is how many of the busiest endpoints a Snapshot
	// carries. Collector.Endpoints returns all of them.
	SnapshotEndpoints = 20

	// maxEndpointName bounds the stored length of an endpoint name so a
	// pathological caller cannot make every name megabytes long.
	maxEndpointName = 512

	// recentWindows is how many closed windows "recent" and the 10-second
	// rates cover.
	recentWindows = 10
)

// Status class slots; see statusClass.
const (
	classErr   = 0
	numClasses = 6
)

// classNames are the StatusClasses keys, indexed by status class slot: "err"
// first, then "1xx".."5xx".
var classNames = [numClasses]string{"err", "1xx", "2xx", "3xx", "4xx", "5xx"}

// statusClass returns the slot for an HTTP status: 0 for "err" (no response
// or a status outside 100-599), otherwise status/100.
func statusClass(status int) int {
	if status < 100 || status > 599 {
		return classErr
	}
	return status / 100
}

// Observation is one completed request.
type Observation struct {
	Endpoint string        // e.g. "GET /users/{id}"
	Status   int           // 0 = no response (transport error)
	Duration time.Duration // service time
	BytesIn  int64         // request bytes
	BytesOut int64         // response bytes
	// Error marks the request as an error. Callers normally set it to
	// model.IsErrorStatus(Status).
	Error bool
}

// endpointStats is the cumulative state for one endpoint. The request count is
// latency.count.
type endpointStats struct {
	name    string
	errors  atomic.Int64
	latency Histogram
}

func (e *endpointStats) summary() model.EndpointSummary {
	l := e.latency.Summary()
	return model.EndpointSummary{
		Endpoint: e.name,
		Count:    l.Count,
		Errors:   e.errors.Load(),
		Latency:  l,
	}
}

// Collector aggregates traffic for one service or replay job.
//
// Observe touches only atomics: the cumulative counters, the cumulative
// latency histogram, and the per-endpoint stats found through a copy-on-write
// map. Tick turns the difference between the cumulative state now and at the
// previous Tick into the closed one-second window, so every observation lands
// in exactly one window no matter how it races with Tick, and the request path
// never touches a lock or allocates a window.
type Collector struct {
	// Read-mostly after construction.
	name  string
	kind  model.CollectorKind
	id    string
	eps   atomic.Pointer[map[string]*endpointStats]
	other atomic.Pointer[endpointStats] // overflow endpoint; nil until needed
	epMu  sync.Mutex                    // serialises endpoint creation and Reset
	since atomic.Int64                  // UnixNano of creation or last Reset
	_     [cacheLineSize]byte

	// winMax and winMinC are the extremes since the last Tick, swapped out by
	// Tick. winMinC is stored as MaxInt64-min (0 = none yet), like
	// Histogram.minC. Observe only reads them unless it sets a new extreme, so
	// they live on their own line, away from the counters below.
	winMax  atomic.Int64
	winMinC atomic.Int64
	_       [cacheLineSize]byte

	// Written on every Observe.
	errors   atomic.Int64
	bytesIn  atomic.Int64
	bytesOut atomic.Int64
	classes  [numClasses]atomic.Int64
	_        [cacheLineSize]byte

	// inflight is written twice per request by the caller.
	inflight atomic.Int64
	_        [cacheLineSize]byte

	// latency is the cumulative histogram; latency.count is the request
	// total.
	latency Histogram

	// mu guards everything below. Only Tick, Series, Snapshot and Reset take
	// it; Observe never does.
	mu     sync.Mutex
	ticked bool
	// lastSec is the Unix second of the newest point in the ring and lastNow
	// the time passed to the Tick that closed it.
	lastSec int64
	lastNow time.Time
	// lastInFlight is the previous point's in-flight value, reused for
	// gap-filling points.
	lastInFlight int64

	// Cumulative values as of the previous Tick; the next window is the
	// difference.
	prev      *[numBuckets]uint64
	prevCount int64
	prevErrs  int64
	prevIn    int64
	prevOut   int64
	prevSum   int64

	// ring holds closed points, oldest at ring[head] once full (head is 0
	// while it is still growing).
	ring []model.SeriesPoint
	head int

	// wins holds the last recentWindows closed windows (circular, next write
	// at winPos); recent is the sum of their buckets.
	wins   [recentWindows]windowStats
	winPos int
	winLen int
	recent *Histogram
}

// windowStats is one closed one-second window.
type windowStats struct {
	sparseCounts // latency bucket deltas
	requests     int64
	errors       int64
	bytesIn      int64
	bytesOut     int64
	sum          int64 // ns
	min, max     int64 // ns; meaningful when the window saw traffic
}

func (w *windowStats) reset() {
	w.sparseCounts.reset()
	w.requests, w.errors, w.bytesIn, w.bytesOut, w.sum = 0, 0, 0, 0, 0
	w.min, w.max = 0, 0
}

// NewCollector returns a standalone collector (the registry creates its own).
func NewCollector(name string, kind model.CollectorKind, id string) *Collector {
	c := &Collector{
		name:   name,
		kind:   kind,
		id:     id,
		prev:   new([numBuckets]uint64),
		recent: NewHistogram(),
		ring:   make([]model.SeriesPoint, 0, 64),
	}
	empty := map[string]*endpointStats{}
	c.eps.Store(&empty)
	c.since.Store(time.Now().UnixNano())
	return c
}

// Name returns the collector name ("service:<id>", "replay:<id>").
func (c *Collector) Name() string { return c.name }

// Kind returns the collector kind.
func (c *Collector) Kind() model.CollectorKind { return c.kind }

// ID returns the service or replay job ID the collector was created for.
func (c *Collector) ID() string { return c.id }

// Observe records one completed request. It is lock-free and, for an endpoint
// the collector has already seen, allocation-free.
func (c *Collector) Observe(o Observation) {
	d := max(int64(o.Duration), 0)
	ep := c.endpoint(o.Endpoint)

	c.classes[statusClass(o.Status)].Add(1)
	if o.Error {
		c.errors.Add(1)
		ep.errors.Add(1)
	}
	if o.BytesIn > 0 {
		c.bytesIn.Add(o.BytesIn)
	}
	if o.BytesOut > 0 {
		c.bytesOut.Add(o.BytesOut)
	}
	atomicMax(&c.winMax, d)
	atomicMax(&c.winMinC, math.MaxInt64-d)
	ep.latency.observe(d)
	c.latency.observe(d)
}

// InFlight adjusts the in-flight gauge by delta (+1 on start, -1 on finish).
func (c *Collector) InFlight(delta int64) { c.inflight.Add(delta) }

// endpoint returns the stats for name, creating them on first use.
func (c *Collector) endpoint(name string) *endpointStats {
	if name == "" {
		name = UnknownEndpoint
	} else if len(name) > maxEndpointName {
		name = name[:maxEndpointName]
	}
	tbl := *c.eps.Load()
	if ep, ok := tbl[name]; ok {
		return ep
	}
	if name == OtherEndpoint || len(tbl) >= MaxEndpoints {
		return c.otherStats()
	}
	return c.addEndpoint(name)
}

// otherStats returns the overflow endpoint, creating it once.
func (c *Collector) otherStats() *endpointStats {
	if o := c.other.Load(); o != nil {
		return o
	}
	o := &endpointStats{name: OtherEndpoint}
	if c.other.CompareAndSwap(nil, o) {
		return o
	}
	return c.other.Load()
}

// addEndpoint is the slow path of endpoint: it publishes a new copy of the
// endpoint table containing name.
func (c *Collector) addEndpoint(name string) *endpointStats {
	c.epMu.Lock()
	defer c.epMu.Unlock()
	tbl := *c.eps.Load()
	if ep, ok := tbl[name]; ok {
		return ep
	}
	if len(tbl) >= MaxEndpoints {
		return c.otherStats()
	}
	// Clone: the caller's string may alias a pooled or reused buffer.
	ep := &endpointStats{name: strings.Clone(name)}
	next := make(map[string]*endpointStats, len(tbl)+1)
	for k, v := range tbl {
		next[k] = v
	}
	next[ep.name] = ep
	c.eps.Store(&next)
	return ep
}

// rankedEndpoint pairs endpoint stats with the request count used to order
// them, read once so sorting sees stable keys.
type rankedEndpoint struct {
	ep *endpointStats
	n  int64
}

// rankedEndpoints returns every endpoint ordered by request count descending,
// then name ascending.
func (c *Collector) rankedEndpoints() []rankedEndpoint {
	tbl := *c.eps.Load()
	out := make([]rankedEndpoint, 0, len(tbl)+1)
	for _, ep := range tbl {
		out = append(out, rankedEndpoint{ep, ep.latency.Count()})
	}
	if o := c.other.Load(); o != nil {
		out = append(out, rankedEndpoint{o, o.latency.Count()})
	}
	slices.SortFunc(out, func(a, b rankedEndpoint) int {
		if r := cmp.Compare(b.n, a.n); r != 0 {
			return r
		}
		return strings.Compare(a.ep.name, b.ep.name)
	})
	return out
}

// Endpoints returns cumulative per-endpoint summaries, most requests first
// (ties by name). Observations beyond MaxEndpoints appear as OtherEndpoint.
func (c *Collector) Endpoints() []model.EndpointSummary {
	ranked := c.rankedEndpoints()
	out := make([]model.EndpointSummary, len(ranked))
	for i, r := range ranked {
		out[i] = r.ep.summary()
	}
	return out
}

// topEndpoints summarises the busiest limit endpoints.
func (c *Collector) topEndpoints(limit int) []model.EndpointSummary {
	ranked := c.rankedEndpoints()
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	if len(ranked) == 0 {
		return nil
	}
	out := make([]model.EndpointSummary, len(ranked))
	for i, r := range ranked {
		out[i] = r.ep.summary()
	}
	return out
}

// Latency returns a copy of the cumulative latency histogram.
func (c *Collector) Latency() *Histogram { return c.latency.Clone() }

// Reset clears all counters, histograms and series and sets Since to now. The
// in-flight gauge is left alone: it tracks requests that are still running and
// their completion will decrement it.
func (c *Collector) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.epMu.Lock()
	empty := map[string]*endpointStats{}
	c.eps.Store(&empty)
	c.other.Store(nil)
	c.epMu.Unlock()

	c.errors.Store(0)
	c.bytesIn.Store(0)
	c.bytesOut.Store(0)
	for i := range c.classes {
		c.classes[i].Store(0)
	}
	c.winMax.Store(0)
	c.winMinC.Store(0)
	c.latency.Reset()

	clear(c.prev[:])
	c.prevCount, c.prevErrs, c.prevIn, c.prevOut, c.prevSum = 0, 0, 0, 0, 0
	c.ticked, c.lastSec, c.lastNow, c.lastInFlight = false, 0, time.Time{}, 0
	c.ring = c.ring[:0]
	c.head = 0
	for i := range c.wins {
		c.wins[i].reset()
	}
	c.winPos, c.winLen = 0, 0
	c.recent.Reset()
	c.since.Store(time.Now().UnixNano())
}
