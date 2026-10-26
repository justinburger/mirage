package config

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/ops"
	"github.com/justinburger/mirage/internal/store"
)

// harImporter imports a HAR document into a new recording named name.
type harImporter func(ctx context.Context, st store.Store, r io.Reader, name string) (model.Recording, error)

func defaultImportHAR(ctx context.Context, st store.Store, r io.Reader, name string) (model.Recording, error) {
	// No engine (the file is imported as is) and the default normalizer (nil).
	return ops.ImportHAR(ctx, st, r, name, nil, nil)
}

// Apply writes the configuration's objects to the store.
//
// Order: profiles, recordings (importing HAR files for new ones), services,
// fault rules. All validation and name resolution that can fail happens
// before the first write; if it fails nothing is applied and the returned
// error wraps model.ErrValidation. A store or HAR import error part-way
// returns the result so far with the error; applying again completes the
// job. Apply does not modify c.
func (c *Config) Apply(ctx context.Context, st store.Store) (ApplyResult, error) {
	res := ApplyResult{Created: []string{}, Updated: []string{}}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if err := c.Validate(); err != nil {
		return res, fmt.Errorf("config: %w", err)
	}
	a := &applier{c: c, st: st, res: &res}
	if err := a.load(ctx); err != nil {
		return res, err
	}
	if err := a.plan(); err != nil {
		return res, fmt.Errorf("config: %w", err)
	}
	if err := a.execute(ctx); err != nil {
		return res, err
	}
	return res, nil
}

// applier holds the state of one Apply call.
type applier struct {
	c   *Config
	st  store.Store
	res *ApplyResult

	// What the store holds before Apply.
	recByName   map[string]model.Recording // oldest recording of each name
	recByID     map[string]model.Recording
	profByName  map[string]model.AnonymizationProfile // by normName
	profByID    map[string]model.AnonymizationProfile
	svcByName   map[string]model.Service
	svcByID     map[string]model.Service
	faultByName map[string]model.FaultRule // first rule of each name
	faultByID   map[string]model.FaultRule
}

// load reads the existing objects.
func (a *applier) load(ctx context.Context) error {
	recs, err := a.st.ListRecordings(ctx)
	if err != nil {
		return fmt.Errorf("config: list recordings: %w", err)
	}
	profs, err := a.st.ListProfiles(ctx)
	if err != nil {
		return fmt.Errorf("config: list profiles: %w", err)
	}
	svcs, err := a.st.ListServices(ctx)
	if err != nil {
		return fmt.Errorf("config: list services: %w", err)
	}
	faults, err := a.st.ListFaultRules(ctx)
	if err != nil {
		return fmt.Errorf("config: list fault rules: %w", err)
	}

	a.recByName = make(map[string]model.Recording, len(recs))
	a.recByID = make(map[string]model.Recording, len(recs))
	for _, r := range recs { // newest first: the oldest of a name wins
		a.recByName[r.Name] = r
		a.recByID[r.ID] = r
	}
	a.profByName = make(map[string]model.AnonymizationProfile, len(profs))
	a.profByID = make(map[string]model.AnonymizationProfile, len(profs))
	for _, p := range profs {
		a.profByName[normName(p.Name)] = p
		a.profByID[p.ID] = p
	}
	a.svcByName = make(map[string]model.Service, len(svcs))
	a.svcByID = make(map[string]model.Service, len(svcs))
	for _, s := range svcs {
		a.svcByName[s.Name] = s
		a.svcByID[s.ID] = s
	}
	a.faultByName = make(map[string]model.FaultRule, len(faults))
	a.faultByID = make(map[string]model.FaultRule, len(faults))
	for _, f := range faults {
		if _, dup := a.faultByName[strings.TrimSpace(f.Name)]; !dup {
			a.faultByName[strings.TrimSpace(f.Name)] = f
		}
		a.faultByID[f.ID] = f
	}
	return nil
}

