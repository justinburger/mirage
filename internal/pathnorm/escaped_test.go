package pathnorm

import (
	"strings"
	"testing"
)

// hashA and friends are Mirage anonymization hash tokens: "h_" plus exactly
// 16 lowercase hex characters.
const (
	hashA = "h_3f9a1c0b7d2e4a51"
	hashB = "h_00000000000000ff"
	hashC = "h_0123456789012345" // digits only
	hashD = "h_abcdefabcdefabcd" // letters only
)

func TestTemplateHashTokens(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		// Hash tokens are {id}.
		{"hash", "/users/" + hashA, "/users/{id}"},
		{"hash first segment", "/" + hashA + "/orders", "/{id}/orders"},
		{"hash only", "/" + hashA, "/{id}"},
		{"hash digits only", "/users/" + hashC, "/users/{id}"},
		{"hash letters only", "/users/" + hashD, "/users/{id}"},
		{"hash mostly zero", "/users/" + hashB, "/users/{id}"},
		{"hash several", "/users/" + hashA + "/orders/" + hashB + "/items/" + hashC, "/users/{id}/orders/{id}/items/{id}"},
		{"hash then plain id", "/users/" + hashA + "/orders/9001", "/users/{id}/orders/{id}"},
		{"hash trailing slash", "/users/" + hashA + "/", "/users/{id}"},
		{"hash with query", "/users/" + hashA + "?x=1", "/users/{id}"},
		{"hash beside uuid", "/o/" + hashA + "/" + "123e4567-e89b-12d3-a456-426614174000", "/o/{id}/{uuid}"},
		{"hash escaped underscore", "/users/h%5F3f9a1c0b7d2e4a51", "/users/{id}"},
		{"hash escaped underscore lowercase hex", "/users/h%5f3f9a1c0b7d2e4a51", "/users/{id}"},
		{"hash escaped prefix", "/users/%68_3f9a1c0b7d2e4a51", "/users/{id}"},
		{"hash escaped digit", "/users/h_%33f9a1c0b7d2e4a51", "/users/{id}"},
		{"hash escaped hex letter", "/users/h_3f9a1c0b7d2e4a%35%31", "/users/{id}"},

		// Near misses stay what they were before.
		{"hash uppercase hex", "/users/h_3F9A1C0B7D2E4A51", "/users/h_3F9A1C0B7D2E4A51"},
		{"hash one uppercase hex", "/users/h_3f9a1c0b7d2e4a5F", "/users/h_3f9a1c0b7d2e4a5F"},
		{"hash uppercase prefix", "/users/H_3f9a1c0b7d2e4a51", "/users/H_3f9a1c0b7d2e4a51"},
		{"hash other prefix", "/users/x_3f9a1c0b7d2e4a51", "/users/x_3f9a1c0b7d2e4a51"},
		{"hash dash instead of underscore", "/users/h-3f9a1c0b7d2e4a51", "/users/h-3f9a1c0b7d2e4a51"},
		{"hash no separator", "/users/h3f9a1c0b7d2e4a512", "/users/h3f9a1c0b7d2e4a512"},
		{"hash 15 hex", "/users/h_3f9a1c0b7d2e4a5", "/users/h_3f9a1c0b7d2e4a5"},
		{"hash 17 hex", "/users/h_3f9a1c0b7d2e4a512", "/users/h_3f9a1c0b7d2e4a512"},
		{"hash non hex letter", "/users/h_3f9a1c0b7d2e4a5g", "/users/h_3f9a1c0b7d2e4a5g"},
		{"hash non hex byte, 18 bytes long", "/users/h_3f9a1c0b7d2e4aé", "/users/h_3f9a1c0b7d2e4aé"},
		{"hash trailing text", "/users/h_3f9a1c0b7d2e4a51x", "/users/h_3f9a1c0b7d2e4a51x"},
		{"hash leading text", "/users/xh_3f9a1c0b7d2e4a51", "/users/xh_3f9a1c0b7d2e4a51"},
		{"hash prefix only", "/users/h_", "/users/h_"},
		{"hash 20 chars falls to the token rule", "/users/h_3f9a1c0b7d2e4a51ab", "/users/{token}"},
		{"hash suffix of a longer prefix", "/users/hh_3f9a1c0b7d2e4a51", "/users/hh_3f9a1c0b7d2e4a51"},
		{"hash inside email local part", "/users/" + hashA + "@example.com", "/users/{email}"},
		{"hash escaped slash", "/users/h_3f9a1c0b7d2e4a%2F1", "/users/h_3f9a1c0b7d2e4a%2F1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Default.Template(tc.path); got != tc.want {
				t.Errorf("Template(%q) = %q, want %q", tc.path, got, tc.want)
			}
			if again := Default.Template(tc.want); again != tc.want {
				t.Errorf("Template(%q) = %q, not a fixed point", tc.want, again)
			}
		})
	}
}

