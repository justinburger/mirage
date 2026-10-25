// Package anonymize removes or transforms sensitive data in captured
// exchanges while keeping traffic realistic enough to replay.
//
// CONTRACT (implemented by the anonymize agent):
//
// Determinism. hash and fake are keyed by HMAC-SHA256(secret‖salt, value):
// the same input always gives the same output for a given profile secret and
// salt, so a user ID or email that appears in many requests maps to one
// consistent token everywhere (request path, query, JSON body, response).
// Different salts (WithSalt) give different but equally consistent outputs;
// replay uses this to give each amplified copy its own identities.
//
// Order of operations for each side (request, response):
//  1. RedactHeaders: listed headers get hash tokens; an auth scheme prefix
//     ("Bearer ", "Basic ", "Token ") is preserved. Cookie / Set-Cookie are
//     handled per cookie (names and attributes kept, values hashed).
//  2. Rules, in order, filtered by Scope and Enabled.
//  3. Detectors over every remaining textual value: JSON string leaves (and
//     numbers for credit_card/phone/ssn detectors when they look like one),
//     form values, query values, header values (except redacted ones),
//     cookie values, and plain text bodies (text/*, xml, unknown text).
//
// Bodies. JSON bodies (Content-Type */json or +json, or sniffed when the
// type is missing) are rewritten preserving member order, number literals and
// unknown structure exactly; output is compact JSON. NDJSON is handled line
// by line. Form bodies are rewritten field by field. Other textual bodies get
// regex rules and detectors. Binary bodies and bodies with a non-empty
// ContentEncoding (still compressed) are left alone. When a body changes,
// BodySize is updated and any Content-Length header is fixed.
// A truncated body (BodyTruncated) is still processed best-effort as text,
// never as JSON (it would not parse).
//
// Fake kinds must preserve format: email → "<word>.<word><nn>@example.com"
// style (domain example.com/.net/.org only); phone keeps punctuation and
// length; credit_card keeps length and grouping and is Luhn-valid (prefix 4
// so it is clearly a test-style Visa number); digits keeps length (no
// leading zero if the input had none); uuid keeps the 8-4-4-4-12 format and
// version nibble; ipv4 maps into 10.0.0.0/8; name uses an embedded list of
// common first/last names; string/auto keeps length and character classes
// (upper→upper, lower→lower, digit→digit, punctuation kept). auto first
// recognises email/uuid/ipv4/phone/card/digits shapes.
//
// Performance. Engine is immutable after New and safe for concurrent use.
// Compile everything (regexes, JSONPaths) in New. Avoid re-parsing JSON
// more than once per body. Target: < 50µs for a typical 2 KB JSON exchange
// with the default profile (benchmark it). Use sync.Pool for HMAC state.
//
// Safety. Anonymization must never panic on malformed input; on a body it
// cannot parse it falls back to text processing. The redact strategy default
// replacement is "[REDACTED]".
package anonymize

import (
	"github.com/justinburger/mirage/internal/model"
)

// Engine applies one anonymization profile.
type Engine struct{}

// New compiles a profile. The profile should already have defaults applied
// (model.AnonymizationProfile.WithDefaults). An empty Secret is allowed (a
// random per-process secret is used) but then tokens are not stable across
// restarts. Returns a *model.ValidationError for bad rules (invalid JSONPath,
// regex, path template).
func New(p model.AnonymizationProfile) (*Engine, error) { panic("anonymize: not implemented") }

// Profile returns the profile the engine was built from.
func (e *Engine) Profile() model.AnonymizationProfile { panic("anonymize: not implemented") }

// WithSalt returns an engine with the same rules whose hash/fake outputs are
// keyed by secret+salt. Cheap (no recompilation). WithSalt("") returns e.
func (e *Engine) WithSalt(salt string) *Engine { panic("anonymize: not implemented") }

// Exchange anonymizes request and response in place and sets
// ex.Anonymized = true.
func (e *Engine) Exchange(ex *model.Exchange) error { panic("anonymize: not implemented") }

// ExchangeWithReport is Exchange plus a list of what changed, for previews.
// The list is capped at 500 entries; Before/After values are truncated to
// 200 characters.
func (e *Engine) ExchangeWithReport(ex *model.Exchange) ([]model.AnonChange, error) {
	panic("anonymize: not implemented")
}

// Request anonymizes only a request (used by replay on the fly).
func (e *Engine) Request(r *model.Request) error { panic("anonymize: not implemented") }

// Response anonymizes only a response.
func (e *Engine) Response(r *model.Response) error { panic("anonymize: not implemented") }

// Validate compiles a profile and reports problems without keeping the
// engine. It includes model-level validation.
func Validate(p model.AnonymizationProfile) error { panic("anonymize: not implemented") }

// DetectorInfo describes a built-in detector for the UI.
type DetectorInfo struct {
	Name        model.Detector `json:"name"`
	Description string         `json:"description"`
	Example     string         `json:"example"` // a sample value it matches
}

// Detectors lists every built-in detector, in model.AllDetectors order.
func Detectors() []DetectorInfo { panic("anonymize: not implemented") }
