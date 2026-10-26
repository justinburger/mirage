package store

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

// revision1Profile is what a database created by the first release holds as
// its default profile: the credential and name rules only (the first 48 rules
// of the current list), the shorter header list and the old description. A
// rule is edited, one is switched off and one is the user's own.
func revision1Profile(t *testing.T) model.AnonymizationProfile {
	t.Helper()
	p := model.DefaultAnonymizationProfile().WithDefaults()
	p.Description = "Hashes auth headers, session cookies and common credential fields (passwords, tokens, API keys), fakes person names, and replaces detected PII (emails, cards, SSNs, phone numbers, IPs, JWTs, AWS keys, IBANs) with consistent fake values."
	p.RedactHeaders = p.RedactHeaders[:11]
	p.Rules = p.Rules[:48]
	if p.Rules[0].ID != "def-json-password" || p.Rules[47].ID != "def-query-sessionid" {
		t.Fatalf("the revision 1 rules are no longer the first 48 (%s .. %s); update this test", p.Rules[0].ID, p.Rules[47].ID)
	}
	p.Rules[0].Replacement = "***mine***"
	for i := range p.Rules {
		if p.Rules[i].ID == "def-json-first_name" {
			p.Rules[i].Enabled = false
		}
	}
	p.Rules = append(p.Rules, model.AnonymizationRule{ID: "rule_mine", Name: "mine", Enabled: true, Target: model.TargetJSON, Selector: "$.x", Scope: model.ScopeBoth, Strategy: model.StrategyHash})
	return p
}

// downgrade rewrites the stored default profile to p and forgets the recorded
// revision, as an older release would have left the database.
func downgrade(t *testing.T, s *SQLite, p model.AnonymizationProfile) {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.wdb.Exec(`UPDATE profiles SET data = ? WHERE id = ?`, string(data), model.DefaultProfileID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.wdb.Exec(`DELETE FROM settings WHERE key = ?`, defaultProfileRevKey); err != nil {
		t.Fatal(err)
	}
}

func TestFreshDatabaseRecordsDefaultProfileRevision(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		v, err := s.GetSetting(ctx(), defaultProfileRevKey)
		if err != nil || v != strconv.Itoa(model.DefaultProfileRev) {
			t.Fatalf("%s = %q, %v; want %d", defaultProfileRevKey, v, err, model.DefaultProfileRev)
		}
	})
}

func TestOldDatabaseGainsNewDefaultRules(t *testing.T) {
	s, path := openFile(t)
	before, err := s.GetProfile(ctx(), model.DefaultProfileID)
	if err != nil {
		t.Fatal(err)
	}
	old := revision1Profile(t)
	downgrade(t, s, old)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	got, err := s2.GetProfile(ctx(), model.DefaultProfileID)
	if err != nil {
		t.Fatal(err)
	}
	cur := model.DefaultAnonymizationProfile()

	if got.Secret != before.Secret || got.Secret == "" {
		t.Error("the upgrade changed the profile's secret")
	}
	if got.Description != cur.Description {
		t.Errorf("the old description was not replaced: %q", got.Description)
	}
	byID := map[string]model.AnonymizationRule{}
	for _, r := range got.Rules {
		byID[r.ID] = r
	}
	for _, r := range cur.Rules {
		if _, ok := byID[r.ID]; !ok {
			t.Errorf("built-in rule %s is missing after the upgrade", r.ID)
		}
	}
	if len(got.Rules) != len(cur.Rules)+1 {
		t.Errorf("%d rules, want %d built in plus the user's own", len(got.Rules), len(cur.Rules))
	}
	if byID["def-json-password"].Replacement != "***mine***" || byID["def-json-first_name"].Enabled {
		t.Error("the upgrade overwrote an edited or disabled rule")
	}
	if _, ok := byID["rule_mine"]; !ok {
		t.Error("the upgrade dropped the user's rule")
	}
	have := map[string]bool{}
	for _, h := range got.RedactHeaders {
		have[strings.ToLower(h)] = true
	}
	for _, h := range cur.RedactHeaders {
		if !have[strings.ToLower(h)] {
			t.Errorf("header %s is missing after the upgrade", h)
		}
	}
	if v, _ := s2.GetSetting(ctx(), defaultProfileRevKey); v != strconv.Itoa(model.DefaultProfileRev) {
		t.Errorf("revision recorded as %q", v)
	}
}

func TestDefaultProfileUpgradeRunsOnce(t *testing.T) {
	s, path := openFile(t)
	downgrade(t, s, revision1Profile(t))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path) // upgrades
	if err != nil {
		t.Fatal(err)
	}
	// The user deletes a built-in rule and saves the profile.
	p, _ := s.GetProfile(ctx(), model.DefaultProfileID)
	var kept []model.AnonymizationRule
	for _, r := range p.Rules {
		if r.ID != "def-json-city" {
			kept = append(kept, r)
		}
	}
	p.Rules = kept
	p.Secret = ""
	if _, err := s.PutProfile(ctx(), p); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path) // must not bring it back
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	got, _ := s.GetProfile(ctx(), model.DefaultProfileID)
	for _, r := range got.Rules {
		if r.ID == "def-json-city" {
			t.Fatal("a rule the user deleted came back on the next start")
		}
	}
}

func TestDefaultProfileUpgradeLeavesNewerDatabaseAlone(t *testing.T) {
	s, path := openFile(t)
	p := revision1Profile(t)
	downgrade(t, s, p)
	if err := s.PutSetting(ctx(), defaultProfileRevKey, strconv.Itoa(model.DefaultProfileRev+5)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	got, _ := s.GetProfile(ctx(), model.DefaultProfileID)
	if len(got.Rules) != len(p.Rules) {
		t.Errorf("a database from a newer release was modified: %d rules, had %d", len(got.Rules), len(p.Rules))
	}
	if v, _ := s.GetSetting(ctx(), defaultProfileRevKey); v != strconv.Itoa(model.DefaultProfileRev+5) {
		t.Errorf("revision overwritten with %q", v)
	}
}
