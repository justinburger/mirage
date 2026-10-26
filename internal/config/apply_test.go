package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/store"
)

// baseYAML exercises every kind of object and every kind of reference.
const baseYAML = `
recordings:
  - name: cap
    description: captured traffic
profiles:
  - name: strict
    description: first version
    detectors: [email]
    rules:
      - {name: emails, enabled: true, target: json, selector: "$..email", strategy: fake, fake: email}
      - {name: tail, enabled: true, target: json, selector: "$.card", strategy: mask, keep_suffix: 4}
services:
  - name: web
    listen_addr: "127.0.0.1:19001"
    upstream: http://127.0.0.1:7001
    mode: record
    enabled: true
    recording: cap
    profile: strict
    anonymize_capture: true
  - name: api
    listen_addr: "127.0.0.1:19002"
    upstream: http://127.0.0.1:7002
    mode: passthrough
faults:
  - name: slow
    service_id: web
    path_pattern: /api/**
    probability: 0.5
    type: latency
    delay: 200ms
  - name: boom
    type: error
    probability: 0.1
`

func openStore(t testing.TB) *store.SQLite {
	t.Helper()
	st, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func apply(t *testing.T, c *Config, st store.Store) ApplyResult {
	t.Helper()
	res, err := c.Apply(context.Background(), st)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return res
}

// snapshot is everything in the store as JSON, for before/after comparisons.
func snapshot(t testing.TB, st store.Store) string {
	t.Helper()
	ctx := context.Background()
	var all struct {
		Recordings []model.Recording
		Profiles   []model.AnonymizationProfile
		Services   []model.Service
		Faults     []model.FaultRule
	}
	var err error
	if all.Recordings, err = st.ListRecordings(ctx); err != nil {
		t.Fatal(err)
	}
	if all.Profiles, err = st.ListProfiles(ctx); err != nil {
		t.Fatal(err)
	}
	if all.Services, err = st.ListServices(ctx); err != nil {
		t.Fatal(err)
	}
	if all.Faults, err = st.ListFaultRules(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// assertPristine fails unless the store still holds only what Open seeds.
func assertPristine(t *testing.T, st store.Store) {
	t.Helper()
	ctx := context.Background()
	recs, _ := st.ListRecordings(ctx)
	profs, _ := st.ListProfiles(ctx)
	svcs, _ := st.ListServices(ctx)
	faults, _ := st.ListFaultRules(ctx)
	if len(recs) != 0 || len(profs) != 1 || profs[0].ID != model.DefaultProfileID || len(svcs) != 0 || len(faults) != 0 {
		t.Errorf("store was written to: %d recordings, %d profiles, %d services, %d faults", len(recs), len(profs), len(svcs), len(faults))
	}
}

func byName[T any](t *testing.T, list []T, name string, get func(T) string) T {
	t.Helper()
	for _, x := range list {
		if get(x) == name {
			return x
		}
	}
	t.Fatalf("no object named %q", name)
	var zero T
	return zero
}

func service(t *testing.T, st store.Store, name string) model.Service {
	t.Helper()
	l, err := st.ListServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return byName(t, l, name, func(s model.Service) string { return s.Name })
}

func profile(t *testing.T, st store.Store, name string) model.AnonymizationProfile {
	t.Helper()
	l, err := st.ListProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return byName(t, l, name, func(p model.AnonymizationProfile) string { return p.Name })
}

func fault(t *testing.T, st store.Store, name string) model.FaultRule {
	t.Helper()
	l, err := st.ListFaultRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return byName(t, l, name, func(f model.FaultRule) string { return f.Name })
}

func recording(t *testing.T, st store.Store, name string) model.Recording {
	t.Helper()
	l, err := st.ListRecordings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return byName(t, l, name, func(r model.Recording) string { return r.Name })
}

func TestApplyCreatesEverything(t *testing.T) {
	st := openStore(t)
	c := mustParse(t, "c.yaml", baseYAML)
	res := apply(t, c, st)

	wantCreated := []string{"profile strict", "recording cap", "service web", "service api", "fault slow", "fault boom"}
	if !slices.Equal(res.Created, wantCreated) {
		t.Errorf("Created = %v, want %v", res.Created, wantCreated)
	}
	if len(res.Updated) != 0 || len(res.Unchanged) != 0 {
		t.Errorf("Updated = %v, Unchanged = %v", res.Updated, res.Unchanged)
	}

	rec := recording(t, st, "cap")
	if rec.Description != "captured traffic" || rec.Source != "proxy" || rec.ExchangeCount != 0 {
		t.Errorf("recording = %+v", rec)
	}
	prof := profile(t, st, "strict")
	if prof.Secret == "" || prof.Description != "first version" || len(prof.Rules) != 2 || prof.Rules[0].ID == "" || prof.Rules[1].ID == "" {
		t.Errorf("profile = %+v", prof)
	}
	if prof.DetectorStrategy != model.StrategyFake || prof.OpaqueBodies != model.OpaqueKeep {
		t.Errorf("profile defaults were not applied: %+v", prof)
	}

	web := service(t, st, "web")
	if web.RecordingID != rec.ID || web.ProfileID != prof.ID || !web.AnonymizeCapture || !web.Enabled {
		t.Errorf("web = %+v", web)
	}
	if web.Timeout.D() != model.DefaultServiceTimeout || web.Match.Query != model.QueryExact || web.Latency.Mode != model.LatencyRecorded {
		t.Errorf("service defaults were not applied: %+v", web)
	}
	api := service(t, st, "api")
	if api.RecordingID != "" || api.ProfileID != "" || api.Enabled {
		t.Errorf("api = %+v", api)
	}

	slow := fault(t, st, "slow")
	if slow.ServiceID != web.ID || slow.Delay.D() != 200*time.Millisecond {
		t.Errorf("slow = %+v", slow)
	}
	boom := fault(t, st, "boom")
	if boom.ServiceID != "" || boom.Status != 503 {
		t.Errorf("boom = %+v (status defaults to 503)", boom)
	}
}

func TestApplyIdempotent(t *testing.T) {
	st := openStore(t)
	c := mustParse(t, "c.yaml", baseYAML)
	apply(t, c, st)
	before := snapshot(t, st)

	for i := 0; i < 3; i++ {
		res := apply(t, c, st)
		if len(res.Created) != 0 || len(res.Updated) != 0 {
			t.Fatalf("run %d: Created = %v, Updated = %v", i+2, res.Created, res.Updated)
		}
		want := []string{"profile strict", "recording cap", "service web", "service api", "fault slow", "fault boom"}
		if !slices.Equal(res.Unchanged, want) {
			t.Errorf("run %d: Unchanged = %v, want %v", i+2, res.Unchanged, want)
		}
		if after := snapshot(t, st); after != before {
			t.Fatalf("run %d changed the store (ids, rule ids, secrets or timestamps moved):\nbefore %s\nafter  %s", i+2, before, after)
		}
	}

	// A freshly parsed copy of the same file is also a no-op.
	apply(t, mustParse(t, "c.yaml", baseYAML), st)
	if after := snapshot(t, st); after != before {
		t.Errorf("a second Config for the same file changed the store")
	}
}

func TestApplyUpdatesAndKeepsIdentity(t *testing.T) {
	st := openStore(t)
	apply(t, mustParse(t, "c.yaml", baseYAML), st)

	oldWeb, oldSlow := service(t, st, "web"), fault(t, st, "slow")
	oldProf := profile(t, st, "strict")
	oldAPI := service(t, st, "api")
	time.Sleep(2 * time.Millisecond)

	changed := strings.NewReplacer(
		"http://127.0.0.1:7001", "http://127.0.0.1:7777",
		"probability: 0.5", "probability: 0.25",
		"first version", "second version",
		`{name: tail, enabled: true, target: json, selector: "$.card", strategy: mask, keep_suffix: 4}`,
		`{name: tail, enabled: true, target: json, selector: "$.card", strategy: mask, keep_suffix: 2}
      - {name: phones, enabled: true, target: json, selector: "$..phone", strategy: hash}`,
	).Replace(baseYAML)
	res := apply(t, mustParse(t, "c.yaml", changed), st)

	if len(res.Created) != 0 {
		t.Errorf("Created = %v", res.Created)
	}
	wantUpdated := []string{"profile strict", "service web", "fault slow"}
	if !slices.Equal(res.Updated, wantUpdated) {
		t.Errorf("Updated = %v, want %v", res.Updated, wantUpdated)
	}
	wantSame := []string{"recording cap", "service api", "fault boom"}
	if !slices.Equal(res.Unchanged, wantSame) {
		t.Errorf("Unchanged = %v, want %v", res.Unchanged, wantSame)
	}

	web := service(t, st, "web")
	if web.ID != oldWeb.ID || !web.CreatedAt.Equal(oldWeb.CreatedAt) || web.Upstream != "http://127.0.0.1:7777" || !web.UpdatedAt.After(oldWeb.UpdatedAt) {
		t.Errorf("web: %+v (was %+v)", web, oldWeb)
	}
	if web.RecordingID != oldWeb.RecordingID || web.ProfileID != oldWeb.ProfileID {
		t.Errorf("references changed: %+v", web)
	}
	slow := fault(t, st, "slow")
	if slow.ID != oldSlow.ID || !slow.CreatedAt.Equal(oldSlow.CreatedAt) || slow.Probability != 0.25 || slow.ServiceID != web.ID {
		t.Errorf("slow: %+v", slow)
	}
	prof := profile(t, st, "strict")
	if prof.ID != oldProf.ID || prof.Secret != oldProf.Secret || !prof.CreatedAt.Equal(oldProf.CreatedAt) || prof.Description != "second version" {
		t.Errorf("profile: %+v (was %+v)", prof, oldProf)
	}
	if len(prof.Rules) != 3 || prof.Rules[0].ID != oldProf.Rules[0].ID || prof.Rules[1].ID != oldProf.Rules[1].ID || prof.Rules[1].KeepSuffix != 2 {
		t.Errorf("rules: %+v (were %+v)", prof.Rules, oldProf.Rules)
	}
	if prof.Rules[2].ID == "" || prof.Rules[2].Name != "phones" {
		t.Errorf("new rule: %+v", prof.Rules[2])
	}
	if api := service(t, st, "api"); !reflect.DeepEqual(api, oldAPI) {
		t.Errorf("untouched service was rewritten: %+v vs %+v", api, oldAPI)
	}
}

func TestApplyProfileSecret(t *testing.T) {
	st := openStore(t)
	withoutSecret := "profiles:\n  - name: p1\n    description: v1\n"
	apply(t, mustParse(t, "c.yaml", withoutSecret), st)
	generated := profile(t, st, "p1").Secret
	if generated == "" {
		t.Fatal("no secret was generated")
	}

	// Leaving the secret out keeps the stored one.
	res := apply(t, mustParse(t, "c.yaml", strings.Replace(withoutSecret, "v1", "v2", 1)), st)
	if !slices.Equal(res.Updated, []string{"profile p1"}) {
		t.Errorf("Updated = %v", res.Updated)
	}
	if got := profile(t, st, "p1").Secret; got != generated {
		t.Errorf("secret changed to %q", got)
	}

	// A secret in the file replaces it, and then stays put.
	withSecret := "profiles:\n  - name: p1\n    description: v2\n    secret: from-the-file\n"
	apply(t, mustParse(t, "c.yaml", withSecret), st)
	if got := profile(t, st, "p1").Secret; got != "from-the-file" {
		t.Errorf("secret = %q", got)
	}
	if res := apply(t, mustParse(t, "c.yaml", withSecret), st); len(res.Updated) != 0 {
		t.Errorf("Updated = %v", res.Updated)
	}
	// A new profile with a secret in the file uses it.
	apply(t, mustParse(t, "c.yaml", "profiles:\n  - name: p2\n    secret: abc123\n"), st)
	if got := profile(t, st, "p2").Secret; got != "abc123" {
		t.Errorf("new profile secret = %q", got)
	}
}

func TestApplyLeavesOtherObjectsAlone(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	rec, _ := st.CreateRecording(ctx, model.Recording{Name: "old", Source: "proxy"})
	if _, err := st.PutProfile(ctx, model.AnonymizationProfile{Name: "mine"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutService(ctx, model.Service{Name: "other", ListenAddr: "127.0.0.1:19050", Upstream: "http://x:1", Mode: model.ModePassthrough}.WithDefaults()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutFaultRule(ctx, model.FaultRule{Name: "mine", Type: model.FaultAbort, Enabled: true, Probability: 1}); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, st)

	res := apply(t, mustParse(t, "c.yaml", baseYAML), st)
	if len(res.Created) != 6 {
		t.Fatalf("Created = %v", res.Created)
	}
	if got := recording(t, st, "old"); got.ID != rec.ID {
		t.Errorf("recording was replaced")
	}
	for _, name := range []string{"mine"} {
		profile(t, st, name)
		fault(t, st, name)
	}
	service(t, st, "other")
	// Everything that was there before is byte-identical afterwards.
	var b, a struct {
		Recordings []model.Recording
		Profiles   []model.AnonymizationProfile
		Services   []model.Service
		Faults     []model.FaultRule
	}
	if err := json.Unmarshal([]byte(before), &b); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(snapshot(t, st)), &a); err != nil {
		t.Fatal(err)
	}
	for _, p := range b.Profiles {
		if got := byName(t, a.Profiles, p.Name, func(x model.AnonymizationProfile) string { return x.Name }); !reflect.DeepEqual(got, p) {
			t.Errorf("profile %q changed", p.Name)
		}
	}
	for _, s := range b.Services {
		if got := byName(t, a.Services, s.Name, func(x model.Service) string { return x.Name }); !reflect.DeepEqual(got, s) {
			t.Errorf("service %q changed", s.Name)
		}
	}
	for _, f := range b.Faults {
		if got := byName(t, a.Faults, f.Name, func(x model.FaultRule) string { return x.Name }); !reflect.DeepEqual(got, f) {
			t.Errorf("fault %q changed", f.Name)
		}
	}
}

func TestApplyResolvesExistingNames(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	pre, _ := st.CreateRecording(ctx, model.Recording{Name: "pre-existing", Source: "har"})
	legacy, err := st.PutProfile(ctx, model.AnonymizationProfile{Name: "Legacy Profile"})
	if err != nil {
		t.Fatal(err)
	}

	src := `
services:
  - {name: a, listen_addr: "127.0.0.1:19101", upstream: "http://x:1", mode: hybrid, recording: pre-existing, profile: " legacy PROFILE "}
  - {name: b, listen_addr: "127.0.0.1:19102", upstream: "http://x:1", mode: record, recording: "brand new", profile: default}
  - {name: c, listen_addr: "127.0.0.1:19103", upstream: "http://x:1", mode: virtualize, recording_id: ` + pre.ID + `, profile_id: ` + legacy.ID + `}
`
	res := apply(t, mustParse(t, "c.yaml", src), st)
	if !slices.Equal(res.Created, []string{"recording brand new", "service a", "service b", "service c"}) {
		t.Errorf("Created = %v", res.Created)
	}
	a, b, c := service(t, st, "a"), service(t, st, "b"), service(t, st, "c")
	if a.RecordingID != pre.ID || a.ProfileID != legacy.ID {
		t.Errorf("a = %+v", a)
	}
	if b.RecordingID != recording(t, st, "brand new").ID || b.ProfileID != model.DefaultProfileID {
		t.Errorf("b = %+v", b)
	}
	if c.RecordingID != pre.ID || c.ProfileID != legacy.ID {
		t.Errorf("c = %+v", c)
	}
	if profs, _ := st.ListProfiles(ctx); len(profs) != 2 {
		t.Errorf("profiles = %d, want default + Legacy Profile", len(profs))
	}
}

func TestApplyProfileDefinedLaterInFile(t *testing.T) {
	// The profile section is applied first, so services can name any profile
	// of the file regardless of order.
	st := openStore(t)
	src := `
services:
  - {name: a, listen_addr: ":19201", upstream: "http://x:1", profile: P}
profiles:
  - name: p
`
	apply(t, mustParse(t, "c.yaml", src), st)
	if service(t, st, "a").ProfileID != profile(t, st, "p").ID {
		t.Error("profile reference not resolved")
	}
}

func TestApplyOldestRecordingOfANameWins(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	first, _ := st.CreateRecording(ctx, model.Recording{Name: "dup", Source: "proxy"})
	time.Sleep(3 * time.Millisecond)
	if _, err := st.CreateRecording(ctx, model.Recording{Name: "dup", Source: "proxy"}); err != nil {
		t.Fatal(err)
	}
	src := `
recordings: [{name: dup}]
services:
  - {name: a, listen_addr: ":19301", upstream: "http://x:1", mode: record, recording: dup}
`
	res := apply(t, mustParse(t, "c.yaml", src), st)
	if len(res.Created) != 1 || res.Created[0] != "service a" {
		t.Errorf("Created = %v", res.Created)
	}
	if got := service(t, st, "a").RecordingID; got != first.ID {
		t.Errorf("service uses %s, want the oldest %s", got, first.ID)
	}
}

func TestApplyValidatesBeforeWriting(t *testing.T) {
	good := ServiceConfig{Service: model.Service{Name: "good", ListenAddr: "127.0.0.1:19401", Upstream: "http://x:1", Mode: model.ModePassthrough}}
	goodProfile := model.AnonymizationProfile{Name: "fine"}
	goodFault := model.FaultRule{Name: "ok", Type: model.FaultAbort, Probability: 1}

	tests := []struct {
		name  string
		cfg   Config
		parts []string
	}{
		{
			name: "invalid later service",
			cfg: Config{
				Recordings: []RecordingConfig{{Name: "r"}},
				Profiles:   []model.AnonymizationProfile{goodProfile},
				Services:   []ServiceConfig{good, {Service: model.Service{Name: "bad name", ListenAddr: "x", Upstream: "nope"}}},
				Faults:     []model.FaultRule{goodFault},
			},
			parts: []string{"services[1]", "name must be 1-64 chars"},
		},
		{
			name: "invalid profile",
			cfg: Config{
				Services: []ServiceConfig{good},
				Profiles: []model.AnonymizationProfile{goodProfile, {Name: "broken", Rules: []model.AnonymizationRule{{Target: "nope", Selector: "x"}}}},
			},
			parts: []string{"profiles[1]", "unknown target"},
		},
		{
			name: "invalid last fault",
			cfg: Config{
				Services: []ServiceConfig{good},
				Faults:   []model.FaultRule{goodFault, {Name: "bad", Type: model.FaultError, Probability: 7}},
			},
			parts: []string{"faults[1]", "probability"},
		},
		{
			name:  "unknown profile",
			cfg:   Config{Services: []ServiceConfig{{Service: good.Service, Profile: "ghost"}}},
			parts: []string{`profile "ghost" does not exist`},
		},
		{
			name:  "unknown profile id",
			cfg:   Config{Services: []ServiceConfig{{Service: model.Service{Name: "x", ListenAddr: ":19402", Upstream: "http://x:1", ProfileID: "prof_nope"}}}},
			parts: []string{`profile_id "prof_nope" does not exist`},
		},
		{
			name:  "unknown recording id",
			cfg:   Config{Services: []ServiceConfig{{Service: model.Service{Name: "x", ListenAddr: ":19402", Upstream: "http://x:1", Mode: model.ModeRecord, RecordingID: "rec_nope"}}}},
			parts: []string{`recording_id "rec_nope" does not exist`},
		},
		{
			name: "unknown fault service",
			cfg: Config{
				Services: []ServiceConfig{good},
				Faults:   []model.FaultRule{{Name: "f", Type: model.FaultAbort, Probability: 1, ServiceID: "ghost"}},
			},
			parts: []string{"faults[0]", `service_id "ghost" matches no service`},
		},
		{
			name: "several problems at once",
			cfg: Config{
				Services: []ServiceConfig{{Service: good.Service, Profile: "ghost"}},
				Faults:   []model.FaultRule{{Name: "f", Type: model.FaultAbort, Probability: 1, ServiceID: "ghost"}},
			},
			parts: []string{"profile", "service_id"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			_, err := tc.cfg.Apply(context.Background(), st)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !errors.Is(err, model.ErrValidation) {
				t.Errorf("error does not wrap ErrValidation: %v", err)
			}
			var ve *model.ValidationError
			if !errors.As(err, &ve) {
				t.Errorf("no *model.ValidationError in %v", err)
			}
			for _, p := range tc.parts {
				if !strings.Contains(err.Error(), p) {
					t.Errorf("error %q does not mention %q", err, p)
				}
			}
			assertPristine(t, st)
		})
	}
}

func TestApplyListenAddressConflicts(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name         string
		existingAddr string
		fileAddr     string
		conflict     bool
	}{
		{"same address", ":19501", ":19501", true},
		{"wildcard spelled differently", ":19501", "0.0.0.0:19501", true},
		{"ipv6 wildcard", "[::]:19501", ":19501", true},
		{"different port", ":19501", ":19502", false},
		{"specific host next to another", "127.0.0.1:19501", "127.0.0.2:19501", false},
		{"port zero", ":0", ":0", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			if _, err := st.PutService(ctx, model.Service{Name: "legacy", ListenAddr: tc.existingAddr, Upstream: "http://x:1", Mode: model.ModePassthrough}.WithDefaults()); err != nil {
				t.Fatal(err)
			}
			src := `
profiles: [{name: first}]
recordings: [{name: first}]
services:
  - {name: web, listen_addr: "` + tc.fileAddr + `", upstream: "http://x:1"}
`
			_, err := mustParse(t, "c.yaml", src).Apply(ctx, st)
			if !tc.conflict {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, model.ErrValidation) || !strings.Contains(err.Error(), `already used by service "legacy"`) {
				t.Fatalf("err = %v", err)
			}
			// Nothing else was written: only "legacy" is there.
			if svcs, _ := st.ListServices(ctx); len(svcs) != 1 {
				t.Errorf("services = %d", len(svcs))
			}
			if recs, _ := st.ListRecordings(ctx); len(recs) != 0 {
				t.Errorf("recordings = %d", len(recs))
			}
			if profs, _ := st.ListProfiles(ctx); len(profs) != 1 {
				t.Errorf("profiles = %d", len(profs))
			}
		})
	}
}

func TestApplyMovingAServiceKeepsItsPort(t *testing.T) {
	// A service that is in the file may keep (or give up) its own address.
	st := openStore(t)
	src := `services: [{name: web, listen_addr: ":19601", upstream: "http://x:1"}]`
	apply(t, mustParse(t, "c.yaml", src), st)
	apply(t, mustParse(t, "c.yaml", src), st)
	apply(t, mustParse(t, "c.yaml", strings.Replace(src, "19601", "19602", 1)), st)
	if got := service(t, st, "web").ListenAddr; got != ":19602" {
		t.Errorf("listen_addr = %q", got)
	}
}

func TestApplyExplicitIDs(t *testing.T) {
	st := openStore(t)
	src := `
profiles: [{id: prof_custom, name: p}]
services: [{id: svc_custom, name: s, listen_addr: ":19701", upstream: "http://x:1"}]
faults: [{id: flt_custom, name: f, type: abort, probability: 1, service_id: svc_custom}]
`
	apply(t, mustParse(t, "c.yaml", src), st)
	if profile(t, st, "p").ID != "prof_custom" || service(t, st, "s").ID != "svc_custom" {
		t.Error("explicit ids were not used")
	}
	f := fault(t, st, "f")
	if f.ID != "flt_custom" || f.ServiceID != "svc_custom" {
		t.Errorf("fault = %+v", f)
	}
	// The same ids again: unchanged.
	if res := apply(t, mustParse(t, "c.yaml", src), st); len(res.Created)+len(res.Updated) != 0 {
		t.Errorf("res = %+v", res)
	}

	clashes := []struct{ name, src, part string }{
		{"stored id differs", `services: [{id: svc_other, name: s, listen_addr: ":19701", upstream: "http://x:1"}]`, `differs from the stored id "svc_custom"`},
		{"id belongs to another service", `services: [{id: svc_custom, name: renamed, listen_addr: ":19702", upstream: "http://x:1"}]`, `id "svc_custom" already belongs to service "s"`},
		{"profile id belongs to another", `profiles: [{id: prof_custom, name: q}]`, `already belongs to profile "p"`},
		{"fault id belongs to another", `faults: [{id: flt_custom, name: g, type: abort}]`, `already belongs to fault rule "f"`},
	}
	for _, tc := range clashes {
		before := snapshot(t, st)
		_, err := mustParse(t, "c.yaml", tc.src).Apply(context.Background(), st)
		if !errors.Is(err, model.ErrValidation) || !strings.Contains(err.Error(), tc.part) {
			t.Errorf("%s: err = %v", tc.name, err)
		}
		if snapshot(t, st) != before {
			t.Errorf("%s: store changed", tc.name)
		}
	}
}

func TestApplyFaultServiceReferences(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	existing, err := st.PutService(ctx, model.Service{Name: "legacy", ListenAddr: ":19801", Upstream: "http://x:1", Mode: model.ModePassthrough}.WithDefaults())
	if err != nil {
		t.Fatal(err)
	}
	src := `
services: [{name: web, listen_addr: ":19802", upstream: "http://x:1"}]
faults:
  - {name: by-file-name, type: abort, probability: 1, service_id: web}
  - {name: by-store-name, type: abort, probability: 1, service_id: legacy}
  - {name: by-store-id, type: abort, probability: 1, service_id: ` + existing.ID + `}
  - {name: global, type: abort, probability: 1}
`
	apply(t, mustParse(t, "c.yaml", src), st)
	web := service(t, st, "web")
	for name, want := range map[string]string{"by-file-name": web.ID, "by-store-name": existing.ID, "by-store-id": existing.ID, "global": ""} {
		if got := fault(t, st, name).ServiceID; got != want {
			t.Errorf("%s: service_id = %q, want %q", name, got, want)
		}
	}
	// Names resolve again on the next run without rewriting anything.
	if res := apply(t, mustParse(t, "c.yaml", src), st); len(res.Created)+len(res.Updated) != 0 {
		t.Errorf("second run: %+v", res)
	}
}

func TestApplyHARImport(t *testing.T) {
	dir := t.TempDir()
	har, err := os.ReadFile("testdata/tiny.har")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "captures"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "captures", "tiny.har"), har, 0o600); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(dir, "captures", "tiny.har")

	src := `
recordings:
  - name: relative
    description: from a relative path
    har: captures/tiny.har
  - name: absolute
    har: ` + abs + `
  - name: empty
services:
  - {name: web, listen_addr: ":19901", upstream: "http://x:1", mode: virtualize, recording: relative}
`
	st := openStore(t)
	c := mustParse(t, "c.yaml", src)
	c.Dir = dir
	res := apply(t, c, st)
	if !slices.Equal(res.Created, []string{"recording relative", "recording absolute", "recording empty", "service web"}) {
		t.Errorf("Created = %v", res.Created)
	}

	rel := recording(t, st, "relative")
	if rel.Source != "har" || rel.ExchangeCount != 2 || rel.ErrorCount != 1 {
		t.Errorf("relative = %+v", rel)
	}
	if rel.Description != "from a relative path" {
		t.Errorf("description = %q, want the one from the file", rel.Description)
	}
	if abs := recording(t, st, "absolute"); abs.ExchangeCount != 2 || abs.Description == "" {
		t.Errorf("absolute = %+v (the importer's own description is kept when the file has none)", abs)
	}
	if e := recording(t, st, "empty"); e.ExchangeCount != 0 || e.Source != "proxy" {
		t.Errorf("empty = %+v", e)
	}
	if service(t, st, "web").RecordingID != rel.ID {
		t.Error("service does not use the imported recording")
	}

	var endpoints []string
	err = st.IterateExchanges(context.Background(), rel.ID, model.IterOptions{ResponseBodies: true}, func(ex *model.Exchange) error {
		endpoints = append(endpoints, ex.Endpoint)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(endpoints)
	if !slices.Equal(endpoints, []string{"GET /api/users/{id}", "POST /api/login"}) {
		t.Errorf("endpoints = %v", endpoints)
	}

	// Applying again does not import again, and the file may be gone by then.
	if err := os.Remove(abs); err != nil {
		t.Fatal(err)
	}
	res = apply(t, c, st)
	if len(res.Created)+len(res.Updated) != 0 {
		t.Errorf("second run: %+v", res)
	}
	if got := recording(t, st, "relative"); got.ExchangeCount != 2 {
		t.Errorf("exchange count after re-apply = %d", got.ExchangeCount)
	}
}

func TestApplyHARPreflight(t *testing.T) {
	dir := t.TempDir()
	notJSON := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notJSON, []byte("this is not a HAR file"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.har")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, har, part string }{
		{"missing", filepath.Join(dir, "nope.har"), "no such file"},
		{"directory", dir, "not a regular file"},
		{"text file", notJSON, "not a JSON object"},
		{"empty file", empty, "not a JSON object"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			c := &Config{
				Profiles:   []model.AnonymizationProfile{{Name: "p"}},
				Recordings: []RecordingConfig{{Name: "r", HAR: tc.har}},
				Services:   []ServiceConfig{{Service: model.Service{Name: "s", ListenAddr: ":19951", Upstream: "http://x:1"}}},
			}
			_, err := c.Apply(context.Background(), st)
			if !errors.Is(err, model.ErrValidation) || !strings.Contains(err.Error(), tc.part) || !strings.Contains(err.Error(), `recordings[0] "r"`) {
				t.Fatalf("err = %v", err)
			}
			assertPristine(t, st)
		})
	}

	// A HAR file that already has its recording is never opened.
	st := openStore(t)
	if _, err := st.CreateRecording(context.Background(), model.Recording{Name: "r", Source: "har"}); err != nil {
		t.Fatal(err)
	}
	c := &Config{Recordings: []RecordingConfig{{Name: "r", HAR: filepath.Join(dir, "nope.har")}}}
	if _, err := c.Apply(context.Background(), st); err != nil {
		t.Errorf("existing recording with a missing HAR: %v", err)
	}
}

func TestApplyHARImportFailureLeavesNoRecording(t *testing.T) {
	dir := t.TempDir()
	// Starts like JSON (passes the pre-flight) but is not a HAR document.
	bad := filepath.Join(dir, "bad.har")
	if err := os.WriteFile(bad, []byte(`{"log": {"entries": [ {"request": `), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := func(har string) *Config {
		return &Config{
			Dir:        dir,
			Profiles:   []model.AnonymizationProfile{{Name: "p"}},
			Recordings: []RecordingConfig{{Name: "r", HAR: har}},
			Services:   []ServiceConfig{{Service: model.Service{Name: "s", ListenAddr: ":19961", Upstream: "http://x:1", Mode: model.ModeVirtualize}, Recording: "r"}},
		}
	}
	st := openStore(t)
	res, err := cfg("bad.har").Apply(context.Background(), st)
	if err == nil || !strings.Contains(err.Error(), "bad.har") {
		t.Fatalf("err = %v", err)
	}
	// What was done before the failure is reported, and nothing partial stays.
	if !slices.Equal(res.Created, []string{"profile p"}) {
		t.Errorf("partial result = %v", res.Created)
	}
	if recs, _ := st.ListRecordings(context.Background()); len(recs) != 0 {
		t.Errorf("recordings left behind: %+v", recs)
	}
	if svcs, _ := st.ListServices(context.Background()); len(svcs) != 0 {
		t.Errorf("services written after the failure: %d", len(svcs))
	}

	// Fixing the file and applying again completes the job.
	good, _ := os.ReadFile("testdata/tiny.har")
	if err := os.WriteFile(filepath.Join(dir, "good.har"), good, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = cfg("good.har").Apply(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Created, []string{"recording r", "service s"}) || !slices.Equal(res.Unchanged, []string{"profile p"}) {
		t.Errorf("result = %+v", res)
	}
}

func TestApplyHARImporterHook(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.har"), []byte(`{"log":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotName string
	var gotBody []byte
	calls := 0
	c := &Config{
		Dir:        dir,
		Recordings: []RecordingConfig{{Name: "imp", Description: "desc", HAR: "x.har"}},
		importHAR: func(ctx context.Context, st store.Store, r io.Reader, name string) (model.Recording, error) {
			calls++
			gotName = name
			gotBody, _ = io.ReadAll(r)
			return st.CreateRecording(ctx, model.Recording{Name: name, Source: "har", Description: "importer's note"})
		},
	}
	st := openStore(t)
	apply(t, c, st)
	if calls != 1 || gotName != "imp" || !bytes.Equal(gotBody, []byte(`{"log":{}}`)) {
		t.Errorf("hook calls = %d, name = %q, body = %q", calls, gotName, gotBody)
	}
	if got := recording(t, st, "imp"); got.Description != "desc" {
		t.Errorf("description = %q, want the configured one to win", got.Description)
	}
	apply(t, c, st)
	if calls != 1 {
		t.Errorf("importer ran again (%d calls)", calls)
	}

	// Errors from the importer propagate with the sentinel intact.
	boom := errors.New("importer exploded")
	c2 := &Config{
		Dir:        dir,
		Recordings: []RecordingConfig{{Name: "imp2", HAR: "x.har"}},
		importHAR: func(context.Context, store.Store, io.Reader, string) (model.Recording, error) {
			return model.Recording{}, boom
		},
	}
	if _, err := c2.Apply(context.Background(), st); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
}

// hookStore wraps a store to observe or fail individual writes.
type hookStore struct {
	store.Store
	afterPutProfile func()
	putFault        func(model.FaultRule) error
}

func (h *hookStore) PutProfile(ctx context.Context, p model.AnonymizationProfile) (model.AnonymizationProfile, error) {
	got, err := h.Store.PutProfile(ctx, p)
	if err == nil && h.afterPutProfile != nil {
		h.afterPutProfile()
	}
	return got, err
}

func (h *hookStore) PutFaultRule(ctx context.Context, r model.FaultRule) (model.FaultRule, error) {
	if h.putFault != nil {
		if err := h.putFault(r); err != nil {
			return model.FaultRule{}, err
		}
	}
	return h.Store.PutFaultRule(ctx, r)
}

func TestApplyStoreFailureReturnsPartialResult(t *testing.T) {
	st := openStore(t)
	boom := errors.New("disk on fire")
	hs := &hookStore{Store: st, putFault: func(r model.FaultRule) error {
		if r.Name == "boom" {
			return boom
		}
		return nil
	}}
	c := mustParse(t, "c.yaml", baseYAML)
	res, err := c.Apply(context.Background(), hs)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "fault boom") {
		t.Fatalf("err = %v", err)
	}
	want := []string{"profile strict", "recording cap", "service web", "service api", "fault slow"}
	if !slices.Equal(res.Created, want) {
		t.Errorf("partial Created = %v, want %v", res.Created, want)
	}

	// Applying again against the healthy store finishes the work.
	res = apply(t, c, st)
	if !slices.Equal(res.Created, []string{"fault boom"}) || len(res.Updated) != 0 || len(res.Unchanged) != 5 {
		t.Errorf("second run: %+v", res)
	}
}

func TestApplyContextCancellation(t *testing.T) {
	t.Run("before start", func(t *testing.T) {
		st := openStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := mustParse(t, "c.yaml", baseYAML).Apply(ctx, st)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
		assertPristine(t, st)
	})
	t.Run("between writes", func(t *testing.T) {
		st := openStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		hs := &hookStore{Store: st, afterPutProfile: cancel}
		res, err := mustParse(t, "c.yaml", baseYAML).Apply(ctx, hs)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if !slices.Equal(res.Created, []string{"profile strict"}) {
			t.Errorf("Created = %v", res.Created)
		}
		if svcs, _ := st.ListServices(context.Background()); len(svcs) != 0 {
			t.Errorf("%d services written after cancellation", len(svcs))
		}
	})
}

func TestApplyDoesNotModifyConfig(t *testing.T) {
	a := mustParse(t, "c.yaml", baseYAML)
	b := mustParse(t, "c.yaml", baseYAML)
	st := openStore(t)
	apply(t, a, st)
	apply(t, a, st)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("Apply modified the Config:\n got %+v\nwant %+v", a, b)
	}
}

func TestApplyConcurrentOnSeparateStores(t *testing.T) {
	c := mustParse(t, "c.yaml", baseYAML)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := store.Open(store.MemoryPath)
			if err != nil {
				errs <- err
				return
			}
			defer st.Close()
			for round := 0; round < 3; round++ {
				if _, err := c.Apply(context.Background(), st); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestParseConcurrent(t *testing.T) {
	t.Setenv("MIRAGE_TEST_CONC", "v")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := Parse("c.yaml", []byte(baseYAML+"\nserver: {data: ${MIRAGE_TEST_CONC}}\n")); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestApplyEmptyConfig(t *testing.T) {
	st := openStore(t)
	res := apply(t, &Config{}, st)
	if len(res.Created)+len(res.Updated)+len(res.Unchanged) != 0 {
		t.Errorf("res = %+v", res)
	}
	assertPristine(t, st)
	// The JSON form of the result has arrays, never null.
	b, _ := json.Marshal(res)
	if !strings.Contains(string(b), `"created":[]`) || !strings.Contains(string(b), `"updated":[]`) {
		t.Errorf("json = %s", b)
	}
}

func TestApplyExampleConfig(t *testing.T) {
	c, err := Load("../../examples/mirage.yaml")
	if err != nil {
		t.Fatal(err)
	}
	st := openStore(t)
	res := apply(t, c, st)
	want := []string{"profile strict", "recording demo-capture", "service demo", "fault orders 10% 503", "fault search +200ms"}
	if !slices.Equal(res.Created, want) {
		t.Errorf("Created = %v, want %v", res.Created, want)
	}
	demo := service(t, st, "demo")
	if demo.RecordingID != recording(t, st, "demo-capture").ID || demo.ProfileID != profile(t, st, "strict").ID {
		t.Errorf("demo = %+v", demo)
	}
	if demo.Match.Query != model.QueryExact || demo.Match.PathTemplates != true || demo.Timeout.D() != 30*time.Second {
		t.Errorf("demo match/timeout = %+v %v", demo.Match, demo.Timeout)
	}
	for _, name := range []string{"orders 10% 503", "search +200ms"} {
		f := fault(t, st, name)
		if f.Enabled || f.ServiceID != demo.ID {
			t.Errorf("%s: %+v", name, f)
		}
	}
	before := snapshot(t, st)
	res = apply(t, c, st)
	if len(res.Created)+len(res.Updated) != 0 || snapshot(t, st) != before {
		t.Errorf("example config is not idempotent: %+v", res)
	}
}

func TestReuseRuleIDs(t *testing.T) {
	r := func(id, name, sel string) model.AnonymizationRule {
		return model.AnonymizationRule{ID: id, Name: name, Target: model.TargetJSON, Selector: sel, Scope: model.ScopeBoth}
	}
	stored := []model.AnonymizationRule{r("r1", "emails", "$..email"), r("r2", "", "$.card"), r("r3", "gone", "$.x"), r("r4", "dup", "$.d"), r("r5", "dup", "$.e")}
	tests := []struct {
		name string
		in   []model.AnonymizationRule
		want []string
	}{
		{"by name", []model.AnonymizationRule{r("", "emails", "$..changed")}, []string{"r1"}},
		{"by target, selector and scope when unnamed", []model.AnonymizationRule{r("", "", "$.card")}, []string{"r2"}},
		{"reordered", []model.AnonymizationRule{r("", "gone", "$.x"), r("", "emails", "$..email")}, []string{"r3", "r1"}},
		{"explicit ids are kept and not reused", []model.AnonymizationRule{r("r1", "other", "$.o"), r("", "emails", "$..email")}, []string{"r1", ""}},
		{"same name twice takes each stored rule once", []model.AnonymizationRule{r("", "dup", "$.d"), r("", "dup", "$.e"), r("", "dup", "$.f")}, []string{"r4", "r5", ""}},
		{"new rule gets no id here", []model.AnonymizationRule{r("", "fresh", "$.fresh")}, []string{""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := slices.Clone(tc.in)
			reuseRuleIDs(in, stored)
			var got []string
			for _, x := range in {
				got = append(got, x.ID)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("ids = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListenKey(t *testing.T) {
	same := [][]string{
		{":9001", "0.0.0.0:9001", "[::]:9001", " :9001 "},
		{"127.0.0.1:80", "127.0.0.1:80"},
		{"LOCALHOST:80", "localhost:80"},
	}
	for _, group := range same {
		first, ok := listenKey(group[0])
		if !ok {
			t.Fatalf("%q unchecked", group[0])
		}
		for _, a := range group[1:] {
			if k, _ := listenKey(a); k != first {
				t.Errorf("%q and %q should be one socket (%q vs %q)", group[0], a, first, k)
			}
		}
	}
	for _, pair := range [][2]string{{":9001", "127.0.0.1:9001"}, {":9001", ":9002"}, {"localhost:80", "127.0.0.1:80"}} {
		a, _ := listenKey(pair[0])
		b, _ := listenKey(pair[1])
		if a == b {
			t.Errorf("%q and %q should differ", pair[0], pair[1])
		}
	}
	for _, addr := range []string{":0", "127.0.0.1:0", "garbage", ""} {
		if _, ok := listenKey(addr); ok {
			t.Errorf("%q should be exempt from the uniqueness check", addr)
		}
	}
}

func TestSameJSONTreatsNilAndEmptyAlike(t *testing.T) {
	a := model.Service{Name: "x", EndpointPatterns: nil}.WithDefaults()
	b := model.Service{Name: "x", EndpointPatterns: []string{}}.WithDefaults()
	if !sameJSON(a, b) {
		t.Error("nil and empty omitempty slices should compare equal")
	}
	b.Upstream = "http://y"
	if sameJSON(a, b) {
		t.Error("different services compared equal")
	}
	if sameJSON(a, make(chan int)) {
		t.Error("unencodable values must not compare equal")
	}
}

func TestErrorFormatting(t *testing.T) {
	e := &Error{File: "x.yaml", Problems: []Problem{{Line: 3, Field: "a.b", Msg: "bad"}}}
	if got := e.Error(); got != "config: x.yaml: line 3: a.b: bad" {
		t.Errorf("single = %q", got)
	}
	e.Problems = append(e.Problems, Problem{Msg: "no position"}, Problem{Field: "f", Msg: "no line"})
	want := "config: x.yaml: 3 problems\n  - line 3: a.b: bad\n  - no position\n  - f: no line"
	if got := e.Error(); got != want {
		t.Errorf("multi = %q, want %q", got, want)
	}
	if got := (&Error{Problems: []Problem{{Msg: "m"}}}).Error(); got != "config: input: m" {
		t.Errorf("no file = %q", got)
	}
	if !errors.Is(e, model.ErrValidation) {
		t.Error("Error should wrap ErrValidation")
	}
}
