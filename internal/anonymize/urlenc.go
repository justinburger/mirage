package anonymize

import (
	"net/url"
	"strings"
)

// decodeQuery decodes a query or form component ('+' is a space). A component
// that is not valid percent-encoding is returned as written: it is still
// scanned, and if it changes the replacement is written escaped.
func decodeQuery(s string) string {
	if strings.IndexByte(s, '%') < 0 && strings.IndexByte(s, '+') < 0 {
		return s
	}
	if d, err := url.QueryUnescape(s); err == nil {
		return d
	}
	return s
}

func decodePath(s string) string {
	if strings.IndexByte(s, '%') < 0 {
		return s
	}
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	return s
}

// unreservedQuery are the bytes written unescaped into a query or form
// component besides letters and digits. "&", "=", "+", ";", "#" and "%" are
// structural there and always escaped.
const unreservedQuery = "-._~!$'()*,:@/?"

// unreservedPath are the bytes written unescaped into a path segment besides
// letters and digits (RFC 3986 pchar without "%"). "/", "?", "#" are always
// escaped.
const unreservedPath = "-._~!$&'()*+,;=:@"

func escapeWith(s, extra string) string {
	need := false
	for i := 0; i < len(s); i++ {
		if c := s[i]; !isAlnum(c) && strings.IndexByte(extra, c) < 0 {
			need = true
			break
		}
	}
	if !need {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isAlnum(c) || strings.IndexByte(extra, c) >= 0 {
			sb.WriteByte(c)
			continue
		}
		sb.WriteByte('%')
		sb.WriteByte("0123456789ABCDEF"[c>>4])
		sb.WriteByte("0123456789ABCDEF"[c&0xf])
	}
	return sb.String()
}

func escapeQuery(s string) string       { return escapeWith(s, unreservedQuery) }
func escapePathSegment(s string) string { return escapeWith(s, unreservedPath) }

// query anonymizes a request's RawQuery.
func (r *run) query(raw string) string {
	loc := locator{side: r.side, where: "query"}
	return r.params(raw, r.sd.query, &loc)
}

// params rewrites a query string or a form body, preserving parameter order
// and the exact bytes of every parameter that does not change.
func (r *run) params(raw string, rules map[string][]*rule, loc *locator) string {
	if raw == "" {
		return raw
	}
	var out []byte // built lazily, on the first change
	wrote := false
	for pos := 0; pos <= len(raw); {
		end := strings.IndexByte(raw[pos:], '&')
		if end < 0 {
			end = len(raw)
		} else {
			end += pos
		}
		pair := raw[pos:end]
		np, drop := r.param(pair, rules, loc)
		if out == nil && (drop || np != pair) {
			out = make([]byte, 0, len(raw)+16)
			if pos > 0 {
				out = append(out, raw[:pos-1]...) // raw[pos-1] is the '&' before this pair
				wrote = true
			}
		}
		if out != nil && !drop {
			if wrote {
				out = append(out, '&')
			}
			out = append(out, np...)
			wrote = true
		}
		pos = end + 1
	}
	if out == nil {
		return raw
	}
	return string(out)
}

// param anonymizes one "name=value" pair.
func (r *run) param(pair string, rules map[string][]*rule, loc *locator) (string, bool) {
	if pair == "" {
		return pair, false
	}
	k, v, hasEq := strings.Cut(pair, "=")
	key := decodeQuery(k)
	l := *loc
	l.name = key
	sel := r.keyedRules(rules, key, true)
	if rl := removes(sel); rl != nil {
		r.record(&l, rl.source, pair, "")
		return "", true
	}

	newKey := k
	if r.c.detMask != 0 && len(key) >= 3 {
		if nk := r.detect(key, nil, &l); nk != key {
			newKey = escapeQuery(nk)
		}
	}
	if !hasEq {
		return newKey, false
	}
	val := decodeQuery(v)
	nv := r.value(sel, val, &l)
	if nv == val {
		if newKey == k {
			return pair, false
		}
		return newKey + "=" + v, false
	}
	return newKey + "=" + escapeQuery(nv), false
}

// path anonymizes a request's escaped path: path rules first, then regex rules
// and detectors on the segments no path rule handled.
func (r *run) path(p string) string {
	if p == "" {
		return p
	}
	loc := locator{side: r.side, where: "path"}
	var handled uint64
	if len(r.sd.path) > 0 {
		p, handled = r.pathRules(p, &loc)
	}
	return r.pathScan(p, handled, &loc)
}

// pathRules applies the path-template rules in order and returns the new path
// and a bit set of the segments (by index in strings.Split(p, "/")) they
// replaced.
func (r *run) pathRules(p string, loc *locator) (string, uint64) {
	var handled uint64
	esc := strings.Split(p, "/")
	dec := make([]string, len(esc))
	for i, e := range esc {
		dec[i] = decodePath(e)
	}
	// The template sees the segments after the leading "/", without a
	// trailing empty one.
	lo, hi := 0, len(dec)
	if hi > 0 && esc[0] == "" {
		lo = 1
	}
	if hi > lo && esc[hi-1] == "" {
		hi--
	}
	if hi <= lo {
		return p, 0
	}
	hits := make([]span, hi-lo)
	changed := false
	// A segment that a rule covers is that rule's: when several rules match
	// it the first wins and the others leave it alone.
	var taken []bool
	if len(r.sd.path) > 1 {
		taken = make([]bool, len(dec))
	}
	for _, rl := range r.sd.path {
		if !rl.tmpl.match(dec[lo:hi], hits) {
			continue
		}
		for j, sp := range hits {
			if sp.hi <= sp.lo {
				continue
			}
			i := lo + j
			if taken != nil {
				if taken[i] {
					continue
				}
				taken[i] = true
			}
			seg := dec[i]
			sel := [1]*rule{rl}
			res := r.value(sel[:], seg[sp.lo:sp.hi], loc)
			nseg := seg[:sp.lo] + res + seg[sp.hi:]
			if nseg == seg {
				continue
			}
			dec[i], esc[i] = nseg, escapePathSegment(nseg)
			changed = true
			if i < 64 {
				handled |= 1 << uint(i)
			}
		}
	}
	if !changed {
		return p, handled
	}
	return strings.Join(esc, "/"), handled
}

// pathScan applies the regex rules and the detectors to every path segment
// that a path rule did not already replace. Nothing is allocated when no
// segment changes.
func (r *run) pathScan(p string, handled uint64, loc *locator) string {
	if r.c.detMask == 0 && len(r.sd.regex) == 0 {
		return p
	}
	var out []byte
	seg := 0
	for pos := 0; pos <= len(p); seg++ {
		end := strings.IndexByte(p[pos:], '/')
		if end < 0 {
			end = len(p)
		} else {
			end += pos
		}
		s := p[pos:end]
		ns := s
		if s != "" && !(seg < 64 && handled&(1<<uint(seg)) != 0) {
			dv := decodePath(s)
			if nv := r.text(dv, loc); nv != dv {
				ns = escapePathSegment(nv)
			}
		}
		if out == nil && ns != s {
			out = append(make([]byte, 0, len(p)+16), p[:pos]...)
		}
		if out != nil {
			out = append(out, ns...)
			if end < len(p) {
				out = append(out, '/')
			}
		}
		pos = end + 1
	}
	if out == nil {
		return p
	}
	return string(out)
}
