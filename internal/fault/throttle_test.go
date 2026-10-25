package fault

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// chunkRecorder is a ResponseWriter that records each underlying Write and
// Flush, optionally failing.
type chunkRecorder struct {
	mu       sync.Mutex
	header   http.Header
	status   int
	chunks   []int
	flushes  int
	total    int
	failAt   int // fail the Nth Write (1-based); 0 = never
	flushErr error
}

func newChunkRecorder() *chunkRecorder { return &chunkRecorder{header: http.Header{}} }

func (c *chunkRecorder) Header() http.Header { return c.header }
func (c *chunkRecorder) WriteHeader(s int)   { c.status = s }
func (c *chunkRecorder) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failAt > 0 && len(c.chunks)+1 == c.failAt {
		return 0, errors.New("boom")
	}
	c.chunks = append(c.chunks, len(p))
	c.total += len(p)
	return len(p), nil
}
func (c *chunkRecorder) Flush() { c.mu.Lock(); c.flushes++; c.mu.Unlock() }
func (c *chunkRecorder) FlushError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushes++
	return c.flushErr
}

func TestWrapWriterNoThrottleReturnsOriginal(t *testing.T) {
	w := httptest.NewRecorder()
	for _, p := range []*Plan{{}, {BytesPerSecond: 0}, {BytesPerSecond: -5}, {Delay: time.Second}} {
		if got := p.WrapWriter(w); got != http.ResponseWriter(w) {
			t.Errorf("WrapWriter(%+v) wrapped the writer", p)
		}
	}
	if _, ok := (&Plan{BytesPerSecond: 100}).WrapWriter(w).(*throttleWriter); !ok {
		t.Error("WrapWriter with a throttle did not return a throttleWriter")
	}
}

func TestThrottleWriterSplitsAndFlushes(t *testing.T) {
	t.Parallel()
	// 200 KB/s -> 20 KB per 100 ms refill.
	rec := newChunkRecorder()
	w := (&Plan{BytesPerSecond: 200_000}).WrapWriter(rec)
	data := bytes.Repeat([]byte("x"), 70_000)

	start := time.Now()
	n, err := w.Write(data)
	el := time.Since(start)

	if err != nil || n != len(data) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if rec.total != len(data) {
		t.Errorf("underlying got %d bytes, want %d", rec.total, len(data))
	}
	want := []int{20_000, 20_000, 20_000, 10_000}
	if len(rec.chunks) != len(want) {
		t.Fatalf("chunks = %v, want %v", rec.chunks, want)
	}
	for i := range want {
		if rec.chunks[i] != want[i] {
			t.Fatalf("chunks = %v, want %v", rec.chunks, want)
		}
	}
	if rec.flushes != len(want) {
		t.Errorf("flushes = %d, want one per chunk (%d)", rec.flushes, len(want))
	}
	// 70 KB at 200 KB/s = 350 ms.
	if el < 300*time.Millisecond || el > 600*time.Millisecond {
		t.Errorf("took %v, want about 350ms", el)
	}
}

func TestThrottleWriterRate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		bps  int64
		size int
	}{
		{"100 KB/s 20 KB", 100_000, 20_000},
		{"1 MB/s 300 KB", 1_000_000, 300_000},
		{"5 KB/s 1.5 KB (sub-chunk writes)", 5_000, 1_500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := newChunkRecorder()
			w := (&Plan{BytesPerSecond: tt.bps}).WrapWriter(rec)
			data := make([]byte, tt.size)
			start := time.Now()
			// Write in uneven pieces, as a real handler would.
			for off := 0; off < len(data); {
				end := min(off+777, len(data))
				if _, err := w.Write(data[off:end]); err != nil {
					t.Fatal(err)
				}
				off = end
			}
			el := time.Since(start)
			ideal := time.Duration(float64(tt.size) / float64(tt.bps) * float64(time.Second))
			if el < ideal*8/10 || el > ideal*12/10+30*time.Millisecond {
				t.Errorf("took %v, ideal %v (tolerance 20%%)", el, ideal)
			}
		})
	}
}

