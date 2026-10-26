package virtual

import (
	"bytes"
	"compress/gzip"
	"container/list"
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// loadTimeout bounds one body load from the store. Loads ignore the cancellation
// of the request that triggered them (other requests may be waiting on the
// same load), so they need a limit of their own.
const loadTimeout = 30 * time.Second

// bodyEntry is the response body of one exchange plus its gzip-encoded form,
// which is built at most once.
type bodyEntry struct {
	raw []byte
	id  int64      // lazy entries: the exchange ID
	lru *bodyCache // lazy entries: the cache that accounts for gz

	once sync.Once
	gz   []byte
}

// gzipped returns the gzip-compressed body, compressing it on first use.
func (b *bodyEntry) gzipped(e *Engine) []byte {
	b.once.Do(func() {
		b.gz = e.compress(b.raw)
		if b.lru != nil {
			b.lru.grow(b, int64(len(b.gz)))
		}
	})
	return b.gz
}

// compress gzips raw with a pooled writer. The output carries no timestamp, so
// it is deterministic.
func (e *Engine) compress(raw []byte) []byte {
	e.gzips.Add(1)
	var buf bytes.Buffer
	buf.Grow(len(raw)/2 + 64)
	zw, _ := e.gzPool.Get().(*gzip.Writer)
	if zw == nil {
		zw = gzip.NewWriter(&buf)
	} else {
		zw.Reset(&buf)
	}
	_, _ = zw.Write(raw)
	_ = zw.Close()
	zw.Reset(io.Discard)
	e.gzPool.Put(zw)
	return buf.Bytes()
}

// bodyCache is an LRU of lazily loaded response bodies bounded by bytes. A
// load that several requests wait for runs once.
type bodyCache struct {
	max int64

	mu     sync.Mutex
	size   int64
	ll     *list.List // *cacheItem, front = most recently used
	m      map[int64]*list.Element
	flight map[int64]*loadCall
	closed bool
	loads  int64 // completed loads, for tests
}

type cacheItem struct {
	id   int64
	b    *bodyEntry
	size int64
}

type loadCall struct {
	done chan struct{}
	b    *bodyEntry
	err  error
}

func newBodyCache(max int64) *bodyCache {
	return &bodyCache{
		max:    max,
		ll:     list.New(),
		m:      make(map[int64]*list.Element),
		flight: make(map[int64]*loadCall),
	}
}

// get returns the cached body of id, calling load (in the caller's goroutine)
// when it is missing. Concurrent callers for one id share a single load; a
// waiter gives up when its ctx ends.
func (c *bodyCache) get(ctx context.Context, id int64, load func() (*bodyEntry, error)) (*bodyEntry, error) {
	c.mu.Lock()
	if el, ok := c.m[id]; ok {
		c.ll.MoveToFront(el)
		b := el.Value.(*cacheItem).b
		c.mu.Unlock()
		return b, nil
	}
	if call, ok := c.flight[id]; ok {
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.b, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &loadCall{done: make(chan struct{})}
	c.flight[id] = call
	c.mu.Unlock()

	finished := false
	defer func() {
		c.mu.Lock()
		delete(c.flight, id)
		if finished && call.err == nil && !c.closed {
			c.loads++
			c.insertLocked(id, call.b)
		}
		c.mu.Unlock()
		if !finished {
			call.err = fmt.Errorf("virtual: loading exchange %d panicked", id)
		}
		close(call.done)
	}()
	call.b, call.err = load()
	finished = true
	return call.b, call.err
}

// insertLocked adds b and evicts from the cold end until the cache fits. A
// body larger than the whole cache is returned to its caller but not kept.
func (c *bodyCache) insertLocked(id int64, b *bodyEntry) {
	size := int64(len(b.raw))
	c.m[id] = c.ll.PushFront(&cacheItem{id: id, b: b, size: size})
	c.size += size
	c.evictLocked()
}

func (c *bodyCache) evictLocked() {
	for c.size > c.max {
		el := c.ll.Back()
		if el == nil {
			return
		}
		it := c.ll.Remove(el).(*cacheItem)
		delete(c.m, it.id)
		c.size -= it.size
	}
}

// grow charges n more bytes (a gzip form built after the body was cached) to b
// if it is still cached.
func (c *bodyCache) grow(b *bodyEntry, n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[b.id]; ok {
		if it := el.Value.(*cacheItem); it.b == b {
			it.size += n
			c.size += n
			c.evictLocked()
		}
	}
}

// close empties the cache; later loads are served but not kept.
func (c *bodyCache) close() {
	c.mu.Lock()
	c.closed = true
	c.ll.Init()
	clear(c.m)
	c.size = 0
	c.mu.Unlock()
}

// stats returns the cached bytes and entries (tests).
func (c *bodyCache) stats() (size int64, entries int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size, c.ll.Len()
}

// bodyFor returns the response body of it: resident, or loaded from the store
// through the LRU.
func (e *Engine) bodyFor(ctx context.Context, it *item) (*bodyEntry, error) {
	if it.body != nil {
		return it.body, nil
	}
	if e.cache == nil || e.st == nil {
		return nil, fmt.Errorf("virtual: exchange %d body is not resident and the engine has no store", it.id)
	}
	return e.cache.get(ctx, it.id, func() (*bodyEntry, error) {
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loadTimeout)
		defer cancel()
		full, err := e.st.GetExchange(lctx, it.id)
		if err != nil {
			return nil, fmt.Errorf("virtual: load exchange %d: %w", it.id, err)
		}
		return &bodyEntry{raw: full.Response.Body, id: it.id, lru: e.cache}, nil
	})
}
