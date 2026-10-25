package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// MemoryPath is the Open path that selects a private in-memory database.
const MemoryPath = ":memory:"

const (
	driverName = "sqlite"
	// busyTimeoutMs is how long SQLite waits on a lock held by another
	// process before reporting SQLITE_BUSY.
	busyTimeoutMs = 5000
	// writerCacheKiB and readerCacheKiB size the per-connection page caches.
	writerCacheKiB = 32 * 1024
	readerCacheKiB = 16 * 1024
	// walSizeLimit truncates the WAL file back to this size after checkpoints.
	walSizeLimit = 64 << 20
	// pageSize is the page size of new databases. Larger pages keep a typical
	// exchange row (headers plus a few KB of bodies) on a single page and
	// need far fewer write calls than SQLite's 4 KiB default.
	pageSize = 16384
	// walCheckpointPages is the WAL size that triggers an automatic
	// checkpoint (about 40 MiB with 16 KiB pages). Checkpointing less often
	// lets hot index pages be written back once instead of many times.
	walCheckpointPages = 2560
	// maxReaders bounds the reader pool.
	maxReaders = 16
)

// SQLite is the SQLite implementation of Store. Create one with Open and
// release it with Close. All methods are safe for concurrent use.
type SQLite struct {
	path string
	mem  bool

	// wdb is the single-connection writer pool and rdb the read-only pool. In
	// memory mode both are the same single-connection pool.
	wdb *sql.DB
	rdb *sql.DB
	// keeper is a connection pinned for the lifetime of an in-memory store
	// so the database cannot disappear if the working connection is recycled.
	keeper *sql.Conn

	insertExchange *sql.Stmt // prepared on wdb
	getExchange    *sql.Stmt // prepared on rdb

	// now is the clock; tests replace it.
	now func() time.Time

	closeOnce sync.Once
	closeErr  error
}

// Open opens (creating if needed) the database at path, applies pending
// schema migrations and seeds the built-in "default" anonymization profile.
// Missing parent directories are created, and a new database file is made
// readable by its owner only because it holds anonymization secrets and
// captured traffic. The special path ":memory:" opens a private in-memory
// database: every Open returns an independent database that disappears on
// Close.
func Open(path string) (*SQLite, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("store: database path is empty")
	}
	s := &SQLite{path: path, mem: path == MemoryPath, now: time.Now}

	var wdsn, rdsn string
	if s.mem {
		name, err := memoryName()
		if err != nil {
			return nil, err
		}
		wdsn = memoryDSN(name)
	} else {
		if err := prepareFile(path); err != nil {
			return nil, err
		}
		if err := initFile(path); err != nil {
			return nil, err
		}
		wdsn, rdsn = fileDSN(path, true), fileDSN(path, false)
	}

	if err := s.connect(wdsn, rdsn); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// connect opens the pools, migrates the schema, seeds defaults and prepares
// the hot statements.
func (s *SQLite) connect(wdsn, rdsn string) (err error) {
	ctx := context.Background()

	s.wdb, err = sql.Open(driverName, wdsn)
	if err != nil {
		return fmt.Errorf("store: open writer: %w", err)
	}
	if s.mem {
		// One database/sql pool does all the work through a single
		// connection, which serialises every operation and so gives readers
		// full isolation from writers without shared-cache table locks. A
		// second connection is pinned by the keeper below; being pinned it
		// never serves queries, so at most one connection is ever active.
		s.rdb = s.wdb
		s.wdb.SetMaxOpenConns(2)
	} else {
		// One writer connection: SQLite serialises writers anyway, and
		// queueing in database/sql honours context cancellation, unlike a busy
		// loop.
		s.wdb.SetMaxOpenConns(1)
	}
	s.wdb.SetMaxIdleConns(1)
	s.wdb.SetConnMaxLifetime(0)
	s.wdb.SetConnMaxIdleTime(0)
	if err := s.wdb.PingContext(ctx); err != nil {
		return fmt.Errorf("store: open %s: %w", s.describe(), err)
	}
	if s.mem {
		// A shared-cache memory database lives only while a connection to it
		// is open: pin the one just created.
		if s.keeper, err = s.wdb.Conn(ctx); err != nil {
			return fmt.Errorf("store: pin memory database: %w", err)
		}
	}

	if err := applyMigrations(ctx, s.wdb, migrations); err != nil {
		return err
	}
	if err := s.seedDefaultProfile(ctx); err != nil {
		return err
	}

	if !s.mem {
		s.rdb, err = sql.Open(driverName, rdsn)
		if err != nil {
			return fmt.Errorf("store: open reader: %w", err)
		}
		readers := min(max(runtime.GOMAXPROCS(0), 4), maxReaders)
		s.rdb.SetMaxOpenConns(readers)
		s.rdb.SetMaxIdleConns(readers)
		s.rdb.SetConnMaxLifetime(0)
		s.rdb.SetConnMaxIdleTime(0)
		if err := s.rdb.PingContext(ctx); err != nil {
			return fmt.Errorf("store: open reader %s: %w", s.describe(), err)
		}
	}

	if s.insertExchange, err = s.wdb.PrepareContext(ctx, insertExchangeSQL); err != nil {
		return fmt.Errorf("store: prepare insert: %w", err)
	}
	if s.getExchange, err = s.rdb.PrepareContext(ctx, getExchangeSQL); err != nil {
		return fmt.Errorf("store: prepare get: %w", err)
	}
	return nil
}

