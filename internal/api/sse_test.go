package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
)

// sseEvent is one parsed block of the stream: a named event with its data, or
// a comment line.
type sseEvent struct {
	Name    string
	Data    string
	Comment string
}

// sseStream is a live connection to /api/v1/stream read by a real HTTP client.
type sseStream struct {
	t      testing.TB
	resp   *http.Response
	events chan sseEvent
	done   chan struct{} // closed when the reader goroutine ends
	cancel context.CancelFunc
}

// openStream connects a client that uses its own transport, so closing the
// stream really closes the TCP connection.
func (e *env) openStream(path string, header map[string]string) (*sseStream, *http.Response) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", e.ts.URL+path, nil)
	if err != nil {
		cancel()
		e.t.Fatal(err)
	}
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		e.t.Fatalf("GET %s: %v", path, err)
	}
	s := &sseStream{t: e.t, resp: resp, events: make(chan sseEvent, 256), done: make(chan struct{}), cancel: cancel}
	if resp.StatusCode != http.StatusOK {
		cancel()
		close(s.done)
		return s, resp
	}
	go s.read()
	e.t.Cleanup(s.close)
	return s, resp
}

func (e *env) stream() *sseStream {
	e.t.Helper()
	s, resp := e.openStream("/api/v1/stream", nil)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("stream status %d", resp.StatusCode)
	}
	return s
}

func (s *sseStream) read() {
	defer close(s.done)
	defer close(s.events)
	br := bufio.NewReader(s.resp.Body)
	var cur sseEvent
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if cur.Name != "" || cur.Data != "" || cur.Comment != "" {
				s.events <- cur
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, ":"):
			cur.Comment = strings.TrimSpace(line[1:])
		case strings.HasPrefix(line, "event: "):
			cur.Name = line[len("event: "):]
		case strings.HasPrefix(line, "data: "):
			cur.Data += line[len("data: "):]
		}
	}
}

// next returns the next event or fails the test after timeout.
func (s *sseStream) next(timeout time.Duration) sseEvent {
	s.t.Helper()
	select {
	case ev, ok := <-s.events:
		if !ok {
			s.t.Fatal("the stream ended while an event was expected")
		}
		return ev
	case <-time.After(timeout):
		s.t.Fatal("timed out waiting for an event")
	}
	return sseEvent{}
}

// expect returns the next event, which must be named name.
func (s *sseStream) expect(name string) sseEvent {
	s.t.Helper()
	ev := s.next(5 * time.Second)
	if ev.Name != name {
		s.t.Fatalf("got event %q (%q), want %q", ev.Name, ev.Data, name)
	}
	return ev
}

// ended waits for the server to end the stream.
func (s *sseStream) ended(timeout time.Duration) bool {
	for {
		select {
		case _, ok := <-s.events:
			if !ok {
				return true
			}
		case <-time.After(timeout):
			return false
		}
	}
}

func (s *sseStream) close() {
	s.cancel()
	_ = s.resp.Body.Close()
	<-s.done
}

// waitFor polls cond for up to 5 seconds.
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestStreamHeadersAndInitialServices(t *testing.T) {
	e := newEnv(t) // note: Run is not started
	var s ServiceWithStatus
	e.call("POST", "/api/v1/services", validService("first", ":0"), http.StatusCreated, &s)

	stream, resp := e.openStream("/api/v1/stream", nil)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q", cc)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("security headers are missing on the stream")
	}
	ev := stream.expect("services")
	var statuses []model.ServiceStatus
	if err := json.Unmarshal([]byte(ev.Data), &statuses); err != nil {
		t.Fatalf("services data %q: %v", ev.Data, err)
	}
	if len(statuses) != 1 || statuses[0].ServiceID != s.ID {
		t.Errorf("statuses = %+v", statuses)
	}
}

func TestStreamInitialServicesIsAnEmptyArray(t *testing.T) {
	e := newEnv(t)
	ev := e.stream().expect("services")
	if ev.Data != "[]" {
		t.Errorf("data = %q, want []", ev.Data)
	}
}

