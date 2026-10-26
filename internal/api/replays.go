package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/justinburger/mirage/internal/model"
)

// maskedValue replaces the value of a sensitive replay header in responses.
const maskedValue = "***"

// createReplayRequest is the body of POST /api/v1/replays.
type createReplayRequest struct {
	Name   string             `json:"name"`
	Config model.ReplayConfig `json:"config"`
	// Start begins the run right after the job is created.
	Start bool `json:"start"`
}

// sensitiveHeaders returns the lower-cased names of headers whose values must
// not leave the server: the default profile's redact list, which is what
// anonymization treats as credentials.
func (s *Server) sensitiveHeaders(ctx context.Context) map[string]struct{} {
	names := model.DefaultAnonymizationProfile().RedactHeaders
	if p, err := s.deps.Store.GetProfile(ctx, model.DefaultProfileID); err == nil {
		names = p.RedactHeaders
	}
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[strings.ToLower(n)] = struct{}{}
	}
	return set
}

// maskJob returns job as it may be shown to clients: values of sensitive
// set_headers are replaced by "***". The map is copied, never modified in
// place, because the job may share it with live state.
func maskJob(job model.ReplayJob, sensitive map[string]struct{}) model.ReplayJob {
	if len(job.Config.SetHeaders) == 0 {
		return job
	}
	masked := make(map[string]string, len(job.Config.SetHeaders))
	for name, value := range job.Config.SetHeaders {
		if _, ok := sensitive[strings.ToLower(name)]; ok {
			value = maskedValue
		}
		masked[name] = value
	}
	job.Config.SetHeaders = masked
	return job
}

// jobOut returns the masked job.
func (s *Server) jobOut(ctx context.Context, job model.ReplayJob) model.ReplayJob {
	return maskJob(job, s.sensitiveHeaders(ctx))
}

// listReplays answers GET /api/v1/replays (newest first, without series).
func (s *Server) listReplays(w http.ResponseWriter, r *http.Request) error {
	jobs, err := s.rpls.List(r.Context())
	if err != nil {
		return err
	}
	sensitive := s.sensitiveHeaders(r.Context())
	out := make([]model.ReplayJob, 0, len(jobs))
	for _, job := range jobs {
		job.Series = nil
		out = append(out, maskJob(job, sensitive))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// getReplay answers GET /api/v1/replays/{id}.
func (s *Server) getReplay(w http.ResponseWriter, r *http.Request) error {
	job, err := s.rpls.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.jobOut(r.Context(), job))
	return nil
}

// createReplay answers POST /api/v1/replays. The manager validates the config
// and checks the recording and profile it names. With "start": true the job
// begins at once; if it cannot start, the job is not left behind.
func (s *Server) createReplay(w http.ResponseWriter, r *http.Request) error {
	var in createReplayRequest
	if err := decodeJSON(w, r, &in, false); err != nil {
		return err
	}
	name := strings.TrimSpace(in.Name)
	if utf8.RuneCountInString(name) > maxNameLen {
		return invalid(fmt.Sprintf("name must be at most %d characters", maxNameLen))
	}
	if name == "" {
		name = "replay " + time.Now().Format("2006-01-02 15:04:05")
	}
	ctx := r.Context()
	job, err := s.rpls.Create(ctx, name, in.Config)
	if err != nil {
		return err
	}
	if in.Start {
		if err := s.rpls.Start(ctx, job.ID); err != nil {
			dctx, cancel := detached(ctx)
			defer cancel()
			if derr := s.rpls.Delete(dctx, job.ID); derr != nil {
				s.log.Warn("api: cannot remove the replay that failed to start", "replay", job.ID, "error", derr)
			}
			return err
		}
		if fresh, err := s.rpls.Get(ctx, job.ID); err == nil {
			job = fresh
		}
	}
	writeJSON(w, http.StatusCreated, s.jobOut(ctx, job))
	return nil
}

// startReplay answers POST /api/v1/replays/{id}/start (re-runs a finished job).
func (s *Server) startReplay(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if err := s.rpls.Start(r.Context(), id); err != nil {
		return err
	}
	return s.writeJob(w, r, id)
}

// stopReplay answers POST /api/v1/replays/{id}/stop.
func (s *Server) stopReplay(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if err := s.rpls.Stop(id); err != nil {
		return err
	}
	return s.writeJob(w, r, id)
}

// writeJob responds with the current state of a job.
func (s *Server) writeJob(w http.ResponseWriter, r *http.Request, id string) error {
	job, err := s.rpls.Get(r.Context(), id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.jobOut(r.Context(), job))
	return nil
}

// cloneReplay answers POST /api/v1/replays/{id}/clone: a new pending job with
// the stored config of an existing one (including unmasked headers and the
// seed, so the clone replays the same schedule).
func (s *Server) cloneReplay(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &in, true); err != nil {
		return err
	}
	ctx := r.Context()
	orig, err := s.rpls.Get(ctx, r.PathValue("id"))
	if err != nil {
		return err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = orig.Name + " (copy)"
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return invalid(fmt.Sprintf("name must be at most %d characters", maxNameLen))
	}
	job, err := s.rpls.Create(ctx, name, orig.Config)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, s.jobOut(ctx, job))
	return nil
}

// deleteReplay answers DELETE /api/v1/replays/{id}.
func (s *Server) deleteReplay(w http.ResponseWriter, r *http.Request) error {
	if err := s.rpls.Delete(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	writeNoContent(w)
	return nil
}

// replaySeries answers GET /api/v1/replays/{id}/series?since=<unix ms>. Without
// "since" the whole run is returned.
func (s *Server) replaySeries(w http.ResponseWriter, r *http.Request) error {
	since, err := queryInt(r, "since", 0)
	if err != nil {
		return err
	}
	points, err := s.rpls.Series(r.Context(), r.PathValue("id"), since)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, nonNil(points))
	return nil
}
