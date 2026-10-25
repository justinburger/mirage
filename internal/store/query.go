package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/justinburger/mirage/internal/model"
)

const (
	defaultListLimit = 100
	maxListLimit     = 1000
)

const summaryCols = `id, seq, started_at, duration, method, path, raw_query, status,
	endpoint, req_bytes, resp_bytes, content_type, error`

// filterSQL builds the WHERE clause (always anchored on recording_id) for an
// ExchangeQuery. unfiltered reports that only the recording restricts the
// result, in which case the total is the recording's exchange_count. The
// literal "is_error = 1" lets SQLite use the partial errors index.
func filterSQL(recordingID string, q model.ExchangeQuery) (where string, args []any, unfiltered bool) {
	var b strings.Builder
	b.WriteString("recording_id = ?")
	args = append(args, recordingID)
	unfiltered = true
	if q.Method != "" {
		b.WriteString(" AND method = ? COLLATE NOCASE")
		args = append(args, q.Method)
		unfiltered = false
	}
	if q.Status != 0 {
		b.WriteString(" AND status = ?")
		args = append(args, q.Status)
		unfiltered = false
	}
	if q.StatusClass > 0 {
		b.WriteString(" AND status >= ? AND status < ?")
		args = append(args, q.StatusClass*100, (q.StatusClass+1)*100)
		unfiltered = false
	}
	if q.PathContains != "" {
		// instr, unlike LIKE, treats % and _ literally and is case sensitive.
		b.WriteString(" AND instr(path || CASE WHEN raw_query = '' THEN '' ELSE '?' || raw_query END, ?) > 0")
		args = append(args, q.PathContains)
		unfiltered = false
	}
	if q.Endpoint != "" {
		b.WriteString(" AND endpoint = ?")
		args = append(args, q.Endpoint)
		unfiltered = false
	}
	if q.ErrorsOnly {
		b.WriteString(" AND is_error = 1")
		unfiltered = false
	}
	return b.String(), args, unfiltered
}

