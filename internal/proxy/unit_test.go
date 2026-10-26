package proxy

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

func TestCapBuf(t *testing.T) {
	for _, tc := range []struct {
		name      string
		limit     int64
		writes    []string
		wantBody  string
		wantTotal int64
		wantTrunc bool
	}{
		{"under the limit", 10, []string{"abc", "def"}, "abcdef", 6, false},
		{"exactly the limit", 6, []string{"abc", "def"}, "abcdef", 6, false},
		{"over the limit in one write", 4, []string{"abcdef"}, "abcd", 6, true},
		{"over the limit across writes", 4, []string{"ab", "cd", "ef"}, "abcd", 6, true},
		{"limit 0 only counts", 0, []string{"abc", "def"}, "", 6, true},
		{"nothing written", 8, nil, "", 0, false},
		{"empty writes", 8, []string{"", ""}, "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCapBuf(tc.limit)
			for _, w := range tc.writes {
				c.write([]byte(w))
			}
			body, total, trunc := c.seal()
			if string(body) != tc.wantBody || total != tc.wantTotal || trunc != tc.wantTrunc {
				t.Errorf("seal = %q, %d, %v; want %q, %d, %v", body, total, trunc, tc.wantBody, tc.wantTotal, tc.wantTrunc)
			}
		})
	}
}

func TestCapBufSealCopiesAndIgnoresLaterWrites(t *testing.T) {
	c := newCapBuf(100)
	c.write([]byte("first"))
	body, _, _ := c.seal()
	// The buffer went back to the pool and may be reused at once.
	other := newCapBuf(100)
	other.write([]byte("XXXXX"))
	c.write([]byte("late"))
	if string(body) != "first" {
		t.Errorf("sealed body changed to %q: it must be an independent copy", body)
	}
	if b, total, _ := c.seal(); b != nil || total != 5 {
		t.Errorf("second seal = %q, %d", b, total)
	}
	other.discard()
	other.write([]byte("more")) // no panic, ignored
}

func TestCapBufGrowsBeyondPoolSize(t *testing.T) {
	c := newCapBuf(1 << 20)
	chunk := bytes.Repeat([]byte("z"), 10<<10)
	for i := 0; i < 30; i++ {
		c.write(chunk)
	}
	body, total, trunc := c.seal()
	if len(body) != 300<<10 || total != 300<<10 || trunc {
		t.Errorf("body %d total %d trunc %v", len(body), total, trunc)
	}
}

func TestCapBufConcurrentWritersAndSeal(t *testing.T) {
	for i := 0; i < 50; i++ {
		c := newCapBuf(1 << 10)
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 100; j++ {
					c.write([]byte("0123456789"))
				}
			}()
		}
		time.Sleep(time.Duration(i%5) * 100 * time.Microsecond)
		body, total, _ := c.seal() // races with the writers on purpose
		wg.Wait()
		if len(body) > 1<<10 || total > 4000 {
			t.Fatalf("body %d total %d", len(body), total)
		}
	}
}

func TestAppendRead(t *testing.T) {
	data := strings.Repeat("0123456789", 100) // 1000 bytes
	for _, tc := range []struct {
		name      string
		src       io.Reader
		limit     int64
		want      int
		leftAfter int
	}{
		{"shorter than limit", strings.NewReader(data), 5000, 1000, 0},
		{"exactly the limit", strings.NewReader(data), 1000, 1000, 0},
		{"longer than limit", strings.NewReader(data), 300, 300, 700},
		{"one byte at a time", iotest.OneByteReader(strings.NewReader(data)), 200, 200, 800},
		{"half reader", iotest.HalfReader(strings.NewReader(data)), 1000, 1000, 0},
		{"data and EOF together", iotest.DataErrReader(strings.NewReader(data)), 5000, 1000, 0},
		{"empty", strings.NewReader(""), 100, 0, 0},
		{"zero limit", strings.NewReader(data), 0, 0, 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := appendRead(nil, tc.src, tc.limit)
			if err != nil || len(got) != tc.want || string(got) != data[:tc.want] {
				t.Fatalf("got %d bytes, err %v; want %d", len(got), err, tc.want)
			}
			rest, _ := io.ReadAll(tc.src)
			if len(rest) != tc.leftAfter {
				t.Errorf("%d bytes left in the source, want %d", len(rest), tc.leftAfter)
			}
		})
	}
	boom := io.MultiReader(strings.NewReader("abc"), iotest.ErrReader(errTest))
	got, err := appendRead(nil, boom, 100)
	if string(got) != "abc" || !errors.Is(err, errTest) {
		t.Errorf("read error: %q, %v", got, err)
	}
}

