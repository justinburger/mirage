package anonymize

import (
	"fmt"
	"slices"
	"sync"

	"github.com/justinburger/mirage/internal/model"
)

// maxRules bounds the number of rules in a profile. The JSONPath NFA packs the
// rule index into 16 bits, and no real profile comes close.
const maxRules = 4096

// Engine applies one anonymization profile.
//
// An Engine is immutable after New and safe for concurrent use by any number
// of goroutines.
type Engine struct {
	prof model.AnonymizationProfile
	c    *compiled
	k    *keyer
	// salted caches the engines WithSalt derived, shared by all of them.
	salted *saltCache
}

// maxSaltedEngines bounds the engines WithSalt keeps. Replay with per-copy
// identities asks for the same few salts over and over; a caller that makes up
// a new salt per request just churns the cache.
const maxSaltedEngines = 1024

type saltCache struct {
	mu sync.RWMutex
	m  map[string]*Engine
}

// cloneProfile copies the slices of p so that defaulting and keeping the
// profile never touches the caller's data.
func cloneProfile(p model.AnonymizationProfile) model.AnonymizationProfile {
	p.Detectors = slices.Clone(p.Detectors)
	p.RedactHeaders = slices.Clone(p.RedactHeaders)
	p.Rules = slices.Clone(p.Rules)
	return p
}

// New compiles a profile. The profile should already have defaults applied
// (model.AnonymizationProfile.WithDefaults); New applies them again, which is
// harmless. An empty Secret is allowed: a random per-process secret is used,
// so tokens are then not stable across restarts. It returns a
// *model.ValidationError listing every bad rule (invalid JSONPath, regex,
// path template, unknown strategy and so on).
func New(p model.AnonymizationProfile) (*Engine, error) {
	p = cloneProfile(p).WithDefaults()
	if len(p.Rules) > maxRules {
		return nil, &model.ValidationError{Problems: []string{
			fmt.Sprintf("a profile may have at most %d rules, got %d", maxRules, len(p.Rules)),
		}}
	}
	c, err := compile(p, false)
	if err != nil {
		return nil, err
	}
	secret := p.Secret
	if secret == "" {
		secret = model.NewSecret()
	}
	c.secret = []byte(secret)
	return &Engine{prof: p, c: c, k: newKeyer(c.secret), salted: &saltCache{m: map[string]*Engine{}}}, nil
}

// Profile returns the profile the engine was built from, with defaults
// applied. The returned value shares no memory with the engine.
func (e *Engine) Profile() model.AnonymizationProfile { return cloneProfile(e.prof) }

// WithSalt returns an engine with the same rules whose hash and fake outputs
// are keyed by secret and salt. The same value maps to a different token under
// each salt, and consistently so within one salt. It is cheap: nothing is
// recompiled, and engines for recently used salts are reused. WithSalt("")
// returns e.
func (e *Engine) WithSalt(salt string) *Engine {
	if salt == "" {
		return e
	}
	sc := e.salted
	sc.mu.RLock()
	se := sc.m[salt]
	sc.mu.RUnlock()
	if se != nil {
		return se
	}
	se = &Engine{prof: e.prof, c: e.c, k: newKeyer(deriveKey(e.c.secret, salt)), salted: sc}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if have := sc.m[salt]; have != nil {
		return have
	}
	if len(sc.m) >= maxSaltedEngines {
		for k := range sc.m { // evict an arbitrary entry
			delete(sc.m, k)
			break
		}
	}
	sc.m[salt] = se
	return se
}

// runPool recycles run state so a call allocates nothing for it.
var runPool = sync.Pool{New: func() any { return new(run) }}

func (e *Engine) acquire(rep *reporter) *run {
	r := runPool.Get().(*run)
	r.c, r.k, r.rep = e.c, e.k, rep
	return r
}

// done clears the run and returns it to the pool.
func (r *run) done() {
	clear(r.chain)
	*r = run{chain: r.chain[:0]}
	runPool.Put(r)
}

