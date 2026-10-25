package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func TestOpenRejectsEmptyPath(t *testing.T) {
	for _, p := range []string{"", "   "} {
		if s, err := Open(p); err == nil {
			_ = s.Close()
			t.Errorf("Open(%q) succeeded, want error", p)
		}
	}
}

func TestOpenMemoryIsPrivate(t *testing.T) {
	a, b := openMem(t), openMem(t)
	rec := newRecording(t, a, "only in a")
	if _, err := b.GetRecording(ctx(), rec.ID); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("second memory store sees the first one's data: err = %v", err)
	}
	if got, _ := b.ListRecordings(ctx()); len(got) != 0 {
		t.Fatalf("second memory store has %d recordings, want 0", len(got))
	}
}

// TestMemoryConcurrentUse hammers one in-memory store from many goroutines.
func TestMemoryConcurrentUse(t *testing.T) {
	s := openMem(t)
	rec := newRecording(t, s, "shared")
	appendN(t, s, rec.ID, 10)

	const n = 24
	var (
		wg   sync.WaitGroup
		gate = make(chan struct{})
		errs = make(chan error, n)
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			errs <- s.read(ctx(), func(tx *sql.Tx) error {
				var c int64
				if err := tx.QueryRow(`SELECT exchange_count FROM recordings WHERE id = ?`, rec.ID).Scan(&c); err != nil {
					return err
				}
				if c != 10 {
					return fmt.Errorf("exchange_count = %d, want 10", c)
				}
				return nil
			})
		}()
	}
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// TestMemorySurvivesConnectionRecycling closes the working connection after
// every operation; the pinned keeper connection must keep the data alive.
func TestMemorySurvivesConnectionRecycling(t *testing.T) {
	s := openMem(t)
	s.wdb.SetMaxIdleConns(0) // every released connection is closed
	rec := newRecording(t, s, "survivor")
	appendN(t, s, rec.ID, 5)
	for range 5 {
		got, err := s.GetRecording(ctx(), rec.ID)
		if err != nil || got.ExchangeCount != 5 {
			t.Fatalf("after recycling: %+v, %v", got, err)
		}
	}
	if _, err := s.GetProfile(ctx(), model.DefaultProfileID); err != nil {
		t.Fatalf("seeded profile lost: %v", err)
	}
}

func TestOpenFilePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "nested", "mirage.db") // parent dirs are created
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := s.PutService(ctx(), model.Service{Name: "api", ListenAddr: ":9001", Upstream: "http://up", Mode: model.ModeRecord})
	if err != nil {
		t.Fatal(err)
	}
	rec := newRecording(t, s, "traffic")
	exs := appendN(t, s, rec.ID, 7, withStatus(500))
	if err := s.PutSetting(ctx(), "ui.theme", "dark"); err != nil {
		t.Fatal(err)
	}
	def, err := s.GetProfile(ctx(), model.DefaultProfileID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	if got, err := s2.GetService(ctx(), svc.ID); err != nil || got.Name != "api" || got.Mode != model.ModeRecord {
		t.Errorf("service after reopen = %+v, %v", got, err)
	}
	got, err := s2.GetRecording(ctx(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExchangeCount != 7 || got.ErrorCount != 7 {
		t.Errorf("recording counters after reopen = %d/%d, want 7/7", got.ExchangeCount, got.ErrorCount)
	}
	ex, err := s2.GetExchange(ctx(), exs[3].ID)
	if err != nil || ex.Request.Path != "/items/3" || ex.Seq != 4 {
		t.Errorf("exchange after reopen = %+v, %v", ex, err)
	}
	if v, err := s2.GetSetting(ctx(), "ui.theme"); err != nil || v != "dark" {
		t.Errorf("setting after reopen = %q, %v", v, err)
	}
	// Reopening must not re-seed the default profile with a new secret.
	def2, err := s2.GetProfile(ctx(), model.DefaultProfileID)
	if err != nil || def2.Secret != def.Secret {
		t.Errorf("default profile secret changed across reopen: %q -> %q (%v)", def.Secret, def2.Secret, err)
	}
	if v, err := s2.SchemaVersion(ctx()); err != nil || v != latestVersion(migrations) {
		t.Errorf("SchemaVersion = %d, %v", v, err)
	}
}

func TestOpenFileWeirdNames(t *testing.T) {
	for _, name := range []string{"has space.db", "q?mark.db", "hash#tag.db", "pct%41.db", "ünï.db"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			s, err := Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			rec := newRecording(t, s, "x")
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("database file not at the requested path: %v", err)
			}
			s2, err := Open(path)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer s2.Close()
			if _, err := s2.GetRecording(ctx(), rec.ID); err != nil {
				t.Fatalf("recording lost: %v", err)
			}
		})
	}
}

func TestOpenFileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	_, path := openFile(t)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("database file mode = %v, want no group/other access", perm)
	}
}