// TestRawAndHashedFormsShareATemplate checks the reason hash tokens are {id}:
// anonymized traffic must group into the same endpoints as the raw traffic.
func TestRawAndHashedFormsShareATemplate(t *testing.T) {
	n := mustNew(t, "/files/{path...}", "/teams/{team}/members/{member}")
	pairs := []struct {
		name        string
		raw, hashed string
		want        string
	}{
		{"one id", "/users/42", "/users/" + hashA, "/users/{id}"},
		{"nested ids", "/api/v1/users/42/orders/9001/items/7",
			"/api/v1/users/" + hashA + "/orders/" + hashB + "/items/" + hashC,
			"/api/v1/users/{id}/orders/{id}/items/{id}"},
		{"mixed raw and hashed", "/api/v1/users/42/orders/9001",
			"/api/v1/users/" + hashA + "/orders/9001", "/api/v1/users/{id}/orders/{id}"},
		{"id at the root", "/42", "/" + hashA, "/{id}"},
		{"trailing slash", "/users/42/", "/users/" + hashA + "/", "/users/{id}"},
		{"query", "/users/42?expand=orders", "/users/" + hashA + "?expand=orders", "/users/{id}"},
		{"escaped id", "/users/%34%32", "/users/h%5F3f9a1c0b7d2e4a51", "/users/{id}"},
		{"literals kept", "/users/42/orders/by%20date", "/users/" + hashA + "/orders/by%20date", "/users/{id}/orders/by%20date"},
	}
	for _, tc := range pairs {
		t.Run(tc.name, func(t *testing.T) {
			for _, norm := range []*Normalizer{Default, n} {
				raw, hashed := norm.Template(tc.raw), norm.Template(tc.hashed)
				if raw != tc.want || hashed != tc.want {
					t.Errorf("Template: raw %q → %q, hashed %q → %q, want both %q", tc.raw, raw, tc.hashed, hashed, tc.want)
				}
				rawEp, hashedEp := norm.Endpoint("get", tc.raw), norm.Endpoint("get", tc.hashed)
				if rawEp != hashedEp || rawEp != "GET "+tc.want {
					t.Errorf("Endpoint: raw %q, hashed %q, want both %q", rawEp, hashedEp, "GET "+tc.want)
				}
			}
		})
	}

	// Under a user pattern both forms report the pattern.
	for _, p := range []string{"/files/42/a", "/files/" + hashA + "/a"} {
		if got := n.Template(p); got != "/files/{path...}" {
			t.Errorf("Template(%q) = %q, want the pattern", p, got)
		}
	}
	for _, p := range []string{"/teams/7/members/42", "/teams/" + hashA + "/members/" + hashB} {
		if got := n.Template(p); got != "/teams/{team}/members/{member}" {
			t.Errorf("Template(%q) = %q, want the pattern", p, got)
		}
	}
}

