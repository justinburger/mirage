package api

import (
	"net/http"
	"time"

	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
)

// defaultSeriesWindow is how far back a service's series goes when the
// request gives no "since".
const defaultSeriesWindow = 15 * time.Minute

// statusClassKeys are the keys of StatsSnapshot.StatusClasses.
var statusClassKeys = []string{"1xx", "2xx", "3xx", "4xx", "5xx", "err"}

// allStats answers GET /api/v1/stats: a snapshot of every collector.
func (s *Server) allStats(w http.ResponseWriter, r *http.Request) error {
	cols := s.deps.Metrics.List()
	out := make([]model.StatsSnapshot, 0, len(cols))
	for _, c := range cols {
		out = append(out, c.Snapshot())
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// serviceStats answers GET /api/v1/services/{id}/stats. A service that exists
// but has no collector yet (it never saw traffic) has an empty snapshot.
func (s *Server) serviceStats(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if _, err := s.deps.Store.GetService(r.Context(), id); err != nil {
		return err
	}
	name := metrics.ServiceCollectorName(id)
	if c, ok := s.deps.Metrics.Get(name); ok {
		writeJSON(w, http.StatusOK, c.Snapshot())
		return nil
	}
	writeJSON(w, http.StatusOK, emptySnapshot(name, model.KindService, id))
	return nil
}

// serviceSeries answers GET /api/v1/services/{id}/series?since=<unix ms>.
func (s *Server) serviceSeries(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	since, err := queryInt(r, "since", time.Now().Add(-defaultSeriesWindow).UnixMilli())
	if err != nil {
		return err
	}
	if _, err := s.deps.Store.GetService(r.Context(), id); err != nil {
		return err
	}
	if c, ok := s.deps.Metrics.Get(metrics.ServiceCollectorName(id)); ok {
		writeJSON(w, http.StatusOK, nonNil(c.Series(since)))
		return nil
	}
	writeJSON(w, http.StatusOK, []model.SeriesPoint{})
	return nil
}

// emptySnapshot is what a collector that has seen nothing would report.
func emptySnapshot(name string, kind model.CollectorKind, id string) model.StatsSnapshot {
	classes := make(map[string]int64, len(statusClassKeys))
	for _, k := range statusClassKeys {
		classes[k] = 0
	}
	return model.StatsSnapshot{
		Name:          name,
		Kind:          kind,
		ID:            id,
		Since:         time.Now().UTC(),
		StatusClasses: classes,
	}
}
