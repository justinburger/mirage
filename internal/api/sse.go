package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
)

const (
	// pingInterval is how often idle streams get a comment line, which keeps
	// proxies and browsers from closing them.
	pingInterval = 15 * time.Second
	// clientBuffer is how many events a client may fall behind before it is
	// dropped. Events are one second apart, so this is generous.
	clientBuffer = 16
	// streamWriteTimeout bounds each write to a client. It also overrides a
	// server-wide WriteTimeout, which would otherwise end every stream.
	streamWriteTimeout = 10 * time.Second
	// replayLookupTimeout bounds the lookup of a finished job.
	replayLookupTimeout = 2 * time.Second
)

var pingFrame = []byte(": ping\n\n")

// hub is the single broadcaster behind /api/v1/stream. One goroutine (run)
// subscribes to the metrics registry once, turns every tick into finished SSE
// bytes and offers them to each client's buffered channel. Marshalling happens
// once per tick, however many browsers are watching, and a client that does not
// drain its channel is dropped instead of ever blocking the others.
type hub struct {
	s         *Server
	pingEvery time.Duration

	mu      sync.Mutex
	clients map[*streamClient]struct{}
	closed  bool
}

// streamClient is one connected stream.
type streamClient struct {
	ch chan []byte
}

func newHub(s *Server) *hub {
	return &hub{s: s, pingEvery: pingInterval, clients: make(map[*streamClient]struct{})}
}

// subscribe registers a client. The returned function unregisters it (and may
// be called repeatedly). The channel is closed when the client is dropped for
// being slow or when the hub shuts down, so receivers must treat a closed
// channel as the end of the stream.
func (h *hub) subscribe() (*streamClient, func()) {
	c := &streamClient{ch: make(chan []byte, clientBuffer)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(c.ch)
		return c, func() {}
	}
	h.clients[c] = struct{}{}
	return c, func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
	}
}

// broadcast offers msg to every client without ever blocking. A client whose
// buffer is full is dropped: it is removed and its channel closed, which ends
// its stream.
func (h *hub) broadcast(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.ch <- msg:
		default:
			delete(h.clients, c)
			close(c.ch)
		}
	}
}

// closeAll ends every stream and refuses new ones.
func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for c := range h.clients {
		delete(h.clients, c)
		close(c.ch)
	}
}

// count reports the connected clients (for tests).
func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// run is the broadcaster: it turns every registry update into events for the
// clients. It returns when ctx is done (or updates is closed), after closing
// every stream.
func (h *hub) run(ctx context.Context, updates <-chan metrics.Update) {
	defer h.closeAll()
	ping := time.NewTicker(h.pingEvery)
	defer ping.Stop()

	var prevRunning map[string]struct{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			h.broadcast(pingFrame)
		case u, ok := <-updates:
			if !ok {
				return
			}
			var frame []byte
			frame = h.appendStats(frame, u)
			frame = appendEvent(frame, "services", h.s.servicesJSON())
			frame, prevRunning = h.appendReplays(ctx, frame, prevRunning)
			h.broadcast(frame)
		}
	}
}

// appendStats appends the "stats" event for a registry update.
func (h *hub) appendStats(dst []byte, u metrics.Update) []byte {
	if u.Snapshots == nil {
		u.Snapshots = []model.StatsSnapshot{}
	}
	if u.Points == nil {
		u.Points = map[string]model.SeriesPoint{}
	}
	payload, err := json.Marshal(u)
	if err != nil {
		h.s.log.Error("api: cannot encode stats event", "error", err)
		return dst
	}
	return appendEvent(dst, "stats", payload)
}

// replayEvent is one element of the "replays" event.
type replayEvent struct {
	ID      string               `json:"id"`
	Status  model.ReplayStatus   `json:"status"`
	Summary *model.ReplaySummary `json:"summary"`
}

// appendReplays appends the "replays" event: the running jobs, plus, once, each
// job that has left the running set since the last tick (with its final state).
// It returns the new running set.
func (h *hub) appendReplays(ctx context.Context, dst []byte, prev map[string]struct{}) ([]byte, map[string]struct{}) {
	running := h.s.rpls.Running()
	now := make(map[string]struct{}, len(running))
	events := make([]replayEvent, 0, len(running))
	for _, j := range running {
		now[j.ID] = struct{}{}
		events = append(events, replayEvent{ID: j.ID, Status: j.Status, Summary: j.Summary})
	}
	var finished []string
	for id := range prev {
		if _, still := now[id]; !still {
			finished = append(finished, id)
		}
	}
	slices.Sort(finished)
	for _, id := range finished {
		lctx, cancel := context.WithTimeout(ctx, replayLookupTimeout)
		j, err := h.s.rpls.Get(lctx, id)
		cancel()
		switch {
		case err == nil:
			events = append(events, replayEvent{ID: j.ID, Status: j.Status, Summary: j.Summary})
		case errors.Is(err, model.ErrNotFound):
			// Deleted while running; nothing left to report.
		default:
			h.s.log.Warn("api: cannot look up a finished replay for the stream", "replay", id, "error", err)
		}
	}
	if len(events) == 0 {
		return dst, now
	}
	payload, err := json.Marshal(events)
	if err != nil {
		h.s.log.Error("api: cannot encode replays event", "error", err)
		return dst, now
	}
	return appendEvent(dst, "replays", payload), now
}

// servicesJSON is the payload of the "services" event: all service statuses.
func (s *Server) servicesJSON() []byte {
	list := s.svcs.Statuses()
	statuses := make([]model.ServiceStatus, len(list))
	for i, st := range list {
		statuses[i] = publicStatus(st)
	}
	payload, err := json.Marshal(statuses)
	if err != nil {
		s.log.Error("api: cannot encode services event", "error", err)
		return []byte("[]")
	}
	return payload
}

// appendEvent appends one SSE event. JSON payloads contain no raw newlines, so
// a single data line suffices.
func appendEvent(dst []byte, name string, payload []byte) []byte {
	dst = append(dst, "event: "...)
	dst = append(dst, name...)
	dst = append(dst, "\ndata: "...)
	dst = append(dst, payload...)
	return append(dst, '\n', '\n')
}

// stream answers GET /api/v1/stream with Server-Sent Events: "stats" and
// "services" every second, "replays" while a job runs, and a ping comment
// every 15 seconds. A new client gets "services" at once. The stream ends when
// the client goes away, when the client is too slow, or when the server stops.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) error {
	client, unsubscribe := s.hub.subscribe()
	defer unsubscribe()

	rc := http.NewResponseController(w)
	// A server-wide ReadTimeout would otherwise cut a stream that has long
	// been idle on the read side.
	_ = rc.SetReadDeadline(time.Time{})
	write := func(msg []byte) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
		if _, err := w.Write(msg); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no") // do not let nginx buffer the stream
	w.WriteHeader(http.StatusOK)
	if !write(appendEvent(nil, "services", s.servicesJSON())) {
		return nil
	}
	for {
		select {
		case <-r.Context().Done():
			return nil
		case msg, ok := <-client.ch:
			if !ok || !write(msg) {
				return nil
			}
		}
	}
}
