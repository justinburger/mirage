// Package anonymize removes or transforms sensitive data in captured
// exchanges while keeping traffic realistic enough to replay.
//
// # Behaviour
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
// regex rules and detectors. Binary bodies and bodies that are still
// compressed are left alone (see the implementation notes). When a body
// changes, BodySize is updated and any Content-Length header is fixed.
// A truncated body (BodyTruncated) is processed best-effort: JSON is
// rewritten as far as it goes, everything else as text.
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
//
// # Implementation notes
//
// Where this package goes beyond, or interprets, the rules above:
//
//   - A body counts as "still compressed" when a Content-Encoding header
//     survives, or when ContentEncoding is set and the bytes are plainly
//     compressed (gzip, zlib, zstd magic, or not text). A body the proxy
//     decoded keeps its original encoding name in ContentEncoding; that body
//     is plain text and is anonymized.
//   - A truncated JSON body is rewritten by the same streaming rewriter in a
//     lenient mode that stops where the capture stopped, so JSONPath rules
//     still redact the fields in the retained prefix. Anything that is not
//     JSON falls back to text processing.
//   - A JSON body that changes nothing is returned byte for byte (including
//     its whitespace); only a changed body is re-emitted compact.
//   - JSON numbers are only tested by the card detector (a bare nine or ten
//     digit integer is far more often an ID or a timestamp than an SSN or a
//     phone number). A rule or fake that yields a valid JSON number literal
//     keeps the value a number; redact, hash and mask produce a string.
//   - remove on a JSON container deletes the whole member (or array
//     element). On the document root it redacts the leaves instead.
//   - Rules win over redact_headers: a header or cookie rule sees the original
//     value. A value handled by a selector rule is not scanned by detectors.
//   - Detectors also scan the request path segments, JSON object keys, query
//     parameter names, Exchange.Error, and percent-encoded e-mail addresses.
//   - The anonymizer never panics on input; an internal panic is recovered and
//     returned as an error, and the caller must then discard the exchange.
package anonymize
