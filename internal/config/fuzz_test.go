package config

import (
	"errors"
	"os"
	"testing"
)

// FuzzParse checks that arbitrary input never panics, that every failure is a
// *Error, and that a successful parse yields a configuration that validates.
func FuzzParse(f *testing.F) {
	example, err := os.ReadFile("../../examples/mirage.yaml")
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range []string{
		fullYAML, fullJSON, string(example), baseYAML,
		"", "{}", "[]", "null", "a: b", "? [a]\n: 1", "a: &x [*x]", "<<: *a", "services:\n  - <<: [{a: 1}]\n",
		"server: {data: ${A:-b}}", "x: $${", "a:\n\t- b", "{\"a\": 1, \"a\": 2}", "\xef\xbb\xbf{}",
	} {
		f.Add(seed, false)
		f.Add(seed, true)
	}
	f.Fuzz(func(t *testing.T, src string, asJSON bool) {
		name := "fuzz.yaml"
		if asJSON {
			name = "fuzz.json"
		}
		c, err := parse(name, []byte(src), func(string) (string, bool) { return "v", true })
		if err != nil {
			var ce *Error
			if !errors.As(err, &ce) || len(ce.Problems) == 0 {
				t.Fatalf("error %T %v is not a *Error with problems", err, err)
			}
			_ = err.Error()
			return
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("parsed config does not validate: %v", err)
		}
	})
}
