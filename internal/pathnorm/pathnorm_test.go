package pathnorm

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestTemplateHeuristics(t *testing.T) {
	const (
		uuidLower = "123e4567-e89b-12d3-a456-426614174000"
		uuidUpper = "123E4567-E89B-12D3-A456-426614174000"
		uuidMixed = "123e4567-E89B-12d3-A456-426614174000"
		oid24     = "507f1f77bcf86cd799439011"
		sha256    = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	)
	tests := []struct {
		name string
		path string
		want string
	}{
		// Roots and trivial inputs.
		{"root", "/", "/"},
		{"empty", "", "/"},
		{"only slashes", "///", "/"},
		{"options star", "*", "*"},
		{"no leading slash", "users/42", "/users/{id}"},
		{"no leading slash literal", "users/me", "/users/me"},

		// Literals that must NOT be normalised.
		{"literal single", "/users", "/users"},
		{"literal nested", "/api/v1/users/me", "/api/v1/users/me"},
		{"version v1", "/v1/status", "/v1/status"},
		{"version v2", "/v2", "/v2"},
		{"api word", "/api", "/api"},
		{"2fa", "/auth/2fa/verify", "/auth/2fa/verify"},
		{"html5", "/docs/html5", "/docs/html5"},
		{"me", "/users/me/orders", "/users/me/orders"},
		{"short mixed slug", "/p/abc123", "/p/abc123"},
		{"short slug with dash", "/blog/go-1-23-released", "/blog/go-1-23-released"},
		{"19 char mixed token", "/t/abcdefghi1234567890", "/t/abcdefghi1234567890"},
		{"words only long", "/t/thequickbrownfoxjumpsoverthelazydog", "/t/thequickbrownfoxjumpsoverthelazydog"},
		{"digits and dash only long", "/t/1234-5678-9012-3456-7890", "/t/1234-5678-9012-3456-7890"},
		{"hex 15 chars", "/h/abcdef012345678", "/h/abcdef012345678"},
		{"word with hex letters", "/h/deadbeef", "/h/deadbeef"},
		{"dot segment", "/a/./b", "/a/./b"},
		{"dotdot segment", "/a/../b", "/a/../b"},
		{"extension stays literal", "/files/42.json", "/files/42.json"},
		{"negative number", "/n/-5", "/n/-5"},
		{"decimal", "/n/3.14", "/n/3.14"},
		{"braces already", "/users/{id}/orders", "/users/{id}/orders"},
		{"percent kept", "/a%20b/c%2Fd", "/a%20b/c%2Fd"},
		{"percent in lit with digits", "/q/%7B1%7D", "/q/%7B1%7D"},
		{"at-name", "/@me/feed", "/@me/feed"},
		{"scoped npm", "/@types/node", "/@types/node"},
		{"colon", "/a:b/c", "/a:b/c"},
		{"unicode", "/héllo/wörld", "/héllo/wörld"},
		{"uppercase literal kept", "/Users/Me", "/Users/Me"},

		// Numeric ids.
		{"id", "/users/42", "/users/{id}"},
		{"id zero", "/users/0", "/users/{id}"},
		{"id leading zeros", "/users/007", "/users/{id}"},
		{"id huge", "/users/123456789012345678901234567890", "/users/{id}"},
		{"id first", "/42", "/{id}"},
		{"id only", "/42/43/44", "/{id}/{id}/{id}"},
		{"nested ids", "/users/42/orders/9", "/users/{id}/orders/{id}"},
		{"year is id", "/archive/2024", "/archive/{id}"},
		{"16 digits is id not hex", "/x/1234567890123456", "/x/{id}"},
		{"20 digits is id not token", "/x/12345678901234567890", "/x/{id}"},

		// UUIDs.
		{"uuid lower", "/orders/" + uuidLower, "/orders/{uuid}"},
		{"uuid upper", "/orders/" + uuidUpper, "/orders/{uuid}"},
		{"uuid mixed", "/orders/" + uuidMixed, "/orders/{uuid}"},
		{"uuid v7-ish", "/o/01890a5d-ac96-774b-bcce-b302099a8057", "/o/{uuid}"},
		{"uuid nil", "/o/00000000-0000-0000-0000-000000000000", "/o/{uuid}"},
		{"uuid max", "/o/ffffffff-ffff-ffff-ffff-ffffffffffff", "/o/{uuid}"},
		{"uuid then id", "/o/" + uuidLower + "/items/3", "/o/{uuid}/items/{id}"},
		{"uuid-like short last group is a token", "/o/123e4567-e89b-12d3-a456-42661417400", "/o/{token}"},
		{"uuid-like non hex is a token", "/o/123e4567-e89b-12d3-a456-42661417400g", "/o/{token}"},
		{"uuid wrong dashes", "/o/123e4567e-89b-12d3-a456-426614174000", "/o/{token}"},

		// Hex.
		{"hex 16", "/h/0123456789abcdef", "/h/{hex}"},
		{"hex letters only 16", "/h/deadbeefdeadbeef", "/h/{hex}"},
		{"hex objectid", "/docs/" + oid24, "/docs/{hex}"},
		{"hex sha256", "/blobs/" + sha256, "/blobs/{hex}"},
		{"hex upper", "/h/0123456789ABCDEF", "/h/{hex}"},
		{"hex uuid without dashes", "/h/123e4567e89b12d3a456426614174000", "/h/{hex}"},
		{"hex 20 beats token", "/h/0123456789abcdef0123", "/h/{hex}"},

		// Tokens.
		{"token alnum", "/s/abcdefghij1234567890", "/s/{token}"},
		{"token exactly 20", "/s/abcdefghij0123456789", "/s/{token}"},
		{"token with dash underscore", "/s/ab-cd_ef-12-34_gh-56-78-ij", "/s/{token}"},
		{"token jwt segment", "/s/eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9", "/s/{token}"},
		{"token stripe-like", "/pay/pi_3MtwBwLkdIwHu7ix28a3tqPa", "/pay/{token}"},
		{"token long slug with digits", "/blog/how-to-benchmark-go-in-2026", "/blog/{token}"},
		{"token needs digit", "/s/abcdefghij-klmnopqrst", "/s/abcdefghij-klmnopqrst"},
		{"token needs letter", "/s/1234567890-1234567890", "/s/1234567890-1234567890"},
		{"token with plus not token", "/s/abcdefghij+1234567890", "/s/abcdefghij+1234567890"},
		{"token with dot not token", "/s/abcdefghij.1234567890", "/s/abcdefghij.1234567890"},

		// Emails.
		{"email", "/users/jane@example.com", "/users/{email}"},
		{"email plus", "/users/jane+tag@example.com", "/users/{email}"},
		{"email dots", "/users/first.last@mail.example.co.uk/prefs", "/users/{email}/prefs"},
		{"email escaped at", "/users/jane%40example.com", "/users/{email}"},
		{"email escaped plus and at", "/users/jane%2Btag%40example.com", "/users/{email}"},
		{"email idn domain", "/users/jane@bücher.de", "/users/{email}"},
		{"email no domain dot", "/users/jane@localhost", "/users/jane@localhost"},
		{"email empty local", "/users/@example.com", "/users/@example.com"},
		{"email empty domain", "/users/jane@", "/users/jane@"},
		{"email one char tld", "/users/jane@example.c", "/users/jane@example.c"},
		{"email numeric tld", "/users/jane@1.2.3.4", "/users/jane@1.2.3.4"},
		{"email double dot", "/users/jane@example..com", "/users/jane@example..com"},
		{"email leading dash label", "/users/jane@-example.com", "/users/jane@-example.com"},
		{"email trailing dash label", "/users/jane@example-.com", "/users/jane@example-.com"},
		{"email underscore domain", "/users/jane@exa_mple.com", "/users/jane@exa_mple.com"},
		{"email two ats uses last", "/users/a@b@example.com", "/users/a@b@example.com"},
		{"email escaped at but bad domain", "/users/jane%40localhost", "/users/jane%40localhost"},

		// Dates.
		{"date", "/reports/2024-01-31", "/reports/{date}"},
		{"date leap day", "/reports/2024-02-29", "/reports/{date}"},
		{"date non leap feb 29", "/reports/2023-02-29", "/reports/2023-02-29"},
		{"date century non leap", "/reports/1900-02-29", "/reports/1900-02-29"},
		{"date 400 leap", "/reports/2000-02-29", "/reports/{date}"},
		{"date april 31", "/reports/2024-04-31", "/reports/2024-04-31"},
		{"date month 13", "/reports/2024-13-01", "/reports/2024-13-01"},
		{"date month 00", "/reports/2024-00-10", "/reports/2024-00-10"},
		{"date day 00", "/reports/2024-01-00", "/reports/2024-01-00"},
		{"date year-month only", "/reports/2024-01", "/reports/2024-01"},
		{"date slashes are segments", "/reports/2024/01/31", "/reports/{id}/{id}/{id}"},
		{"date compact is id", "/reports/20240131", "/reports/{id}"},
		{"date datetime stays literal", "/reports/2024-01-31T10:00:00Z", "/reports/2024-01-31T10:00:00Z"},
		{"date then id", "/reports/2024-12-01/rows/5", "/reports/{date}/rows/{id}"},

		// Mixed paths.
		{"mixed full", "/v1/users/42/orders/" + uuidLower + "/events/2024-05-06/by/a@b.io",
			"/v1/users/{id}/orders/{uuid}/events/{date}/by/{email}"},
		{"mixed with hex and token", "/v2/shas/" + sha256 + "/sessions/abcdefghij1234567890",
			"/v2/shas/{hex}/sessions/{token}"},
		{"consecutive empty middle", "/a//b", "/a//b"},
		{"empty middle id", "/a//42", "/a//{id}"},
		{"leading double slash", "//a/42", "//a/{id}"},

		// Trailing slash and query.
		{"trailing slash", "/users/42/", "/users/{id}"},
		{"trailing slash literal", "/users/", "/users"},
		{"trailing slashes many", "/users/42///", "/users/{id}"},
		{"trailing slash same as without", "/api/v1/", "/api/v1"},
		{"query ignored", "/users/42?x=1&y=2", "/users/{id}"},
		{"query ignored literal", "/users?x=42", "/users"},
		{"query with slash ignored", "/users/42?next=/a/b/99", "/users/{id}"},
		{"query only after root", "/?a=1", "/"},
		{"query empty", "/users/?", "/users"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Default.Template(tc.path); got != tc.want {
				t.Errorf("Template(%q) = %q, want %q", tc.path, got, tc.want)
			}
			// Templates are fixed points.
			if again := Default.Template(tc.want); again != tc.want {
				t.Errorf("Template(%q) = %q, not a fixed point", tc.want, again)
			}
		})
	}
}

