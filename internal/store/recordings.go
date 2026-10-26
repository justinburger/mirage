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
// AppendExchanges. Source and the initial Anonymized flag are fixed here
// (AppendExchanges maintains Anonymized afterwards; UpdateRecording changes
// neither). An existing ID, including one whose recording is still being
// purged by DeleteRecording, returns an error wrapping model.ErrConflict.
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
		`SELECT `+recordingCols+` FROM recordings WHERE deleted_at IS NULL ORDER BY created_at DESC, rowid DESC`)
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
		`SELECT `+recordingCols+` FROM recordings WHERE id = ? AND deleted_at IS NULL`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Recording{}, notFound("recording", id)
	}
	return r, err
}

// UpdateRecording changes Name, Description and Labels of an existing
// recording and returns it with its current state. Source and Anonymized are
// not written: Source is fixed by CreateRecording and Anonymized is maintained
// by AppendExchanges (see there).
func (s *SQLite) UpdateRecording(ctx context.Context, r model.Recording) (model.Recording, error) {
	labels, err := encodeLabels(r.Labels)
	if err != nil {
		return model.Recording{}, err
	}
	out, err := scanRecording(s.wdb.QueryRowContext(ctx, `
		UPDATE recordings SET name = ?, description = ?, labels = ?, updated_at = ?
		WHERE id = ? AND deleted_at IS NULL
		RETURNING `+recordingCols,
		r.Name, r.Description, labels, nanos(s.nowUTC()), r.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Recording{}, notFound("recording", r.ID)
	}
	if err != nil {
		return model.Recording{}, fmt.Errorf("store: update recording: %w", err)
	}
	return out, nil
}

// deleteChunkRows is the most exchange rows one DeleteRecording transaction
// removes, so the single SQLite writer is released between chunks and capture
// batches of other recordings interleave with a long delete.
const deleteChunkRows = 10_000

// DeleteRecording removes a recording and all of its exchanges.
//
// The recording disappears first, in one short transaction that tombstones it
// (deleted_at): GetRecording, ListRecordings and every operation on it report
// it as gone, and its ID stays reserved until the purge ends. The exchanges
// are then deleted synchronously in chunks of at most deleteChunkRows rows,
// one transaction per chunk, before the recording row itself is removed; on a
// file database the freed pages are finally returned to the filesystem in
// small steps. Nothing is held between chunks, so concurrent appends to other
// recordings keep making progress.
//
// If ctx ends after the recording was tombstoned, the rest of the purge
// continues in the background (and DeleteRecording returns ctx's error); a
// purge that is interrupted otherwise (Close, a crash) is resumed by the next
// Open. A recording that is already being deleted is reported as not found.
func (s *SQLite) DeleteRecording(ctx context.Context, id string) error {
	res, err := s.wdb.ExecContext(ctx,
		`UPDATE recordings SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, nanos(s.nowUTC()), id)
	if err != nil {
		return fmt.Errorf("store: delete recording %q: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return notFound("recording", id)
	}

	if err := s.purgeRecording(ctx, id); err != nil {
		if ctx.Err() != nil {
			s.purgeInBackground(id)
		}
		return fmt.Errorf("store: delete recording %q: %w", id, err)
	}
	s.reclaimSpace(ctx)
	return nil
}

// purgeRecording deletes the exchanges of a tombstoned recording chunk by
// chunk and then the recording row. It is idempotent, so an interrupted purge
// can simply be run again.
func (s *SQLite) purgeRecording(ctx context.Context, id string) error {
	chunk := max(s.deleteChunk, 1)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Each Exec is its own transaction on the writer connection, so the
		// next waiting writer gets in before the following chunk.
		res, err := s.wdb.ExecContext(ctx, `
			DELETE FROM exchanges WHERE id IN (
				SELECT id FROM exchanges WHERE recording_id = ? LIMIT ?)`, id, chunk)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if s.afterPurgeChunk != nil {
			s.afterPurgeChunk(id, int(n))
		}
		if n < int64(chunk) {
			break
		}
	}
	// The cascade now finds no exchanges left to delete.
	if _, err := s.wdb.ExecContext(ctx,
		`DELETE FROM recordings WHERE id = ? AND deleted_at IS NOT NULL`, id); err != nil {
		return err
	}
	return nil
}

// purgeInBackground finishes purging the given tombstoned recordings on a
// goroutine that Close cancels and waits for. Errors are dropped: the
// tombstone stays and the next Open retries.
func (s *SQLite) purgeInBackground(ids ...string) {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	if s.bgClosed {
		return
	}
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		for _, id := range ids {
			if err := s.purgeRecording(s.bgCtx, id); err != nil {
				return
			}
		}
		s.reclaimSpace(s.bgCtx)
	}()
}

// resumePurges looks for recordings a previous process tombstoned but did not
// finish deleting and purges them in the background.
func (s *SQLite) resumePurges(ctx context.Context) error {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id FROM recordings WHERE deleted_at IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("store: find half-deleted recordings: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) > 0 {
		s.purgeInBackground(ids...)
	}
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
