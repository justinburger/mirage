package har

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func gzipZeros(t testing.TB, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	chunk := make([]byte, 1<<20)
	for left := n; left > 0; {
		k := min(left, len(chunk))
		if _, err := zw.Write(chunk[:k]); err != nil {
			t.Fatal(err)
		}
		left -= k
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A small gzip body that inflates to hundreds of megabytes must not become
// hundreds of megabytes: it is cut at the proxy's capture cap and flagged.
func TestInflateIsCapped(t *testing.T) {
	bomb := gzipZeros(t, 100<<20) // ~100 KiB of gzip
	if len(bomb) > 1<<20 {
		t.Fatalf("test bomb is %d bytes", len(bomb))
	}
	e := &Entry{
		Request: Request{Method: "POST", URL: "http://x.test/", Headers: []NameValue{{Name: "Content-Encoding", Value: "gzip"}},
			PostData: &PostData{Text: base64.StdEncoding.EncodeToString(bomb), Encoding: "base64"}},
		Response: Response{Status: 200, Headers: []NameValue{{Name: "Content-Encoding", Value: "gzip"}},
			Content: Content{Text: base64.StdEncoding.EncodeToString(bomb), Encoding: "base64", Size: 100 << 20}},
	}
	ex := mustConvert(t, e)
	limit := int(model.DefaultMaxBodyBytes)
	if len(ex.Response.Body) != limit || len(ex.Request.Body) != limit {
		t.Fatalf("bodies are %d and %d bytes, want %d", len(ex.Request.Body), len(ex.Response.Body), limit)
	}
	if !ex.Response.BodyTruncated || !ex.Request.BodyTruncated {
		t.Errorf("truncated flags: request %v response %v", ex.Request.BodyTruncated, ex.Response.BodyTruncated)
	}
	if ex.Response.BodySize < int64(limit) || ex.Request.BodySize < int64(limit) {
		t.Errorf("BodySize %d / %d is smaller than the retained body", ex.Request.BodySize, ex.Response.BodySize)
	}
	if ex.Response.ContentEncoding != "gzip" {
		t.Errorf("content encoding = %q", ex.Response.ContentEncoding)
	}

	// A body that fits is untouched by the cap and not flagged.
	small := gzipZeros(t, 100<<10)
	ex = mustConvert(t, &Entry{Request: Request{Method: "GET", URL: "http://x.test/"}, Response: Response{
		Status: 200, Headers: []NameValue{{Name: "Content-Encoding", Value: "gzip"}},
		Content: Content{Text: base64.StdEncoding.EncodeToString(small), Encoding: "base64", Size: 100 << 10}}})
	if len(ex.Response.Body) != 100<<10 || ex.Response.BodyTruncated {
		t.Errorf("small body: %d bytes, truncated %v", len(ex.Response.Body), ex.Response.BodyTruncated)
	}
	// Exactly at the cap is not truncation.
	exact := gzipZeros(t, limit)
	ex = mustConvert(t, &Entry{Request: Request{Method: "GET", URL: "http://x.test/"}, Response: Response{
		Status: 200, Headers: []NameValue{{Name: "Content-Encoding", Value: "gzip"}},
		Content: Content{Text: base64.StdEncoding.EncodeToString(exact), Encoding: "base64", Size: Size(limit)}}})
	if len(ex.Response.Body) != limit || ex.Response.BodyTruncated {
		t.Errorf("body at the cap: %d bytes, truncated %v", len(ex.Response.Body), ex.Response.BodyTruncated)
	}
}

func TestDecodeEntriesRejectsOversizedEntry(t *testing.T) {
	old := maxEntryBytes
	maxEntryBytes = 64 << 10
	defer func() { maxEntryBytes = old }()

	mk := func(size int) string {
		e := Entry{Request: Request{Method: "GET", URL: "http://x.test/"}, Response: Response{Status: 200, Content: Content{Text: strings.Repeat("a", size)}}}
		b, _ := json.Marshal(e)
		return string(b)
	}
	doc := func(entries ...string) string {
		return `{"log":{"version":"1.2","creator":{"name":"t","version":"1"},"entries":[` + strings.Join(entries, ",") + `]}}`
	}

	var n int
	// Many entries that are each small fit, however large the document is.
	var many []string
	for i := 0; i < 40; i++ {
		many = append(many, mk(8<<10))
	}
	if _, err := DecodeEntries(strings.NewReader(doc(many...)), func(int, *Entry) error { n++; return nil }); err != nil || n != 40 {
		t.Fatalf("40 small entries: %d, %v", n, err)
	}

	// One entry past the limit fails, and the entries before it were delivered.
	n = 0
	_, err := DecodeEntries(strings.NewReader(doc(mk(1<<10), mk(200<<10), mk(1<<10))), func(int, *Entry) error { n++; return nil })
	if err == nil {
		t.Fatal("an oversized entry was accepted")
	}
	if !errors.Is(err, ErrEntryTooLarge) {
		t.Errorf("error %v does not wrap ErrEntryTooLarge", err)
	}
	if n != 1 {
		t.Errorf("%d entries delivered before the failure, want 1", n)
	}
	if !strings.Contains(err.Error(), "64 KiB") {
		t.Errorf("message does not state the limit: %v", err)
	}

	// The default limit is large enough for real captures.
	if old < 64<<20 {
		t.Errorf("default per-entry limit is only %d bytes", old)
	}
}
