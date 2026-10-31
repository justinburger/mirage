// Package bodycodec decodes HTTP bodies that carry a Content-Encoding, for
// capture (the proxy), anonymization (copies, exports, imports, the CLI) and
// matching (the virtual engine).
//
// The supported codings are gzip (and x-gzip) and deflate (zlib, or the raw
// deflate stream some servers send), including stacked codings such as
// "gzip, gzip" or "deflate, gzip", which are undone in reverse order.
// Anything else (br, zstd, compress) is reported as not decodable.
//
// Decoding always stops at a byte limit, as protection against decompression
// bombs: the result is then cut, marked truncated, and its size is at least
// limit+1.
package bodycodec

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/justinburger/mirage/internal/model"
)

// gzipReaders pools gzip readers; a reader costs about 40 KiB of tables.
var gzipReaders sync.Pool

// DecodeExchange decodes the request and response bodies of an exchange in
// place, stopping at limit decoded bytes per body (limit <= 0 does nothing).
// A decoded body loses its Content-Encoding and Content-Length headers and
// records the coding in ContentEncoding. Bodies that cannot be decoded
// strictly (unknown coding, cut or corrupt stream) are left as they are; see
// Decode.
func DecodeExchange(ex *model.Exchange, limit int64) {
	if limit <= 0 {
		return
	}
	decodeRequest(&ex.Request, limit, false)
	decodeResponse(&ex.Response, limit, false)
}

// PrepareForAnonymization makes the bodies of an exchange inspectable before
// it is anonymized. The anonymizer can only see plain bodies, and a body that
// is still compressed would be kept unchanged under opaque_bodies "keep",
// storing raw personal data in something that claims to be anonymized. So on
// top of DecodeExchange it
//
//   - decodes what the strict decoder refuses but that still yields readable
//     bytes: a stream cut by a capture cap (the decoded prefix is kept,
//     marked truncated) or a stream that turns corrupt part way, and
//   - drops, and marks truncated, every body that is still under a content
//     coding after that (an unknown coding such as br, or a stream that yields
//     nothing): it cannot be inspected, so it must not be kept.
//
// BodySize keeps at least the wire size, so the record still says how big the
// body was. limit must be positive.
func PrepareForAnonymization(ex *model.Exchange, limit int64) {
	DecodeExchange(ex, limit)
	req, resp := &ex.Request, &ex.Response
	makeInspectable(req.Header, &req.Body, &req.BodySize, &req.BodyTruncated, &req.ContentEncoding, limit)
	makeInspectable(resp.Header, &resp.Body, &resp.BodySize, &resp.BodyTruncated, &resp.ContentEncoding, limit)
}

func decodeRequest(r *model.Request, limit int64, lenient bool) {
	if b, size, trunc, enc, ok := decodeCore(r.Header, r.Body, r.BodySize, r.BodyTruncated, limit, lenient); ok {
		r.Body, r.BodySize, r.BodyTruncated, r.ContentEncoding = b, size, trunc, enc
		r.Header.Del("Content-Encoding")
		r.Header.Del("Content-Length")
	}
}

func decodeResponse(r *model.Response, limit int64, lenient bool) {
	if b, size, trunc, enc, ok := decodeCore(r.Header, r.Body, r.BodySize, r.BodyTruncated, limit, lenient); ok {
		r.Body, r.BodySize, r.BodyTruncated, r.ContentEncoding = b, size, trunc, enc
		r.Header.Del("Content-Encoding")
		r.Header.Del("Content-Length")
	}
}

// makeInspectable is the per-body step of PrepareForAnonymization.
func makeInspectable(h http.Header, body *[]byte, size *int64, truncated *bool, contentEncoding *string, limit int64) {
	if len(*body) == 0 || !StillEncoded(h) {
		return
	}
	if limit > 0 {
		if b, sz, trunc, enc, ok := DecodeLenient(h, *body, *size, *truncated, limit); ok {
			*body, *size, *truncated, *contentEncoding = b, sz, trunc, enc
			h.Del("Content-Encoding")
			h.Del("Content-Length")
			return
		}
	}
	*size = max(*size, int64(len(*body)))
	*body, *truncated = nil, true
}

