package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/ops"
)

// profileBody is the request body for creating or replacing a profile.
type profileBody struct {
	model.AnonymizationProfile
	// RegenerateSecret asks for a new secret on PUT. New captures then
	// produce different tokens than the existing recordings contain.
	RegenerateSecret bool `json:"regenerate_secret"`
}

// publicProfile is the form of a profile that may leave the server: defaults
// filled in, so lists marshal as [] rather than null, and the secret replaced
// by has_secret.
func publicProfile(p model.AnonymizationProfile) model.AnonymizationProfile {
	return p.WithDefaults().Redacted()
}

// listProfiles answers GET /api/v1/profiles.
func (s *Server) listProfiles(w http.ResponseWriter, r *http.Request) error {
	profiles, err := s.deps.Store.ListProfiles(r.Context())
	if err != nil {
		return err
	}
	out := make([]model.AnonymizationProfile, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, publicProfile(p))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// getProfile answers GET /api/v1/profiles/{id}.
func (s *Server) getProfile(w http.ResponseWriter, r *http.Request) error {
	p, err := s.deps.Store.GetProfile(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, publicProfile(p))
	return nil
}

// createProfile answers POST /api/v1/profiles.
func (s *Server) createProfile(w http.ResponseWriter, r *http.Request) error {
	var in profileBody
	if err := decodeJSON(w, r, &in, false); err != nil {
		return err
	}
	p := in.AnonymizationProfile
	if p.ID != "" {
		if err := checkID(p.ID); err != nil {
			return err
		}
	}
	p = p.WithDefaults()
	if err := anonymize.Validate(p); err != nil {
		return err
	}
	s.profileMu.Lock()
	defer s.profileMu.Unlock()
	if p.ID != "" {
		if _, err := s.deps.Store.GetProfile(r.Context(), p.ID); err == nil {
			return newHTTPError(http.StatusConflict, "profile %q already exists", p.ID)
		} else if !errors.Is(err, model.ErrNotFound) {
			return err
		}
	}
	stored, err := s.deps.Store.PutProfile(r.Context(), p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, publicProfile(stored))
	return nil
}

// updateProfile answers PUT /api/v1/profiles/{id}. An empty secret keeps the
// stored one; regenerate_secret replaces it. Running services pick the change
// up through ServiceController.ProfileChanged.
func (s *Server) updateProfile(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	s.profileMu.Lock()
	defer s.profileMu.Unlock()

	if _, err := s.deps.Store.GetProfile(r.Context(), id); err != nil {
		return err
	}
	var in profileBody
	if err := decodeJSON(w, r, &in, false); err != nil {
		return err
	}
	p := in.AnonymizationProfile
	p.ID = id
	if in.RegenerateSecret {
		p.Secret = model.NewSecret()
	}
	p = p.WithDefaults()
	if err := anonymize.Validate(p); err != nil {
		return err
	}
	dctx, cancel := detached(r.Context())
	defer cancel()
	stored, err := s.deps.Store.PutProfile(dctx, p)
	if err != nil {
		return err
	}
	if err := s.svcs.ProfileChanged(dctx, id); err != nil {
		// The profile is saved; a service that could not adopt it reports that
		// in its own status.
		s.log.Warn("api: profile saved but a service did not pick it up", "profile", id, "error", err)
	}
	writeJSON(w, http.StatusOK, publicProfile(stored))
	return nil
}

// deleteProfile answers DELETE /api/v1/profiles/{id}.
func (s *Server) deleteProfile(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	s.profileMu.Lock()
	defer s.profileMu.Unlock()

	ctx := r.Context()
	if _, err := s.deps.Store.GetProfile(ctx, id); err != nil {
		return err
	}
	if id == model.DefaultProfileID {
		return newHTTPError(http.StatusConflict, "the default profile cannot be deleted")
	}
	if err := s.profileInUse(ctx, id); err != nil {
		return err
	}
	if err := s.deps.Store.DeleteProfile(ctx, id); err != nil {
		return err
	}
	writeNoContent(w)
	return nil
}

// profileInUse returns a conflict error naming a service or replay job that
// references the profile.
func (s *Server) profileInUse(ctx context.Context, id string) error {
	svcs, err := s.deps.Store.ListServices(ctx)
	if err != nil {
		return err
	}
	for _, svc := range svcs {
		if svc.ProfileID == id {
			return newHTTPError(http.StatusConflict, "profile %q is used by the service %q; change the service first", id, svc.Name)
		}
	}
	jobs, err := s.rpls.List(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.Config.ProfileID == id {
			return newHTTPError(http.StatusConflict, "profile %q is used by the replay %q; delete the replay first", id, job.Name)
		}
	}
	return nil
}

// listDetectors answers GET /api/v1/anonymize/detectors.
func (s *Server) listDetectors(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, anonymize.Detectors())
	return nil
}

