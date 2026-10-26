package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// bodyUpstream answers every request with a body of n bytes.
func bodyUpstream(t testing.TB, n int) *httptest.Server {
	t.Helper()
	body := strings.Repeat("x", n)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// gatedEnv returns an env whose store blocks every append until the returned
// release function is called.
func gatedEnv(t *testing.T, tweak ...func(*Deps)) (*env, *failingStore, func()) {
	t.Helper()
	fs := &failingStore{gate: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(fs.gate) }) }
	e := newEnv(t, append([]func(*Deps){func(d *Deps) { fs.Store = d.Store; d.Store = fs }}, tweak...)...)
	t.Cleanup(release) // runs before the manager's cleanup (LIFO): never leave the writer stuck
	return e, fs, release
}

func TestFullCaptureQueueDropsInsteadOfSlowingTraffic(t *testing.T) {
	up := newEcho(t)
	e, fs, release := gatedEnv(t, func(d *Deps) {
		d.CaptureQueueSize = 3
		d.CaptureWorkers = 1
		d.CaptureBatchSize = 1
	})
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	c := newClient(t)

	const n = 300
	start := time.Now()
	for i := 0; i < n; i++ {
		if status, _, _ := do(t, c, "GET", fmt.Sprintf("%s/q/%d", base, i), nil); status != 200 {
			t.Fatalf("request %d: status %d: capture back-pressure must never fail traffic", i, status)
		}
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("%d requests took %v with a stuck store", n, d)
	}
	waitFor(t, "the writer to block", func() bool { return fs.appends.Load() >= 1 })
	// All but the few that fit into the pipeline (queue 3, ready 64, worker,
	// writer) must have been dropped. The hand-off runs after the response is
	// written, so the last drops can lag the client by a moment.
	waitFor(t, "most captures to be dropped", func() bool { return e.mgr.Status(svc.ID).CaptureDropped >= n-100 })
	release()
	waitFor(t, "every capture to be stored or dropped", func() bool {
		s := e.mgr.Status(svc.ID)
		return s.Captured+s.CaptureDropped == n
	})
	st := e.mgr.Status(svc.ID)
	if got := e.captured(svc.RecordingID); int64(len(got)) != st.Captured {
		t.Errorf("store holds %d, status says %d captured", len(got), st.Captured)
	}
	if st.Captured == 0 {
		t.Error("nothing was captured after the store recovered")
	}
}

func TestCaptureQueueByteBound(t *testing.T) {
	t.Run("excess bytes are dropped", func(t *testing.T) {
		up := bodyUpstream(t, 600)
		e, _, release := gatedEnv(t, func(d *Deps) {
			d.CaptureQueueBytes = 1000
			d.CaptureWorkers = 1
			d.CaptureBatchSize = 1
		})
		svc := e.service(up.URL, model.ModeRecord)
		base := e.start(svc)
		for i := 0; i < 5; i++ {
			do(t, newClient(t), "GET", fmt.Sprintf("%s/b%d", base, i), nil)
		}
		waitFor(t, "four drops (only one 600-byte capture fits into 1000 queued bytes)", func() bool { return e.mgr.Status(svc.ID).CaptureDropped == 4 })
		release()
		waitFor(t, "the queued capture to be stored", func() bool { return e.mgr.Status(svc.ID).Captured == 1 })
	})
	t.Run("an exchange larger than the bound is admitted into an empty queue", func(t *testing.T) {
		up := bodyUpstream(t, 5000)
		e, _, release := gatedEnv(t, func(d *Deps) {
			d.CaptureQueueBytes = 1000
			d.CaptureWorkers = 1
			d.CaptureBatchSize = 1
		})
		svc := e.service(up.URL, model.ModeRecord)
		base := e.start(svc)
		for i := 0; i < 3; i++ {
			do(t, newClient(t), "GET", fmt.Sprintf("%s/huge%d", base, i), nil)
		}
		waitFor(t, "two drops", func() bool { return e.mgr.Status(svc.ID).CaptureDropped == 2 })
		release()
		waitFor(t, "the oversized capture to be stored", func() bool { return e.mgr.Status(svc.ID).Captured == 1 })
	})
}

