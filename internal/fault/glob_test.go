package fault

import (
	"strings"
	"testing"
)

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		// "/**" matches everything.
		{"/**", "/", true},
		{"/**", "", true},
		{"/**", "/a", true},
		{"/**", "/a/b/c/d", true},

		// Root only.
		{"/", "/", true},
		{"/", "", true},
		{"/", "/a", false},

		// Literals, case-sensitive, trailing slash ignored on both sides.
		{"/users", "/users", true},
		{"/users", "/users/", true},
		{"/users/", "/users", true},
		{"/users/", "/users/", true},
		{"/users", "/users/1", false},
		{"/users", "/user", false},
		{"/users", "/usersx", false},
		{"/users", "/Users", false},
		{"/users", "/", false},
		{"/a/b/c", "/a/b/c", true},
		{"/a/b/c", "/a/b", false},
		{"/a/b", "/a/b/c", false},
		{"/a/b/c", "/a/x/c", false},
		{"/café", "/café", true},
		{"/v1.0/x", "/v1.0/x", true},
		{"/v1.0/x", "/v1x0/x", false}, // dots are not regex wildcards
		{"/a*", "/a*", true},          // partial wildcards are literal text
		{"/a*", "/ab", false},
		{"/a?b", "/a?b", true},

		// "*" is exactly one non-empty segment.
		{"/users/*", "/users/1", true},
		{"/users/*", "/users/alice", true},
		{"/users/*", "/users", false},
		{"/users/*", "/users/", false},
		{"/users/*", "/users/1/orders", false},
		{"/users/*", "/users//", false},
		{"/users/*/orders", "/users/42/orders", true},
		{"/users/*/orders", "/users/42/orders/", true},
		{"/users/*/orders", "/users/orders", false},
		{"/users/*/orders", "/users/42/orders/7", false},
		{"/users/*/orders", "/users/42/order", false},
		{"/users/*/orders", "/users//orders", false},
		{"/*", "/x", true},
		{"/*", "/", false},
		{"/*", "/x/y", false},
		{"/*/*", "/a/b", true},
		{"/*/*", "/a", false},
		{"/*/*", "/a/b/c", false},

		// "**" matches zero or more segments.
		{"/api/**", "/api", true},
		{"/api/**", "/api/", true},
		{"/api/**", "/api/v1", true},
		{"/api/**", "/api/v1/users/42", true},
		{"/api/**", "/apix", false},
		{"/api/**", "/ap", false},
		{"/api/**", "/other/api/x", false},
		{"/**/health", "/health", true},
		{"/**/health", "/a/health", true},
		{"/**/health", "/a/b/health", true},
		{"/**/health", "/a/b/healthz", false},
		{"/**/health", "/health/x", false},
		{"/a/**/b", "/a/b", true},
		{"/a/**/b", "/a/x/b", true},
		{"/a/**/b", "/a/x/y/b", true},
		{"/a/**/b", "/a", false},
		{"/a/**/b", "/a/x", false},
		{"/a/**/**/b", "/a/b", true}, // consecutive ** collapse
		{"/a/**/**/b", "/a/x/y/b", true},
		{"/**/*", "/a", true},
		{"/**/*", "/", false},
		{"/**/*", "/a/b", true},
		{"/a/*/**", "/a/b", true},
		{"/a/*/**", "/a/b/c/d", true},
		{"/a/*/**", "/a", false},

		// Backtracking across several "**".
		{"/a/**/b/**/c", "/a/b/c", true},
		{"/a/**/b/**/c", "/a/x/b/y/c", true},
		{"/a/**/b/**/c", "/a/x/y/b/c", true},
		{"/a/**/b/**/c", "/a/b/b/c", true},
		{"/a/**/b/**/c", "/a/c", false},
		{"/a/**/b/**/c", "/a/b", false},
		{"/a/**/b/**/c", "/a/x/b/y/z/c/d", false},
		{"/**/a/b", "/a/a/b", true},
		{"/**/a/b", "/a/b/a/b", true},
		{"/**/a/b", "/a/b/a", false},
		{"/**/a/*/c", "/a/a/x/c", true},
		{"/**/a/*/c", "/a/x/c", true},
		{"/**/a/*/c", "/a/c", false},
		{"/files/**/report.csv", "/files/report.csv", true},
		{"/files/**/report.csv", "/files/2024/q1/report.csv", true},
		{"/files/**/report.csv", "/files/2024/q1/report.csv.gz", false},
	}
	for _, tt := range tests {
		g, err := compileGlob(tt.pattern)
		if err != nil {
			t.Fatalf("compileGlob(%q): %v", tt.pattern, err)
		}
		if got := g.match(tt.path); got != tt.want {
			t.Errorf("glob %q on %q = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func TestCompileGlobErrors(t *testing.T) {
	// compileGlob knows nothing of "re:" patterns (the injector strips that
	// prefix first); it only demands a leading slash.
	for _, pattern := range []string{"", "users", "re:^/x", "*", "**"} {
		_, err := compileGlob(pattern)
		if err == nil {
			t.Errorf("compileGlob(%q): expected error", pattern)
			continue
		}
		if !strings.Contains(err.Error(), "must start with /") {
			t.Errorf("compileGlob(%q) error = %v", pattern, err)
		}
	}
}

func TestCompileGlobCollapsesDoubleStars(t *testing.T) {
	g, err := compileGlob("/a/**/**/**/b")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.segs) != 3 {
		t.Fatalf("segments = %d, want 3 (a, **, b)", len(g.segs))
	}
	all, _ := compileGlob("/**/**")
	if !all.all {
		t.Error("/**/** should collapse to match-all")
	}
}

// TestGlobMatchPathologicalDoesNotBlowUp guards against exponential
// backtracking: many "**" segments against a long non-matching path must
// finish quickly (the algorithm is O(len(path) * len(pattern))).
func TestGlobMatchPathologicalDoesNotBlowUp(t *testing.T) {
	pattern := "/" + strings.Repeat("**/a/", 20) + "z"
	g, err := compileGlob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	path := "/" + strings.Repeat("a/", 500) + "b"
	if g.match(path) {
		t.Fatal("unexpected match")
	}
}
