package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// nanos converts a time to its stored form: Unix nanoseconds, with the zero
// time stored as 0.
func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// fromNanos is the inverse of nanos. Times are returned in UTC.
func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// nullNanos converts an optional time to a SQL value (NULL when absent).
func nullNanos(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UnixNano()
}

// timePtr converts a nullable stored time to *time.Time.
func timePtr(n *int64) *time.Time {
	if n == nil {
		return nil
	}
	t := fromNanos(*n)
	return &t
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// scanner is satisfied by *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// encodeHeader renders headers as JSON text, or NULL when there are none.
func encodeHeader(h http.Header) (any, error) {
	if len(h) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("encode headers: %w", err)
	}
	return string(b), nil
}

// decodeHeader parses stored header JSON; NULL (nil) yields nil.
func decodeHeader(b []byte) (http.Header, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var h http.Header
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, fmt.Errorf("decode headers: %w", err)
	}
	return h, nil
}

// notFound builds an error wrapping model.ErrNotFound.
func notFound(kind, id string) error {
	return fmt.Errorf("store: %s %q: %w", kind, id, model.ErrNotFound)
}

// conflict builds an error wrapping model.ErrConflict.
func conflict(format string, args ...any) error {
	return fmt.Errorf("store: %s: %w", fmt.Sprintf(format, args...), model.ErrConflict)
}

// isUniqueViolation reports whether err is a SQLite UNIQUE or PRIMARY KEY
// constraint failure.
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code() {
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
		return true
	}
	return false
}

// violatedColumn extracts "table.column" from a UNIQUE failure message such
// as "constraint failed: UNIQUE constraint failed: services.name (2067)".
func violatedColumn(err error) string {
	const marker = "UNIQUE constraint failed: "
	msg := err.Error()
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	col := msg[i+len(marker):]
	if j := strings.IndexAny(col, " ("); j >= 0 {
		col = col[:j]
	}
	return col
}