func TestReadPrefixAndReadPooled(t *testing.T) {
	data := strings.Repeat("ab", 5000)
	got, err := readPrefix(strings.NewReader(data), 100, int64(len(data)))
	if err != nil || string(got) != data[:100] {
		t.Errorf("readPrefix: %d bytes, %v", len(got), err)
	}
	if got, _ := readPrefix(strings.NewReader(data), 0, 10); got != nil {
		t.Errorf("limit 0: %q", got)
	}
	if got, _ := readPrefix(strings.NewReader("xyz"), 100, 0); string(got) != "xyz" {
		t.Errorf("no size hint: %q", got)
	}
	h, err := readPooled(strings.NewReader(data), 6000)
	if err != nil || string(h.b) != data[:6000] {
		t.Errorf("readPooled: %d bytes, %v", len(h.b), err)
	}
	putBuf(h)
	h2, _ := readPooled(strings.NewReader("short"), 6000)
	if string(h2.b) != "short" {
		t.Errorf("a recycled buffer leaked old content: %q", h2.b)
	}
	putBuf(h2)
}

func TestPrefixBodyReplaysThenContinues(t *testing.T) {
	rest := &reqTee{src: io.NopCloser(strings.NewReader("-the rest"))}
	rest.buf = newCapBuf(100)
	pre := []byte("prefix")
	rest.seed(pre)
	body := &prefixBody{pre: pre, rest: rest}
	got, err := io.ReadAll(iotest.OneByteReader(body))
	if err != nil || string(got) != "prefix-the rest" {
		t.Fatalf("body = %q, %v", got, err)
	}
	captured, total, trunc := rest.buf.seal()
	if string(captured) != "prefix-the rest" || total != 15 || trunc || rest.n.Load() != 15 {
		t.Errorf("capture %q total %d trunc %v n %d: the prefix must be counted exactly once", captured, total, trunc, rest.n.Load())
	}
	if string(pre) != "prefix" {
		t.Error("replaying consumed the caller's slice")
	}
}

func TestRespTeeRecordsHowTheBodyEnded(t *testing.T) {
	read := func(r io.Reader) *respTee {
		tee := &respTee{src: io.NopCloser(r), buf: newCapBuf(100)}
		io.Copy(io.Discard, tee)
		return tee
	}
	if tee := read(strings.NewReader("complete")); !tee.eof || tee.err != nil || tee.endAt.IsZero() {
		t.Errorf("clean end: %+v", tee)
	}
	tee := read(io.MultiReader(strings.NewReader("part"), iotest.ErrReader(errTest)))
	if tee.eof || !errors.Is(tee.err, errTest) || tee.endAt.IsZero() {
		t.Errorf("failed body: %+v", tee)
	}
	if b, _, _ := tee.buf.seal(); string(b) != "part" {
		t.Errorf("captured %q", b)
	}
	tee = &respTee{src: io.NopCloser(strings.NewReader("never read")), buf: newCapBuf(10)}
	tee.Close()
	if tee.eof || tee.err != nil || !tee.endAt.IsZero() {
		t.Errorf("closed without reading: %+v", tee)
	}
}