// Exchange anonymizes request and response in place and sets
// ex.Anonymized = true. On error the exchange may be partly anonymized and
// must not be stored.
func (e *Engine) Exchange(ex *model.Exchange) error {
	return e.exchange(ex, nil)
}

// ExchangeWithReport is Exchange plus a list of what changed, for previews.
// The list is capped at 500 entries; Before and After values are truncated to
// 200 characters.
func (e *Engine) ExchangeWithReport(ex *model.Exchange) ([]model.AnonChange, error) {
	rep := &reporter{changes: []model.AnonChange{}}
	err := e.exchange(ex, rep)
	return rep.changes, err
}

func (e *Engine) exchange(ex *model.Exchange, rep *reporter) (err error) {
	if ex == nil {
		return fmt.Errorf("anonymize: nil exchange: %w", model.ErrValidation)
	}
	defer recoverInternal(&err)
	r := e.acquire(rep)
	defer r.done()
	r.setSide(true)
	r.request(&ex.Request)
	r.setSide(false)
	r.response(&ex.Response)
	if ex.Error != "" {
		// Transport errors quote addresses ("dial tcp 10.1.2.3:443: ...").
		loc := locator{where: "error"}
		ex.Error = r.detect(ex.Error, nil, &loc)
	}
	ex.Anonymized = true
	return nil
}

// Request anonymizes only a request (used by replay on the fly).
func (e *Engine) Request(req *model.Request) (err error) {
	if req == nil {
		return fmt.Errorf("anonymize: nil request: %w", model.ErrValidation)
	}
	defer recoverInternal(&err)
	r := e.acquire(nil)
	defer r.done()
	r.setSide(true)
	r.request(req)
	return nil
}

// Response anonymizes only a response.
func (e *Engine) Response(resp *model.Response) (err error) {
	if resp == nil {
		return fmt.Errorf("anonymize: nil response: %w", model.ErrValidation)
	}
	defer recoverInternal(&err)
	r := e.acquire(nil)
	defer r.done()
	r.setSide(false)
	r.response(resp)
	return nil
}

// recoverInternal turns a panic into an error. Anonymization runs inside the
// capture pipeline, where a panic would take the proxy down; the engine is
// written not to panic, and this is the last line of defence.
func recoverInternal(err *error) {
	if rec := recover(); rec != nil {
		*err = fmt.Errorf("anonymize: internal error: %v", rec)
	}
}

func (r *run) request(q *model.Request) {
	ct := getHeader(q.Header, "Content-Type") // before headers are rewritten
	r.headers(q.Header)
	q.Path = r.path(q.Path)
	q.RawQuery = r.query(q.RawQuery)
	r.body(&q.Body, &q.BodySize, q.BodyTruncated, q.ContentEncoding, q.Header, ct)
}

func (r *run) response(p *model.Response) {
	ct := getHeader(p.Header, "Content-Type")
	r.headers(p.Header)
	r.body(&p.Body, &p.BodySize, p.BodyTruncated, p.ContentEncoding, p.Header, ct)
}

// Validate compiles a profile and reports problems without keeping the
// engine. It includes the model-level validation, so a profile without a name
// is rejected here (New does not require one).
func Validate(p model.AnonymizationProfile) error {
	p = cloneProfile(p).WithDefaults()
	if len(p.Rules) > maxRules {
		return &model.ValidationError{Problems: []string{
			fmt.Sprintf("a profile may have at most %d rules, got %d", maxRules, len(p.Rules)),
		}}
	}
	_, err := compile(p, true)
	return err
}

// DetectorInfo describes a built-in detector for the UI.
type DetectorInfo struct {
	Name        model.Detector `json:"name"`
	Description string         `json:"description"`
	Example     string         `json:"example"` // a sample value it matches
}

// Detectors lists every built-in detector, in model.AllDetectors order.
func Detectors() []DetectorInfo {
	out := make([]DetectorInfo, 0, len(detectors))
	for _, name := range model.AllDetectors {
		for i := range detectors {
			if detectors[i].name == name {
				out = append(out, DetectorInfo{Name: name, Description: detectors[i].desc, Example: detectors[i].example})
			}
		}
	}
	return out
}
