package virtual

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/store"
)

func openStore(t testing.TB) *store.SQLite {
	t.Helper()
	st, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// fill creates a recording holding exs (in Seq order) and returns it.
func fill(t testing.TB, st store.Store, exs ...*model.Exchange) model.Recording {
	t.Helper()
	ctx := context.Background()
	rec, err := st.CreateRecording(ctx, model.Recording{Name: "test", Source: "proxy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendExchanges(ctx, rec.ID, exs); err != nil {
		t.Fatal(err)
	}
	rec, err = st.GetRecording(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// bigBody returns a distinctive body of n bytes.
func bigBody(i, n int) string {
	return strings.Repeat(fmt.Sprintf("<%04d>", i), n/6+1)[:n]
}

// countingStore counts GetExchange calls (and can delay them).
type countingStore struct {
	store.Store
	gets atomic.Int64
	gate chan struct{} // when set, GetExchange blocks until it is closed
}

func (c *countingStore) GetExchange(ctx context.Context, id int64) (*model.Exchange, error) {
	c.gets.Add(1)
	if c.gate != nil {
		<-c.gate
	}
	return c.Store.GetExchange(ctx, id)
}

// waitFor polls cond for up to 5 seconds.
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func build(t testing.TB, st store.Store, rec model.Recording, o Options) *Engine {
	t.Helper()
	o.RecordingID = rec.ID
	o = noLatency(o)
	e, err := Build(context.Background(), st, o)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func TestBuildResident(t *testing.T) {
	st := openStore(t)
	rec := fill(t, st,
		mkEx(1, "GET", "/a", 200, "A"),
		mkEx(2, "GET", "/b?x=1", 200, "B"),
		mkEx(3, "POST", "/c", 201, "C", withReqBody(`{"k":1}`)),
		mkEx(4, "GET", "/d", 504, "", withError("timeout")),
	)
	cs := &countingStore{Store: st}
	e := build(t, cs, rec, Options{Match: model.MatchConfig{PathTemplates: true}})
	if e.Lazy() {
		t.Fatal("a tiny recording must stay resident")
	}
	if e.Len() != 4 || e.Stats().Indexed != 4 {
		t.Fatalf("Len=%d Indexed=%d, want 4", e.Len(), e.Stats().Indexed)
	}
	if l, b := lookup(e, "GET", "/a", ""); l != LevelExact || b != "A" {
		t.Fatalf("(%s,%q)", l, b)
	}
	if l, b := lookup(e, "GET", "/b?x=1", ""); l != LevelExact || b != "B" {
		t.Fatalf("(%s,%q)", l, b)
	}
	if l, b := lookup(e, "POST", "/c", `{ "k": 1 }`); l != LevelExact || b != "C" {
		t.Fatalf("request bodies must be loaded for json matching: (%s,%q)", l, b)
	}
	if l, _ := lookup(e, "GET", "/d", ""); l != LevelMiss {
		t.Fatalf("failed exchange served: %s", l)
	}
	serve(e, mkReq("GET", "/a"), "")
	if cs.gets.Load() != 0 {
		t.Fatalf("resident engine hit the store %d times while serving", cs.gets.Load())
	}
}

func TestBuildErrors(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := Build(ctx, st, Options{RecordingID: "rec_missing"}); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing recording: %v", err)
	}
	if _, err := Build(ctx, nil, Options{RecordingID: "rec_x"}); !errors.Is(err, model.ErrValidation) {
		t.Fatalf("nil store: %v", err)
	}
	if _, err := Build(ctx, st, Options{}); !errors.Is(err, model.ErrValidation) {
		t.Fatalf("no recording id: %v", err)
	}
	rec := fill(t, st, mkEx(1, "GET", "/a", 200, "A"))
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if e, err := Build(cctx, st, Options{RecordingID: rec.ID}); err == nil || e != nil {
		t.Fatalf("cancelled context: engine=%v err=%v", e, err)
	}
	// An empty recording builds an empty engine.
	empty, err := st.CreateRecording(ctx, model.Recording{Name: "empty", Source: "proxy"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := Build(ctx, st, Options{RecordingID: empty.ID})
	if err != nil || e.Len() != 0 {
		t.Fatalf("empty recording: len=%d err=%v", e.Len(), err)
	}
}

func TestBuildOrdersCandidatesByStartedAt(t *testing.T) {
	st := openStore(t)
	// Persisted (Seq) order is the reverse of arrival order.
	rec := fill(t, st,
		mkEx(1, "GET", "/o", 200, "third", withStart(base.Add(30*time.Second))),
		mkEx(2, "GET", "/o", 200, "first", withStart(base.Add(10*time.Second))),
		mkEx(3, "GET", "/o", 200, "second", withStart(base.Add(20*time.Second))),
	)
	e := build(t, st, rec, Options{})
	var got []string
	for range 4 {
		_, b := lookup(e, "GET", "/o", "")
		got = append(got, b)
	}
	if strings.Join(got, ",") != "first,second,third,first" {
		t.Fatalf("order = %v", got)
	}
}

func TestBuildSkipsRequestBodiesWhenIgnored(t *testing.T) {
	st := openStore(t)
	rec := fill(t, st, mkEx(1, "POST", "/p", 200, "P", withReqBody("payload")))
	e := build(t, st, rec, Options{Match: model.MatchConfig{Body: model.BodyIgnore}})
	m := e.Lookup(mkReq("POST", "/p"), []byte("anything"))
	if m == nil || m.Level != LevelExact {
		t.Fatalf("match = %+v", m)
	}
	if len(m.Exchange.Request.Body) != 0 {
		t.Fatal("request bodies were loaded although they are not matched on")
	}
}

func TestLazyModeServesCorrectBodies(t *testing.T) {
	st := openStore(t)
	const n, size = 60, 2000
	var exs []*model.Exchange
	for i := 1; i <= n; i++ {
		exs = append(exs, mkEx(int64(i), "GET", fmt.Sprintf("/item/%d", i), 200, bigBody(i, size),
			withRespHeader("X-Item", fmt.Sprint(i))))
	}
	rec := fill(t, st, exs...)
	cs := &countingStore{Store: st}
	const maxResident = 10 * size // far below the ~120 KB recording
	e := build(t, cs, rec, Options{MaxResidentBytes: maxResident, Match: model.MatchConfig{PathTemplates: true}})
	if !e.Lazy() {
		t.Fatalf("recording of %d bytes with a %d byte limit must be lazy", rec.Bytes, maxResident)
	}
	if e.Len() != n {
		t.Fatalf("Len = %d", e.Len())
	}
	// The index keeps no bodies.
	for i := range e.shards {
		for _, en := range e.shards[i].m {
			for _, it := range en.cands {
				if it.body != nil || len(it.ex.Response.Body) != 0 || len(it.ex.Request.Body) != 0 {
					t.Fatal("lazy item holds bodies in memory")
				}
				if it.id == 0 {
					t.Fatal("lazy item has no exchange ID")
				}
			}
		}
	}

	check := func(i int) {
		t.Helper()
		rec, ok := serve(e, mkReq("GET", fmt.Sprintf("/item/%d", i)), "")
		if !ok || rec.Code != 200 || bodyOf(rec) != bigBody(i, size) {
			t.Fatalf("item %d: ok=%v code=%d body=%.20q", i, ok, rec.Code, bodyOf(rec))
		}
		if rec.Header().Get("X-Item") != fmt.Sprint(i) || rec.Header().Get("Content-Length") != fmt.Sprint(size) {
			t.Fatalf("item %d headers = %v", i, rec.Header())
		}
	}
	// Walk the whole recording twice: far more than fits in the cache.
	for round := 0; round < 2; round++ {
		for i := 1; i <= n; i++ {
			check(i)
			if sz, _ := e.cache.stats(); sz > maxResident {
				t.Fatalf("cache holds %d bytes, limit %d", sz, maxResident)
			}
		}
	}
	size1, entries := e.cache.stats()
	if entries > 10 || entries < 5 || size1 > maxResident {
		t.Fatalf("cache: %d entries, %d bytes", entries, size1)
	}
	// Evictions force reloads; a hot item stays cached.
	before := cs.gets.Load()
	for range 20 {
		check(n)
	}
	if got := cs.gets.Load(); got != before {
		t.Fatalf("a hot cached item was reloaded %d times", got-before)
	}
	// Templates work in lazy mode.
	if l, b := lookup(e, "GET", "/item/999", ""); l != LevelTemplate || len(b) != 0 {
		t.Fatalf("lazy template: (%s,%q): Lookup exposes the trimmed exchange", l, b)
	}
	rec2, _ := serve(e, mkReq("GET", "/item/999"), "")
	if rec2.Code != 200 || rec2.Body.Len() != size {
		t.Fatalf("template response: %d, %d bytes", rec2.Code, rec2.Body.Len())
	}
}

func TestLazyMatchesOnRequestBodies(t *testing.T) {
	st := openStore(t)
	rec := fill(t, st,
		mkEx(1, "POST", "/q", 200, bigBody(1, 3000), withReqBody(`{"a":1,"b":2}`)),
		mkEx(2, "POST", "/q", 200, bigBody(2, 3000), withReqBody(`{"a":9}`)),
	)
	e := build(t, st, rec, Options{MaxResidentBytes: 100})
	if !e.Lazy() {
		t.Fatal("not lazy")
	}
	rr, ok := serve(e, mkReq("POST", "/q"), `{ "b": 2, "a": 1 }`)
	if !ok || bodyOf(rr) != bigBody(1, 3000) || rr.Header().Get(HeaderMatch) != "exact" {
		t.Fatalf("ok=%v level=%s body=%.12q", ok, rr.Header().Get(HeaderMatch), bodyOf(rr))
	}
	rr, _ = serve(e, mkReq("POST", "/q"), `{"a":9}`)
	if bodyOf(rr) != bigBody(2, 3000) {
		t.Fatal("second request body matched the wrong exchange")
	}
}

func TestLazyConcurrentLoadsAreShared(t *testing.T) {
	st := openStore(t)
	rec := fill(t, st, mkEx(1, "GET", "/slow", 200, bigBody(1, 5000)))
	cs := &countingStore{Store: st, gate: make(chan struct{})}
	// The recording (5000 bytes) is just over the limit, and a body does not
	// fit in the cache either.
	e := build(t, cs, rec, Options{MaxResidentBytes: 4999})
	if !e.Lazy() {
		t.Fatal("not lazy")
	}
	var started, done sync.WaitGroup
	for range 40 {
		started.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			started.Done()
			rr, ok := serve(e, mkReq("GET", "/slow"), "")
			if !ok || bodyOf(rr) != bigBody(1, 5000) {
				t.Errorf("ok=%v body=%.12q", ok, bodyOf(rr))
			}
		}()
	}
	started.Wait()
	waitFor(t, "the first load to start", func() bool { return cs.gets.Load() == 1 })
	time.Sleep(100 * time.Millisecond) // let the other goroutines join the load
	close(cs.gate)
	done.Wait()
	if n := cs.gets.Load(); n != 1 {
		t.Fatalf("GetExchange called %d times for 40 concurrent requests of one body, want 1", n)
	}
	// The body is larger than the cache, so it is not kept: the next request
	// loads again.
	serve(e, mkReq("GET", "/slow"), "")
	if n := cs.gets.Load(); n != 2 {
		t.Fatalf("GetExchange calls = %d, want 2 (oversized bodies are not kept)", n)
	}
}

func TestLazyWaiterGivesUpWhenItsContextEnds(t *testing.T) {
	st := openStore(t)
	rec := fill(t, st, mkEx(1, "GET", "/slow", 200, bigBody(1, 500)))
	cs := &countingStore{Store: st, gate: make(chan struct{})}
	e := build(t, cs, rec, Options{MaxResidentBytes: 10})

	done := make(chan struct{})
	go func() {
		defer close(done)
		serve(e, mkReq("GET", "/slow"), "")
	}()
	waitFor(t, "the loader to reach the store", func() bool { return cs.gets.Load() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	begin := time.Now()
	rr, ok := serve(e, mkReq("GET", "/slow").WithContext(ctx), "")
	if !ok {
		t.Fatal("miss")
	}
	if el := time.Since(begin); el > 3*time.Second {
		t.Fatalf("waiter blocked %v despite its 40ms deadline", el)
	}
	if rr.Code != 500 || rr.Header().Get("X-Mirage-Error") != "store" {
		t.Fatalf("code=%d hdr=%v", rr.Code, rr.Header())
	}
	if cs.gets.Load() != 1 {
		t.Fatalf("the waiter started its own load (%d)", cs.gets.Load())
	}
	close(cs.gate)
	<-done
}

func TestLazyStoreFailure(t *testing.T) {
	st := openStore(t)
	rec := fill(t, st, mkEx(1, "GET", "/gone", 200, bigBody(1, 4000)))
	e := build(t, st, rec, Options{MaxResidentBytes: 10})
	if err := st.DeleteRecording(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}
	rr, ok := serve(e, mkReq("GET", "/gone"), "")
	if !ok || rr.Code != 500 || rr.Header().Get("X-Mirage-Error") != "store" {
		t.Fatalf("ok=%v code=%d hdr=%v body=%s", ok, rr.Code, rr.Header(), rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "recorded response unavailable") {
		t.Fatalf("body = %s", rr.Body.String())
	}
	// HEAD gets no body even for the error.
	rr, _ = serve(e, mkReq("HEAD", "/gone"), "")
	if rr.Code != 500 || rr.Body.Len() != 0 {
		t.Fatalf("HEAD: code=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestLazyGzip(t *testing.T) {
	st := openStore(t)
	plain := strings.Repeat("compress me ", 400) // 4800 bytes
	rec := fill(t, st,
		mkEx(1, "GET", "/z", 200, plain, withGzip()),
		mkEx(2, "GET", "/z2", 200, plain, withGzip()),
	)
	get := func(e *Engine, path string) *httptest.ResponseRecorder {
		rr, ok := serve(e, mkReq("GET", path, "Accept-Encoding", "gzip"), "")
		if !ok || rr.Header().Get("Content-Encoding") != "gzip" || gunzip(t, rr.Body.Bytes()) != plain {
			t.Fatalf("lazy gzip response wrong: ok=%v %v", ok, rr.Header())
		}
		return rr
	}

	// Limit below one body: nothing can be cached, every request reloads and
	// recompresses, but the answer is always right.
	tiny := build(t, st, rec, Options{MaxResidentBytes: 10})
	if !tiny.Lazy() {
		t.Fatal("not lazy")
	}
	for range 3 {
		get(tiny, "/z")
	}
	if n := tiny.gzips.Load(); n != 3 {
		t.Fatalf("uncacheable body compressed %d times, want 3", n)
	}

	// Limit above one body (and its gzip form) but below the recording: the
	// hot body stays cached with its compressed form, which is built once.
	roomy := build(t, st, rec, Options{MaxResidentBytes: 6000})
	if !roomy.Lazy() {
		t.Fatalf("not lazy (recording bytes %d)", rec.Bytes)
	}
	for range 5 {
		get(roomy, "/z")
	}
	if n := roomy.gzips.Load(); n != 1 {
		t.Fatalf("compressed %d times with the body cached, want 1", n)
	}
	size, entries := roomy.cache.stats()
	if entries != 1 || size <= int64(len(plain)) || size > 6000 {
		t.Fatalf("cache entries=%d size=%d: the gzip form should be accounted for", entries, size)
	}
	// Loading the other body evicts the first (they do not fit together).
	get(roomy, "/z2")
	if _, entries := roomy.cache.stats(); entries != 1 {
		t.Fatalf("entries = %d, want 1", entries)
	}
}

func TestLazyAddInHybridMode(t *testing.T) {
	st := openStore(t)
	rec := fill(t, st, mkEx(1, "GET", "/old", 200, bigBody(1, 3000)))
	e := build(t, st, rec, Options{MaxResidentBytes: 100})
	if !e.Lazy() {
		t.Fatal("not lazy")
	}
	// A captured exchange is stored first (which assigns its ID) and then added.
	ctx := context.Background()
	stored := mkEx(2, "GET", "/new", 200, bigBody(2, 3000))
	if err := st.AppendExchanges(ctx, rec.ID, []*model.Exchange{stored}); err != nil {
		t.Fatal(err)
	}
	if stored.ID == 0 {
		t.Fatal("store did not assign an ID")
	}
	e.Add(stored)
	rr, ok := serve(e, mkReq("GET", "/new"), "")
	if !ok || bodyOf(rr) != bigBody(2, 3000) {
		t.Fatalf("ok=%v body=%.12q", ok, bodyOf(rr))
	}
	// An exchange that is not in the store has to stay resident.
	e.Add(mkEx(3, "GET", "/unsaved", 200, "unsaved-body", func(x *model.Exchange) { x.ID = 0 }))
	rr, ok = serve(e, mkReq("GET", "/unsaved"), "")
	if !ok || bodyOf(rr) != "unsaved-body" {
		t.Fatalf("ok=%v body=%q", ok, bodyOf(rr))
	}
	if e.Len() != 3 {
		t.Fatalf("Len = %d", e.Len())
	}
}

func TestResidentOverflowTurnsLazy(t *testing.T) {
	st := openStore(t)
	rec := fill(t, st)
	var stored []*model.Exchange
	for i := 1; i <= 10; i++ {
		stored = append(stored, mkEx(int64(i), "GET", fmt.Sprintf("/x/%d", i), 200, bigBody(i, 1000)))
	}
	if err := st.AppendExchanges(context.Background(), rec.ID, stored); err != nil {
		t.Fatal(err)
	}
	// A hybrid engine starts empty (nothing recorded yet) and grows by Add.
	e := NewEngine(st, noLatency(Options{RecordingID: rec.ID, MaxResidentBytes: 3500}))
	t.Cleanup(func() { _ = e.Close() })
	for _, ex := range stored {
		e.Add(ex)
	}
	seen := map[*item]bool{}
	resident, lazy := 0, 0
	for i := range e.shards {
		for _, en := range e.shards[i].m {
			for _, it := range en.cands {
				if seen[it] {
					continue // indexed under the exact and the path level
				}
				seen[it] = true
				if it.body != nil {
					resident++
				} else {
					lazy++
				}
			}
		}
	}
	// Three 1000 byte bodies fit under 3500; the rest are loaded on demand.
	if resident != 3 || lazy != 7 {
		t.Fatalf("resident=%d lazy=%d, want 3 and 7", resident, lazy)
	}
	if r := e.resident.Load(); r > 3500 {
		t.Fatalf("resident bytes = %d", r)
	}
	for i := 1; i <= 10; i++ {
		rr, ok := serve(e, mkReq("GET", fmt.Sprintf("/x/%d", i)), "")
		if !ok || bodyOf(rr) != bigBody(i, 1000) {
			t.Fatalf("item %d: ok=%v body=%.12q", i, ok, bodyOf(rr))
		}
	}
}

func TestLazyConcurrentServing(t *testing.T) {
	st := openStore(t)
	var exs []*model.Exchange
	for i := 1; i <= 40; i++ {
		exs = append(exs, mkEx(int64(i), "GET", fmt.Sprintf("/c/%d", i), 200, bigBody(i, 1500), withGzip()))
	}
	rec := fill(t, st, exs...)
	e := build(t, st, rec, Options{MaxResidentBytes: 6000})
	if !e.Lazy() {
		t.Fatal("not lazy")
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range 400 {
				i := (g*31+k*7)%40 + 1
				var hdr []string
				if k%2 == 0 {
					hdr = []string{"Accept-Encoding", "gzip"}
				}
				rr, ok := serve(e, mkReq("GET", fmt.Sprintf("/c/%d", i), hdr...), "")
				if !ok {
					t.Errorf("miss for %d", i)
					return
				}
				got := rr.Body.Bytes()
				if rr.Header().Get("Content-Encoding") == "gzip" {
					got = []byte(gunzip(t, got))
				}
				if !bytes.Equal(got, []byte(bigBody(i, 1500))) {
					t.Errorf("wrong body for %d", i)
					return
				}
			}
		}()
	}
	wg.Wait()
	if size, _ := e.cache.stats(); size > 6000 {
		t.Fatalf("cache over its limit: %d", size)
	}
}

func TestBodyCacheUnit(t *testing.T) {
	c := newBodyCache(100)
	loads := map[int64]int{}
	load := func(id int64, n int) func() (*bodyEntry, error) {
		return func() (*bodyEntry, error) {
			loads[id]++
			return &bodyEntry{raw: make([]byte, n), id: id, lru: c}, nil
		}
	}
	ctx := context.Background()
	for _, id := range []int64{1, 2, 3} { // 3 x 40 bytes: the first is evicted
		if _, err := c.get(ctx, id, load(id, 40)); err != nil {
			t.Fatal(err)
		}
	}
	if size, entries := c.stats(); size != 80 || entries != 2 {
		t.Fatalf("size=%d entries=%d, want 80/2", size, entries)
	}
	c.get(ctx, 2, load(2, 40)) // hit, becomes most recent
	c.get(ctx, 4, load(4, 40)) // evicts 3 (least recent), not 2
	c.get(ctx, 2, load(2, 40))
	if loads[2] != 1 || loads[3] != 1 {
		t.Fatalf("loads = %v: 2 should still be cached", loads)
	}
	c.get(ctx, 3, load(3, 40))
	if loads[3] != 2 {
		t.Fatalf("3 should have been evicted: %v", loads)
	}
	// An oversized entry is returned but not kept, and flushes nothing else
	// that would fit.
	b, err := c.get(ctx, 9, load(9, 500))
	if err != nil || len(b.raw) != 500 {
		t.Fatalf("oversized: %v", err)
	}
	if size, _ := c.stats(); size > 100 {
		t.Fatalf("cache over its limit: %d", size)
	}
	// Errors are not cached.
	boom := errors.New("boom")
	if _, err := c.get(ctx, 7, func() (*bodyEntry, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.get(ctx, 7, load(7, 10)); err != nil {
		t.Fatalf("retry after an error: %v", err)
	}
	// A panicking loader releases its waiters and the in-flight slot.
	func() {
		defer func() { _ = recover() }()
		c.get(ctx, 8, func() (*bodyEntry, error) { panic("loader") })
	}()
	if _, err := c.get(ctx, 8, load(8, 10)); err != nil {
		t.Fatalf("after a panic: %v", err)
	}
	c.close()
	if size, entries := c.stats(); size != 0 || entries != 0 {
		t.Fatalf("after close: %d %d", size, entries)
	}
	if _, err := c.get(ctx, 1, load(1, 10)); err != nil {
		t.Fatal(err)
	}
	if _, entries := c.stats(); entries != 0 {
		t.Fatal("a closed cache keeps entries")
	}
}
