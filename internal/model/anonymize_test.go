package model

import (
	"slices"
	"strings"
	"testing"
)

func TestDefaultProfileRuleIDsUnique(t *testing.T) {
	p := DefaultAnonymizationProfile()
	seen := map[string]bool{}
	for _, r := range p.Rules {
		if !strings.HasPrefix(r.ID, "def-") {
			t.Errorf("rule %q has ID %q, want a def- ID", r.Name, r.ID)
		}
		if seen[r.ID] {
			t.Errorf("duplicate default rule ID %q", r.ID)
		}
		seen[r.ID] = true
	}
	if err := p.WithDefaults().Validate(); err != nil {
		t.Errorf("default profile does not validate: %v", err)
	}
}

func TestDefaultProfileCoversNamesAddressesAndCredentials(t *testing.T) {
	p := DefaultAnonymizationProfile()
	has := func(target RuleTarget, selector string) bool {
		for _, r := range p.Rules {
			if r.Target == target && r.Selector == selector && r.Enabled {
				return true
			}
		}
		return false
	}
	for _, sel := range []string{
		"$..customer_name", "$..cardholder", "$..holder_name", "$..address", "$..street", "$..city",
		"$..zip", "$..postal_code", "$..address1", "$..billing_address",
	} {
		if !has(TargetJSON, sel) {
			t.Errorf("no default JSON rule %s", sel)
		}
	}
	nameRule := false
	for _, r := range p.Rules {
		if r.Target == TargetJSON && strings.HasSuffix(r.Selector, ")].name") && strings.Contains(r.Selector, "@.email") {
			nameRule = true
		}
		if r.Target == TargetJSON && r.Selector == "$..name" {
			t.Errorf("an unconditional $..name rule would fake product names")
		}
	}
	if !nameRule {
		t.Error("no conditional name rule (name next to email, phone or address)")
	}
	for _, k := range []string{"client_secret", "refresh_token", "id_token", "auth", "pwd", "sid", "X-Amz-Signature", "X-Amz-Security-Token"} {
		if !has(TargetQuery, k) {
			t.Errorf("no default query rule for %s", k)
		}
	}
	headers := map[string]bool{}
	for _, h := range p.RedactHeaders {
		headers[strings.ToLower(h)] = true
	}
	for _, h := range []string{"Api-Key", "X-Amz-Security-Token", "X-Session-Id", "X-Xsrf-Token", "X-Csrftoken", "X-Goog-Api-Key", "True-Client-IP", "CF-Connecting-IP"} {
		if !headers[strings.ToLower(h)] {
			t.Errorf("%s is not in the default redact_headers", h)
		}
	}
	if !strings.Contains(p.Description, "NOT detected") {
		t.Errorf("the description must state the residual gap: %q", p.Description)
	}
}

