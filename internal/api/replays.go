package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
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

// sensitiveHeaderNames are the replay set_headers whose values never leave the
// server, whatever the anonymization profiles say. They are the credential
// headers every HTTP stack knows; sensitiveHeaderParts catch the many
// vendor-specific ones.
var sensitiveHeaderNames = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"cookie":              {},
	"set-cookie":          {},
	"x-api-key":           {},
	"x-auth-token":        {},
	"x-access-token":      {},
	"x-csrf-token":        {},
	"x-xsrf-token":        {},
}

// sensitiveHeaderParts are fragments of header names that mark a credential
// ("X-Amz-Security-Token", "X-Api-Secret", "Ocp-Apim-Subscription-Key", ...).
var sensitiveHeaderParts = []string{
	"auth", "token", "secret", "password", "passwd", "credential", "session",
	"cookie", "api-key", "apikey", "api_key", "private-key", "access-key", "signature",
}

// isSensitiveHeader reports whether the value of a replay set_header must be
// masked in API responses. The rule is fixed (names above, a name ending in
// "-key" or "_key", or a name containing one of the fragments), so editing
// the default anonymization profile changes neither what is masked nor what
// is shown.
func isSensitiveHeader(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if _, ok := sensitiveHeaderNames[n]; ok {
		return true
	}
	if n == "key" || strings.HasSuffix(n, "-key") || strings.HasSuffix(n, "_key") {
		return true
	}
	for _, part := range sensitiveHeaderParts {
		if strings.Contains(n, part) {
			return true
		}
	}
	return false
}

// publicJob returns job as it may be shown to clients: values of sensitive
// set_headers are replaced by "***" and the failure message loses the internal
// package prefixes of the errors it was built from. The header map is copied,
// never modified in place, because the job may share it with live state.
func publicJob(job model.ReplayJob) model.ReplayJob {
	if job.Error != "" {
		job.Error = cleanMessage(job.Error)
	}
	if len(job.Config.SetHeaders) == 0 {
		return job
	}
	masked := make(map[string]string, len(job.Config.SetHeaders))
	for name, value := range job.Config.SetHeaders {
		if isSensitiveHeader(name) {
			value = maskedValue
		}
		masked[name] = value
	}
	job.Config.SetHeaders = masked
	return job
}

// jobOut returns the job as shown to clients.
func (s *Server) jobOut(_ context.Context, job model.ReplayJob) model.ReplayJob {
	return publicJob(job)
}

// listReplays answers GET /api/v1/replays (newest first, without series).
func (s *Server) listReplays(w http.ResponseWriter, r *http.Request) error {
	jobs, err := s.rpls.List(r.Context())
	if err != nil {
		return err
	}
	out := make([]model.ReplayJob, 0, len(jobs))
	for _, job := range jobs {
		job.Series = nil
		out = append(out, publicJob(job))
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
// and checks the recording and profile it names; one that does not exist is a
// 400 like every reference in a request. With "start": true the job begins at
// once; if it cannot start (the recording is empty, say) the job is not left
// behind.
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
	if err := rejectMaskedHeaders(in.Config.SetHeaders); err != nil {
		return err
	}
	ctx := r.Context()
	job, err := s.rpls.Create(ctx, name, in.Config)
	if err != nil {
		return s.missingReplayReference(ctx, in.Config, err)
	}
	if in.Start {
		if err := s.startJob(ctx, job); err != nil {
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

// rejectMaskedHeaders refuses a sensitive set_headers value that is the mask
// the API itself returns. A client that fetches a job, edits it and posts it
// back would otherwise store "***" and send it to the target as the credential;
// clone a job to reuse its stored secrets unchanged.
func rejectMaskedHeaders(headers map[string]string) error {
	var problems []string
	for name, value := range headers {
		if value == maskedValue && isSensitiveHeader(name) {
			problems = append(problems, fmt.Sprintf(
				"config.set_headers %s is %q, the masked form the API returns; send the real value, or clone the replay to reuse the stored one",
				quote(name), maskedValue))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	slices.Sort(problems)
	return invalid(problems...)
}

// missingReplayReference turns the "not found" of Create into the 400 that
// names the recording or profile the config points at, which is what the
// client got wrong. Other errors pass through.
func (s *Server) missingReplayReference(ctx context.Context, cfg model.ReplayConfig, err error) error {
	if !errors.Is(err, model.ErrNotFound) {
		return err
	}
	var problems []string
	if cfg.RecordingID != "" {
		if _, gerr := s.deps.Store.GetRecording(ctx, cfg.RecordingID); errors.Is(gerr, model.ErrNotFound) {
			problems = append(problems, "recording_id "+quote(cfg.RecordingID)+" does not exist")
		}
	}
	if cfg.ProfileID != "" {
		if _, gerr := s.deps.Store.GetProfile(ctx, cfg.ProfileID); errors.Is(gerr, model.ErrNotFound) {
			problems = append(problems, "profile_id "+quote(cfg.ProfileID)+" does not exist")
		}
	}
	if len(problems) == 0 {
		return err
	}
	return invalid(problems...)
}

// startJob starts a job after checking what the run would otherwise discover
// asynchronously: a job whose recording is gone or empty has nothing to
// replay, and saying so now (409) beats a 2xx "running" that turns "failed" a
// moment later.
func (s *Server) startJob(ctx context.Context, job model.ReplayJob) error {
	rec, err := s.deps.Store.GetRecording(ctx, job.Config.RecordingID)
	switch {
	case errors.Is(err, model.ErrNotFound):
		return newHTTPError(http.StatusConflict,
			"the recording %q of replay %q no longer exists, so there is nothing to replay", job.Config.RecordingID, job.Name)
	case err != nil:
		return err
	case rec.ExchangeCount == 0:
		return newHTTPError(http.StatusConflict,
			"the recording %q is empty, so there is nothing to replay; record or import traffic first", rec.Name)
	}
	return s.rpls.Start(ctx, job.ID)
}

// startReplay answers POST /api/v1/replays/{id}/start (re-runs a finished job).
func (s *Server) startReplay(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	job, err := s.rpls.Get(r.Context(), id)
	if err != nil {
		return err
	}
	if err := s.startJob(r.Context(), job); err != nil {
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
		if errors.Is(err, model.ErrNotFound) {
			// The config is the stored job's, not the request's: a recording
			// deleted after the run is a conflict with the job's state.
			if _, gerr := s.deps.Store.GetRecording(ctx, orig.Config.RecordingID); errors.Is(gerr, model.ErrNotFound) {
				return newHTTPError(http.StatusConflict,
					"the recording %q of replay %q no longer exists, so it cannot be cloned", orig.Config.RecordingID, orig.Name)
			}
		}
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
