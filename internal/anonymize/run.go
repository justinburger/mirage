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
// selector rules that matched it plus every regex rule of the current side.
// The result aliases scratch memory and is valid until the next call.
func (r *run) mergeChain(sel []*rule) []*rule {
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

// removes reports whether a selector rule in the chain deletes its target.
func removes(chain []*rule) *rule {
	for _, rl := range chain {
		if rl.target != model.TargetRegex && rl.act.remove() {
			return rl
		}
	}
	return nil
}

// applyChain runs the rules over s in order. Remove behaves like redact here;
// callers that can delete check removes first.
func (r *run) applyChain(chain []*rule, s string, loc *locator) string {
	for _, rl := range chain {
		if rl.target == model.TargetRegex {
			s = r.applyRegex(rl, s, loc)
			continue
		}
		next := r.k.apply(&rl.act, s)
		r.record(loc, rl.source, s, next)
		s = next
	}
	return s
}

// applyRegex replaces every match of the rule's pattern (or its first capture
// group) with the rule's strategy applied to the matched text.
func (r *run) applyRegex(rl *rule, s string, loc *locator) string {
	if s == "" {
		return s
	}
	idx := rl.re.FindAllStringSubmatchIndex(s, -1)
	if len(idx) == 0 {
		return s
	}
	var sb strings.Builder
	prev := 0
	changed := false
	for _, m := range idx {
		a, z := m[0], m[1]
		if rl.group {
			a, z = m[2], m[3]
		}
		if a < prev || z <= a { // group did not participate, empty match, overlap
			continue
		}
		sub := s[a:z]
		rep := r.k.apply(&rl.act, sub)
		if rep == sub {
			continue
		}
		changed = true
		sb.WriteString(s[prev:a])
		sb.WriteString(rep)
		prev = z
		r.record(loc, rl.source, sub, rep)
	}
	if !changed {
		return s
	}
	sb.WriteString(s[prev:])
	return sb.String()
}

// detect replaces every detector match in s. It returns s itself when nothing
// matched. st may be nil when the caller has not classified s yet.
func (r *run) detect(s string, st *textStats, loc *locator) string {
	if r.c.detMask == 0 || len(s) < 3 {
		return s
	}
	var local textStats
	if st == nil {
		local = classify(s)
		st = &local
	}
	mask := candidates(r.c.detMask, st)
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
	for _, rl := range r.sd.regex {
		s = r.applyRegex(rl, s, loc)
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