// ListExchanges returns one page of exchange summaries of a recording in Seq
// order (newest first with q.Desc) plus the total number of exchanges matching
// the filters. Limit defaults to 100 and is capped at 1000. Bodies and headers
// are never read. An unknown recording returns an error wrapping
// model.ErrNotFound.
func (s *SQLite) ListExchanges(ctx context.Context, recordingID string, q model.ExchangeQuery) ([]model.ExchangeSummary, int64, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	limit = min(limit, maxListLimit)
	offset := max(q.Offset, 0)

	where, args, unfiltered := filterSQL(recordingID, q)
	order := "ASC"
	if q.Desc {
		order = "DESC"
	}

	var (
		items = []model.ExchangeSummary{}
		total int64
	)
	err := s.read(ctx, func(tx *sql.Tx) error {
		// One snapshot for the existence check, the count and the page.
		err := tx.QueryRowContext(ctx, `SELECT exchange_count FROM recordings WHERE id = ?`, recordingID).Scan(&total)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("recording", recordingID)
		}
		if err != nil {
			return fmt.Errorf("store: list exchanges: %w", err)
		}
		if !unfiltered {
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM exchanges WHERE "+where, args...).Scan(&total); err != nil {
				return fmt.Errorf("store: count exchanges: %w", err)
			}
		}
		if total == 0 || int64(offset) >= total {
			return nil
		}

		rows, err := tx.QueryContext(ctx,
			"SELECT "+summaryCols+" FROM exchanges WHERE "+where+" ORDER BY seq "+order+" LIMIT ? OFFSET ?",
			append(args, limit, offset)...)
		if err != nil {
			return fmt.Errorf("store: list exchanges: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				it                  model.ExchangeSummary
				startedAt, duration int64
			)
			if err := rows.Scan(&it.ID, &it.Seq, &startedAt, &duration, &it.Method, &it.Path, &it.RawQuery,
				&it.Status, &it.Endpoint, &it.ReqBytes, &it.RespBytes, &it.ContentType, &it.Error); err != nil {
				return fmt.Errorf("store: list exchanges: %w", err)
			}
			it.StartedAt, it.Duration = fromNanos(startedAt), model.Duration(duration)
			items = append(items, it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

const (
	// iterChunkRows and iterChunkBytes bound how much of a recording
	// IterateExchanges holds in memory at once.
	iterChunkRows  = 256
	iterChunkBytes = 8 << 20
)

// iterSQL builds the chunk query for IterateExchanges. Bodies that the caller
// did not ask for are selected as NULL, so they are never read from disk.
func iterSQL(o model.IterOptions) string {
	req, resp := "NULL", "NULL"
	if o.RequestBodies {
		req = "req_body"
	}
	if o.ResponseBodies {
		resp = "resp_body"
	}
	return `SELECT ` + fullCols + `, ` + req + `, ` + resp + `
		FROM exchanges WHERE recording_id = ? AND seq > ? AND seq <= ? ORDER BY seq LIMIT ?`
}

// IterateExchanges calls fn for every exchange of the recording in Seq order.
// Bodies are loaded only if requested in opts; otherwise Request.Body and
// Response.Body are nil (BodySize is still set). The recording is read in
// small keyset-paginated chunks, so memory stays bounded however large it is
// and no read transaction stays open while fn runs (a slow consumer, such as a
// replay waiting on the clock, does not pin the WAL). Exchanges appended after
// the call began are not visited. fn may call other Store methods. If fn
// returns an error, iteration stops and that exact error is returned; a
// cancelled ctx stops it with ctx's error. An unknown recording, or one that is
// deleted while being iterated, returns an error wrapping model.ErrNotFound.
func (s *SQLite) IterateExchanges(ctx context.Context, recordingID string, opts model.IterOptions, fn func(*model.Exchange) error) error {
	var upTo int64
	err := s.rdb.QueryRowContext(ctx, `
		SELECT COALESCE((SELECT MAX(seq) FROM exchanges WHERE recording_id = r.id), 0)
		FROM recordings r WHERE r.id = ?`, recordingID).Scan(&upTo)
	if errors.Is(err, sql.ErrNoRows) {
		return notFound("recording", recordingID)
	}
	if err != nil {
		return fmt.Errorf("store: iterate exchanges: %w", err)
	}

	query := iterSQL(opts)
	var after int64
	for after < upTo {
		chunk, err := s.readChunk(ctx, query, recordingID, after, upTo)
		if err != nil {
			return err
		}
		if len(chunk) == 0 {
			// Rows we saw at the start are gone: the recording was deleted
			// while we were reading it.
			return notFound("recording", recordingID)
		}
		for _, e := range chunk {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := fn(e); err != nil {
				return err
			}
		}
		after = chunk[len(chunk)-1].Seq
	}
	return nil
}

// readChunk reads up to iterChunkRows exchanges (and about iterChunkBytes of
// body data) after seq `after`, releasing the connection before returning.
func (s *SQLite) readChunk(ctx context.Context, query, recordingID string, after, upTo int64) ([]*model.Exchange, error) {
	rows, err := s.rdb.QueryContext(ctx, query, recordingID, after, upTo, iterChunkRows)
	if err != nil {
		return nil, fmt.Errorf("store: iterate exchanges: %w", err)
	}
	defer rows.Close()
	var (
		chunk []*model.Exchange
		bytes int
	)
	for rows.Next() {
		e, err := scanExchange(rows)
		if err != nil {
			return nil, fmt.Errorf("store: iterate exchanges: %w", err)
		}
		chunk = append(chunk, e)
		bytes += len(e.Request.Body) + len(e.Response.Body)
		if bytes >= iterChunkBytes {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate exchanges: %w", err)
	}
	return chunk, nil
}
