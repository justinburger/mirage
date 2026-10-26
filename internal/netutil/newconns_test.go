package netutil

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func startServer(t *testing.T, prev func(net.Conn, http.ConnState)) (*http.Server, *NewConns, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }), ConnState: prev}
	nc := Track(srv)
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })
	return srv, nc, ln.Addr().String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestShutdownIsNotDelayedByAConnectionThatSentNothing(t *testing.T) {
	srv, nc, addr := startServer(t, nil)

	idle, err := net.Dial("tcp", addr) // like a browser's speculative connection
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	waitFor(t, "the connection to be tracked", func() bool {
		nc.mu.Lock()
		defer nc.mu.Unlock()
		return len(nc.conns) == 1
	})

	if n := nc.CloseAll(); n != 1 {
		t.Fatalf("CloseAll closed %d connections, want 1", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v (took %v)", err, time.Since(start))
	}
	// The client sees its connection closed.
	idle.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := idle.Read(make([]byte, 1)); err == nil {
		t.Error("the unused connection should have been closed")
	}
}

func TestUsedConnectionsAreNotTracked(t *testing.T) {
	var hookCalls atomic.Int64
	_, nc, addr := startServer(t, func(net.Conn, http.ConnState) { hookCalls.Add(1) })
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	waitFor(t, "the connection to be idle", func() bool {
		nc.mu.Lock()
		defer nc.mu.Unlock()
		return len(nc.conns) == 0
	})
	if n := nc.CloseAll(); n != 0 {
		t.Errorf("CloseAll closed %d connections that had served a request", n)
	}
	// The chained hook still ran.
	if hookCalls.Load() == 0 {
		t.Error("the previous ConnState hook was not called")
	}
}
