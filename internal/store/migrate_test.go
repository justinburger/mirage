package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

// rawDB opens the writer pool of a fresh file database without migrating it.
func rawDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "raw.db")
	if err := prepareFile(path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(driverName, fileDSN(path, true))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func appliedCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMigrationsAreIdempotent(t *testing.T) {
	db, _ := rawDB(t)
	for i := range 3 {
		if err := applyMigrations(context.Background(), db, migrations); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if n := appliedCount(t, db); n != len(migrations) {
		t.Errorf("schema_migrations has %d rows, want %d", n, len(migrations))
	}
	for _, table := range []string{"services", "profiles", "fault_rules", "recordings", "exchanges", "replay_jobs", "settings"} {
		var name string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
	for _, idx := range []string{"exchanges_rec_seq", "exchanges_rec_endpoint", "exchanges_rec_status"} {
		var name string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&name); err != nil {
			t.Errorf("index %s missing: %v", idx, err)
		}
	}
}

func TestMigrationsUpgradeExistingDatabase(t *testing.T) {
	db, _ := rawDB(t)
	ctx := context.Background()
	if err := applyMigrations(ctx, db, migrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO settings(key, value, updated_at) VALUES ('keep', 'me', 1)`); err != nil {
		t.Fatal(err)
	}

	next := migration{version: latestVersion(migrations) + 1, name: "add notes", stmts: []string{
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT NOT NULL)`,
		`INSERT INTO notes(body) VALUES ('hello')`,
	}}
	upgraded := append(append([]migration{}, migrations...), next)
	for i := range 2 { // the second pass must be a no-op
		if err := applyMigrations(ctx, db, upgraded); err != nil {
			t.Fatalf("upgrade pass %d: %v", i, err)
		}
	}
	var body string
	if err := db.QueryRow(`SELECT body FROM notes`).Scan(&body); err != nil || body != "hello" {
		t.Errorf("notes = %q, %v", body, err)
	}
	var kept string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = 'keep'`).Scan(&kept); err != nil || kept != "me" {
		t.Errorf("existing data lost by upgrade: %q, %v", kept, err)
	}
	if n := appliedCount(t, db); n != len(upgraded) {
		t.Errorf("schema_migrations has %d rows, want %d", n, len(upgraded))
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	db, _ := rawDB(t)
	ctx := context.Background()
	if err := applyMigrations(ctx, db, migrations); err != nil {
		t.Fatal(err)
	}
	bad := migration{version: latestVersion(migrations) + 1, name: "broken", stmts: []string{
		`CREATE TABLE half_done (id INTEGER)`,
		`THIS IS NOT SQL`,
	}}
	err := applyMigrations(ctx, db, append(append([]migration{}, migrations...), bad))
	if want := fmt.Sprintf("migration %d", bad.version); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want one naming %s", err, want)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'half_done'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("failed migration left its first statement behind (count %d, %v)", n, err)
	}
	if got := appliedCount(t, db); got != len(migrations) {
		t.Errorf("failed migration was recorded: %d rows, want %d", got, len(migrations))
	}
}

func TestNewerDatabaseIsRefused(t *testing.T) {
	db, path := rawDB(t)
	ctx := context.Background()
	if err := applyMigrations(ctx, db, migrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES (99, 'future', 0)`); err != nil {
		t.Fatal(err)
	}
	err := applyMigrations(ctx, db, migrations)
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("applyMigrations on a newer database: %v", err)
	}
	_ = db.Close()
	if s, err := Open(path); err == nil {
		_ = s.Close()
		t.Fatal("Open accepted a database from a newer version")
	}
}

func TestMigrationsMustBeOrdered(t *testing.T) {
	db, _ := rawDB(t)
	bad := []migration{{version: 2, name: "b"}, {version: 1, name: "a"}}
	if err := applyMigrations(context.Background(), db, bad); err == nil {
		t.Fatal("unordered migrations were accepted")
	}
}

func TestMigrationVersionsAreContiguous(t *testing.T) {
	for i, m := range migrations {
		if m.version != i+1 {
			t.Errorf("migrations[%d].version = %d, want %d", i, m.version, i+1)
		}
		if m.name == "" || len(m.stmts) == 0 {
			t.Errorf("migration %d has no name or statements", m.version)
		}
	}
}

