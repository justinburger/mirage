package anonymize

import (
	"bytes"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

// tpart describes one part of a test multipart body.
type tpart struct {
	name     string
	filename string // "" = not a file part
	ct       string // "" = no Content-Type header
	extra    string // extra raw header lines, each ending in the line break
	body     string
}

// buildMP renders parts as a multipart body with line ending nl, with a
// preamble and an epilogue.
func buildMP(boundary, nl string, parts ...tpart) string {
	var sb strings.Builder
	sb.WriteString("This is a preamble." + nl)
	for _, p := range parts {
		sb.WriteString("--" + boundary + nl)
		sb.WriteString(`Content-Disposition: form-data; name="` + p.name + `"`)
		if p.filename != "" {
			sb.WriteString(`; filename="` + p.filename + `"`)
		}
		sb.WriteString(nl)
		if p.ct != "" {
			sb.WriteString("Content-Type: " + p.ct + nl)
		}
		sb.WriteString(strings.ReplaceAll(p.extra, "\r\n", nl))
		sb.WriteString(nl)
		sb.WriteString(p.body)
		sb.WriteString(nl)
	}
	sb.WriteString("--" + boundary + "--" + nl)
	sb.WriteString("An epilogue.")
	return sb.String()
}

const mpBoundary = "----MirageBoundary7MA4YWxk"

func mpCT(boundary string) string { return "multipart/form-data; boundary=" + boundary }

// mpRequest anonymizes a request carrying body as multipart form data.
func mpRequest(t testing.TB, e *Engine, ct, body string, truncated bool, size int64) *model.Request {
	t.Helper()
	if size == 0 {
		size = int64(len(body))
	}
	req := &model.Request{
		Method: "POST", Path: "/upload", BodySize: size, BodyTruncated: truncated, Body: []byte(body),
		Header: http.Header{"Content-Type": {ct}, "Content-Length": {strconv.FormatInt(size, 10)}},
	}
	if err := e.Request(req); err != nil {
		t.Fatalf("Request: %v", err)
	}
	return req
}

type gotPart struct {
	name, filename, ct, body string
}

// readMP parses a multipart body with the standard library.
func readMP(t testing.TB, body, boundary string) []gotPart {
	t.Helper()
	mr := multipart.NewReader(strings.NewReader(body), boundary)
	var out []gotPart
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("the output is not valid multipart: %v\n%q", err, body)
		}
		data, err := io.ReadAll(p)
		if err != nil {
			t.Fatalf("reading part: %v\n%q", err, body)
		}
		out = append(out, gotPart{p.FormName(), p.FileName(), p.Header.Get("Content-Type"), string(data)})
	}
}

func mpByName(parts []gotPart, name string) (gotPart, bool) {
	for _, p := range parts {
		if p.name == name {
			return p, true
		}
	}
	return gotPart{}, false
}

func TestSplitMultipart(t *testing.T) {
	const b = "BND"
	cases := []struct {
		name      string
		body      string
		truncated bool
		ok        bool
		bodies    []string // expected part bodies
	}{
		{"crlf", "--BND\r\nA: 1\r\n\r\none\r\n--BND\r\nA: 2\r\n\r\ntwo\r\n--BND--\r\n", false, true, []string{"one", "two"}},
		{"lf only", "--BND\nA: 1\n\none\n--BND--\n", false, true, []string{"one"}},
		{"preamble and epilogue", "junk\r\n--BND\r\n\r\nx\r\n--BND--\r\nmore --BND junk", false, true, []string{"x"}},
		{"no headers", "--BND\r\n\r\nbody\r\n--BND--", false, true, []string{"body"}},
		{"empty body", "--BND\r\nA: 1\r\n\r\n\r\n--BND--\r\n", false, true, []string{""}},
		{"delimiter right after the headers", "--BND\r\nA: 1\r\n\r\n--BND--\r\n", false, true, []string{""}},
		{"no parts", "--BND--\r\n", false, true, nil},
		{"closing delimiter without a line break", "--BND\r\n\r\nx\r\n--BND--", false, true, []string{"x"}},
		{"transport padding", "--BND  \t\r\n\r\nx\r\n--BND--  \r\n", false, true, []string{"x"}},
		{"boundary text inside a body", "--BND\r\n\r\nline\r\n--BNDX not a delimiter\r\nx--BND\r\n--BND--\r\n", false, true, []string{"line\r\n--BNDX not a delimiter\r\nx--BND"}},
		{"body ending in a bare CR", "--BND\r\n\r\nab\r\r\n--BND--\r\n", false, true, []string{"ab\r"}},
		{"binary body", "--BND\r\n\r\n\x00\x01\xff\r\n--BND--\r\n", false, true, []string{"\x00\x01\xff"}},

		{"no delimiter", "just text", false, false, nil},
		{"empty", "", false, false, nil},
		{"wrong boundary", "--OTHER\r\n\r\nx\r\n--OTHER--\r\n", false, false, nil},
		{"delimiter not at a line start", "x--BND\r\n\r\nx\r\n", false, false, nil},
		{"junk before a closing delimiter is preamble", "x--BND\r\n\r\nx\r\n--BND--\r\n", false, true, nil},
		{"no closing delimiter", "--BND\r\n\r\nx\r\n", false, false, nil},
		{"unterminated headers", "--BND\r\nA: 1", false, false, nil},
		{"part delimiter at the end", "--BND\r\n\r\nx\r\n--BND", false, false, nil},

		{"truncated in a body", "--BND\r\n\r\none\r\n--BND\r\n\r\ntw", true, true, []string{"one", "tw"}},
		{"truncated in headers", "--BND\r\n\r\none\r\n--BND\r\nA: 1\r\nB:", true, true, []string{"one", ""}},
		{"truncated after a delimiter", "--BND\r\n\r\none\r\n--BND", true, true, []string{"one", ""}},
		{"truncated inside the opening delimiter", "--BN", true, false, nil},
		{"truncated but closed", "--BND\r\n\r\none\r\n--BND--\r\n", true, true, []string{"one"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parts, ok := splitMultipart([]byte(c.body), b, c.truncated)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (parts %+v)", ok, c.ok, parts)
			}
			var got []string
			for _, p := range parts {
				if p.lo > p.hdrLo || p.hdrLo > p.bodyLo || p.bodyLo > p.bodyHi || p.bodyHi > len(c.body) {
					t.Fatalf("inconsistent offsets %+v for a %d byte body", p, len(c.body))
				}
				got = append(got, c.body[p.bodyLo:p.bodyHi])
			}
			if !reflect.DeepEqual(got, c.bodies) {
				t.Errorf("part bodies = %q, want %q", got, c.bodies)
			}
		})
	}
}

