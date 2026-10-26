package store

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func ruleWithID(id, name string) model.AnonymizationRule {
	return model.AnonymizationRule{
		ID: id, Name: name, Enabled: true, Target: model.TargetJSON, Selector: "$." + name,
		Scope: model.ScopeBoth, Strategy: model.StrategyRedact,
	}
}

func ruleIDs(rules []model.AnonymizationRule) []string {
	ids := make([]string, len(rules))
	for i, r := range rules {
		ids[i] = r.ID
	}
	return ids
}

func TestPutProfileAssignsRuleIDs(t *testing.T) {
	cases := []struct {
		name string
		ids  []string // IDs of the rules passed in; "" = unassigned
	}{
		{"no rules", nil},
		{"empty rule list", []string{}},
		{"all unassigned", []string{"", "", ""}},
		{"mixed", []string{"", "keep-me", "", "def-custom"}},
		{"all assigned", []string{"r1", "r2"}},
	}
	backends(t, func(t *testing.T, s *SQLite) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				var rules []model.AnonymizationRule
				if c.ids != nil {
					rules = []model.AnonymizationRule{}
				}
				for i, id := range c.ids {
					rules = append(rules, ruleWithID(id, "f"+string(rune('a'+i))))
				}
				before := ruleIDs(rules)

				created, err := s.PutProfile(ctx(), model.AnonymizationProfile{Name: "ids " + c.name, Rules: rules})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(ruleIDs(rules), before) {
					t.Errorf("PutProfile modified the caller's rules: %v -> %v", before, ruleIDs(rules))
				}
				if len(created.Rules) != len(c.ids) {
					t.Fatalf("%d rules stored, want %d", len(created.Rules), len(c.ids))
				}
				seen := map[string]bool{}
				for i, r := range created.Rules {
					switch {
					case c.ids[i] != "":
						if r.ID != c.ids[i] {
							t.Errorf("rule %d: ID %q was replaced by %q", i, c.ids[i], r.ID)
						}
					case !strings.HasPrefix(r.ID, "rule_") || len(r.ID) <= len("rule_"):
						t.Errorf("rule %d: assigned ID %q, want rule_ prefix", i, r.ID)
					}
					if seen[r.ID] {
						t.Errorf("rule %d: duplicate ID %q", i, r.ID)
					}
					seen[r.ID] = true
				}

				got, err := s.GetProfile(ctx(), created.ID)
				if err != nil || !reflect.DeepEqual(got, created) {
					t.Fatalf("round trip: %v\n got %+v\nwant %+v", err, got, created)
				}

				// Saving the stored profile again keeps the IDs it was given.
				got.Secret = ""
				again, err := s.PutProfile(ctx(), got)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(ruleIDs(again.Rules), ruleIDs(created.Rules)) {
					t.Errorf("rule IDs changed on re-save: %v -> %v", ruleIDs(created.Rules), ruleIDs(again.Rules))
				}
			})
		}
	})
}