// plan resolves every reference that depends on the store's contents and
// reports all problems at once, before anything is written.
func (a *applier) plan() error {
	var p []string
	addf := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	c := a.c

	// Profiles: explicit ids must not collide with another object.
	fileProfiles := map[string]bool{} // normalised names available to services
	for k := range a.profByName {
		fileProfiles[k] = true
	}
	fileProfileIDs := map[string]bool{}
	for i, pr := range c.Profiles {
		label := fmt.Sprintf("profiles[%d] %q", i, pr.Name)
		ex, found := a.profByName[normName(pr.Name)]
		owner, owned := a.profByID[pr.ID]
		if msg := idProblem("profile", pr.ID, found, ex.ID, owner.Name, owned); msg != "" {
			addf("%s: %s", label, msg)
		}
		fileProfiles[normName(pr.Name)] = true
		if pr.ID != "" {
			fileProfileIDs[pr.ID] = true
		}
	}

	// Recordings: HAR files for recordings that will be created must exist.
	for i, rc := range c.Recordings {
		if _, exists := a.recByName[rc.Name]; exists || rc.HAR == "" {
			continue
		}
		if err := checkHARFile(c.harPath(rc.HAR)); err != nil {
			addf("recordings[%d] %q: %v", i, rc.Name, err)
		}
	}

	// Services: references, ids and the final set of listen addresses.
	fileServices := map[string]bool{}
	listen := map[string]string{} // listen key -> service name
	for _, sc := range c.Services {
		fileServices[sc.Name] = true
	}
	for _, ex := range a.svcByName { // services the file does not mention stay as they are
		if fileServices[ex.Name] {
			continue
		}
		if key, ok := listenKey(ex.ListenAddr); ok {
			listen[key] = ex.Name
		}
	}
	for i, sc := range c.Services {
		label := fmt.Sprintf("services[%d] %q", i, sc.Name)
		ex, found := a.svcByName[sc.Name]
		owner, owned := a.svcByID[sc.ID]
		if msg := idProblem("service", sc.ID, found, ex.ID, owner.Name, owned); msg != "" {
			addf("%s: %s", label, msg)
		}
		switch {
		case sc.Profile != "":
			if !fileProfiles[normName(sc.Profile)] {
				addf("%s: profile %q does not exist (define it under profiles or create it first)", label, sc.Profile)
			}
		case sc.ProfileID != "":
			if _, ok := a.profByID[sc.ProfileID]; !ok && !fileProfileIDs[sc.ProfileID] {
				addf("%s: profile_id %q does not exist", label, sc.ProfileID)
			}
		}
		if sc.Recording == "" && sc.RecordingID != "" {
			if _, ok := a.recByID[sc.RecordingID]; !ok {
				addf("%s: recording_id %q does not exist", label, sc.RecordingID)
			}
		}
		if key, ok := listenKey(sc.ListenAddr); ok {
			if other, dup := listen[key]; dup {
				addf("%s: listen_addr %q is already used by service %q", label, sc.ListenAddr, other)
			}
			listen[key] = sc.Name
		}
	}

	// Faults: ids, and service references (an id or a name).
	for i, f := range c.Faults {
		label := fmt.Sprintf("faults[%d] %q", i, f.Name)
		ex, found := a.faultByName[strings.TrimSpace(f.Name)]
		owner, owned := a.faultByID[f.ID]
		if msg := idProblem("fault rule", f.ID, found, ex.ID, owner.Name, owned); msg != "" {
			addf("%s: %s", label, msg)
		}
		if f.ServiceID == "" {
			continue
		}
		_, byID := a.svcByID[f.ServiceID]
		_, byName := a.svcByName[f.ServiceID]
		if !byID && !byName && !fileServices[f.ServiceID] && !fileServiceID(c, f.ServiceID) {
			addf("%s: service_id %q matches no service id or name", label, f.ServiceID)
		}
	}

	if len(p) == 0 {
		return nil
	}
	return &model.ValidationError{Problems: p}
}

func fileServiceID(c *Config, id string) bool {
	for _, sc := range c.Services {
		if sc.ID == id {
			return true
		}
	}
	return false
}

// idProblem checks an explicit id from the file against the stored objects:
// the object matched by name keeps its stored id, and an id may not take
// over another object.
func idProblem(kind, fileID string, foundByName bool, storedID, idOwnerName string, idOwned bool) string {
	switch {
	case fileID == "":
		return ""
	case foundByName && storedID == fileID:
		return ""
	case foundByName:
		return fmt.Sprintf("id %q differs from the stored id %q of this %s (remove it from the file)", fileID, storedID, kind)
	case idOwned:
		return fmt.Sprintf("id %q already belongs to %s %q", fileID, kind, idOwnerName)
	}
	return ""
}