// TestBoundaryOfNonASCII is the regression test for a crash: boundaryOf looked
// for "boundary=" in a lower-cased copy of the header and sliced the original
// with that index, but lower-casing changes the length of non-ASCII text (and
// of invalid UTF-8), so the slice went out of range.
func TestBoundaryOfNonASCII(t *testing.T) {
	cases := []struct{ ct, want string }{
		// U+023A is two bytes, its lower case U+2C65 three.
		{"multipart/form-data; x=\u023a; boundary=", ""},
		{"multipart/form-data; x=\u023a; boundary=abc", "abc"},
		{"multipart/form-data; x=\u023a; Boundary=XyZ; y=1", "XyZ"},
		// Invalid UTF-8 is replaced by a three-byte U+FFFD when lower-cased.
		{"multipart/form-data; x=\xff\xfe; boundary=", ""},
		{"multipart/form-data; x=\xff\xfe; BOUNDARY=a(b)c", "a(b)c"},
		{"\xff\xff\xff\xffboundary=x", "x"},
		{"\u0130boundary=x", "x"}, // U+0130 lower-cases to two runes
		{"boundary=", ""},
	}
	for _, c := range cases {
		if got := boundaryOf(c.ct); got != c.want {
			t.Errorf("boundaryOf(%q) = %q, want %q", c.ct, got, c.want)
		}
	}
	// And through the engine: the body is malformed multipart, not a panic.
	e := builtinEngine(t)
	for _, ct := range []string{
		"multipart/form-data; x=\u023a; boundary=",
		"multipart/form-data; x=\xff\xfe; boundary=",
	} {
		req := &model.Request{Method: "POST", Path: "/x", Header: http.Header{"Content-Type": {ct}}, Body: []byte("--b\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\nv\r\n--b--\r\n")}
		if err := e.Request(req); err != nil {
			t.Errorf("Content-Type %q: %v", ct, err)
		}
	}
}

func TestMultipartHelpers(t *testing.T) {
	boundaries := []struct{ ct, want string }{
		{"multipart/form-data; boundary=abc", "abc"},
		{`multipart/form-data; boundary="a b+c"`, "a b+c"},
		{"Multipart/Form-Data; Boundary=XyZ; charset=utf-8", "XyZ"},
		{"multipart/form-data; boundary=a(b)c", "a(b)c"}, // mime rejects the parentheses; the lenient scan keeps them
		{"multipart/form-data", ""},
		{"multipart/form-data; boundary=", ""},
		{"", ""},
	}
	for _, c := range boundaries {
		if got := boundaryOf(c.ct); got != c.want {
			t.Errorf("boundaryOf(%q) = %q, want %q", c.ct, got, c.want)
		}
	}

	hdr := "Content-Disposition: form-data; name=\"up\"; filename=\"a.png\"\r\nContent-Type: image/png\r\nContent-Transfer-Encoding: Base64\r\n\r\n"
	pi := parsePartHeaders([]byte(hdr))
	if pi.name != "up" || !pi.hasFilename || pi.contentType != "image/png" || !pi.encoded {
		t.Errorf("parsePartHeaders = %+v", pi)
	}
	folded := "content-disposition: form-data;\r\n\tname=\"f\"\r\nCONTENT-TYPE:   text/plain  \r\n\r\n"
	pi = parsePartHeaders([]byte(folded))
	if pi.name != "f" || pi.hasFilename || pi.contentType != "text/plain" || pi.encoded {
		t.Errorf("folded headers: %+v", pi)
	}
	// Dispositions the mime package rejects are still read.
	pi = parsePartHeaders([]byte("Content-Disposition: form-data; name=a b; filename=x y.txt\r\n\r\n"))
	if pi.name != "a b" || !pi.hasFilename {
		t.Errorf("lenient disposition: %+v", pi)
	}
	// An empty filename (a file input left empty) still marks a file part.
	pi = parsePartHeaders([]byte("Content-Disposition: form-data; name=\"f\"; filename=\"\"\r\n\r\n"))
	if !pi.hasFilename {
		t.Errorf("empty filename: %+v", pi)
	}
	if pi = parsePartHeaders([]byte("garbage line without a colon\r\n\r\n")); pi != (partInfo{}) {
		t.Errorf("garbage headers: %+v", pi)
	}
	if pi = parsePartHeaders(nil); pi != (partInfo{}) {
		t.Errorf("no headers: %+v", pi)
	}
}

