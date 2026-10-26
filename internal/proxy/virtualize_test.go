package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
)

// seedExchange stores a recorded exchange in a recording.
func seedExchange(t testing.TB, e *env, recID, method, uri string, status int, body string) {
	t.Helper()
	path, query, _ := strings.Cut(uri, "?")
	ex := &model.Exchange{
		StartedAt: time.Now(),
		Endpoint:  method + " " + path,
		Request:   model.Request{Method: method, Path: path, RawQuery: query, Header: http.Header{}},
		Response: model.Response{
			Status:   status,
			Header:   http.Header{"Content-Type": {"application/json"}},
			Body:     []byte(body),
			BodySize: int64(len(body)),
		},
	}
	if err := e.st.AppendExchanges(context.Background(), recID, []*model.Exchange{ex}); err != nil {
		t.Fatalf("AppendExchanges: %v", err)
	}
}

// failIfHit is an upstream that fails the test when contacted.
func failIfHit(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream contacted in virtualize mode: %s %s", r.Method, r.URL)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVirtualizeServesHitsAndMissesWithoutUpstream(t *testing.T) {
	up := failIfHit(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeVirtualize, func(s *model.Service) { s.Latency.Mode = model.LatencyNone })
	seedExchange(t, e, svc.RecordingID, "GET", "/users/42", 200, `{"id":42}`)
	seedExchange(t, e, svc.RecordingID, "GET", "/users/42?expand=orders", 200, `{"id":42,"orders":[]}`)
	base := e.start(svc)
	c := newClient(t)

	status, hdr, body := do(t, c, "GET", base+"/users/42?expand=orders", nil)
	if status != 200 || bodyOf(body) != `{"id":42,"orders":[]}` || hdr.Get("X-Mirage-Match") != "exact" {
		t.Errorf("hit: %d %v %s", status, hdr, body)
	}
	status, hdr, body = do(t, c, "GET", base+"/nope", nil)
	if status != 404 || hdr.Get("X-Mirage-Match") != "miss" || !strings.Contains(string(body), "no recorded response") {
		t.Errorf("miss: %d %v %s", status, hdr, body)
	}

	st := e.mgr.Status(svc.ID)
	if st.VirtualHits != 1 || st.VirtualMisses != 1 || st.IndexedExchanges != 2 || st.Mode != model.ModeVirtualize {
		t.Errorf("status = %+v", st)
	}
	snap := e.waitIdle(svc.ID)
	if snap.Requests != 2 || snap.Errors != 0 || snap.StatusClasses["4xx"] != 1 {
		t.Errorf("metrics: %d requests, %d errors, %v (a 404 miss is not an error status)", snap.Requests, snap.Errors, snap.StatusClasses)
	}
	if st.Captured != 0 || st.CaptureSkipped != 0 {
		t.Errorf("virtualize must not capture: %+v", st)
	}
}

func TestVirtualizeWithoutUpstreamURL(t *testing.T) {
	e := newEnv(t)
	svc := e.service("", model.ModeVirtualize)
	seedExchange(t, e, svc.RecordingID, "GET", "/a", 200, `{}`)
	base := e.start(svc)
	if status, _, _ := do(t, newClient(t), "GET", base+"/a", nil); status != 200 {
		t.Errorf("status %d", status)
	}
}

func TestVirtualizePassesBoundedBodyToEngine(t *testing.T) {
	e := newEnv(t)
	svc := e.service("", model.ModeVirtualize, func(s *model.Service) { s.MaxBodyBytes = 16 })
	base := e.start(svc)
	c := newClient(t)

	do(t, c, "POST", base+"/small", strings.NewReader("tiny"))
	do(t, c, "POST", base+"/big", strings.NewReader(strings.Repeat("0123456789", 100)))
	do(t, c, "GET", base+"/none", nil)
	bodies := e.virts.last().seenBodies()
	if len(bodies) != 2 || string(bodies[0]) != "tiny" || string(bodies[1]) != strings.Repeat("0123456789", 100)[:16] {
		t.Errorf("bodies seen by the engine: %q", bodies)
	}
	snap := e.waitIdle(svc.ID)
	if snap.BytesIn != 4+1000 {
		t.Errorf("BytesIn = %d, want the full 1004 bytes, not just what was read for matching", snap.BytesIn)
	}
}

func TestVirtualizeWithoutBodyCaptureSendsNoBody(t *testing.T) {
	e := newEnv(t)
	svc := e.service("", model.ModeVirtualize, func(s *model.Service) { s.MaxBodyBytes = -1 })
	base := e.start(svc)
	do(t, newClient(t), "POST", base+"/x", strings.NewReader("payload"))
	if bodies := e.virts.last().seenBodies(); len(bodies) != 0 {
		t.Errorf("engine got bodies %q with MaxBodyBytes=-1", bodies)
	}
}

func TestVirtualizeNeedsAFactory(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Virtualizers = nil })
	svc := e.service("", model.ModeVirtualize)
	if err := e.mgr.Apply(context.Background(), svc); err == nil || e.mgr.Status(svc.ID).Running {
		t.Errorf("Apply = %v, running %v: want an error and no listener", err, e.mgr.Status(svc.ID).Running)
	}
}