func TestOpenFileInUnwritableLocation(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A path below a regular file cannot be created.
	if s, err := Open(filepath.Join(blocker, "sub", "x.db")); err == nil {
		_ = s.Close()
		t.Fatal("Open below a regular file succeeded")
	}
}

func TestOpenGarbageFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.db")
	if err := os.WriteFile(path, []byte(strings.Repeat("this is not sqlite ", 500)), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		_ = s.Close()
		t.Fatal("Open on a non-database file succeeded")
	}
}

func TestFileDatabaseSettings(t *testing.T) {
	s, _ := openFile(t)
	pragma := func(db *sql.DB, name string) string {
		t.Helper()
		var v string
		if err := db.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
			t.Fatalf("PRAGMA %s: %v", name, err)
		}
		return v
	}
	for _, c := range []struct{ db, pragma, want string }{
		{"writer", "journal_mode", "wal"},
		{"writer", "foreign_keys", "1"},
		{"writer", "synchronous", "1"}, // NORMAL
		{"writer", "busy_timeout", "5000"},
		{"writer", "page_size", "16384"},
		{"writer", "auto_vacuum", "2"},
		{"reader", "query_only", "1"},
		{"reader", "busy_timeout", "5000"},
	} {
		db := s.wdb
		if c.db == "reader" {
			db = s.rdb
		}
		if got := pragma(db, c.pragma); got != c.want {
			t.Errorf("%s PRAGMA %s = %s, want %s", c.db, c.pragma, got, c.want)
		}
	}
	// Readers cannot write.
	if _, err := s.rdb.Exec(`DELETE FROM settings`); err == nil {
		t.Error("the reader pool accepted a write")
	}
}

func TestCloseIsIdempotentAndStopsUse(t *testing.T) {
	s, err := Open(MemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	rec := newRecording(t, s, "r")
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := s.GetRecording(ctx(), rec.ID); err == nil {
		t.Error("GetRecording after Close succeeded")
	}
	if _, err := s.CreateRecording(ctx(), model.Recording{Name: "x"}); err == nil {
		t.Error("CreateRecording after Close succeeded")
	}
	if err := s.AppendExchanges(ctx(), rec.ID, []*model.Exchange{mkEx(0)}); err == nil {
		t.Error("AppendExchanges after Close succeeded")
	}
}

func TestCancelledContext(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		rec := newRecording(t, s, "r")
		appendN(t, s, rec.ID, 3)

		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := s.AppendExchanges(cctx, rec.ID, []*model.Exchange{mkEx(9)}); err == nil {
			t.Error("AppendExchanges with a cancelled context succeeded")
		}
		if _, _, err := s.ListExchanges(cctx, rec.ID, model.ExchangeQuery{}); err == nil {
			t.Error("ListExchanges with a cancelled context succeeded")
		}
		if _, err := s.PutService(cctx, model.Service{Name: "x", ListenAddr: ":1"}); err == nil {
			t.Error("PutService with a cancelled context succeeded")
		}
		got, err := s.GetRecording(ctx(), rec.ID)
		if err != nil || got.ExchangeCount != 3 {
			t.Errorf("cancelled append changed the recording: %+v, %v", got, err)
		}
	})
}

func TestEscapePath(t *testing.T) {
	for in, want := range map[string]string{
		"/tmp/a.db":       "/tmp/a.db",
		"/tmp/a b.db":     "/tmp/a b.db",
		"/tmp/a?b#c%d.db": "/tmp/a%3Fb%23c%25d.db",
		"rel/x.db":        "rel/x.db",
	} {
		if got := escapePath(in); got != want {
			t.Errorf("escapePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTwoStoresOneFile opens the same database twice (as two processes would,
// for example `mirage serve` and a CLI command) and writes from both.
func TestTwoStoresOneFile(t *testing.T) {
	a, path := openFile(t)
	b, err := Open(path)
	if err != nil {
		t.Fatalf("second Open of the same file: %v", err)
	}
	defer b.Close()

	recA, recB := newRecording(t, a, "from-a"), newRecording(t, b, "from-b")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, w := range []struct {
		s   *SQLite
		rec string
	}{{a, recA.ID}, {b, recB.ID}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 20 {
				exs := []*model.Exchange{mkEx(i), mkEx(i + 100)}
				if err := w.s.AppendExchanges(ctx(), w.rec, exs); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// Each store sees the other's recording and data.
	for _, s := range []*SQLite{a, b} {
		for _, rec := range []string{recA.ID, recB.ID} {
			got, err := s.GetRecording(ctx(), rec)
			if err != nil || got.ExchangeCount != 40 {
				t.Errorf("recording %s: %+v, %v", rec, got, err)
			}
		}
	}
	if err := a.PutSetting(ctx(), "k", "from a"); err != nil {
		t.Fatal(err)
	}
	if v, err := b.GetSetting(ctx(), "k"); err != nil || v != "from a" {
		t.Errorf("setting across stores = %q, %v", v, err)
	}
}
