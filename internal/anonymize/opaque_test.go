package anonymize

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

// fillProfile is testProfile with OpaqueBodies "fill".
func fillProfile(rules ...model.AnonymizationRule) model.AnonymizationProfile {
	p := testProfile(rules...)
	p.OpaqueBodies = model.OpaqueFill
	return p
}

func fillEngine(t testing.TB, rules ...model.AnonymizationRule) *Engine {
	return mustEngine(t, fillProfile(rules...))
}

// patterned returns n bytes that are recognisably not random, so a test can
// tell whether any of them survive.
func patterned(n int, tag string) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		b.WriteString(tag)
		b.WriteString(strconv.Itoa(i))
		b.WriteByte(0)
		b.WriteByte(0xff)
	}
	return b.Bytes()[:n]
}

// opaqueBodies are bodies the engine cannot inspect, each in a way that is
// caught by a different rule.
func opaqueBodies(t testing.TB) []struct {
	name            string
	ct              string
	header          http.Header
	contentEncoding string
	body            []byte
} {
	gz := gzipBytes(t, `{"email":"jane@acme.io","pad":"`+strings.Repeat("x", 200)+`"}`)
	return []struct {
		name            string
		ct              string
		header          http.Header
		contentEncoding string
		body            []byte
	}{
		{"image", "image/png", nil, "", append([]byte("\x89PNG\r\n\x1a\n"), patterned(500, "px")...)},
		{"pdf", "application/pdf", nil, "", append([]byte("%PDF-1.4\n"), patterned(300, "obj")...)},
		{"protobuf with ascii bytes", "application/protobuf", nil, "", []byte("plain looking protobuf payload, still framed by lengths")},
		{"font", "font/woff2", nil, "", patterned(256, "wOF2")},
		{"header gzip", "application/json", http.Header{"Content-Encoding": {"gzip"}}, "", gz},
		{"header br on text", "text/plain", http.Header{"Content-Encoding": {"br"}}, "", []byte("looks like text but jane@acme.io is compressed")},
		{"decoded-name gzip bytes", "application/json", nil, "gzip", gz},
		{"unparsable binary, no type", "", nil, "", patterned(300, "bin")},
		{"binary octet-stream", "application/octet-stream", nil, "", append([]byte{0, 1, 2, 3}, patterned(200, "oc")...)},
		{"binary unknown type", "application/x-custom", nil, "", []byte{0xff, 0xfe, 0xfd, 0, 'j', 'a', 'n', 'e'}},
	}
}

func exchangeWithBodies(ct string, h http.Header, contentEncoding string, body []byte) *model.Exchange {
	ex := newExchange()
	ex.Request.Method = "POST"
	for _, side := range []http.Header{ex.Request.Header, ex.Response.Header} {
		if ct != "" {
			side.Set("Content-Type", ct)
		}
		for k, v := range h {
			side[k] = append([]string(nil), v...)
		}
		side.Set("Content-Length", strconv.Itoa(len(body)))
	}
	ex.Request.Body, ex.Request.BodySize, ex.Request.ContentEncoding = bytes.Clone(body), int64(len(body)), contentEncoding
	ex.Response.Body, ex.Response.BodySize, ex.Response.ContentEncoding = bytes.Clone(body), int64(len(body)), contentEncoding
	return ex
}

func TestOpaqueBodiesKeepIsTheDefault(t *testing.T) {
	keepExplicit := testProfile()
	keepExplicit.OpaqueBodies = model.OpaqueKeep
	for _, tc := range []struct {
		name string
		p    model.AnonymizationProfile
	}{
		{"zero value", testProfile()},
		{"keep", keepExplicit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := mustEngine(t, tc.p)
			if got := e.Profile().OpaqueBodies; got != model.OpaqueKeep {
				t.Errorf("Profile().OpaqueBodies = %q, want keep", got)
			}
			for _, c := range opaqueBodies(t) {
				ex := exchangeWithBodies(c.ct, c.header, c.contentEncoding, c.body)
				changes, err := e.ExchangeWithReport(ex)
				if err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
				if !bytes.Equal(ex.Request.Body, c.body) || !bytes.Equal(ex.Response.Body, c.body) {
					t.Errorf("%s: body changed under keep", c.name)
				}
				for _, ch := range changes {
					if ch.Source == "opaque_bodies" {
						t.Errorf("%s: keep reported a fill: %+v", c.name, ch)
					}
				}
			}
		})
	}
}

