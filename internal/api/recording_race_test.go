package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/store"
)

// gateStore wraps a store.Store and runs a hook just before a delete of a
// recording or a write of a service reaches the underlying store, so a test
// can hold one request at exactly the point where the other would interleave.
type gateStore struct {
	store.Store
	onDeleteRecording func(id string)
	onPutService      func(svc model.Service)
}

func (g *gateStore) DeleteRecording(ctx context.Context, id string) error {
	if g.onDeleteRecording != nil {
		g.onDeleteRecording(id)
	}
	return g.Store.DeleteRecording(ctx, id)
}

func (g *gateStore) PutService(ctx context.Context, svc model.Service) (model.Service, error) {
	if g.onPutService != nil {
		g.onPutService(svc)
	}
	return g.Store.PutService(ctx, svc)
}

// reply is the outcome of one HTTP call made from a goroutine.
type reply struct {
	status int
	body   string
	err    error
}

func (r reply) String() string { return fmt.Sprintf("%d %s %v", r.status, r.body, r.err) }

// try performs a JSON request without failing the test, so it is safe to call
// from goroutines other than the test's own.
func (e *env) try(method, path string, in any) reply {
	var rd io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return reply{err: err}
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		return reply{err: err}
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		return reply{err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, body: string(data), err: err}
}

// async runs fn on its own goroutine and delivers its reply on the channel.
func async(fn func() reply) <-chan reply {
	ch := make(chan reply, 1)
	go func() { ch <- fn() }()
	return ch
}

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func awaitReply(t *testing.T, ch <-chan reply, what string) reply {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return reply{}
	}
}

// latch is a one-shot signal. A held request opens one to announce that it has
// arrived and waits on another that the test opens to let it go on. newGate
// also opens the release latch when the test ends, however it ends, so that a
// failing test does not leave a handler blocked and hang the shutdown of the
// test server.
type latch struct {
	once sync.Once
	ch   chan struct{}
}

func newLatch() *latch { return &latch{ch: make(chan struct{})} }

func (l *latch) open() { l.once.Do(func() { close(l.ch) }) }
func (l *latch) wait() { <-l.ch }

func newGate(t *testing.T) (reached, release *latch) {
	t.Helper()
	reached, release = newLatch(), newLatch()
	t.Cleanup(release.open)
	return reached, release
}

// settle is how long a test waits to be sure a request that must be blocked
// has not got through. Only the passing run pays it.
const settle = 250 * time.Millisecond

// gatedEnv builds an environment whose store is wrapped in a gateStore.
func gatedEnv(t *testing.T) (*env, *gateStore) {
	t.Helper()
	gs := &gateStore{}
	e := newEnv(t, func(d *Deps) { gs.Store, d.Store = d.Store, gs })
	return e, gs
}

// An enable that arrives while the delete of its recording is between "nobody
// uses it" and the delete itself must wait for the delete and then be refused,
// instead of enabling a service on a recording that is about to vanish.
func TestEnableWaitsForRecordingDeleteInFlight(t *testing.T) {
	e, gs := gatedEnv(t)
	rec := e.seedRecording("race", 3)
	svc := e.seedService(model.Service{Name: "racer", Mode: model.ModeRecord, RecordingID: rec.ID})

	reached, release := newGate(t)
	gs.onDeleteRecording = func(string) { reached.open(); release.wait() }

	del := async(func() reply { return e.try("DELETE", "/api/v1/recordings/"+rec.ID, nil) })
	awaitSignal(t, reached.ch, "the delete to reach the store")

	enable := async(func() reply { return e.try("POST", "/api/v1/services/"+svc.ID+"/enable", nil) })
	select {
	case r := <-enable:
		t.Fatalf("enable finished (%s) while the delete of its recording was between its check and the delete", r)
	case <-time.After(settle):
	}
	release.open()

	if r := awaitReply(t, del, "the delete"); r.status != http.StatusNoContent {
		t.Fatalf("delete = %s, want 204", r)
	}
	r := awaitReply(t, enable, "the enable")
	if r.status != http.StatusBadRequest || !strings.Contains(r.body, "does not exist") {
		t.Fatalf("enable = %s, want 400 naming the missing recording", r)
	}
	assertNoEnabledServiceOnMissingRecording(t, e)
	if got, _ := e.st.GetService(context.Background(), svc.ID); got.Enabled {
		t.Error("the service is enabled although its enable was refused")
	}
	if _, ok := e.svcs.lastApplied(svc.ID); ok {
		t.Error("the controller was told to run a service on a deleted recording")
	}
}

