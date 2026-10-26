// Package netutil holds small net/http helpers shared by the packages that
// run HTTP servers.
package netutil

import (
	"net"
	"net/http"
	"sync"
)

// NewConns tracks connections of an http.Server that have been accepted but
// have not carried a request yet (state StateNew).
//
// http.Server.Shutdown closes idle connections at once but waits until a
// StateNew connection is five seconds old before it counts it as idle. Browsers
// and HTTP clients open such connections speculatively, so without help a
// graceful shutdown of a server that had a client a moment ago takes five
// seconds. CloseAll closes exactly those connections; call it right before
// Shutdown. A connection that is about to send its first request is lost,
// which is fine at shutdown: the listener is closed and the client retries
// elsewhere.
type NewConns struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// Track installs a ConnState hook on srv (chaining one that is already set)
// and returns the tracker. Call it before the server starts serving.
func Track(srv *http.Server) *NewConns {
	t := &NewConns{conns: make(map[net.Conn]struct{})}
	prev := srv.ConnState
	srv.ConnState = func(c net.Conn, s http.ConnState) {
		t.mu.Lock()
		if s == http.StateNew {
			t.conns[c] = struct{}{}
		} else {
			delete(t.conns, c)
		}
		t.mu.Unlock()
		if prev != nil {
			prev(c, s)
		}
	}
	return t
}

// CloseAll closes every connection that has not sent a request yet and
// returns how many it closed.
func (t *NewConns) CloseAll() int {
	t.mu.Lock()
	conns := make([]net.Conn, 0, len(t.conns))
	for c := range t.conns {
		conns = append(conns, c)
	}
	clear(t.conns)
	t.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	return len(conns)
}
