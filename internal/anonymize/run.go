package anonymize

import (
	"strings"
	"unicode/utf8"

	"github.com/justinburger/mirage/internal/model"
)

const (
	maxReportChanges = 500
	maxReportValue   = 200
)

// reporter collects model.AnonChange entries for previews.
type reporter struct {
	changes []model.AnonChange
}

// full reports whether the report has reached its cap; callers use it to skip
// building strings that would be dropped.
func (rp *reporter) full() bool { return len(rp.changes) >= maxReportChanges }

func (rp *reporter) add(loc *locator, source, before, after string) {
	if rp.full() {
		return
	}
	rp.changes = append(rp.changes, model.AnonChange{
		Location: loc.String(), Source: source,
		Before: clipValue(before), After: clipValue(after),
	})
}

// mark and rollback let a failed JSON pass discard the changes it recorded
// before the text fallback re-processes the same body.
func (rp *reporter) mark() int { return len(rp.changes) }
func (rp *reporter) rollback(n int) {
	if n < len(rp.changes) {
		rp.changes = rp.changes[:n]
	}
}

// clipValue truncates v to 200 characters (the last one is an ellipsis when
// something was cut).
func clipValue(v string) string {
	if len(v) <= maxReportValue && utf8.ValidString(v) {
		return v
	}
	if utf8.RuneCountInString(v) <= maxReportValue {
		return v
	}
	n := 0
	for i := range v {
		if n == maxReportValue-1 {
			return v[:i] + "…"
		}
		n++
	}
	return v
}

// locator describes where a value lives, for report locations. It is a plain
// value so passing it around costs nothing; the string is only built when a
// change is actually recorded.
type locator struct {
	side  string // "request" or "response"
	where string // header, cookie, query, path, body, error
	name  string // header, cookie, query or form field name
	form  bool   // body is a form
	line  int    // 1-based NDJSON line, 0 if not NDJSON
	json  []byte // JSON path while rewriting a JSON body
	part  string // "multipart.<name>" of the part being processed; only set while a report is collected
}