// A mode change that points a service at a recording is held to the same rule.
func TestModeChangeWaitsForRecordingDeleteInFlight(t *testing.T) {
	e, gs := gatedEnv(t)
	keep := e.seedRecording("keep", 1)
	rec := e.seedRecording("doomed", 3)
	svc := e.seedService(model.Service{Name: "mover", Mode: model.ModeRecord, RecordingID: keep.ID, Enabled: true})

	reached, release := newGate(t)
	gs.onDeleteRecording = func(string) { reached.open(); release.wait() }
	del := async(func() reply { return e.try("DELETE", "/api/v1/recordings/"+rec.ID, nil) })
	awaitSignal(t, reached.ch, "the delete to reach the store")

	mode := async(func() reply {
		return e.try("POST", "/api/v1/services/"+svc.ID+"/mode", map[string]any{"mode": "virtualize", "recording_id": rec.ID})
	})
	select {
	case r := <-mode:
		t.Fatalf("mode change finished (%s) while the delete of its recording was in flight", r)
	case <-time.After(settle):
	}
	release.open()

	if r := awaitReply(t, del, "the delete"); r.status != http.StatusNoContent {
		t.Fatalf("delete = %s, want 204", r)
	}
	if r := awaitReply(t, mode, "the mode change"); r.status != http.StatusBadRequest {
		t.Fatalf("mode = %s, want 400", r)
	}
	assertNoEnabledServiceOnMissingRecording(t, e)
}

// The other order: a service save that has passed its reference check and is
// about to write must finish before a delete of that recording looks for users.
func TestRecordingDeleteWaitsForServiceSaveInFlight(t *testing.T) {
	e, gs := gatedEnv(t)
	rec := e.seedRecording("race", 3)
	svc := e.seedService(model.Service{Name: "racer", Mode: model.ModeRecord, RecordingID: rec.ID})

	reached, release := newGate(t)
	gs.onPutService = func(model.Service) { reached.open(); release.wait() }
	enable := async(func() reply { return e.try("POST", "/api/v1/services/"+svc.ID+"/enable", nil) })
	awaitSignal(t, reached.ch, "the enable to reach the store")

	del := async(func() reply { return e.try("DELETE", "/api/v1/recordings/"+rec.ID, nil) })
	select {
	case r := <-del:
		t.Fatalf("delete finished (%s) while a service was being saved on the recording", r)
	case <-time.After(settle):
	}
	release.open()

	if r := awaitReply(t, enable, "the enable"); r.status != http.StatusOK {
		t.Fatalf("enable = %s, want 200", r)
	}
	r := awaitReply(t, del, "the delete")
	if r.status != http.StatusConflict || !strings.Contains(r.body, "used by the enabled service") {
		t.Fatalf("delete = %s, want 409 (the service is enabled now)", r)
	}
	if _, err := e.st.GetRecording(context.Background(), rec.ID); err != nil {
		t.Fatalf("the recording an enabled service uses is gone: %v", err)
	}
	assertNoEnabledServiceOnMissingRecording(t, e)
}

