package anonymize

import (
	"bytes"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

type bodyKind int

const (
	kindBinary bodyKind = iota
	kindJSON
	kindNDJSON
	kindForm
	kindText
)

// mediaType returns the lower-case media type of a Content-Type value without
// its parameters.
func mediaType(ct string) string {
	if ct == "" {
		return ""
	}
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		return mt
	}
	mt, _, _ := strings.Cut(ct, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}

func subtype(mt string) string {
	_, sub, _ := strings.Cut(mt, "/")
	return sub
}

// isJSONType reports whether the media type is JSON in any of its spellings:
// json, x-json, +json suffixes (problem+json, vnd.api+json), vendor types
// (vnd.acme.customer.v2.json) and RPC flavours (x-amz-json-1.1). Check
// isNDJSONType first: "x-ndjson" contains "json" too. A body that does not
// parse as JSON is processed as text anyway, so a loose match costs nothing.
func isJSONType(mt string) bool {
	return strings.Contains(subtype(mt), "json")
}

func isNDJSONType(mt string) bool {
	switch subtype(mt) {
	case "x-ndjson", "ndjson", "jsonl", "x-jsonl", "jsonlines", "x-jsonlines", "x-json-stream":
		return true
	}
	return false
}

func isTextType(mt string) bool {
	if strings.HasPrefix(mt, "text/") {
		return true
	}
	sub := subtype(mt)
	switch sub {
	case "xml", "javascript", "x-javascript", "ecmascript", "yaml", "x-yaml", "graphql",
		"sql", "x-sh", "xhtml+xml", "x-www-form-urlencoded":
		return true
	}
	return strings.HasSuffix(sub, "+xml") || strings.HasSuffix(sub, "+yaml")
}

// isBinaryType lists types whose bytes are framed by lengths or checksums, so
// that substituting text inside them would corrupt them. They are left alone.
func isBinaryType(mt string) bool {
	switch {
	case strings.HasPrefix(mt, "image/"), strings.HasPrefix(mt, "audio/"),
		strings.HasPrefix(mt, "video/"), strings.HasPrefix(mt, "font/"),
		strings.HasPrefix(mt, "model/"):
		return true
	}
	sub := subtype(mt)
	switch sub {
	case "pdf", "zip", "gzip", "x-gzip", "x-tar", "x-bzip2", "x-7z-compressed",
		"x-rar-compressed", "zstd", "protobuf", "x-protobuf", "msgpack", "x-msgpack",
		"cbor", "avro", "x-thrift", "wasm", "x-www-form-urlencoded-binary":
		return true
	}
	return strings.HasPrefix(sub, "grpc") || isBinaryVendorType(sub)
}

// binaryVendorPrefixes are vendor (vnd.*) subtypes known to be binary
// formats. Any other vnd.* type is left to the byte sniffing: most are JSON or
// XML under another name, and treating them all as binary stored their
// credentials and PII in clear.
var binaryVendorPrefixes = []string{
	"vnd.openxmlformats", "vnd.ms-", "vnd.oasis.opendocument", "vnd.android.", "vnd.apple.",
	"vnd.google-earth.kmz", "vnd.rar", "vnd.sqlite", "vnd.mapbox-vector-tile", "vnd.msgpack",
	"vnd.google.protobuf", "vnd.tcpdump", "vnd.lotus", "vnd.rn-", "vnd.visio", "vnd.debian",
	"vnd.adobe.", "vnd.sun.", "vnd.wap.wbxml", "vnd.3gpp", "vnd.amazon.ebook",
}

func isBinaryVendorType(sub string) bool {
	if !strings.HasPrefix(sub, "vnd.") || strings.HasSuffix(sub, "+xml") || strings.HasSuffix(sub, "+yaml") {
		return false
	}
	for _, p := range binaryVendorPrefixes {
		if strings.HasPrefix(sub, p) {
			return true
		}
	}
	return false
}

// trimPartialRune cuts an incomplete UTF-8 sequence off the end of p.
func trimPartialRune(p []byte) []byte {
	for i := 1; i <= utf8.UTFMax-1 && i <= len(p); i++ {
		c := p[len(p)-i]
		if utf8.RuneStart(c) {
			if !utf8.FullRune(p[len(p)-i:]) {
				return p[:len(p)-i]
			}
			return p
		}
	}
	return p
}

// looksBinary sniffs the start of a body: a NUL byte or invalid UTF-8 means
// binary. A multi-byte character cut by the capture limit does not count.
func looksBinary(b []byte, mayBeCut bool) bool {
	n := min(len(b), 8192)
	p := b[:n]
	if bytes.IndexByte(p, 0) >= 0 {
		return true
	}
	if n < len(b) || mayBeCut {
		p = trimPartialRune(p)
	}
	return !utf8.Valid(p)
}

func looksJSON(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		case '{', '[':
			return true
		}
		return false
	}
	return false
}

