package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// numExchangeArgs is the number of bound parameters of insertExchangeSQL.
const numExchangeArgs = 30

const insertExchangeSQL = `INSERT INTO exchanges (
	recording_id, seq, service_id, started_at, duration, ttfb, endpoint,
	method, path, raw_query, status, is_error, req_bytes, resp_bytes,
	content_type, error, anonymized,
	req_scheme, req_host, req_proto, req_encoding, req_truncated,
	resp_proto, resp_encoding, resp_truncated,
	req_headers, resp_headers, body_flags, req_body, resp_body
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// fullCols selects everything a model.Exchange carries except bodies; the
// body columns are appended by the caller. is_error and content_type are left
// out because they derive from the other columns.
const fullCols = `id, recording_id, seq, service_id, started_at, duration, ttfb, endpoint,
	method, path, raw_query, status, req_bytes, resp_bytes, error, anonymized,
	req_scheme, req_host, req_proto, req_encoding, req_truncated,
	resp_proto, resp_encoding, resp_truncated, req_headers, resp_headers, body_flags`

const getExchangeSQL = `SELECT ` + fullCols + `, req_body, resp_body FROM exchanges WHERE id = ?`

// insertRow is one exchange prepared for insertion: headers encoded, bodies
// compressed. Preparing happens before the write transaction starts so the
// single writer is never held up by CPU work.
type insertRow struct {
	ex                *model.Exchange
	reqHdr, respHdr   any
	reqBody, respBody any // []byte or nil
	flags             int64
	isErr             bool
	contentType       string
	captured          int64 // len(request body) + len(response body)
}

func prepareRow(ex *model.Exchange) (insertRow, error) {
	var (
		r   = insertRow{ex: ex, isErr: ex.IsError(), contentType: ex.Response.Header.Get("Content-Type")}
		err error
	)
	if r.reqHdr, err = encodeHeader(ex.Request.Header); err != nil {
		return r, err
	}
	if r.respHdr, err = encodeHeader(ex.Response.Header); err != nil {
		return r, err
	}
	if b, gz := packBody(ex.Request.Body); len(b) > 0 {
		r.reqBody = b
		if gz {
			r.flags |= flagReqGzip
		}
	}
	if b, gz := packBody(ex.Response.Body); len(b) > 0 {
		r.respBody = b
		if gz {
			r.flags |= flagRespGzip
		}
	}
	r.captured = int64(len(ex.Request.Body) + len(ex.Response.Body))
	return r, nil
}

// fill writes the statement arguments for this row into a.
func (r *insertRow) fill(a []any, recordingID string, seq int64) {
	e := r.ex
	a[0] = recordingID
	a[1] = seq
	a[2] = e.ServiceID
	a[3] = nanos(e.StartedAt)
	a[4] = int64(e.Duration)
	a[5] = int64(e.TTFB)
	a[6] = e.Endpoint
	a[7] = e.Request.Method
	a[8] = e.Request.Path
	a[9] = e.Request.RawQuery
	a[10] = int64(e.Response.Status)
	a[11] = b2i(r.isErr)
	a[12] = e.Request.BodySize
	a[13] = e.Response.BodySize
	a[14] = r.contentType
	a[15] = e.Error
	a[16] = b2i(e.Anonymized)
	a[17] = e.Request.Scheme
	a[18] = e.Request.Host
	a[19] = e.Request.Proto
	a[20] = e.Request.ContentEncoding
	a[21] = b2i(e.Request.BodyTruncated)
	a[22] = e.Response.Proto
	a[23] = e.Response.ContentEncoding
	a[24] = b2i(e.Response.BodyTruncated)
	a[25] = r.reqHdr
	a[26] = r.respHdr
	a[27] = r.flags
	a[28] = r.reqBody
	a[29] = r.respBody
}

const (
	// parallelPrepareBytes is the batch body size above which preparing rows
	// (mostly gzip) is spread over several goroutines.
	parallelPrepareBytes = 256 << 10
	maxPrepareWorkers    = 8
)

// Bounds of the times the store keeps: Unix nanoseconds in an int64 (the years
// 1678 to 2262). time.Time.UnixNano is undefined outside them, in practice it
// wraps around, so a time beyond them would be stored as a different instant.
var (
	minStoredTime = time.Unix(0, math.MinInt64)
	maxStoredTime = time.Unix(0, math.MaxInt64)
)

// maxTimeProblems caps how many out-of-range exchanges one error names.
const maxTimeProblems = 5

// checkStartTimes returns a validation error naming the exchanges of the batch
// whose StartedAt cannot be stored. The zero time means "unknown" and is stored
// as such.
func checkStartTimes(exs []*model.Exchange) error {
	var problems []string
	bad := 0
	for i, ex := range exs {
		t := ex.StartedAt
		if t.IsZero() || (!t.Before(minStoredTime) && !t.After(maxStoredTime)) {
			continue
		}
		if bad++; bad <= maxTimeProblems {
			problems = append(problems, fmt.Sprintf("exchanges[%d]: started_at %s is outside the range the store keeps (years 1678 to 2262)",
				i, t.Format(time.RFC3339Nano)))
		}
	}
	if bad > maxTimeProblems {
		problems = append(problems, fmt.Sprintf("and %d more exchanges with a started_at out of range", bad-maxTimeProblems))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("store: append exchanges: %w", &model.ValidationError{Problems: problems})
}

// prepareRows validates and prepares a batch, in parallel when the bodies are
// large enough for compression to dominate, and totals the counter deltas.
func prepareRows(exs []*model.Exchange) ([]insertRow, batchStats, error) {
	st := batchStats{anonymized: true}
	total := 0
	for i, ex := range exs {
		if ex == nil {
			return nil, st, fmt.Errorf("store: append exchanges: exchange %d is nil", i)
		}
		total += len(ex.Request.Body) + len(ex.Response.Body)
	}
	if err := checkStartTimes(exs); err != nil {
		return nil, st, err
	}

	rows := make([]insertRow, len(exs))
	errs := make([]error, len(exs))
	workers := min(runtime.GOMAXPROCS(0), maxPrepareWorkers, len(exs))
	if workers <= 1 || total < parallelPrepareBytes {
		for i, ex := range exs {
			rows[i], errs[i] = prepareRow(ex)
		}
	} else {
		var wg sync.WaitGroup
		per := (len(exs) + workers - 1) / workers
		for lo := 0; lo < len(exs); lo += per {
			hi := min(lo+per, len(exs))
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := lo; i < hi; i++ {
					rows[i], errs[i] = prepareRow(exs[i])
				}
			}()
		}
		wg.Wait()
	}

	for i := range rows {
		if errs[i] != nil {
			return nil, st, fmt.Errorf("store: append exchange %d: %w", i, errs[i])
		}
		r := &rows[i]
		st.count++
		st.bytes += r.captured
		if r.isErr {
			st.errors++
		}
		if !r.ex.Anonymized {
			st.anonymized = false
		}
		if t := nanos(r.ex.StartedAt); t != 0 {
			if st.first == 0 || t < st.first {
				st.first = t
			}
			st.last = max(st.last, t)
		}
	}
	return rows, st, nil
}

// batchStats are the recording counter deltas of one append.
type batchStats struct {
	count, errors, bytes int64
	first, last          int64 // Unix nanos of the earliest and latest StartedAt; 0 = none
	anonymized           bool  // every exchange of the batch has Anonymized set
}

// AppendExchanges writes a batch of exchanges to a recording in one
// transaction. On success each exchange has its ID (unique across all
// recordings), Seq (continuing the recording's 1-based sequence in slice
// order) and RecordingID set in place; on failure nothing is written and the
// exchanges are left untouched. The recording's ExchangeCount, ErrorCount,
// Bytes (captured body bytes), FirstAt (earliest StartedAt), LastAt (latest
// StartedAt) and UpdatedAt are updated in the same transaction, and so is the
// recording's Anonymized flag: appending any exchange with Anonymized false
// clears it, and appending to an empty recording sets it to whether every
// appended exchange is anonymized. A non-empty recording therefore stays
// anonymized only while every exchange it ever received was. An unknown (or
// being deleted) recording returns an error wrapping model.ErrNotFound; an
// empty batch is a no-op. An exchange whose StartedAt (when not the zero time)
// lies outside the years 1678 to 2262, which Unix nanoseconds in an int64 cannot
// hold, is refused with an error wrapping model.ErrValidation before anything
// is written: the whole batch is rejected, never stored as a wrapped-around
// instant.
func (s *SQLite) AppendExchanges(ctx context.Context, recordingID string, exs []*model.Exchange) error {
	if len(exs) == 0 {
		return nil
	}
	rows, st, err := prepareRows(exs)
	if err != nil {
		return err
	}

	var (
		base int64
		ids  = make([]int64, len(rows))
	)
	err = s.write(ctx, func(tx *sql.Tx) error {
		// The next sequence number and the existence check share one query;
		// MAX(seq) is answered from the (recording_id, seq) index.
		err := tx.QueryRowContext(ctx, `
			SELECT COALESCE((SELECT MAX(seq) FROM exchanges WHERE recording_id = r.id), 0)
			FROM recordings r WHERE r.id = ? AND r.deleted_at IS NULL`, recordingID).Scan(&base)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("recording", recordingID)
		}
		if err != nil {
			return fmt.Errorf("store: append exchanges: %w", err)
		}

		stmt := tx.StmtContext(ctx, s.insertExchange)
		defer stmt.Close()
		args := make([]any, numExchangeArgs)
		for i := range rows {
			rows[i].fill(args, recordingID, base+int64(i)+1)
			res, err := stmt.ExecContext(ctx, args...)
			if err != nil {
				return fmt.Errorf("store: insert exchange %d: %w", i, err)
			}
			if ids[i], err = res.LastInsertId(); err != nil {
				return fmt.Errorf("store: insert exchange %d: %w", i, err)
			}
		}

		var first, last any
		if st.first != 0 {
			first, last = st.first, st.last
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE recordings SET
				exchange_count = exchange_count + ?1,
				error_count    = error_count + ?2,
				bytes          = bytes + ?3,
				first_at = CASE WHEN ?4 IS NULL THEN first_at
				                WHEN first_at IS NULL OR ?4 < first_at THEN ?4 ELSE first_at END,
				last_at  = CASE WHEN ?5 IS NULL THEN last_at
				                WHEN last_at IS NULL OR ?5 > last_at THEN ?5 ELSE last_at END,
				anonymized = CASE WHEN exchange_count = 0 THEN ?8
				                  WHEN ?8 = 0 THEN 0
				                  ELSE anonymized END,
				updated_at = ?6
			WHERE id = ?7`,
			st.count, st.errors, st.bytes, first, last, nanos(s.nowUTC()), recordingID, b2i(st.anonymized))
		if err != nil {
			return fmt.Errorf("store: update recording counters: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i, ex := range exs {
		ex.ID, ex.Seq, ex.RecordingID = ids[i], base+int64(i)+1, recordingID
	}
	return nil
}

