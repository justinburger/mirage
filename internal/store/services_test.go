package store

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func TestListenKey(t *testing.T) {
	cases := []struct {
		addr    string
		key     string
		checked bool
	}{
		{":9001", ":9001", true},
		{"0.0.0.0:9001", ":9001", true},
		{"[::]:9001", ":9001", true},
		{"[0:0:0:0:0:0:0:0]:9001", ":9001", true},
		{"[::ffff:0.0.0.0]:9001", ":9001", true},
		{" :9001 ", ":9001", true},
		{":09001", ":9001", true},
		{"127.0.0.1:9001", "127.0.0.1:9001", true},
		{"[::ffff:127.0.0.1]:9001", "127.0.0.1:9001", true},
		{"[::1]:9001", "[::1]:9001", true},
		{"[0:0:0:0:0:0:0:1]:9001", "[::1]:9001", true},
		{"LocalHost:9001", "localhost:9001", true},
		{":http", ":http", true}, // named ports are compared as written
		{"not an address", "not an address", true},
		{"", "", true},
		{":0", "", false},
		{":00", "", false},
		{"127.0.0.1:0", "", false},
		{"0.0.0.0:0", "", false},
		{"[::]:0", "", false},
		{"localhost:0", "", false},
	}
	for _, c := range cases {
		t.Run(c.addr, func(t *testing.T) {
			key, checked := listenKey(c.addr)
			if checked != c.checked || (checked && key != c.key) {
				t.Errorf("listenKey(%q) = %q, %v; want %q, %v", c.addr, key, checked, c.key, c.checked)
			}
		})
	}
}

func TestServiceListenAddrUniqueness(t *testing.T) {
	// conflict: the first put of addr is refused. Every address but port 0
	// conflicts on a second put, because the first one holds the socket.
	cases := []struct {
		addr     string
		conflict bool
		why      string
	}{
		// The same socket in other spellings.
		{":9001", true, "identical"},
		{"0.0.0.0:9001", true, "IPv4 wildcard"},
		{"[::]:9001", true, "IPv6 wildcard"},
		{"[0:0:0:0:0:0:0:0]:9001", true, "IPv6 wildcard, long form"},
		{":09001", true, "leading zero in the port"},
		{" :9001 ", true, "white space"},
		{"127.0.0.1:9002", true, "identical host"},
		{"[::ffff:127.0.0.1]:9002", true, "IPv4-mapped IPv6"},
		{"[0:0:0:0:0:0:0:1]:9003", true, "IPv6 long form"},
		{"EXAMPLE.test:9004", true, "host name case"},
		// Different sockets. A wildcard and a specific host on one port, or
		// "localhost" and 127.0.0.1, are left to the OS to refuse at listen
		// time: they are only the same socket on some systems.
		{":9005", false, "free port"},
		{"127.0.0.1:9001", false, "specific host next to a wildcard"},
		{"127.0.0.2:9002", false, "other host, same port"},
		{"localhost:9002", false, "host names are not resolved"},
		{"[::1]:9002", false, "other host, same port (IPv6)"},
		// Port 0 asks for a free port every time and never conflicts.
		{":0", false, "port 0"},
		{"127.0.0.1:0", false, "port 0 with a host"},
		{"0.0.0.0:0", false, "port 0, IPv4 wildcard"},
		{"[::]:0", false, "port 0, IPv6 wildcard"},
		{":00", false, "port 0 spelled 00"},
	}
	backends(t, func(t *testing.T, s *SQLite) {
		var a model.Service
		for i, addr := range []string{":9001", "127.0.0.1:9002", "[::1]:9003", "example.test:9004"} {
			svc, err := s.PutService(ctx(), model.Service{Name: fmt.Sprintf("base%d", i), ListenAddr: addr})
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				a = svc
			}
		}

		for i, c := range cases {
			t.Run(c.why, func(t *testing.T) {
				_, port, _ := net.SplitHostPort(strings.TrimSpace(c.addr))
				portZero := port == "0" || port == "00"
				for round := range 2 {
					wantConflict := c.conflict || (round == 1 && !portZero)
					_, err := s.PutService(ctx(), model.Service{Name: fmt.Sprintf("case%d-%d", i, round), ListenAddr: c.addr})
					if !wantConflict {
						if err != nil {
							t.Fatalf("round %d: PutService(%q): %v", round, c.addr, err)
						}
						continue
					}
					if !errors.Is(err, model.ErrConflict) {
						t.Fatalf("round %d: PutService(%q) = %v, want ErrConflict", round, c.addr, err)
					}
					if !strings.Contains(err.Error(), "listen address") {
						t.Errorf("error %q does not mention the listen address", err)
					}
				}
			})
		}

		before := countRows(t, s, "services")

		// Updates: an unchanged address and a respelling of one's own are fine; taking
		// another service's is not, however it is spelled.
		for _, addr := range []string{":9001", "0.0.0.0:9001", ":0"} {
			upd := a
			upd.ListenAddr = addr
			if _, err := s.PutService(ctx(), upd); err != nil {
				t.Errorf("service re-put with its own address %q: %v", addr, err)
			}
		}
		upd := a
		upd.ListenAddr = "0.0.0.0:9002" // not base1's "127.0.0.1:9002": distinct
		if _, err := s.PutService(ctx(), upd); err != nil {
			t.Errorf("move to a free socket: %v", err)
		}
		upd.ListenAddr = "[::ffff:127.0.0.1]:9002"
		if _, err := s.PutService(ctx(), upd); !errors.Is(err, model.ErrConflict) {
			t.Errorf("move onto another service's socket: %v, want ErrConflict", err)
		}
		if got, _ := s.GetService(ctx(), a.ID); got.ListenAddr != "0.0.0.0:9002" {
			t.Errorf("refused move modified the service: %q", got.ListenAddr)
		}

		// A deleted service frees its socket; a port-0 service never held one.
		if err := s.DeleteService(ctx(), a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutService(ctx(), model.Service{Name: "reuse", ListenAddr: "0.0.0.0:9002"}); err != nil {
			t.Errorf("address of a deleted service is still taken: %v", err)
		}
		if n := countRows(t, s, "services"); n != before {
			t.Errorf("%d services, want %d", n, before) // one deleted, one added
		}
	})
}

