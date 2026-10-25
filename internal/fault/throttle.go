package fault

import (
	"context"
	"errors"
	"net/http"
	"time"
)

const (
	// refillsPerSecond is how often the throttle's token bucket is topped up:
	// the bucket holds at most refillInterval worth of bytes and a Write is
	// split into chunks of that size.
	refillsPerSecond = 10
	refillInterval   = time.Second / refillsPerSecond

	// maxChunk bounds a single underlying Write (and flush) at high rates so
	// that cancellation is noticed and data keeps flowing in reasonable pieces.
	maxChunk = 256 << 10

	// minSleep is the shortest pacing delay worth a timer. Smaller debts are
	// carried forward and repaid by the next sleep, so the average rate holds.
	minSleep = 200 * time.Microsecond
)

// throttleWriter paces writes to a response to a fixed number of bytes per
// second using a token bucket that refills ten times a second.
//
// The bucket is kept as a virtual clock: clock is the instant by which the
// bytes written so far have been "paid for" at the configured rate. Writing n
// bytes advances clock by n/rate, and the writer sleeps until real time catches
// up. Idle time earns credit, but never more than refillInterval of it, which
// is the bucket size. Because the clock is virtual, sleep jitter does not
// accumulate: an overlong sleep is simply credited to the next chunk.
//
// Each chunk is written and then flushed, so the client sees data arrive
// steadily rather than in one lump when the handler returns.
type throttleWriter struct {
	rw  http.ResponseWriter
	rc  *http.ResponseController
	ctx context.Context

	nsPerByte float64
	chunk     int
	clock     time.Time
	noFlush   bool // the underlying writer cannot flush
}

func newThrottleWriter(ctx context.Context, w http.ResponseWriter, bytesPerSecond int64) *throttleWriter {
	if ctx == nil {
		ctx = context.Background()
	}
	chunk := bytesPerSecond / refillsPerSecond
	if chunk < 1 {
		chunk = 1
	}
	if chunk > maxChunk {
		chunk = maxChunk
	}
	return &throttleWriter{
		rw:        w,
		rc:        http.NewResponseController(w),
		ctx:       ctx,
		nsPerByte: float64(time.Second) / float64(bytesPerSecond),
		chunk:     int(chunk),
		clock:     time.Now(),
	}
}

// Header implements http.ResponseWriter.
func (t *throttleWriter) Header() http.Header { return t.rw.Header() }

// WriteHeader implements http.ResponseWriter. Headers are not throttled.
func (t *throttleWriter) WriteHeader(status int) { t.rw.WriteHeader(status) }

// Write implements http.ResponseWriter. It splits p into chunks of at most
// one refill, waits for each chunk's share of the bandwidth, then writes and
// flushes it. It returns early with the context's error once the request
// context is done, and with the underlying error if a write or flush fails.
func (t *throttleWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return t.rw.Write(p)
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), t.chunk)
		if err := t.pace(n); err != nil {
			return written, err
		}
		m, err := t.rw.Write(p[:n])
		written += m
		if err != nil {
			return written, err
		}
		if !t.noFlush {
			if err := t.rc.Flush(); errors.Is(err, http.ErrNotSupported) {
				t.noFlush = true // don't pay for the error on every chunk
			} else if err != nil {
				return written, err
			}
		}
		p = p[n:]
	}
	return written, nil
}

// pace charges n bytes against the bucket and sleeps until they are paid for.
func (t *throttleWriter) pace(n int) error {
	if err := t.ctx.Err(); err != nil {
		return err
	}
	now := time.Now()
	if floor := now.Add(-refillInterval); t.clock.Before(floor) {
		t.clock = floor // idle credit is capped at one bucket
	}
	t.clock = t.clock.Add(time.Duration(float64(n) * t.nsPerByte))
	if wait := t.clock.Sub(now); wait >= minSleep {
		return sleepCtx(t.ctx, wait)
	}
	return nil
}

// Flush implements http.Flusher.
func (t *throttleWriter) Flush() { _ = t.rc.Flush() }

// FlushError flushes buffered data, reporting http.ErrNotSupported (wrapped)
// when the underlying writer cannot flush. It is what
// http.ResponseController.Flush prefers over Flush.
func (t *throttleWriter) FlushError() error { return t.rc.Flush() }

// Unwrap returns the underlying writer so http.ResponseController can reach
// its other capabilities (Hijack, deadlines, EnableFullDuplex).
func (t *throttleWriter) Unwrap() http.ResponseWriter { return t.rw }
