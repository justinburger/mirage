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

func isJSONType(mt string) bool {
	sub := subtype(mt)
	return sub == "json" || sub == "x-json" || strings.HasSuffix(sub, "+json")
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
	return strings.HasPrefix(sub, "grpc") || strings.HasPrefix(sub, "vnd.")
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

// classifyBody decides how a body is processed from its Content-Type and, when
// the type does not decide, from its bytes.
func classifyBody(ct string, b []byte, truncated bool) bodyKind {
	mt := mediaType(ct)
	switch {
	case isJSONType(mt):
		return kindJSON
	case isNDJSONType(mt):
		return kindNDJSON
	case mt == "application/x-www-form-urlencoded":
		return kindForm
	case isBinaryType(mt):
		return kindBinary
	case isTextType(mt):
		return kindText
	}
	// Unknown or missing type: look at the bytes.
	if looksBinary(b, truncated) {
		return kindBinary
	}
	if mt == "" && looksJSON(b) {
		return kindJSON
	}
	return kindText
}

// stillCompressed reports whether the stored body is compressed bytes. The
// proxy stores decoded bodies and records the original encoding in
// ContentEncoding; a body whose Content-Encoding header survives, or whose
// bytes are plainly compressed, could not be decoded and is left alone.
func stillCompressed(h http.Header, contentEncoding string, b []byte) bool {
	if v := strings.TrimSpace(getHeader(h, "Content-Encoding")); v != "" && !strings.EqualFold(v, "identity") {
		return true
	}
	if ce := strings.TrimSpace(contentEncoding); ce == "" || strings.EqualFold(ce, "identity") {
		return false
	}
	if len(b) >= 2 {
		switch {
		case b[0] == 0x1f && b[1] == 0x8b: // gzip
			return true
		case b[0]&0x0f == 8 && (uint(b[0])<<8|uint(b[1]))%31 == 0: // zlib / deflate
			return true
		case len(b) >= 4 && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd: // zstd
			return true
		}
	}
	return looksBinary(b, false)
}

// body anonymizes one body and keeps BodySize and Content-Length consistent.
func (r *run) body(body *[]byte, size *int64, truncated bool, contentEncoding string, h http.Header, ct string) {
	b := *body
	if len(b) == 0 || stillCompressed(h, contentEncoding, b) {
		return
	}
	loc := locator{side: r.side, where: "body"}
	var nb []byte
	changed := false
	switch classifyBody(ct, b, truncated) {
	case kindBinary:
		return
	case kindJSON:
		nb, changed = r.jsonBody(b, truncated, loc)
	case kindNDJSON:
		nb, changed = r.ndjsonBody(b, truncated, loc)
	case kindForm:
		loc.form = true
		s := string(b)
		if ns := r.params(s, r.sd.form, &loc); ns != s {
			nb, changed = []byte(ns), true
		}
	default:
		nb, changed = r.textBody(b, &loc)
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
// malformed document is still scanned by the regex rules and the detectors.
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
