package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/ops"
	"github.com/justinburger/mirage/internal/pathnorm"
)

const (
	maxNameLen        = 200
	maxLabels         = 50
	maxLabelKeyLen    = 64
	maxLabelValueLen  = 256
	maxFormFieldBytes = 4 << 10
)

// exchangePage is the paginated exchange list.
type exchangePage struct {
	Items []model.ExchangeSummary `json:"items"`
	Total int64                   `json:"total"`
}

// listRecordings answers GET /api/v1/recordings (newest first).
func (s *Server) listRecordings(w http.ResponseWriter, r *http.Request) error {
	recs, err := s.deps.Store.ListRecordings(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, nonNil(recs))
	return nil
}

// recordingBody is the body of POST /recordings and PATCH /recordings/{id}.
// Pointers tell "absent" from "empty" for the PATCH.
type recordingBody struct {
	Name        *string           `json:"name"`
	Description *string           `json:"description"`
	Labels      map[string]string `json:"labels"`
}

// validate lists problems with the fields that are present.
func (b recordingBody) validate(nameRequired bool) []string {
	var problems []string
	switch {
	case b.Name == nil:
		if nameRequired {
			problems = append(problems, "name is required")
		}
	case strings.TrimSpace(*b.Name) == "":
		problems = append(problems, "name must not be blank")
	case utf8.RuneCountInString(*b.Name) > maxNameLen:
		problems = append(problems, fmt.Sprintf("name must be at most %d characters", maxNameLen))
	}
	if b.Description != nil && utf8.RuneCountInString(*b.Description) > 10_000 {
		problems = append(problems, "description must be at most 10000 characters")
	}
	if len(b.Labels) > maxLabels {
		problems = append(problems, fmt.Sprintf("at most %d labels are allowed", maxLabels))
	}
	for k, v := range b.Labels {
		if k == "" || utf8.RuneCountInString(k) > maxLabelKeyLen || utf8.RuneCountInString(v) > maxLabelValueLen {
			problems = append(problems, fmt.Sprintf("label %q: keys are 1-%d characters, values at most %d", k, maxLabelKeyLen, maxLabelValueLen))
			break
		}
	}
	return problems
}

// createRecording answers POST /api/v1/recordings: an empty recording.
func (s *Server) createRecording(w http.ResponseWriter, r *http.Request) error {
	var in recordingBody
	if err := decodeJSON(w, r, &in, false); err != nil {
		return err
	}
	if problems := in.validate(true); len(problems) > 0 {
		return invalid(problems...)
	}
	rec := model.Recording{Name: strings.TrimSpace(*in.Name), Source: "proxy", Labels: in.Labels}
	if in.Description != nil {
		rec.Description = *in.Description
	}
	out, err := s.deps.Store.CreateRecording(r.Context(), rec)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, out)
	return nil
}

// getRecording answers GET /api/v1/recordings/{id}.
func (s *Server) getRecording(w http.ResponseWriter, r *http.Request) error {
	rec, err := s.deps.Store.GetRecording(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rec)
	return nil
}

