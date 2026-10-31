package bodycodec

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func gz(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func zl(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func rawDeflate(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

var plain = []byte(`{"user":{"email":"jane.doe@acme-corp.io"},"password":"hunter2hunter2"}`)

func TestDecodeCodings(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		body         []byte
		wantEnc      string
	}{
		{"gzip", "gzip", gz(t, plain), "gzip"},
		{"x-gzip", "x-gzip", gz(t, plain), "gzip"},
		{"deflate zlib", "deflate", zl(t, plain), "deflate"},
		{"deflate raw", "deflate", rawDeflate(t, plain), "deflate"},
		{"gzip, gzip", "gzip, gzip", gz(t, gz(t, plain)), "gzip, gzip"},
		{"deflate then gzip", "deflate, gzip", gz(t, zl(t, plain)), "deflate, gzip"},
		{"identity around gzip", "identity, gzip", gz(t, plain), "gzip"},
		{"upper case", "GZIP", gz(t, plain), "gzip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{"Content-Encoding": {tc.header}}
			out, size, trunc, enc, ok := Decode(h, tc.body, int64(len(tc.body)), false, 1<<20)
			if !ok || !bytes.Equal(out, plain) || size != int64(len(plain)) || trunc || enc != tc.wantEnc {
				t.Fatalf("ok=%v out=%q size=%d trunc=%v enc=%q", ok, out, size, trunc, enc)
			}
		})
	}
}

func TestDecodeRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		body         []byte
	}{
		{"no coding", "", plain},
		{"identity", "identity", plain},
		{"brotli", "br", []byte{0x8b, 0x02, 0x80}},
		{"gzip then br", "gzip, br", gz(t, plain)},
		{"corrupt gzip", "gzip", []byte("\x1f\x8bnot really gzip")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.header != "" {
				h.Set("Content-Encoding", tc.header)
			}
			if _, _, _, _, ok := Decode(h, tc.body, int64(len(tc.body)), false, 1<<20); ok {
				t.Fatal("decoded")
			}
		})
	}
}

func TestDecodeLimitStopsBombs(t *testing.T) {
	big := bytes.Repeat([]byte("a"), 10<<20)
	for _, tc := range []struct {
		header string
		body   []byte
	}{
		{"gzip", gz(t, big)},
		{"gzip, gzip", gz(t, gz(t, big))},
	} {
		out, size, trunc, _, ok := Decode(http.Header{"Content-Encoding": {tc.header}}, tc.body, int64(len(tc.body)), false, 1000)
		if !ok || len(out) != 1000 || !trunc || size < 1001 {
			t.Errorf("%s: ok=%v len=%d trunc=%v size=%d", tc.header, ok, len(out), trunc, size)
		}
	}
}

func TestDecodeLenientKeepsPrefixOfCutStream(t *testing.T) {
	wire := gz(t, bytes.Repeat(plain, 200))
	h := http.Header{"Content-Encoding": {"gzip"}}
	if _, _, _, _, ok := Decode(h, wire[:len(wire)/2], int64(len(wire)), true, 1<<20); ok {
		t.Fatal("strict decode accepted a cut stream")
	}
	out, size, trunc, _, ok := DecodeLenient(h, wire[:len(wire)/2], int64(len(wire)), true, 1<<20)
	if !ok || !trunc || len(out) == 0 || !bytes.HasPrefix(bytes.Repeat(plain, 200), out) || size <= int64(len(out)) {
		t.Fatalf("ok=%v trunc=%v len=%d size=%d", ok, trunc, len(out), size)
	}
}

func TestPrepareForAnonymization(t *testing.T) {
	mk := func(enc string, body []byte) *model.Exchange {
		h := http.Header{"Content-Type": {"application/json"}}
		if enc != "" {
			h.Set("Content-Encoding", enc)
			h.Set("Content-Length", "1")
		}
		return &model.Exchange{Response: model.Response{Status: 200, Header: h, Body: body, BodySize: int64(len(body))}}
	}
	t.Run("stacked codings are decoded", func(t *testing.T) {
		ex := mk("gzip, gzip", gz(t, gz(t, plain)))
		PrepareForAnonymization(ex, 1<<20)
		r := ex.Response
		if !bytes.Equal(r.Body, plain) || r.ContentEncoding != "gzip, gzip" || r.Header.Get("Content-Encoding") != "" || r.Header.Get("Content-Length") != "" {
			t.Fatalf("body=%q enc=%q headers=%v", r.Body, r.ContentEncoding, r.Header)
		}
	})
	t.Run("an undecodable coding is dropped, never kept", func(t *testing.T) {
		wire := []byte("brotli bytes we cannot read")
		ex := mk("br", wire)
		PrepareForAnonymization(ex, 1<<20)
		r := ex.Response
		if r.Body != nil || !r.BodyTruncated || r.BodySize != int64(len(wire)) {
			t.Fatalf("body=%q truncated=%v size=%d", r.Body, r.BodyTruncated, r.BodySize)
		}
	})
	t.Run("a stream cut by a cap keeps its decoded prefix", func(t *testing.T) {
		var doc strings.Builder
		for i := range 2000 {
			fmt.Fprintf(&doc, `{"id":%d,"email":"user%d@acme-corp.io"}`, i*7919, i)
		}
		full := gz(t, []byte(doc.String()))
		ex := mk("gzip", full[:len(full)/3])
		ex.Response.BodySize, ex.Response.BodyTruncated = int64(len(full)), true
		PrepareForAnonymization(ex, 1<<20)
		r := ex.Response
		if len(r.Body) == 0 || !r.BodyTruncated || !strings.HasPrefix(doc.String(), string(r.Body)) {
			t.Fatalf("len=%d truncated=%v", len(r.Body), r.BodyTruncated)
		}
	})
	t.Run("plain bodies are untouched", func(t *testing.T) {
		ex := mk("", plain)
		PrepareForAnonymization(ex, 1<<20)
		if !bytes.Equal(ex.Response.Body, plain) || ex.Response.BodyTruncated {
			t.Fatal("plain body changed")
		}
	})
}

func TestStillEncoded(t *testing.T) {
	for v, want := range map[string]bool{"": false, "identity": false, "gzip": true, "identity, br": true, " Identity ": false} {
		h := http.Header{}
		if v != "" {
			h.Set("Content-Encoding", v)
		}
		if got := StillEncoded(h); got != want {
			t.Errorf("StillEncoded(%q) = %v, want %v", v, got, want)
		}
	}
}

func BenchmarkDecodeGzip(b *testing.B) {
	wire := gz(b, bytes.Repeat(plain, 30))
	h := http.Header{"Content-Encoding": {"gzip"}}
	b.ReportAllocs()
	for b.Loop() {
		if _, _, _, _, ok := Decode(h, wire, int64(len(wire)), false, 1<<20); !ok {
			b.Fatal("decode failed")
		}
	}
}