func TestVirtualizerBuildFailureLeavesRunningServiceAlone(t *testing.T) {
	e := newEnv(t)
	svc := e.service("", model.ModeVirtualize)
	seedExchange(t, e, svc.RecordingID, "GET", "/a", 200, `{"v":1}`)
	base := e.start(svc)
	c := newClient(t)

	e.virts.setFail(errTest)
	changed := svc
	changed.Match.Selection = model.SelectFirst // forces a rebuild
	err := e.mgr.Apply(context.Background(), changed)
	if !errors.Is(err, errTest) {
		t.Fatalf("Apply = %v, want the factory error", err)
	}
	st := e.mgr.Status(svc.ID)
	if !st.Running || st.Error == "" {
		t.Errorf("status = %+v: the service keeps running and shows the error", st)
	}
	if status, _, _ := do(t, c, "GET", base+"/a", nil); status != 200 {
		t.Errorf("service stopped serving after a failed Apply: %d", status)
	}
	e.virts.setFail(nil)
	if err := e.mgr.Apply(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	if st := e.mgr.Status(svc.ID); st.Error != "" {
		t.Errorf("error not cleared by a successful Apply: %q", st.Error)
	}
}

func TestReloadRebuildsVirtualizerFromRecording(t *testing.T) {
	e := newEnv(t)
	svc := e.service("", model.ModeVirtualize)
	seedExchange(t, e, svc.RecordingID, "GET", "/old", 200, `{"v":"old"}`)
	base := e.start(svc)
	c := newClient(t)

	if status, _, _ := do(t, c, "GET", base+"/new", nil); status != 404 {
		t.Fatalf("/new before the exchange exists: %d", status)
	}
	seedExchange(t, e, svc.RecordingID, "GET", "/new", 200, `{"v":"new"}`)
	first := e.virts.last()
	if err := e.mgr.Reload(context.Background(), svc.ID); err != nil {
		t.Fatal(err)
	}
	if e.virts.count() != 2 {
		t.Fatalf("factory called %d times, want 2", e.virts.count())
	}
	if status, _, body := do(t, c, "GET", base+"/new", nil); status != 200 || bodyOf(body) != `{"v":"new"}` {
		t.Errorf("after Reload: %d %s", status, body)
	}
	waitFor(t, "old virtualizer to be closed", first.closed.Load)
	if e.virts.last().closed.Load() {
		t.Error("the new virtualizer was closed")
	}
	if st := e.mgr.Status(svc.ID); st.IndexedExchanges != 2 {
		t.Errorf("IndexedExchanges = %d", st.IndexedExchanges)
	}

	// Failure keeps the current engine.
	e.virts.setFail(errTest)
	if err := e.mgr.Reload(context.Background(), svc.ID); !errors.Is(err, errTest) {
		t.Errorf("Reload = %v", err)
	}
	if status, _, _ := do(t, c, "GET", base+"/new", nil); status != 200 {
		t.Errorf("service broke after a failed Reload: %d", status)
	}

	if err := e.mgr.Reload(context.Background(), "svc_unknown"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("Reload(unknown) = %v, want ErrNotFound", err)
	}
}

func TestReloadIsANoOpWhenModeDoesNotServeRecording(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord)
	e.start(svc)
	if err := e.mgr.Reload(context.Background(), svc.ID); err != nil {
		t.Errorf("Reload = %v", err)
	}
	if e.virts.count() != 0 {
		t.Error("a virtualizer was built for a record-mode service")
	}
}

// blockingVirt blocks Serve until released, to hold a request in flight.
type blockingVirt struct {
	*fakeVirt
	entered          chan struct{}
	release          chan struct{}
	closedAtServeEnd atomic.Bool
}

func (b *blockingVirt) Serve(w http.ResponseWriter, r *http.Request, body []byte) bool {
	b.entered <- struct{}{}
	<-b.release
	b.closedAtServeEnd.Store(b.closed.Load())
	w.WriteHeader(200)
	return true
}

func TestRetiredVirtualizerIsClosedOnlyAfterRequestsFinish(t *testing.T) {
	bv := &blockingVirt{fakeVirt: newFakeVirt(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	var builds atomic.Int32
	e := newEnv(t, func(d *Deps) {
		d.Virtualizers = func(ctx context.Context, svc model.Service, _ *pathnorm.Normalizer) (Virtualizer, error) {
			if builds.Add(1) == 1 {
				return bv, nil
			}
			return newFakeVirt(), nil
		}
	})
	svc := e.service("", model.ModeVirtualize)
	base := e.start(svc)

	done := make(chan int, 1)
	go func() {
		status, _, _ := do(t, newClient(t), "GET", base+"/held", nil)
		done <- status
	}()
	<-bv.entered
	if err := e.mgr.Reload(context.Background(), svc.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	if bv.closed.Load() {
		t.Fatal("the old virtualizer was closed while a request was still using it")
	}
	close(bv.release)
	if status := <-done; status != 200 {
		t.Errorf("held request: %d", status)
	}
	waitFor(t, "retired virtualizer to be closed", bv.closed.Load)
	if bv.closedAtServeEnd.Load() {
		t.Error("closed before Serve returned")
	}
}
