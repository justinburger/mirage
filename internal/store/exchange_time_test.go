package store

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// A StartedAt that Unix nanoseconds in an int64 cannot hold would be stored as
// the instant time.Time.UnixNano wraps it to. AppendExchanges refuses the whole
// batch instead.
func TestAppendExchangesRefusesStartTimesOutOfRange(t *testing.T) {
	maxT := time.Unix(0, math.MaxInt64)
	minT := time.Unix(0, math.MinInt64)
	backends(t, func(t *testing.T, s *SQLite) {
		rec := newRecording(t, s, "r")
		cases := []struct {
			name string
			at   time.Time
		}{
			{"year 2300", time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC)},
			{"year 9999", time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)},
			{"year 1500", time.Date(1500, 6, 1, 0, 0, 0, 0, time.UTC)},
			{"year 1", time.Date(1, 1, 2, 0, 0, 0, 0, time.UTC)}, // not the zero time
			{"year -4000", time.Date(-4000, 1, 1, 0, 0, 0, 0, time.UTC)},
			{"one nanosecond after the last instant", maxT.Add(time.Nanosecond)},
			{"one nanosecond before the first instant", minT.Add(-time.Nanosecond)},
			{"in another zone", time.Date(2300, 1, 1, 0, 0, 0, 0, time.FixedZone("x", 5*3600))},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				batch := []*model.Exchange{mkEx(0), mkEx(1), mkEx(2, withStart(c.at)), mkEx(3)}
				err := s.AppendExchanges(ctx(), rec.ID, batch)
				if !errors.Is(err, model.ErrValidation) {
					t.Fatalf("err = %v, want ErrValidation", err)
				}
				var ve *model.ValidationError
				if !errors.As(err, &ve) || len(ve.Problems) != 1 || !strings.Contains(ve.Problems[0], "exchanges[2]") ||
					!strings.Contains(ve.Problems[0], "started_at") {
					t.Errorf("err = %v, want one problem naming exchanges[2].started_at", err)
				}
				for i, ex := range batch {
					if ex.ID != 0 || ex.Seq != 0 || ex.RecordingID != "" {
						t.Errorf("exchange %d was modified by a refused batch: %+v", i, ex)
					}
				}
				got, _ := s.GetRecording(ctx(), rec.ID)
				if got.ExchangeCount != 0 || got.FirstAt != nil || got.LastAt != nil || got.Bytes != 0 {
					t.Errorf("a refused batch changed the recording: %+v", got)
				}
				if n := countRows(t, s, "exchanges"); n != 0 {
					t.Errorf("%d exchange rows after a refused batch", n)
				}
			})
		}

		t.Run("at most five problems are listed", func(t *testing.T) {
			var batch []*model.Exchange
			for i := range 8 {
				batch = append(batch, mkEx(i, withStart(time.Date(3000+i, 1, 1, 0, 0, 0, 0, time.UTC))))
			}
			var ve *model.ValidationError
			err := s.AppendExchanges(ctx(), rec.ID, batch)
			if !errors.As(err, &ve) || len(ve.Problems) != 6 || !strings.Contains(ve.Problems[5], "3 more") {
				t.Fatalf("err = %v, want five problems and a count of the rest", err)
			}
		})

		t.Run("the edges of the range and the zero time are stored", func(t *testing.T) {
			zero := mkEx(0, withStart(time.Time{}))
			last, first := mkEx(1, withStart(maxT)), mkEx(2, withStart(minT))
			if err := s.AppendExchanges(ctx(), rec.ID, []*model.Exchange{zero, last, first}); err != nil {
				t.Fatalf("in-range batch refused: %v", err)
			}
			for _, c := range []struct {
				ex   *model.Exchange
				want time.Time
			}{{zero, time.Time{}}, {last, maxT}, {first, minT}} {
				got, err := s.GetExchange(ctx(), c.ex.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !got.StartedAt.Equal(c.want) {
					t.Errorf("StartedAt round trip = %v, want %v", got.StartedAt, c.want)
				}
			}
			rc, _ := s.GetRecording(ctx(), rec.ID)
			if rc.ExchangeCount != 3 || rc.FirstAt == nil || !rc.FirstAt.Equal(minT) || rc.LastAt == nil || !rc.LastAt.Equal(maxT) {
				t.Errorf("recording range = %v .. %v (%d exchanges), want %v .. %v (3)", rc.FirstAt, rc.LastAt, rc.ExchangeCount, minT, maxT)
			}
		})
	})
}