// patchRecording answers PATCH /api/v1/recordings/{id}: only the members
// present in the body change.
func (s *Server) patchRecording(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	rec, err := s.deps.Store.GetRecording(ctx, r.PathValue("id"))
	if err != nil {
		return err
	}
	var in recordingBody
	if err := decodeJSON(w, r, &in, false); err != nil {
		return err
	}
	if problems := in.validate(false); len(problems) > 0 {
		return invalid(problems...)
	}
	if in.Name != nil {
		rec.Name = strings.TrimSpace(*in.Name)
	}
	if in.Description != nil {
		rec.Description = *in.Description
	}
	if in.Labels != nil {
		rec.Labels = in.Labels
	}
	out, err := s.deps.Store.UpdateRecording(ctx, rec)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// deleteRecording answers DELETE /api/v1/recordings/{id}. A recording that a
// service is using, or a replay is about to use, cannot be deleted.
//
// The in-use check and the delete run under Server.recordingMu, held
// exclusively, which service saves and replay starts also take (shared) around
// their own reference check and write: a service cannot be enabled on, or
// pointed at, the recording between the check here and the delete, and the
// delete cannot slip in between theirs. A large recording takes a while to
// purge, during which service changes wait for the lock; that is the price of
// the guarantee.
func (s *Server) deleteRecording(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id := r.PathValue("id")
	s.recordingMu.Lock()
	defer s.recordingMu.Unlock()
	if _, err := s.deps.Store.GetRecording(ctx, id); err != nil {
		return err
	}
	if err := s.recordingInUse(ctx, id); err != nil {
		return err
	}
	if err := s.deps.Store.DeleteRecording(ctx, id); err != nil {
		return err
	}
	writeNoContent(w)
	return nil
}

// recordingInUse returns a conflict error when an enabled service in a mode
// that uses recordings points at the recording, or a replay job on it is
// pending or running.
func (s *Server) recordingInUse(ctx context.Context, id string) error {
	svcs, err := s.deps.Store.ListServices(ctx)
	if err != nil {
		return err
	}
	for _, svc := range svcs {
		svc = svc.WithDefaults()
		if svc.Enabled && svc.Mode.UsesRecording() && svc.RecordingID == id {
			return newHTTPError(http.StatusConflict,
				"recording %q is used by the enabled service %q (%s mode); disable the service or point it at another recording first", id, svc.Name, svc.Mode)
		}
	}
	jobs, err := s.rpls.List(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.Config.RecordingID == id && (job.Status == model.ReplayPending || job.Status == model.ReplayRunning) {
			return newHTTPError(http.StatusConflict,
				"recording %q is the source of the %s replay %q; stop or delete the replay first", id, job.Status, job.Name)
		}
	}
	return nil
}

// recordingEndpoints answers GET /api/v1/recordings/{id}/endpoints.
func (s *Server) recordingEndpoints(w http.ResponseWriter, r *http.Request) error {
	stats, err := s.deps.Store.EndpointStats(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, nonNil(stats))
	return nil
}

// listExchanges answers GET /api/v1/recordings/{id}/exchanges.
func (s *Server) listExchanges(w http.ResponseWriter, r *http.Request) error {
	query := r.URL.Query()
	q := model.ExchangeQuery{
		Method:       strings.ToUpper(query.Get("method")),
		PathContains: query.Get("path"),
		Endpoint:     query.Get("endpoint"),
		ErrorsOnly:   queryFlag(r, "errors"),
		Desc:         queryFlag(r, "desc"),
	}
	var problems []string
	intParam := func(name string, lo, hi int64) int {
		n, err := queryInt(r, name, 0)
		switch {
		case err != nil:
			problems = append(problems, err.Error())
		case n < lo || n > hi:
			problems = append(problems, fmt.Sprintf("query parameter %s must be between %d and %d", name, lo, hi))
		default:
			return int(n)
		}
		return 0
	}
	q.Status = intParam("status", 0, 599)
	q.StatusClass = intParam("status_class", 0, 5)
	q.Offset = intParam("offset", 0, 1<<31-1)
	q.Limit = intParam("limit", 0, 1<<31-1)
	if len(problems) > 0 {
		return invalid(problems...)
	}
	items, total, err := s.deps.Store.ListExchanges(r.Context(), r.PathValue("id"), q)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, exchangePage{Items: nonNil(items), Total: total})
	return nil
}

// getExchange answers GET /api/v1/exchanges/{id}.
func (s *Server) getExchange(w http.ResponseWriter, r *http.Request) error {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return newHTTPError(http.StatusNotFound, "exchange %q not found", raw)
	}
	ex, err := s.deps.Store.GetExchange(r.Context(), id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, ex.View())
	return nil
}

// anonymizeRequest is the body of POST /recordings/{id}/anonymize.
type anonymizeRequest struct {
	ProfileID string `json:"profile_id"`
	Name      string `json:"name"`
}

// anonymizeRecording answers POST /api/v1/recordings/{id}/anonymize: it
// copies the recording through an anonymization profile (default when absent)
// and returns the new recording. The copy runs synchronously, streaming, and
// stops if the client goes away.
func (s *Server) anonymizeRecording(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id := r.PathValue("id")
	var in anonymizeRequest
	if err := decodeJSON(w, r, &in, true); err != nil {
		return err
	}
	if utf8.RuneCountInString(in.Name) > maxNameLen {
		return invalid(fmt.Sprintf("name must be at most %d characters", maxNameLen))
	}
	if _, err := s.deps.Store.GetRecording(ctx, id); err != nil {
		return err
	}
	eng, err := s.loadEngine(ctx, in.ProfileID)
	if err != nil {
		return err
	}
	longRunning(w)
	rec, err := ops.CopyAnonymized(ctx, s.deps.Store, id, eng, strings.TrimSpace(in.Name), nil)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, rec)
	return nil
}

// exportRecording answers GET /api/v1/recordings/{id}/export.har, streaming
// the recording as a HAR download, anonymized on the fly when profile_id is
// given.
func (s *Server) exportRecording(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id := r.PathValue("id")
	rec, err := s.deps.Store.GetRecording(ctx, id)
	if err != nil {
		return err
	}
	var eng *anonymize.Engine
	if pid := r.URL.Query().Get("profile_id"); pid != "" {
		if eng, err = s.loadEngine(ctx, pid); err != nil {
			return err
		}
	}
	longRunning(w)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": downloadName(rec.Name, rec.ID) + ".har"}))
	if err := ops.ExportHAR(ctx, s.deps.Store, id, w, eng, s.deps.Version); err != nil {
		if started(w) {
			return err // fail aborts the connection: a truncated HAR must not look complete
		}
		h.Del("Content-Disposition")
		return err
	}
	return nil
}

