package anonymize

import (
	"errors"
	"fmt"
	"net/textproto"
	"regexp"
	"strings"

	"github.com/justinburger/mirage/internal/model"
)

// rule is a compiled model.AnonymizationRule.
type rule struct {
	idx    int    // position in the profile; chains run in this order
	source string // label used in reports
	target model.RuleTarget
	act    action

	name  string         // header (canonical), cookie, query or form name
	re    *regexp.Regexp // regex target
	group bool           // regex has a capture group: only group 1 is replaced
	jp    *jsonPath      // json target
	tmpl  *pathTemplate  // path target
}

// sideRules indexes the rules that apply to one side of an exchange.
type sideRules struct {
	header map[string][]*rule
	cookie map[string][]*rule
	query  map[string][]*rule
	form   map[string][]*rule
	path   []*rule
	json   []*rule
	regex  []*rule
}

func newSideRules() sideRules {
	return sideRules{
		header: map[string][]*rule{},
		cookie: map[string][]*rule{},
		query:  map[string][]*rule{},
		form:   map[string][]*rule{},
	}
}

func (s *sideRules) add(r *rule) {
	switch r.target {
	case model.TargetHeader:
		s.header[r.name] = append(s.header[r.name], r)
	case model.TargetCookie:
		s.cookie[r.name] = append(s.cookie[r.name], r)
	case model.TargetQuery:
		s.query[r.name] = append(s.query[r.name], r)
	case model.TargetForm:
		s.form[r.name] = append(s.form[r.name], r)
	case model.TargetPath:
		s.path = append(s.path, r)
	case model.TargetJSON:
		s.json = append(s.json, r)
	case model.TargetRegex:
		s.regex = append(s.regex, r)
	}
}

// compiled is everything derived from a profile. It is immutable and shared
// between an engine and the engines WithSalt derives from it.
type compiled struct {
	secret  []byte
	detMask uint16
	detAct  [numDetectors]action
	redact  map[string]struct{}
	req     sideRules
	resp    sideRules
}

// compile validates the profile and builds the compiled form. Every problem
// found is reported in one *model.ValidationError.
func compile(p model.AnonymizationProfile, requireName bool) (*compiled, error) {
	checked := p
	if !requireName && strings.TrimSpace(checked.Name) == "" {
		checked.Name = "profile"
	}
	var problems []string
	if err := checked.Validate(); err != nil {
		var ve *model.ValidationError
		if errors.As(err, &ve) {
			problems = append(problems, ve.Problems...)
		} else {
			problems = append(problems, err.Error())
		}
	}
	addProblem := func(s string) {
		for _, have := range problems {
			if have == s {
				return
			}
		}
		problems = append(problems, s)
	}

	c := &compiled{
		redact: map[string]struct{}{},
		req:    newSideRules(),
		resp:   newSideRules(),
	}

	for _, d := range p.Detectors {
		for di := range detectors {
			if detectors[di].name == d {
				c.detMask |= 1 << di
			}
		}
	}
	for di := range detectors {
		a := action{strategy: p.DetectorStrategy, replacement: defaultReplacement}
		if a.strategy == model.StrategyFake {
			a.fake = detectors[di].fake
		}
		c.detAct[di] = a
	}
	for _, h := range p.RedactHeaders {
		if h = strings.TrimSpace(h); h != "" {
			c.redact[textproto.CanonicalMIMEHeaderKey(h)] = struct{}{}
		}
	}

	for i, r := range p.Rules {
		label := r.Name
		if label == "" {
			label = r.Selector
		}
		cr := &rule{idx: i, target: r.Target}
		cr.source = r.Name
		if cr.source == "" {
			cr.source = r.ID
		}
		if cr.source == "" {
			cr.source = r.Selector
		}
		cr.act = action{
			strategy: r.Strategy, fake: r.Fake,
			keepPrefix: r.KeepPrefix, keepSuffix: r.KeepSuffix,
			replacement: r.Replacement,
		}
		if cr.act.replacement == "" {
			cr.act.replacement = defaultReplacement
		}
		sel := strings.TrimSpace(r.Selector)
		switch r.Target {
		case model.TargetHeader:
			cr.name = textproto.CanonicalMIMEHeaderKey(sel)
		case model.TargetCookie, model.TargetQuery, model.TargetForm:
			cr.name = sel
		case model.TargetRegex:
			re, err := regexp.Compile(r.Selector)
			if err != nil {
				addProblem(fmt.Sprintf("rule %d (%s): invalid regex: %v", i+1, label, err))
				continue
			}
			cr.re, cr.group = re, re.NumSubexp() > 0
		case model.TargetJSON:
			jp, err := parseJSONPath(r.Selector)
			if err != nil {
				if strings.HasPrefix(sel, "$") { // the model already flags a missing $
					addProblem(fmt.Sprintf("rule %d (%s): invalid JSONPath: %v", i+1, label, err))
				}
				continue
			}
			cr.jp = jp
		case model.TargetPath:
			t, err := parsePathTemplate(r.Selector)
			if err != nil {
				if strings.HasPrefix(sel, "/") {
					addProblem(fmt.Sprintf("rule %d (%s): invalid path template: %v", i+1, label, err))
				}
				continue
			}
			cr.tmpl = t
		default:
			continue
		}
		if !r.Enabled {
			continue
		}
		switch r.Scope {
		case model.ScopeRequest:
			c.req.add(cr)
		case model.ScopeResponse:
			c.resp.add(cr)
		default:
			c.req.add(cr)
			c.resp.add(cr)
		}
	}
	if len(problems) > 0 {
		return nil, &model.ValidationError{Problems: problems}
	}
	return c, nil
}