func TestMultipartTextPartsGetRulesAndDetectors(t *testing.T) {
	rules := []model.AnonymizationRule{
		rl(model.TargetForm, "password", model.StrategyRedact),
		rl(model.TargetForm, "token", model.StrategyHash),
		withFake(rl(model.TargetForm, "nickname", model.StrategyFake), model.FakeName),
		rl(model.TargetRegex, `ORDER-\d{4}`, model.StrategyHash),
		rl(model.TargetJSON, "$..session", model.StrategyRedact),
	}
	e := mustEngine(t, testProfile(rules...))
	in := buildMP(mpBoundary, "\r\n",
		tpart{name: "email", body: "jane@acme.io"},
		tpart{name: "password", ct: "text/plain; charset=utf-8", body: "hunter2"},
		tpart{name: "token", body: "tok-123"},
		tpart{name: "nickname", body: "Jane"},
		tpart{name: "note", ct: "text/plain", body: "call 415-555-2671 about ORDER-1234, thanks"},
		tpart{name: "plain", body: "nothing sensitive here"},
		tpart{name: "meta", ct: "application/json", body: `{"owner":"jane@acme.io","session":"abc","n":1}`},
		tpart{name: "avatar", filename: "me.png", ct: "image/png", body: "\x89PNG-bytes jane@acme.io"},
		tpart{name: "resume", filename: "cv.txt", ct: "text/plain", body: "John Smith, jane@acme.io"},
	)
	req := mpRequest(t, e, mpCT(mpBoundary), in, false, 0)
	got := string(req.Body)
	parts := readMP(t, got, mpBoundary)
	if len(parts) != 9 {
		t.Fatalf("%d parts in the output: %q", len(parts), got)
	}
	k := e.k
	check := func(name string, f func(p gotPart) bool, what string) {
		t.Helper()
		p, ok := mpByName(parts, name)
		if !ok {
			t.Errorf("part %q is gone", name)
			return
		}
		if !f(p) {
			t.Errorf("part %q: %s; got %q", name, what, p.body)
		}
	}
	check("email", func(p gotPart) bool {
		return !strings.Contains(p.body, "acme.io") && strings.Contains(p.body, "@example.")
	}, "e-mail should be faked by the detector")
	check("password", func(p gotPart) bool { return p.body == "[REDACTED]" && p.ct == "text/plain; charset=utf-8" }, "form rule should redact it and keep the part headers")
	check("token", func(p gotPart) bool { return p.body == k.hashToken("tok-123") }, "form rule should hash it")
	check("nickname", func(p gotPart) bool { return p.body == k.fake(model.FakeName, "Jane") && p.body != "Jane" }, "form rule should fake it")
	check("note", func(p gotPart) bool {
		return !strings.Contains(p.body, "415-555-2671") && !strings.Contains(p.body, "ORDER-1234") &&
			strings.HasPrefix(p.body, "call ") && strings.HasSuffix(p.body, ", thanks")
	}, "detectors and regex rules should apply inside the text")
	check("plain", func(p gotPart) bool { return p.body == "nothing sensitive here" }, "should be untouched")
	check("meta", func(p gotPart) bool {
		return strings.Contains(p.body, `"session":"[REDACTED]"`) && !strings.Contains(p.body, "acme.io") && strings.Contains(p.body, `"n":1`)
	}, "a JSON part should get JSON rules and detectors")
	check("avatar", func(p gotPart) bool { return p.body == "\x89PNG-bytes jane@acme.io" && p.filename == "me.png" }, "binary file parts are kept")
	check("resume", func(p gotPart) bool {
		return strings.HasPrefix(p.body, "John Smith, ") && !strings.Contains(p.body, "acme.io") && strings.Contains(p.body, "@example.") && p.filename == "cv.txt"
	}, "text file parts are inspected like any text body")

	// Boundary, preamble, epilogue, delimiters and part headers are byte for byte.
	skeleton := func(s string, bodies []string) string {
		for _, b := range bodies {
			s = strings.Replace(s, b, "§", 1)
		}
		return s
	}
	var inBodies, outBodies []string
	for _, p := range readMP(t, in, mpBoundary) {
		inBodies = append(inBodies, p.body)
	}
	for _, p := range parts {
		outBodies = append(outBodies, p.body)
	}
	if skeleton(in, inBodies) != skeleton(got, outBodies) {
		t.Errorf("framing changed:\n in: %q\nout: %q", skeleton(in, inBodies), skeleton(got, outBodies))
	}
	if !strings.HasPrefix(got, "This is a preamble.\r\n--"+mpBoundary+"\r\n") || !strings.HasSuffix(got, "--"+mpBoundary+"--\r\nAn epilogue.") {
		t.Errorf("preamble or epilogue changed: %q", got)
	}

	// BodySize and Content-Length follow the new length.
	if req.BodySize != int64(len(got)) || req.Header.Get("Content-Length") != strconv.Itoa(len(got)) {
		t.Errorf("BodySize %d, Content-Length %q, body %d bytes", req.BodySize, req.Header.Get("Content-Length"), len(got))
	}
	if req.Header.Get("Content-Type") != mpCT(mpBoundary) {
		t.Errorf("Content-Type changed: %q", req.Header.Get("Content-Type"))
	}
}

func TestMultipartUnchangedBodyIsByteIdentical(t *testing.T) {
	for _, e := range []*Engine{defaultEngine(t), mustEngine(t, testProfile(rl(model.TargetForm, "password", model.StrategyRedact)))} {
		in := buildMP(mpBoundary, "\r\n",
			tpart{name: "a", body: "hello"},
			tpart{name: "pic", filename: "x.bin", ct: "application/octet-stream", body: "\x00\x01\x02 jane@acme.io"},
			tpart{name: "empty", body: ""},
		)
		req := mpRequest(t, e, mpCT(mpBoundary), in, false, 0)
		if string(req.Body) != in {
			t.Errorf("unchanged multipart body was rewritten:\n in: %q\nout: %q", in, req.Body)
		}
	}
}