// exchangeScan holds the destinations of one full-exchange row so iteration
// can reuse them.
type exchangeScan struct {
	startedAt, duration, ttfb int64
	reqHdr, respHdr           []byte
	reqBody, respBody         []byte
	flags                     int64
}

// scanExchange scans one row selected with fullCols followed by req_body and
// resp_body, and assembles the exchange.
func scanExchange(sc scanner) (*model.Exchange, error) {
	var (
		e model.Exchange
		x exchangeScan
	)
	err := sc.Scan(&e.ID, &e.RecordingID, &e.Seq, &e.ServiceID, &x.startedAt, &x.duration, &x.ttfb, &e.Endpoint,
		&e.Request.Method, &e.Request.Path, &e.Request.RawQuery, &e.Response.Status,
		&e.Request.BodySize, &e.Response.BodySize, &e.Error, &e.Anonymized,
		&e.Request.Scheme, &e.Request.Host, &e.Request.Proto, &e.Request.ContentEncoding, &e.Request.BodyTruncated,
		&e.Response.Proto, &e.Response.ContentEncoding, &e.Response.BodyTruncated,
		&x.reqHdr, &x.respHdr, &x.flags, &x.reqBody, &x.respBody)
	if err != nil {
		return nil, err
	}
	return x.assemble(&e)
}

