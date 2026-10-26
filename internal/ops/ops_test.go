package ops

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/fault"
	"github.com/justinburger/mirage/internal/model"
)

// The profile id appears once in the error of an unknown profile.
func TestLoadEngineUnknownProfileNamesItOnce(t *testing.T) {
	_, err := LoadEngine(context.Background(), newStore(t), "prof_nope")
	if !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if n := strings.Count(err.Error(), "prof_nope"); n != 1 {
		t.Errorf("err = %q names the profile %d times, want once", err, n)
	}
}

func TestLoadEngine(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	prof, err := st.PutProfile(ctx, model.AnonymizationProfile{
		Name:             "custom",
		Detectors:        []model.Detector{model.DetectEmail},
		DetectorStrategy: model.StrategyHash,
		Rules: []model.AnonymizationRule{
			{Name: "customer", Enabled: true, Target: model.TargetJSON, Selector: "$..customer_id", Strategy: model.StrategyHash},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	broken, err := st.PutProfile(ctx, model.AnonymizationProfile{
		Name:  "broken",
		Rules: []model.AnonymizationRule{{Name: "bad", Enabled: true, Target: model.TargetJSON, Selector: "$.[[", Strategy: model.StrategyHash}},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		id      string
		wantID  string // expected Engine.Profile().ID
		wantErr error
	}{
		{name: "empty id selects the default profile", id: "", wantID: model.DefaultProfileID},
		{name: "explicit default", id: model.DefaultProfileID, wantID: model.DefaultProfileID},
		{name: "custom profile", id: prof.ID, wantID: prof.ID},
		{name: "unknown profile", id: "prof_nope", wantErr: model.ErrNotFound},
		{name: "profile that does not compile", id: broken.ID, wantErr: model.ErrValidation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eng, err := LoadEngine(ctx, st, tc.id)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if eng != nil {
					t.Error("engine returned together with an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadEngine: %v", err)
			}
			got := eng.Profile()
			if got.ID != tc.wantID {
				t.Errorf("profile ID = %q, want %q", got.ID, tc.wantID)
			}
			if got.Secret != "" || !got.HasSecret {
				t.Errorf("engine profile must hide the secret and report it: Secret=%q HasSecret=%v", got.Secret, got.HasSecret)
			}
		})
	}

	t.Run("tokens are stable across loads", func(t *testing.T) {
		a, err := LoadEngine(ctx, st, prof.ID)
		if err != nil {
			t.Fatal(err)
		}
		b, err := LoadEngine(ctx, st, prof.ID)
		if err != nil {
			t.Fatal(err)
		}
		ex1, ex2 := mkExchange(7), mkExchange(7)
		if err := a.Exchange(ex1); err != nil {
			t.Fatal(err)
		}
		if err := b.Exchange(ex2); err != nil {
			t.Fatal(err)
		}
		if got, want := string(ex1.Request.Body), string(ex2.Request.Body); got != want {
			t.Errorf("two engines for one stored profile disagree:\n%s\n%s", got, want)
		}
		if strings.Contains(string(ex1.Request.Body), "cust-7") {
			t.Errorf("customer_id not anonymized: %s", ex1.Request.Body)
		}
	})

	t.Run("profile without secret is an error", func(t *testing.T) {
		flaky := &flakyStore{Store: st, failGetProf: func(id string) (model.AnonymizationProfile, bool) {
			p := model.DefaultAnonymizationProfile()
			p.Secret = ""
			return p, true
		}}
		_, err := LoadEngine(ctx, flaky, "")
		if err == nil || !strings.Contains(err.Error(), "no secret") {
			t.Fatalf("err = %v, want a no-secret error", err)
		}
		if errors.Is(err, model.ErrNotFound) || errors.Is(err, model.ErrValidation) {
			t.Errorf("a missing secret is a server-side problem, got %v", err)
		}
	})
}

func TestLoadFaults(t *testing.T) {
	ctx := context.Background()

	t.Run("loads every stored rule in order", func(t *testing.T) {
		st := newStore(t)
		for _, r := range []model.FaultRule{
			{Name: "b-late", Enabled: true, Order: 2, Probability: 1, Type: model.FaultAbort},
			{Name: "a-first", Enabled: true, Order: 1, Probability: 0.5, Type: model.FaultLatency, Delay: model.Duration(5e6)},
			{Name: "off", Enabled: false, Order: 3, Probability: 1, Type: model.FaultError},
		} {
			if _, err := st.PutFaultRule(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		inj := fault.NewInjector()
		if err := LoadFaults(ctx, st, inj); err != nil {
			t.Fatalf("LoadFaults: %v", err)
		}
		rules := inj.Rules()
		var names []string
		for _, r := range rules {
			names = append(names, r.Name)
		}
		if got, want := strings.Join(names, ","), "a-first,b-late,off"; got != want {
			t.Errorf("rules = %s, want %s", got, want)
		}
		if rules[2].Status != 503 {
			t.Errorf("defaults not applied: status = %d", rules[2].Status)
		}
		if got := len(inj.Counts()); got != 3 {
			t.Errorf("Counts has %d entries, want 3", got)
		}
	})

	t.Run("reload replaces the rule set and keeps fire counts of unchanged ids", func(t *testing.T) {
		st := newStore(t)
		keep, err := st.PutFaultRule(ctx, model.FaultRule{Name: "keep", Enabled: true, Probability: 1, Type: model.FaultAbort})
		if err != nil {
			t.Fatal(err)
		}
		drop, err := st.PutFaultRule(ctx, model.FaultRule{Name: "drop", Enabled: true, Probability: 1, Type: model.FaultAbort})
		if err != nil {
			t.Fatal(err)
		}
		inj := fault.NewInjector()
		if err := LoadFaults(ctx, st, inj); err != nil {
			t.Fatal(err)
		}
		if err := st.DeleteFaultRule(ctx, drop.ID); err != nil {
			t.Fatal(err)
		}
		if err := LoadFaults(ctx, st, inj); err != nil {
			t.Fatal(err)
		}
		counts := inj.Counts()
		if _, ok := counts[keep.ID]; !ok || len(counts) != 1 {
			t.Errorf("Counts = %v, want only %s", counts, keep.ID)
		}
	})

	t.Run("empty store clears the injector", func(t *testing.T) {
		st := newStore(t)
		inj := fault.NewInjector()
		if err := inj.SetRules([]model.FaultRule{{ID: "x", Name: "x", Probability: 1, Type: model.FaultAbort}}); err != nil {
			t.Fatal(err)
		}
		if err := LoadFaults(ctx, st, inj); err != nil {
			t.Fatal(err)
		}
		if n := len(inj.Rules()); n != 0 {
			t.Errorf("injector still has %d rules", n)
		}
	})

	t.Run("invalid stored rules leave the previous rules active", func(t *testing.T) {
		st := newStore(t)
		if _, err := st.PutFaultRule(ctx, model.FaultRule{Name: "bad", Enabled: true, Probability: 7, Type: model.FaultAbort}); err != nil {
			t.Fatal(err)
		}
		inj := fault.NewInjector()
		if err := inj.SetRules([]model.FaultRule{{ID: "old", Name: "old", Probability: 1, Type: model.FaultAbort}}); err != nil {
			t.Fatal(err)
		}
		err := LoadFaults(ctx, st, inj)
		if !errors.Is(err, model.ErrValidation) {
			t.Fatalf("err = %v, want validation error", err)
		}
		if rules := inj.Rules(); len(rules) != 1 || rules[0].Name != "old" {
			t.Errorf("previous rules were replaced: %+v", rules)
		}
	})

	t.Run("store failure", func(t *testing.T) {
		st := &flakyStore{Store: newStore(t), failList: true}
		if err := LoadFaults(ctx, st, fault.NewInjector()); !errors.Is(err, errInjected) {
			t.Fatalf("err = %v, want the store error", err)
		}
	})

	t.Run("nil injector", func(t *testing.T) {
		if err := LoadFaults(ctx, newStore(t), nil); err == nil {
			t.Fatal("want an error for a nil injector")
		}
	})
}

func TestDiscardReportsAFailedCleanup(t *testing.T) {
	ctx := context.Background()
	real := newStore(t)
	src := seed(t, real, "src", 600)
	eng, _ := LoadEngine(ctx, real, "")
	st := &flakyStore{Store: real, failAppendOn: 2, failDelete: true}
	_, err := CopyAnonymized(ctx, st, src.ID, eng, "doomed", nil)
	if !errors.Is(err, errInjected) {
		t.Fatalf("err = %v, want the original failure to stay recognisable", err)
	}
	if !strings.Contains(err.Error(), "could not be deleted") {
		t.Errorf("err = %q, want it to say the partial recording is still there", err)
	}
}
