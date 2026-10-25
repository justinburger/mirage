package har

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// roundTrip exports ex through the streaming encoder, decodes the document and
// converts the entry back.
func roundTrip(t *testing.T, ex *model.Exchange) *model.Exchange {
	t.Helper()
	var buf bytes.Buffer
	enc, err := NewEncoder(&buf, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := enc.Write(ex); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	h, err := Decode(&buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(h.Log.Entries) != 1 {
		t.Fatalf("entries = %d", len(h.Log.Entries))
	}
	back, err := ToExchange(&h.Log.Entries[0])
	if err != nil {
		t.Fatalf("ToExchange: %v", err)
	}
	return back
}

// wantRoundTrip is what an exchange looks like after a trip through HAR:
// store-assigned fields and the endpoint are gone.
func wantRoundTrip(ex *model.Exchange) *model.Exchange {
	w := ex.Clone()
	w.ID, w.RecordingID, w.Seq, w.ServiceID, w.Endpoint, w.Anonymized = 0, "", 0, "", "", false
	if len(w.Request.Body) == 0 {
		w.Request.Body = nil
	}
	if len(w.Response.Body) == 0 {
		w.Response.Body = nil
	}
	return w
}

func TestRoundTrip(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\xff\xfe")
	tests := []struct {
		name string
		ex   func() *model.Exchange
	}{
		{"sample", sampleExchange},
		{"post json with query", func() *model.Exchange {
			ex := sampleExchange()
			ex.Request.Method = "POST"
			ex.Request.Header.Set("Content-Type", "application/json")
			ex.Request.Body = []byte(`{"qty":2,"note":"café <b>&</b>"}`)
			ex.Request.BodySize = int64(len(ex.Request.Body))
			ex.Response.Status = 201
			return ex
		}},
		{"binary bodies", func() *model.Exchange {
			ex := sampleExchange()
			ex.Request.Method = "PUT"
			ex.Request.Header.Set("Content-Type", "application/octet-stream")
			ex.Request.Body = png
			ex.Request.BodySize = int64(len(png))
			ex.Response.Header.Set("Content-Type", "image/png")
			ex.Response.Body = png
			ex.Response.BodySize = int64(len(png))
			return ex
		}},
		{"content encoding", func() *model.Exchange {
			ex := sampleExchange()
			ex.Response.ContentEncoding = "gzip"
			ex.Request.ContentEncoding = "gzip"
			ex.Request.Method = "POST"
			ex.Request.Body = []byte("req")
			ex.Request.BodySize = 3
			return ex
		}},
		{"truncated", func() *model.Exchange {
			ex := sampleExchange()
			ex.Response.Body = []byte("partial")
			ex.Response.BodySize = 1 << 20
			ex.Response.BodyTruncated = true
			ex.Request.Method = "POST"
			ex.Request.Body = []byte("also partial")
			ex.Request.BodySize = 5000
			ex.Request.BodyTruncated = true
			return ex
		}},
		{"upstream error", func() *model.Exchange {
			ex := sampleExchange()
			ex.Error = "dial tcp 10.0.0.1:443: connect: connection refused"
			ex.Response = model.Response{Status: 502, Proto: "HTTP/1.1", Header: http.Header{"Content-Type": {"text/plain"}}, Body: []byte("Bad Gateway"), BodySize: 11}
			return ex
		}},
		{"redirect", func() *model.Exchange {
			ex := sampleExchange()
			ex.Response = model.Response{Status: 302, Proto: "HTTP/1.1", Header: http.Header{"Location": {"/elsewhere"}}}
			return ex
		}},
		{"no headers no body", func() *model.Exchange {
			return &model.Exchange{
				StartedAt: time.Date(2025, 1, 2, 3, 4, 5, 6_000, time.UTC),
				Duration:  model.Duration(time.Millisecond), TTFB: model.Duration(time.Millisecond / 2),
				Request:  model.Request{Method: "DELETE", Scheme: "http", Host: "svc.local:9000", Path: "/things/9", Proto: "HTTP/1.1"},
				Response: model.Response{Status: 204, Proto: "HTTP/1.1"},
			}
		}},
		{"escaped path and odd query", func() *model.Exchange {
			ex := sampleExchange()
			ex.Request.Path = "/files/a%2Fb/caf%C3%A9%20au%20lait"
			ex.Request.RawQuery = "name=caf%C3%A9&x=1&x=2&empty=&flag&plus=a+b&enc=%26%3D"
			return ex
		}},
		{"nanosecond timing", func() *model.Exchange {
			ex := sampleExchange()
			ex.Duration = model.Duration(1234567 * time.Nanosecond)
			ex.TTFB = model.Duration(987654 * time.Nanosecond)
			return ex
		}},
		{"unicode and control chars in body", func() *model.Exchange {
			ex := sampleExchange()
			ex.Response.Body = []byte("日本語     \x01\x1f \" \\ / emoji \U0001F600")
			ex.Response.BodySize = int64(len(ex.Response.Body))
			return ex
		}},
		{"many headers", func() *model.Exchange {
			ex := sampleExchange()
			for i := range 40 {
				ex.Request.Header.Add(fmt.Sprintf("X-H-%02d", i), fmt.Sprintf("v%d", i))
				ex.Response.Header.Add("X-Multi", fmt.Sprintf("m%d", i))
			}
			return ex
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ex := tc.ex()
			orig := ex.Clone()
			back := roundTrip(t, ex)
			want := wantRoundTrip(orig)
			if !reflect.DeepEqual(ex, orig) {
				t.Error("export modified the exchange")
			}
			// Compare the parts the contract promises, then everything.
			if back.Request.Method != want.Request.Method || back.Request.Scheme != want.Request.Scheme ||
				back.Request.Host != want.Request.Host || back.Request.Path != want.Request.Path ||
				back.Request.RawQuery != want.Request.RawQuery {
				t.Errorf("request line: %+v\nwant %+v", back.Request, want.Request)
			}
			if !reflect.DeepEqual(back.Request.Header, want.Request.Header) {
				t.Errorf("request headers:\n got %v\nwant %v", back.Request.Header, want.Request.Header)
			}
			if !reflect.DeepEqual(back.Response.Header, want.Response.Header) {
				t.Errorf("response headers:\n got %v\nwant %v", back.Response.Header, want.Response.Header)
			}
			if !bytes.Equal(back.Request.Body, want.Request.Body) || !bytes.Equal(back.Response.Body, want.Response.Body) {
				t.Errorf("bodies differ: req %q/%q resp %q/%q", back.Request.Body, want.Request.Body, back.Response.Body, want.Response.Body)
			}
			if back.Response.Status != want.Response.Status {
				t.Errorf("status = %d", back.Response.Status)
			}
			if !back.StartedAt.Equal(want.StartedAt) || back.Duration != want.Duration || back.TTFB != want.TTFB {
				t.Errorf("timing: %v %v %v, want %v %v %v", back.StartedAt, back.Duration, back.TTFB, want.StartedAt, want.Duration, want.TTFB)
			}
			if !reflect.DeepEqual(back, normalizeTimes(want, back)) {
				t.Errorf("full comparison:\n got %+v\nwant %+v", back, want)
			}
		})
	}
}

// normalizeTimes makes want use back's *time.Time representation (UTC) so
// DeepEqual compares instants rather than locations.
func normalizeTimes(want, back *model.Exchange) *model.Exchange {
	w := want.Clone()
	w.StartedAt = back.StartedAt
	return w
}

func TestRoundTripStatusZero(t *testing.T) {
	ex := sampleExchange()
	ex.Response = model.Response{}
	ex.Error = "dial tcp: lookup nosuch.invalid: no such host"
	back := roundTrip(t, ex)
	if back.Response.Status != 0 {
		t.Errorf("status = %d", back.Response.Status)
	}
	if !strings.HasPrefix(back.Error, "no response in HAR") || !strings.Contains(back.Error, "no such host") {
		t.Errorf("Error = %q", back.Error)
	}
}

func TestRoundTripPreservesOrderAndCount(t *testing.T) {
	var buf bytes.Buffer
	enc, err := NewEncoder(&buf, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const n = 250
	for i := range n {
		ex := sampleExchange()
		ex.StartedAt = base.Add(time.Duration(i) * 10 * time.Millisecond)
		ex.Request.Path = fmt.Sprintf("/items/%d", i)
		if err := enc.Write(ex); err != nil {
			t.Fatal(err)
		}
	}
	if enc.Count() != n {
		t.Errorf("Count = %d", enc.Count())
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	h, err := Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if h.Log.Version != "1.2" || h.Log.Creator.Name != "Mirage" || h.Log.Creator.Version != "1.2.3" || len(h.Log.Entries) != n {
		t.Fatalf("log = %+v (%d entries)", h.Log.Creator, len(h.Log.Entries))
	}
	exs, skipped := ToExchanges(h)
	if len(skipped) != 0 || len(exs) != n {
		t.Fatalf("ToExchanges: %d / %d skipped", len(exs), len(skipped))
	}
	for i, ex := range exs {
		if want := fmt.Sprintf("/items/%d", i); ex.Request.Path != want {
			t.Fatalf("entry %d path = %q, want %q", i, ex.Request.Path, want)
		}
		if want := base.Add(time.Duration(i) * 10 * time.Millisecond); !ex.StartedAt.Equal(want) {
			t.Fatalf("entry %d StartedAt = %v", i, ex.StartedAt)
		}
	}
}

func TestEncoderOutputIsValidHAR(t *testing.T) {
	for _, n := range []int{0, 1, 2, 17} {
		t.Run(fmt.Sprint(n, " entries"), func(t *testing.T) {
			var buf bytes.Buffer
			enc, err := NewEncoder(&buf, `1.0 "quoted" \ <tag> é`)
			if err != nil {
				t.Fatal(err)
			}
			// The header is written immediately, before any entry.
			if !strings.HasPrefix(buf.String(), `{"log":{"version":"1.2","creator":{"name":"Mirage","version":`) || buf.Len() == 0 {
				t.Errorf("header not written eagerly: %q", buf.String())
			}
			for range n {
				if err := enc.Write(sampleExchange()); err != nil {
					t.Fatal(err)
				}
			}
			if err := enc.Close(); err != nil {
				t.Fatal(err)
			}
			doc := buf.Bytes()
			if !json.Valid(doc) {
				t.Fatalf("invalid JSON:\n%s", doc)
			}
			var generic struct {
				Log struct {
					Version string `json:"version"`
					Creator struct {
						Name    string `json:"name"`
						Version string `json:"version"`
					} `json:"creator"`
					Entries []json.RawMessage `json:"entries"`
				} `json:"log"`
			}
			if err := json.Unmarshal(doc, &generic); err != nil {
				t.Fatal(err)
			}
			if generic.Log.Version != "1.2" || generic.Log.Creator.Name != "Mirage" || generic.Log.Creator.Version != `1.0 "quoted" \ <tag> é` {
				t.Errorf("header = %+v", generic.Log)
			}
			if generic.Log.Entries == nil || len(generic.Log.Entries) != n {
				t.Errorf("entries = %v (%d), want a non-null array of %d", generic.Log.Entries == nil, len(generic.Log.Entries), n)
			}
			h, err := Decode(bytes.NewReader(doc))
			if err != nil || len(h.Log.Entries) != n {
				t.Errorf("Decode: %v (%d entries)", err, len(h.Log.Entries))
			}
			// One entry per line, ending in a newline.
			if !bytes.HasSuffix(doc, []byte("]}}\n")) {
				t.Errorf("document ends with %q", doc[max(0, len(doc)-10):])
			}
			// Bodies are not HTML-escaped.
			if n > 0 && (!bytes.Contains(doc, []byte(`<jane@example.com>`)) || bytes.Contains(doc, []byte("\\u003c"))) {
				t.Error("HTML characters were escaped")
			}
		})
	}
}

func TestEncoderIncremental(t *testing.T) {
	// The document is valid JSON only once closed, but every entry is on the
	// wire as soon as Write returns.
	var buf bytes.Buffer
	enc, err := NewEncoder(&buf, "v")
	if err != nil {
		t.Fatal(err)
	}
	before := buf.Len()
	if err := enc.Write(sampleExchange()); err != nil {
		t.Fatal(err)
	}
	mid := buf.Len()
	if mid <= before {
		t.Error("Write did not reach the underlying writer")
	}
	if json.Valid(buf.Bytes()) {
		t.Error("document valid before Close")
	}
	if err := enc.Write(sampleExchange()); err != nil {
		t.Fatal(err)
	}
	if buf.Len() <= mid {
		t.Error("second Write did not reach the underlying writer")
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(buf.Bytes()) {
		t.Error("document invalid after Close")
	}
}

type countingWriter struct {
	calls int
	bytes int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.calls++
	c.bytes += len(p)
	return len(p), nil
}

func TestEncoderOneWritePerEntry(t *testing.T) {
	var cw countingWriter
	enc, err := NewEncoder(&cw, "v")
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := enc.Write(sampleExchange()); err != nil {
			t.Fatal(err)
		}
	}
	_ = enc.Close()
	if cw.calls != 1+5+1 {
		t.Errorf("underlying Write called %d times, want 7", cw.calls)
	}
}

func TestEncoderCloseIdempotent(t *testing.T) {
	var buf bytes.Buffer
	enc, _ := NewEncoder(&buf, "v")
	_ = enc.Write(sampleExchange())
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	after := buf.String()
	for range 3 {
		if err := enc.Close(); err != nil {
			t.Errorf("repeat Close = %v", err)
		}
	}
	if buf.String() != after {
		t.Error("repeat Close wrote more output")
	}
	if err := enc.Write(sampleExchange()); !errors.Is(err, ErrClosed) {
		t.Errorf("Write after Close = %v, want ErrClosed", err)
	}
	if buf.String() != after {
		t.Error("Write after Close produced output")
	}
	if !json.Valid(buf.Bytes()) {
		t.Error("invalid JSON")
	}
}

func TestEncoderDoesNotCloseWriter(t *testing.T) {
	cw := &closeTracker{}
	enc, _ := NewEncoder(cw, "v")
	_ = enc.Close()
	if cw.closed {
		t.Error("Close closed the underlying writer")
	}
}

type closeTracker struct {
	bytes.Buffer
	closed bool
}

func (c *closeTracker) Close() error { c.closed = true; return nil }

func TestEncoderNilExchange(t *testing.T) {
	var buf bytes.Buffer
	enc, _ := NewEncoder(&buf, "v")
	if err := enc.Write(nil); err == nil {
		t.Error("Write(nil) succeeded")
	}
	// The encoder is still usable.
	if err := enc.Write(sampleExchange()); err != nil {
		t.Fatal(err)
	}
	_ = enc.Close()
	if !json.Valid(buf.Bytes()) {
		t.Error("invalid JSON")
	}
}

// failingWriter fails after n successful writes.
type failingWriter struct {
	n   int
	err error
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, f.err
	}
	f.n--
	return len(p), nil
}

func TestEncoderWriteErrors(t *testing.T) {
	boom := errors.New("disk full")
	t.Run("header", func(t *testing.T) {
		enc, err := NewEncoder(&failingWriter{n: 0, err: boom}, "v")
		if !errors.Is(err, boom) || enc != nil {
			t.Errorf("NewEncoder = %v, %v", enc, err)
		}
	})
	t.Run("entry is sticky", func(t *testing.T) {
		fw := &failingWriter{n: 2, err: boom}
		enc, err := NewEncoder(fw, "v")
		if err != nil {
			t.Fatal(err)
		}
		if err := enc.Write(sampleExchange()); err != nil {
			t.Fatal(err)
		}
		if err := enc.Write(sampleExchange()); !errors.Is(err, boom) {
			t.Fatalf("second Write = %v", err)
		}
		fw.n = 100 // the writer recovers, the encoder must not resume mid-document
		if err := enc.Write(sampleExchange()); !errors.Is(err, boom) {
			t.Errorf("Write after failure = %v, want the sticky error", err)
		}
		if enc.Count() != 1 {
			t.Errorf("Count = %d, want 1", enc.Count())
		}
		if err := enc.Close(); !errors.Is(err, boom) {
			t.Errorf("Close = %v, want the sticky error", err)
		}
		if err := enc.Close(); !errors.Is(err, boom) {
			t.Errorf("repeat Close = %v", err)
		}
	})
	t.Run("close", func(t *testing.T) {
		enc, err := NewEncoder(&failingWriter{n: 1, err: boom}, "v")
		if err != nil {
			t.Fatal(err)
		}
		if err := enc.Close(); !errors.Is(err, boom) {
			t.Errorf("Close = %v", err)
		}
	})
	t.Run("short write is an error", func(t *testing.T) {
		enc, err := NewEncoder(io.Discard, "v")
		if err != nil {
			t.Fatal(err)
		}
		enc.w = shortWriter{}
		if err := enc.Write(sampleExchange()); err == nil {
			t.Error("short write not reported")
		}
	})
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, io.ErrShortWrite }

func TestEncoderConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	enc, err := NewEncoder(&buf, "v")
	if err != nil {
		t.Fatal(err)
	}
	const writers, each = 8, 100
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				ex := sampleExchange()
				ex.Request.Path = fmt.Sprintf("/w%d/%d", w, i)
				if err := enc.Write(ex); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	h, err := Decode(&buf)
	if err != nil || len(h.Log.Entries) != writers*each {
		t.Fatalf("Decode: %v, %d entries", err, len(h.Log.Entries))
	}
	// Per-writer order is preserved.
	next := make([]int, writers)
	for _, e := range h.Log.Entries {
		var w, i int
		if _, err := fmt.Sscanf(strings.TrimPrefix(e.Request.URL, "https://api.example.com"), "/w%d/%d", &w, &i); err != nil {
			t.Fatal(err)
		}
		if i != next[w] {
			t.Fatalf("writer %d: entry %d out of order (want %d)", w, i, next[w])
		}
		next[w]++
	}
}

func TestEncoderLargeBodyDoesNotPinMemory(t *testing.T) {
	enc, err := NewEncoder(io.Discard, "v")
	if err != nil {
		t.Fatal(err)
	}
	ex := sampleExchange()
	ex.Response.Body = bytes.Repeat([]byte("abcdefgh"), 1<<20) // 8 MiB
	ex.Response.BodySize = int64(len(ex.Response.Body))
	if err := enc.Write(ex); err != nil {
		t.Fatal(err)
	}
	if c := enc.buf.Cap(); c > maxRetainedBuffer {
		t.Errorf("encoder retains a %d byte buffer after a large entry", c)
	}
	if err := enc.Write(sampleExchange()); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestEncoderConstantMemory exports 100k entries and checks that memory use
// per entry is bounded and that nothing accumulates.
func TestEncoderConstantMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	const entries = 100_000
	small := &model.Exchange{
		StartedAt: time.Date(2024, 3, 5, 17, 20, 31, 0, time.UTC),
		Duration:  model.Duration(12 * time.Millisecond), TTFB: model.Duration(8 * time.Millisecond),
		Request: model.Request{Method: "GET", Scheme: "https", Host: "api.example.com", Path: "/v1/items/1", RawQuery: "a=1&b=2", Proto: "HTTP/1.1",
			Header: http.Header{"Accept": {"application/json"}, "User-Agent": {"loadgen/1.0"}}},
		Response: model.Response{Status: 200, Proto: "HTTP/1.1",
			Header:   http.Header{"Content-Type": {"application/json"}},
			Body:     []byte(`{"id":1,"ok":true}`),
			BodySize: 18},
	}
	enc, err := NewEncoder(io.Discard, "bench")
	if err != nil {
		t.Fatal(err)
	}
	// Warm up pools and buffers.
	for range 1000 {
		_ = enc.Write(small)
	}
	runtime.GC()
	var before, mid, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range entries / 2 {
		if err := enc.Write(small); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&mid)
	for range entries / 2 {
		if err := enc.Write(small); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}

	perEntry := float64(after.Mallocs-before.Mallocs) / entries
	bytesPerEntry := float64(after.TotalAlloc-before.TotalAlloc) / entries
	t.Logf("allocations per entry: %.1f, bytes per entry: %.0f", perEntry, bytesPerEntry)
	if perEntry > 100 {
		t.Errorf("%.1f allocations per entry, want <= 100", perEntry)
	}
	if bytesPerEntry > 12<<10 {
		t.Errorf("%.0f bytes allocated per entry, want <= 12 KiB", bytesPerEntry)
	}
	// Live heap after GC must not grow with the number of entries written:
	// 100k entries as a document would be tens of MiB.
	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("live heap growth over %d entries: %d bytes", entries, growth)
	if growth > 1<<20 {
		t.Errorf("live heap grew by %d bytes while streaming %d entries", growth, entries)
	}
	if second := int64(after.HeapAlloc) - int64(mid.HeapAlloc); second > 256<<10 {
		t.Errorf("live heap grew by %d bytes in the second half", second)
	}
}

func TestEncoderAllocsPerWrite(t *testing.T) {
	enc, err := NewEncoder(io.Discard, "v")
	if err != nil {
		t.Fatal(err)
	}
	ex := sampleExchange()
	for range 100 {
		_ = enc.Write(ex)
	}
	got := testing.AllocsPerRun(200, func() {
		if err := enc.Write(ex); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("allocs per Write: %.0f", got)
	if got > 120 {
		t.Errorf("%.0f allocations per Write, want <= 120", got)
	}
}

func TestDecodeFixtureThroughEncoder(t *testing.T) {
	// Real-world HAR -> exchanges -> streamed HAR -> exchanges: stable.
	h := fixture(t, "chrome.har")
	first, skipped := ToExchanges(h)
	if len(skipped) != 1 {
		t.Fatalf("skipped = %d", len(skipped))
	}
	var buf bytes.Buffer
	enc, _ := NewEncoder(&buf, "v")
	for _, ex := range first {
		if err := enc.Write(ex); err != nil {
			t.Fatal(err)
		}
	}
	_ = enc.Close()
	h2, err := Decode(iotest.HalfReader(&buf))
	if err != nil {
		t.Fatal(err)
	}
	second, skipped := ToExchanges(h2)
	if len(skipped) != 0 || len(second) != len(first) {
		t.Fatalf("second pass: %d exchanges, %d skipped", len(second), len(skipped))
	}
	for i := range first {
		a, b := first[i], second[i]
		if !a.StartedAt.Equal(b.StartedAt) {
			b.StartedAt = a.StartedAt
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("exchange %d changed on the second pass:\n first %+v\nsecond %+v", i, a, b)
		}
	}
}