// previewRequest is the body of POST /api/v1/anonymize/preview.
type previewRequest struct {
	// Profile is an inline (possibly unsaved) profile; ProfileID names a
	// stored one. Neither means the default profile.
	Profile   *model.AnonymizationProfile `json:"profile"`
	ProfileID string                      `json:"profile_id"`
	// ExchangeID names a stored exchange; Exchange is a sample to use instead.
	ExchangeID *int64              `json:"exchange_id"`
	Exchange   *model.ExchangeView `json:"exchange"`
}

// previewResponse is the answer of the preview.
type previewResponse struct {
	Exchange model.ExchangeView `json:"exchange"`
	Changes  []model.AnonChange `json:"changes"`
}

// previewAnonymization answers POST /api/v1/anonymize/preview: it runs a
// profile over one exchange (a copy; nothing is stored) and reports what
// changed.
func (s *Server) previewAnonymization(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var in previewRequest
	if err := decodeJSON(w, r, &in, false); err != nil {
		return err
	}
	if (in.ExchangeID == nil) == (in.Exchange == nil) {
		return invalid(`give exactly one of "exchange_id" and "exchange"`)
	}
	eng, err := s.previewEngine(ctx, in)
	if err != nil {
		return err
	}
	var ex *model.Exchange
	if in.ExchangeID != nil {
		if ex, err = s.deps.Store.GetExchange(ctx, *in.ExchangeID); errors.Is(err, model.ErrNotFound) {
			return refMissing("exchange_id", strconv.FormatInt(*in.ExchangeID, 10))
		} else if err != nil {
			return err
		}
	} else if ex, err = in.Exchange.Exchange(); err != nil {
		return invalid(fmt.Sprintf("exchange body is not valid base64: %v", err))
	}
	// The same steps as a copy or export, so the preview shows what one
	// would store: compressed bodies decoded (or dropped), then the rules.
	changes, err := ops.AnonymizeExchange(eng, ex, nil, true)
	if err != nil {
		return fmt.Errorf("anonymize preview: %w", err)
	}
	writeJSON(w, http.StatusOK, previewResponse{Exchange: ex.View(), Changes: nonNil(changes)})
	return nil
}

// previewEngine builds the engine for a preview request. An inline profile
// with the ID of a stored one and no secret of its own keeps the stored secret,
// so the preview of an edited profile shows the tokens its saved form would
// produce. Without any secret a per-server preview secret is used, which keeps
// repeated previews of an unsaved profile stable.
func (s *Server) previewEngine(ctx context.Context, in previewRequest) (*anonymize.Engine, error) {
	if in.Profile == nil {
		id := in.ProfileID
		if id == "" {
			id = model.DefaultProfileID
		}
		stored, err := s.deps.Store.GetProfile(ctx, id)
		if errors.Is(err, model.ErrNotFound) {
			return nil, refMissing("profile_id", id)
		} else if err != nil {
			return nil, err
		}
		return anonymize.New(stored.WithDefaults())
	}
	p := in.Profile.WithDefaults()
	if p.Secret == "" && p.ID != "" {
		stored, err := s.deps.Store.GetProfile(ctx, p.ID)
		switch {
		case err == nil:
			p.Secret = stored.Secret
		case !errors.Is(err, model.ErrNotFound):
			return nil, err
		}
	}
	if p.Secret == "" {
		p.Secret = s.previewSecret
	}
	return anonymize.New(p)
}
