package netutil

import (
	"net"
	"net/http"
	"testing"
)

func BenchmarkTrackActiveIdle(b *testing.B) {
	srv := &http.Server{}
	Track(srv)
	c, _ := net.Pipe()
	defer c.Close()
	srv.ConnState(c, http.StateNew)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			srv.ConnState(c, http.StateActive)
			srv.ConnState(c, http.StateIdle)
		}
	})
}
