package proxy

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

// decodeExchange decodes the gzip or deflate request and response bodies of a
// captured exchange in place, stopping at limit decoded bytes per body. See
// decodeBody for the rules.
func decodeExchange(ex *model.Exchange, limit int64) {
	if limit <= 0 {
		return
	}
	req, resp := &ex.Request, &ex.Response
	if b, size, trunc, enc, ok := decodeBody(req.Header, req.Body, req.BodySize, req.BodyTruncated, limit); ok {
		req.Body, req.BodySize, req.BodyTruncated, req.ContentEncoding = b, size, trunc, enc
		req.Header.Del("Content-Encoding")
		req.Header.Del("Content-Length")
	}
	if b, size, trunc, enc, ok := decodeBody(resp.Header, resp.Body, resp.BodySize, resp.BodyTruncated, limit); ok {
		resp.Body, resp.BodySize, resp.BodyTruncated, resp.ContentEncoding = b, size, trunc, enc
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
	}
}

// decodeExchangeForAnon prepares an exchange for the anonymizer. The
// anonymizer can only inspect plain bodies, and a body it takes for
// "still compressed" is kept unchanged under opaque_bodies=keep, which would
// store raw personal data in a recording that claims to be anonymized. So on
// top of decodeExchange it
//
//   - decodes what the strict decoder refuses but that still yields readable
//     bytes: a stream cut by the capture cap (the decoded prefix is kept,
//     marked truncated) or a stream that turns corrupt part way, and
//   - drops, and marks truncated, every body that is still under a content
//     coding after that (an unknown or stacked coding, or a stream that
//     yields nothing): it cannot be inspected, so it is not stored.
//
// BodySize keeps the wire size, so the record still says how big the body was.
func decodeExchangeForAnon(ex *model.Exchange, limit int64) {
	decodeExchange(ex, limit)
	req, resp := &ex.Request, &ex.Response
	makeInspectable(req.Header, &req.Body, &req.BodySize, &req.BodyTruncated, &req.ContentEncoding, limit)
	makeInspectable(resp.Header, &resp.Body, &resp.BodySize, &resp.BodyTruncated, &resp.ContentEncoding, limit)
}

// makeInspectable is the per-body step of decodeExchangeForAnon.
func makeInspectable(h http.Header, body *[]byte, size *int64, truncated *bool, contentEncoding *string, limit int64) {
	if len(*body) == 0 || !stillEncoded(h) {
		return
	}
	if limit > 0 {
		if b, sz, trunc, enc, ok := decodeBodyLenient(h, *body, *size, *truncated, limit); ok {
			*body, *size, *truncated, *contentEncoding = b, sz, trunc, enc
			h.Del("Content-Encoding")
			h.Del("Content-Length")
			return
		}
	}
	*size = max(*size, int64(len(*body)))
	*body, *truncated = nil, true
}

// stillEncoded reports whether h announces a content coding other than
// identity.
func stillEncoded(h http.Header) bool {
	v := strings.TrimSpace(h.Get("Content-Encoding"))
	return v != "" && !strings.EqualFold(v, "identity")
}

// bodyEncoding returns the single content coding of h that decodeBody
// understands ("gzip" or "deflate"), or "" when there is none, there are
// several codings or the coding is unknown.
func bodyEncoding(h http.Header) string {
	v := h.Get("Content-Encoding")
	if v == "" {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "gzip", "x-gzip":
		return "gzip"
	case "deflate":
		return "deflate"
	}
	return ""
}

// decodeBody decodes a captured body that carries a gzip or deflate
// Content-Encoding. It reports ok=false, and the caller keeps the body as
// captured, when there is nothing to decode, when the captured bytes are
// incomplete (truncated), or when the stream is corrupt.
//
// Decoding stops after limit bytes (protection against decompression bombs):
// the result is then cut, truncated is set and size is at least limit+1.
// Otherwise size is the decoded length.
func decodeBody(h http.Header, body []byte, size int64, truncated bool, limit int64) (out []byte, newSize int64, newTruncated bool, enc string, ok bool) {
	return decodeCore(h, body, size, truncated, limit, false)
}

// decodeBodyLenient is decodeBody for bodies that must be inspected whatever
// their state: captured bytes cut by the cap, or a stream that fails after
// some output, still yield the decoded prefix. Such a result is always marked
// truncated and its size is at least one more than the bytes kept (the real
// decoded size is unknown). It reports ok=false when nothing could be decoded.
func decodeBodyLenient(h http.Header, body []byte, size int64, truncated bool, limit int64) (out []byte, newSize int64, newTruncated bool, enc string, ok bool) {
	return decodeCore(h, body, size, truncated, limit, true)
}

func decodeCore(h http.Header, body []byte, size int64, truncated bool, limit int64, lenient bool) (out []byte, newSize int64, newTruncated bool, enc string, ok bool) {
	if len(body) == 0 || (truncated && !lenient) {
		return nil, 0, false, "", false
	}
	enc = bodyEncoding(h)
	if enc == "" {
		return nil, 0, false, "", false
	}
	zr, release, err := newDecoder(enc, body)
	if err != nil {
		return nil, 0, false, "", false
	}
	defer release()

	out, err = readDecoded(zr, decodedSizeHint(enc, body, truncated, limit), limit+1)
	if err != nil && (!lenient || len(out) == 0) {
		return nil, 0, false, "", false
	}
	cut := err != nil || truncated
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

// decodedSizeHint guesses the decoded length of body, to size the output
// buffer in one allocation. A complete gzip stream ends with its decoded
// length modulo 2^32 (RFC 1952), exact for the usual single-member stream; a
// wrong value only costs a regrow. Everything else is guessed from the
// compression ratio. The result is at most limit+1, the most that is read.
func decodedSizeHint(enc string, body []byte, truncated bool, limit int64) int64 {
	hint := int64(len(body)) * 4
	if enc == "gzip" && !truncated && len(body) >= 4 {
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
func newDecoder(enc string, body []byte) (io.Reader, func(), error) {
	if enc == "gzip" {
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