func TestOpaqueBodiesFill(t *testing.T) {
	e := fillEngine(t)
	other := fillEngine(t) // same secret, separate engine
	for _, c := range opaqueBodies(t) {
		t.Run(c.name, func(t *testing.T) {
			ex := exchangeWithBodies(c.ct, c.header, c.contentEncoding, c.body)
			changes, err := e.ExchangeWithReport(ex)
			if err != nil {
				t.Fatal(err)
			}
			for _, side := range []struct {
				name string
				body []byte
				size int64
				h    http.Header
			}{
				{"request", ex.Request.Body, ex.Request.BodySize, ex.Request.Header},
				{"response", ex.Response.Body, ex.Response.BodySize, ex.Response.Header},
			} {
				if len(side.body) != len(c.body) {
					t.Fatalf("%s: filler is %d bytes, body was %d", side.name, len(side.body), len(c.body))
				}
				if bytes.Equal(side.body, c.body) {
					t.Fatalf("%s: body was kept", side.name)
				}
				if n := min(8, len(c.body)); bytes.Contains(side.body, c.body[:n]) {
					t.Errorf("%s: filler still contains the start of the original", side.name)
				}
				if side.size != int64(len(c.body)) {
					t.Errorf("%s: BodySize %d, want %d", side.name, side.size, len(c.body))
				}
				if got := side.h.Get("Content-Length"); got != strconv.Itoa(len(c.body)) {
					t.Errorf("%s: Content-Length %q", side.name, got)
				}
			}
			// Equal bodies get equal filler, whatever the engine instance.
			if !bytes.Equal(ex.Request.Body, ex.Response.Body) {
				t.Error("equal bodies got different filler")
			}
			ex2 := exchangeWithBodies(c.ct, c.header, c.contentEncoding, c.body)
			if err := other.Exchange(ex2); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(ex.Request.Body, ex2.Request.Body) {
				t.Error("filler is not deterministic across engines with the same secret")
			}
			// The report names the side and the source.
			for _, loc := range []string{"request.body", "response.body"} {
				ch, ok := changeAt(changes, loc)
				if !ok || ch.Source != "opaque_bodies" {
					t.Errorf("%s: no opaque_bodies change in %v", loc, changes)
					continue
				}
				if !strings.Contains(ch.Before, strconv.Itoa(len(c.body))) || !strings.Contains(ch.After, strconv.Itoa(len(c.body))) {
					t.Errorf("%s: before/after should state the size: %+v", loc, ch)
				}
				if strings.Contains(ch.Before+ch.After, "jane") {
					t.Errorf("%s: the report leaks body bytes: %+v", loc, ch)
				}
			}
		})
	}
}

func TestOpaqueFillSeparatesBodiesSecretsAndSalts(t *testing.T) {
	a := patterned(400, "A")
	b := append(bytes.Clone(a), 'x') // differs only by one trailing byte
	c := append(bytes.Clone(a[:399]), 'y')
	fill := func(e *Engine, body []byte) []byte {
		t.Helper()
		resp := &model.Response{Header: http.Header{"Content-Type": {"image/png"}}, Body: bytes.Clone(body), BodySize: int64(len(body))}
		if err := e.Response(resp); err != nil {
			t.Fatal(err)
		}
		return resp.Body
	}
	e := fillEngine(t)
	fa, fa2, fb, fc := fill(e, a), fill(e, a), fill(e, b), fill(e, c)
	if !bytes.Equal(fa, fa2) {
		t.Error("the same body got different filler")
	}
	if bytes.Equal(fa, fb[:len(fa)]) || bytes.Equal(fa, fc) || bytes.Equal(fb, fc) {
		t.Error("different bodies got the same filler")
	}
	if bytes.Equal(fa[:32], fb[:32]) || bytes.Equal(fa[:32], fc[:32]) {
		t.Error("a one byte difference should change the whole filler, not just the tail")
	}

	p := fillProfile()
	p.Secret = "another-secret"
	if bytes.Equal(fa, fill(mustEngine(t, p), a)) {
		t.Error("a different secret gave the same filler")
	}
	if bytes.Equal(fa, fill(e.WithSalt("clone-1"), a)) {
		t.Error("a different salt gave the same filler")
	}
	if !bytes.Equal(fill(e.WithSalt("clone-1"), a), fill(e.WithSalt("clone-1"), a)) {
		t.Error("filler is not stable within one salt")
	}
	// Looks like noise: nearly every byte value appears.
	big := fill(e, patterned(1<<16, "Z"))
	var seen [256]bool
	n := 0
	for _, v := range big {
		if !seen[v] {
			seen[v] = true
			n++
		}
	}
	if n < 250 {
		t.Errorf("filler uses only %d distinct byte values", n)
	}
}

