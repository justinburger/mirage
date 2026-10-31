package har

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"testing"
	"time"
)

func codingGzip(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func codingZlib(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write(b)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func codingRawDeflate(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(b)
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A HAR body that is still compressed is decoded whatever the coding: deflate
// (zlib or raw) and stacks such as "gzip, gzip" were left as they were when
// only a single gzip layer was understood.
func TestToExchangeDecodesAllCodings(t *testing.T) {
	const payload = `{"email":"jane@acme.io","password":"Pa$$w0rd!x","pad":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	p := []byte(payload)
	cases := []struct {
		name, header string
		raw          []byte
		wantEnc      string
	}{
		{"gzip", "gzip", codingGzip(t, p), "gzip"},
		{"x-gzip", "x-gzip", codingGzip(t, p), "x-gzip"},
		{"deflate zlib", "deflate", codingZlib(t, p), "deflate"},
		{"deflate raw", "deflate", codingRawDeflate(t, p), "deflate"},
		{"gzip twice", "gzip, gzip", codingGzip(t, codingGzip(t, p)), "gzip, gzip"},
		{"upper case list", "GZIP,  Gzip", codingGzip(t, codingGzip(t, p)), "gzip, gzip"},
		{"deflate then gzip", "deflate, gzip", codingGzip(t, codingZlib(t, p)), "deflate, gzip"},
		{"gzip then deflate", "gzip, deflate", codingZlib(t, codingGzip(t, p)), "gzip, deflate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b64 := base64.StdEncoding.EncodeToString(tc.raw)
			e := &Entry{
				Request: Request{Method: "POST", URL: "http://x.test/",
					Headers:  []NameValue{{Name: "Content-Encoding", Value: tc.header}},
					PostData: &PostData{Text: b64, Encoding: "base64"}},
				Response: Response{Status: 200,
					Headers: []NameValue{{Name: "Content-Encoding", Value: tc.header}},
					Content: Content{Text: b64, Encoding: "base64", Size: Size(len(payload))}},
			}
			ex := mustConvert(t, e)
			for side, got := range map[string]struct {
				body []byte
				enc  string
				cut  bool
			}{
				"request":  {ex.Request.Body, ex.Request.ContentEncoding, ex.Request.BodyTruncated},
				"response": {ex.Response.Body, ex.Response.ContentEncoding, ex.Response.BodyTruncated},
			} {
				if string(got.body) != payload || got.enc != tc.wantEnc || got.cut {
					t.Errorf("%s: body %q, encoding %q, truncated %v", side, got.body, got.enc, got.cut)
				}
			}
			if ex.Request.Header.Get("Content-Encoding") != "" || ex.Response.Header.Get("Content-Encoding") != "" {
				t.Errorf("the Content-Encoding header was kept: %v / %v", ex.Request.Header, ex.Response.Header)
			}
		})
	}

	t.Run("repeated header lines form a list", func(t *testing.T) {
		raw := codingGzip(t, codingGzip(t, p))
		e := &Entry{Request: Request{Method: "GET", URL: "http://x.test/"}, Response: Response{Status: 200,
			Headers: []NameValue{{Name: "Content-Encoding", Value: "gzip"}, {Name: "content-encoding", Value: "gzip"}},
			Content: Content{Text: base64.StdEncoding.EncodeToString(raw), Encoding: "base64"}}}
		ex := mustConvert(t, e)
		if string(ex.Response.Body) != payload || ex.Response.ContentEncoding != "gzip, gzip" {
			t.Errorf("body %q, encoding %q", ex.Response.Body, ex.Response.ContentEncoding)
		}
	})
	t.Run("identity in a list is not a coding", func(t *testing.T) {
		raw := codingGzip(t, p)
		e := &Entry{Request: Request{Method: "GET", URL: "http://x.test/"}, Response: Response{Status: 200,
			Headers: []NameValue{{Name: "Content-Encoding", Value: "identity, gzip"}},
			Content: Content{Text: base64.StdEncoding.EncodeToString(raw), Encoding: "base64"}}}
		ex := mustConvert(t, e)
		if string(ex.Response.Body) != payload {
			t.Errorf("body %q", ex.Response.Body)
		}
	})
	t.Run("a decoded text body under deflate stays", func(t *testing.T) {
		// Browsers save the decoded text next to the original header.
		for _, text := range []string{payload, "{", "x^", "plain words", "\x03\x00"} {
			e := &Entry{Request: Request{Method: "GET", URL: "http://x.test/"}, Response: Response{Status: 200,
				Headers: []NameValue{{Name: "Content-Encoding", Value: "deflate"}},
				Content: Content{Text: text, Size: Size(len(text))}}}
			if ex := mustConvert(t, e); string(ex.Response.Body) != text || ex.Response.ContentEncoding != "deflate" {
				t.Errorf("text %q became %q (encoding %q)", text, ex.Response.Body, ex.Response.ContentEncoding)
			}
		}
	})
	t.Run("a raw deflate stream that is valid UTF-8 is decoded", func(t *testing.T) {
		// zlib's raw deflate of "ssn=314-87-8072" happens to be valid UTF-8,
		// so a HAR can carry it as text; it must not pass for decoded text.
		raw := "+.\xce\xb3564\xd1\xb50\xd7\xb5007\x02\x00"
		e := &Entry{Request: Request{Method: "GET", URL: "http://x.test/"}, Response: Response{Status: 200,
			Headers: []NameValue{{Name: "Content-Encoding", Value: "deflate"}},
			Content: Content{Text: raw}}}
		if ex := mustConvert(t, e); string(ex.Response.Body) != "ssn=314-87-8072" || ex.Response.ContentEncoding != "deflate" {
			t.Errorf("body %q, encoding %q", ex.Response.Body, ex.Response.ContentEncoding)
		}
	})
	t.Run("a cut stream is left alone", func(t *testing.T) {
		for _, raw := range [][]byte{codingGzip(t, p)[:20], codingZlib(t, p)[:14], codingGzip(t, codingGzip(t, p))[:30]} {
			e := &Entry{Request: Request{Method: "GET", URL: "http://x.test/"}, Response: Response{Status: 200,
				Headers: []NameValue{{Name: "Content-Encoding", Value: "gzip, gzip"}},
				Content: Content{Text: base64.StdEncoding.EncodeToString(raw), Encoding: "base64"}}}
			if ex := mustConvert(t, e); !bytes.Equal(ex.Response.Body, raw) {
				t.Errorf("a cut stream was changed: % x", ex.Response.Body)
			}
		}
	})
	t.Run("an unknown coding in the stack is left alone", func(t *testing.T) {
		raw := codingGzip(t, p)
		e := &Entry{Request: Request{Method: "GET", URL: "http://x.test/"}, Response: Response{Status: 200,
			Headers: []NameValue{{Name: "Content-Encoding", Value: "br, gzip"}},
			Content: Content{Text: base64.StdEncoding.EncodeToString(raw), Encoding: "base64"}}}
		ex := mustConvert(t, e)
		if !bytes.Equal(ex.Response.Body, raw) || ex.Response.ContentEncoding != "br, gzip" {
			t.Errorf("body % x, encoding %q", ex.Response.Body, ex.Response.ContentEncoding)
		}
	})
}

func TestStartTimeStorable(t *testing.T) {
	cases := []struct {
		name string
		t    time.Time
		ok   bool
	}{
		{"now", time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC), true},
		{"1970 plus a second", time.Unix(1, 0), true},
		{"1900", time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{"2200", time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{"zero time (.NET 0001-01-01)", time.Time{}, false},
		{"year 1 with an offset", time.Date(1, 1, 1, 0, 0, 0, 0, time.FixedZone("x", 3600)), false},
		{"year 1500", time.Date(1500, 6, 1, 0, 0, 0, 0, time.UTC), false},
		{"year 1677", time.Date(1677, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"year 2262 end", time.Date(2262, 12, 31, 0, 0, 0, 0, time.UTC), false},
		{"year 9999", time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), false},
		{"the Unix epoch reads back as unknown", time.Unix(0, 0), false},
	}
	for _, tc := range cases {
		if got := StartTimeStorable(tc.t); got != tc.ok {
			t.Errorf("%s: StartTimeStorable(%v) = %v, want %v", tc.name, tc.t, got, tc.ok)
		}
	}
	// What is accepted survives a trip through int64 nanoseconds.
	for _, tc := range cases {
		if tc.ok {
			if back := time.Unix(0, tc.t.UnixNano()); !back.Equal(tc.t) {
				t.Errorf("%s: %v became %v", tc.name, tc.t, back)
			}
		}
	}
}
