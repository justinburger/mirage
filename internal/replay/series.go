package replay

import (
	"sync"

	"github.com/justinburger/mirage/internal/model"
)

// seriesBuf is a job's own per-second series. Points the collector closes are
// appended; when more than max would be kept, adjacent pairs are merged
// (halving the resolution) so a long soak keeps its beginning and stays
// bounded. Merged points sum counts and take the maximum of percentiles,
// maxima and the in-flight gauge; they keep the first point's timestamp.
type seriesBuf struct {
	mu     sync.Mutex
	max    int
	pts    []model.SeriesPoint
	stride int // raw one-second points per stored point
	acc    model.SeriesPoint
	accN   int // raw points folded into acc (< stride)
}

func newSeriesBuf(max int) *seriesBuf { return &seriesBuf{max: max, stride: 1} }

// mergePoints folds b (the later second) into a.
func mergePoints(a, b model.SeriesPoint) model.SeriesPoint {
	a.Requests += b.Requests
	a.Errors += b.Errors
	a.BytesIn += b.BytesIn
	a.BytesOut += b.BytesOut
	a.P50 = max(a.P50, b.P50)
	a.P95 = max(a.P95, b.P95)
	a.P99 = max(a.P99, b.P99)
	a.Max = max(a.Max, b.Max)
	a.InFlight = max(a.InFlight, b.InFlight)
	a.CorrectedP95 = max(a.CorrectedP95, b.CorrectedP95)
	a.CorrectedP99 = max(a.CorrectedP99, b.CorrectedP99)
	return a
}

// add appends one raw one-second point.
func (s *seriesBuf) add(p model.SeriesPoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accN == 0 {
		s.acc = p
	} else {
		s.acc = mergePoints(s.acc, p)
	}
	if s.accN++; s.accN < s.stride {
		return
	}
	s.pts = append(s.pts, s.acc)
	s.accN = 0
	if len(s.pts) > s.max {
		s.compact()
	}
}

// compact merges adjacent stored points and doubles the stride. An odd last
// point becomes the half-filled accumulator of the new, wider stride.
func (s *seriesBuf) compact() {
	n := len(s.pts)
	out := s.pts[:0]
	for i := 0; i+1 < n; i += 2 {
		out = append(out, mergePoints(s.pts[i], s.pts[i+1]))
	}
	if n%2 == 1 {
		s.acc, s.accN = s.pts[n-1], s.stride
	}
	s.pts = out
	s.stride *= 2
}

// since returns a copy of the points with T >= sinceMs, including the group
// still being filled. The result is never nil.
func (s *seriesBuf) since(sinceMs int64) []model.SeriesPoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.SeriesPoint, 0, len(s.pts)+1)
	for _, p := range s.pts {
		if p.T >= sinceMs {
			out = append(out, p)
		}
	}
	if s.accN > 0 && s.acc.T >= sinceMs {
		out = append(out, s.acc)
	}
	return out
}

// all returns every point.
func (s *seriesBuf) all() []model.SeriesPoint { return s.since(0) }
