package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
)

// ServiceWithStatus is the API form of a service: every model.Service field,
// flattened, plus its runtime "status".
type ServiceWithStatus struct {
	model.Service
	Status model.ServiceStatus `json:"status"`
}

// detachedTimeout bounds work that must finish even if the client goes away.
const detachedTimeout = 5 * time.Minute

// detached returns a context that keeps r's values but not its cancellation.
// Used after a change reached the store, so that the controller can still be
// brought in line with it.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), detachedTimeout)
}

// statusIndex returns the controller's known statuses by service ID.
func (s *Server) statusIndex() map[string]model.ServiceStatus {
	list := s.svcs.Statuses()
	idx := make(map[string]model.ServiceStatus, len(list))
	for _, st := range list {
		idx[st.ServiceID] = st
	}
	return idx
}

// statusOf returns the runtime status of svc, taken from known when present.
//
// status.mode is the mode of the running listener. When nothing is running
// (a disabled service, or one whose Apply failed) there is no listener to
// describe, so it follows the saved service; otherwise a failed Apply would
// leave the controller's previous mode next to the service's new one. A
// listener that is still running (an address change that could not bind keeps
// the old one) keeps reporting its own mode.
func (s *Server) statusOf(svc model.Service, known map[string]model.ServiceStatus) model.ServiceStatus {
	st, ok := known[svc.ID]
	if !ok {
		st = s.svcs.Status(svc.ID)
	}
	st.ServiceID = svc.ID
	if st.Mode == "" || !st.Running {
		st.Mode = svc.Mode
	}
	return st
}

func (s *Server) withStatus(svc model.Service) ServiceWithStatus {
	return ServiceWithStatus{Service: svc, Status: s.statusOf(svc, nil)}
}

