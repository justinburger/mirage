package virtual

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// Add runs while requests are being served: nothing races, every response is
// a complete recorded one, and every added exchange is eventually served.
func TestAddDuringConcurrentServe(t *testing.T) {
	e := newTestEngine(t, Options{Match: model.MatchConfig{PathTemplates: true}},
		mkEx(1, "GET", "/seed/1", 200, "/seed/1"))
	const writers, perWriter, readers = 4, 300, 6

	var stop atomic.Bool
	var rwg sync.WaitGroup
	var served atomic.Int64
	for r := range readers {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			for k := 0; !stop.Load(); k++ {
				w := k % writers
				i := k % perWriter
				path := fmt.Sprintf("/w%d/%d", w, i)
				rec, ok := serve(e, mkReq("GET", path), "")
				if ok {
					served.Add(1)
					// Exact matches carry their own path as body; template
					// matches carry the path of a sibling.
					if rec.Code != 200 || rec.Body.Len() == 0 {
						t.Errorf("reader %d: %s: code=%d body=%q", r, path, rec.Code, bodyOf(rec))
						return
					}
					if rec.Header().Get(HeaderMatch) == "exact" && bodyOf(rec) != path {
						t.Errorf("exact match of %s returned %q", path, bodyOf(rec))
						return
					}
				}
				e.Lookup(mkReq("GET", "/seed/1"), nil)
				e.Stats()
				e.Len()
			}
		}()
	}

	var wwg sync.WaitGroup
	for w := range writers {
		wwg.Add(1)
		go func() {
			defer wwg.Done()
			for i := range perWriter {
				path := fmt.Sprintf("/w%d/%d", w, i)
				seq := int64(w*perWriter + i + 10)
				// Alternate newer and older start times so inserts land at the
				// end and in the middle of shared template lists.
				start := base.Add(time.Duration(seq) * time.Second)
				if i%2 == 1 {
					start = base.Add(-time.Duration(seq) * time.Second)
				}
				e.Add(mkEx(seq, "GET", path, 200, path, withStart(start)))
			}
		}()
	}
	wwg.Wait()
	stop.Store(true)
	rwg.Wait()

	if want := 1 + writers*perWriter; e.Len() != want {
		t.Fatalf("Len = %d, want %d", e.Len(), want)
	}
	for w := range writers {
		for i := range perWriter {
			path := fmt.Sprintf("/w%d/%d", w, i)
			if l, b := lookup(e, "GET", path, ""); l != LevelExact || b != path {
				t.Fatalf("%s after the adds: (%s,%q)", path, l, b)
			}
		}
	}
	if served.Load() == 0 {
		t.Fatal("readers never hit")
	}
	// The shared template list is ordered by StartedAt after all the inserts.
	for i := range e.shards {
		sh := &e.shards[i]
		sh.mu.RLock()
		for k, en := range sh.m {
			for j := 1; j < len(en.cands); j++ {
				if before(en.cands[j], en.cands[j-1]) {
					t.Errorf("entry %q is out of order at %d", k, j)
				}
			}
		}
		sh.mu.RUnlock()
	}
}