func TestTemplateIsAFixedPointWithPatterns(t *testing.T) {
	n := mustNew(t, "/users/{id}/files/{path...}", "/x/{a}/y", "/x/{b}/{c}", "/s/{n}")
	for _, p := range []string{"/users/9/files/a/b/c", "/x/1/y", "/x/1/z", "/s/abc", "/other/42"} {
		first := n.Template(p)
		if second := n.Template(first); second != first {
			t.Errorf("Template(Template(%q)) = %q, want %q", p, second, first)
		}
	}
}

func TestEndpoint(t *testing.T) {
	tests := []struct {
		method, path, want string
	}{
		{"GET", "/users/42", "GET /users/{id}"},
		{"get", "/users/42", "GET /users/{id}"},
		{"Post", "/users/42/orders", "POST /users/{id}/orders"},
		{"delete", "/", "DELETE /"},
		{"PATCH", "", "PATCH /"},
		{"options", "*", "OPTIONS *"},
		{"get", "users/42", "GET /users/{id}"},
		{"get", "/users/42/", "GET /users/{id}"},
		{"get", "/users/42?x=1", "GET /users/{id}"},
		{"purge", "/cache/abcdefghij1234567890", "PURGE /cache/{token}"},
		{"propfind", "/dav/a%20b/42", "PROPFIND /dav/a%20b/{id}"},
		{"gét", "/a", "GÉT /a"}, // non-ASCII upper-casing matches strings.ToUpper
		{"", "/a", " /a"},       // contract: ToUpper("") + " " + Template
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			if got := Default.Endpoint(tc.method, tc.path); got != tc.want {
				t.Errorf("Endpoint(%q, %q) = %q, want %q", tc.method, tc.path, got, tc.want)
			}
			want := strings.ToUpper(tc.method) + " " + Default.Template(tc.path)
			if got := Default.Endpoint(tc.method, tc.path); got != want {
				t.Errorf("Endpoint(%q, %q) = %q, contract says %q", tc.method, tc.path, got, want)
			}
		})
	}
}

