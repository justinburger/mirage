package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/justinburger/mirage/internal/model"
)

const recordingCols = `id, name, description, service_id, source, anonymized, labels,
	exchange_count, error_count, bytes, first_at, last_at, created_at, updated_at`

func scanRecording(sc scanner) (model.Recording, error) {
	var (
		r                model.Recording
		labels           []byte
		first, last      *int64
		created, updated int64
	)
	err := sc.Scan(&r.ID, &r.Name, &r.Description, &r.ServiceID, &r.Source, &r.Anonymized, &labels,
		&r.ExchangeCount, &r.ErrorCount, &r.Bytes, &first, &last, &created, &updated)
	if err != nil {
		return r, err
	}
	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &r.Labels); err != nil {
			return r, fmt.Errorf("store: decode recording labels: %w", err)
		}
	}
	r.FirstAt, r.LastAt = timePtr(first), timePtr(last)
	r.CreatedAt, r.UpdatedAt = fromNanos(created), fromNanos(updated)
	return r, nil
}

func encodeLabels(l map[string]string) (any, error) {
	if len(l) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(l)
	if err != nil {
		return nil, fmt.Errorf("store: encode recording labels: %w", err)
	}
	return string(b), nil
}

// CreateRecording inserts a recording, assigning a "rec_..." ID when empty.
// Counters and FirstAt/LastAt always start empty: they belong to
// AppendExchanges. An existing ID returns an error wrapping model.ErrConflict.
func (s *SQLite) CreateRecording(ctx context.Context, r model.Recording) (model.Recording, error) {
	if r.ID == "" {
		r.ID = model.NewID("rec")
	}
	now := s.nowUTC()
	r.CreatedAt, r.UpdatedAt = now, now
	r.ExchangeCount, r.ErrorCount, r.Bytes = 0, 0, 0
	r.FirstAt, r.LastAt = nil, nil
	r.Labels = maps.Clone(r.Labels)
	labels, err := encodeLabels(r.Labels)
	if err != nil {
		return model.Recording{}, err
	}
	_, err = s.wdb.ExecContext(ctx, `
		INSERT INTO recordings (id, name, description, service_id, source, anonymized, labels, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Name, r.Description, r.ServiceID, r.Source, b2i(r.Anonymized), labels, nanos(now), nanos(now))
	if err != nil {
		if isUniqueViolation(err) {
			return model.Recording{}, conflict("recording %q already exists", r.ID)
		}
		return model.Recording{}, fmt.Errorf("store: create recording: %w", err)
	}
	return r, nil
}

// ListRecordings returns every recording, newest first.
func (s *SQLite) ListRecordings(ctx context.Context) ([]model.Recording, error) {
	rows, err := s.rdb.QueryContext(ctx,
		`SELECT `+recordingCols+` FROM recordings ORDER BY created_at DESC, rowid DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list recordings: %w", err)
	}
	defer rows.Close()
	out := []model.Recording{}
	for rows.Next() {
		r, err := scanRecording(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRecording returns the recording with the given ID.
func (s *SQLite) GetRecording(ctx context.Context, id string) (model.Recording, error) {
	r, err := scanRecording(s.rdb.QueryRowContext(ctx,
		`SELECT `+recordingCols+` FROM recordings WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Recording{}, notFound("recording", id)
	}
	return r, err
}

// UpdateRecording changes Name, Description, Labels, Anonymized and Source of
// an existing recording and returns it with its current counters.
func (s *SQLite) UpdateRecording(ctx context.Context, r model.Recording) (model.Recording, error) {
	labels, err := encodeLabels(r.Labels)
	if err != nil {
		return model.Recording{}, err
	}
	out, err := scanRecording(s.wdb.QueryRowContext(ctx, `
		UPDATE recordings SET name = ?, description = ?, source = ?, anonymized = ?, labels = ?, updated_at = ?
		WHERE id = ?
		RETURNING `+recordingCols,
		r.Name, r.Description, r.Source, b2i(r.Anonymized), labels, nanos(s.nowUTC()), r.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Recording{}, notFound("recording", r.ID)
	}
	if err != nil {
		return model.Recording{}, fmt.Errorf("store: update recording: %w", err)
	}
	return out, nil
}

// DeleteRecording removes a recording and, through the foreign key cascade,
// all of its exchanges. On a file database it then returns the freed pages to
// the filesystem in small steps, so concurrent appends are not starved.
func (s *SQLite) DeleteRecording(ctx context.Context, id string) error {
	if err := s.deleteByID(ctx, "recordings", "recording", id); err != nil {
		return err
	}
	s.reclaimSpace(ctx)
	return nil
}

const (
	vacuumStepPages = 4096
	vacuumMaxSteps  = 4096
)

// reclaimSpace releases free pages with incremental_vacuum (effective on
// databases created with auto_vacuum=INCREMENTAL, a no-op otherwise). Each
// step is its own short write so other writers interleave. It is best effort.
func (s *SQLite) reclaimSpace(ctx context.Context) {
	if s.mem {
		return
	}
	prev := int64(-1)
	for range vacuumMaxSteps {
		var free int64
		if err := s.wdb.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil || free == 0 {
			return
		}
		if prev >= 0 && free >= prev {
			return // no progress: the database is not in incremental mode
		}
		prev = free
		if _, err := s.wdb.ExecContext(ctx, fmt.Sprintf(`PRAGMA incremental_vacuum(%d)`, vacuumStepPages)); err != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}
