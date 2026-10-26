package virtual

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"hash/maphash"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/justinburger/mirage/internal/model"
)

// param is one decoded query parameter.
type param struct{ name, value string }

func cmpParam(a, b param) int {
	if c := strings.Compare(a.name, b.name); c != 0 {
		return c
	}
	return strings.Compare(a.value, b.value)
}

// unescapeQuery decodes a query component, returning s itself (no
// allocation) when there is nothing to decode or it is malformed.
func unescapeQuery(s string) string {
	if strings.IndexByte(s, '%') < 0 && strings.IndexByte(s, '+') < 0 {
		return s
	}
	if u, err := url.QueryUnescape(s); err == nil {
		return u
	}
	return s
}

func (e *Engine) ignored(name string) bool {
	for _, p := range e.cfg.ignore {
		if p == name {
			return true
		}
	}
	return false
}

// parseParams splits a raw query into decoded parameters appended to dst.
// With prune set the parameters named in IgnoreParams are dropped; with sorted
// set the result is ordered by name, then value. A parameter without "=" has
// an empty value. Empty segments are skipped.
func (e *Engine) parseParams(raw string, dst []param, prune, sorted bool) []param {
	for raw != "" {
		var piece string
		piece, raw, _ = strings.Cut(raw, "&")
		if piece == "" {
			continue
		}
		name, value, _ := strings.Cut(piece, "=")
		name = unescapeQuery(name)
		if prune && e.ignored(name) {
			continue
		}
		dst = append(dst, param{name, unescapeQuery(value)})
	}
	if sorted && len(dst) > 1 {
		slices.SortFunc(dst, cmpParam)
	}
	return dst
}

// subsetOf reports whether every recorded parameter is present, with an equal
// value, among the request's.
func subsetOf(recorded, req []param) bool {
	for _, p := range recorded {
		if !slices.Contains(req, p) {
			return false
		}
	}
	return true
}

// appendSuffix appends the query, body and header parts of the exact-level
// key. Request and recorded sides use this same function, so equal inputs
// give equal keys.
func (e *Engine) appendSuffix(dst []byte, rawQuery string, body []byte, hdr http.Header) []byte {
	if e.cfg.query == model.QueryExact {
		var pb [8]param
		ps := e.parseParams(rawQuery, pb[:0], true, true)
		dst = binary.AppendUvarint(dst, uint64(len(ps)))
		for _, p := range ps {
			dst = binary.AppendUvarint(dst, uint64(len(p.name)))
			dst = append(dst, p.name...)
			dst = binary.AppendUvarint(dst, uint64(len(p.value)))
			dst = append(dst, p.value...)
		}
	}
	dst = e.appendBodyKey(dst, body)
	for _, name := range e.cfg.headers {
		vals := hdr[name]
		dst = binary.AppendUvarint(dst, uint64(len(vals)))
		for _, v := range vals {
			dst = binary.AppendUvarint(dst, uint64(len(v)))
			dst = append(dst, v...)
		}
	}
	return dst
}

// Body key tags.
const (
	tagBytes = 'b'
	tagJSON  = 'j'
)

func (e *Engine) appendBodyKey(dst []byte, body []byte) []byte {
	switch e.cfg.body {
	case model.BodyIgnore:
		return dst
	case model.BodyJSON:
		if len(body) > 0 {
			if h, ok := e.hashJSON(body); ok {
				return binary.LittleEndian.AppendUint64(append(dst, tagJSON), h)
			}
		}
	}
	return binary.LittleEndian.AppendUint64(append(dst, tagBytes), maphash.Bytes(e.seed, body))
}

// maxRequestBody bounds how much a compressed request body is inflated for
// matching.
const maxRequestBody = 32 << 20