func TestThrottleWriterTinyRate(t *testing.T) {
	t.Parallel()
	// 20 B/s -> 2 bytes per refill; a 6-byte write is three chunks over ~300 ms.
	rec := newChunkRecorder()
	w := (&Plan{BytesPerSecond: 20}).WrapWriter(rec)
	start := time.Now()
	if _, err := w.Write([]byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	el := time.Since(start)
	if len(rec.chunks) != 3 || el < 250*time.Millisecond || el > 600*time.Millisecond {
		t.Errorf("chunks = %v in %v, want 3 chunks in about 300ms", rec.chunks, el)
	}

	// Below 10 B/s the chunk is a single byte.
	rec = newChunkRecorder()
	w = (&Plan{BytesPerSecond: 5}).WrapWriter(rec)
	if _, err := w.Write([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	if len(rec.chunks) != 2 || rec.chunks[0] != 1 {
		t.Errorf("chunks = %v, want [1 1]", rec.chunks)
	}
}

func TestThrottleWriterIdleCreditIsCapped(t *testing.T) {
	t.Parallel()
	// After a long idle period the bucket holds one refill (100 ms of data),
	// not the whole idle time: 40 KB at 100 KB/s must still take ~300 ms
	// (the first 10 KB goes out immediately).
	rec := newChunkRecorder()
	w := (&Plan{BytesPerSecond: 100_000}).WrapWriter(rec)
	time.Sleep(400 * time.Millisecond)
	start := time.Now()
	if _, err := w.Write(make([]byte, 40_000)); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 250*time.Millisecond || el > 500*time.Millisecond {
		t.Errorf("took %v, want about 300ms", el)
	}
}

func TestThrottleWriterEmptyWrite(t *testing.T) {
	rec := newChunkRecorder()
	w := (&Plan{BytesPerSecond: 1}).WrapWriter(rec)
	start := time.Now()
	n, err := w.Write(nil)
	if n != 0 || err != nil || time.Since(start) > 100*time.Millisecond {
		t.Errorf("empty Write = %d, %v after %v", n, err, time.Since(start))
	}
	if len(rec.chunks) != 1 || rec.chunks[0] != 0 {
		t.Errorf("empty write was not passed through: %v", rec.chunks)
	}
}

func TestThrottleWriterPassesThroughHeadersAndStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	w := (&Plan{BytesPerSecond: 1 << 20}).WrapWriter(rec)
	w.Header().Set("X-Test", "1")
	w.WriteHeader(201)
	_, _ = io.WriteString(w, "hello")
	if rec.Code != 201 || rec.Header().Get("X-Test") != "1" || rec.Body.String() != "hello" {
		t.Errorf("recorder = %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
}

func TestThrottleWriterCancellation(t *testing.T) {
	t.Parallel()
	t.Run("during pacing", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rec := newChunkRecorder()
		// 1 KB/s: 100-byte chunks, one per 100 ms; 100 KB would take 100 s.
		w := (&Plan{BytesPerSecond: 1000}).WrapWriterContext(ctx, rec)
		time.AfterFunc(250*time.Millisecond, cancel)

		start := time.Now()
		n, err := w.Write(make([]byte, 100_000))
		el := time.Since(start)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if el > 3*time.Second {
			t.Errorf("Write kept going for %v after cancellation", el)
		}
		if n != rec.total || n >= 1000 {
			t.Errorf("n = %d, underlying total = %d; want a handful of chunks only", n, rec.total)
		}
		// A write after cancellation fails immediately without touching the client.
		before := rec.total
		if n, err := w.Write([]byte("more")); n != 0 || !errors.Is(err, context.Canceled) {
			t.Errorf("Write after cancel = %d, %v", n, err)
		}
		if rec.total != before {
			t.Error("data was written after cancellation")
		}
	})
	t.Run("already cancelled", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rec := newChunkRecorder()
		w := (&Plan{BytesPerSecond: 1_000_000}).WrapWriterContext(ctx, rec)
		if n, err := w.Write([]byte("data")); n != 0 || !errors.Is(err, context.Canceled) {
			t.Errorf("Write = %d, %v", n, err)
		}
		if rec.total != 0 {
			t.Error("wrote to a cancelled request")
		}
	})
	t.Run("nil context is background", func(t *testing.T) {
		rec := newChunkRecorder()
		//lint:ignore SA1012 deliberately passing a nil context to prove it is tolerated
		w := (&Plan{BytesPerSecond: 1_000_000}).WrapWriterContext(nil, rec)
		if n, err := w.Write([]byte("data")); n != 4 || err != nil {
			t.Errorf("Write = %d, %v", n, err)
		}
	})
}

func TestThrottleWriterUnderlyingErrors(t *testing.T) {
	t.Parallel()
	t.Run("write error", func(t *testing.T) {
		rec := newChunkRecorder()
		rec.failAt = 2
		w := (&Plan{BytesPerSecond: 1_000_000}).WrapWriter(rec) // 100 KB chunks
		n, err := w.Write(make([]byte, 250_000))
		if err == nil || err.Error() != "boom" {
			t.Fatalf("err = %v, want the underlying error", err)
		}
		if n != 100_000 {
			t.Errorf("n = %d, want the 100000 bytes written before the failure", n)
		}
	})
	t.Run("flush error", func(t *testing.T) {
		rec := newChunkRecorder()
		rec.flushErr = errors.New("flush failed")
		w := (&Plan{BytesPerSecond: 1_000_000}).WrapWriter(rec)
		if _, err := w.Write(make([]byte, 10)); err == nil || err.Error() != "flush failed" {
			t.Errorf("err = %v, want the flush error", err)
		}
	})
	t.Run("writer that cannot flush", func(t *testing.T) {
		// http.ResponseWriter without Flush/FlushError/Unwrap: ErrNotSupported is not fatal.
		w := (&Plan{BytesPerSecond: 1_000_000}).WrapWriter(plainWriter{header: http.Header{}})
		if n, err := w.Write([]byte("abc")); n != 3 || err != nil {
			t.Errorf("Write = %d, %v", n, err)
		}
	})
}

type plainWriter struct{ header http.Header }

func (p plainWriter) Header() http.Header         { return p.header }
func (p plainWriter) WriteHeader(int)             {}
func (p plainWriter) Write(b []byte) (int, error) { return len(b), nil }

func TestThrottleWriterFlusherAndUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	w := (&Plan{BytesPerSecond: 1 << 20}).WrapWriter(rec)

	// Direct http.Flusher assertion, as handlers use it.
	f, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("throttled writer does not implement http.Flusher")
	}
	f.Flush()
	if !rec.Flushed {
		t.Error("Flush did not reach the underlying writer")
	}

	// Unwrap exposes the underlying writer.
	u, ok := w.(interface{ Unwrap() http.ResponseWriter })
	if !ok || u.Unwrap() != http.ResponseWriter(rec) {
		t.Error("Unwrap does not return the underlying writer")
	}

	// http.ResponseController finds Flush on the wrapper...
	rec2 := httptest.NewRecorder()
	w2 := (&Plan{BytesPerSecond: 1 << 20}).WrapWriter(rec2)
	if err := http.NewResponseController(w2).Flush(); err != nil || !rec2.Flushed {
		t.Errorf("ResponseController.Flush = %v (flushed %v)", err, rec2.Flushed)
	}

	// ...and reports ErrNotSupported when the underlying writer cannot flush.
	w3 := (&Plan{BytesPerSecond: 1 << 20}).WrapWriter(plainWriter{header: http.Header{}})
	if err := http.NewResponseController(w3).Flush(); !errors.Is(err, http.ErrNotSupported) {
		t.Errorf("Flush on unflushable writer = %v, want ErrNotSupported", err)
	}
}