func TestMultipartFillPartBehaviour(t *testing.T) {
	bin := string(patterned(300, "BIN"))
	big := string(patterned(5000, "FILE"))
	in := buildMP(mpBoundary, "\r\n",
		tpart{name: "email", body: "jane@acme.io"},
		tpart{name: "photo", filename: "a.jpg", ct: "image/jpeg", body: bin},
		tpart{name: "doc", filename: "cv.txt", ct: "text/plain", body: "John Smith lives at 1 Main St, jane@acme.io"}, // a text file: inspected, never filled
		tpart{name: "blob", ct: "application/octet-stream", body: "\x00\x01\x02\xffbinary"},                           // binary part without filename
		tpart{name: "enc", ct: "application/octet-stream", extra: "Content-Transfer-Encoding: base64\r\n", body: "AAEC/2JpbmFyeS1ieXRlcy1oZXJlAH8="},
		tpart{name: "video", filename: "v.mp4", ct: "video/mp4", body: big},
		tpart{name: "same", filename: "s.bin", ct: "image/png", body: bin}, // equal to photo
		tpart{name: "empty", filename: "e.bin", ct: "image/png", body: ""},
	)
	keep := mpRequest(t, defaultEngine(t), mpCT(mpBoundary), in, false, 0)
	kp := readMP(t, string(keep.Body), mpBoundary)
	for _, n := range []string{"photo", "blob", "enc", "video", "same"} {
		p, _ := mpByName(kp, n)
		orig, _ := mpByName(readMP(t, in, mpBoundary), n)
		if p.body != orig.body {
			t.Errorf("keep: part %q changed", n)
		}
	}
	if doc, _ := mpByName(kp, "doc"); strings.Contains(doc.body, "acme.io") || !strings.HasPrefix(doc.body, "John Smith lives at 1 Main St, ") {
		t.Errorf("keep: the text file part was not anonymized: %q", doc.body)
	}

	e := fillEngine(t)
	req := mpRequest(t, e, mpCT(mpBoundary), in, false, 0)
	op := readMP(t, in, mpBoundary)
	fp := readMP(t, string(req.Body), mpBoundary)
	if len(fp) != len(op) {
		t.Fatalf("%d parts, want %d", len(fp), len(op))
	}
	for i := range op {
		o, f := op[i], fp[i]
		if o.name != f.name || o.filename != f.filename || o.ct != f.ct {
			t.Errorf("part %d headers changed: %+v -> %+v", i, o, f)
		}
		switch o.name {
		case "email":
			if strings.Contains(f.body, "acme.io") {
				t.Errorf("text part not anonymized: %q", f.body)
			}
		case "empty":
			if f.body != "" {
				t.Errorf("empty part got content: %q", f.body)
			}
		case "doc":
			// A text file is anonymized like any text, not replaced by filler.
			if strings.Contains(f.body, "acme.io") || !strings.HasPrefix(f.body, "John Smith lives at 1 Main St, ") || !strings.Contains(f.body, "@example.") {
				t.Errorf("text file part: %q", f.body)
			}
		default:
			if len(f.body) != len(o.body) {
				t.Errorf("part %q: %d bytes, want %d", o.name, len(f.body), len(o.body))
			}
			if f.body == o.body {
				t.Errorf("part %q was not filled", o.name)
			}
		}
	}
	photo, _ := mpByName(fp, "photo")
	same, _ := mpByName(fp, "same")
	if photo.body != same.body {
		t.Error("equal file bodies got different filler")
	}
	video, _ := mpByName(fp, "video")
	if photo.body == video.body[:len(photo.body)] {
		t.Error("different file bodies got the same filler")
	}
	// The text part changed length but the whole thing stays consistent.
	if req.BodySize != int64(len(req.Body)) || req.Header.Get("Content-Length") != strconv.Itoa(len(req.Body)) {
		t.Errorf("BodySize %d, CL %q, body %d", req.BodySize, req.Header.Get("Content-Length"), len(req.Body))
	}

	// A body with only file parts keeps its exact length and size.
	onlyFiles := buildMP(mpBoundary, "\r\n", tpart{name: "f", filename: "f.bin", ct: "image/png", body: bin})
	r2 := mpRequest(t, e, mpCT(mpBoundary), onlyFiles, false, 0)
	if len(r2.Body) != len(onlyFiles) || r2.BodySize != int64(len(onlyFiles)) || string(r2.Body) == onlyFiles {
		t.Errorf("file-only multipart: %d -> %d bytes, size %d, kept=%v", len(onlyFiles), len(r2.Body), r2.BodySize, string(r2.Body) == onlyFiles)
	}
	if !strings.Contains(string(r2.Body), `Content-Disposition: form-data; name="f"; filename="f.bin"`) {
		t.Error("part headers were not preserved")
	}
}