func (c *Config) harPath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

// checkHARFile makes a cheap pre-flight check that path is a readable file
// that starts like a JSON object, so typos are caught before any write.
func checkHARFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("har file: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("har file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("har file %s is not a regular file", path)
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = bytes.TrimPrefix(head[:n], []byte("\xef\xbb\xbf"))
	if t := bytes.TrimSpace(head); len(t) == 0 || t[0] != '{' {
		return fmt.Errorf("har file %s is not a JSON object (is it a HAR export?)", path)
	}
	return nil
}

// execute performs the writes.
func (a *applier) execute(ctx context.Context) error {
	profIDs, err := a.applyProfiles(ctx)
	if err != nil {
		return err
	}
	recIDs, err := a.applyRecordings(ctx)
	if err != nil {
		return err
	}
	svcIDs, err := a.applyServices(ctx, profIDs, recIDs)
	if err != nil {
		return err
	}
	return a.applyFaults(ctx, svcIDs)
}

func (a *applier) applyProfiles(ctx context.Context) (map[string]string, error) {
	ids := make(map[string]string, len(a.profByName)+len(a.c.Profiles))
	for k, p := range a.profByName {
		ids[k] = p.ID
	}
	for _, in := range a.c.Profiles {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		label := "profile " + in.Name
		d := withDefaultsProfile(in)
		d.CreatedAt, d.UpdatedAt = time.Time{}, time.Time{}
		d.HasSecret = false
		ex, found := a.profByName[normName(in.Name)]
		if found {
			d.ID = ex.ID
			if d.Secret == "" {
				d.Secret = ex.Secret // keep the stored secret unless the file sets one
			}
			reuseRuleIDs(d.Rules, ex.Rules)
			exn := withDefaultsProfile(ex)
			exn.CreatedAt, exn.UpdatedAt, exn.HasSecret = time.Time{}, time.Time{}, false
			if sameJSON(exn, d) {
				a.res.Unchanged = append(a.res.Unchanged, label)
				continue
			}
		}
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("config: %s: %w", label, err)
		}
		got, err := a.st.PutProfile(ctx, d)
		if err != nil {
			return nil, fmt.Errorf("config: apply %s: %w", label, err)
		}
		ids[normName(in.Name)] = got.ID
		a.record(found, label)
	}
	return ids, nil
}

// reuseRuleIDs gives rules without an id the id of the stored rule they
// replace (same name, else same target, selector and scope), so applying a
// file again does not churn rule ids.
func reuseRuleIDs(rules, stored []model.AnonymizationRule) {
	used := map[string]bool{}
	for _, r := range rules {
		if r.ID != "" {
			used[r.ID] = true
		}
	}
	take := func(match func(model.AnonymizationRule) bool) string {
		for _, s := range stored {
			if s.ID != "" && !used[s.ID] && match(s) {
				used[s.ID] = true
				return s.ID
			}
		}
		return ""
	}
	for i := range rules {
		r := &rules[i]
		if r.ID != "" {
			continue
		}
		if r.Name != "" {
			r.ID = take(func(s model.AnonymizationRule) bool { return s.Name == r.Name })
		}
		if r.ID == "" {
			r.ID = take(func(s model.AnonymizationRule) bool {
				return s.Target == r.Target && s.Selector == r.Selector && s.Scope == r.Scope
			})
		}
	}
}