// ---- path templates -------------------------------------------------------

// tmplSeg is one segment of a path template. A literal segment must match
// exactly. A parameter segment is `{name}`, optionally with a literal prefix
// and suffix (`v{version}`, `{file}.json`); only the part between them is
// anonymized. A rest segment `{name...}` must be last and covers one or more
// remaining segments.
type tmplSeg struct {
	literal string
	prefix  string
	suffix  string
	param   bool
	rest    bool
}

type pathTemplate struct {
	segs []tmplSeg
}

func parsePathTemplate(sel string) (*pathTemplate, error) {
	sel = strings.TrimSpace(sel)
	if !strings.HasPrefix(sel, "/") {
		return nil, errors.New("path template must start with /")
	}
	parts := strings.Split(strings.Trim(sel, "/"), "/")
	t := &pathTemplate{}
	params := 0
	for i, part := range parts {
		if part == "" {
			return nil, errors.New("path template has an empty segment")
		}
		lb, rb := strings.IndexByte(part, '{'), strings.IndexByte(part, '}')
		if lb < 0 && rb < 0 {
			t.segs = append(t.segs, tmplSeg{literal: part})
			continue
		}
		if lb < 0 || rb < lb || strings.Count(part, "{") != 1 || strings.Count(part, "}") != 1 {
			return nil, fmt.Errorf("segment %q has unbalanced or repeated braces", part)
		}
		name := part[lb+1 : rb]
		seg := tmplSeg{param: true, prefix: part[:lb], suffix: part[rb+1:]}
		if strings.HasSuffix(name, "...") {
			if i != len(parts)-1 || seg.prefix != "" || seg.suffix != "" {
				return nil, fmt.Errorf("segment %q: {name...} must be the whole last segment", part)
			}
			seg.rest = true
			name = strings.TrimSuffix(name, "...")
		}
		if name == "" {
			return nil, fmt.Errorf("segment %q has an empty parameter name", part)
		}
		params++
		t.segs = append(t.segs, seg)
	}
	if params == 0 {
		return nil, errors.New("path template has no {parameter} to anonymize")
	}
	return t, nil
}

// span is a half-open byte range inside a decoded path segment.
type span struct{ lo, hi int }

// match tests the decoded segments against the template. On success it fills
// hits[i] with the part of segment i that a parameter covers (hi > lo) and
// reports true. hits must have len(segs) entries.
func (t *pathTemplate) match(segs []string, hits []span) bool {
	last := t.segs[len(t.segs)-1]
	if last.rest {
		if len(segs) < len(t.segs) {
			return false
		}
	} else if len(segs) != len(t.segs) {
		return false
	}
	for i := range hits {
		hits[i] = span{}
	}
	for i, ts := range t.segs {
		if ts.rest {
			for j := i; j < len(segs); j++ {
				hits[j] = span{0, len(segs[j])}
			}
			return true
		}
		seg := segs[i]
		if !ts.param {
			if seg != ts.literal {
				return false
			}
			continue
		}
		if len(seg) < len(ts.prefix)+len(ts.suffix)+1 ||
			!strings.HasPrefix(seg, ts.prefix) || !strings.HasSuffix(seg, ts.suffix) {
			return false
		}
		hits[i] = span{len(ts.prefix), len(seg) - len(ts.suffix)}
	}
	return true
}