func TestTemplateEscapedPath(t *testing.T) {
	const (
		uuidLower = "123e4567-e89b-12d3-a456-426614174000"
		uuidEsc   = "123e4567%2De89b%2D12d3%2Da456%2D426614174000"
	)
	tests := []struct {
		name string
		path string
		want string
	}{
		// Classified on the decoded segment.
		{"email escaped at", "/users/jane%40acme.io", "/users/{email}"},
		{"email escaped at mixed case hex", "/users/jane%40Acme.io", "/users/{email}"},
		{"email escaped letters", "/users/%6Aane%40acme.io", "/users/{email}"},
		{"email escaped dot", "/users/jane%2Edoe%40acme%2Eio", "/users/{email}"},
		{"email escaped plus", "/users/jane%2Btag%40acme.io", "/users/{email}"},
		{"email literal plus stays a plus", "/users/jane+tag%40acme.io", "/users/{email}"},
		{"email literal at and escaped dot", "/users/jane@acme%2Eio", "/users/{email}"},
		{"email escaped utf8 local part", "/users/j%C3%BCrgen%40acme.io", "/users/{email}"},
		{"email escaped utf8 domain", "/users/jane%40b%C3%BCcher.de", "/users/{email}"},
		{"email in path", "/orgs/7/users/jane%40acme.io/prefs", "/orgs/{id}/users/{email}/prefs"},
		{"id escaped digits", "/n/%31%32%33", "/n/{id}"},
		{"id one escaped digit", "/n/12%33", "/n/{id}"},
		{"escaped colon is not a digit", "/n/%3a", "/n/%3a"},
		{"id leading zeros escaped", "/n/%30%30%37", "/n/{id}"},
		{"uuid escaped dashes", "/o/" + uuidEsc, "/o/{uuid}"},
		{"uuid escaped letter", "/o/%31" + uuidLower[1:], "/o/{uuid}"},
		{"date escaped dashes", "/r/2024%2D01%2D31", "/r/{date}"},
		{"date escaped digits", "/r/%32024-01-31", "/r/{date}"},
		{"hex escaped letter", "/h/0123456789abcde%66", "/h/{hex}"},
		{"hex too short escaped", "/h/0123456789abcd%65", "/h/0123456789abcd%65"},
		{"token escaped letter", "/s/%61bcdefghij1234567890", "/s/{token}"},
		{"token escaped underscore", "/s/abcdefghij%5F1234567890", "/s/{token}"},
		{"token escaped plus is not a token", "/s/abcdefghij%2B1234567890", "/s/abcdefghij%2B1234567890"},
		{"mixed", "/users/jane%40acme.io/orders/%34%32/files/a%20b.txt", "/users/{email}/orders/{id}/files/a%20b.txt"},

		// Literal segments keep their escaped form.
		{"literal space", "/a%20b/42", "/a%20b/{id}"},
		{"literal slash", "/a/c%2Fd/42", "/a/c%2Fd/{id}"},
		{"literal lowercase hex kept", "/caf%c3%a9/42", "/caf%c3%a9/{id}"},
		{"literal uppercase hex kept", "/caf%C3%A9/42", "/caf%C3%A9/{id}"},
		{"literal braces", "/q/%7Bid%7D", "/q/%7Bid%7D"},
		{"literal question mark", "/q/%3F42", "/q/%3F42"},
		{"literal question mark after id", "/q/42%3Fx", "/q/42%3Fx"},
		{"literal hash sign", "/q/%2342", "/q/%2342"},
		{"literal euro", "/q/%E2%82%AC1", "/q/%E2%82%AC1"},
		{"literal invalid utf8", "/q/%ff%fe", "/q/%ff%fe"},
		{"literal nul", "/q/%00", "/q/%00"},
		{"literal escaped plus", "/q/a%2Bb", "/q/a%2Bb"},
		{"literal plus", "/q/a+b", "/q/a+b"},
		{"literal escaped slash between digits", "/q/1%2F2", "/q/1%2F2"},
		{"literal escaped dot dot", "/q/%2E%2E/42", "/q/%2E%2E/{id}"},
		{"escaped email bad domain", "/users/jane%40localhost", "/users/jane%40localhost"},
		{"escaped email space in local part", "/users/jane%20doe%40acme.io", "/users/jane%20doe%40acme.io"},
		{"escaped email control char", "/users/ja%09ne%40acme.io", "/users/ja%09ne%40acme.io"},
		{"escaped email two ats", "/users/a%40b%40acme.io", "/users/a%40b%40acme.io"},
		{"escaped email no local part", "/users/%40acme.io", "/users/%40acme.io"},
		{"escaped email no domain", "/users/jane%40", "/users/jane%40"},
		{"escaped email underscore domain", "/users/jane%40exa%5Fmple.com", "/users/jane%40exa%5Fmple.com"},

		// Decoded once only.
		{"double escaped digit", "/n/%2531", "/n/%2531"},
		{"double escaped at", "/users/jane%2540acme.io", "/users/jane%2540acme.io"},
		{"percent literal", "/n/%25", "/n/%25"},
		{"percent then digits", "/n/%2542", "/n/%2542"},

		// Undecodable segments are classified as they are.
		{"bad escape letters", "/n/%zz", "/n/%zz"},
		{"bad escape short", "/n/%4", "/n/%4"},
		{"bad escape bare percent", "/n/%", "/n/%"},
		{"bad escape after digits", "/n/12%", "/n/12%"},
		{"bad escape with good escape", "/n/%31%zz", "/n/%31%zz"},
		{"bad escape good escape first", "/n/%31%3", "/n/%31%3"},
		{"bad escape hash lookalike", "/n/h_3f9a1c0b7d2e4a5%", "/n/h_3f9a1c0b7d2e4a5%"},
		{"bad escape email lookalike", "/users/jane@acme.io%", "/users/jane@acme.io%"},
		{"bad escape keeps a literal at", "/users/%zz@acme.io", "/users/{email}"},
		{"bad escape rest of path still classified", "/a/%zz/42/jane%40acme.io", "/a/%zz/{id}/{email}"},
		{"bad escape does not decode its neighbours", "/a/%31%zz/%31", "/a/%31%zz/{id}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Default.Template(tc.path); got != tc.want {
				t.Errorf("Template(%q) = %q, want %q", tc.path, got, tc.want)
			}
			if again := Default.Template(tc.want); again != tc.want {
				t.Errorf("Template(%q) = %q, not a fixed point", tc.want, again)
			}
			wantEP := "GET " + tc.want
			if got := Default.Endpoint("get", tc.path); got != wantEP {
				t.Errorf("Endpoint(get, %q) = %q, want %q", tc.path, got, wantEP)
			}
		})
	}
}

