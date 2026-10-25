package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/justinburger/mirage/internal/model"
)

// endpointAcc accumulates one endpoint's numbers while the recording is
// scanned once.
type endpointAcc struct {
	count, errors, sum, max int64
	durations               []int64
	statuses                map[int]int64
}

// EndpointStats groups a recording's exchanges by Endpoint and computes the
// count, error count (model.Exchange.IsError semantics), error rate, average,
// p50/p95/p99 and maximum duration and the per-status counts of each group.
// Percentiles use the nearest-rank method on the exact durations. The result
// is sorted by count descending, then endpoint. An unknown recording returns
// an error wrapping model.ErrNotFound.
func (s *SQLite) EndpointStats(ctx context.Context, recordingID string) ([]model.EndpointStat, error) {
	groups := map[string]*endpointAcc{}
	err := s.read(ctx, func(tx *sql.Tx) error {
		var n int64
		err := tx.QueryRowContext(ctx, `SELECT exchange_count FROM recordings WHERE id = ?`, recordingID).Scan(&n)
		if errors.Is(err, sql.ErrNoRows) {
			return notFound("recording", recordingID)
		}
		if err != nil {
			return fmt.Errorf("store: endpoint stats: %w", err)
		}

		rows, err := tx.QueryContext(ctx,
			`SELECT endpoint, status, duration, is_error FROM exchanges WHERE recording_id = ?`, recordingID)
		if err != nil {
			return fmt.Errorf("store: endpoint stats: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				endpoint string
				status   int
				duration int64
				isErr    bool
			)
			if err := rows.Scan(&endpoint, &status, &duration, &isErr); err != nil {
				return fmt.Errorf("store: endpoint stats: %w", err)
			}
			g := groups[endpoint]
			if g == nil {
				g = &endpointAcc{statuses: map[int]int64{}}
				groups[endpoint] = g
			}
			g.count++
			g.sum += duration
			g.max = max(g.max, duration)
			g.durations = append(g.durations, duration)
			g.statuses[status]++
			if isErr {
				g.errors++
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}

	out := make([]model.EndpointStat, 0, len(groups))
	for endpoint, g := range groups {
		slices.Sort(g.durations)
		st := model.EndpointStat{
			Endpoint:     endpoint,
			Count:        g.count,
			Errors:       g.errors,
			ErrorRate:    float64(g.errors) / float64(g.count),
			Avg:          model.Duration(g.sum / g.count),
			P50:          model.Duration(percentile(g.durations, 50)),
			P95:          model.Duration(percentile(g.durations, 95)),
			P99:          model.Duration(percentile(g.durations, 99)),
			Max:          model.Duration(g.max),
			StatusCounts: make(map[string]int64, len(g.statuses)),
		}
		for code, n := range g.statuses {
			st.StatusCounts[strconv.Itoa(code)] = n
		}
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b model.EndpointStat) int {
		if a.Count != b.Count {
			return int(b.Count - a.Count)
		}
		switch {
		case a.Endpoint < b.Endpoint:
			return -1
		case a.Endpoint > b.Endpoint:
			return 1
		}
		return 0
	})
	return out, nil
}

// percentile returns the nearest-rank p-th percentile (0 < p <= 100) of a
// sorted, non-empty slice: the value at rank ceil(p/100 * n).
func percentile(sorted []int64, p int) int64 {
	n := len(sorted)
	rank := (n*p + 99) / 100
	return sorted[min(max(rank, 1), n)-1]
}