// LooksCompressed reports whether body, recorded as already decoded from the
// content coding ce (a Content-Encoding list in the order the codings were
// applied), is in fact still compressed with the outermost one. A HAR stores
// bodies decoded and keeps the Content-Encoding header, so an importer cannot
// tell from the header alone; a stream the importer could not decode (cut by
// a capture cap, or an unknown coding) arrives looking the same as a decoded
// body.
//
// The tests are chosen so that a decoded body is practically never taken for
// a compressed one:
//
//   - gzip: the gzip magic number, which is never valid UTF-8.
//   - deflate, body not valid UTF-8: a complete zlib stream (its checksum
//     matches), or a zlib or raw deflate stream, possibly cut, that inflates
//     into more than 16 bytes of text, at least half as long as itself.
//     Binary data rarely inflates at all, and then into a few bytes of noise:
//     about one random body in 100,000 passes.
//   - deflate, body valid UTF-8 (only very short streams are): the whole body
//     must be one complete stream, to its last byte, that inflates into text.
//   - any other coding (br, zstd): bytes that are not valid UTF-8.
//
// A cut stream of binary data under a deflate coding is not recognised; the
// anonymizer could not read that data decoded either.
func LooksCompressed(ce string, body []byte) bool {
	if len(body) == 0 {
		return false
	}
	switch outerCoding(ce) {
	case "":
		return false
	case "gzip", "x-gzip":
		return len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b
	case "deflate":
		out, complete, _, rest, isZlib := inflateProbe(body)
		switch {
		case len(out) == 0:
			return false
		case len(out) >= looksCompressedProbe:
			return textPrefix(out) // still going: text this long is no accident
		case utf8.Valid(body):
			return complete && rest == 0 && utf8.Valid(out) && textPrefix(out)
		case isZlib && complete:
			return true
		}
		return len(out) > 16 && len(out) >= len(body)/2 && textPrefix(out)
	}
	return !utf8.Valid(body)
}

// looksCompressedProbe is how much LooksCompressed inflates at most.
const looksCompressedProbe = 64 << 10

// zlibHeader reports whether body starts with a zlib header (RFC 1950) that
// a decoder accepts: deflate, a window of at most 32 KiB, no preset
// dictionary, and the check bits.
func zlibHeader(body []byte) bool {
	return len(body) >= 2 && body[0]&0x0f == 8 && body[0]>>4 <= 7 && body[1]&0x20 == 0 &&
		(uint16(body[0])<<8|uint16(body[1]))%31 == 0
}

// inflateProbe inflates at most looksCompressedProbe bytes of body: as a zlib
// stream when it starts with a zlib header, else as a raw deflate stream.
// complete reports a stream that ended cleanly (for zlib, with a matching
// checksum), cut one that ran out of input part way (the body was cut), and
// rest the bytes of body after the end of the stream. flate reads a
// bytes.Reader byte by byte (it is an io.ByteReader), so what the reader has
// left is exactly what the stream did not use.
func inflateProbe(body []byte) (out []byte, complete, cut bool, rest int, isZlib bool) {
	br := bytes.NewReader(body)
	var r io.ReadCloser
	if isZlib = zlibHeader(body); isZlib {
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, false, false, 0, true
		}
		r = zr
	} else {
		r = flate.NewReader(br)
	}
	defer r.Close()
	out, err := readDecoded(r, int64(len(body))*4, looksCompressedProbe)
	full := len(out) >= looksCompressedProbe
	return out, err == nil && !full, errors.Is(err, io.ErrUnexpectedEOF), br.Len(), isZlib
}

// MayBeCompressed is LooksCompressed for deciding that a body must not be
// stored as readable data, as an anonymized artifact would: it errs the other
// way. Wrongly taking a decoded body for a compressed one only costs that
// body (it is decoded into noise or dropped); wrongly taking compressed
// personal data for decoded content keeps it, recoverable by anyone who
// inflates it. On top of LooksCompressed, under a deflate coding it accepts
//
//   - any zlib header (a preset dictionary included) on bytes that are not
//     valid UTF-8, and
//   - a zlib or raw deflate stream that uses the whole body, whatever it
//     inflates into: complete (followed by nothing but white space) or, for
//     bytes that are not valid UTF-8, cut short by the end of the body. That
//     covers text in a legacy charset or with control characters, and a cut
//     stack such as "gzip, deflate", whose inner stream is not text.
//
// A short decoded text body is rarely a complete deflate stream by accident
// (about one in 100,000); random binary data more often looks like a cut one.
func MayBeCompressed(ce string, body []byte) bool {
	if LooksCompressed(ce, body) {
		return true
	}
	if len(body) == 0 || outerCoding(ce) != "deflate" {
		return false
	}
	text := utf8.Valid(body)
	if !text && len(body) >= 2 && body[0]&0x0f == 8 && body[0]>>4 <= 7 && (uint16(body[0])<<8|uint16(body[1]))%31 == 0 {
		return true // a zlib header; a preset dictionary is allowed here
	}
	out, complete, cut, rest, _ := inflateProbe(body)
	if len(out) == 0 {
		return false
	}
	if complete {
		return rest == 0 || len(bytes.TrimSpace(body[len(body)-rest:])) == 0
	}
	return cut && !text && rest == 0
}