func TestProfileNamesAreUniqueIgnoringCase(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		alpha, err := s.PutProfile(ctx(), model.AnonymizationProfile{Name: "Alpha"})
		if err != nil {
			t.Fatal(err)
		}
		cafe, err := s.PutProfile(ctx(), model.AnonymizationProfile{Name: "Ünï Café"})
		if err != nil {
			t.Fatal(err)
		}
		names := func() []string {
			list, err := s.ListProfiles(ctx())
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, p := range list {
				out = append(out, p.Name)
			}
			return out
		}

		cases := []struct {
			name     string
			p        model.AnonymizationProfile
			conflict bool
		}{
			{"same name, other case", model.AnonymizationProfile{Name: "ALPHA"}, true},
			{"same name, lower case", model.AnonymizationProfile{Name: "alpha"}, true},
			{"padded with white space", model.AnonymizationProfile{Name: "  Alpha\t"}, true},
			{"the built-in name", model.AnonymizationProfile{Name: "default"}, true},
			{"non-ASCII letters", model.AnonymizationProfile{Name: "üNÏ cAFÉ"}, true},
			{"rename onto the built-in", model.AnonymizationProfile{ID: alpha.ID, Name: "DEFAULT"}, true},
			{"rename onto another profile", model.AnonymizationProfile{ID: cafe.ID, Name: "alpha"}, true},
			{"different name", model.AnonymizationProfile{Name: "Alpha 2"}, false},
			{"prefix of another name", model.AnonymizationProfile{Name: "Alph"}, false},
			{"own name in another case", model.AnonymizationProfile{ID: alpha.ID, Name: "aLPHA"}, false},
			{"own name unchanged", model.AnonymizationProfile{ID: model.DefaultProfileID, Name: "Default"}, false},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				snapshot := names()
				_, err := s.PutProfile(ctx(), c.p)
				if c.conflict {
					if !errors.Is(err, model.ErrConflict) {
						t.Fatalf("err = %v, want ErrConflict", err)
					}
					if !strings.Contains(err.Error(), "name") {
						t.Errorf("error %q does not mention the name", err)
					}
					if got := names(); !reflect.DeepEqual(got, snapshot) {
						t.Errorf("a refused put changed the profiles: %v -> %v", snapshot, got)
					}
					return
				}
				if err != nil {
					t.Fatalf("err = %v, want success", err)
				}
			})
		}

		// A refused rename leaves the profile as it was; a delete frees the name.
		if got, _ := s.GetProfile(ctx(), cafe.ID); got.Name != "Ünï Café" {
			t.Errorf("refused rename modified the stored name: %q", got.Name)
		}
		if err := s.DeleteProfile(ctx(), cafe.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutProfile(ctx(), model.AnonymizationProfile{Name: "ünï café"}); err != nil {
			t.Errorf("name of a deleted profile is still taken: %v", err)
		}
	})
}

// TestConcurrentProfileNameRace: of many simultaneous puts of one name (in
// different cases) exactly one wins.
func TestConcurrentProfileNameRace(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		const n = 12
		var (
			wg  sync.WaitGroup
			mu  sync.Mutex
			won int
		)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				name := "Contested"
				if i%2 == 1 {
					name = "CONTESTED"
				}
				_, err := s.PutProfile(ctx(), model.AnonymizationProfile{Name: name})
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					won++
				case !errors.Is(err, model.ErrConflict):
					t.Errorf("put %d: %v", i, err)
				}
			}()
		}
		wg.Wait()
		if won != 1 {
			t.Errorf("%d concurrent puts of one name succeeded, want 1", won)
		}
	})
}

// TestSeededDefaultProfile checks that a fresh database carries exactly
// model.DefaultAnonymizationProfile: its credential/name rules with their
// stable IDs and OpaqueBodies "keep".
func TestSeededDefaultProfile(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		got, err := s.GetProfile(ctx(), model.DefaultProfileID)
		if err != nil {
			t.Fatal(err)
		}
		want := model.DefaultAnonymizationProfile().WithDefaults()
		if len(want.Rules) == 0 {
			t.Fatal("model.DefaultAnonymizationProfile has no rules; this test is stale")
		}
		if got.OpaqueBodies != model.OpaqueKeep {
			t.Errorf("OpaqueBodies = %q, want %q", got.OpaqueBodies, model.OpaqueKeep)
		}
		if !reflect.DeepEqual(got.Rules, want.Rules) {
			t.Errorf("seeded rules differ from the model's: %d stored, %d expected", len(got.Rules), len(want.Rules))
		}
		seen := map[string]bool{}
		for i, r := range got.Rules {
			if !strings.HasPrefix(r.ID, "def-") {
				t.Errorf("rule %d (%s) has ID %q, want the model's stable def- ID", i, r.Name, r.ID)
			}
			if seen[r.ID] {
				t.Errorf("duplicate rule ID %q", r.ID)
			}
			seen[r.ID] = true
		}

		// The whole profile matches, apart from the generated secret and timestamps.
		want.Secret, want.HasSecret = got.Secret, true
		want.CreatedAt, want.UpdatedAt = got.CreatedAt, got.UpdatedAt
		if !reflect.DeepEqual(got, want) {
			t.Errorf("seeded profile differs from model.DefaultAnonymizationProfile:\n got %+v\nwant %+v", got, want)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("seeded profile does not validate: %v", err)
		}

		// Saving it unchanged keeps every ID.
		got.Secret = ""
		saved, err := s.PutProfile(ctx(), got)
		if err != nil || !reflect.DeepEqual(ruleIDs(saved.Rules), ruleIDs(want.Rules)) {
			t.Errorf("re-saving the default profile changed rule IDs: %v", err)
		}
	})
}