func TestCaptureSampling(t *testing.T) {
	up := newEcho(t)
	for _, tc := range []struct {
		name         string
		sample       float64
		minCap, maxC int64
	}{
		{"default captures everything", 0, 400, 400},
		{"one captures everything", 1, 400, 400},
		{"half", 0.5, 120, 280},
		{"almost none", 1e-9, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.CaptureSample = tc.sample })
			base := e.start(svc)
			var wg sync.WaitGroup
			for w := 0; w < 8; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					c := newClient(t)
					for i := 0; i < 50; i++ {
						do(t, c, "GET", base+"/s", nil)
					}
				}()
			}
			wg.Wait()
			waitFor(t, "captured + skipped = 400", func() bool {
				s := e.mgr.Status(svc.ID)
				return s.Captured+s.CaptureSkipped == 400
			})
			st := e.mgr.Status(svc.ID)
			if st.Captured < tc.minCap || st.Captured > tc.maxC {
				t.Errorf("captured %d of 400, want %d..%d", st.Captured, tc.minCap, tc.maxC)
			}
			if st.CaptureDropped != 0 {
				t.Errorf("dropped %d", st.CaptureDropped)
			}
			e.waitStored(svc.RecordingID, int(st.Captured))
		})
	}
}

func TestMaxCaptureBytesStopsCapture(t *testing.T) {
	up := bodyUpstream(t, 500)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.MaxCaptureBytes = 2000 })
	base := e.start(svc)
	c := newClient(t)

	for i := 0; i < 20; i++ {
		if status, _, _ := do(t, c, "GET", fmt.Sprintf("%s/m%d", base, i), nil); status != 200 {
			t.Fatalf("traffic must keep flowing past the limit: %d", status)
		}
	}
	waitFor(t, "four captures", func() bool { return e.mgr.Status(svc.ID).Captured == 4 })
	st := e.mgr.Status(svc.ID)
	if st.CaptureSkipped != 16 || st.Warning != "capture limit reached" || st.Error != "" || !st.Running {
		t.Errorf("status = %+v, want 16 skipped, running, and the limit as a warning (not an error)", st)
	}

	// Raising the limit clears the error and resumes capture.
	svc.MaxCaptureBytes = 0
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	if st := e.mgr.Status(svc.ID); st.Error != "" || st.Warning != "" {
		t.Errorf("note after raising the limit: error %q, warning %q", st.Error, st.Warning)
	}
	do(t, c, "GET", base+"/resumed", nil)
	e.waitStored(svc.RecordingID, 5)
}

func TestMaxCaptureBytesCountsWhatTheRecordingAlreadyHolds(t *testing.T) {
	up := bodyUpstream(t, 500)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.MaxCaptureBytes = 2000 })
	seedExchange(t, e, svc.RecordingID, "GET", "/old", 200, strings.Repeat("o", 1500))
	base := e.start(svc)
	c := newClient(t)

	do(t, c, "GET", base+"/first", nil)
	waitFor(t, "first capture", func() bool { return e.mgr.Status(svc.ID).Captured == 1 })
	do(t, c, "GET", base+"/second", nil)
	waitFor(t, "second request skipped", func() bool { return e.mgr.Status(svc.ID).CaptureSkipped == 1 })
	if got := e.captured(svc.RecordingID); len(got) != 2 {
		t.Errorf("recording holds %d exchanges, want the seeded one plus one capture", len(got))
	}
}

func TestBatchingByCountAndInterval(t *testing.T) {
	up := newEcho(t)
	t.Run("count", func(t *testing.T) {
		fs := &failingStore{}
		e := newEnv(t, func(d *Deps) {
			fs.Store = d.Store
			d.Store = fs
			d.CaptureBatchSize = 3
			d.CaptureFlushInterval = time.Hour
		})
		svc := e.service(up.URL, model.ModeRecord)
		base := e.start(svc)
		for i := 0; i < 7; i++ {
			do(t, newClient(t), "GET", fmt.Sprintf("%s/c%d", base, i), nil)
		}
		e.waitStored(svc.RecordingID, 6)
		time.Sleep(50 * time.Millisecond)
		if got := len(e.captured(svc.RecordingID)); got != 6 {
			t.Errorf("%d stored before the interval, want two full batches of 3", got)
		}
		if fs.appends.Load() != 2 {
			t.Errorf("%d appends, want 2", fs.appends.Load())
		}
		if err := e.mgr.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := len(e.captured(svc.RecordingID)); got != 7 {
			t.Errorf("%d stored after Shutdown, want 7", got)
		}
	})
	t.Run("interval", func(t *testing.T) {
		e := newEnv(t, func(d *Deps) {
			d.CaptureBatchSize = 100000
			d.CaptureFlushInterval = 25 * time.Millisecond
		})
		svc := e.service(up.URL, model.ModeRecord)
		base := e.start(svc)
		do(t, newClient(t), "GET", base+"/slowflush", nil)
		e.waitStored(svc.RecordingID, 1)
	})
	t.Run("batches are per recording", func(t *testing.T) {
		e := newEnv(t, func(d *Deps) { d.CaptureBatchSize = 2; d.CaptureFlushInterval = time.Hour })
		a := e.service(up.URL, model.ModeRecord)
		b := e.service(up.URL, model.ModeRecord)
		baseA, baseB := e.start(a), e.start(b)
		c := newClient(t)
		do(t, c, "GET", baseA+"/a1", nil)
		do(t, c, "GET", baseB+"/b1", nil)
		do(t, c, "GET", baseB+"/b2", nil)
		do(t, c, "GET", baseA+"/a2", nil)
		e.waitStored(a.RecordingID, 2)
		e.waitStored(b.RecordingID, 2)
		for _, ex := range e.captured(a.RecordingID) {
			if ex.ServiceID != a.ID || !strings.HasPrefix(ex.Request.Path, "/a") {
				t.Errorf("exchange %s landed in the wrong recording", ex.Request.Path)
			}
		}
	})
}

