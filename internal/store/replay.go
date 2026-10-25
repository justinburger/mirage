package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/justinburger/mirage/internal/model"
)

// replaySelect lists the columns of a replay job without its series.
const replaySelect = `SELECT data, created_at, started_at, ended_at FROM replay_jobs`

func scanReplayJob(sc scanner) (model.ReplayJob, error) {
	var (
		j              model.ReplayJob
		data           []byte
		created        int64
		started, ended *int64
	)
	if err := sc.Scan(&data, &created, &started, &ended); err != nil {
		return j, err
	}
	if err := json.Unmarshal(data, &j); err != nil {
		return j, fmt.Errorf("store: decode replay job: %w", err)
	}
	j.CreatedAt, j.StartedAt, j.EndedAt = fromNanos(created), timePtr(started), timePtr(ended)
	return j, nil
}

// ListReplayJobs returns every replay job, newest first, without Series.
func (s *SQLite) ListReplayJobs(ctx context.Context) ([]model.ReplayJob, error) {
	rows, err := s.rdb.QueryContext(ctx, replaySelect+` ORDER BY created_at DESC, rowid DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list replay jobs: %w", err)
	}
	defer rows.Close()
	out := []model.ReplayJob{}
	for rows.Next() {
		j, err := scanReplayJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// GetReplayJob returns the replay job with the given ID, including Series.
func (s *SQLite) GetReplayJob(ctx context.Context, id string) (model.ReplayJob, error) {
	var (
		j              model.ReplayJob
		data, series   []byte
		created        int64
		started, ended *int64
		gz             bool
	)
	err := s.rdb.QueryRowContext(ctx,
		`SELECT data, created_at, started_at, ended_at, series, series_gz FROM replay_jobs WHERE id = ?`, id).
		Scan(&data, &created, &started, &ended, &series, &gz)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ReplayJob{}, notFound("replay job", id)
	}
	if err != nil {
		return model.ReplayJob{}, fmt.Errorf("store: get replay job: %w", err)
	}
	if err := json.Unmarshal(data, &j); err != nil {
		return model.ReplayJob{}, fmt.Errorf("store: decode replay job: %w", err)
	}
	j.CreatedAt, j.StartedAt, j.EndedAt = fromNanos(created), timePtr(started), timePtr(ended)
	if len(series) > 0 {
		raw, err := unpackBody(series, gz)
		if err != nil {
			return model.ReplayJob{}, fmt.Errorf("store: replay job %q series: %w", id, err)
		}
		if err := json.Unmarshal(raw, &j.Series); err != nil {
			return model.ReplayJob{}, fmt.Errorf("store: decode replay job series: %w", err)
		}
	}
	return j, nil
}

// PutReplayJob inserts or updates a replay job by ID (assigning "rpl_..."
// when empty). The stored Series is replaced by j.Series, so a job that was
// read with ListReplayJobs (which omits Series) must not be written back
// unless the series should be dropped. The returned job echoes j with ID and
// CreatedAt filled in.
func (s *SQLite) PutReplayJob(ctx context.Context, j model.ReplayJob) (model.ReplayJob, error) {
	if j.ID == "" {
		j.ID = model.NewID("rpl")
	}
	now := s.nowUTC()
	j.CreatedAt = now

	var (
		series   any
		seriesGz bool
	)
	if len(j.Series) > 0 {
		raw, err := json.Marshal(j.Series)
		if err != nil {
			return model.ReplayJob{}, fmt.Errorf("store: encode replay series: %w", err)
		}
		packed, gz := packBody(raw)
		series, seriesGz = packed, gz
	}
	enc := j
	enc.Series = nil
	data, err := json.Marshal(enc)
	if err != nil {
		return model.ReplayJob{}, fmt.Errorf("store: encode replay job: %w", err)
	}

	var created int64
	err = s.wdb.QueryRowContext(ctx, `
		INSERT INTO replay_jobs (id, name, status, created_at, started_at, ended_at, data, series, series_gz)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name, status = excluded.status,
			started_at = excluded.started_at, ended_at = excluded.ended_at,
			data = excluded.data, series = excluded.series, series_gz = excluded.series_gz
		RETURNING created_at`,
		j.ID, j.Name, string(j.Status), nanos(now), nullNanos(j.StartedAt), nullNanos(j.EndedAt),
		string(data), series, b2i(seriesGz)).Scan(&created)
	if err != nil {
		return model.ReplayJob{}, fmt.Errorf("store: put replay job: %w", err)
	}
	j.CreatedAt = fromNanos(created)
	return j, nil
}

// DeleteReplayJob removes a replay job.
func (s *SQLite) DeleteReplayJob(ctx context.Context, id string) error {
	return s.deleteByID(ctx, "replay_jobs", "replay job", id)
}