// looksJSONDocument is the stricter test for a body whose declared type says
// otherwise (curl -d sends JSON as application/x-www-form-urlencoded): it
// opens with { or [ and, unless the capture was cut, ends with the matching
// closer. A real urlencoded body never does.
func looksJSONDocument(b []byte, truncated bool) bool {
	if !looksJSON(b) {
		return false
	}
	if truncated {
		return bytes.IndexByte(b, '"') >= 0
	}
	first, last := byte(0), byte(0)
	for _, c := range b {
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			first = c
			break
		}
	}
	for i := len(b) - 1; i >= 0; i-- {
		if c := b[i]; c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			last = c
			break
		}
	}
	return first == '{' && last == '}' || first == '[' && last == ']'
}

// classifyBody decides how a body is processed from its Content-Type and, when
// the type does not decide, from its bytes.
func classifyBody(ct string, b []byte, truncated bool) bodyKind {
	return classifyMedia(mediaType(ct), b, truncated)
}

// classifyMedia is classifyBody for an already parsed media type. The declared
// type is trusted when it names a format and is checked against the bytes
// when it can be wrong: JSON is routinely sent as text/plain, as
// application/octet-stream, under a vendor type or, with curl -d, as a form,
// and the credential rules must reach it all the same. A body that is not
// JSON after all falls back to text processing.
func classifyMedia(mt string, b []byte, truncated bool) bodyKind {
	switch {
	case isNDJSONType(mt):
		return kindNDJSON
	case isJSONType(mt):
		return kindJSON
	case mt == "application/x-www-form-urlencoded":
		if looksJSONDocument(b, truncated) {
			return kindJSON
		}
		return kindForm
	case isBinaryType(mt):
		return kindBinary
	case strings.HasPrefix(mt, "text/"):
		if looksJSON(b) {
			return kindJSON
		}
		return kindText
	case isTextType(mt):
		return kindText
	}
	// Unknown or missing type: look at the bytes.
	if looksBinary(b, truncated) {
		return kindBinary
	}
	if looksJSON(b) {
		return kindJSON
	}
	return kindText
}

// stillCompressed reports whether the stored body is compressed bytes: its
// Content-Encoding header survived (the body could not be decoded), or it
// starts with a gzip or zstd magic number although ContentEncoding says it was
// decoded. Anything else under ContentEncoding is decoded content and goes
// through the rules like any other body; that includes text in a legacy
// charset such as ISO-8859-1, which is not valid UTF-8. Every caller that
// stores the result decodes bodies first and drops what is still compressed
// (bodycodec.PrepareForAnonymization), so the engine never needs to guess.
func stillCompressed(h http.Header, contentEncoding string, b []byte) bool {
	if v := strings.TrimSpace(getHeader(h, "Content-Encoding")); v != "" && !strings.EqualFold(v, "identity") {
		return true
	}
	if ce := strings.TrimSpace(contentEncoding); ce == "" || strings.EqualFold(ce, "identity") {
		return false
	}
	switch {
	case len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b: // gzip
		return true
	case len(b) >= 4 && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd: // zstd
		return true
	}
	return false
}

// body anonymizes one body and keeps BodySize and Content-Length consistent.
func (r *run) body(body *[]byte, size *int64, truncated bool, contentEncoding string, h http.Header, ct string) {
	b := *body
	if len(b) == 0 {
		return
	}
	loc := locator{side: r.side, where: "body"}
	var nb []byte
	var changed bool
	if stillCompressed(h, contentEncoding, b) {
		nb, changed = r.opaque(b, &loc)
	} else {
		nb, changed = r.inspect(b, ct, truncated, &loc)
	}
	if !changed {
		return
	}
	old := int64(len(b))
	*body = nb
	if truncated {
		// BodySize is the size on the wire and the capture is only a prefix:
		// move it by the amount the prefix changed.
		*size += int64(len(nb)) - old
		if *size < int64(len(nb)) {
			*size = int64(len(nb))
		}
	} else {
		*size = int64(len(nb))
	}
	if k, ok := headerKey(h, "Content-Length"); ok {
		h[k] = []string{strconv.FormatInt(*size, 10)}
	}
}