// TestEscapedAndPlainFormsAgree: a segment and its percent-encoded spelling
// (every byte escaped) get the same placeholder, or both stay literal; only a
// literal's own spelling differs.
func TestEscapedAndPlainFormsAgree(t *testing.T) {
	segs := []string{
		"42", "007", "0123456789abcdef", "123e4567-e89b-12d3-a456-426614174000",
		"abcdefghij1234567890", "jane@acme.io", "jane+tag@acme.io", "2024-01-31",
		hashA, "users", "me", "a b", "h_3F9A1C0B7D2E4A51", "jane@localhost",
		"2023-02-29", "héllo",
	}
	for _, seg := range segs {
		var enc strings.Builder
		for i := 0; i < len(seg); i++ {
			const hexd = "0123456789ABCDEF"
			enc.WriteByte('%')
			enc.WriteByte(hexd[seg[i]>>4])
			enc.WriteByte(hexd[seg[i]&15])
		}
		plain, escaped := classify(seg), classify(enc.String())
		if plain != escaped {
			t.Errorf("classify(%q) = %q but classify(%q) = %q", seg, plain, enc.String(), escaped)
		}
		// The template keeps whichever spelling the caller used.
		wantPlain, wantEnc := "/x/"+seg, "/x/"+enc.String()
		if plain != "" {
			wantPlain, wantEnc = "/x/"+plain, "/x/"+plain
		}
		if got := Default.Template("/x/" + seg); got != wantPlain {
			t.Errorf("Template(/x/%s) = %q, want %q", seg, got, wantPlain)
		}
		if got := Default.Template("/x/" + enc.String()); got != wantEnc {
			t.Errorf("Template(/x/%s) = %q, want %q", enc.String(), got, wantEnc)
		}
	}
}

func TestClassifyAllocations(t *testing.T) {
	tests := []struct {
		name string
		seg  string
		max  float64
	}{
		// Segments without a '%' are never decoded or copied.
		{"literal", "users", 0},
		{"id", "42", 0},
		{"hash token", hashA, 0},
		{"hash lookalike", "h_3F9A1C0B7D2E4A51", 0},
		{"uuid", "123e4567-e89b-12d3-a456-426614174000", 0},
		{"token", "abcdefghij1234567890", 0},
		{"email", "jane@acme.io", 0},
		{"date", "2024-01-31", 0},
		// An escape costs the one decoded string.
		{"escaped email", "jane%40acme.io", 1},
		{"escaped id", "%31%32%33", 1},
		{"escaped literal", "a%20b", 1},
		{"undecodable", "%zz", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := testing.AllocsPerRun(200, func() { _ = classify(tc.seg) }); got > tc.max {
				t.Errorf("classify(%q): allocs per run = %v, want <= %v", tc.seg, got, tc.max)
			}
		})
	}

	// Whole paths: no '%' means no extra allocation over the result string.
	paths := []struct {
		name string
		path string
		max  float64
	}{
		{"hash token path", "/api/v1/users/" + hashA + "/orders/" + hashB, 1},
		{"literal path", "/api/v1/users/me/orders", 0},
		{"escaped email path", "/api/v1/users/jane.doe%40acme.io/prefs", 2},
	}
	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			if got := testing.AllocsPerRun(200, func() { _ = Default.Template(tc.path) }); got > tc.max {
				t.Errorf("Template(%q): allocs per run = %v, want <= %v", tc.path, got, tc.max)
			}
		})
	}
}
