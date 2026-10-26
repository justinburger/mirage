package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

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
	s.faultMu.Lock()
	defer s.faultMu.Unlock()
	rule, err := s.checkFault(r.Context(), in)
	if err != nil {
		return err
	}
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
	rule, err := s.checkFault(r.Context(), in)
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

// faultLabel is the "rules[0] "name" (id): " the injector puts before every
// problem of the one-rule list checkFault gives it. API clients never sent a
// list, and a rule name in the text confuses clients that match problems to
// fields by their wording.
var faultLabel = regexp.MustCompile(`^rules\[0\](?: "(?:[^"\\]|\\.)*")?(?: \([^)]*\))?: `)

// httpMethods are the methods a fault rule may name: RFC 9110 plus WebDAV.
var httpMethods = map[string]struct{}{
	"GET": {}, "HEAD": {}, "POST": {}, "PUT": {}, "PATCH": {}, "DELETE": {}, "OPTIONS": {}, "CONNECT": {}, "TRACE": {},
	"PROPFIND": {}, "PROPPATCH": {}, "MKCOL": {}, "COPY": {}, "MOVE": {}, "LOCK": {}, "UNLOCK": {}, "REPORT": {}, "SEARCH": {},
}

// checkFault applies defaults and validates a rule exactly as the injector
// will when it loads it, so that a rule which would be refused there is
// refused here, before it reaches the store. It also checks what the injector
// cannot: that the methods are HTTP methods and that the service the rule is
// scoped to exists. Every problem is reported together. The caller holds
// faultMu, which orders this check against a service's deletion.
func (s *Server) checkFault(ctx context.Context, in model.FaultRule) (model.FaultRule, error) {
	rule := in.WithDefaults()
	var problems []string
	if err := fault.NewInjector().SetRules([]model.FaultRule{rule}); err != nil {
		for _, p := range validationProblems(err) {
			// "invalid method" is covered, more strictly, below.
			if p = faultLabel.ReplaceAllString(p, ""); !strings.HasPrefix(p, "invalid method ") {
				problems = append(problems, p)
			}
		}
	}
	for _, m := range rule.Methods {
		if _, ok := httpMethods[strings.ToUpper(m)]; !ok {
			problems = append(problems, "methods: "+quote(m)+" is not an HTTP method (use GET, POST, PUT, PATCH, DELETE, ...)")
		}
	}
	if rule.ServiceID != "" {
		if _, err := s.deps.Store.GetService(ctx, rule.ServiceID); errors.Is(err, model.ErrNotFound) {
			problems = append(problems, "service_id "+quote(rule.ServiceID)+" does not exist")
		} else if err != nil {
			return rule, err
		}
	}
	if len(problems) > 0 {
		return rule, invalid(problems...)
	}
	return rule, nil
}

// deleteFaultsOf removes the fault rules scoped to a service, which would
// never fire again, and reloads the injector. The caller holds faultMu.
func (s *Server) deleteFaultsOf(ctx context.Context, serviceID string) error {
	rules, err := s.deps.Store.ListFaultRules(ctx)
	if err != nil {
		return err
	}
	removed := 0
	for _, rule := range rules {
		if rule.ServiceID != serviceID {
			continue
		}
		if err := s.deps.Store.DeleteFaultRule(ctx, rule.ID); err != nil && !errors.Is(err, model.ErrNotFound) {
			return err
		}
		removed++
	}
	if removed == 0 {
		return nil
	}
	s.log.Info("api: removed the fault rules of a deleted service", "service", serviceID, "rules", removed)
	return s.reloadFaults(ctx)
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
