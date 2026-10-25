package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// nowUTC returns the current time truncated to what the database stores
// (nanoseconds, UTC, no monotonic reading), so a value returned by a write
// equals the one a later read returns.
func (s *SQLite) nowUTC() time.Time { return time.Unix(0, s.now().UnixNano()).UTC() }

// deleteByID deletes one row of a table keyed by id and reports ErrNotFound
// when nothing matched. table is always a package constant, never user input.
func (s *SQLite) deleteByID(ctx context.Context, table, kind, id string) error {
	res, err := s.wdb.ExecContext(ctx, "DELETE FROM "+table+" WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: delete %s %q: %w", kind, id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return notFound(kind, id)
	}
	return nil
}

func scanService(sc scanner) (model.Service, error) {
	var (
		svc              model.Service
		data             []byte
		created, updated int64
	)
	if err := sc.Scan(&data, &created, &updated); err != nil {
		return svc, err
	}
	if err := json.Unmarshal(data, &svc); err != nil {
		return svc, fmt.Errorf("store: decode service: %w", err)
	}
	svc.CreatedAt, svc.UpdatedAt = fromNanos(created), fromNanos(updated)
	return svc, nil
}

// ListServices returns every service ordered by name.
func (s *SQLite) ListServices(ctx context.Context) ([]model.Service, error) {
	rows, err := s.rdb.QueryContext(ctx,
		`SELECT data, created_at, updated_at FROM services ORDER BY name COLLATE NOCASE, name, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list services: %w", err)
	}
	defer rows.Close()
	out := []model.Service{}
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

// GetService returns the service with the given ID.
func (s *SQLite) GetService(ctx context.Context, id string) (model.Service, error) {
	svc, err := scanService(s.rdb.QueryRowContext(ctx,
		`SELECT data, created_at, updated_at FROM services WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Service{}, notFound("service", id)
	}
	return svc, err
}

// PutService inserts or updates a service by ID (assigning "svc_..." when
// empty). A duplicate Name or ListenAddr on another service returns an error
// wrapping model.ErrConflict.
func (s *SQLite) PutService(ctx context.Context, svc model.Service) (model.Service, error) {
	if svc.ID == "" {
		svc.ID = model.NewID("svc")
	}
	now := s.nowUTC()
	svc.CreatedAt, svc.UpdatedAt = now, now
	data, err := json.Marshal(svc)
	if err != nil {
		return model.Service{}, fmt.Errorf("store: encode service: %w", err)
	}
	var created int64
	err = s.wdb.QueryRowContext(ctx, `
		INSERT INTO services (id, name, listen_addr, mode, enabled, recording_id, created_at, updated_at, data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name, listen_addr = excluded.listen_addr, mode = excluded.mode,
			enabled = excluded.enabled, recording_id = excluded.recording_id,
			updated_at = excluded.updated_at, data = excluded.data
		RETURNING created_at`,
		svc.ID, svc.Name, svc.ListenAddr, string(svc.Mode), b2i(svc.Enabled), svc.RecordingID,
		nanos(now), nanos(now), string(data)).Scan(&created)
	if err != nil {
		if isUniqueViolation(err) {
			switch violatedColumn(err) {
			case "services.name":
				return model.Service{}, conflict("service name %q is already in use", svc.Name)
			case "services.listen_addr":
				return model.Service{}, conflict("listen address %q is already in use", svc.ListenAddr)
			}
			return model.Service{}, conflict("service %q conflicts with an existing service", svc.Name)
		}
		return model.Service{}, fmt.Errorf("store: put service: %w", err)
	}
	svc.CreatedAt = fromNanos(created)
	return svc, nil
}

// DeleteService removes a service. Its recording is kept.
func (s *SQLite) DeleteService(ctx context.Context, id string) error {
	return s.deleteByID(ctx, "services", "service", id)
}