func (s *SQLite) describe() string {
	if s.mem {
		return "in-memory database"
	}
	return s.path
}

// Close releases the database. It is safe to call more than once; later calls
// return the first call's result. Operations still running are allowed to
// finish; new ones fail.
func (s *SQLite) Close() error {
	s.closeOnce.Do(func() {
		var errs []error
		if s.insertExchange != nil {
			errs = append(errs, s.insertExchange.Close())
		}
		if s.getExchange != nil {
			errs = append(errs, s.getExchange.Close())
		}
		if s.keeper != nil {
			errs = append(errs, s.keeper.Close())
		}
		if s.rdb != nil && s.rdb != s.wdb {
			errs = append(errs, s.rdb.Close())
		}
		if s.wdb != nil {
			// Let the planner refresh its statistics; closing the last
			// connection also checkpoints and removes the WAL.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = s.wdb.ExecContext(ctx, "PRAGMA optimize")
			cancel()
			errs = append(errs, s.wdb.Close())
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

// write runs fn inside a write transaction on the single writer connection.
// The transaction commits when fn returns nil and rolls back otherwise.
func (s *SQLite) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// read runs fn inside a read-only transaction, so every query in fn sees one
// consistent snapshot.
func (s *SQLite) read(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("store: begin read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

// memoryName returns a unique name for a shared-cache in-memory database.
func memoryName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: random name: %w", err)
	}
	return "mirage-mem-" + hex.EncodeToString(b[:]), nil
}

// pragmas returns the per-connection pragmas for the writer (or, in memory
// mode, the only) connection and for file-mode readers.
func pragmas(writer, mem bool) []string {
	if writer {
		p := []string{
			fmt.Sprintf("busy_timeout(%d)", busyTimeoutMs),
			"synchronous(NORMAL)",
			"foreign_keys(ON)",
			fmt.Sprintf("cache_size(-%d)", writerCacheKiB),
		}
		if !mem {
			p = append(p, "journal_mode(WAL)", fmt.Sprintf("journal_size_limit(%d)", walSizeLimit),
				fmt.Sprintf("wal_autocheckpoint(%d)", walCheckpointPages))
		}
		return p
	}
	return []string{
		fmt.Sprintf("busy_timeout(%d)", busyTimeoutMs),
		"query_only(1)",
		fmt.Sprintf("cache_size(-%d)", readerCacheKiB),
	}
}

func buildQuery(writer, mem bool, extra url.Values) string {
	q := url.Values{}
	for k, vs := range extra {
		q[k] = vs
	}
	for _, p := range pragmas(writer, mem) {
		q.Add("_pragma", p)
	}
	if writer {
		q.Set("_txlock", "immediate")
	}
	return q.Encode()
}

func memoryDSN(name string) string {
	return "file:" + name + "?" + buildQuery(true, true, url.Values{
		"mode":  {"memory"},
		"cache": {"shared"},
	})
}

func fileDSN(path string, writer bool) string {
	return "file:" + escapePath(path) + "?" + buildQuery(writer, false, nil)
}

// escapePath makes a filesystem path safe to embed in a SQLite file: URI.
func escapePath(p string) string {
	p = filepath.ToSlash(p)
	if len(p) >= 2 && p[1] == ':' { // Windows drive letter
		p = "/" + p
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '%', '?', '#':
			fmt.Fprintf(&b, "%%%02X", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// prepareFile creates the database's directory and an empty owner-only file.
// The database holds anonymization secrets and captured traffic, so it should
// not be world-readable; SQLite gives the WAL and shared-memory files the same
// permissions as the main file.
func prepareFile(path string) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("store: create directory: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("store: create %s: %w", path, err)
	}
	return f.Close()
}

// initFile applies the settings that can only be chosen while a database is
// still empty: the page size and incremental auto-vacuum (which lets
// DeleteRecording hand freed pages back to the filesystem). They must precede
// the switch to WAL mode, which the driver would otherwise apply first, so
// they are set on a throwaway connection. Existing databases are left alone.
func initFile(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("store: stat %s: %w", path, err)
	}
	if fi.Size() != 0 {
		return nil
	}
	db, err := sql.Open(driverName, "file:"+escapePath(path))
	if err != nil {
		return fmt.Errorf("store: initialise %s: %w", path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		fmt.Sprintf("PRAGMA page_size = %d", pageSize),
		"PRAGMA auto_vacuum = INCREMENTAL",
		"PRAGMA journal_mode = WAL",
	} {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("store: initialise %s: %s: %w", path, stmt, err)
		}
	}
	return nil
}
