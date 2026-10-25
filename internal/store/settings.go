package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetSetting returns the value stored under key, or an error wrapping
// model.ErrNotFound when it was never set.
func (s *SQLite) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.rdb.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", notFound("setting", key)
	}
	if err != nil {
		return "", fmt.Errorf("store: get setting %q: %w", key, err)
	}
	return v, nil
}

// PutSetting stores value under key, replacing any previous value.
func (s *SQLite) PutSetting(ctx context.Context, key, value string) error {
	_, err := s.wdb.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, nanos(s.nowUTC()))
	if err != nil {
		return fmt.Errorf("store: put setting %q: %w", key, err)
	}
	return nil
}