func (a *applier) applyRecordings(ctx context.Context) (map[string]string, error) {
	ids := make(map[string]string, len(a.recByName)+len(a.c.Recordings))
	for name, r := range a.recByName {
		ids[name] = r.ID
	}
	ensure := func(rc RecordingConfig) error {
		if _, ok := ids[rc.Name]; ok {
			a.res.Unchanged = append(a.res.Unchanged, "recording "+rc.Name)
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rec, err := a.createRecording(ctx, rc)
		if err != nil {
			return fmt.Errorf("config: apply recording %s: %w", rc.Name, err)
		}
		ids[rc.Name] = rec.ID
		a.res.Created = append(a.res.Created, "recording "+rc.Name)
		return nil
	}
	for _, rc := range a.c.Recordings {
		if err := ensure(rc); err != nil {
			return nil, err
		}
	}
	// Recordings that services name but the file does not list.
	for _, sc := range a.c.Services {
		if sc.Recording == "" {
			continue
		}
		if _, ok := ids[sc.Recording]; ok {
			continue
		}
		if err := ensure(RecordingConfig{Name: sc.Recording}); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// createRecording creates an empty recording, or imports the HAR file.
func (a *applier) createRecording(ctx context.Context, rc RecordingConfig) (model.Recording, error) {
	if rc.HAR == "" {
		return a.st.CreateRecording(ctx, model.Recording{Name: rc.Name, Description: rc.Description, Source: "proxy"})
	}
	path := a.c.harPath(rc.HAR)
	f, err := os.Open(path)
	if err != nil {
		return model.Recording{}, err
	}
	defer f.Close()

	imp := a.c.importHAR
	if imp == nil {
		imp = defaultImportHAR
	}
	rec, err := imp(ctx, a.st, bufio.NewReaderSize(f, 1<<20), rc.Name)
	if err != nil { // ImportHAR deletes the partial recording itself
		return model.Recording{}, fmt.Errorf("import %s: %w", path, err)
	}
	if rc.Description != "" && rec.Description != rc.Description {
		rec.Description = rc.Description
		if rec, err = a.st.UpdateRecording(ctx, rec); err != nil {
			return model.Recording{}, err
		}
	}
	return rec, nil
}

func (a *applier) applyServices(ctx context.Context, profIDs, recIDs map[string]string) (map[string]string, error) {
	ids := make(map[string]string, len(a.svcByName)+len(a.c.Services))
	for name, s := range a.svcByName {
		ids[name] = s.ID
	}
	for _, sc := range a.c.Services {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		label := "service " + sc.Name
		d := sc.Service
		d.CreatedAt, d.UpdatedAt = time.Time{}, time.Time{}
		if sc.Recording != "" {
			d.RecordingID = recIDs[sc.Recording]
		}
		if sc.Profile != "" {
			d.ProfileID = profIDs[normName(sc.Profile)]
		}
		d = d.WithDefaults()
		ex, found := a.svcByName[sc.Name]
		if found {
			d.ID = ex.ID
			exn := ex.WithDefaults()
			exn.CreatedAt, exn.UpdatedAt = time.Time{}, time.Time{}
			if sameJSON(exn, d) {
				a.res.Unchanged = append(a.res.Unchanged, label)
				continue
			}
		}
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("config: %s: %w", label, err)
		}
		got, err := a.st.PutService(ctx, d)
		if err != nil {
			return nil, fmt.Errorf("config: apply %s: %w", label, err)
		}
		ids[sc.Name] = got.ID
		a.record(found, label)
	}
	return ids, nil
}

func (a *applier) applyFaults(ctx context.Context, svcIDs map[string]string) error {
	known := make(map[string]bool, len(svcIDs))
	for _, id := range svcIDs {
		known[id] = true
	}
	for _, f := range a.c.Faults {
		if err := ctx.Err(); err != nil {
			return err
		}
		label := "fault " + f.Name
		d := f.WithDefaults()
		d.CreatedAt, d.UpdatedAt = time.Time{}, time.Time{}
		if d.ServiceID != "" && !known[d.ServiceID] { // a service name
			d.ServiceID = svcIDs[d.ServiceID]
		}
		ex, found := a.faultByName[strings.TrimSpace(f.Name)]
		if found {
			d.ID = ex.ID
			exn := ex.WithDefaults()
			exn.CreatedAt, exn.UpdatedAt = time.Time{}, time.Time{}
			if sameJSON(exn, d) {
				a.res.Unchanged = append(a.res.Unchanged, label)
				continue
			}
		}
		if err := d.Validate(); err != nil {
			return fmt.Errorf("config: %s: %w", label, err)
		}
		if _, err := a.st.PutFaultRule(ctx, d); err != nil {
			return fmt.Errorf("config: apply %s: %w", label, err)
		}
		a.record(found, label)
	}
	return nil
}

func (a *applier) record(updated bool, label string) {
	if updated {
		a.res.Updated = append(a.res.Updated, label)
	} else {
		a.res.Created = append(a.res.Created, label)
	}
}

// sameJSON reports whether two objects encode to the same JSON, which is how
// the store persists them (so nil and empty collections compare equal where
// the model says omitempty).
func sameJSON(a, b any) bool {
	ja, err := json.Marshal(a)
	if err != nil {
		return false
	}
	jb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return slices.Equal(ja, jb)
}
