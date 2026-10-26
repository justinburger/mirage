package virtual

import (
	"context"
	"fmt"
	"hash/maphash"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
	"github.com/justinburger/mirage/internal/store"
)

// DefaultMaxResidentBytes is the default MaxResidentBytes (512 MiB).
const DefaultMaxResidentBytes = 512 << 20

// Match levels, as reported in Match.Level and in the X-Mirage-Match header.
const (
	LevelExact    = "exact"
	LevelPath     = "path"
	LevelTemplate = "template"
	// LevelMiss is the X-Mirage-Match value of a miss response.
	LevelMiss = "miss"
)

// Response headers Mirage adds to virtualized responses.
const (
	// HeaderMatch carries the match level (or "miss").
	HeaderMatch = "X-Mirage-Match"
	// HeaderInjected is "error" on responses where error amplification
	// replaced a success with an error.
	HeaderInjected = "X-Mirage-Injected"
	// HeaderError names why Mirage answered instead of the recording:
	// ErrorTruncated, or "store" when a lazily loaded body could not be read.
	HeaderError = "X-Mirage-Error"
	// ErrorTruncated is the HeaderError value for a matched recording whose
	// response body was truncated at capture.
	ErrorTruncated = "truncated"
)

// Options configures an Engine.
type Options struct {
	RecordingID        string
	Match              model.MatchConfig
	Latency            model.LatencyConfig
	ErrorAmplification float64
	ErrorFloor         float64
	// Normalizer computes endpoint templates; nil = pathnorm.Default.
	Normalizer       *pathnorm.Normalizer
	MaxResidentBytes int64 // 0 = DefaultMaxResidentBytes
	// Seed makes random choices reproducible in tests (0 = random seed).
	Seed uint64
	// TruncatedAsMiss makes a request whose only matching recordings have a
	// truncated response body a miss, so that a hybrid service proxies it to
	// the upstream (and records it again) instead of answering 502.
	TruncatedAsMiss bool
}

// OptionsFromService derives Options from a service (defaults applied).
func OptionsFromService(svc model.Service, n *pathnorm.Normalizer) Options {
	svc = svc.WithDefaults()
	return Options{
		RecordingID:        svc.RecordingID,
		Match:              svc.Match,
		Latency:            svc.Latency,
		ErrorAmplification: svc.ErrorAmplification,
		ErrorFloor:         svc.ErrorFloor,
		Normalizer:         n,
		TruncatedAsMiss:    svc.Mode == model.ModeHybrid,
	}
}

// Match is the result of a lookup. It is read-only: Exchange belongs to the
// index and must not be modified.
type Match struct {
	// Exchange is the recorded exchange that will be served. In lazy mode (see
	// Engine.Lazy) its Response.Body is nil: the body is loaded by Respond.
	Exchange *model.Exchange
	Level    string // "exact", "path" or "template"
	Injected bool   // response swapped by error amplification
	// Synthetic is true when Injected and no recorded error existed, so a
	// synthetic 503 is served (Exchange is then nil).
	Synthetic bool

	// it is the indexed form of Exchange (nil for a synthetic response), and
	// ttfb and dur are the recorded latency to simulate.
	it        *item
	ttfb, dur time.Duration
}

// Stats counts engine activity since Build.
type Stats = model.VirtualStats

// numShards is the number of index shards; a power of two.
const numShards = 64

// Engine is a virtualized service backed by one recording. It is safe for
// concurrent use; Add may run while requests are being served.
type Engine struct {
	st       store.Store
	opts     Options
	cfg      config
	seed     maphash.Seed
	norm     *pathnorm.Normalizer
	patterns map[string]struct{} // norm's endpoint_patterns
	rng      rng

	shards [numShards]shard
	stats  [numShards]counters

	// builtMaxID is the highest exchange ID Build read from the store. Add
	// ignores an exchange up to it: the build already indexed it. Set by Build
	// before the engine is shared.
	builtMaxID int64

	count    atomic.Int64 // exchanges indexed (Len)
	truncs   atomic.Int64 // requests answered 502 because the body was truncated
	resident atomic.Int64 // request+response body bytes held in memory
	lazy     bool         // set by Build before the engine is shared
	closed   atomic.Bool

	epMu sync.Mutex
	eps  map[string]*epool // endpoint → error-rate pools

	internMu sync.Mutex
	interned map[string][]hdr // shared response header sets (see internHeaders)

	cache *bodyCache // lazy bodies; nil without a store
	synth *bodyEntry // body of the synthetic 503

	gzPool sync.Pool    // *gzip.Writer
	gzips  atomic.Int64 // bodies compressed (diagnostics, tests)
	date   atomic.Pointer[dateStamp]

	// Test hooks.
	sleep func(ctx context.Context, d time.Duration) bool
	now   func() time.Time
}