// deadlineWriter records SetWriteDeadline calls to prove that
// http.ResponseController reaches the underlying writer through Unwrap.
type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error { d.deadline = t; return nil }

func TestThrottleWriterResponseControllerUnwraps(t *testing.T) {
	dw := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	w := (&Plan{BytesPerSecond: 1 << 20}).WrapWriter(dw)
	want := time.Now().Add(time.Hour)
	if err := http.NewResponseController(w).SetWriteDeadline(want); err != nil {
		t.Fatal(err)
	}
	if !dw.deadline.Equal(want) {
		t.Errorf("deadline = %v, want %v: ResponseController did not unwrap", dw.deadline, want)
	}
}

// TestThrottleRateOverHTTP is the end-to-end rate check: a 200 KB body at
// 100 KB/s must take 2 s, +-20%, as seen by a real client.
func TestThrottleRateOverHTTP(t *testing.T) {
	t.Parallel()
	const (
		size = 200_000
		bps  = 100_000
	)
	inj := newInjector(t, throttleRule("t", bps))
	body := bytes.Repeat([]byte("0123456789abcdef"), size/16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plan := inj.Decide("svc", r)
		if plan.Apply(w, r) {
			return
		}
		w = plan.WrapWriterContext(r.Context(), w)
		w.Header().Set("Content-Type", "application/octet-stream")
		// A typical handler: one big Write.
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	start := time.Now()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	el := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body corrupted: got %d bytes, want %d", len(got), len(body))
	}
	ideal := time.Duration(float64(size) / bps * float64(time.Second))
	if el < ideal*8/10 || el > ideal*12/10 {
		t.Errorf("200 KB at 100 KB/s took %v, want %v +-20%%", el, ideal)
	}
	t.Logf("200 KB at 100 KB/s took %v (ideal %v)", el.Round(time.Millisecond), ideal)
}

