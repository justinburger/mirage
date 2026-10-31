package proxy

import (
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// initialCaptureBuf is the starting capacity of a pooled capture buffer.
	initialCaptureBuf = 4 << 10
	// maxPooledBuf is the largest buffer returned to the pool; bigger ones
	// are left to the garbage collector so one huge body does not pin memory.
	maxPooledBuf = 64 << 10
)

// bufHolder wraps a byte slice so the pool stores a pointer (no allocation on
// Put).
type bufHolder struct{ b []byte }

var bufPool = sync.Pool{New: func() any {
	return &bufHolder{b: make([]byte, 0, initialCaptureBuf)}
}}

func getBuf() *bufHolder { return bufPool.Get().(*bufHolder) }

func putBuf(h *bufHolder) {
	if cap(h.b) > maxPooledBuf {
		return
	}
	h.b = h.b[:0]
	bufPool.Put(h)
}

// capBuf accumulates the first limit bytes of a body stream and counts all of
// it. It is safe for concurrent use: the transport reads a request body on its
// own goroutine, which may still be running when the handler finishes.
//
// seal ends the capture. Later writes are ignored, so a lagging reader can
// neither append to nor race with a buffer that went back to the pool.
type capBuf struct {
	mu     sync.Mutex
	h      *bufHolder
	total  int64
	limit  int64
	sealed bool
}

// newCapBuf returns a buffer that keeps at most limit bytes. A limit of 0
// keeps nothing and only counts.
func newCapBuf(limit int64) *capBuf { return &capBuf{limit: limit} }

// write records p.
func (c *capBuf) write(p []byte) {
	if len(p) == 0 {
		return
	}
	c.mu.Lock()
	if !c.sealed {
		c.total += int64(len(p))
		have := int64(0)
		if c.h != nil {
			have = int64(len(c.h.b))
		}
		if room := c.limit - have; room > 0 {
			if c.h == nil {
				c.h = getBuf()
			}
			if int64(len(p)) > room {
				p = p[:room]
			}
			c.h.b = append(c.h.b, p...)
		}
	}
	c.mu.Unlock()
}

// seal stops the capture and returns an exact-size heap copy of the captured
// bytes (nil when there are none), the number of bytes seen and whether the
// copy is shorter than that. The pooled buffer is released.
func (c *capBuf) seal() (body []byte, total int64, truncated bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sealed {
		return nil, c.total, false
	}
	c.sealed = true
	if c.h != nil {
		if len(c.h.b) > 0 {
			body = make([]byte, len(c.h.b))
			copy(body, c.h.b)
		}
		putBuf(c.h)
		c.h = nil
	}
	return body, c.total, c.total > int64(len(body))
}

// discard stops the capture and releases the buffer without copying.
func (c *capBuf) discard() {
	c.mu.Lock()
	c.sealed = true
	if c.h != nil {
		putBuf(c.h)
		c.h = nil
	}
	c.mu.Unlock()
}

// reqTee wraps the client's request body. It counts every byte and, when buf
// is set, copies the first bytes into it. The transport reads it, so the
// counter is atomic and buf locks.
type reqTee struct {
	src io.ReadCloser
	n   atomic.Int64
	eof atomic.Bool // the client's body has been read to its end
	buf *capBuf
}

// seed accounts for bytes that were read before the tee was created and are
// replayed to the upstream by a prefixBody in front of it.
func (t *reqTee) seed(pre []byte) {
	t.n.Add(int64(len(pre)))
	if t.buf != nil {
		t.buf.write(pre)
	}
}

func (t *reqTee) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		t.n.Add(int64(n))
		if t.buf != nil {
			t.buf.write(p[:n])
		}
	}
	if err == io.EOF {
		t.eof.Store(true)
	}
	return n, err
}

func (t *reqTee) Close() error { return t.src.Close() }

// prefixBody replays bytes the handler already consumed (hybrid matching)
// before the rest of the body. The prefix is a private heap copy, so the
// transport goroutine may keep reading it after the handler returned.
type prefixBody struct {
	pre  []byte
	rest io.ReadCloser
}

func (b *prefixBody) Read(p []byte) (int, error) {
	if len(b.pre) > 0 {
		n := copy(p, b.pre)
		b.pre = b.pre[n:]
		return n, nil
	}
	return b.rest.Read(p)
}

func (b *prefixBody) Close() error { return b.rest.Close() }

// respTee wraps the upstream response body. ReverseProxy reads it on the
// handler goroutine, so it needs no locking of its own. It records how the
// body ended, which decides whether the exchange is complete, interrupted by
// the upstream or abandoned by the client.
type respTee struct {
	src   io.ReadCloser
	buf   *capBuf
	eof   bool      // the upstream body ended cleanly
	err   error     // the first non-EOF read error
	endAt time.Time // when EOF or the error was seen
}

func (t *respTee) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		t.buf.write(p[:n])
	}
	if err != nil && t.endAt.IsZero() {
		t.endAt = time.Now()
		if err == io.EOF {
			t.eof = true
		} else {
			t.err = err
		}
	}
	return n, err
}

func (t *respTee) Close() error { return t.src.Close() }

// appendRead reads from src into dst until dst holds limit bytes, src ends or
// a read fails. io.EOF is not an error: a body shorter than limit simply ends.
// Bytes beyond limit stay unread in src.
func appendRead(dst []byte, src io.Reader, limit int64) ([]byte, error) {
	for int64(len(dst)) < limit {
		if len(dst) == cap(dst) {
			dst = slices.Grow(dst, max(512, len(dst)))
		}
		room := min(int64(cap(dst)), limit) - int64(len(dst))
		n, err := src.Read(dst[len(dst) : len(dst)+int(room)])
		dst = dst[:len(dst)+n]
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return dst, err
		}
	}
	return dst, nil
}

// readPrefix reads up to limit bytes of src into a fresh heap slice (nil when
// limit is not positive). sizeHint, usually the Content-Length, sizes the first
// allocation.
func readPrefix(src io.Reader, limit, sizeHint int64) ([]byte, error) {
	if limit <= 0 {
		return nil, nil
	}
	first := int64(512)
	if sizeHint > 0 {
		first = min(sizeHint, limit)
	}
	return appendRead(make([]byte, 0, first), src, limit)
}

// readPooled is readPrefix into a pooled buffer, for callers that are done
// with the bytes before they return (virtualize mode). The caller hands the
// holder to putBuf afterwards.
func readPooled(src io.Reader, limit int64) (*bufHolder, error) {
	h := getBuf()
	var err error
	h.b, err = appendRead(h.b[:0], src, limit)
	return h, err
}