// requestBody returns the request body as it would have been recorded: the
// capture stores request bodies decoded, so a gzip or deflate body is inflated
// for matching. A body that cannot be decoded is used as is.
func (e *Engine) requestBody(r *http.Request, body []byte) []byte {
	if e.cfg.body == model.BodyIgnore || len(body) == 0 {
		return body
	}
	ce := r.Header.Get("Content-Encoding")
	if ce == "" {
		return body
	}
	var zr io.Reader
	var err error
	switch strings.ToLower(strings.TrimSpace(ce)) {
	case "gzip", "x-gzip":
		zr, err = gzip.NewReader(bytes.NewReader(body))
	case "deflate":
		zr, err = zlib.NewReader(bytes.NewReader(body))
	default:
		return body
	}
	if err != nil {
		return body
	}
	out, err := io.ReadAll(io.LimitReader(zr, maxRequestBody+1))
	if err != nil || len(out) > maxRequestBody {
		return body
	}
	return out
}

// hashJSON returns a hash of the canonical form of body: it does not depend
// on whitespace, key order or string escaping, while numbers are compared as
// literals. ok is false when body is not valid JSON.
func (e *Engine) hashJSON(body []byte) (h uint64, ok bool) {
	if !json.Valid(body) {
		return 0, false
	}
	jh := jsonHasher{seed: e.seed, b: body}
	return jh.value(), true
}

// Type tags mixed into JSON hashes.
const (
	tagObject = 0x9e3779b97f4a7c15
	tagArray  = 0xc2b2ae3d27d4eb4f
	tagString = 0x165667b19e3779f9
	tagNumber = 0x27d4eb2f165667c5
	tagTrue   = 0xd6e8feb86659fd93
	tagFalse  = 0xa0761d6478bd642f
	tagNull   = 0xe7037ed1a0b428db
)

// mix combines two hashes (order-sensitive).
func mix(a, b uint64) uint64 {
	x := a ^ (b + 0x9e3779b97f4a7c15 + (a << 6) + (a >> 2))
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}

// jsonHasher walks a valid JSON document once, without allocating (except to
// decode strings that contain escapes). Object members are combined by
// addition, so their order does not matter.
type jsonHasher struct {
	seed maphash.Seed
	b    []byte
	i    int
}

func (h *jsonHasher) skipSpace() {
	for h.i < len(h.b) {
		switch h.b[h.i] {
		case ' ', '\t', '\n', '\r':
			h.i++
		default:
			return
		}
	}
}

func (h *jsonHasher) value() uint64 {
	h.skipSpace()
	switch h.b[h.i] {
	case '{':
		h.i++
		h.skipSpace()
		if h.b[h.i] == '}' {
			h.i++
			return mix(tagObject, 0)
		}
		var sum, n uint64
		for {
			h.skipSpace()
			k := h.str()
			h.skipSpace()
			h.i++ // ':'
			sum += mix(k, h.value())
			n++
			h.skipSpace()
			c := h.b[h.i]
			h.i++
			if c == '}' {
				return mix(mix(tagObject, n), sum)
			}
		}
	case '[':
		h.i++
		h.skipSpace()
		acc := uint64(tagArray)
		if h.b[h.i] == ']' {
			h.i++
			return acc
		}
		for {
			acc = mix(acc, h.value())
			h.skipSpace()
			c := h.b[h.i]
			h.i++
			if c == ']' {
				return acc
			}
		}
	case '"':
		return mix(tagString, h.str())
	case 't':
		h.i += 4
		return tagTrue
	case 'f':
		h.i += 5
		return tagFalse
	case 'n':
		h.i += 4
		return tagNull
	}
	start := h.i
	for h.i < len(h.b) {
		c := h.b[h.i]
		if c >= '0' && c <= '9' || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
			h.i++
			continue
		}
		break
	}
	return mix(tagNumber, maphash.Bytes(h.seed, h.b[start:h.i]))
}

// str hashes the string at h.i (which is its opening quote) by its decoded
// content and moves past it.
func (h *jsonHasher) str() uint64 {
	open := h.i
	h.i++
	start := h.i
	escaped := false
	for {
		c := h.b[h.i]
		if c == '\\' {
			escaped = true
			h.i += 2
			continue
		}
		if c == '"' {
			break
		}
		h.i++
	}
	raw := h.b[start:h.i]
	h.i++
	if escaped {
		var s string
		if json.Unmarshal(h.b[open:h.i], &s) == nil {
			return maphash.String(h.seed, s)
		}
	}
	return maphash.Bytes(h.seed, raw)
}