// Starting a replay is the other thing recordingInUse protects.
func TestRecordingDeleteWaitsForReplayStart(t *testing.T) {
	e, _ := gatedEnv(t)
	rec := e.seedRecording("race", 3)
	job := e.seedReplay("job", rec.ID, model.ReplayPending)

	reached, release := newGate(t)
	e.rpls.onStart = func() { reached.open(); release.wait() }
	start := async(func() reply { return e.try("POST", "/api/v1/replays/"+job.ID+"/start", nil) })
	awaitSignal(t, reached.ch, "the replay to be starting")

	del := async(func() reply { return e.try("DELETE", "/api/v1/recordings/"+rec.ID, nil) })
	select {
	case r := <-del:
		t.Fatalf("delete finished (%s) while a replay on the recording was starting", r)
	case <-time.After(settle):
	}
	release.open()

	if r := awaitReply(t, start, "the replay start"); r.status != http.StatusOK {
		t.Fatalf("start = %s, want 200", r)
	}
	if r := awaitReply(t, del, "the delete"); r.status != http.StatusConflict {
		t.Fatalf("delete = %s, want 409 (a replay is running on the recording)", r)
	}
}

// assertNoEnabledServiceOnMissingRecording is the invariant the lock protects:
// no enabled service in a recording mode points at a recording that is gone.
func assertNoEnabledServiceOnMissingRecording(t testing.TB, e *env) {
	t.Helper()
	ctx := context.Background()
	svcs, err := e.st.ListServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range svcs {
		if !svc.Enabled || !svc.Mode.UsesRecording() {
			continue
		}
		if _, err := e.st.GetRecording(ctx, svc.RecordingID); errors.Is(err, model.ErrNotFound) {
			t.Errorf("service %q (%s) is enabled on the deleted recording %s", svc.Name, svc.ID, svc.RecordingID)
		} else if err != nil {
			t.Fatal(err)
		}
	}
}

// Fifty services race their recordings' deletes at once, each pair released
// together, the service reaching the recording by a different route in
// different rounds (enable, PUT, and the mode shortcut). The outcome of a
// round is one of exactly two consistent ones; an enabled service on a deleted
// recording is never one of them.
func TestEnableVersusDeleteRecordingStress(t *testing.T) {
	const rounds = 50
	e := newEnv(t)
	ctx := context.Background()
	keep := e.seedRecording("keep", 1)

	type round struct {
		rec model.Recording
		svc model.Service
		use func() reply // the call that makes the service depend on rec
	}
	all := make([]round, rounds)
	for i := range all {
		rec := e.seedRecording(fmt.Sprintf("stress-%d", i), 2)
		r := round{rec: rec}
		switch i % 3 {
		case 0: // POST /enable
			r.svc = e.seedService(model.Service{Name: fmt.Sprintf("stress-%d", i), Mode: model.ModeVirtualize, RecordingID: rec.ID})
			r.use = func() reply { return e.try("POST", "/api/v1/services/"+r.svc.ID+"/enable", nil) }
		case 1: // PUT with enabled: true
			r.svc = e.seedService(model.Service{Name: fmt.Sprintf("stress-%d", i), Mode: model.ModeRecord, RecordingID: rec.ID})
			full := r.svc.WithDefaults()
			full.Enabled = true
			r.use = func() reply { return e.try("PUT", "/api/v1/services/"+r.svc.ID, full) }
		case 2: // POST /mode re-pointing an enabled service at rec
			r.svc = e.seedService(model.Service{Name: fmt.Sprintf("stress-%d", i), Mode: model.ModeHybrid, RecordingID: keep.ID, Enabled: true})
			r.use = func() reply {
				return e.try("POST", "/api/v1/services/"+r.svc.ID+"/mode", map[string]any{"mode": "virtualize", "recording_id": rec.ID})
			}
		}
		all[i] = r
	}

	start := make(chan struct{})
	use, del := make([]reply, rounds), make([]reply, rounds)
	var wg sync.WaitGroup
	for i := range all {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			use[i] = all[i].use()
		}()
		go func() {
			defer wg.Done()
			<-start
			del[i] = e.try("DELETE", "/api/v1/recordings/"+all[i].rec.ID, nil)
		}()
	}
	close(start)
	wg.Wait()

	deleted := 0
	for i, r := range all {
		if use[i].err != nil || del[i].err != nil {
			t.Fatalf("round %d: transport errors: %v / %v", i, use[i].err, del[i].err)
		}
		_, recErr := e.st.GetRecording(ctx, r.rec.ID)
		gone := errors.Is(recErr, model.ErrNotFound)
		if recErr != nil && !gone {
			t.Fatal(recErr)
		}
		svc, err := e.st.GetService(ctx, r.svc.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case gone:
			deleted++
			if del[i].status != http.StatusNoContent || use[i].status != http.StatusBadRequest {
				t.Errorf("round %d: recording deleted but delete = %s, use = %s; want 204 and 400", i, del[i], use[i])
			}
			if svc.RecordingID == r.rec.ID && svc.Enabled {
				t.Errorf("round %d: the service is enabled on the deleted recording", i)
			}
		default:
			if del[i].status != http.StatusConflict || use[i].status != http.StatusOK || !svc.Enabled || svc.RecordingID != r.rec.ID {
				t.Errorf("round %d: recording kept but delete = %s, use = %s, service = enabled %v on %s; want 409, 200, enabled on the recording",
					i, del[i], use[i], svc.Enabled, svc.RecordingID)
			}
		}
	}
	t.Logf("%d of %d recordings were deleted, the rest were claimed first", deleted, rounds)
	assertNoEnabledServiceOnMissingRecording(t, e)
}

