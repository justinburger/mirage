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
// Bodies. JSON bodies (any Content-Type whose subtype contains "json"; and,
// because JSON is routinely sent under a wrong type, any text/*, unknown,
// missing or application/x-www-form-urlencoded body that opens like a JSON
// document) are rewritten preserving member order, number literals and
// unknown structure exactly; output is compact JSON. A body that turns out
// not to be JSON is processed as text (a form falls back to its form
// fields). Vendor types (application/vnd.*) are not assumed to be binary:
// they are sniffed, except the few known binary families. NDJSON is handled line
// by line. Form bodies are rewritten field by field. Other textual bodies get
// regex rules and detectors. When a body changes, BodySize is updated and any
// Content-Length header is fixed. A truncated body (BodyTruncated) is
// processed best-effort: JSON is rewritten as far as it goes, everything else
// as text.
//
// Opaque bodies. A body the engine cannot inspect is "opaque": a binary
// content type (image/*, application/pdf, protobuf, ...), unparsable binary
// bytes (an unknown or missing type with binary content, or a JSON-typed body
// that is neither JSON nor text), or a body that is still compressed (its
// Header still carries a Content-Encoding other than identity). The profile's OpaqueBodies setting
// decides what happens to it. "keep" (the default) stores it unchanged. "fill"
// replaces it with deterministic filler of exactly the same length: bytes of
// the keyed PRNG, seeded by the HMAC of the original body, so equal bodies get
// equal filler, different bodies get different filler, and no original byte
// is kept. BodySize and Content-Length stay as they were, and the change is
// reported at "request.body" or "response.body" with source "opaque_bodies"
// (the report states the size, never the bytes).
//
// Multipart. A multipart/form-data body with a boundary is processed part by
// part. A text part (no filename, a textual or missing Content-Type) is
// anonymized like a form field: a form rule whose selector is the part name
// wins (and a remove rule drops the whole part), otherwise the part is handled
// by its own content type, so a JSON part gets JSON rules, a text part regex
// rules, and every part the detectors. A file part (the Content-Disposition
// carries a filename) is handled by its own Content-Type and bytes in the same
// way, because a CSV, JSON or text export uploaded as a file is the usual
// carrier of customer data; only a binary file (an image, a PDF, bytes that
// are not text) is opaque. A part with a base64 or quoted-printable
// Content-Transfer-Encoding is decoded, inspected and encoded again if it
// changed (one that does not decode, or decodes to binary, is opaque). Opaque
// parts are kept or filled as above. The boundary, preamble, epilogue, delimiters and part
// headers are written back byte for byte; only part bodies change, and
// BodySize and Content-Length follow. A multipart body inside a part is split
// the same way, to a depth of three. A body that cannot be split (no boundary,
// no delimiter, a part cut off in its headers, no closing delimiter unless the
// capture was truncated) is malformed and falls back to the opaque handling:
// filled under "fill"; under "keep" kept if binary and scanned as text, like
// any body of unknown type, if textual. A truncated capture is split as far as
// it goes.
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
// with the default profile (benchmark it; BenchmarkExchangeBuiltinProfile is
// the real built-in profile with its rules, about 25µs: the sample carries the
// names and addresses the profile has faked since revision 2). Use sync.Pool for
// HMAC state. JSONPath rules of the form $..name, which the built-in profile
// is full of, are matched by a hash lookup of the member name instead of an
// automaton state each, so their number does not slow the walk down.
//
// Safety. Anonymization must never panic on malformed input; on a body it
// cannot parse it falls back to text processing. The redact strategy default
// replacement is "[REDACTED]".
//
// # Implementation notes
//
// Where this package goes beyond, or interprets, the rules above:
//
//   - A body counts as "still compressed" (opaque) when a Content-Encoding
//     header other than identity survives, or when ContentEncoding is set and
//     the bytes are plainly compressed (gzip, zlib, zstd magic, or not text).
//     A body the proxy decoded keeps its original encoding name in
//     ContentEncoding; that body is plain text and is anonymized.
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
//     On protocol-metadata headers (User-Agent, Server, Accept-*, validators,
//     dates) only the e-mail, JWT and AWS key detectors run: the numeric ones
//     read version numbers as addresses and validators as cards.
//   - The built-in profile fakes names and postal addresses where the field
//     name says what they are (first_name, customer_name, cardholder, address
//     and everything below it, street, city, zip, ...), and "name" only next
//     to an e-mail, phone or address member (the JSONPath sibling filter), so
//     product and file names survive. It does NOT find a name or address in
//     free text, in a URL path, in a field with an unusual name, or in a bare
//     "name" with no such neighbour: add a rule for those. Bodies of the other
//     formats get the XML password and token elements and the detectors only.
//   - The anonymizer never panics on input; an internal panic is recovered and
//     returned as an error, and the caller must then discard the exchange.
//   - Engine.Profile returns the profile without its secret (Secret empty,
//     HasSecret set), as model.AnonymizationProfile.Redacted does.
//   - Rule keys (JSON member names, query and form names, part names) match
//     exactly, including case: the built-in profile lists the usual spellings
//     (password, accessToken, access_token, ...) rather than matching loosely.
//   - A new part body, filled or not, never contains a line starting with the
//     part's own delimiter, so a short boundary cannot be hit by chance.
package anonymize