// The sequential cursor hands every candidate out the same number of times
// when many goroutines share one key.
func TestSequentialSelectionConcurrent(t *testing.T) {
	const cands, goroutines, each = 6, 8, 1200
	var exs []*model.Exchange
	for i := 1; i <= cands; i++ {
		exs = append(exs, mkEx(int64(i), "GET", "/seq", 200, fmt.Sprint(i)))
	}
	e := newTestEngine(t, Options{Match: model.MatchConfig{Selection: model.SelectSequential}}, exs...)
	var counts [cands + 1]atomic.Int64
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := mkReq("GET", "/seq")
			for range each {
				m := e.Lookup(r, nil)
				counts[m.Exchange.Seq].Add(1)
			}
		}()
	}
	wg.Wait()
	for i := 1; i <= cands; i++ {
		if got, want := counts[i].Load(), int64(goroutines*each/cands); got != want {
			t.Errorf("candidate %d chosen %d times, want exactly %d", i, got, want)
		}
	}
	if s := e.Stats(); s.Hits != goroutines*each || s.Misses != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestConcurrentStatsAddUp(t *testing.T) {
	e := newTestEngine(t, Options{Match: model.MatchConfig{PathTemplates: true}, ErrorFloor: 0.2},
		mkEx(1, "GET", "/a/1", 200, "a"), mkEx(2, "GET", "/b", 500, "b"))
	var wg sync.WaitGroup
	const g, n = 8, 2000
	for range g {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range n {
				switch k % 4 {
				case 0:
					e.Lookup(mkReq("GET", "/a/1"), nil)
				case 1:
					e.Lookup(mkReq("GET", "/a/2"), nil)
				case 2:
					e.Lookup(mkReq("GET", "/b"), nil)
				default:
					e.Lookup(mkReq("GET", "/nope"), nil)
				}
			}
		}()
	}
	wg.Wait()
	s := e.Stats()
	if s.Hits+s.Misses != g*n || s.Misses != g*n/4 {
		t.Fatalf("stats = %+v", s)
	}
	if lv := s.ByLevel; lv[LevelExact]+lv[LevelPath]+lv[LevelTemplate] != s.Hits {
		t.Fatalf("levels %v do not add up to %d hits", lv, s.Hits)
	}
	if s.Injected == 0 {
		t.Fatal("no injections with a 20% floor")
	}
}

// Respond for the same match from many goroutines (gzip once-initialisation,
// date cache, header copying) is race free.
func TestConcurrentRespond(t *testing.T) {
	e := newTestEngine(t, Options{ErrorAmplification: 3, Seed: 1},
		mkEx(1, "GET", "/r", 200, string(make([]byte, 4000)), withGzip(), withRespHeader("X-A", "1", "2")),
		mkEx(2, "GET", "/r", 500, "err"))
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range 500 {
				hdr := []string{}
				if (g+k)%2 == 0 {
					hdr = []string{"Accept-Encoding", "gzip"}
				}
				rec, ok := serve(e, mkReq("GET", "/r", hdr...), "")
				if !ok || (rec.Code != 200 && rec.Code != 500 && rec.Code != 503) {
					t.Errorf("ok=%v code=%d", ok, rec.Code)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// A real HTTP server end to end, including a streaming-friendly flush before
// the body and a client that reads the headers early.
func TestServeOverHTTPServer(t *testing.T) {
	e := newTestEngine(t, Options{Latency: model.LatencyConfig{Mode: model.LatencyRecorded}, Match: model.MatchConfig{PathTemplates: true}},
		mkEx(1, "GET", "/users/1", 200, `{"id":1}`, withLatency(10*time.Millisecond, 600*time.Millisecond), withRespHeader("X-Tag", "t")),
		mkEx(2, "POST", "/echo", 201, `{"ok":true}`, withReqBody(`{"a":1}`)),
	)
	srv := newTestServer(t, e)

	start := time.Now()
	resp, err := http.Get(srv + "/users/1")
	if err != nil {
		t.Fatal(err)
	}
	hdrAt := time.Since(start)
	body := readAll(t, resp)
	total := time.Since(start)
	if resp.StatusCode != 200 || body != `{"id":1}` || resp.Header.Get("X-Tag") != "t" {
		t.Fatalf("code=%d body=%q hdr=%v", resp.StatusCode, body, resp.Header)
	}
	if hdrAt >= 400*time.Millisecond {
		t.Fatalf("headers took %v: they must arrive well before the body (~10ms vs 600ms)", hdrAt)
	}
	if total < 550*time.Millisecond {
		t.Fatalf("total %v, want at least ~600ms", total)
	}
	if resp.Header.Get("Date") == "" || resp.Header.Get(HeaderMatch) != "exact" {
		t.Fatalf("headers = %v", resp.Header)
	}

	// Templates and misses through the same listener.
	resp, err = http.Get(srv + "/users/77")
	if err != nil || resp.Header.Get(HeaderMatch) != "template" {
		t.Fatalf("template: err=%v hdr=%v", err, resp.Header)
	}
	readAll(t, resp)
	resp, err = http.Get(srv + "/nothing")
	if err != nil || resp.StatusCode != 404 || resp.Header.Get(HeaderMatch) != "miss" {
		t.Fatalf("miss: err=%v code=%d", err, resp.StatusCode)
	}
	readAll(t, resp)
}