// A failure of the capture pipeline reaches the status through the controller;
// the text the API shows names no internal packages.
func TestServiceStatusErrorHasNoInternalPrefixes(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("rec", 0)
	svc := e.seedService(model.Service{Name: "leaky", Mode: model.ModeRecord, RecordingID: rec.ID, Enabled: true})
	e.svcs.setStatus(model.ServiceStatus{
		ServiceID: svc.ID, Mode: model.ModeRecord, Running: true,
		Error: fmt.Sprintf(`capture: store: store: recording %q: not found`, rec.ID),
	})

	check := func(what, msg string) {
		t.Helper()
		if strings.Contains(msg, "store:") || msg == "" {
			t.Errorf("%s: status.error = %q, want no internal prefix", what, msg)
		}
		if !strings.Contains(msg, rec.ID) || !strings.Contains(msg, "not found") {
			t.Errorf("%s: status.error = %q lost the detail", what, msg)
		}
	}
	var got ServiceWithStatus
	e.call("GET", "/api/v1/services/"+svc.ID, nil, http.StatusOK, &got)
	check("get", got.Status.Error)
	var list []ServiceWithStatus
	e.call("GET", "/api/v1/services", nil, http.StatusOK, &list)
	if len(list) != 1 {
		t.Fatalf("list = %d services", len(list))
	}
	check("list", list[0].Status.Error)
	check("servicesJSON", func() string {
		var sts []model.ServiceStatus
		if err := json.Unmarshal(e.srv.servicesJSON(), &sts); err != nil || len(sts) != 1 {
			t.Fatalf("servicesJSON: %v %d", err, len(sts))
		}
		return sts[0].Error
	}())

	// The error of a failed Apply, when the controller recorded none, as well.
	e.svcs.applyErr, e.svcs.silentFail = fmt.Errorf("proxy: store: apply %q: %w", svc.ID, errors.New("listen tcp :1: bind: address already in use")), true
	var out ServiceWithStatus
	e.call("POST", "/api/v1/services/"+svc.ID+"/mode", map[string]any{"mode": "record"}, http.StatusOK, &out)
	if strings.Contains(out.Status.Error, "store:") || strings.Contains(out.Status.Error, "proxy:") ||
		!strings.Contains(out.Status.Error, "address already in use") {
		t.Errorf("status.error after a failed apply = %q", out.Status.Error)
	}
}