// TestServiceNameStillUnique: dropping the listen_addr constraint did not drop
// the name's, which is a database constraint, including for port-0 services.
func TestServiceNameStillUnique(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		if _, err := s.PutService(ctx(), model.Service{Name: "api", ListenAddr: ":0"}); err != nil {
			t.Fatal(err)
		}
		other, err := s.PutService(ctx(), model.Service{Name: "other", ListenAddr: "127.0.0.1:0"})
		if err != nil {
			t.Fatal(err)
		}
		for _, svc := range []model.Service{
			{Name: "api", ListenAddr: ":0"},
			{Name: "api", ListenAddr: ":9100"},
			{ID: other.ID, Name: "api", ListenAddr: ":0"},
		} {
			_, err := s.PutService(ctx(), svc)
			if !errors.Is(err, model.ErrConflict) || !strings.Contains(err.Error(), "name") {
				t.Errorf("PutService(%+v) = %v, want a name ErrConflict", svc, err)
			}
		}
		if got, _ := s.GetService(ctx(), other.ID); got.Name != "other" {
			t.Errorf("refused rename modified the service: %q", got.Name)
		}
	})
}

// TestConcurrentListenAddrRace: of many simultaneous puts of one socket (in
// different spellings) exactly one wins.
func TestConcurrentListenAddrRace(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		spellings := []string{":9300", "0.0.0.0:9300", "[::]:9300"}
		const n = 12
		var (
			wg  sync.WaitGroup
			mu  sync.Mutex
			won int
		)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.PutService(ctx(), model.Service{Name: fmt.Sprintf("racer%d", i), ListenAddr: spellings[i%len(spellings)]})
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					won++
				case !errors.Is(err, model.ErrConflict):
					t.Errorf("put %d: %v", i, err)
				}
			}()
		}
		wg.Wait()
		if won != 1 {
			t.Errorf("%d concurrent puts of one socket succeeded, want 1", won)
		}
	})
}