// inspect anonymizes the bytes of a body whose Content-Type is ct (a whole
// body, or one part of a multipart body) and returns the new bytes. changed
// is false when nothing needs to be written back.
func (r *run) inspect(b []byte, ct string, truncated bool, loc *locator) (nb []byte, changed bool) {
	mt := mediaType(ct)
	if mt == "multipart/form-data" || (r.mpDepth > 0 && strings.HasPrefix(mt, "multipart/")) {
		return r.multipartBody(b, ct, truncated, loc)
	}
	switch classifyMedia(mt, b, truncated) {
	case kindBinary:
		return r.opaque(b, loc)
	case kindJSON:
		return r.jsonBody(b, truncated, *loc)
	case kindNDJSON:
		return r.ndjsonBody(b, truncated, *loc)
	case kindForm:
		l := *loc
		l.form = true
		s := string(b)
		if ns := r.params(s, r.sd.form, &l); ns != s {
			return []byte(ns), true
		}
		return nil, false
	default:
		return r.textBody(b, loc)
	}
}

// opaque handles a body the engine cannot inspect: binary content, a body
// still carrying a Content-Encoding, a binary or malformed multipart part.
// With OpaqueBodies "keep" it is left alone. With "fill" it is replaced by
// deterministic filler of the same length (see keyer.opaqueFill) and the
// replacement is reported with source "opaque_bodies".
func (r *run) opaque(b []byte, loc *locator) ([]byte, bool) {
	if !r.c.fill || len(b) == 0 {
		return nil, false
	}
	nb := r.k.opaqueFill(b)
	if r.rep != nil && !r.rep.full() {
		n := itoa(len(b))
		r.rep.add(loc, "opaque_bodies", "["+n+" opaque bytes]", "["+n+" filler bytes]")
	}
	return nb, true
}

func (r *run) textBody(b []byte, loc *locator) ([]byte, bool) {
	s := string(b)
	ns := r.text(s, loc)
	if ns == s {
		return nil, false
	}
	return []byte(ns), true
}

// jsonBody rewrites a JSON body. A truncated body is rewritten as far as it
// goes. Anything that does not parse falls back to text processing, so a
// malformed document is still scanned by the regex rules and the detectors;
// under OpaqueBodies "fill" a document that does not parse and is binary is
// filled instead.
func (r *run) jsonBody(b []byte, truncated bool, loc locator) ([]byte, bool) {
	mark := 0
	if r.rep != nil {
		mark = r.rep.mark()
	}
	out, changed, err := r.rewriteJSON(b, truncated, loc)
	if err == nil {
		return out, changed
	}
	if r.rep != nil {
		r.rep.rollback(mark)
	}
	if errors.Is(err, errJSONTrailing) {
		// Several documents back to back: handle them line by line.
		return r.ndjsonBody(b, truncated, loc)
	}
	if r.c.fill && looksBinary(b, truncated) {
		// Not JSON and not text either: nothing in it can be inspected.
		return r.opaque(b, &loc)
	}
	return r.textBody(b, &loc)
}

// ndjsonBody rewrites newline-delimited JSON line by line. A line that is not
// valid JSON is processed as text, so one bad line never exposes the rest.
func (r *run) ndjsonBody(b []byte, truncated bool, loc locator) ([]byte, bool) {
	var out []byte // built lazily on the first change
	line := 0
	for pos := 0; pos <= len(b); {
		end := bytes.IndexByte(b[pos:], '\n')
		last := end < 0
		if last {
			end = len(b)
		} else {
			end += pos
		}
		raw := b[pos:end]
		line++
		piece := raw
		changed := false
		if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 {
			l := loc
			l.line = line
			mark := 0
			if r.rep != nil {
				mark = r.rep.mark()
			}
			o, ch, err := r.rewriteJSON(trimmed, truncated && last, l)
			switch {
			case err == nil && ch:
				piece, changed = o, true
				if bytes.HasSuffix(raw, []byte{'\r'}) {
					piece = append(piece, '\r')
				}
			case err != nil:
				if r.rep != nil {
					r.rep.rollback(mark)
				}
				s := string(raw)
				if ns := r.text(s, &l); ns != s {
					piece, changed = []byte(ns), true
				}
			}
		}
		if out == nil && changed {
			out = append(make([]byte, 0, len(b)+64), b[:pos]...)
		}
		if out != nil {
			out = append(out, piece...)
			if !last {
				out = append(out, '\n')
			}
		}
		pos = end + 1
	}
	if out == nil {
		return nil, false
	}
	return out, true
}
