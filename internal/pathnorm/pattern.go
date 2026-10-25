package pathnorm

import (
	"cmp"
	"fmt"
	"net/url"
	"strings"
)

// PatternError describes a malformed user pattern.
type PatternError struct {
	Pattern string // the pattern as supplied
	Reason  string
}

func (e *PatternError) Error() string {
	return fmt.Sprintf("pathnorm: invalid pattern %q: %s", e.Pattern, e.Reason)
}

// ValidatePattern reports whether s is a well-formed user pattern: it starts
// with "/", every segment is either literal or a whole "{name}" parameter,
// and "{name...}" appears only as the last segment. It returns a
// *PatternError otherwise.
func ValidatePattern(s string) error {
	_, err := parsePattern(s)
	return err
}

type segKind uint8

const (
	segLiteral segKind = iota // exact text
	segParam                  // {name}: one non-empty segment
	segRest                   // {name...}: zero or more trailing segments
)

type segment struct {
	kind segKind
	lit  string // segLiteral: the text as written
	dec  string // segLiteral: lit percent-decoded (equals lit when it has no escapes)
}

// pattern is a compiled user pattern.
type pattern struct {
	raw      string // reported template: the pattern without a trailing slash
	segs     []segment
	literals int  // number of literal segments
	hasRest  bool // last segment is {name...}
}

func parsePattern(s string) (pattern, error) {
	fail := func(format string, args ...any) (pattern, error) {
		return pattern{}, &PatternError{Pattern: s, Reason: fmt.Sprintf(format, args...)}
	}
	if s == "" || s[0] != '/' {
		return fail("must start with /")
	}
	if strings.ContainsAny(s, "?#") {
		return fail("must not contain a query string or fragment")
	}
	raw := s
	if len(raw) > 1 {
		if raw = strings.TrimRight(raw, "/"); raw == "" {
			raw = "/"
		}
	}
	p := pattern{raw: raw}
	body := raw[1:]
	if body == "" {
		return p, nil // the root path
	}
	parts := strings.Split(body, "/")
	p.segs = make([]segment, 0, len(parts))
	for i, part := range parts {
		switch {
		case part == "":
			return fail("empty path segment")
		case !strings.ContainsAny(part, "{}"):
			dec := part
			if strings.IndexByte(part, '%') >= 0 {
				if d, err := url.PathUnescape(part); err == nil {
					dec = d
				}
			}
			p.segs = append(p.segs, segment{kind: segLiteral, lit: part, dec: dec})
			p.literals++
		case part[0] != '{' || part[len(part)-1] != '}':
			return fail("unbalanced or misplaced braces in segment %q: a parameter must be a whole segment", part)
		default:
			name := part[1 : len(part)-1]
			kind := segParam
			if strings.HasSuffix(name, "...") {
				kind = segRest
				name = strings.TrimSuffix(name, "...")
			}
			if err := checkParamName(name); err != "" {
				return fail("segment %q: %s", part, err)
			}
			if kind == segRest {
				if i != len(parts)-1 {
					return fail("segment %q: {name...} is only allowed as the last segment", part)
				}
				p.hasRest = true
			}
			p.segs = append(p.segs, segment{kind: kind})
		}
	}
	return p, nil
}

// checkParamName returns a problem description, or "" for a valid name.
func checkParamName(name string) string {
	if name == "" {
		return "empty parameter name"
	}
	if strings.Contains(name, "..") {
		return "invalid parameter name"
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return fmt.Sprintf("invalid character %q in parameter name", c)
		}
	}
	return ""
}

// comparePriority orders patterns most specific first.
func comparePriority(a, b pattern) int {
	if c := cmp.Compare(b.literals, a.literals); c != 0 {
		return c
	}
	if c := cmp.Compare(len(b.segs), len(a.segs)); c != 0 {
		return c
	}
	// A fixed-length pattern is more specific than a tail pattern.
	switch {
	case a.hasRest && !b.hasRest:
		return 1
	case !a.hasRest && b.hasRest:
		return -1
	}
	return 0
}

// match reports whether the path segments (no leading or trailing slash,
// already split) match the pattern. The root path is never passed in.
func (p *pattern) match(segs []string) bool {
	fixed := len(p.segs)
	if p.hasRest {
		fixed--
		if len(segs) < fixed {
			return false
		}
	} else if len(segs) != fixed {
		return false
	}
	for i := 0; i < fixed; i++ {
		ps := &p.segs[i]
		switch ps.kind {
		case segParam:
			if segs[i] == "" {
				return false
			}
		case segLiteral:
			if !literalMatches(segs[i], ps) {
				return false
			}
		}
	}
	return true
}

// literalMatches compares a raw path segment with a pattern literal, also
// accepting equal percent-decoded forms ("a%20b" matches "a b").
func literalMatches(seg string, ps *segment) bool {
	if seg == ps.lit {
		return true
	}
	if ps.dec == ps.lit && strings.IndexByte(seg, '%') < 0 {
		return false
	}
	d, err := url.PathUnescape(seg)
	return err == nil && d == ps.dec
}
