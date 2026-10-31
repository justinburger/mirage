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

// TestServiceNamesAreUniqueIgnoringCase: like profile names, service names are
// compared ignoring case and surrounding white space, not byte for byte.
func TestServiceNamesAreUniqueIgnoringCase(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		api, err := s.PutService(ctx(), model.Service{Name: "API", ListenAddr: ":0"})
		if err != nil {
			t.Fatal(err)
		}
		cafe, err := s.PutService(ctx(), model.Service{Name: "Ünï Café", ListenAddr: ":0"})
		if err != nil {
			t.Fatal(err)
		}
		names := func() []string {
			list, err := s.ListServices(ctx())
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, svc := range list {
				out = append(out, svc.Name)
			}
			return out
		}

		cases := []struct {
			name     string
			svc      model.Service
			conflict bool
		}{
			{"lower case", model.Service{Name: "api"}, true},
			{"mixed case", model.Service{Name: "Api"}, true},
			{"padded with white space", model.Service{Name: "  api\t"}, true},
			{"non-ASCII letters", model.Service{Name: "üNÏ cAFÉ"}, true},
			{"rename onto another service, other case", model.Service{ID: cafe.ID, Name: "aPi"}, true},
			{"different name", model.Service{Name: "API 2"}, false},
			{"prefix of another name", model.Service{Name: "AP"}, false},
			{"own name in another case", model.Service{ID: api.ID, Name: "api"}, false},
			{"own name unchanged", model.Service{ID: api.ID, Name: "API"}, false},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				c.svc.ListenAddr = ":0"
				snapshot := names()
				_, err := s.PutService(ctx(), c.svc)
				if c.conflict {
					if !errors.Is(err, model.ErrConflict) {
						t.Fatalf("err = %v, want ErrConflict", err)
					}
					if !strings.Contains(err.Error(), "name") {
						t.Errorf("error %q does not mention the name", err)
					}
					if got := names(); fmt.Sprint(got) != fmt.Sprint(snapshot) {
						t.Errorf("a refused put changed the services: %v -> %v", snapshot, got)
					}
					return
				}
				if err != nil {
					t.Fatalf("err = %v, want success", err)
				}
			})
		}

		// A refused rename leaves the service as it was; a delete frees the name.
		if got, _ := s.GetService(ctx(), cafe.ID); got.Name != "Ünï Café" {
			t.Errorf("refused rename modified the stored name: %q", got.Name)
		}
		if err := s.DeleteService(ctx(), cafe.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutService(ctx(), model.Service{Name: "ünï café", ListenAddr: ":0"}); err != nil {
			t.Errorf("name of a deleted service is still taken: %v", err)
		}
	})
}

// TestConcurrentServiceNameRace: of many simultaneous puts of one name (in
// different cases) exactly one wins.
func TestConcurrentServiceNameRace(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
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
				name := "Contested"
				if i%2 == 1 {
					name = "CONTESTED"
				}
				_, err := s.PutService(ctx(), model.Service{Name: name, ListenAddr: ":0"})
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
			t.Errorf("%d concurrent puts of one name succeeded, want 1", won)
		}
	})
}

// A database written before names were compared ignoring case may hold two that
// differ only in case. It still opens and lists both; one of them has to be
// renamed before it can be saved again.
func TestServiceNamesDifferingInCaseInAnOlderDatabase(t *testing.T) {
	s := openMem(t)
	a, err := s.PutService(ctx(), model.Service{Name: "api", ListenAddr: ":0"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.PutService(ctx(), model.Service{Name: "other", ListenAddr: ":0"})
	if err != nil {
		t.Fatal(err)
	}
	// The older release's UNIQUE constraint compared bytes, so this was allowed.
	if _, err := s.wdb.Exec(`UPDATE services SET name = 'API', data = json_set(data, '$.name', 'API') WHERE id = ?`, b.ID); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListServices(ctx())
	if err != nil || len(list) != 2 {
		t.Fatalf("ListServices = %d services, %v", len(list), err)
	}
	if _, err := s.PutService(ctx(), a); !errors.Is(err, model.ErrConflict) {
		t.Errorf("saving a service whose name another one shares in other case: %v, want ErrConflict", err)
	}
	b.Name = "api 2"
	if _, err := s.PutService(ctx(), b); err != nil {
		t.Fatalf("renaming one of the two: %v", err)
	}
	if _, err := s.PutService(ctx(), a); err != nil {
		t.Errorf("saving the other one after the rename: %v", err)
	}
}
