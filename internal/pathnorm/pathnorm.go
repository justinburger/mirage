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
package pathnorm

// Normalizer maps paths to endpoint templates.
type Normalizer struct{}

// New builds a normalizer from optional user patterns. Returns an error for
// malformed patterns (must start with "/", balanced braces, {x...} only
// last).
func New(patterns []string) (*Normalizer, error) { panic("pathnorm: not implemented") }

// Default is a Normalizer with no user patterns. Set in init by the
// implementation.
var Default *Normalizer

// Template returns the endpoint template for a path.
func (n *Normalizer) Template(path string) string { panic("pathnorm: not implemented") }

// Endpoint returns "METHOD template".
func (n *Normalizer) Endpoint(method, path string) string { panic("pathnorm: not implemented") }