func TestUpgradeDefaultProfile(t *testing.T) {
	cur := DefaultAnonymizationProfile()

	// A revision 1 profile: no revision 2 rules or headers, old description,
	// one edited rule, one rule switched off.
	old := cur
	old.Description = defaultProfileDescriptionRev1
	old.RedactHeaders = []string{"Authorization", "cookie", "X-Api-Key"}
	old.Rules = nil
	for _, r := range cur.Rules {
		switch r.ID {
		case "def-json-password", "def-json-first_name", "def-query-token":
			old.Rules = append(old.Rules, r)
		}
	}
	for i := range old.Rules {
		switch old.Rules[i].ID {
		case "def-json-password":
			old.Rules[i].Replacement = "***mine***"
		case "def-json-first_name":
			old.Rules[i].Enabled = false
		}
	}
	custom := AnonymizationRule{ID: "rule_mine", Name: "mine", Enabled: true, Target: TargetJSON, Selector: "$.x", Scope: ScopeBoth, Strategy: StrategyHash}
	old.Rules = append(old.Rules, custom)

	up, changed := UpgradeDefaultProfile(old, 1)
	if !changed {
		t.Fatal("changed = false for a revision 1 profile")
	}
	if up.Description != cur.Description {
		t.Errorf("the unmodified old description was not replaced: %q", up.Description)
	}
	byID := map[string]AnonymizationRule{}
	for _, r := range up.Rules {
		if _, dup := byID[r.ID]; dup {
			t.Errorf("rule %s duplicated by the upgrade", r.ID)
		}
		byID[r.ID] = r
	}
	if byID["def-json-password"].Replacement != "***mine***" || byID["def-json-first_name"].Enabled {
		t.Error("the upgrade overwrote an edited or disabled rule")
	}
	if _, ok := byID["rule_mine"]; !ok {
		t.Error("the upgrade dropped a user rule")
	}
	rules, revs := defaultRulesByRev()
	for i, r := range rules {
		_, ok := byID[r.ID]
		switch {
		case revs[i] > 1 && !ok:
			t.Errorf("revision %d rule %s was not added", revs[i], r.ID)
		case revs[i] == 1 && ok && r.ID != "def-json-password" && r.ID != "def-json-first_name" && r.ID != "def-query-token":
			t.Errorf("revision 1 rule %s, which the user deleted, was added back", r.ID)
		}
	}
	if up.Rules[0].ID != "def-json-password" || up.Rules[len(up.Rules)-1].ID == "rule_mine" && len(up.Rules) == len(old.Rules) {
		t.Errorf("existing rules must keep their order")
	}
	have := map[string]int{}
	for _, h := range up.RedactHeaders {
		have[strings.ToLower(h)]++
	}
	for rev, hs := range defaultRedactHeadersByRev {
		for _, h := range hs {
			want := 1
			if rev == 1 && !slices.ContainsFunc(old.RedactHeaders, func(x string) bool { return strings.EqualFold(x, h) }) {
				want = 0 // a revision 1 header the user deleted stays deleted
			}
			if have[strings.ToLower(h)] != want {
				t.Errorf("revision %d header %s appears %d times after the upgrade, want %d", rev, h, have[strings.ToLower(h)], want)
			}
		}
	}
	if up.RedactHeaders[1] != "cookie" {
		t.Errorf("the user's spelling of a header was changed: %v", up.RedactHeaders[:3])
	}
	if err := up.WithDefaults().Validate(); err != nil {
		t.Errorf("upgraded profile does not validate: %v", err)
	}

	// Idempotent and a no-op at the current revision.
	again, changed := UpgradeDefaultProfile(up, 1)
	if changed || len(again.Rules) != len(up.Rules) || len(again.RedactHeaders) != len(up.RedactHeaders) {
		t.Errorf("a second upgrade changed the profile (changed=%v)", changed)
	}
	if _, changed := UpgradeDefaultProfile(old, DefaultProfileRev); changed {
		t.Error("upgrade at the current revision must do nothing")
	}

	// A description the user edited is kept.
	old.Description = "my own text"
	if up, _ := UpgradeDefaultProfile(old, 1); up.Description != "my own text" {
		t.Errorf("an edited description was replaced: %q", up.Description)
	}
}

// An upgrade from revision 2 adds the revision 3 rules and headers and leaves
// out the revision 1 and 2 ones the user deleted.
func TestUpgradeDefaultProfileKeepsDeletions(t *testing.T) {
	rules, revs := defaultRulesByRev()
	old := DefaultAnonymizationProfile()
	old.Rules, old.RedactHeaders = nil, nil
	deleted := map[string]bool{"def-json-session_id": true, "def-json-city": true}
	for i, r := range rules {
		if revs[i] <= 2 && !deleted[r.ID] {
			old.Rules = append(old.Rules, r)
		}
	}
	for _, h := range slices.Concat(defaultRedactHeadersByRev[1], defaultRedactHeadersByRev[2]) {
		if h != "X-Forwarded-For" && h != "Cf-Connecting-Ip" {
			old.RedactHeaders = append(old.RedactHeaders, h)
		}
	}
	up, changed := UpgradeDefaultProfile(old, 2)
	if !changed {
		t.Fatal("changed = false for a revision 2 profile")
	}
	got := map[string]bool{}
	for _, r := range up.Rules {
		got[r.ID] = true
	}
	for i, r := range rules {
		if want := revs[i] == 3 || !deleted[r.ID]; got[r.ID] != want {
			t.Errorf("rule %s (revision %d): present %v, want %v", r.ID, revs[i], got[r.ID], want)
		}
	}
	for _, h := range []string{"X-Forwarded-For", "Cf-Connecting-Ip"} {
		if slices.Contains(up.RedactHeaders, h) {
			t.Errorf("deleted header %s was added back", h)
		}
	}
	for _, h := range defaultRedactHeadersByRev[3] {
		if !slices.Contains(up.RedactHeaders, h) {
			t.Errorf("revision 3 header %s was not added", h)
		}
	}
	if len(up.Rules) != len(old.Rules)+len(slices.DeleteFunc(slices.Clone(revs), func(r int) bool { return r != 3 })) {
		t.Errorf("rules: %d after the upgrade, %d before", len(up.Rules), len(old.Rules))
	}
}