func TestStoreFailureDropsCapturesAndReports(t *testing.T) {
	up := newEcho(t)
	fs := &failingStore{err: errTest}
	e := newEnv(t, func(d *Deps) { fs.Store = d.Store; d.Store = fs })
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	for i := 0; i < 5; i++ {
		if status, _, _ := do(t, newClient(t), "GET", fmt.Sprintf("%s/f%d", base, i), nil); status != 200 {
			t.Fatalf("a failing store must not fail traffic: %d", status)
		}
	}
	waitFor(t, "five drops", func() bool { return e.mgr.Status(svc.ID).CaptureDropped == 5 })
	st := e.mgr.Status(svc.ID)
	if st.Captured != 0 || !strings.Contains(st.Error, "store") {
		t.Errorf("status = %+v", st)
	}
}

func TestCaptureIntoDeletedRecordingIsDroppedAndReported(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)
	if err := e.st.DeleteRecording(context.Background(), svc.RecordingID); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := do(t, newClient(t), "GET", base+"/orphan", nil); status != 200 {
		t.Fatalf("status %d", status)
	}
	waitFor(t, "the capture to be dropped", func() bool { return e.mgr.Status(svc.ID).CaptureDropped == 1 })
	if st := e.mgr.Status(svc.ID); st.Error == "" {
		t.Errorf("no error reported: %+v", st)
	}
}

func TestParallelRecordingKeepsEveryExchangeIntact(t *testing.T) {
	// The upstream echoes the request body; every request carries its own
	// path as body, so a recycled buffer that leaks between requests shows
	// up as a path/body mismatch.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Path", r.URL.Path)
		w.Write(b)
	}))
	defer up.Close()
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	base := e.start(svc)

	const workers, each = 16, 60
	var wg sync.WaitGroup
	var bad atomic.Int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := newClient(t)
			for i := 0; i < each; i++ {
				path := fmt.Sprintf("/w%d/%d", w, i)
				payload := strings.Repeat(path+";", 1+i%40) // 0..~500 bytes, varying
				status, _, body := do(t, c, "POST", base+path, strings.NewReader(payload))
				if status != 200 || string(body) != payload {
					bad.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d responses were wrong", bad.Load())
	}
	exs := e.waitStored(svc.RecordingID, workers*each)
	seen := map[string]bool{}
	for _, ex := range exs {
		path := ex.Request.Path
		if seen[path] {
			t.Errorf("%s stored twice", path)
		}
		seen[path] = true
		var i int
		fmt.Sscanf(path[strings.LastIndex(path, "/")+1:], "%d", &i)
		want := strings.Repeat(path+";", 1+i%40)
		if string(ex.Request.Body) != want || string(ex.Response.Body) != want {
			t.Errorf("%s: stored bodies do not match what was sent (%d/%d bytes)", path, len(ex.Request.Body), len(ex.Response.Body))
		}
		if ex.Response.Header.Get("X-Path") != path {
			t.Errorf("%s: response header %q", path, ex.Response.Header.Get("X-Path"))
		}
	}
	if len(seen) != workers*each {
		t.Errorf("%d distinct exchanges, want %d", len(seen), workers*each)
	}
	if st := e.mgr.Status(svc.ID); st.Captured != workers*each || st.CaptureDropped != 0 || st.CaptureSkipped != 0 {
		t.Errorf("status = %+v", st)
	}
	rec, _ := e.st.GetRecording(context.Background(), svc.RecordingID)
	if rec.ExchangeCount != workers*each {
		t.Errorf("recording count %d", rec.ExchangeCount)
	}
}

func TestEnqueueAfterStopIsDropped(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	e.start(svc)
	if err := e.mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := e.mgr.get(svc.ID)
	e.mgr.pipe.enqueue(&captureJob{ex: &model.Exchange{}, svc: s, queued: 10})
	if s.dropped.Load() != 1 || e.mgr.pipe.queuedBytes.Load() != 0 {
		t.Errorf("dropped %d, queued bytes %d", s.dropped.Load(), e.mgr.pipe.queuedBytes.Load())
	}
}
