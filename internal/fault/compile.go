package fault

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/justinburger/mirage/internal/model"
)

// ruleSet is an immutable compiled rule set. Injector swaps whole sets
// atomically, so Decide never observes a half-updated configuration.
type ruleSet struct {
	all    []*compiledRule          // evaluation order, disabled rules included
	active []*compiledRule          // enabled rules only, same order
	byID   map[string]*compiledRule // rules with a non-empty ID
}

// compileRules validates and compiles rules into a new set. Fire counters of
// rules whose ID also exists in old are carried over. Every problem found in
// every rule is reported in one *model.ValidationError.
func compileRules(in []model.FaultRule, old *ruleSet) (*ruleSet, error) {
	var problems []string
	firstIdx := make(map[string]int, len(in))
	compiled := make([]*compiledRule, 0, len(in))

	for idx, r := range in {
		r = cloneRule(r.WithDefaults())
		label := ruleLabel(idx, r)
		c, ps := compileRule(r)
		for _, p := range ps {
			problems = append(problems, label+": "+p)
		}
		if r.ID != "" {
			if first, dup := firstIdx[r.ID]; dup {
				problems = append(problems, fmt.Sprintf("%s: duplicate id %q (also rules[%d])", label, r.ID, first))
			} else {
				firstIdx[r.ID] = idx
			}
		}
		if c != nil {
			compiled = append(compiled, c)
		}
	}
	if len(problems) > 0 {
		return nil, &model.ValidationError{Problems: problems}
	}

	set := &ruleSet{
		all:    compiled,
		active: make([]*compiledRule, 0, len(compiled)),
		byID:   make(map[string]*compiledRule, len(compiled)),
	}
	sort.SliceStable(set.all, func(a, b int) bool {
		ra, rb := &set.all[a].rule, &set.all[b].rule
		if ra.Order != rb.Order {
			return ra.Order < rb.Order
		}
		return ra.Name < rb.Name
	})
	for _, c := range set.all {
		if id := c.rule.ID; id != "" {
			if prev := old.lookup(id); prev != nil {
				c.fired = prev.fired
			}
			set.byID[id] = c
		}
		if c.rule.Enabled {
			set.active = append(set.active, c)
		}
	}
	return set, nil
}

// lookup finds a rule by ID in s; it is nil-safe.
func (s *ruleSet) lookup(id string) *compiledRule {
	if s == nil {
		return nil
	}
	return s.byID[id]
}

// ruleLabel names a rule in error messages.
func ruleLabel(idx int, r model.FaultRule) string {
	switch {
	case r.Name != "" && r.ID != "":
		return fmt.Sprintf("rules[%d] %q (%s)", idx, r.Name, r.ID)
	case r.Name != "":
		return fmt.Sprintf("rules[%d] %q", idx, r.Name)
	case r.ID != "":
		return fmt.Sprintf("rules[%d] (%s)", idx, r.ID)
	}
	return fmt.Sprintf("rules[%d]", idx)
}

// cloneRule deep-copies the reference fields of a rule.
func cloneRule(r model.FaultRule) model.FaultRule {
	r.Methods = slices.Clone(r.Methods)
	r.Headers = maps.Clone(r.Headers)
	r.ResponseHeader = maps.Clone(r.ResponseHeader)
	return r
}

// compileRule validates one (already defaulted and cloned) rule and compiles
// it. When the returned problems are non-empty the compiled rule is nil.
func compileRule(r model.FaultRule) (*compiledRule, []string) {
	var problems []string
	if err := r.Validate(); err != nil {
		var ve *model.ValidationError
		if errors.As(err, &ve) {
			problems = append(problems, ve.Problems...)
		} else {
			problems = append(problems, err.Error())
		}
	}
	problems = append(problems, extraChecks(r)...)
	if len(problems) > 0 {
		return nil, problems
	}

	c := &compiledRule{
		rule:      r,
		fired:     new(atomic.Int64),
		prob:      r.Probability,
		serviceID: r.ServiceID,
	}
	switch r.Type {
	case model.FaultLatency:
		c.kind = kindLatency
		c.delay = clampDuration(r.Delay.D())
		c.jitter = clampDuration(r.Jitter.D())
	case model.FaultError:
		c.kind, c.terminal = kindError, true
	case model.FaultAbort:
		c.kind, c.terminal = kindAbort, true
	case model.FaultTimeout:
		c.kind, c.terminal = kindTimeout, true
	case model.FaultThrottle:
		c.kind = kindThrottle
		c.bps = r.BytesPerSecond
	}

	c.methods = slices.Clone(r.Methods)
	for name, value := range r.Headers {
		canon := http.CanonicalHeaderKey(name)
		c.headers = append(c.headers, headerCond{name: canon, value: value, host: canon == "Host"})
	}
	// Deterministic evaluation order regardless of map iteration order.
	sort.Slice(c.headers, func(a, b int) bool { return c.headers[a].name < c.headers[b].name })

	if pat := r.PathPattern; pat != "" {
		if expr, ok := strings.CutPrefix(pat, "re:"); ok {
			re, err := regexp.Compile(expr)
			if err != nil {
				return nil, []string{fmt.Sprintf("path_pattern regex: %v", err)}
			}
			c.re, c.hasPath = re, true
		} else {
			g, err := compileGlob(pat)
			if err != nil {
				return nil, []string{err.Error()}
			}
			c.glob, c.hasPath = g, !g.all
		}
	}
	if c.kind == kindError {
		c.resp = newErrorResponse(&c.rule)
	}
	return c, nil
}

// extraChecks are the validations that go beyond model.FaultRule.Validate and
// that the injector relies on.
func extraChecks(r model.FaultRule) []string {
	var ps []string
	if math.IsNaN(r.Probability) {
		ps = append(ps, "probability must be a number")
	}
	if r.Type == model.FaultError && r.Status >= 100 && r.Status < 200 {
		ps = append(ps, "error status must be 200 or above (1xx responses are informational)")
	}
	for _, m := range r.Methods {
		if !validToken(m) {
			ps = append(ps, fmt.Sprintf("invalid method %q", m))
		}
	}
	for name := range r.Headers {
		if !validToken(name) {
			ps = append(ps, fmt.Sprintf("invalid header name %q in headers", name))
		}
	}
	for name := range r.ResponseHeader {
		if !validToken(name) {
			ps = append(ps, fmt.Sprintf("invalid header name %q in response_headers", name))
		}
	}
	return ps
}

// validToken reports whether s is a non-empty RFC 9110 token.
func validToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}