func (l *locator) String() string {
	switch l.where {
	case "path":
		return l.side + ".path"
	case "header", "query", "cookie":
		return l.side + "." + l.where + "." + l.name
	case "error":
		return "error"
	}
	var sb strings.Builder
	sb.WriteString(l.side)
	sb.WriteString(".body")
	if l.line > 0 {
		sb.WriteString(" line ")
		sb.WriteString(itoa(l.line))
	}
	if l.part != "" {
		sb.WriteByte(' ')
		sb.WriteString(l.part)
	}
	switch {
	case l.form:
		sb.WriteString(" form.")
		sb.WriteString(l.name)
	case len(l.json) > 0:
		sb.WriteByte(' ')
		sb.Write(l.json)
	}
	return sb.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// run is the per-call state of one anonymization: it ties the immutable
// engine data to the side being processed and to an optional report. It is
// confined to one goroutine.
type run struct {
	c     *compiled
	k     *keyer
	rep   *reporter
	sd    *sideRules
	side  string
	chain []*rule // scratch for merged rule chains
	// selBuf is scratch for keyedRules; claimBuf for applyChain.
	selBuf   []*rule
	claimBuf []claim
	// mpDepth is how many multipart bodies enclose the body being processed.
	mpDepth int
}

func (r *run) setSide(request bool) {
	if request {
		r.sd, r.side = &r.c.req, "request"
	} else {
		r.sd, r.side = &r.c.resp, "response"
	}
}

func (r *run) record(loc *locator, source, before, after string) {
	if r.rep != nil && !r.rep.full() && before != after {
		r.rep.add(loc, source, before, after)
	}
}

// mergeChain returns the rules that apply to one value, in profile order: the
// selector rule that matched it plus every regex rule of the current side.
// When several selector rules match the value the first in the profile wins
// and the others leave it alone, so only that one is kept. The result aliases
// scratch memory and is valid until the next call.
func (r *run) mergeChain(sel []*rule) []*rule {
	if len(sel) > 1 {
		sel = sel[:1]
	}
	rx := r.sd.regex
	if len(rx) == 0 {
		return sel
	}
	if len(sel) == 0 {
		return rx
	}
	out := r.chain[:0]
	i, j := 0, 0
	for i < len(sel) && j < len(rx) {
		if sel[i].idx < rx[j].idx {
			out = append(out, sel[i])
			i++
		} else {
			out = append(out, rx[j])
			j++
		}
	}
	out = append(out, sel[i:]...)
	out = append(out, rx[j:]...)
	r.chain = out
	return out
}

// selected reports whether the chain contains a selector (non-regex) rule. A
// value a selector rule handled is not scanned by detectors again.
func selected(chain []*rule) bool {
	for _, rl := range chain {
		if rl.target != model.TargetRegex {
			return true
		}
	}
	return false
}

// removes returns the rule that deletes the value when the first selector
// rule in the chain deletes its target; a remove listed after a rule that
// transforms the value does not apply (the first rule wins).
func removes(chain []*rule) *rule {
	for _, rl := range chain {
		if rl.target != model.TargetRegex {
			if rl.act.remove() {
				return rl
			}
			return nil
		}
	}
	return nil
}

// claim is a stretch of a value, [lo,hi), that a regex rule has taken, with
// the text it becomes.
type claim struct {
	lo, hi int
	rep    string
}

// applyChain runs the rules over s in profile order. A value is transformed
// once: the first selector rule applies to all of it and the rules after it
// leave it alone, and a regex rule takes only the stretches it matches, so
// the rules after it (regex rules included) never see its output and cannot
// transform it a second time. A selector rule listed after a regex rule that
// took part of the value leaves the value alone too. Remove behaves like
// redact here; callers that can delete check removes first.
func (r *run) applyChain(chain []*rule, s string, loc *locator) string {
	claims := r.claimBuf[:0]
	for _, rl := range chain {
		if rl.target == model.TargetRegex {
			claims = r.claimRegex(rl, s, claims, loc)
			continue
		}
		if len(claims) > 0 {
			continue
		}
		next := r.k.apply(&rl.act, s)
		r.record(loc, rl.source, s, next)
		return next
	}
	out := spliceClaims(s, claims)
	if cap(claims) <= 64 {
		clear(claims)
		r.claimBuf = claims[:0]
	}
	return out
}

// spliceClaims returns s with the claimed stretches (sorted, disjoint)
// replaced; s itself when nothing changes.
func spliceClaims(s string, claims []claim) string {
	changed := false
	for i := range claims {
		if c := &claims[i]; c.rep != s[c.lo:c.hi] {
			changed = true
			break
		}
	}
	if !changed {
		return s
	}
	var sb strings.Builder
	prev := 0
	for i := range claims {
		c := &claims[i]
		sb.WriteString(s[prev:c.lo])
		sb.WriteString(c.rep)
		prev = c.hi
	}
	sb.WriteString(s[prev:])
	return sb.String()
}

// claimRegex finds the matches of the rule's pattern in s (or of its first
// capture group) that no earlier rule has claimed and adds them, with the
// rule's strategy applied, to have (sorted, disjoint). A match that overlaps
// a stretch an earlier rule took is left alone: that text is already
// anonymized.
func (r *run) claimRegex(rl *rule, s string, have []claim, loc *locator) []claim {
	if s == "" || rl.prefix != "" && !strings.Contains(s, rl.prefix) {
		return have
	}
	idx := rl.re.FindAllStringSubmatchIndex(s, -1)
	if len(idx) == 0 {
		return have
	}
	var add []claim
	if len(have) == 0 {
		add = have // the common case: no earlier claims, append in place
	}
	n0 := len(add)
	h := 0
	for _, m := range idx {
		a, z := m[0], m[1]
		if rl.group {
			a, z = m[2], m[3]
		}
		if a < 0 || z <= a { // group did not participate, or an empty match
			continue
		}
		for h < len(have) && have[h].hi <= a {
			h++
		}
		if h < len(have) && have[h].lo < z { // overlaps an earlier rule's claim
			continue
		}
		sub := s[a:z]
		rep := r.k.apply(&rl.act, sub)
		add = append(add, claim{lo: a, hi: z, rep: rep})
		r.record(loc, rl.source, sub, rep)
	}
	if len(add) == n0 {
		return have
	}
	if len(have) == 0 {
		return add
	}
	// Merge the new claims into the earlier ones.
	out := make([]claim, 0, len(have)+len(add))
	i, j := 0, 0
	for i < len(have) && j < len(add) {
		if have[i].lo < add[j].lo {
			out = append(out, have[i])
			i++
		} else {
			out = append(out, add[j])
			j++
		}
	}
	out = append(out, have[i:]...)
	out = append(out, add[j:]...)
	return out
}

// detect replaces every detector match in s. It returns s itself when nothing
// matched. st may be nil when the caller has not classified s yet.
func (r *run) detect(s string, st *textStats, loc *locator) string {
	return r.detectWith(s, st, loc, r.c.detMask)
}

// detectWith is detect restricted to the detectors in only (a subset of the
// profile's: a detector the profile has not enabled never runs).
func (r *run) detectWith(s string, st *textStats, loc *locator, only uint16) string {
	only &= r.c.detMask
	if only == 0 || len(s) < 3 {
		return s
	}
	var local textStats
	if st == nil {
		local = classify(s)
		st = &local
	}
	mask := candidates(only, st)
	if mask == 0 {
		return s
	}
	var c claims
	c.scan(s, mask)
	if c.n == 0 {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s) + 16)
	prev := 0
	changed := false
	for _, m := range c.list() {
		sub := s[m.start:m.end]
		rep := r.k.apply(&r.c.detAct[m.det], sub)
		if m.enc {
			rep = strings.ReplaceAll(rep, "@", "%40")
		}
		sb.WriteString(s[prev:m.start])
		sb.WriteString(rep)
		prev = m.end
		if rep != sub {
			changed = true
			if r.rep != nil && !r.rep.full() {
				r.rep.add(loc, "detector:"+string(detectors[m.det].name), sub, rep)
			}
		}
	}
	if !changed {
		return s
	}
	sb.WriteString(s[prev:])
	return sb.String()
}

// text applies the regex rules and then the detectors to a free-form string.
func (r *run) text(s string, loc *locator) string {
	if s == "" {
		return s
	}
	if len(r.sd.regex) > 0 {
		s = r.applyChain(r.sd.regex, s, loc)
	}
	return r.detect(s, nil, loc)
}

// value is the common path for a named value (header value, query value, form
// value, cookie value): selector rules first, regex rules in profile order,
// and the detectors unless a selector rule already handled the value.
func (r *run) value(sel []*rule, s string, loc *locator) string {
	if s == "" {
		return s
	}
	chain := r.mergeChain(sel)
	if len(chain) > 0 {
		s = r.applyChain(chain, s, loc)
	}
	if selected(chain) {
		return s
	}
	return r.detect(s, nil, loc)
}