func TestRespWriter(t *testing.T) {
	t.Run("status and bytes", func(t *testing.T) {
		stub := &stubWriter{}
		w := &respWriter{ResponseWriter: stub}
		w.WriteHeader(http.StatusEarlyHints) // informational: ignored
		if w.wrote || w.status != 0 {
			t.Errorf("1xx counted as the status: %+v", w)
		}
		w.WriteHeader(http.StatusCreated)
		w.WriteHeader(http.StatusTeapot) // superfluous
		io.WriteString(w, "hello")
		if w.status != 201 || w.bytes != 5 || !w.wrote || w.err != nil {
			t.Errorf("w = %+v", w)
		}
		if got := fmt.Sprint(stub.codes); got != "[103 201 418]" {
			t.Errorf("the underlying writer saw %s: every call must be forwarded", got)
		}
	})
	t.Run("write without WriteHeader is 200", func(t *testing.T) {
		w := &respWriter{ResponseWriter: httptest.NewRecorder()}
		io.WriteString(w, "x")
		if w.status != 200 {
			t.Errorf("status = %d", w.status)
		}
	})
	t.Run("101 is a final status", func(t *testing.T) {
		w := &respWriter{ResponseWriter: httptest.NewRecorder()}
		w.WriteHeader(101)
		if w.status != 101 || !w.wrote {
			t.Errorf("w = %+v", w)
		}
	})
	t.Run("flush counts as a 200", func(t *testing.T) {
		rec := httptest.NewRecorder()
		w := &respWriter{ResponseWriter: rec}
		w.Flush()
		if w.status != 200 || !rec.Flushed {
			t.Errorf("status %d flushed %v", w.status, rec.Flushed)
		}
	})
	t.Run("write error is remembered", func(t *testing.T) {
		w := &respWriter{ResponseWriter: &failingWriter{}}
		w.Write([]byte("abc"))
		w.Write([]byte("def"))
		if w.err == nil || w.bytes != 0 {
			t.Errorf("w = %+v", w)
		}
	})
	t.Run("unwrap reaches the real writer", func(t *testing.T) {
		rec := httptest.NewRecorder()
		w := &respWriter{ResponseWriter: rec}
		if http.NewResponseController(w).Flush() != nil || !rec.Flushed {
			t.Error("ResponseController cannot flush through the wrapper")
		}
		if w.Unwrap() != http.ResponseWriter(rec) {
			t.Error("Unwrap")
		}
	})
	t.Run("flush on a writer that cannot flush", func(t *testing.T) {
		w := &respWriter{ResponseWriter: &failingWriter{noFlush: true}}
		if err := w.FlushError(); err != nil {
			t.Errorf("FlushError = %v; an unsupported flush is not an error", err)
		}
	})
}

// stubWriter accepts everything and records the status codes it was given.
type stubWriter struct {
	hdr   http.Header
	codes []int
	body  bytes.Buffer
}

func (s *stubWriter) Header() http.Header {
	if s.hdr == nil {
		s.hdr = http.Header{}
	}
	return s.hdr
}
func (s *stubWriter) WriteHeader(code int)        { s.codes = append(s.codes, code) }
func (s *stubWriter) Write(p []byte) (int, error) { return s.body.Write(p) }

type failingWriter struct {
	hdr     http.Header
	noFlush bool
}

func (f *failingWriter) Header() http.Header {
	if f.hdr == nil {
		f.hdr = http.Header{}
	}
	return f.hdr
}
func (f *failingWriter) WriteHeader(int)           {}
func (f *failingWriter) Write([]byte) (int, error) { return 0, errTest }

