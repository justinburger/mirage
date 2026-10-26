package proxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
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
	if len(body) == 0 || truncated {
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

	hint := min(int64(len(body))*4, limit+1)
	buf := bytes.NewBuffer(make([]byte, 0, hint))
	if _, err := buf.ReadFrom(io.LimitReader(zr, limit+1)); err != nil {
		return nil, 0, false, "", false
	}
	out = buf.Bytes()
	if int64(len(out)) > limit {
		out = out[:limit]
		newTruncated = true
		newSize = max(size, limit+1)
	} else {
		newSize = int64(len(out))
	}
	// Trim slack so long-lived exchanges (the virtual index) do not keep a
	// quarter or more of their memory empty.
	if cap(out)-len(out) > 1024 && cap(out) > len(out)+len(out)/4 {
		out = bytes.Clone(out)
	}
	return out, newSize, newTruncated, enc, true
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
