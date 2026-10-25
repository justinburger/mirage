package fault

import (
	"errors"
	"strings"
)

// segKind is the kind of one compiled glob segment.
type segKind uint8

const (
	segLiteral segKind = iota // matches exactly one segment with this text
	segOne                    // "*": exactly one non-empty segment
	segAny                    // "**": zero or more segments
)

type globSeg struct {
	kind segKind
	lit  string // segLiteral only
}

// pathGlob is a compiled path glob. It matches request paths segment by
// segment, without allocating and without regular expressions:
//
//   - "*" matches exactly one (non-empty) segment,
//   - "**" matches zero or more segments,
//   - any other segment is a literal compared case-sensitively.
//
// A single trailing slash on either the pattern or the path is ignored, so
// "/users" matches "/users/" and "/api/**" matches "/api/". Consecutive "**"
// segments are collapsed.
type pathGlob struct {
	segs []globSeg
	all  bool // pattern is "/**": matches every path
}

// compileGlob compiles a glob such as "/users/*/orders" or "/api/**".
func compileGlob(pattern string) (pathGlob, error) {
	if !strings.HasPrefix(pattern, "/") {
		return pathGlob{}, errors.New("path pattern must start with /")
	}
	var g pathGlob
	p := trimSlashes(pattern)
	if p == "" {
		return g, nil // "/" matches only the root path
	}
	for _, s := range strings.Split(p, "/") {
		switch s {
		case "**":
			if n := len(g.segs); n > 0 && g.segs[n-1].kind == segAny {
				continue
			}
			g.segs = append(g.segs, globSeg{kind: segAny})
		case "*":
			g.segs = append(g.segs, globSeg{kind: segOne})
		default:
			g.segs = append(g.segs, globSeg{kind: segLiteral, lit: s})
		}
	}
	g.all = len(g.segs) == 1 && g.segs[0].kind == segAny
	return g, nil
}

// trimSlashes strips one leading and one trailing slash.
func trimSlashes(s string) string {
	s = strings.TrimPrefix(s, "/")
	return strings.TrimSuffix(s, "/")
}

// nextSegment returns the segment of path that starts at offset sp and the
// offset where the following segment starts. A returned offset greater than
// len(path) means there are no more segments.
func nextSegment(path string, sp int) (seg string, next int) {
	rest := path[sp:]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i], sp + i + 1
	}
	return rest, len(path) + 1
}

// match reports whether path matches the glob. It never allocates.
//
// The algorithm is the classic linear wildcard matcher with a single
// backtracking point: "**" behaves like "*" in a string glob and "*" like "?"
// (but over whole segments), so remembering only the most recent "**" is
// sufficient for correctness.
func (g *pathGlob) match(path string) bool {
	if g.all {
		return true
	}
	path = trimSlashes(path)
	sp := 0 // offset of the current path segment; len(path)+1 = exhausted
	if path == "" {
		sp = 1
	}
	pi := 0
	starPI, starSP := -1, 0
	for sp <= len(path) {
		if pi < len(g.segs) {
			s := &g.segs[pi]
			if s.kind == segAny {
				starPI, starSP = pi, sp
				pi++
				continue
			}
			seg, next := nextSegment(path, sp)
			if (s.kind == segOne && seg != "") || (s.kind == segLiteral && seg == s.lit) {
				pi++
				sp = next
				continue
			}
		}
		if starPI < 0 {
			return false
		}
		// Let the last "**" swallow one more segment and retry.
		_, starSP = nextSegment(path, starSP)
		sp = starSP
		pi = starPI + 1
	}
	for pi < len(g.segs) && g.segs[pi].kind == segAny {
		pi++
	}
	return pi == len(g.segs)
}
