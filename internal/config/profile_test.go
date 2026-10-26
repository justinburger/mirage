package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadProfile(t *testing.T) {
	const yamlProfile = `
name: strict
secret: abc123
detectors: [email, ssn]
redact_headers: [Authorization]
rules:
  - name: customer id
    enabled: true
    target: json
    selector: $..customer_id
    strategy: hash
  - name: card
    enabled: true
    target: json
    selector: $.card.last4
    scope: response
    strategy: mask
    keep_suffix: 2
`
	t.Run("bare yaml", func(t *testing.T) {
		p, err := LoadProfile(writeTemp(t, "p.yaml", yamlProfile))
		if err != nil {
			t.Fatal(err)
		}
		if p.Name != "strict" || p.Secret != "abc123" || len(p.Rules) != 2 || len(p.Detectors) != 2 {
			t.Fatalf("unexpected profile: %+v", p)
		}
		if p.DetectorStrategy != model.StrategyFake || p.Rules[0].Scope != model.ScopeBoth {
			t.Errorf("defaults not applied: %+v", p)
		}
	})
	t.Run("json", func(t *testing.T) {
		p, err := LoadProfile(writeTemp(t, "p.json", `{"name":"j","detectors":["email"]}`))
		if err != nil || p.Name != "j" {
			t.Fatalf("got %+v, %v", p, err)
		}
	})
	t.Run("name from file", func(t *testing.T) {
		p, err := LoadProfile(writeTemp(t, "my-profile.yaml", "detectors: [email]\n"))
		if err != nil || p.Name != "my-profile" {
			t.Fatalf("got %+v, %v", p, err)
		}
	})
	t.Run("inside a config file", func(t *testing.T) {
		p, err := LoadProfile(writeTemp(t, "c.yaml", "profiles:\n  - name: inner\n    detectors: [email]\n"))
		if err != nil || p.Name != "inner" {
			t.Fatalf("got %+v, %v", p, err)
		}
	})
	t.Run("several profiles", func(t *testing.T) {
		_, err := LoadProfile(writeTemp(t, "c.yaml", "profiles:\n  - name: a\n  - name: b\n"))
		if err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("unknown field names the line", func(t *testing.T) {
		_, err := LoadProfile(writeTemp(t, "p.yaml", "name: x\ndetector: [email]\n"))
		var ce *Error
		if !errors.As(err, &ce) || !errors.Is(err, model.ErrValidation) {
			t.Fatalf("got %v", err)
		}
		if !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "detectors") {
			t.Errorf("error should point at line 2 and suggest detectors: %v", err)
		}
	})
	t.Run("invalid rule", func(t *testing.T) {
		_, err := LoadProfile(writeTemp(t, "p.yaml", "name: x\nrules:\n  - target: json\n    selector: nope\n"))
		if !errors.Is(err, model.ErrValidation) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if _, err := LoadProfile(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("not an object", func(t *testing.T) {
		if _, err := LoadProfile(writeTemp(t, "p.yaml", "- a\n- b\n")); err == nil {
			t.Fatal("want error")
		}
	})
}