func TestDecodeBody(t *testing.T) {
	plain := []byte(strings.Repeat("decode me please ", 100))
	gzBody := gz(t, plain)
	var zl, raw bytes.Buffer
	zw := zlib.NewWriter(&zl)
	zw.Write(plain)
	zw.Close()
	fw, _ := flate.NewWriter(&raw, flate.DefaultCompression)
	fw.Write(plain)
	fw.Close()
	hdr := func(enc string) http.Header { return http.Header{"Content-Encoding": {enc}} }

	for _, tc := range []struct {
		name      string
		h         http.Header
		body      []byte
		size      int64
		truncated bool
		limit     int64
		wantOK    bool
		wantLen   int
		wantTrunc bool
		wantEnc   string
	}{
		{"gzip", hdr("gzip"), gzBody, int64(len(gzBody)), false, 1 << 20, true, len(plain), false, "gzip"},
		{"gzip mixed case with spaces", hdr(" GZip "), gzBody, int64(len(gzBody)), false, 1 << 20, true, len(plain), false, "gzip"},
		{"x-gzip", hdr("x-gzip"), gzBody, int64(len(gzBody)), false, 1 << 20, true, len(plain), false, "gzip"},
		{"deflate (zlib)", hdr("deflate"), zl.Bytes(), int64(zl.Len()), false, 1 << 20, true, len(plain), false, "deflate"},
		{"deflate (raw)", hdr("deflate"), raw.Bytes(), int64(raw.Len()), false, 1 << 20, true, len(plain), false, "deflate"},
		{"no encoding", http.Header{}, plain, int64(len(plain)), false, 1 << 20, false, 0, false, ""},
		{"identity", hdr("identity"), plain, int64(len(plain)), false, 1 << 20, false, 0, false, ""},
		{"unknown coding", hdr("br"), gzBody, int64(len(gzBody)), false, 1 << 20, false, 0, false, ""},
		{"stacked codings", hdr("gzip, gzip"), gzBody, int64(len(gzBody)), false, 1 << 20, false, 0, false, ""},
		{"empty body", hdr("gzip"), nil, 0, false, 1 << 20, false, 0, false, ""},
		{"captured bytes were cut", hdr("gzip"), gzBody[:len(gzBody)/2], int64(len(gzBody)), true, 1 << 20, false, 0, false, ""},
		{"corrupt gzip", hdr("gzip"), []byte("not gzip"), 8, false, 1 << 20, false, 0, false, ""},
		{"corrupt deflate", hdr("deflate"), bytes.Repeat([]byte{0xff}, 20), 20, false, 1 << 20, false, 0, false, ""},
		{"decoded exactly at the limit", hdr("gzip"), gzBody, int64(len(gzBody)), false, int64(len(plain)), true, len(plain), false, "gzip"},
		{"decoded one over the limit", hdr("gzip"), gzBody, int64(len(gzBody)), false, int64(len(plain)) - 1, true, len(plain) - 1, true, "gzip"},
		{"decoded far over the limit", hdr("gzip"), gzBody, int64(len(gzBody)), false, 10, true, 10, true, "gzip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, size, trunc, enc, ok := decodeBody(tc.h, tc.body, tc.size, tc.truncated, tc.limit)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if len(out) != tc.wantLen || !bytes.Equal(out, plain[:tc.wantLen]) || trunc != tc.wantTrunc || enc != tc.wantEnc {
				t.Errorf("decoded %d bytes (match %v), truncated %v, enc %q", len(out), bytes.Equal(out, plain[:len(out)]), trunc, enc)
			}
			if trunc && size <= int64(len(out)) {
				t.Errorf("a cut body must report a size above what is kept: size %d, kept %d", size, len(out))
			}
			if !trunc && size != int64(len(out)) {
				t.Errorf("size %d, want %d", size, len(out))
			}
		})
	}
}

func TestDecodeExchangeUpdatesHeadersAndSkipsWhenBodiesAreOff(t *testing.T) {
	plain := []byte(strings.Repeat("q", 500))
	mk := func() *model.Exchange {
		return &model.Exchange{
			Request: model.Request{Header: http.Header{"Content-Encoding": {"gzip"}, "Content-Length": {"99"}, "X-Keep": {"1"}}, Body: gz(t, plain), BodySize: 99},
			Response: model.Response{Header: http.Header{"Content-Encoding": {"gzip"}, "Content-Length": {"77"}, "Content-Type": {"text/plain"}},
				Body: gz(t, plain), BodySize: 77},
		}
	}
	ex := mk()
	decodeExchange(ex, 1<<20)
	for name, h := range map[string]http.Header{"request": ex.Request.Header, "response": ex.Response.Header} {
		if h.Get("Content-Encoding") != "" || h.Get("Content-Length") != "" {
			t.Errorf("%s headers: %v", name, h)
		}
	}
	if ex.Request.Header.Get("X-Keep") != "1" || ex.Response.Header.Get("Content-Type") != "text/plain" {
		t.Error("unrelated headers were dropped")
	}
	if ex.Request.ContentEncoding != "gzip" || ex.Response.ContentEncoding != "gzip" ||
		!bytes.Equal(ex.Request.Body, plain) || !bytes.Equal(ex.Response.Body, plain) ||
		ex.Request.BodySize != 500 || ex.Response.BodySize != 500 {
		t.Errorf("exchange not decoded: %+v", ex.Summary())
	}
	untouched := mk()
	decodeExchange(untouched, 0)
	if untouched.Request.Header.Get("Content-Encoding") != "gzip" {
		t.Error("decoding ran with a zero body cap")
	}
}