// listServices answers GET /api/v1/services.
func (s *Server) listServices(w http.ResponseWriter, r *http.Request) error {
	svcs, err := s.deps.Store.ListServices(r.Context())
	if err != nil {
		return err
	}
	known := s.statusIndex()
	out := make([]ServiceWithStatus, 0, len(svcs))
	for _, svc := range svcs {
		out = append(out, ServiceWithStatus{Service: svc, Status: s.statusOf(svc, known)})
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// getService answers GET /api/v1/services/{id}.
func (s *Server) getService(w http.ResponseWriter, r *http.Request) error {
	svc, err := s.deps.Store.GetService(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.withStatus(svc))
	return nil
}

// createService answers POST /api/v1/services.
func (s *Server) createService(w http.ResponseWriter, r *http.Request) error {
	var in model.Service
	if err := decodeJSON(w, r, &in, false, echoServiceStatus); err != nil {
		return err
	}
	if in.ID == "" {
		in.ID = model.NewID("svc")
	} else if err := checkID(in.ID); err != nil {
		return err
	}
	defer s.svcLocks.Lock(in.ID)()

	ctx := r.Context()
	if _, err := s.deps.Store.GetService(ctx, in.ID); err == nil {
		return newHTTPError(http.StatusConflict, "service %q already exists", in.ID)
	} else if !errors.Is(err, model.ErrNotFound) {
		return err
	}
	out, err := s.saveService(ctx, in, true)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, out)
	return nil
}

// updateService answers PUT /api/v1/services/{id}. The body is the full
// service; the id in the URL wins.
func (s *Server) updateService(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	defer s.svcLocks.Lock(id)()

	ctx := r.Context()
	if _, err := s.deps.Store.GetService(ctx, id); err != nil {
		return err
	}
	var in model.Service
	if err := decodeJSON(w, r, &in, false, echoServiceStatus); err != nil {
		return err
	}
	in.ID = id
	out, err := s.saveService(ctx, in, true)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// deleteService answers DELETE /api/v1/services/{id}. The recordings the
// service wrote are kept; the fault rules scoped to it are deleted with it.
func (s *Server) deleteService(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	defer s.svcLocks.Lock(id)()

	ctx := r.Context()
	if _, err := s.deps.Store.GetService(ctx, id); err != nil {
		return err
	}
	dctx, cancel := detached(ctx)
	defer cancel()
	// A controller that never heard of the service has nothing to remove.
	if err := s.svcs.Remove(dctx, id); err != nil && !errors.Is(err, model.ErrNotFound) {
		return err
	}
	s.deps.Metrics.Remove(metrics.ServiceCollectorName(id))
	// Fault rules scoped to the service go with it. faultMu keeps a rule being
	// created for the service from slipping in between the two deletes.
	s.faultMu.Lock()
	defer s.faultMu.Unlock()
	if err := s.deleteFaultsOf(dctx, id); err != nil {
		return err
	}
	if err := s.deps.Store.DeleteService(dctx, id); err != nil {
		return err
	}
	writeNoContent(w)
	return nil
}

// modeRequest is the body of POST /api/v1/services/{id}/mode.
type modeRequest struct {
	Mode             model.Mode `json:"mode"`
	RecordingID      string     `json:"recording_id"`
	NewRecordingName string     `json:"new_recording_name"`
}

// setServiceMode answers POST /api/v1/services/{id}/mode: the shortcut that
// switches a service's mode, optionally pointing it at another recording or at
// a brand new one.
func (s *Server) setServiceMode(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	defer s.svcLocks.Lock(id)()

	ctx := r.Context()
	svc, err := s.deps.Store.GetService(ctx, id)
	if err != nil {
		return err
	}
	var req modeRequest
	if err := decodeJSON(w, r, &req, false); err != nil {
		return err
	}
	newName := strings.TrimSpace(req.NewRecordingName)
	var problems []string
	switch {
	case req.Mode == "":
		problems = append(problems, "mode is required")
	case !req.Mode.Valid():
		problems = append(problems, "mode "+quote(string(req.Mode))+" must be passthrough, record, virtualize or hybrid")
	}
	if req.RecordingID != "" && req.NewRecordingName != "" {
		problems = append(problems, "recording_id and new_recording_name are mutually exclusive")
	}
	if req.NewRecordingName != "" && newName == "" {
		problems = append(problems, "new_recording_name must not be blank")
	}
	if newName != "" && req.Mode.Valid() && !req.Mode.UsesRecording() {
		problems = append(problems, "mode "+string(req.Mode)+" does not use a recording; drop new_recording_name")
	}
	if len(problems) > 0 {
		return invalid(problems...)
	}

	svc.Mode = req.Mode
	if req.RecordingID != "" {
		svc.RecordingID = req.RecordingID
	}
	if newName == "" {
		out, err := s.saveService(ctx, svc, true)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}

	// Validate against a stand-in recording first, so a request that is going
	// to be refused anyway creates no recording.
	probe := svc
	probe.RecordingID = "rec_pending"
	if _, err := s.checkService(ctx, probe, false); err != nil {
		return err
	}
	rec, err := s.deps.Store.CreateRecording(ctx, model.Recording{
		Name:       newName,
		ServiceID:  svc.ID,
		Source:     "proxy",
		Anonymized: svc.AnonymizeCapture,
	})
	if err != nil {
		return err
	}
	svc.RecordingID = rec.ID
	out, err := s.saveService(ctx, svc, true)
	if err != nil {
		dctx, cancel := detached(ctx)
		defer cancel()
		if derr := s.deps.Store.DeleteRecording(dctx, rec.ID); derr != nil {
			s.log.Warn("api: cannot remove the recording created for a failed mode change", "recording", rec.ID, "error", derr)
		}
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// enableService answers POST /api/v1/services/{id}/enable.
func (s *Server) enableService(w http.ResponseWriter, r *http.Request) error {
	return s.setEnabled(w, r, true)
}

// disableService answers POST /api/v1/services/{id}/disable.
func (s *Server) disableService(w http.ResponseWriter, r *http.Request) error {
	return s.setEnabled(w, r, false)
}

func (s *Server) setEnabled(w http.ResponseWriter, r *http.Request, enabled bool) error {
	id := r.PathValue("id")
	defer s.svcLocks.Lock(id)()

	ctx := r.Context()
	svc, err := s.deps.Store.GetService(ctx, id)
	if err != nil {
		return err
	}
	svc.Enabled = enabled
	// Disabling must work on a service whose recording is gone; enabling must
	// not start one that cannot work.
	out, err := s.saveService(ctx, svc, enabled)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// reloadService answers POST /api/v1/services/{id}/reload: rebuild the
// service's virtual index from its recording.
func (s *Server) reloadService(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	defer s.svcLocks.Lock(id)()

	svc, err := s.deps.Store.GetService(r.Context(), id)
	if err != nil {
		return err
	}
	if err := s.svcs.Reload(r.Context(), id); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.withStatus(svc))
	return nil
}

// saveService validates svc (with reference checks when refs is set), stores it
// and applies it. The caller holds the service's lock.
func (s *Server) saveService(ctx context.Context, svc model.Service, refs bool) (ServiceWithStatus, error) {
	checked, err := s.checkService(ctx, svc, refs)
	if err != nil {
		return ServiceWithStatus{}, err
	}
	return s.persistService(ctx, checked)
}

// checkService applies defaults to svc and validates it. With refs set it also
// checks that the recording and profile it names exist. Problems are reported
// together as one validation error.
func (s *Server) checkService(ctx context.Context, svc model.Service, refs bool) (model.Service, error) {
	svc = svc.WithDefaults()
	problems := validationProblems(svc.Validate())
	if refs {
		more, err := s.referenceProblems(ctx, svc)
		if err != nil {
			return svc, err
		}
		problems = append(problems, more...)
	}
	if len(problems) > 0 {
		return svc, invalid(problems...)
	}
	return svc, nil
}

// referenceProblems lists the objects svc points at that do not exist.
func (s *Server) referenceProblems(ctx context.Context, svc model.Service) ([]string, error) {
	var problems []string
	if svc.RecordingID != "" {
		if _, err := s.deps.Store.GetRecording(ctx, svc.RecordingID); errors.Is(err, model.ErrNotFound) {
			problems = append(problems, "recording_id "+quote(svc.RecordingID)+" does not exist")
		} else if err != nil {
			return nil, err
		}
	}
	if svc.ProfileID != "" {
		if _, err := s.deps.Store.GetProfile(ctx, svc.ProfileID); errors.Is(err, model.ErrNotFound) {
			problems = append(problems, "profile_id "+quote(svc.ProfileID)+" does not exist")
		} else if err != nil {
			return nil, err
		}
	}
	return problems, nil
}

// persistService stores a checked service and applies it to the controller. The
// caller holds the service's lock. Once the store accepted the change the
// controller is brought in line even if the client has gone away. If Apply
// fails the service stays saved and the status carries the error.
func (s *Server) persistService(ctx context.Context, svc model.Service) (ServiceWithStatus, error) {
	dctx, cancel := detached(ctx)
	defer cancel()
	stored, err := s.deps.Store.PutService(dctx, svc)
	if err != nil {
		return ServiceWithStatus{}, err
	}
	applyErr := s.svcs.Apply(dctx, stored)
	out := s.withStatus(stored)
	if applyErr != nil {
		s.log.Warn("api: service saved but not applied", "service", stored.ID, "name", stored.Name, "error", applyErr)
		if out.Status.Error == "" {
			out.Status.Error = applyErr.Error()
		}
	}
	return out, nil
}

// quote formats a value for a problem message.
func quote(s string) string { return `"` + s + `"` }