// v1Database creates a file database with only the first migration applied,
// like one written by the first release, holds two services and one recording
// in it and returns its path with the database closed.
func v1Database(t *testing.T) (path string, services []model.Service) {
	t.Helper()
	db, path := rawDB(t)
	if err := applyMigrations(context.Background(), db, migrations[:1]); err != nil {
		t.Fatal(err)
	}
	services = []model.Service{
		{ID: "svc_old1", Name: "orders", ListenAddr: ":9001", Upstream: "http://up1", Mode: model.ModeRecord, Enabled: true},
		{ID: "svc_old2", Name: "billing", ListenAddr: "127.0.0.1:9002", Upstream: "http://up2", Mode: model.ModeVirtualize},
	}
	for i, svc := range services {
		data, err := json.Marshal(svc)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO services (id, name, listen_addr, mode, enabled, recording_id, created_at, updated_at, data)
			VALUES (?, ?, ?, ?, ?, '', ?, ?, ?)`,
			svc.ID, svc.Name, svc.ListenAddr, string(svc.Mode), b2i(svc.Enabled), 100+i, 200+i, string(data)); err != nil {
			t.Fatal(err)
		}
	}
	// The v1 constraint really is in place.
	if _, err := db.Exec(`INSERT INTO services (id, name, listen_addr, mode, enabled, recording_id, created_at, updated_at, data)
		VALUES ('svc_dup', 'dup', ':9001', 'record', 0, '', 1, 1, '{}')`); err == nil {
		t.Fatal("v1 schema accepted a duplicate listen_addr; the fixture is not a v1 database")
	}
	if _, err := db.Exec(`INSERT INTO recordings (id, name, description, service_id, source, anonymized, created_at, updated_at)
		VALUES ('rec_old', 'old capture', '', 'svc_old1', 'proxy', 1, 5, 6)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path, services
}

// TestOpenMigratesV1Database opens a database written by the first release:
// existing services and recordings survive, services may now share port 0, and
// the other constraints still hold.
func TestOpenMigratesV1Database(t *testing.T) {
	path, services := v1Database(t)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a v1 database: %v", err)
	}
	defer s.Close()

	if v, err := s.SchemaVersion(ctx()); err != nil || v != latestVersion(migrations) {
		t.Errorf("SchemaVersion = %d, %v; want %d", v, err, latestVersion(migrations))
	}

	// Services keep their data, timestamps and order.
	list, err := s.ListServices(ctx())
	if err != nil || len(list) != len(services) {
		t.Fatalf("ListServices = %d services, %v", len(list), err)
	}
	for _, want := range services { // listed by name: billing, orders
		got, err := s.GetService(ctx(), want.ID)
		if err != nil {
			t.Fatal(err)
		}
		want.CreatedAt, want.UpdatedAt = got.CreatedAt, got.UpdatedAt
		if !reflect.DeepEqual(got, want) {
			t.Errorf("service %s after migration:\n got %+v\nwant %+v", want.ID, got, want)
		}
		if got.CreatedAt.IsZero() || !got.UpdatedAt.After(got.CreatedAt) {
			t.Errorf("service %s lost its timestamps: %v / %v", want.ID, got.CreatedAt, got.UpdatedAt)
		}
	}

	// The schema now allows what v1 forbade, and nothing more.
	for _, tc := range []struct {
		name string
		svc  model.Service
		want error
	}{
		{"port 0 twice (first)", model.Service{Name: "t1", ListenAddr: ":0"}, nil},
		{"port 0 twice (second)", model.Service{Name: "t2", ListenAddr: "127.0.0.1:0"}, nil},
		{"taken socket", model.Service{Name: "t3", ListenAddr: "0.0.0.0:9001"}, model.ErrConflict},
		{"taken name", model.Service{Name: "orders", ListenAddr: ":9100"}, model.ErrConflict},
	} {
		if _, err := s.PutService(ctx(), tc.svc); !errors.Is(err, tc.want) {
			t.Errorf("%s: PutService = %v, want %v", tc.name, err, tc.want)
		}
	}
	// The name uniqueness is a database constraint, not just the Go check.
	_, err = s.wdb.Exec(`INSERT INTO services (id, name, listen_addr, mode, enabled, recording_id, created_at, updated_at, data)
		VALUES ('svc_x', 'orders', ':9200', 'record', 0, '', 1, 1, '{}')`)
	if err == nil || !strings.Contains(err.Error(), "services.name") {
		t.Errorf("duplicate name inserted through SQL: %v", err)
	}

	// The recording is intact and fully usable, including the new delete path.
	rec, err := s.GetRecording(ctx(), "rec_old")
	if err != nil || rec.Name != "old capture" || !rec.Anonymized {
		t.Fatalf("recording after migration: %+v, %v", rec, err)
	}
	appendN(t, s, rec.ID, 3, withAnonymized(true))
	if got, _ := s.GetRecording(ctx(), rec.ID); got.ExchangeCount != 3 || !got.Anonymized {
		t.Errorf("append to a migrated recording: %+v", got)
	}
	if err := s.DeleteRecording(ctx(), rec.ID); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, "exchanges"); n != 0 {
		t.Errorf("%d exchanges left after deleting the migrated recording", n)
	}
}

// TestMigrationsLeaveNoStrayObjects: after all migrations the services table
// is the only copy and keeps no leftover from the rebuild.
func TestMigrationsLeaveNoStrayObjects(t *testing.T) {
	db, _ := rawDB(t)
	if err := applyMigrations(context.Background(), db, migrations); err != nil {
		t.Fatal(err)
	}
	var stray int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'services_v2%' OR tbl_name LIKE 'services_v2%'`).Scan(&stray); err != nil || stray != 0 {
		t.Errorf("%d objects left from the services rebuild (%v)", stray, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('recordings') WHERE name = 'deleted_at'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("recordings.deleted_at missing (%d, %v)", n, err)
	}
}
