package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/justinburger/mirage/internal/fault"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/ops"
)

// FaultRuleWithCount is the API form of a fault rule: every model.FaultRule
// field, flattened, plus how often the rule has fired since it was loaded.
type FaultRuleWithCount struct {
	model.FaultRule
	Fired int64 `json:"fired"`
}

// listFaults answers GET /api/v1/faults.
func (s *Server) listFaults(w http.ResponseWriter, r *http.Request) error {
	rules, err := s.deps.Store.ListFaultRules(r.Context())
	if err != nil {
		return err
	}
	counts := s.deps.Faults.Counts()
	out := make([]FaultRuleWithCount, 0, len(rules))
	for _, rule := range rules {
		out = append(out, FaultRuleWithCount{FaultRule: rule, Fired: counts[rule.ID]})
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// getFault answers GET /api/v1/faults/{id}.
func (s *Server) getFault(w http.ResponseWriter, r *http.Request) error {
	rule, err := s.deps.Store.GetFaultRule(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, FaultRuleWithCount{FaultRule: rule, Fired: s.deps.Faults.Counts()[rule.ID]})
	return nil
}

// createFault answers POST /api/v1/faults.
func (s *Server) createFault(w http.ResponseWriter, r *http.Request) error {
	var in model.FaultRule
	if err := decodeJSON(w, r, &in, false, echoFaultFired); err != nil {
		return err
	}
	if in.ID != "" {
		if err := checkID(in.ID); err != nil {
			return err
		}
	}
	rule, err := checkFault(in)
	if err != nil {
		return err
	}
	s.faultMu.Lock()
	defer s.faultMu.Unlock()
	if rule.ID != "" {
		if _, err := s.deps.Store.GetFaultRule(r.Context(), rule.ID); err == nil {
			return newHTTPError(http.StatusConflict, "fault rule %q already exists", rule.ID)
		} else if !errors.Is(err, model.ErrNotFound) {
			return err
		}
	}
	stored, err := s.saveFault(r.Context(), rule)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, stored)
	return nil
}

// updateFault answers PUT /api/v1/faults/{id}.
func (s *Server) updateFault(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	s.faultMu.Lock()
	defer s.faultMu.Unlock()

	if _, err := s.deps.Store.GetFaultRule(r.Context(), id); err != nil {
		return err
	}
	var in model.FaultRule
	if err := decodeJSON(w, r, &in, false, echoFaultFired); err != nil {
		return err
	}
	in.ID = id
	rule, err := checkFault(in)
	if err != nil {
		return err
	}
	stored, err := s.saveFault(r.Context(), rule)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, stored)
	return nil
}

// deleteFault answers DELETE /api/v1/faults/{id}.
func (s *Server) deleteFault(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	s.faultMu.Lock()
	defer s.faultMu.Unlock()

	if _, err := s.deps.Store.GetFaultRule(r.Context(), id); err != nil {
		return err
	}
	dctx, cancel := detached(r.Context())
	defer cancel()
	if err := s.deps.Store.DeleteFaultRule(dctx, id); err != nil {
		return err
	}
	if err := s.reloadFaults(dctx); err != nil {
		return err
	}
	writeNoContent(w)
	return nil
}

// checkFault applies defaults and validates a rule exactly as the injector
// will when it loads it, so that a rule which would be refused there is
// refused here, before it reaches the store.
func checkFault(in model.FaultRule) (model.FaultRule, error) {
	rule := in.WithDefaults()
	if err := fault.NewInjector().SetRules([]model.FaultRule{rule}); err != nil {
		return rule, err
	}
	return rule, nil
}

// saveFault stores a checked rule and reloads the injector. The caller holds
// faultMu, which keeps the store and the injector in step.
func (s *Server) saveFault(ctx context.Context, rule model.FaultRule) (FaultRuleWithCount, error) {
	dctx, cancel := detached(ctx)
	defer cancel()
	stored, err := s.deps.Store.PutFaultRule(dctx, rule)
	if err != nil {
		return FaultRuleWithCount{}, err
	}
	if err := s.reloadFaults(dctx); err != nil {
		return FaultRuleWithCount{}, err
	}
	return FaultRuleWithCount{FaultRule: stored, Fired: s.deps.Faults.Counts()[stored.ID]}, nil
}

// reloadFaults copies the stored rules into the injector.
func (s *Server) reloadFaults(ctx context.Context) error {
	return ops.LoadFaults(ctx, s.deps.Store, s.deps.Faults)
}