func TestMultipartLineEndingsAndBoundaries(t *testing.T) {
	e := fillEngine(t)
	for _, c := range []struct {
		name, boundary, ctValue, nl string
	}{
		{"crlf", "plainBoundary", "multipart/form-data; boundary=plainBoundary", "\r\n"},
		{"lf", "plainBoundary", "multipart/form-data; boundary=plainBoundary", "\n"},
		{"quoted", "a b+c=d", `multipart/form-data; boundary="a b+c=d"`, "\r\n"},
		{"upper case type and key", "XyZ", "MULTIPART/FORM-DATA; BOUNDARY=XyZ", "\r\n"},
		{"extra parameter", "bnd", "multipart/form-data; charset=utf-8; boundary=bnd", "\r\n"},
		{"boundary with dashes", "--------------------------1234", "multipart/form-data; boundary=--------------------------1234", "\r\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := buildMP(c.boundary, c.nl,
				tpart{name: "email", body: "jane@acme.io"},
				tpart{name: "file", filename: "f.bin", ct: "image/png", body: string(patterned(100, "q"))},
			)
			req := mpRequest(t, e, c.ctValue, in, false, 0)
			out := string(req.Body)
			if strings.Contains(out, "acme.io") || !strings.Contains(out, "@example.") {
				t.Errorf("text part not anonymized: %q", out)
			}
			if strings.Count(out, "--"+c.boundary+c.nl) != 2 || !strings.Contains(out, "--"+c.boundary+"--"+c.nl+"An epilogue.") {
				t.Errorf("delimiters changed: %q", out)
			}
			if c.nl == "\r\n" {
				parts := readMP(t, out, c.boundary)
				if len(parts) != 2 || len(parts[1].body) != 100 || parts[1].body == string(patterned(100, "q")) {
					t.Errorf("parts: %+v", parts)
				}
			}
			if !strings.HasPrefix(out, "This is a preamble."+c.nl+"--"+c.boundary+c.nl+"Content-Disposition: form-data; name=\"email\""+c.nl+c.nl) {
				t.Errorf("preamble or part headers changed: %q", out)
			}
		})
	}
}

func TestMultipartRemoveRule(t *testing.T) {
	e := mustEngine(t, bareProfile(rl(model.TargetForm, "secret", model.StrategyRemove)))
	a, s, b := tpart{name: "a", body: "AAA"}, tpart{name: "secret", body: "TOPSECRET"}, tpart{name: "b", body: "BBB"}
	cases := []struct {
		name  string
		parts []tpart
		want  []string
	}{
		{"first", []tpart{s, a, b}, []string{"a", "b"}},
		{"middle", []tpart{a, s, b}, []string{"a", "b"}},
		{"last", []tpart{a, b, s}, []string{"a", "b"}},
		{"only", []tpart{s}, nil},
		{"several", []tpart{s, a, s, b, s}, []string{"a", "b"}},
		{"all", []tpart{s, s}, nil},
	}
	for _, c := range cases {
		for _, nl := range []string{"\r\n", "\n"} {
			t.Run(c.name+strconv.Itoa(len(nl)), func(t *testing.T) {
				in := buildMP("bnd", nl, c.parts...)
				req := mpRequest(t, e, mpCT("bnd"), in, false, 0)
				out := string(req.Body)
				if strings.Contains(out, "TOPSECRET") || strings.Contains(out, `name="secret"`) {
					t.Fatalf("part not removed: %q", out)
				}
				if nl == "\r\n" {
					var names []string
					for _, p := range readMP(t, out, "bnd") {
						names = append(names, p.name)
					}
					if !reflect.DeepEqual(names, c.want) {
						t.Errorf("parts %v, want %v\n%q", names, c.want, out)
					}
				}
				if !strings.HasSuffix(out, "--bnd--"+nl+"An epilogue.") || !strings.HasPrefix(out, "This is a preamble."+nl) {
					t.Errorf("framing damaged: %q", out)
				}
				if req.BodySize != int64(len(out)) || req.Header.Get("Content-Length") != strconv.Itoa(len(out)) {
					t.Errorf("BodySize %d CL %q len %d", req.BodySize, req.Header.Get("Content-Length"), len(out))
				}
			})
		}
	}
}

func TestMultipartNested(t *testing.T) {
	inner := "--INNER\r\nContent-Disposition: file; filename=\"a.txt\"\r\nContent-Type: text/plain\r\n\r\nfile a for jane@acme.io\r\n" +
		"--INNER\r\nContent-Disposition: file; filename=\"b.gif\"\r\nContent-Type: image/gif\r\n\r\n" + string(patterned(80, "gif")) + "\r\n--INNER--"
	in := buildMP("OUTER", "\r\n",
		tpart{name: "email", body: "jane@acme.io"},
		tpart{name: "files", ct: "multipart/mixed; boundary=INNER", body: inner},
	)
	keep := mpRequest(t, defaultEngine(t), mpCT("OUTER"), in, false, 0)
	// The nested text file is inspected (its address is faked); the gif is
	// binary and kept.
	if strings.Contains(string(keep.Body), `acme.io`) {
		t.Errorf("an address survived in a nested text file: %q", keep.Body)
	}
	if !strings.Contains(string(keep.Body), "file a for ") || !strings.Contains(string(keep.Body), string(patterned(80, "gif"))) {
		t.Errorf("the nested text file must be anonymized and the nested gif kept: %q", keep.Body)
	}

	req := mpRequest(t, fillEngine(t), mpCT("OUTER"), in, false, 0)
	out := string(req.Body)
	if strings.Contains(out, "acme.io") || strings.Contains(out, string(patterned(80, "gif"))) || !strings.Contains(out, "file a for ") {
		t.Errorf("under fill the nested gif must be filled and the nested text anonymized: %q", out)
	}
	parts := readMP(t, out, "OUTER")
	files, _ := mpByName(parts, "files")
	if !strings.Contains(files.body, "@example.") {
		t.Errorf("nested text file: %q", files.body)
	}
	if !strings.HasPrefix(files.body, "--INNER\r\nContent-Disposition: file; filename=\"a.txt\"\r\nContent-Type: text/plain\r\n\r\n") || !strings.HasSuffix(files.body, "\r\n--INNER--") {
		t.Errorf("nested framing changed: %q", files.body)
	}
	if n := len(readMP(t, files.body, "INNER")); n != 2 {
		t.Errorf("nested multipart has %d parts", n)
	}

}

