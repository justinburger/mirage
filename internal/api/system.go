package api

import (
	"bytes"
	"math"
	"net/http"
	"runtime"
	"time"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/metrics"
)

// health answers GET /api/v1/health. It is open even when auth is on, so it
// reveals nothing but liveness, the version and the uptime.
func (s *Server) health(w http.ResponseWriter, r *http.Request) error {
	up := math.Round(time.Since(s.start).Seconds()*10) / 10
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"version":  s.deps.Version,
		"uptime_s": up,
	})
	return nil
}

// info answers GET /api/v1/info.
func (s *Server) info(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    s.deps.Version,
		"go_version": runtime.Version(),
		"data_path":  s.deps.DataPath,
		"started_at": s.start.UTC().Format(time.RFC3339),
		"auth":       s.hasToken,
		"detectors":  anonymize.Detectors(),
	})
	return nil
}

// prometheus answers GET /metrics. The text is produced into memory first so
// a failure still gets a proper error response.
func (s *Server) prometheus(w http.ResponseWriter, r *http.Request) error {
	var buf bytes.Buffer
	if err := s.deps.Metrics.WritePrometheus(&buf); err != nil {
		return err
	}
	w.Header().Set("Content-Type", metrics.PrometheusContentType)
	_, err := w.Write(buf.Bytes())
	return err
}