// outerCoding returns the last coding applied in ce, lower-cased ("" when
// there is none).
func outerCoding(ce string) string {
	outer := ""
	for c := range strings.SplitSeq(ce, ",") {
		if c = strings.ToLower(strings.TrimSpace(c)); c != "" && c != "identity" {
			outer = c
		}
	}
	return outer
}

// textPrefix reports whether b reads as text: valid UTF-8 once a rune cut at
// its end is ignored, no control characters other than tab, CR and LF, and,
// past 16 bytes, at least 8 distinct byte values, so that a few bytes of
// noise repeated by back-references do not pass.
func textPrefix(b []byte) bool {
	var seen [256]bool
	distinct := 0
	for _, c := range b {
		if (c < 0x20 && c != '\t' && c != '\n' && c != '\r') || c == 0x7f {
			return false
		}
		if !seen[c] {
			seen[c] = true
			distinct++
		}
	}
	if len(b) > 16 && distinct < 8 {
		return false
	}
	for cut := 0; cut < utf8.UTFMax && cut < len(b); cut++ {
		if utf8.Valid(b[:len(b)-cut]) {
			return true
		}
	}
	return false
}

// StillEncoded reports whether h announces a content coding other than
// identity.
func StillEncoded(h http.Header) bool {
	for _, c := range codings(h) {
		if c != "identity" {
			return true
		}
	}
	return false
}

// codings returns the content codings of h, lower-cased, in the order they
// were applied ("identity" entries included).
func codings(h http.Header) []string {
	var out []string
	for _, v := range h.Values("Content-Encoding") {
		for c := range strings.SplitSeq(v, ",") {
			if c = strings.ToLower(strings.TrimSpace(c)); c != "" {
				out = append(out, c)
			}
		}
	}
	return out
}

// layers returns the codings to undo, outermost (last applied) first, using
// the canonical names "gzip" and "deflate", and the coding to record in
// ContentEncoding. ok is false when there is nothing to undo or a coding is
// not supported.
func layers(h http.Header) (todo []string, enc string, ok bool) {
	cs := codings(h)
	applied := make([]string, 0, len(cs))
	for _, c := range cs {
		switch c {
		case "identity":
			continue
		case "gzip", "x-gzip":
			applied = append(applied, "gzip")
		case "deflate":
			applied = append(applied, "deflate")
		default:
			return nil, "", false
		}
	}
	if len(applied) == 0 {
		return nil, "", false
	}
	todo = make([]string, len(applied))
	for i, c := range applied {
		todo[len(applied)-1-i] = c
	}
	return todo, strings.Join(applied, ", "), true
}

// Decode decodes a body that carries gzip and/or deflate codings. It reports
// ok=false, and the caller keeps the body as it is, when there is nothing to
// decode, a coding is not supported, the captured bytes are incomplete
// (truncated), or the stream is corrupt.
//
// Decoding stops after limit bytes: the result is then cut, truncated is set
// and size is at least limit+1. Otherwise size is the decoded length. enc is
// the coding that was removed ("gzip", "deflate", "gzip, gzip", ...).
func Decode(h http.Header, body []byte, size int64, truncated bool, limit int64) (out []byte, newSize int64, newTruncated bool, enc string, ok bool) {
	return decodeCore(h, body, size, truncated, limit, false)
}

// DecodeLenient is Decode for bodies that must be inspected whatever their
// state: captured bytes cut by a cap, or a stream that fails after some
// output, still yield the decoded prefix. Such a result is always marked
// truncated and its size is at least one more than the bytes kept (the real
// decoded size is unknown). It reports ok=false when nothing could be decoded.
func DecodeLenient(h http.Header, body []byte, size int64, truncated bool, limit int64) (out []byte, newSize int64, newTruncated bool, enc string, ok bool) {
	return decodeCore(h, body, size, truncated, limit, true)
}