func TestEndpointWithPatterns(t *testing.T) {
	n := mustNew(t, "/users/{id}", "/files/{path...}")
	tests := []struct{ method, path, want string }{
		{"get", "/users/abc", "GET /users/{id}"},
		{"put", "/files/a/b/c.txt", "PUT /files/{path...}"},
		{"get", "/other/42", "GET /other/{id}"},
	}
	for _, tc := range tests {
		if got := n.Endpoint(tc.method, tc.path); got != tc.want {
			t.Errorf("Endpoint(%q, %q) = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestDefault(t *testing.T) {
	if Default == nil {
		t.Fatal("Default is nil")
	}
	if len(Default.patterns) != 0 {
		t.Errorf("Default has %d patterns, want 0", len(Default.patterns))
	}
	if got := Default.Template("/users/42"); got != "/users/{id}" {
		t.Errorf("Default.Template = %q", got)
	}
}

func TestNilNormalizer(t *testing.T) {
	var n *Normalizer
	if got := n.Template("/users/42/"); got != "/users/{id}" {
		t.Errorf("nil Template = %q", got)
	}
	if got := n.Endpoint("get", "/users/42"); got != "GET /users/{id}" {
		t.Errorf("nil Endpoint = %q", got)
	}
	if got := n.Patterns(); got != nil {
		t.Errorf("nil Patterns = %v", got)
	}
}

func TestNewEmpty(t *testing.T) {
	for _, in := range [][]string{nil, {}} {
		n, err := New(in)
		if err != nil || n == nil {
			t.Fatalf("New(%v) = %v, %v", in, n, err)
		}
		if got := n.Patterns(); got != nil {
			t.Errorf("Patterns() = %v, want nil", got)
		}
	}
}

func TestNewErrors(t *testing.T) {
	tests := []struct {
		pattern string
		reason  string
	}{
		{"", "must start with /"},
		{"users/{id}", "must start with /"},
		{"{id}", "must start with /"},
		{"/users/{id", "unbalanced"},
		{"/users/id}", "unbalanced"},
		{"/users/{{id}}", "invalid character"},
		{"/users/{a}{b}", "invalid character"},
		{"/users/a{id}", "unbalanced"},
		{"/users/{id}b", "unbalanced"},
		{"/users/{}", "empty parameter name"},
		{"/users/{...}", "empty parameter name"},
		{"/users/{..}", "invalid parameter name"},
		{"/users/{a..b}", "invalid parameter name"},
		{"/users/{a b}", "invalid character"},
		{"/users/{a/b}", "unbalanced"},
		{"/files/{path...}/more", "last segment"},
		{"/{rest...}/x/{id}", "last segment"},
		{"/a//b", "empty path segment"},
		{"/a?b=1", "query string"},
		{"/a#frag", "query string"},
	}
	for _, tc := range tests {
		t.Run(tc.pattern, func(t *testing.T) {
			n, err := New([]string{tc.pattern})
			if err == nil {
				t.Fatalf("New(%q) succeeded with %v, want error", tc.pattern, n.Patterns())
			}
			if n != nil {
				t.Errorf("New returned a non-nil normalizer with an error")
			}
			var pe *PatternError
			if !errors.As(err, &pe) {
				t.Fatalf("error %T is not a *PatternError", err)
			}
			if pe.Pattern != tc.pattern {
				t.Errorf("PatternError.Pattern = %q, want %q", pe.Pattern, tc.pattern)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("error %q does not mention %q", err, tc.reason)
			}
			if vErr := ValidatePattern(tc.pattern); vErr == nil {
				t.Errorf("ValidatePattern(%q) = nil, want error", tc.pattern)
			}
		})
	}
}

func TestNewJoinsAllErrors(t *testing.T) {
	_, err := New([]string{"/ok/{id}", "bad", "/also/{bad", "/fine"})
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{`"bad"`, `"/also/{bad"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %s", err, want)
		}
	}
	if strings.Contains(err.Error(), `"/ok/{id}"`) || strings.Contains(err.Error(), `"/fine"`) {
		t.Errorf("error %q mentions a valid pattern", err)
	}
}

func TestValidatePatternAccepts(t *testing.T) {
	for _, p := range []string{
		"/", "/users", "/users/", "/users/{id}", "/users/{user_id}/orders/{order-id}",
		"/files/{path...}", "/{rest...}", "/a/{b.c}", "/v1/{id}/x/", "/a%20b/{id}", "/{a}/{b}/{c}",
		"/*", "/**", "/users/{ID}",
	} {
		if err := ValidatePattern(p); err != nil {
			t.Errorf("ValidatePattern(%q) = %v", p, err)
		}
	}
}

func TestUserPatterns(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		path     string
		want     string
	}{
		{"simple", []string{"/users/{id}"}, "/users/42", "/users/{id}"},
		{"param name verbatim", []string{"/users/{userId}"}, "/users/42", "/users/{userId}"},
		{"param matches non-numeric", []string{"/users/{id}"}, "/users/jane", "/users/{id}"},
		{"param overrides heuristic", []string{"/users/{name}"}, "/users/42", "/users/{name}"},
		{"param matches uuid", []string{"/users/{name}"}, "/users/123e4567-e89b-12d3-a456-426614174000", "/users/{name}"},
		{"no match falls to heuristic", []string{"/users/{id}"}, "/orders/42", "/orders/{id}"},
		{"longer path not matched", []string{"/users/{id}"}, "/users/42/orders", "/users/{id}/orders"},
		{"shorter path not matched", []string{"/users/{id}/orders"}, "/users/42", "/users/{id}"},
		{"param does not match empty segment", []string{"/users/{id}/x"}, "/users//x", "/users//x"},
		{"trailing slash on path", []string{"/users/{id}"}, "/users/42/", "/users/{id}"},
		{"trailing slash on pattern", []string{"/users/{id}/"}, "/users/42", "/users/{id}"},
		{"trailing slash both", []string{"/users/{id}/"}, "/users/42/", "/users/{id}"},
		{"query ignored", []string{"/users/{id}"}, "/users/42?x=/a/b", "/users/{id}"},
		{"literal only pattern", []string{"/health/live"}, "/health/live", "/health/live"},
		{"literal pattern is case sensitive", []string{"/Health"}, "/health/42", "/health/{id}"},
		{"two params", []string{"/a/{x}/b/{y}"}, "/a/1/b/2", "/a/{x}/b/{y}"},
		{"leading param", []string{"/{tenant}/users/{id}"}, "/acme/users/7", "/{tenant}/users/{id}"},
		{"unmatched segments get heuristics", []string{"/a/{x}"}, "/a/1/2/3", "/a/{id}/{id}/{id}"},
		{"root pattern harmless", []string{"/"}, "/", "/"},
		{"root pattern does not match others", []string{"/"}, "/42", "/{id}"},

		// Tail patterns.
		{"rest one", []string{"/files/{path...}"}, "/files/a", "/files/{path...}"},
		{"rest many", []string{"/files/{path...}"}, "/files/a/b/c/d.txt", "/files/{path...}"},
		{"rest with ids", []string{"/files/{path...}"}, "/files/42/9f1c1c1c1c1c1c1c1c", "/files/{path...}"},
		{"rest zero segments", []string{"/files/{path...}"}, "/files", "/files/{path...}"},
		{"rest zero with slash", []string{"/files/{path...}"}, "/files/", "/files/{path...}"},
		{"rest trailing slash", []string{"/files/{path...}"}, "/files/a/b/", "/files/{path...}"},
		{"rest wrong prefix", []string{"/files/{path...}"}, "/docs/a", "/docs/a"},
		{"rest after param", []string{"/u/{id}/f/{p...}"}, "/u/1/f/x/y", "/u/{id}/f/{p...}"},
		{"rest after param too short", []string{"/u/{id}/f/{p...}"}, "/u/1", "/u/{id}"},
		{"rest at root", []string{"/{all...}"}, "/anything/at/all", "/{all...}"},
		{"rest at root does not catch root", []string{"/{all...}"}, "/", "/"},
		{"rest keeps empty middle", []string{"/files/{path...}"}, "/files/a//b", "/files/{path...}"},

		// Percent escapes.
		{"literal escape raw", []string{"/a%20b/{id}"}, "/a%20b/1", "/a%20b/{id}"},
		{"literal escape matches decoded path", []string{"/a%20b/{id}"}, "/a b/1", "/a%20b/{id}"},
		{"decoded literal matches escaped path", []string{"/a b/{id}"}, "/a%20b/1", "/a b/{id}"},
		{"escape hex case differs", []string{"/caf%C3%A9/{id}"}, "/caf%c3%a9/1", "/caf%C3%A9/{id}"},
		{"escape mismatch", []string{"/a%20b/{id}"}, "/a%20c/1", "/a%20c/{id}"},
		{"invalid escape in path no match", []string{"/a b/{id}"}, "/a%2/1", "/a%2/{id}"},
		{"invalid escape in pattern literal kept", []string{"/a%2/{id}"}, "/a%2/1", "/a%2/{id}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := mustNew(t, tc.patterns...)
			if got := n.Template(tc.path); got != tc.want {
				t.Errorf("Template(%q) with %v = %q, want %q", tc.path, tc.patterns, got, tc.want)
			}
		})
	}
}

func TestPatternPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		path     string
		want     string
	}{
		{
			"more literals win over fewer",
			[]string{"/users/{id}/orders/{oid}", "/users/{id}/{what}/{x}"},
			"/users/1/orders/2", "/users/{id}/orders/{oid}",
		},
		{
			"more literals win regardless of declaration order",
			[]string{"/users/{id}/{what}/{x}", "/users/{id}/orders/{oid}"},
			"/users/1/orders/2", "/users/{id}/orders/{oid}",
		},
		{
			"literal beats param at the same position",
			[]string{"/users/{id}", "/users/me"},
			"/users/me", "/users/me",
		},
		{
			"literal falls back to param",
			[]string{"/users/{id}", "/users/me"},
			"/users/42", "/users/{id}",
		},
		{
			"literals beat a longer tail pattern",
			[]string{"/{a}/{b}/{rest...}", "/x/{b}"},
			"/x/y", "/x/{b}",
		},
		{
			"literal count beats length of a tail pattern",
			[]string{"/{x}/{y}/{rest...}", "/a/b"},
			"/a/b", "/a/b",
		},
		{
			"more literals win at equal length",
			[]string{"/a/{b}/{c}", "/{a}/b/c"},
			"/a/b/c", "/{a}/b/c",
		},
		{
			"more literals win at equal length (reversed)",
			[]string{"/{a}/b/c", "/a/{b}/{c}"},
			"/a/b/c", "/{a}/b/c",
		},
		{
			"equal literals longer wins (tail pattern)",
			[]string{"/files/{path...}", "/files/{a}/{b}"},
			"/files/x/y", "/files/{a}/{b}",
		},
		{
			"equal literals equal length fixed beats tail",
			[]string{"/files/{path...}", "/files/{id}"},
			"/files/x", "/files/{id}",
		},
		{
			"equal literals equal length fixed beats tail (reversed)",
			[]string{"/files/{id}", "/files/{path...}"},
			"/files/x", "/files/{id}",
		},
		{
			"tail catches what fixed does not",
			[]string{"/files/{id}", "/files/{path...}"},
			"/files/x/y/z", "/files/{path...}",
		},
		{
			"literal beats tail",
			[]string{"/api/{rest...}", "/api/health"},
			"/api/health", "/api/health",
		},
		{
			"longer literal-led tail beats shorter",
			[]string{"/api/{rest...}", "/api/v1/{rest...}"},
			"/api/v1/users/42", "/api/v1/{rest...}",
		},
		{
			"longer tail falls back to shorter",
			[]string{"/api/{rest...}", "/api/v1/{rest...}"},
			"/api/v2/users/42", "/api/{rest...}",
		},
		{
			"tie goes to first declared",
			[]string{"/x/{a}/y", "/x/{b}/y"},
			"/x/1/y", "/x/{a}/y",
		},
		{
			"tie goes to first declared (reversed)",
			[]string{"/x/{b}/y", "/x/{a}/y"},
			"/x/1/y", "/x/{b}/y",
		},
		{
			"tie across positions goes to first declared",
			[]string{"/a/{x}/c", "/a/b/{y}"},
			"/a/b/c", "/a/{x}/c",
		},
		{
			"tie across positions goes to first declared (reversed)",
			[]string{"/a/b/{y}", "/a/{x}/c"},
			"/a/b/c", "/a/b/{y}",
		},
		{
			"duplicates collapse",
			[]string{"/u/{id}", "/u/{id}", "/u/{id}/"},
			"/u/1", "/u/{id}",
		},
		{
			"root tail is lowest priority",
			[]string{"/{all...}", "/users/{id}", "/{a}/{b}"},
			"/users/1", "/users/{id}",
		},
		{
			"root tail catch-all",
			[]string{"/{all...}", "/users/{id}", "/{a}/{b}"},
			"/foo/bar", "/{a}/{b}",
		},
		{
			"root tail catch-all long",
			[]string{"/{all...}", "/users/{id}", "/{a}/{b}"},
			"/foo/bar/baz", "/{all...}",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := mustNew(t, tc.patterns...)
			if got := n.Template(tc.path); got != tc.want {
				t.Errorf("Template(%q) with %v = %q, want %q", tc.path, tc.patterns, got, tc.want)
			}
		})
	}
}

func TestPatternsOrder(t *testing.T) {
	n := mustNew(t,
		"/{a}/{b}",
		"/files/{path...}",
		"/users/{id}/orders/{oid}",
		"/users/me",
		"/files/{id}",
		"/users/{id}/",
		"/users/me/", // duplicate of /users/me
	)
	want := []string{
		"/users/{id}/orders/{oid}", // 2 literals, 4 segments
		"/users/me",                // 2 literals, 2 segments
		"/files/{id}",              // 1 literal, 2 segments, fixed
		"/users/{id}",              // 1 literal, 2 segments, fixed, declared later
		"/files/{path...}",         // 1 literal, 2 segments, tail
		"/{a}/{b}",                 // 0 literals
	}
	got := n.Patterns()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Patterns() =\n %v\nwant\n %v", got, want)
	}
	// Patterns returns a copy.
	got[0] = "mutated"
	if n.Patterns()[0] != want[0] {
		t.Error("Patterns() exposes internal state")
	}
}

func TestPatternDoesNotAffectInputOrder(t *testing.T) {
	in := []string{"/{a}/{b}", "/users/{id}"}
	_ = mustNew(t, in...)
	if in[0] != "/{a}/{b}" || in[1] != "/users/{id}" {
		t.Errorf("New reordered its argument: %v", in)
	}
}

func TestManyPatterns(t *testing.T) {
	// 300 distinct resources, each with a literal and a nested pattern.
	var patterns []string
	for i := range 300 {
		patterns = append(patterns, fmt.Sprintf("/r%d/{id}", i), fmt.Sprintf("/r%d/{id}/sub/{sid}", i))
	}
	n := mustNew(t, patterns...)
	for _, i := range []int{0, 7, 150, 299} {
		if got, want := n.Template(fmt.Sprintf("/r%d/abc", i)), fmt.Sprintf("/r%d/{id}", i); got != want {
			t.Errorf("got %q want %q", got, want)
		}
		if got, want := n.Template(fmt.Sprintf("/r%d/abc/sub/9", i)), fmt.Sprintf("/r%d/{id}/sub/{sid}", i); got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
	if got := n.Template("/r300/abc"); got != "/r300/abc" {
		t.Errorf("unmatched = %q", got)
	}
}

func TestDeepPathBeyondStackBuffers(t *testing.T) {
	// More than 16 segments and an output longer than the 192-byte scratch
	// buffer: both fall back to the heap.
	var in, want strings.Builder
	for i := range 60 {
		fmt.Fprintf(&in, "/segment%d/%d", i, i)
		fmt.Fprintf(&want, "/segment%d/{id}", i)
	}
	if got := Default.Template(in.String()); got != want.String() {
		t.Errorf("long path:\n got %q\nwant %q", got, want.String())
	}
	n := mustNew(t, "/segment0/{a}/{rest...}")
	if got := n.Template(in.String()); got != "/segment0/{a}/{rest...}" {
		t.Errorf("long path with pattern = %q", got)
	}
	n = mustNew(t, "/nomatch/{x}")
	if got := n.Template(in.String()); got != want.String() {
		t.Errorf("long path with non-matching pattern = %q", got)
	}
	long := Default.Endpoint("get", in.String())
	if long != "GET "+want.String() {
		t.Errorf("long endpoint = %q", long)
	}
}

func TestSubstringResultsAreIndependentOfBuffers(t *testing.T) {
	// A returned string must stay valid after more calls (no aliasing of the
	// scratch buffer).
	var results []string
	var wants []string
	for i := range 200 {
		p := fmt.Sprintf("/users/%d/orders/%d", i, i*7)
		results = append(results, Default.Template(p))
		wants = append(wants, "/users/{id}/orders/{id}")
		results = append(results, Default.Endpoint("get", fmt.Sprintf("/x/%d", i)))
		wants = append(wants, "GET /x/{id}")
	}
	for i := range results {
		if results[i] != wants[i] {
			t.Fatalf("result %d = %q, want %q", i, results[i], wants[i])
		}
	}
}

func TestConcurrent(t *testing.T) {
	n := mustNew(t, "/users/{id}", "/files/{path...}", "/orders/{oid}/items/{iid}")
	cases := []struct{ method, path, wantT, wantE string }{
		{"get", "/users/abc", "/users/{id}", "GET /users/{id}"},
		{"put", "/files/a/b/c", "/files/{path...}", "PUT /files/{path...}"},
		{"get", "/orders/1/items/2", "/orders/{oid}/items/{iid}", "GET /orders/{oid}/items/{iid}"},
		{"post", "/x/123e4567-e89b-12d3-a456-426614174000/y/9", "/x/{uuid}/y/{id}", "POST /x/{uuid}/y/{id}"},
		{"get", "/plain/literal", "/plain/literal", "GET /plain/literal"},
		{"get", "/m/jane@example.com/", "/m/{email}", "GET /m/{email}"},
	}
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 2000 {
				c := cases[(g+i)%len(cases)]
				if got := n.Template(c.path); got != c.wantT {
					errs <- fmt.Sprintf("Template(%q) = %q, want %q", c.path, got, c.wantT)
					return
				}
				if got := n.Endpoint(c.method, c.path); got != c.wantE {
					errs <- fmt.Sprintf("Endpoint(%q, %q) = %q, want %q", c.method, c.path, got, c.wantE)
					return
				}
				// Default is shared too.
				if got := Default.Template(c.path); got == "" {
					errs <- "empty template"
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func TestAllocations(t *testing.T) {
	n := mustNew(t, "/users/{id}", "/files/{path...}")
	tests := []struct {
		name string
		fn   func()
		max  float64
	}{
		{"literal path", func() { _ = Default.Template("/api/v1/users/me/orders") }, 0},
		{"trailing slash literal", func() { _ = Default.Template("/api/v1/users/") }, 0},
		{"query literal", func() { _ = Default.Template("/api/v1/users?x=1") }, 0},
		{"root", func() { _ = Default.Template("/") }, 0},
		{"one id", func() { _ = Default.Template("/api/v1/users/42/orders") }, 1},
		{"many ids", func() { _ = Default.Template("/users/42/orders/9/items/12345/x/678") }, 1},
		{"uuid", func() { _ = Default.Template("/o/123e4567-e89b-12d3-a456-426614174000") }, 1},
		{"pattern hit", func() { _ = n.Template("/users/abc") }, 0},
		{"pattern tail hit", func() { _ = n.Template("/files/a/b/c/d/e") }, 0},
		{"pattern miss literal", func() { _ = n.Template("/other/path") }, 0},
		{"endpoint literal", func() { _ = Default.Endpoint("GET", "/api/v1/users") }, 1},
		{"endpoint lowercase method", func() { _ = Default.Endpoint("get", "/api/v1/users/42") }, 1},
		{"endpoint pattern hit", func() { _ = n.Endpoint("GET", "/users/abc") }, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := testing.AllocsPerRun(200, tc.fn); got > tc.max {
				t.Errorf("allocs per run = %v, want <= %v", got, tc.max)
			}
		})
	}
}

func FuzzTemplate(f *testing.F) {
	for _, s := range []string{
		"", "/", "*", "/users/42", "/a//b", "//", "/users/42/?x=1", "/a%20b", "/o/123e4567-e89b-12d3-a456-426614174000",
		"/x/jane@example.com", "/d/2024-02-29", "/files/a/b/c", "/%", "/\x00/\xff", "a/b",
		"/u/jane%40acme.io", "/n/%31%32%33", "/h/h_3f9a1c0b7d2e4a51", "/h/h%5F3f9a1c0b7d2e4a51", "/n/%zz/%2531/%",
	} {
		f.Add(s)
	}
	n, err := New([]string{"/files/{path...}", "/users/{id}/orders/{oid}", "/x/{a}/y", "/a b/{c}"})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, path string) {
		for _, norm := range []*Normalizer{Default, n} {
			got := norm.Template(path)
			if got == "" {
				t.Fatalf("Template(%q) is empty", path)
			}
			if got != "*" && got[0] != '/' {
				t.Fatalf("Template(%q) = %q lacks a leading slash", path, got)
			}
			if len(got) > 1 && got[len(got)-1] == '/' {
				t.Fatalf("Template(%q) = %q has a trailing slash", path, got)
			}
			if again := norm.Template(got); again != got {
				t.Fatalf("Template(%q) = %q is not a fixed point (second pass %q)", path, got, again)
			}
			if e := norm.Endpoint("get", path); e != "GET "+got {
				t.Fatalf("Endpoint(%q) = %q, want %q", path, e, "GET "+got)
			}
		}
		checkSegmentsEitherKeptOrPlaceholder(t, path, Default.Template(path))
	})
}

// checkSegmentsEitherKeptOrPlaceholder asserts that, without user patterns,
// every segment of the template is a placeholder or the input segment exactly
// as given: decoded text never leaks into a template.
func checkSegmentsEitherKeptOrPlaceholder(t *testing.T, path, got string) {
	t.Helper()
	if got == "/" || got == "*" || strings.IndexByte(path, '?') >= 0 {
		return
	}
	body := func(s string) []string {
		return strings.Split(strings.TrimRight(strings.TrimPrefix(s, "/"), "/"), "/")
	}
	in, out := body(path), body(got)
	if len(in) != len(out) {
		t.Fatalf("Template(%q) = %q has %d segments, input has %d", path, got, len(out), len(in))
	}
	for i := range in {
		switch out[i] {
		case in[i], tokID, tokUUID, tokHex, tokToken, tokEmail, tokDate:
		default:
			t.Fatalf("Template(%q) = %q: segment %d is %q, neither the input segment %q nor a placeholder", path, got, i, out[i], in[i])
		}
	}
}

func mustNew(t testing.TB, patterns ...string) *Normalizer {
	t.Helper()
	n, err := New(patterns)
	if err != nil {
		t.Fatalf("New(%v): %v", patterns, err)
	}
	return n
}