// assemble finishes an exchange from its scanned raw parts.
func (x *exchangeScan) assemble(e *model.Exchange) (*model.Exchange, error) {
	var err error
	e.StartedAt = fromNanos(x.startedAt)
	e.Duration, e.TTFB = model.Duration(x.duration), model.Duration(x.ttfb)
	if e.Request.Header, err = decodeHeader(x.reqHdr); err != nil {
		return nil, err
	}
	if e.Response.Header, err = decodeHeader(x.respHdr); err != nil {
		return nil, err
	}
	if e.Request.Body, err = unpackBody(x.reqBody, x.flags&flagReqGzip != 0); err != nil {
		return nil, err
	}
	if e.Response.Body, err = unpackBody(x.respBody, x.flags&flagRespGzip != 0); err != nil {
		return nil, err
	}
	return e, nil
}

// GetExchange returns one exchange with both bodies, or an error wrapping
// model.ErrNotFound.
func (s *SQLite) GetExchange(ctx context.Context, id int64) (*model.Exchange, error) {
	e, err := scanExchange(s.getExchange.QueryRowContext(ctx, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("exchange", fmt.Sprint(id))
	}
	if err != nil {
		return nil, fmt.Errorf("store: get exchange %d: %w", id, err)
	}
	return e, nil
}