// nestedMP builds multipart bodies nested level deep; the innermost part is a
// file with secret content.
func nestedMP(level int, leaf string) string {
	b := "B" + strconv.Itoa(level)
	hdr := "--" + b + "\r\nContent-Disposition: form-data; name=\"p\"\r\n"
	if level == 0 {
		return hdr + "Content-Type: image/png\r\n\r\n" + leaf + "\r\n--" + b + "--"
	}
	return hdr + "Content-Type: multipart/mixed; boundary=B" + strconv.Itoa(level-1) + "\r\n\r\n" + nestedMP(level-1, leaf) + "\r\n--" + b + "--"
}

func TestMultipartNestingIsBounded(t *testing.T) {
	const leaf = "SECRETLEAF jane@acme.io"
	for _, depth := range []int{0, 1, maxMultipartDepth - 1, maxMultipartDepth, maxMultipartDepth + 1, 40} {
		in := nestedMP(depth, leaf)
		req := mpRequest(t, fillEngine(t), mpCT("B"+strconv.Itoa(depth)), in, false, 0)
		if len(req.Body) != len(in) {
			t.Errorf("depth %d: length %d -> %d", depth, len(in), len(req.Body))
		}
		if strings.Contains(string(req.Body), "SECRETLEAF") || strings.Contains(string(req.Body), "acme.io") {
			t.Errorf("depth %d: the innermost file survived fill", depth)
		}
		keep := mpRequest(t, defaultEngine(t), mpCT("B"+strconv.Itoa(depth)), in, false, 0)
		if depth < maxMultipartDepth && string(keep.Body) != in {
			t.Errorf("depth %d: keep changed a body with only a file in it", depth)
		}
		if depth >= maxMultipartDepth && len(keep.Body) > len(in)+64 {
			t.Errorf("depth %d: beyond the limit the body is scanned as text, not grown: %d -> %d", depth, len(in), len(keep.Body))
		}
	}
}

func TestMultipartTruncatedCapture(t *testing.T) {
	file := string(patterned(4000, "F"))
	full := buildMP(mpBoundary, "\r\n",
		tpart{name: "email", body: "jane@acme.io"},
		tpart{name: "file", filename: "big.bin", ct: "application/octet-stream", body: file},
		tpart{name: "after", body: "never captured"},
	)
	filePos := strings.Index(full, file)
	for _, c := range []struct {
		name string
		cut  int
		fill bool
	}{
		{"inside the file", filePos + 1500, true},
		{"inside the file, keep", filePos + 1500, false},
		{"right after the file's headers", filePos, true},
		{"inside the first text part", strings.Index(full, "jane@acme.io") + 4, true},
		{"inside part headers", strings.Index(full, `name="file"`) + 3, true},
		{"inside the delimiter", strings.Index(full, "--"+mpBoundary+"\r\nContent-Disposition: form-data; name=\"file\"") + 6, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := testProfile()
			if c.fill {
				p.OpaqueBodies = model.OpaqueFill
			}
			e := mustEngine(t, p)
			in := full[:c.cut]
			const wire = 1 << 20
			req := mpRequest(t, e, mpCT(mpBoundary), in, true, wire)
			out := string(req.Body)
			if !req.BodyTruncated {
				t.Error("BodyTruncated was cleared")
			}
			if req.BodySize < int64(len(out)) {
				t.Errorf("BodySize %d smaller than the captured body %d", req.BodySize, len(out))
			}
			if want := int64(wire) + int64(len(out)-len(in)); req.BodySize != want {
				t.Errorf("BodySize %d, want %d (moved by the length change)", req.BodySize, want)
			}
			if strings.Contains(out, "jane@acme.io") {
				t.Errorf("the e-mail in the captured prefix leaked: %q", out)
			}
			if c.fill && c.cut > filePos && strings.Contains(out, string(patterned(200, "F"))[:50]) {
				t.Errorf("file bytes survived under fill")
			}
			if !c.fill && c.cut > filePos && !strings.Contains(out, string(patterned(200, "F"))[:50]) {
				t.Errorf("file bytes should be kept under keep")
			}
		})
	}
}

func TestMultipartMalformed(t *testing.T) {
	text := "--x\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\nmail jane@acme.io please"
	binary := "--x\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\n\x00\x01\x02\xff\xfe" + string(patterned(60, "m"))
	cases := []struct {
		name string
		ct   string
		body string
	}{
		{"no boundary parameter", "multipart/form-data", text},
		{"empty boundary", "multipart/form-data; boundary=", text},
		{"boundary not in the body", "multipart/form-data; boundary=zzz", text},
		{"no closing delimiter", "multipart/form-data; boundary=x", text},
		{"part without header terminator", "multipart/form-data; boundary=x", "--x\r\nContent-Disposition: form-data; name=\"a\""},
		{"not multipart at all", "multipart/form-data; boundary=x", "just some text jane@acme.io"},
	}
	for _, c := range cases {
		t.Run(c.name+"/fill", func(t *testing.T) {
			req := mpRequest(t, fillEngine(t), c.ct, c.body, false, 0)
			if len(req.Body) != len(c.body) || string(req.Body) == c.body {
				t.Errorf("malformed multipart should be filled in place: %d -> %d bytes, kept=%v", len(c.body), len(req.Body), string(req.Body) == c.body)
			}
			if req.BodySize != int64(len(c.body)) {
				t.Errorf("BodySize %d", req.BodySize)
			}
		})
		t.Run(c.name+"/keep text", func(t *testing.T) {
			req := mpRequest(t, defaultEngine(t), c.ct, c.body, false, 0)
			if strings.Contains(string(req.Body), "acme.io") {
				t.Errorf("a textual malformed body must still be scanned for PII: %q", req.Body)
			}
		})
	}
	t.Run("keep binary", func(t *testing.T) {
		for _, ct := range []string{"multipart/form-data", "multipart/form-data; boundary=x"} {
			req := mpRequest(t, defaultEngine(t), ct, binary, false, 0)
			if string(req.Body) != binary {
				t.Errorf("%s: a binary malformed body must be kept", ct)
			}
		}
	})
	t.Run("fill binary", func(t *testing.T) {
		req := mpRequest(t, fillEngine(t), "multipart/form-data; boundary=x", binary, false, 0)
		if len(req.Body) != len(binary) || string(req.Body) == binary {
			t.Error("a binary malformed body should be filled")
		}
	})
}

