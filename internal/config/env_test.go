package config

import (
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestExpandEnv(t *testing.T) {
	env := map[string]string{"A": "alpha", "EMPTY": "", "PORT": "9001", "_X1": "x"}
	tests := []struct {
		name     string
		in       string
		comments bool
		want     string
		errPart  string
	}{
		{name: "no references", in: "a: b\n", want: "a: b\n"},
		{name: "simple", in: "a: ${A}\n", want: "a: alpha\n"},
		{name: "several per line", in: "addr: ${A}:${PORT}\n", want: "addr: alpha:9001\n"},
		{name: "set but empty", in: "a: [${EMPTY}]", want: "a: []"},
		{name: "default unused", in: "a: ${A:-fallback}", want: "a: alpha"},
		{name: "default for unset", in: "a: ${NOPE:-fallback}", want: "a: fallback"},
		{name: "default for empty", in: "a: ${EMPTY:-fallback}", want: "a: fallback"},
		{name: "empty default", in: "a: ${NOPE:-}", want: "a: "},
		{name: "default with spaces and colons", in: "a: ${NOPE:-http://x y:1}", want: "a: http://x y:1"},
		{name: "underscore and digit names", in: "${_X1}", want: "x"},
		{name: "escape", in: "a: $${A}", want: "a: ${A}"},
		{name: "escape then reference", in: "$${A} ${A}", want: "${A} alpha"},
		{name: "bare dollar variable untouched", in: "p: pa$$word $A $", want: "p: pa$$word $A $"},
		{name: "dollar before brace-less text", in: "re: ^a$ and $5", want: "re: ^a$ and $5"},
		{name: "comment line skipped in yaml", comments: true, in: "# token: ${NOPE}\na: ${A}\n", want: "# token: ${NOPE}\na: alpha\n"},
		{name: "indented comment skipped", comments: true, in: "x:\n  # ${NOPE}\n  y: 1", want: "x:\n  # ${NOPE}\n  y: 1"},
		{name: "comment line expanded when not yaml", in: "# ${A}\n", want: "# alpha\n"},
		{name: "trailing comment is expanded", comments: true, in: "a: 1 # ${A}", want: "a: 1 # alpha"},
		{name: "unset without default", in: "a: ${NOPE}", errPart: "line 1: environment variable NOPE is not set"},
		{name: "error carries its line", in: "a: 1\nb: 2\nc: ${NOPE}\n", errPart: "line 3:"},
		{name: "unterminated", in: "a: ${A", errPart: "unterminated"},
		{name: "empty name", in: "a: ${}", errPart: "invalid variable reference"},
		{name: "bad name", in: "a: ${1BAD}", errPart: "invalid variable reference"},
		{name: "bad name with dash", in: "a: ${A-B}", errPart: "invalid variable reference"},
		{name: "unicode value", in: "a: ${U}", want: "a: héllo ✓"},
		{name: "crlf preserved", in: "a: ${A}\r\nb: 1\r\n", want: "a: alpha\r\nb: 1\r\n"},
	}
	env["U"] = "héllo ✓"
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expandEnv([]byte(tc.in), tc.comments, envMap(env))
			if tc.errPart != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errPart) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.errPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExpandEnvDoesNotModifyInput(t *testing.T) {
	in := []byte("a: ${A}\nb: ${A:-x}\n")
	orig := string(in)
	if _, err := expandEnv(in, true, envMap(map[string]string{"A": "v"})); err != nil {
		t.Fatal(err)
	}
	if string(in) != orig {
		t.Errorf("input was modified: %q", in)
	}
}

func TestExpandEnvValuesAreNotRescanned(t *testing.T) {
	// A value that itself looks like a reference must come through verbatim.
	got, err := expandEnv([]byte("a: ${A}"), false, envMap(map[string]string{"A": "${B}", "B": "no"}))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a: ${B}" {
		t.Errorf("got %q", got)
	}
}
