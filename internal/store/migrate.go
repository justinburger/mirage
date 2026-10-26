package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// migration is one versioned, forward-only schema change. Statements run in
// order inside a single transaction together with the schema_migrations row,
// so a failed migration leaves the database untouched.
type migration struct {
	version int
	name    string
	stmts   []string
}

// migrations is the ordered list of schema changes. Append new entries with
// the next version number; never edit or reorder one that has shipped.
var migrations = []migration{
	{version: 1, name: "initial schema", stmts: []string{
		`CREATE TABLE services (
			id           TEXT PRIMARY KEY,
			name         TEXT NOT NULL UNIQUE,
			listen_addr  TEXT NOT NULL UNIQUE,
			mode         TEXT NOT NULL,
			enabled      INTEGER NOT NULL,
			recording_id TEXT NOT NULL,
			created_at   INTEGER NOT NULL,
			updated_at   INTEGER NOT NULL,
			data         TEXT NOT NULL
		)`,

		`CREATE TABLE profiles (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			secret     TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			data       TEXT NOT NULL
		)`,

		`CREATE TABLE fault_rules (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			rule_order INTEGER NOT NULL,
			enabled    INTEGER NOT NULL,
			service_id TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			data       TEXT NOT NULL
		)`,
		`CREATE INDEX fault_rules_order ON fault_rules(rule_order, name)`,

		`CREATE TABLE recordings (
			id             TEXT PRIMARY KEY,
			name           TEXT NOT NULL,
			description    TEXT NOT NULL,
			service_id     TEXT NOT NULL,
			source         TEXT NOT NULL,
			anonymized     INTEGER NOT NULL,
			labels         TEXT,
			exchange_count INTEGER NOT NULL DEFAULT 0,
			error_count    INTEGER NOT NULL DEFAULT 0,
			bytes          INTEGER NOT NULL DEFAULT 0,
			first_at       INTEGER,
			last_at        INTEGER,
			created_at     INTEGER NOT NULL,
			updated_at     INTEGER NOT NULL
		)`,
		`CREATE INDEX recordings_created ON recordings(created_at)`,

		// Bodies are the last columns so that the row prefix holding every
		// scalar column can be read without touching body overflow pages.
		`CREATE TABLE exchanges (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			recording_id   TEXT NOT NULL REFERENCES recordings(id) ON DELETE CASCADE,
			seq            INTEGER NOT NULL,
			service_id     TEXT NOT NULL,
			started_at     INTEGER NOT NULL,
			duration       INTEGER NOT NULL,
			ttfb           INTEGER NOT NULL,
			endpoint       TEXT NOT NULL,
			method         TEXT NOT NULL,
			path           TEXT NOT NULL,
			raw_query      TEXT NOT NULL,
			status         INTEGER NOT NULL,
			is_error       INTEGER NOT NULL,
			req_bytes      INTEGER NOT NULL,
			resp_bytes     INTEGER NOT NULL,
			content_type   TEXT NOT NULL,
			error          TEXT NOT NULL,
			anonymized     INTEGER NOT NULL,
			req_scheme     TEXT NOT NULL,
			req_host       TEXT NOT NULL,
			req_proto      TEXT NOT NULL,
			req_encoding   TEXT NOT NULL,
			req_truncated  INTEGER NOT NULL,
			resp_proto     TEXT NOT NULL,
			resp_encoding  TEXT NOT NULL,
			resp_truncated INTEGER NOT NULL,
			req_headers    TEXT,
			resp_headers   TEXT,
			body_flags     INTEGER NOT NULL,
			req_body       BLOB,
			resp_body      BLOB
		)`,
		`CREATE UNIQUE INDEX exchanges_rec_seq ON exchanges(recording_id, seq)`,
		`CREATE INDEX exchanges_rec_endpoint ON exchanges(recording_id, endpoint)`,
		`CREATE INDEX exchanges_rec_status ON exchanges(recording_id, status)`,
		`CREATE INDEX exchanges_rec_errors ON exchanges(recording_id, seq) WHERE is_error = 1`,

		`CREATE TABLE replay_jobs (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			status     TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			started_at INTEGER,
			ended_at   INTEGER,
			data       TEXT NOT NULL,
			series     BLOB,
			series_gz  INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX replay_jobs_created ON replay_jobs(created_at)`,

		`CREATE TABLE settings (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		) WITHOUT ROWID`,
	}},

	// Version 2 drops the UNIQUE constraint on services.listen_addr. SQLite
	// cannot drop a constraint in place, so the table is rebuilt (nothing
	// references it). Uniqueness of listen addresses moved into PutService,
	// which compares host-normalised addresses and ignores port 0 (every
	// service on ":0" gets its own free port). The name stays UNIQUE.
	{version: 2, name: "services: listen_addr uniqueness moves to PutService", stmts: []string{
		`CREATE TABLE services_v2 (
			id           TEXT PRIMARY KEY,
			name         TEXT NOT NULL UNIQUE,
			listen_addr  TEXT NOT NULL,
			mode         TEXT NOT NULL,
			enabled      INTEGER NOT NULL,
			recording_id TEXT NOT NULL,
			created_at   INTEGER NOT NULL,
			updated_at   INTEGER NOT NULL,
			data         TEXT NOT NULL
		)`,
		`INSERT INTO services_v2 (id, name, listen_addr, mode, enabled, recording_id, created_at, updated_at, data)
			SELECT id, name, listen_addr, mode, enabled, recording_id, created_at, updated_at, data FROM services`,
		`DROP TABLE services`,
		`ALTER TABLE services_v2 RENAME TO services`,
	}},

	// Version 3 adds the tombstone DeleteRecording sets before it purges a
	// recording's exchanges in chunks. A recording with deleted_at set is
	// invisible to every read and write; a leftover one is purged when the
	// database is next opened.
	{version: 3, name: "recordings: deleted_at tombstone", stmts: []string{
		`ALTER TABLE recordings ADD COLUMN deleted_at INTEGER`,
	}},
}

// latestVersion is the schema version this build understands.
func latestVersion(ms []migration) int {
	v := 0
	for _, m := range ms {
		v = max(v, m.version)
	}
	return v
}

// applyMigrations brings the database up to the newest version in ms. It is
// idempotent and safe when several processes open the same file: each
// migration runs under BEGIN IMMEDIATE and re-checks that it is still needed.
// A database written by a newer build is refused rather than guessed at.
func applyMigrations(ctx context.Context, db *sql.DB, ms []migration) error {
	if !slices.IsSortedFunc(ms, func(a, b migration) int { return a.version - b.version }) {
		return errors.New("store: migrations are not in version order")
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}
	if newest := latestVersion(ms); len(applied) > 0 && slices.Max(applied) > newest {
		return fmt.Errorf("store: database schema version %d is newer than this build supports (%d)",
			slices.Max(applied), newest)
	}

	for _, m := range ms {
		if err := applyOne(ctx, db, m); err != nil {
			return fmt.Errorf("store: migration %d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

func appliedVersions(ctx context.Context, db *sql.DB) ([]int, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func applyOne(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil) // BEGIN IMMEDIATE on the writer pool
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.version).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil // already applied, possibly by a concurrent opener
	}
	for _, stmt := range m.stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`,
		m.version, m.name, nanos(time.Now())); err != nil {
		return err
	}
	return tx.Commit()
}

// SchemaVersion returns the newest schema migration applied to the database.
func (s *SQLite) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	if err := s.rdb.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: schema version: %w", err)
	}
	return int(v.Int64), nil
}