func TestMultipartReport(t *testing.T) {
	e := fillEngine(t, rl(model.TargetForm, "password", model.StrategyRedact), rl(model.TargetForm, "gone", model.StrategyRemove),
		rl(model.TargetJSON, "$.pin", model.StrategyRedact))
	in := buildMP("rb", "\r\n",
		tpart{name: "email", body: "jane@acme.io"},
		tpart{name: "password", body: "hunter2"},
		tpart{name: "gone", body: "bye"},
		tpart{name: "meta", ct: "application/json", body: `{"pin":"1234"}`},
		tpart{name: "upload", filename: "u.png", ct: "image/png", body: string(patterned(64, "u"))},
		tpart{name: "", body: "call 415-555-2671"},
	)
	ex := newExchange()
	ex.Request.Method = "POST"
	ex.Request.Header.Set("Content-Type", mpCT("rb"))
	ex.Request.Body = []byte(in)
	ex.Request.BodySize = int64(len(in))
	changes, err := e.ExchangeWithReport(ex)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []struct{ loc, source string }{
		{"request.body multipart.email", "detector:email"},
		{"request.body multipart.password", "rule " + "password"},
		{"request.body multipart.gone", "rule gone"},
		{"request.body multipart.meta $.pin", "rule $.pin"},
		{"request.body multipart.upload", "opaque_bodies"},
		{"request.body multipart[6]", "detector:phone"},
	} {
		ch, ok := changeAt(changes, w.loc)
		if !ok {
			t.Errorf("no change at %q in %v", w.loc, locations(changes))
			continue
		}
		if ch.Source != w.source {
			t.Errorf("%s: source %q, want %q", w.loc, ch.Source, w.source)
		}
	}
	// The report never changes the result.
	ex2 := newExchange()
	ex2.Request.Header.Set("Content-Type", mpCT("rb"))
	ex2.Request.Method = "POST"
	ex2.Request.Body = []byte(in)
	ex2.Request.BodySize = int64(len(in))
	if err := e.Exchange(ex2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ex.Request.Body, ex2.Request.Body) || ex.Request.BodySize != ex2.Request.BodySize {
		t.Error("ExchangeWithReport and Exchange disagree on a multipart body")
	}
}

func TestMultipartIsIdempotentAndDeterministic(t *testing.T) {
	in := buildMP(mpBoundary, "\r\n",
		tpart{name: "email", body: "jane@acme.io"},
		tpart{name: "f", filename: "f.bin", ct: "image/png", body: string(patterned(300, "z"))},
	)
	a := mpRequest(t, fillEngine(t), mpCT(mpBoundary), in, false, 0)
	b := mpRequest(t, fillEngine(t), mpCT(mpBoundary), in, false, 0)
	if !bytes.Equal(a.Body, b.Body) {
		t.Error("multipart anonymization is not deterministic")
	}
	// A second pass over the output must not fail or break the framing.
	c := mpRequest(t, fillEngine(t), mpCT(mpBoundary), string(a.Body), false, 0)
	if len(readMP(t, string(c.Body), mpBoundary)) != 2 {
		t.Error("second pass broke the framing")
	}
}

func TestMultipartThroughExchangeAndResponse(t *testing.T) {
	e := fillEngine(t)
	in := buildMP("rr", "\r\n", tpart{name: "email", body: "jane@acme.io"}, tpart{name: "f", filename: "a.bin", ct: "image/png", body: string(patterned(50, "w"))})
	resp := &model.Response{Status: 200, Header: http.Header{"Content-Type": {"multipart/form-data; boundary=rr"}, "Content-Length": {strconv.Itoa(len(in))}}, Body: []byte(in), BodySize: int64(len(in))}
	if err := e.Response(resp); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(resp.Body), "acme.io") || len(readMP(t, string(resp.Body), "rr")) != 2 {
		t.Errorf("response multipart not handled: %q", resp.Body)
	}
	if resp.BodySize != int64(len(resp.Body)) || resp.Header.Get("Content-Length") != strconv.Itoa(len(resp.Body)) {
		t.Error("response bookkeeping")
	}
}

func TestMultipartBinaryAndGarbageDoNotPanic(t *testing.T) {
	garbage := []string{
		"", "--", "--x", "--x\r\n", "--x--", "--x\r\n\r\n", "\r\n--x\r\n\r\n\r\n--x--", "--x\n\n--x\n\n--x--",
		"--x\r\nContent-Disposition: form-data; name=\"a\"; filename=\r\n\r\nz\r\n--x--",
		"--x\r\nContent-Disposition: \x00\xff\r\n\r\n\x00\r\n--x--\r\n",
		strings.Repeat("--x\r\n", 50), strings.Repeat("--x\r\n\r\n", 50) + "--x--",
		strings.Repeat("\n--x", 1000),
	}
	for _, p := range []model.AnonymizationProfile{testProfile(rl(model.TargetForm, "a", model.StrategyRemove)), fillProfile(rl(model.TargetForm, "a", model.StrategyHash))} {
		e := mustEngine(t, p)
		for _, g := range garbage {
			for _, trunc := range []bool{false, true} {
				req := mpRequest(t, e, "multipart/form-data; boundary=x", g, trunc, int64(len(g))+10)
				if req.BodySize < int64(len(req.Body)) {
					t.Errorf("%q: BodySize %d < body %d", g, req.BodySize, len(req.Body))
				}
			}
		}
	}
}