func TestStreamStatsEveryTick(t *testing.T) {
	e := newEnv(t)
	e.run()
	var svc ServiceWithStatus
	e.call("POST", "/api/v1/services", validService("busy", ":0"), http.StatusCreated, &svc)
	col := e.reg.Collector(metrics.ServiceCollectorName(svc.ID), model.KindService, svc.ID)

	stream := e.stream()
	stream.expect("services") // initial; the stream is subscribed from here on

	col.Observe(metrics.Observation{Endpoint: "GET /a", Status: 200, Duration: 5 * time.Millisecond})
	e.reg.Tick(time.Now())

	ev := stream.expect("stats")
	var stats struct {
		Time      time.Time                    `json:"time"`
		Snapshots []model.StatsSnapshot        `json:"snapshots"`
		Points    map[string]model.SeriesPoint `json:"points"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &stats); err != nil {
		t.Fatalf("stats data %q: %v", ev.Data, err)
	}
	if stats.Time.IsZero() || len(stats.Snapshots) != 1 || stats.Snapshots[0].Requests != 1 {
		t.Errorf("stats = %+v", stats)
	}
	if _, ok := stats.Points["service:"+svc.ID]; !ok {
		t.Errorf("points = %v", stats.Points)
	}
	stream.expect("services") // every tick

	// With no collectors the arrays and objects are still present.
	e2 := newEnv(t)
	e2.run()
	s2 := e2.stream()
	s2.expect("services")
	e2.reg.Tick(time.Now())
	ev = s2.expect("stats")
	if !strings.Contains(ev.Data, `"snapshots":[]`) || !strings.Contains(ev.Data, `"points":{}`) {
		t.Errorf("empty stats = %s", ev.Data)
	}
}

func TestStreamServicesEventReflectsChanges(t *testing.T) {
	e := newEnv(t)
	e.run()
	var svc ServiceWithStatus
	e.call("POST", "/api/v1/services", validService("watched", ":0"), http.StatusCreated, &svc)
	stream := e.stream()
	stream.expect("services")

	read := func() model.ServiceStatus {
		t.Helper()
		e.reg.Tick(time.Now())
		stream.expect("stats")
		var statuses []model.ServiceStatus
		if err := json.Unmarshal([]byte(stream.expect("services").Data), &statuses); err != nil || len(statuses) != 1 {
			t.Fatalf("statuses = %v (%v)", statuses, err)
		}
		return statuses[0]
	}
	if st := read(); st.Captured != 0 || st.Running {
		t.Errorf("before: %+v", st)
	}
	e.svcs.setStatus(model.ServiceStatus{ServiceID: svc.ID, Running: true, Captured: 77, Mode: model.ModeRecord})
	if st := read(); st.Captured != 77 || !st.Running {
		t.Errorf("after the change: %+v", st)
	}
}

func TestStreamReplayEvents(t *testing.T) {
	e := newEnv(t)
	e.run()
	rec := e.seedRecording("r", 1)
	var job model.ReplayJob
	e.call("POST", "/api/v1/replays", replayRequest(rec.ID), http.StatusCreated, &job)

	stream := e.stream()
	stream.expect("services")
	tick := func() {
		e.reg.Tick(time.Now())
		stream.expect("stats")
		stream.expect("services")
	}

	// Nothing running: no replays event.
	tick()

	e.call("POST", "/api/v1/replays/"+job.ID+"/start", nil, http.StatusOK, nil)
	tick()
	var evs []replayEvent
	if err := json.Unmarshal([]byte(stream.expect("replays").Data), &evs); err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].ID != job.ID || evs[0].Status != model.ReplayRunning || evs[0].Summary == nil || evs[0].Summary.Sent != 10 {
		t.Errorf("running event = %+v", evs)
	}
	tick()
	stream.expect("replays") // every second while it runs

	// The job finishes: one more event with its final state, then silence.
	e.rpls.finish(job.ID, model.ReplayCompleted, &model.ReplaySummary{Planned: 100, Sent: 100, Completed: 100})
	tick()
	evs = nil
	if err := json.Unmarshal([]byte(stream.expect("replays").Data), &evs); err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].ID != job.ID || evs[0].Status != model.ReplayCompleted || evs[0].Summary == nil || evs[0].Summary.Completed != 100 {
		t.Errorf("final event = %+v", evs)
	}
	tick() // no replays event this time (tick() would fail on an unexpected one)
	tick()
}

func TestStreamReplayDeletedWhileRunningIsNotReported(t *testing.T) {
	e := newEnv(t)
	e.run()
	rec := e.seedRecording("r", 1)
	var job model.ReplayJob
	body := replayRequest(rec.ID)
	body["start"] = true
	e.call("POST", "/api/v1/replays", body, http.StatusCreated, &job)

	stream := e.stream()
	stream.expect("services")
	e.reg.Tick(time.Now())
	stream.expect("stats")
	stream.expect("services")
	stream.expect("replays")

	e.call("DELETE", "/api/v1/replays/"+job.ID, nil, http.StatusNoContent, nil)
	e.reg.Tick(time.Now())
	stream.expect("stats")
	stream.expect("services")
	// The next tick starts with stats again: no replays event for the deleted job.
	e.reg.Tick(time.Now())
	stream.expect("stats")
}

func TestStreamHeartbeat(t *testing.T) {
	e := newEnv(t)
	e.srv.hub.pingEvery = 20 * time.Millisecond
	e.run()
	stream := e.stream()
	stream.expect("services")
	for i := 0; i < 3; i++ {
		ev := stream.next(5 * time.Second)
		if ev.Comment != "ping" || ev.Name != "" {
			t.Fatalf("event = %+v, want a ping comment", ev)
		}
	}
}

func TestStreamEveryClientGetsTheSameBytes(t *testing.T) {
	e := newEnv(t)
	e.run()
	col := e.reg.Collector("service:x", model.KindService, "x")
	const clients = 6
	streams := make([]*sseStream, clients)
	for i := range streams {
		streams[i] = e.stream()
		streams[i].expect("services")
	}
	waitFor(t, "all clients to register", func() bool { return e.srv.hub.count() == clients })
	if n := e.reg.Subscribers(); n != 1 {
		t.Errorf("the registry has %d subscribers, want the broadcaster alone", n)
	}

	col.Observe(metrics.Observation{Endpoint: "GET /", Status: 200, Duration: time.Millisecond})
	e.reg.Tick(time.Now())
	first := streams[0].expect("stats").Data
	for i := 1; i < clients; i++ {
		if got := streams[i].expect("stats").Data; got != first {
			t.Errorf("client %d got different bytes", i)
		}
	}
}

func TestStreamClientDisconnect(t *testing.T) {
	e := newEnv(t)
	e.run()
	stream := e.stream()
	stream.expect("services")
	waitFor(t, "the client to register", func() bool { return e.srv.hub.count() == 1 })

	stream.close()
	waitFor(t, "the server to notice the disconnect", func() bool { return e.srv.hub.count() == 0 })

	// Ticks with nobody listening are harmless.
	e.reg.Tick(time.Now())
}

func TestStreamDoesNotLeakGoroutines(t *testing.T) {
	e := newEnv(t)
	cancel := e.run()
	// Warm up the server and runtime so their lazily started goroutines exist.
	e.call("GET", "/api/v1/health", nil, http.StatusOK, nil)
	s := e.stream()
	s.expect("services")
	s.close()
	waitFor(t, "warm-up client to leave", func() bool { return e.srv.hub.count() == 0 })
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	for round := 0; round < 3; round++ {
		var streams []*sseStream
		for i := 0; i < 10; i++ {
			s := e.stream()
			s.expect("services")
			streams = append(streams, s)
		}
		e.reg.Tick(time.Now())
		for _, s := range streams {
			s.expect("stats")
		}
		for _, s := range streams {
			s.close()
		}
	}
	waitFor(t, "all clients to leave", func() bool { return e.srv.hub.count() == 0 })
	e.ts.CloseClientConnections()
	waitFor(t, "goroutines to settle", func() bool { return runtime.NumGoroutine() <= base+2 })

	// Stopping the broadcaster leaves nothing behind either.
	cancel()
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	if !e.srv.WaitContext(wctx) {
		t.Fatal("broadcaster still running after cancel")
	}
	waitFor(t, "goroutines to settle after shutdown", func() bool { return runtime.NumGoroutine() <= base })
	if n := e.reg.Subscribers(); n != 0 {
		t.Errorf("registry still has %d subscribers after shutdown", n)
	}
}

func TestStreamEndsWhenTheServerStops(t *testing.T) {
	e := newEnv(t)
	cancel := e.run()
	stream := e.stream()
	stream.expect("services")
	cancel()
	if !stream.ended(5 * time.Second) {
		t.Fatal("the stream did not end after the server context was cancelled")
	}
	// A client arriving afterwards still gets the initial event, then the end.
	late := e.stream()
	late.expect("services")
	if !late.ended(5 * time.Second) {
		t.Error("a late stream stayed open on a stopped server")
	}
}

func TestRunIsIdempotentAndWaitWithoutRun(t *testing.T) {
	e := newEnv(t)
	e.srv.Wait() // never started: returns at once
	ctx, cancel := context.WithCancel(context.Background())
	e.srv.Run(ctx)
	e.srv.Run(ctx) // second call is a no-op
	e.srv.Run(context.Background())
	// Subscribed by the time Run returns, so a tick right after is not missed.
	if n := e.reg.Subscribers(); n != 1 {
		t.Errorf("%d registry subscribers right after Run, want 1", n)
	}
	cancel()
	e.srv.Wait()
	if e.reg.Subscribers() != 0 {
		t.Error("the broadcaster did not unsubscribe")
	}
}

func TestStreamRequiresAuthentication(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.AdminToken = "s3cr3t-token" })
	e.run()
	if _, resp := e.openStream("/api/v1/stream", map[string]string{"Authorization": "Bearer wrong"}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: status %d", resp.StatusCode)
	}
	// EventSource cannot set headers: the query parameter and the cookie work.
	e.token = ""
	s, resp := e.openStream("/api/v1/stream?access_token=s3cr3t-token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("access_token: status %d", resp.StatusCode)
	}
	s.expect("services")
	s2, resp := e.openStream("/api/v1/stream", map[string]string{"Cookie": "mirage_token=s3cr3t-token"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cookie: status %d", resp.StatusCode)
	}
	s2.expect("services")
}

// ------------------------------------------------------------- hub unit tests

func TestHubDropsSlowClientsWithoutBlocking(t *testing.T) {
	h := newHub(newEnv(t).srv)
	slow, _ := h.subscribe() // never reads
	fastA, unsubA := h.subscribe()
	fastB, unsubB := h.subscribe()
	defer unsubA()
	defer unsubB()

	var got atomic.Int64
	var wg sync.WaitGroup
	for _, c := range []*streamClient{fastA, fastB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range c.ch {
				got.Add(1)
			}
		}()
	}

	const sends = clientBuffer * 4
	finished := make(chan struct{})
	go func() {
		for i := 0; i < sends; i++ {
			h.broadcast([]byte("x"))
			time.Sleep(50 * time.Microsecond) // let the fast readers keep up
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("broadcast blocked on a client that does not read")
	}

	// The slow client was dropped: its channel holds what fit, then closes.
	n := 0
	for range slow.ch {
		n++
	}
	if n != clientBuffer {
		t.Errorf("slow client received %d events before being dropped, want %d", n, clientBuffer)
	}
	if h.count() != 2 {
		t.Errorf("%d clients left, want the two fast ones", h.count())
	}
	h.closeAll()
	wg.Wait()
	if got.Load() < sends { // two fast clients, each should have seen nearly all
		t.Errorf("fast clients received only %d events", got.Load())
	}
}

func TestHubSubscribeAfterCloseAndRepeatedUnsubscribe(t *testing.T) {
	h := newHub(newEnv(t).srv)
	c, unsub := h.subscribe()
	unsub()
	unsub() // idempotent
	h.broadcast([]byte("nobody listens"))
	select {
	case <-c.ch:
		t.Error("an unsubscribed client received an event")
	default:
	}
	h.closeAll()
	late, lateUnsub := h.subscribe()
	if _, ok := <-late.ch; ok {
		t.Error("a client of a closed hub must get a closed channel")
	}
	lateUnsub()
	h.closeAll() // idempotent
}

func TestHubConcurrentSubscribeBroadcastClose(t *testing.T) {
	h := newHub(newEnv(t).srv)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c, unsub := h.subscribe()
				select {
				case <-c.ch:
				default:
				}
				unsub()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 2000; j++ {
			h.broadcast([]byte("tick"))
		}
	}()
	wg.Wait()
	h.closeAll()
}

func TestAppendEvent(t *testing.T) {
	got := string(appendEvent([]byte("x"), "stats", []byte(`{"a":1}`)))
	if got != "xevent: stats\ndata: {\"a\":1}\n\n" {
		t.Errorf("appendEvent = %q", got)
	}
}

// A payload with newlines in strings must stay on one data line: encoding/json
// escapes them, so SSE framing cannot be broken by recorded content.
func TestStreamPayloadsNeverContainRawNewlines(t *testing.T) {
	e := newEnv(t)
	e.run()
	col := e.reg.Collector("service:nl", model.KindService, "nl")
	col.Observe(metrics.Observation{Endpoint: "GET /a\nevent: evil\ndata: x", Status: 200, Duration: time.Millisecond})
	stream := e.stream()
	stream.expect("services")
	e.reg.Tick(time.Now())
	ev := stream.expect("stats")
	if strings.ContainsAny(ev.Data, "\n\r") || !strings.Contains(ev.Data, `\nevent: evil`) {
		t.Errorf("data = %q", ev.Data)
	}
	if next := stream.expect("services"); next.Name != "services" {
		t.Errorf("injected event: %+v", next)
	}
}

// A server-wide ReadTimeout or WriteTimeout must not cut live streams: the
// handler manages its own deadlines.
func TestStreamSurvivesServerTimeouts(t *testing.T) {
	e := newEnv(t)
	e.srv.hub.pingEvery = 30 * time.Millisecond
	e.run()

	ts := httptest.NewUnstartedServer(e.srv.Handler())
	ts.Config.ReadTimeout = 150 * time.Millisecond
	ts.Config.WriteTimeout = 150 * time.Millisecond
	ts.Start()
	defer ts.Close()

	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Get(ts.URL + "/api/v1/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	s := &sseStream{t: t, resp: resp, events: make(chan sseEvent, 256), done: make(chan struct{}), cancel: func() {}}
	go s.read()
	s.expect("services")
	deadline := time.Now().Add(700 * time.Millisecond) // several multiples of both timeouts
	pings := 0
	for time.Now().Before(deadline) {
		if ev := s.next(2 * time.Second); ev.Comment == "ping" {
			pings++
		}
	}
	if pings < 10 {
		t.Errorf("only %d pings arrived over 700ms", pings)
	}
}