// TestThrottleIOCopyAndStreaming checks that a streaming handler (io.Copy in
// small pieces) is paced and that the first bytes reach the client long before
// the last, i.e. the writer flushes as it goes.
func TestThrottleIOCopyAndStreaming(t *testing.T) {
	t.Parallel()
	const bps = 20_000
	body := bytes.Repeat([]byte("z"), 10_000) // 500 ms at 20 KB/s
	plan := &Plan{BytesPerSecond: bps}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w = plan.WrapWriterContext(r.Context(), w)
		_, _ = io.CopyBuffer(w, io.LimitReader(bytes.NewReader(body), int64(len(body))), make([]byte, 512))
	}))
	defer srv.Close()

	start := time.Now()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var first time.Duration
	buf := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	first = time.Since(start)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	total := time.Since(start)
	if len(rest)+1 != len(body) {
		t.Fatalf("got %d bytes, want %d", len(rest)+1, len(body))
	}
	if total < 400*time.Millisecond || total > 800*time.Millisecond {
		t.Errorf("total %v, want about 500ms", total)
	}
	if first > total/2 {
		t.Errorf("first byte after %v of %v total: output is not flushed as it goes", first, total)
	}
}

// TestThrottleStopsWhenClientGoesAway verifies that a throttled transfer ends
// promptly, with a write error, when the client hangs up.
func TestThrottleStopsWhenClientGoesAway(t *testing.T) {
	t.Parallel()
	done := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w = (&Plan{BytesPerSecond: 10_000}).WrapWriterContext(r.Context(), w)
		_, err := w.Write(make([]byte, 10_000_000)) // 1000 s of data
		done <- err
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	cancel() // hang up mid-transfer
	resp.Body.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Error("handler Write succeeded for a vanished client")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("throttled handler kept writing after the client disconnected")
	}
}

func TestThrottleManyConcurrentWriters(t *testing.T) {
	t.Parallel()
	// Each writer has its own bucket: eight 20 KB/s transfers of 4 KB should
	// all finish in ~200 ms, in parallel (not 8x that).
	var wg sync.WaitGroup
	start := time.Now()
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := newChunkRecorder()
			w := (&Plan{BytesPerSecond: 20_000}).WrapWriter(rec)
			if _, err := w.Write(make([]byte, 4000)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if el := time.Since(start); el > 600*time.Millisecond {
		t.Errorf("8 parallel transfers took %v, want about 200ms", el)
	}
}