// TestSplitMultipartAgreesWithTheStandardLibrary writes random multipart
// bodies with mime/multipart.Writer, bodies full of look-alike delimiters
// included, and checks that the splitter finds the same parts with the same
// contents, and that anonymizing them with fill keeps them readable.
func TestSplitMultipartAgreesWithTheStandardLibrary(t *testing.T) {
	rnd := rand.New(rand.NewSource(11))
	e := fillEngine(t, rl(model.TargetForm, "pw", model.StrategyRedact))
	pieces := []string{"a", "text ", "\r\n", "\n", "\r", "--", "-", "jane@acme.io", "\x00", "\xff", "--bnd", "\r\n--bnd", "\n--bnd", "\r\n--bndX", "--bnd--", " "}
	for iter := 0; iter < 300; iter++ {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		boundary := "bnd"
		if iter%2 == 0 {
			boundary = "bnd" + strconv.Itoa(iter) // vary so that look-alikes sometimes are exact
		}
		if err := w.SetBoundary(boundary); err != nil {
			t.Fatal(err)
		}
		var want []gotPart
		for i, n := 0, rnd.Intn(5); i < n; i++ {
			var sb strings.Builder
			for j, m := 0, rnd.Intn(12); j < m; j++ {
				sb.WriteString(pieces[rnd.Intn(len(pieces))])
			}
			body := sb.String()
			// A body that really contains this boundary after a line break cannot be written.
			if strings.Contains(body, "\n--"+boundary) || strings.HasPrefix(body, "--"+boundary) {
				continue
			}
			name := []string{"a", "pw", "file", "x y"}[rnd.Intn(4)]
			h := textproto.MIMEHeader{}
			disp := `form-data; name="` + name + `"`
			gp := gotPart{name: name, body: body}
			if rnd.Intn(2) == 0 {
				disp += `; filename="f.bin"`
				gp.filename = "f.bin"
			}
			h.Set("Content-Disposition", disp)
			if rnd.Intn(2) == 0 {
				h.Set("Content-Type", "application/octet-stream")
				gp.ct = "application/octet-stream"
			}
			pw, err := w.CreatePart(h)
			if err != nil {
				t.Fatal(err)
			}
			io.WriteString(pw, body)
			want = append(want, gp)
		}
		w.Close()
		in := buf.Bytes()

		parts, ok := splitMultipart(in, boundary, false)
		if !ok || len(parts) != len(want) {
			t.Fatalf("iter %d: splitMultipart ok=%v, %d parts, want %d\n%q", iter, ok, len(parts), len(want), in)
		}
		for i, p := range parts {
			if got := string(in[p.bodyLo:p.bodyHi]); got != want[i].body {
				t.Fatalf("iter %d part %d: body %q, want %q", iter, i, got, want[i].body)
			}
		}

		req := mpRequest(t, e, mpCT(boundary), string(in), false, 0)
		got := readMP(t, string(req.Body), boundary)
		if len(got) != len(want) {
			t.Fatalf("iter %d: %d parts after fill, want %d\nin:  %q\nout: %q", iter, len(got), len(want), in, req.Body)
		}
		for i := range got {
			if got[i].name != want[i].name || got[i].filename != want[i].filename || got[i].ct != want[i].ct {
				t.Fatalf("iter %d part %d: headers %+v, want %+v", iter, i, got[i], want[i])
			}
		}
	}
}

func TestScrubDelimiter(t *testing.T) {
	dash := []byte("--b")
	cases := []struct{ in, want string }{
		{"abc", "abc"},
		{"--b rest", "\x0d-b rest"}, // first byte 0x2d ^ 0x20 = 0x0d
		{"x\n--b", "x --b"},
		{"x\r\n--b\n--b", "x\r --b --b"},
		{"x--b", "x--b"}, // not at a line start
		{"\n--b", " --b"},
		{"", ""},
	}
	for _, c := range cases {
		b := []byte(c.in)
		scrubDelimiter(b, dash)
		if string(b) != c.want {
			t.Errorf("scrubDelimiter(%q) = %q, want %q", c.in, b, c.want)
		}
	}
	// A scrubbed body never contains a delimiter line (the case that starts
	// at the beginning of the body and the ones after a line break).
	for _, in := range []string{"--b", "--b\n--b\n\n--b", "q\n--b--", "--b\r\n--b"} {
		b := []byte(in)
		scrubDelimiter(b, dash)
		if bytes.HasPrefix(b, dash) || bytes.Contains(b, []byte("\n--b")) {
			t.Errorf("a delimiter survives scrubbing %q: %q", in, b)
		}
	}
	// Filled parts of a one-character boundary stay separate parts.
	e := fillEngine(t)
	for i := 0; i < 200; i++ {
		file := string(patterned(300+i, "q"+strconv.Itoa(i)))
		in := buildMP("b", "\r\n", tpart{name: "f", filename: "f", ct: "image/png", body: file})
		req := mpRequest(t, e, mpCT("b"), in, false, 0)
		if n := len(readMP(t, string(req.Body), "b")); n != 1 {
			t.Fatalf("iteration %d: %d parts after fill", i, n)
		}
	}
}