func TestLearnable(t *testing.T) {
	mk := func(method string, status int, mut func(*model.Exchange)) *model.Exchange {
		ex := &model.Exchange{Request: model.Request{Method: method}, Response: model.Response{Status: status}}
		if mut != nil {
			mut(ex)
		}
		return ex
	}
	for _, tc := range []struct {
		name string
		ex   *model.Exchange
		want bool
	}{
		{"GET 200", mk("GET", 200, nil), true},
		{"HEAD 200", mk("HEAD", 200, nil), true},
		{"OPTIONS 204", mk("OPTIONS", 204, nil), true},
		{"GET 404", mk("GET", 404, nil), true},
		{"GET 429", mk("GET", 429, nil), false}, // throttling is transient, not a property of the URL
		{"GET 499", mk("GET", 499, nil), true},
		{"GET 500", mk("GET", 500, nil), false},
		{"GET 503", mk("GET", 503, nil), false},
		{"POST 200", mk("POST", 200, nil), false},
		{"PUT 200", mk("PUT", 200, nil), false},
		{"PATCH 200", mk("PATCH", 200, nil), false},
		{"DELETE 200", mk("DELETE", 200, nil), false},
		{"error set", mk("GET", 200, func(e *model.Exchange) { e.Error = "x" }), false},
		{"response truncated", mk("GET", 200, func(e *model.Exchange) { e.Response.BodyTruncated = true }), false},
		{"request truncated", mk("GET", 200, func(e *model.Exchange) { e.Request.BodyTruncated = true }), false},
	} {
		if got := learnable(tc.ex); got != tc.want {
			t.Errorf("%s: learnable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSameSocket(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{":9001", ":9001", true},
		{":9001", "0.0.0.0:9001", true},
		{"0.0.0.0:9001", "[::]:9001", true},
		{"127.0.0.1:9001", "127.0.0.1:9001", true},
		{"127.0.0.1:9001", ":9001", false},
		{"127.0.0.1:9001", "127.0.0.2:9001", false},
		{":9001", ":9002", false},
		{":0", ":0", false},
		{"127.0.0.1:0", "127.0.0.1:0", false},
		{"garbage", ":9001", false},
	} {
		if got := sameSocket(tc.a, tc.b); got != tc.want {
			t.Errorf("sameSocket(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSelfSignedCertNames(t *testing.T) {
	for _, tc := range []struct {
		host      string
		covers    []string
		notCovers []string
	}{
		{"", []string{"localhost", "127.0.0.1", "::1"}, []string{"example.com"}},
		{"0.0.0.0", []string{"localhost", "127.0.0.1", "::1"}, []string{"0.0.0.0"}},
		{"10.1.2.3", []string{"localhost", "127.0.0.1", "::1", "10.1.2.3"}, []string{"10.1.2.4"}},
		{"api.internal", []string{"localhost", "127.0.0.1", "api.internal"}, []string{"other.internal"}},
		{"localhost", []string{"localhost", "127.0.0.1"}, nil},
		{"::1", []string{"::1", "localhost"}, nil},
	} {
		t.Run(tc.host, func(t *testing.T) {
			cert, err := selfSignedCert(tc.host)
			if err != nil {
				t.Fatal(err)
			}
			key, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
			if !ok || key.Curve.Params().Name != "P-256" {
				t.Errorf("key = %T", cert.PrivateKey)
			}
			for _, n := range tc.covers {
				if err := cert.Leaf.VerifyHostname(n); err != nil {
					t.Errorf("does not cover %q: %v", n, err)
				}
			}
			for _, n := range tc.notCovers {
				if err := cert.Leaf.VerifyHostname(n); err == nil {
					t.Errorf("covers %q", n)
				}
			}
		})
	}
	a, _ := selfSignedCert("")
	b, _ := selfSignedCert("")
	if a.Leaf.SerialNumber.Cmp(b.Leaf.SerialNumber) == 0 {
		t.Error("two generated certificates share a serial number")
	}
}

func TestTLSConfigErrors(t *testing.T) {
	if _, err := tlsConfig(&model.TLSConfig{}, ":0"); err == nil {
		t.Error("empty TLS config accepted")
	}
	if _, err := tlsConfig(&model.TLSConfig{CertFile: "/nonexistent/c.pem", KeyFile: "/nonexistent/k.pem"}, ":0"); err == nil {
		t.Error("missing files accepted")
	}
	cfg, err := tlsConfig(&model.TLSConfig{SelfSigned: true}, "127.0.0.1:0")
	if err != nil || len(cfg.Certificates) != 1 || cfg.MinVersion < 0x0303 {
		t.Errorf("self-signed config: %+v, %v", cfg, err)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string { return "timed out" }
func (timeoutErr) Timeout() bool { return true }

func TestUpstreamFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, 502},
		{"plain error", errors.New("boom"), 502},
		{"canceled", context.Canceled, 502},
		{"deadline", context.DeadlineExceeded, 504},
		{"wrapped deadline", fmt.Errorf("round trip: %w", context.DeadlineExceeded), 504},
		{"net timeout", timeoutErr{}, 504},
		{"wrapped net timeout", fmt.Errorf("x: %w", timeoutErr{}), 504},
	} {
		status, body := upstreamFailure(tc.err)
		if status != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, status, tc.want)
		}
		var doc struct{ Error, Detail string }
		mustJSON(t, body, &doc)
		if doc.Error != "upstream request failed" || doc.Detail != tc.err.Error() {
			t.Errorf("%s: body %s", tc.name, body)
		}
	}
	h := upstreamFailureHeader(42)
	if h.Get("X-Mirage-Error") != "upstream" || h.Get("Content-Length") != "42" {
		t.Errorf("header = %v", h)
	}
}

func TestBufferPoolSizes(t *testing.T) {
	var p bufferPool
	b := p.Get()
	if len(b) != copyBufSize {
		t.Fatalf("len = %d", len(b))
	}
	p.Put(b)
	p.Put(make([]byte, 10)) // wrong size: ignored, no panic
	if b2 := p.Get(); len(b2) != copyBufSize {
		t.Errorf("len = %d after a foreign Put", len(b2))
	}
}

func TestDepsDefaults(t *testing.T) {
	d := Deps{}.withDefaults()
	if d.Logger == nil || d.CaptureQueueSize != 65536 || d.CaptureQueueBytes != 256<<20 ||
		d.CaptureBatchSize != 512 || d.CaptureFlushInterval != 250*time.Millisecond || d.CaptureWorkers < 1 {
		t.Errorf("defaults = %+v", d)
	}
	d = Deps{CaptureQueueSize: 7, CaptureBatchSize: 9, CaptureWorkers: 3}.withDefaults()
	if d.CaptureQueueSize != 7 || d.CaptureBatchSize != 9 || d.CaptureWorkers != 3 {
		t.Errorf("explicit values overwritten: %+v", d)
	}
}

func TestSameVirtualization(t *testing.T) {
	base := model.Service{RecordingID: "rec_1", Match: model.MatchConfig{Query: model.QueryExact, IgnoreParams: []string{"a"}}}.WithDefaults()
	same := base
	same.Match.IgnoreParams = []string{"a"}
	same.Upstream = "http://elsewhere" // not part of the virtual index
	same.Mode = model.ModeHybrid
	if !sameVirtualization(base, same) {
		t.Error("unrelated changes must keep the virtualizer")
	}
	for name, mut := range map[string]func(*model.Service){
		"recording":    func(s *model.Service) { s.RecordingID = "rec_2" },
		"match":        func(s *model.Service) { s.Match.IgnoreParams = []string{"b"} },
		"selection":    func(s *model.Service) { s.Match.Selection = model.SelectRandom },
		"latency":      func(s *model.Service) { s.Latency.Multiplier = 2 },
		"amplify":      func(s *model.Service) { s.ErrorAmplification = 3 },
		"floor":        func(s *model.Service) { s.ErrorFloor = 0.1 },
		"endpointpats": func(s *model.Service) { s.EndpointPatterns = []string{"/x/{id}"} },
	} {
		c := base
		mut(&c)
		if sameVirtualization(base, c) {
			t.Errorf("%s change must rebuild the virtualizer", name)
		}
	}
}
