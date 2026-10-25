// Package pathnorm groups concrete request paths into endpoint templates
// ("/users/42/orders/9f1c…" → "/users/{id}/orders/{uuid}") for metrics,
// endpoint statistics and template matching in the virtualizer.
//
// CONTRACT (implemented by the util agent):
//
//   - User patterns win: a Normalizer built with patterns such as
//     "/users/{id}" or "/files/{path...}" ({name...} only as the last
//     segment, matching the rest of the path) maps matching paths to that
//     pattern verbatim. The most specific pattern wins (more literal
//     segments first, then longer patterns). Matching ignores a trailing
//     slash difference.
//   - Otherwise each segment is classified heuristically:
//     all digits → {id}; UUID (any version, any case) → {uuid};
//     hex string of length ≥ 16 → {hex}; long opaque token (≥ 20 chars of
//     [A-Za-z0-9_-] with both letters and digits) → {token};
//     an email address → {email}; ISO date (2024-01-31) → {date};
//     anything else is kept literally. Percent-escapes are kept as is.
//   - Query strings are ignored (callers pass r.URL.Path or Request.Path).
//   - Template and Endpoint are pure, allocation-light, safe for concurrent
//     use, and fast (< 1µs for typical paths; benchmark it).
//   - Endpoint(method, path) = strings.ToUpper(method) + " " + Template(path).
//
// # Behaviour details
//
// The details below are decisions this implementation made where the
// contract above is silent. They are covered by tests.
//
//   - Segments are classified in the order the contract lists them, except
//     that "all digits" is checked first, so a 20-digit number is {id}, not
//     {hex}. Classification looks at the raw (still percent-escaped)
//     segment, with one exception: an email address whose "@" is written
//     "%40" is still recognised.
//   - A trailing slash is dropped from the template ("/users/" and "/users"
//     are the same endpoint), and so is everything from the first "?" on.
//     The root path and the empty path are both "/". The OPTIONS target "*"
//     is returned as "*". A path without a leading slash gets one.
//     Empty segments in the middle of a path ("/a//b") are kept as is.
//   - Because heuristics only look at one segment at a time, "42.json"
//     stays literal, as does anything with a prefix or suffix around an
//     id. Add a user pattern for such APIs.
//   - A user pattern "{name}" matches exactly one non-empty segment and
//     "{name...}" matches zero or more trailing segments (so
//     "/files/{path...}" also matches "/files" and "/files/"). Literal
//     pattern segments match the raw path segment, or its percent-decoded
//     form when either side contains an escape.
//   - A user pattern is reported with a trailing slash removed but is
//     otherwise verbatim, including the parameter names.
//   - Templates produced by Template are fixed points: normalising a
//     template returns the same template.
package pathnorm

import (
	"errors"
	"slices"
	"strings"
)

// Normalizer maps paths to endpoint templates. A Normalizer is immutable
// after New and safe for concurrent use. A nil *Normalizer behaves like one
// with no user patterns.
type Normalizer struct {
	// patterns is sorted most specific first.
	patterns []pattern
}

// Default is a Normalizer with no user patterns. Set in init by the
// implementation.
var Default *Normalizer

func init() {
	n, err := New(nil)
	if err != nil {
		panic("pathnorm: building default normalizer: " + err.Error())
	}
	Default = n
}

// New builds a normalizer from optional user patterns. Returns an error for
// malformed patterns (must start with "/", balanced braces, {x...} only
// last). When several patterns are malformed the error joins one
// *PatternError per bad pattern. Duplicate patterns are collapsed.
func New(patterns []string) (*Normalizer, error) {
	n := &Normalizer{}
	if len(patterns) == 0 {
		return n, nil
	}
	var errs []error
	seen := make(map[string]struct{}, len(patterns))
	for _, s := range patterns {
		p, err := parsePattern(s)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, dup := seen[p.raw]; dup {
			continue
		}
		seen[p.raw] = struct{}{}
		n.patterns = append(n.patterns, p)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	// More literal segments first, then longer patterns, then fixed-length
	// before tail ("{x...}") patterns; declaration order breaks any tie.
	slices.SortStableFunc(n.patterns, comparePriority)
	return n, nil
}

// Patterns returns the user patterns in priority order (most specific first).
func (n *Normalizer) Patterns() []string {
	if n == nil || len(n.patterns) == 0 {
		return nil
	}
	out := make([]string, len(n.patterns))
	for i := range n.patterns {
		out[i] = n.patterns[i].raw
	}
	return out
}

// Template returns the endpoint template for a path.
func (n *Normalizer) Template(path string) string {
	var stack [192]byte
	out, s, inBuf := n.normalize(stack[:0], path)
	if inBuf {
		return string(out)
	}
	return s
}

// Endpoint returns "METHOD template".
func (n *Normalizer) Endpoint(method, path string) string {
	var stack [192]byte
	buf := appendUpper(stack[:0], method)
	buf = append(buf, ' ')
	out, s, inBuf := n.normalize(buf, path)
	if !inBuf {
		out = append(out, s...)
	}
	return string(out)
}

// normalize computes the template for path. Either the template was appended
// to dst (inDst true, out is dst plus the template) or it is returned as str
// without copying: a substring of path, or a stored pattern.
func (n *Normalizer) normalize(dst []byte, path string) (out []byte, str string, inDst bool) {
	if q := strings.IndexByte(path, '?'); q >= 0 {
		path = path[:q]
	}
	if path == "*" {
		return dst, "*", false
	}
	slashed := path != "" && path[0] == '/'
	body := path
	if slashed {
		body = path[1:]
	}
	body = strings.TrimRight(body, "/")
	if body == "" {
		return dst, "/", false
	}

	if n != nil && len(n.patterns) > 0 {
		var arr [16]string
		segs := splitSegments(arr[:0], body)
		for i := range n.patterns {
			if n.patterns[i].match(segs) {
				return dst, n.patterns[i].raw, false
			}
		}
	}

	// Heuristic pass. The output is only materialised once a segment has to
	// change; otherwise the answer is a substring of the input.
	started := false
	for start := 0; ; {
		end := len(body)
		if i := strings.IndexByte(body[start:], '/'); i >= 0 {
			end = start + i
		}
		seg := body[start:end]
		repl := classify(seg)
		switch {
		case started:
			dst = append(dst, '/')
			if repl != "" {
				dst = append(dst, repl...)
			} else {
				dst = append(dst, seg...)
			}
		case repl != "":
			started = true
			dst = append(dst, '/')
			dst = append(dst, body[:start]...)
			dst = append(dst, repl...)
		}
		if end == len(body) {
			break
		}
		start = end + 1
	}
	if started {
		return dst, "", true
	}
	if slashed {
		return dst, path[:1+len(body)], false
	}
	dst = append(dst, '/')
	dst = append(dst, body...)
	return dst, "", true
}

// splitSegments splits a path body (no leading or trailing slash) into
// segments, appending to dst.
func splitSegments(dst []string, body string) []string {
	for {
		i := strings.IndexByte(body, '/')
		if i < 0 {
			return append(dst, body)
		}
		dst = append(dst, body[:i])
		body = body[i+1:]
	}
}

// appendUpper appends s upper-cased the way strings.ToUpper would.
func appendUpper(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return append(dst, strings.ToUpper(s)...)
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		}
		dst = append(dst, c)
	}
	return dst
}