// config is Options with defaults applied and derived flags.
type config struct {
	query      model.QueryMatch
	body       model.BodyMatch
	selection  model.Selection
	headers    []string // canonical names whose values are part of the exact key
	ignore     []string // query parameters excluded from matching
	templates  bool
	subset     bool
	pathSame   bool // the path level is identical to the exact level
	missStatus int

	latMode    model.LatencyMode
	multiplier float64
	fixed      time.Duration
	jitter     time.Duration

	amp, floor float64
	ampOn      bool
}

// Build loads the recording from the store.
func Build(ctx context.Context, st store.Store, opts Options) (*Engine, error) {
	if st == nil {
		return nil, fmt.Errorf("virtual: build: nil store: %w", model.ErrValidation)
	}
	if opts.RecordingID == "" {
		return nil, fmt.Errorf("virtual: build: recording id is required: %w", model.ErrValidation)
	}
	rec, err := st.GetRecording(ctx, opts.RecordingID)
	if err != nil {
		return nil, fmt.Errorf("virtual: build: load recording %q: %w", opts.RecordingID, err)
	}
	e := NewEngine(st, opts)
	e.lazy = rec.Bytes > e.opts.MaxResidentBytes

	iter := model.IterOptions{
		RequestBodies:  e.cfg.body != model.BodyIgnore,
		ResponseBodies: !e.lazy,
	}
	n := 0
	err = st.IterateExchanges(ctx, opts.RecordingID, iter, func(ex *model.Exchange) error {
		if n++; n&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		e.builtMaxID = max(e.builtMaxID, ex.ID)
		e.insert(ex, true)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("virtual: build: index recording %q: %w", opts.RecordingID, err)
	}
	e.finalize()
	return e, nil
}

// NewEngine returns an empty engine (populate with Add). st may be nil when
// everything stays resident.
func NewEngine(st store.Store, opts Options) *Engine {
	svc := model.Service{
		Match:              opts.Match,
		Latency:            opts.Latency,
		ErrorAmplification: opts.ErrorAmplification,
	}.WithDefaults()
	opts.Match = svc.Match
	opts.Latency = svc.Latency
	opts.ErrorAmplification = svc.ErrorAmplification
	opts.Match.IgnoreParams = slices.Clone(opts.Match.IgnoreParams)
	opts.Match.Headers = slices.Clone(opts.Match.Headers)
	if opts.MaxResidentBytes <= 0 {
		opts.MaxResidentBytes = DefaultMaxResidentBytes
	}
	if opts.Normalizer == nil {
		opts.Normalizer = pathnorm.Default
	}

	e := &Engine{
		st:   st,
		opts: opts,
		seed: maphash.MakeSeed(),
		norm: opts.Normalizer,
		eps:  make(map[string]*epool),
		now:  time.Now,
	}
	e.rng.init(opts.Seed)
	e.patterns = make(map[string]struct{})
	for _, p := range e.norm.Patterns() {
		e.patterns[p] = struct{}{}
	}
	e.cfg = newConfig(opts, svc)
	for i := range e.shards {
		e.shards[i].m = make(map[string]*entry)
	}
	if st != nil {
		e.cache = newBodyCache(opts.MaxResidentBytes)
	}
	e.synth = &bodyEntry{raw: []byte(syntheticBody)}
	return e
}

func newConfig(opts Options, svc model.Service) config {
	c := config{
		query:      svc.Match.Query,
		body:       svc.Match.Body,
		selection:  svc.Match.Selection,
		templates:  svc.Match.PathTemplates,
		missStatus: svc.Match.MissStatus,
		latMode:    svc.Latency.Mode,
		multiplier: svc.Latency.Multiplier,
		fixed:      max(svc.Latency.Fixed.D(), 0),
		jitter:     max(svc.Latency.Jitter.D(), 0),
		amp:        svc.ErrorAmplification,
		floor:      min(max(opts.ErrorFloor, 0), 1),
	}
	if c.missStatus < 100 || c.missStatus > 999 {
		c.missStatus = model.DefaultMissStatus
	}
	if c.multiplier <= 0 {
		c.multiplier = 1
	}
	if c.amp <= 0 {
		c.amp = 1
	}
	c.ampOn = c.amp != 1 || c.floor > 0
	c.subset = c.query == model.QuerySubset

	seen := make(map[string]bool)
	for _, h := range svc.Match.Headers {
		h = http.CanonicalHeaderKey(strings.TrimSpace(h))
		if h != "" && !seen[h] {
			seen[h] = true
			c.headers = append(c.headers, h)
		}
	}
	c.ignore = slices.Clone(svc.Match.IgnoreParams)
	c.pathSame = c.query != model.QueryExact && c.body == model.BodyIgnore && len(c.headers) == 0
	return c
}

// Lazy reports whether response bodies are loaded from the store on demand
// instead of being held in memory (see Options.MaxResidentBytes).
func (e *Engine) Lazy() bool { return e.lazy }

// Add indexes one exchange (hybrid mode, tests). The engine takes ownership
// of ex: the caller must not touch it afterwards. Add after Close is a no-op.
//
// An exchange that Build already read from the store (its ID is not above the
// highest ID Build saw) is ignored, so a capture that was stored while the
// engine was being built is not indexed twice.
func (e *Engine) Add(ex *model.Exchange) {
	if ex == nil || e.closed.Load() {
		return
	}
	if ex.ID != 0 && ex.ID <= e.builtMaxID {
		return
	}
	e.insert(ex, false)
}

// Lookup finds the response for a request. body is the full request body
// (already read by the caller). Returns nil on a miss.
func (e *Engine) Lookup(r *http.Request, body []byte) *Match {
	m := new(Match)
	if !e.lookup(r, body, m) {
		return nil
	}
	return m
}

// Serve is Lookup + Respond. It returns false, writing nothing, on a miss.
func (e *Engine) Serve(w http.ResponseWriter, r *http.Request, body []byte) bool {
	var m Match
	if !e.lookup(r, body, &m) {
		return false
	}
	e.Respond(w, r, &m)
	return true
}

// Len returns the number of indexed exchanges. Exchanges that failed without
// an upstream response (Exchange.Error set) are included: they are indexed
// for error rates although they are never served.
func (e *Engine) Len() int { return int(e.count.Load()) }

// Suppressed returns how many responses error amplification below 1 replaced
// by a recorded success. They are not part of Stats.Injected, which counts the
// errors that were injected (the responses that carry X-Mirage-Injected).
func (e *Engine) Suppressed() int64 {
	var n int64
	for i := range e.stats {
		n += e.stats[i].suppressed.Load()
	}
	return n
}

// Stats returns activity counters (Hits, Misses, Injected, Indexed,
// ByLevel). Injected counts injected errors only (see Suppressed). ByLevel
// always has the keys "exact", "path" and "template".
func (e *Engine) Stats() model.VirtualStats {
	var hits, misses, injected int64
	var lv [3]int64
	for i := range e.stats {
		c := &e.stats[i]
		hits += c.hits.Load()
		misses += c.misses.Load()
		injected += c.injected.Load()
		for j := range lv {
			lv[j] += c.level[j].Load()
		}
	}
	return model.VirtualStats{
		Indexed:   e.count.Load(),
		Hits:      hits,
		Misses:    misses,
		Injected:  injected,
		Truncated: e.truncs.Load(),
		ByLevel: map[string]int64{
			LevelExact:    lv[levelExact],
			LevelPath:     lv[levelPath],
			LevelTemplate: lv[levelTemplate],
		},
	}
}

// Close releases resources. Add after Close is a no-op; requests already
// matched can still be answered. Close is idempotent.
func (e *Engine) Close() error {
	if e.closed.Swap(true) {
		return nil
	}
	if e.cache != nil {
		e.cache.close()
	}
	return nil
}
