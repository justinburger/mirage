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
	"io"
	"net/http"
	"strings"
	"sync"

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

// decodeOne undoes one coding, reading at most limit+1 bytes.
func decodeOne(coding string, body []byte, truncated bool, limit int64) ([]byte, error) {
	zr, release, err := newDecoder(coding, body)
	if err != nil {
		return nil, err
	}
	defer release()
	return readDecoded(zr, decodedSizeHint(coding, body, truncated, limit), limit+1)
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
// a raw deflate stream, so both are accepted.
func newDecoder(coding string, body []byte) (io.Reader, func(), error) {
	if coding == "gzip" {
		var zr *gzip.Reader
		if v := gzipReaders.Get(); v != nil {
			zr = v.(*gzip.Reader)
			if err := zr.Reset(bytes.NewReader(body)); err != nil {
				gzipReaders.Put(zr)
				return nil, nil, err
			}
		} else {
			var err error
			if zr, err = gzip.NewReader(bytes.NewReader(body)); err != nil {
				return nil, nil, err
			}
		}
		return zr, func() { _ = zr.Close(); gzipReaders.Put(zr) }, nil
	}
	if len(body) >= 2 && body[0]&0x0f == 8 && (uint16(body[0])<<8|uint16(body[1]))%31 == 0 {
		zr, err := zlib.NewReader(bytes.NewReader(body))
		if err == nil {
			return zr, func() { _ = zr.Close() }, nil
		}
	}
	fr := flate.NewReader(bytes.NewReader(body))
	return fr, func() { _ = fr.Close() }, nil
}