// importRecording answers POST /api/v1/recordings/import. The body is the raw
// HAR JSON or a multipart/form-data upload with the HAR in the field "file";
// it is streamed to ops.ImportHAR and never held in memory as a whole.
// Query parameters: name (default: the uploaded file's name, else "import
// <timestamp>") and profile_id (anonymize while importing). The multipart
// form may also carry "name" and "profile_id" fields, provided they come
// before the file.
func (s *Server) importRecording(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	query := r.URL.Query()
	name, profileID := query.Get("name"), query.Get("profile_id")
	longRunning(w)
	r.Body = http.MaxBytesReader(w, r.Body, s.importLimit)

	var src io.Reader = r.Body
	multipart := false
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" {
		multipart = true
		file, form, err := nextFilePart(r)
		if err != nil {
			return err
		}
		src = file
		if name == "" {
			name = form["name"]
		}
		if name == "" {
			name = nameFromUpload(file.FileName())
		}
		if profileID == "" {
			profileID = form["profile_id"]
		}
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return invalid(fmt.Sprintf("name must be at most %d characters", maxNameLen))
	}
	var eng *anonymize.Engine
	if profileID != "" {
		var err error
		if eng, err = s.loadEngine(ctx, profileID); err != nil {
			return err
		}
	}
	// An empty body reads like an empty body of the JSON endpoints, not like
	// a HAR parse failure.
	br := bufio.NewReaderSize(src, 4<<10)
	if _, err := br.Peek(1); errors.Is(err, io.EOF) {
		if multipart {
			return badRequest("the uploaded file is empty; send a HAR document")
		}
		return badRequest("request body is empty; send a HAR document")
	}
	rec, err := ops.ImportHAR(ctx, s.deps.Store, br, strings.TrimSpace(name), eng, pathnorm.Default)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, model.ErrValidation) {
			return badRequest("the upload ended before the HAR document was complete")
		}
		var bad *ops.InvalidHARError
		if errors.As(err, &bad) {
			return &httpError{status: http.StatusBadRequest, msg: "invalid HAR", problems: bad.Problems}
		}
		return err
	}
	writeJSON(w, http.StatusCreated, rec)
	return nil
}

// loadEngine builds the engine of the profile a request names (the default
// profile when profileID is empty). A profile that does not exist is a bad
// request, like every other reference inside a request.
func (s *Server) loadEngine(ctx context.Context, profileID string) (*anonymize.Engine, error) {
	eng, err := ops.LoadEngine(ctx, s.deps.Store, profileID)
	if errors.Is(err, model.ErrNotFound) {
		if profileID == "" {
			profileID = model.DefaultProfileID
		}
		return nil, refMissing("profile_id", profileID)
	}
	return eng, err
}

// filePart is the multipart "file" part, with its name.
type filePart interface {
	io.Reader
	FileName() string
}

// nextFilePart advances through the multipart body to the part named "file",
// collecting the small text fields "name" and "profile_id" seen on the way.
// It streams (MultipartReader, never ParseMultipartForm).
func nextFilePart(r *http.Request) (filePart, map[string]string, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, nil, badRequest("invalid multipart upload: %v", err)
	}
	form := map[string]string{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, nil, badRequest(`multipart field "file" is required`)
		}
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				return nil, nil, err
			}
			return nil, nil, badRequest("invalid multipart upload: %v", err)
		}
		switch part.FormName() {
		case "file":
			return part, form, nil
		case "name", "profile_id":
			b, err := io.ReadAll(io.LimitReader(part, maxFormFieldBytes))
			if err != nil {
				return nil, nil, badRequest("invalid multipart upload: %v", err)
			}
			form[part.FormName()] = strings.TrimSpace(string(b))
		}
	}
}

// nameFromUpload turns an uploaded file name into a recording name.
func nameFromUpload(filename string) string {
	base := path.Base(strings.ReplaceAll(filename, `\`, "/"))
	if base == "." || base == "/" {
		return ""
	}
	for _, ext := range []string{".har", ".json"} {
		if len(base) > len(ext) && strings.EqualFold(base[len(base)-len(ext):], ext) {
			base = base[:len(base)-len(ext)]
			break
		}
	}
	return strings.TrimSpace(base)
}

// downloadName reduces a recording name to a safe download file name.
func downloadName(name, fallback string) string {
	var b strings.Builder
	lastUnderscore := true
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
			lastUnderscore = false
		case !lastUnderscore:
			b.WriteByte('_')
			lastUnderscore = true
		}
		if b.Len() >= 80 {
			break
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		return fallback
	}
	return out
}

// longRunning lifts the server's per-connection deadlines for handlers that
// stream for a long time (uploads, downloads, copies). Servers or recorders
// that cannot do this are left alone.
func longRunning(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
}