func decodeCore(h http.Header, body []byte, size int64, truncated bool, limit int64, lenient bool) (out []byte, newSize int64, newTruncated bool, enc string, ok bool) {
	if len(body) == 0 || limit <= 0 || (truncated && !lenient) {
		return nil, 0, false, "", false
	}
	todo, enc, ok := layers(h)
	if !ok {
		return nil, 0, false, "", false
	}
	data, cut := body, truncated
	for i, c := range todo {
		// Only the last (innermost) layer is bounded by limit; outer layers
		// are bounded too, generously, so a bomb in an outer layer stops.
		layerLimit := limit
		if i < len(todo)-1 {
			layerLimit = max(limit, 64<<20)
		}
		res, err := decodeOne(c, data, cut, layerLimit)
		if err != nil && (!lenient || len(res) == 0) {
			return nil, 0, false, "", false
		}
		if err != nil {
			cut = true
		}
		if int64(len(res)) > layerLimit {
			res = res[:layerLimit]
			cut = true
		}
		data = res
	}
	out = data
	if int64(len(out)) > limit {
		out = out[:limit]
		cut = true
	}
	if cut {
		newTruncated = true
		newSize = max(size, int64(len(out))+1)
	} else {
		newSize = int64(len(out))
	}
	// Trim slack so long-lived exchanges (the virtual index) do not keep a
	// quarter or more of their memory empty. A gzip body sized from its
	// trailer has none.
	if cap(out)-len(out) > 1024 && cap(out) > len(out)+len(out)/4 {
		out = bytes.Clone(out)
	}
	return out, newSize, newTruncated, enc, true
}

// errTrailingData reports a raw deflate stream that ended before the body did.
var errTrailingData = errors.New("bodycodec: data after the end of the deflate stream")

// decodeOne undoes one coding, reading at most limit+1 bytes. A raw deflate
// stream must use the whole body: it has no header or checksum, and a few
// random bytes form a valid stream that ends early about once in 200 tries,
// so a stream that leaves bytes over is not taken for one.
func decodeOne(coding string, body []byte, truncated bool, limit int64) ([]byte, error) {
	zr, rest, release, err := newDecoder(coding, body)
	if err != nil {
		return nil, err
	}
	defer release()
	out, err := readDecoded(zr, decodedSizeHint(coding, body, truncated, limit), limit+1)
	if err == nil && rest != nil && int64(len(out)) <= limit && rest() > 0 {
		err = errTrailingData
	}
	return out, err
}

// decodedSizeHint guesses the decoded length of body, to size the output
// buffer in one allocation. A complete gzip stream ends with its decoded
// length modulo 2^32 (RFC 1952), exact for the usual single-member stream; a
// wrong value only costs a regrow. Everything else is guessed from the
// compression ratio. The result is at most limit+1, the most that is read.
func decodedSizeHint(coding string, body []byte, truncated bool, limit int64) int64 {
	hint := int64(len(body)) * 4
	if coding == "gzip" && !truncated && len(body) >= 4 {
		if isize := int64(binary.LittleEndian.Uint32(body[len(body)-4:])); isize > 0 {
			hint = isize
		}
	}
	return min(hint, limit+1)
}

// readDecoded reads r until EOF or limit bytes, into a buffer of capacity
// hint that only grows when the data does not fit. Unlike bytes.Buffer it
// does not keep spare room after the last read: when the buffer is exactly
// full, a one byte probe tells whether more follows. limit is the number of
// bytes to read at most (callers pass decode limit+1 to detect overflow).
func readDecoded(r io.Reader, hint, limit int64) ([]byte, error) {
	buf := make([]byte, 0, max(min(hint, limit), 1))
	lr := io.LimitedReader{R: r, N: limit}
	for {
		if len(buf) == cap(buf) {
			var probe [1]byte
			n, err := lr.Read(probe[:])
			if n > 0 {
				buf = append(buf, probe[0])
			}
			if err != nil {
				if err == io.EOF {
					err = nil
				}
				return buf, err
			}
			continue
		}
		n, err := lr.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return buf, err
		}
	}
}

// newDecoder returns a reader over the decoded form of body and a function
// that releases it. HTTP "deflate" is the zlib format, but some servers send
// a raw deflate stream, so both are accepted; for a raw stream rest reports
// how many bytes of body the stream has not used (flate reads a bytes.Reader
// byte by byte, as an io.ByteReader), and is nil otherwise.
func newDecoder(coding string, body []byte) (r io.Reader, rest func() int, release func(), err error) {
	if coding == "gzip" {
		var zr *gzip.Reader
		if v := gzipReaders.Get(); v != nil {
			zr = v.(*gzip.Reader)
			if err := zr.Reset(bytes.NewReader(body)); err != nil {
				gzipReaders.Put(zr)
				return nil, nil, nil, err
			}
		} else {
			var err error
			if zr, err = gzip.NewReader(bytes.NewReader(body)); err != nil {
				return nil, nil, nil, err
			}
		}
		return zr, nil, func() { _ = zr.Close(); gzipReaders.Put(zr) }, nil
	}
	if zlibHeader(body) {
		zr, err := zlib.NewReader(bytes.NewReader(body))
		if err == nil {
			return zr, nil, func() { _ = zr.Close() }, nil
		}
	}
	br := bytes.NewReader(body)
	fr := flate.NewReader(br)
	return fr, br.Len, func() { _ = fr.Close() }, nil
}