func TestOpaqueFillUnparsableBinaryUnderATextualType(t *testing.T) {
	garbage := append([]byte("{\x00\x01\xff\xfe nothing here "), patterned(64, "g")...)
	for _, ct := range []string{"application/json", "application/vnd.api+json", ""} {
		for _, fill := range []bool{false, true} {
			p := testProfile()
			if fill {
				p.OpaqueBodies = model.OpaqueFill
			}
			resp := &model.Response{Header: http.Header{"Content-Type": {ct}}, Body: bytes.Clone(garbage), BodySize: int64(len(garbage))}
			if ct == "" {
				resp.Header = http.Header{}
			}
			if err := mustEngine(t, p).Response(resp); err != nil {
				t.Fatal(err)
			}
			if fill && (len(resp.Body) != len(garbage) || bytes.Equal(resp.Body, garbage)) {
				t.Errorf("%q: binary that is not JSON should be filled: %d bytes, kept=%v", ct, len(resp.Body), bytes.Equal(resp.Body, garbage))
			}
			if !fill && !bytes.Equal(resp.Body, garbage) {
				t.Errorf("%q: keep must leave binary alone", ct)
			}
		}
	}
	// Text that is merely not valid JSON is not binary: it is scanned, not filled.
	resp := &model.Response{Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"oops": jane@acme.io`), BodySize: 21}
	if err := fillEngine(t).Response(resp); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(resp.Body), "acme.io") || !strings.HasPrefix(string(resp.Body), `{"oops": `) {
		t.Errorf("malformed JSON text: %q", resp.Body)
	}
}

func TestOpaqueFillLeavesInspectableBodiesAlone(t *testing.T) {
	e := fillEngine(t)
	cases := []struct {
		name, ct, body string
		gone, kept     []string
	}{
		{"json", "application/json", `{"email":"jane@acme.io","name":"Widget"}`, []string{"acme.io"}, []string{`"name":"Widget"`}},
		{"text", "text/plain", "mail jane@acme.io please", []string{"acme.io"}, []string{"mail ", " please"}},
		{"form", "application/x-www-form-urlencoded", "email=jane%40acme.io&a=1", []string{"acme.io"}, []string{"a=1"}},
		{"sniffed text", "application/x-custom", "ping jane@acme.io", []string{"acme.io"}, []string{"ping "}},
		{"identity encoding", "application/json", `{"email":"jane@acme.io"}`, []string{"acme.io"}, nil},
	}
	for _, c := range cases {
		h := http.Header{}
		if c.name == "identity encoding" {
			h.Set("Content-Encoding", "identity")
		}
		ex := exchangeWithBodies(c.ct, h, "", []byte(c.body))
		if err := e.Exchange(ex); err != nil {
			t.Fatal(err)
		}
		got := string(ex.Response.Body)
		for _, g := range c.gone {
			if strings.Contains(got, g) {
				t.Errorf("%s: %q not anonymized: %s", c.name, g, got)
			}
		}
		for _, k := range c.kept {
			if !strings.Contains(got, k) {
				t.Errorf("%s: %q was filled instead of kept: %q", c.name, k, got)
			}
		}
	}
}

func TestOpaqueFillTruncatedAndEmpty(t *testing.T) {
	e := fillEngine(t)
	body := patterned(500, "T")
	resp := &model.Response{
		Header: http.Header{"Content-Type": {"image/jpeg"}, "Content-Length": {"100000"}},
		Body:   bytes.Clone(body), BodySize: 100000, BodyTruncated: true,
	}
	if err := e.Response(resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Body) != 500 || bytes.Equal(resp.Body, body) {
		t.Errorf("truncated opaque body: %d bytes, kept=%v", len(resp.Body), bytes.Equal(resp.Body, body))
	}
	if resp.BodySize != 100000 || !resp.BodyTruncated || resp.Header.Get("Content-Length") != "100000" {
		t.Errorf("truncated capture bookkeeping changed: size %d, CL %q", resp.BodySize, resp.Header.Get("Content-Length"))
	}

	for _, empty := range [][]byte{nil, {}} {
		ex := exchangeWithBodies("image/png", nil, "", nil)
		ex.Request.Body, ex.Response.Body = empty, empty
		changes, err := e.ExchangeWithReport(ex)
		if err != nil {
			t.Fatal(err)
		}
		if len(ex.Request.Body) != 0 || len(ex.Response.Body) != 0 || len(changes) != 0 {
			t.Errorf("empty body: %d/%d bytes, %d changes", len(ex.Request.Body), len(ex.Response.Body), len(changes))
		}
	}
}

func TestOpaqueFillThroughRequestAndResponseAPIs(t *testing.T) {
	e := fillEngine(t)
	body := patterned(100, "R")
	req := &model.Request{Method: "PUT", Path: "/up", Header: http.Header{"Content-Type": {"application/zip"}}, Body: bytes.Clone(body), BodySize: 100}
	if err := e.Request(req); err != nil {
		t.Fatal(err)
	}
	if len(req.Body) != 100 || bytes.Equal(req.Body, body) {
		t.Error("Request() did not fill an opaque body")
	}
	// A body that is also JSON-typed but really compressed (header survived).
	resp := &model.Response{Header: http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {"GZIP"}}, Body: bytes.Clone(body), BodySize: 100}
	if err := e.Response(resp); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(resp.Body, body) {
		t.Error("Response() did not fill a compressed body")
	}
}

func TestOpaqueBodiesValidation(t *testing.T) {
	p := testProfile()
	p.OpaqueBodies = "shred"
	if _, err := New(p); err == nil || !strings.Contains(err.Error(), "opaque_bodies") {
		t.Errorf("New with an unknown opaque_bodies: %v", err)
	}
	if err := Validate(p); err == nil || !strings.Contains(err.Error(), "opaque_bodies") {
		t.Errorf("Validate with an unknown opaque_bodies: %v", err)
	}
	p.OpaqueBodies = model.OpaqueFill
	if err := Validate(p); err != nil {
		t.Errorf("fill must validate: %v", err)
	}
	if got := mustEngine(t, p).Profile().OpaqueBodies; got != model.OpaqueFill {
		t.Errorf("Profile().OpaqueBodies = %q", got)
	}
}

func TestKeyerDigestBytesMatchesDigest(t *testing.T) {
	k := testKeyer("digest-bytes")
	for _, n := range []int{0, 1, 31, 32, 33, 500, 505, 506, 511, 512, 513, 1024, 5000, 100000} {
		body := patterned(n, "d")
		want := k.digest("opaque", string(body))
		if got := k.digestBytes("opaque", body); got != want {
			t.Errorf("n=%d: digestBytes differs from digest", n)
		}
	}
	if k.digestBytes("a", []byte("x")) == k.digestBytes("b", []byte("x")) {
		t.Error("domains do not separate digestBytes")
	}
}

func TestPRNGFillMatchesByteStream(t *testing.T) {
	seed := sha256.Sum256([]byte("seed"))
	for _, n := range []int{0, 1, 31, 32, 33, 64, 65, 1000, 100000} {
		a, b := newPRNG(seed), newPRNG(seed)
		want := make([]byte, n)
		for i := range want {
			want[i] = a.byte()
		}
		got := make([]byte, n)
		b.fill(got)
		if !bytes.Equal(got, want) {
			t.Errorf("n=%d: fill differs from byte()", n)
		}
	}
	// fill continues where byte stopped, and the other way round.
	a, b := newPRNG(seed), newPRNG(seed)
	want := make([]byte, 100)
	for i := range want {
		want[i] = a.byte()
	}
	got := make([]byte, 100)
	for i := 0; i < 7; i++ {
		got[i] = b.byte()
	}
	b.fill(got[7:90])
	for i := 90; i < 100; i++ {
		got[i] = b.byte()
	}
	if !bytes.Equal(got, want) {
		t.Error("mixing fill and byte changes the stream")
	}
}

func TestOpaqueFillSizes(t *testing.T) {
	k := testKeyer("sizes")
	for _, n := range []int{0, 1, 2, 31, 32, 33, 1000, 1 << 20} {
		body := patterned(n, "s")
		f := k.opaqueFill(body)
		if len(f) != n {
			t.Fatalf("n=%d: %d bytes", n, len(f))
		}
		if n > 16 && bytes.Equal(f, body) {
			t.Errorf("n=%d: filler equals body", n)
		}
		if !bytes.Equal(f, k.opaqueFill(body)) {
			t.Errorf("n=%d: not deterministic", n)
		}
	}
}
