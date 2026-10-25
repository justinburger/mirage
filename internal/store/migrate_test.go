package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
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

	v2 := migration{version: 2, name: "add notes", stmts: []string{
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT NOT NULL)`,
		`INSERT INTO notes(body) VALUES ('hello')`,
	}}
	upgraded := append(append([]migration{}, migrations...), v2)
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
	if n := appliedCount(t, db); n != 2 {
		t.Errorf("schema_migrations has %d rows, want 2", n)
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	db, _ := rawDB(t)
	ctx := context.Background()
	if err := applyMigrations(ctx, db, migrations); err != nil {
		t.Fatal(err)
	}
	bad := migration{version: 2, name: "broken", stmts: []string{
		`CREATE TABLE half_done (id INTEGER)`,
		`THIS IS NOT SQL`,
	}}
	err := applyMigrations(ctx, db, append(append([]migration{}, migrations...), bad))
	if err == nil || !strings.Contains(err.Error(), "migration 2") {
		t.Fatalf("error = %v, want one naming migration 2", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'half_done'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("failed migration left its first statement behind (count %d, %v)", n, err)
	}
	if got := appliedCount(t, db); got != 1 {
		t.Errorf("failed migration was recorded: %d rows", got)
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
