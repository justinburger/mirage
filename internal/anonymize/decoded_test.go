package anonymize

import (
	"net/http"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

// A body that was decoded (its coding is in ContentEncoding, the header is
// gone) is anonymized exactly like the same bytes sent without a coding, even
// when it is not valid UTF-8: text in a legacy charset, or with a NUL byte.
// It used to be taken for still compressed bytes and kept as it was,
// personal data included.
func TestDecodedBodiesAreTreatedLikeUncodedOnes(t *testing.T) {
	e := builtinEngine(t)
	run := func(ct, body, ce string) string {
		t.Helper()
		ex := &model.Exchange{Response: model.Response{
			Status: 200, Header: http.Header{}, Body: []byte(body),
			BodySize: int64(len(body)), ContentEncoding: ce,
		}}
		if ct != "" {
			ex.Response.Header.Set("Content-Type", ct)
		}
		if err := e.Exchange(ex); err != nil {
			t.Fatalf("%q (%s): %v", body, ce, err)
		}
		return string(ex.Response.Body)
	}
	for _, tc := range []struct {
		name, ct, body string
		gone           []string // must not survive anonymization
	}{
		{"ISO-8859-1 JSON", "application/json; charset=iso-8859-1",
			`{"name":"Jos` + "\xe9" + `","email":"alice.secret@example.com","password":"hunter2secretpw"}`,
			[]string{"alice.secret", "hunter2secretpw"}},
		{"ISO-8859-1 HTML", "text/html; charset=iso-8859-1",
			"<p>Jos\xe9 &lt;alice.secret@example.com&gt;</p>", []string{"alice.secret"}},
		// Not valid JSON (a raw control character), so the text rules apply.
		{"JSON with a NUL byte", "application/json",
			"{\"note\":\"a\x00b\",\"email\":\"alice.secret@example.com\"}", []string{"alice.secret"}},
		// Without a type, bytes that are not UTF-8 cannot be told from binary.
		{"no type, ISO-8859-1 text", "", "Jos\xe9 alice.secret@example.com", nil},
	} {
		plain := run(tc.ct, tc.body, "")
		for _, g := range tc.gone {
			if strings.Contains(plain, g) {
				t.Errorf("%s: %q survived even without a coding: %q", tc.name, g, plain)
			}
		}
		for _, ce := range []string{"gzip", "deflate", "gzip, gzip"} {
			if got := run(tc.ct, tc.body, ce); got != plain {
				t.Errorf("%s (%s): %q, but without a coding %q", tc.name, ce, got, plain)
			}
		}
	}
}

// A body still under its Content-Encoding header, or with a gzip or zstd
// magic number, is opaque: the engine cannot read it (callers decode or drop
// such bodies before it runs).
func TestStillCompressedBodiesAreOpaque(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.Header
		ce   string
		body string
		want bool
	}{
		{"header survives", http.Header{"Content-Encoding": {"br"}}, "", "anything", true},
		{"gzip magic", http.Header{}, "gzip", "\x1f\x8b\x08rest", true},
		{"zstd magic", http.Header{}, "zstd", "\x28\xb5\x2f\xfdrest", true},
		{"decoded ISO-8859-1", http.Header{}, "deflate", "Jos\xe9", false},
		{"decoded text that starts like a zlib header", http.Header{}, "deflate", "x^2 + y^2", false},
		{"never encoded", http.Header{}, "", "\xff\xfe", false},
	} {
		if got := stillCompressed(tc.h, tc.ce, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: stillCompressed = %v, want %v", tc.name, got, tc.want)
		}
	}
}
