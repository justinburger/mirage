package anonymize

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func gzipBytes(t testing.TB, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestClassifyBody(t *testing.T) {
	cases := []struct {
		ct   string
		body string
		want bodyKind
	}{
		{"application/json", `{}`, kindJSON},
		{"application/json; charset=utf-8", `{}`, kindJSON},
		{"APPLICATION/JSON", `{}`, kindJSON},
		{"text/json", `{}`, kindJSON},
		{"application/vnd.api+json", `{}`, kindJSON},
		{"application/problem+json", `{}`, kindJSON},
		{"application/ld+json; profile=x", `{}`, kindJSON},
		{"application/x-ndjson", `{}`, kindNDJSON},
		{"application/jsonl", `{}`, kindNDJSON},
		{"application/x-www-form-urlencoded", `a=b`, kindForm},
		{"application/x-www-form-urlencoded; charset=UTF-8", `a=b`, kindForm},
		{"text/plain", `hello`, kindText},
		{"text/html; charset=utf-8", `<p>`, kindText},
		{"application/xml", `<a/>`, kindText},
		{"application/soap+xml", `<a/>`, kindText},
		{"application/javascript", `x`, kindText},
		{"application/graphql", `{ me }`, kindText},
		{"multipart/form-data; boundary=x", "--x\r\nContent-Disposition: form-data\r\n\r\nhi\r\n--x--", kindText},
		{"image/png", "\x89PNG\r\n", kindBinary},
		{"audio/mpeg", "ID3", kindBinary},
		{"video/mp4", "ftyp", kindBinary},
		{"application/pdf", "%PDF-1.4", kindBinary},
		{"application/zip", "PK", kindBinary},
		{"application/protobuf", "hello", kindBinary}, // framed by lengths: never rewritten even if it is valid UTF-8
		{"application/grpc", "hello", kindBinary},
		{"application/x-protobuf", "hello", kindBinary},
		{"application/vnd.ms-excel", "hello", kindBinary},
		{"font/woff2", "wOF2", kindBinary},
		{"application/octet-stream", "plain text is fine", kindText},
		{"application/octet-stream", "bin\x00ary", kindBinary},
		{"application/x-custom", "text", kindText},
		{"application/x-custom", "\xff\xfe\x00", kindBinary},
		{"", `{"a":1}`, kindJSON},
		{"", `  [1,2]`, kindJSON},
		{"", "plain", kindText},
		{"", "bin\x00", kindBinary},
		{"", "\xff\xfe", kindBinary},
		{"application/x-custom", `{"a":1}`, kindJSON}, // unknown types are sniffed
		{"application/octet-stream", `{"a":1}`, kindJSON},
		{"application/octet-stream", `plain`, kindText},
		{"text/plain", `{"a":1}`, kindJSON},
		{"text/html", `  [1,2]`, kindJSON},
		{"text/plain", `[INFO] started`, kindJSON}, // looks like JSON; unparsable JSON is processed as text
		{"text/plain", `hello {"a":1}`, kindText},
		{"application/xml", `{"a":1}`, kindText},
		{"application/x-amz-json-1.1", `{}`, kindJSON},
		{"application/x-amz-json-1.0", `{}`, kindJSON},
		{"application/vnd.acme.customer.v2", `{"a":1}`, kindJSON}, // vendor types are sniffed, not assumed binary
		{"application/vnd.acme.customer.v2+json", `x`, kindJSON},
		{"application/vnd.acme.customer.v2.json", `x`, kindJSON},
		{"application/vnd.acme.export", `plain text`, kindText},
		{"application/vnd.acme.export", "bin\x00ary", kindBinary},
		{"application/vnd.acme.thing+xml", `<a/>`, kindText},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "hello", kindBinary},
		{"application/vnd.android.package-archive", "hello", kindBinary},
		{"application/x-www-form-urlencoded", `{"password":"x"}`, kindJSON}, // what curl -d sends
		{"application/x-www-form-urlencoded", "  [1,2]\n", kindJSON},
		{"application/x-www-form-urlencoded", `{a}=b&c=d`, kindForm},
		{"application/x-www-form-urlencoded", `[0]=a&[1]=b`, kindForm},
		{"application/x-www-form-urlencoded", `{weird=1&password=x`, kindForm},
		{"application/x-ndjson", `{}`, kindNDJSON},
		{"application/x-json-stream", `{}`, kindNDJSON},
		{"not a media type", "text", kindText},
	}
	for _, c := range cases {
		if got := classifyBody(c.ct, []byte(c.body), false); got != c.want {
			t.Errorf("classifyBody(%q, %q) = %d, want %d", c.ct, c.body, got, c.want)
		}
	}
	// A multi-byte character cut by the capture limit is not binary.
	cut := []byte("caf\xc3")
	if classifyBody("application/x-custom", cut, true) != kindText {
		t.Error("a truncated rune should not make a body binary")
	}
	if classifyBody("application/x-custom", cut, false) != kindBinary {
		t.Error("invalid UTF-8 in a complete body is binary")
	}
}

func TestBinaryBodiesAreUntouched(t *testing.T) {
	e := defaultEngine(t)
	text := "jane@acme.io 4111111111111111 415-555-2671"
	for _, c := range []struct{ ct, body string }{
		{"image/png", "\x89PNG\r\n\x1a\n" + text},
		{"application/octet-stream", "\x00\x01" + text},
		{"application/protobuf", text},
		{"application/pdf", "%PDF " + text},
		{"", "\x00" + text},
		{"application/x-custom", "\xff\xfe" + text},
	} {
		resp := &model.Response{Header: http.Header{"Content-Type": {c.ct}, "Content-Length": {strconv.Itoa(len(c.body))}}, Body: []byte(c.body), BodySize: int64(len(c.body))}
		if err := e.Response(resp); err != nil {
			t.Fatal(err)
		}
		if string(resp.Body) != c.body || resp.BodySize != int64(len(c.body)) {
			t.Errorf("%s body was modified", c.ct)
		}
	}
}

func TestCompressedBodiesAreUntouched(t *testing.T) {
	e := defaultEngine(t)
	doc := `{"email":"jane@acme.io"}`
	gz := gzipBytes(t, doc)

	// The Content-Encoding header survived: the capture could not decode it.
	resp := &model.Response{
		Header: http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {"gzip"}},
		Body:   gz, BodySize: int64(len(gz)),
	}
	if err := e.Response(resp); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resp.Body, gz) {
		t.Error("gzip body with a Content-Encoding header was modified")
	}

	// ContentEncoding is set and the bytes are still compressed.
	resp = &model.Response{
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   gz, BodySize: int64(len(gz)), ContentEncoding: "gzip",
	}
	if err := e.Response(resp); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resp.Body, gz) {
		t.Error("gzip bytes with ContentEncoding set were modified")
	}

	// zlib / deflate and zstd magic numbers are recognised too.
	for _, magic := range [][]byte{{0x78, 0x9c, 1, 2, 3}, {0x28, 0xb5, 0x2f, 0xfd, 1, 2}} {
		body := append(append([]byte{}, magic...), []byte("jane@acme.io")...)
		resp = &model.Response{Header: http.Header{}, Body: body, BodySize: int64(len(body)), ContentEncoding: "deflate"}
		if err := e.Response(resp); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(resp.Body, body) {
			t.Errorf("compressed body %x was modified", magic)
		}
	}

	// The proxy stores decoded bodies and records the original encoding: that
	// body is plain JSON and must be anonymized.
	resp = &model.Response{
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   []byte(doc), BodySize: int64(len(doc)), ContentEncoding: "gzip",
	}
	if err := e.Response(resp); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(resp.Body), "acme.io") || !json.Valid(resp.Body) {
		t.Errorf("decoded body with ContentEncoding=gzip leaked: %s", resp.Body)
	}

	// identity means no compression at all.
	for _, enc := range []string{"identity", "Identity"} {
		resp = &model.Response{Header: http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {enc}}, Body: []byte(doc)}
		if err := e.Response(resp); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(resp.Body), "acme.io") {
			t.Errorf("Content-Encoding %s: body not anonymized", enc)
		}
	}
}

func TestBodySizeAndContentLength(t *testing.T) {
	e := defaultEngine(t)
	doc := `{"email":"jane@acme.io","pad":"` + strings.Repeat("x", 40) + `"}`
	run := func(truncated bool, size int64, cl string) *model.Response {
		resp := &model.Response{Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(doc), BodySize: size, BodyTruncated: truncated}
		if cl != "" {
			resp.Header.Set("Content-Length", cl)
		}
		if err := e.Response(resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	r := run(false, int64(len(doc)), strconv.Itoa(len(doc)))
	if r.BodySize != int64(len(r.Body)) || r.Header.Get("Content-Length") != strconv.Itoa(len(r.Body)) {
		t.Errorf("complete body: size=%d cl=%s len=%d", r.BodySize, r.Header.Get("Content-Length"), len(r.Body))
	}
	if int64(len(r.Body)) == int64(len(doc)) {
		t.Fatal("test needs the fake to change the length")
	}

	// No Content-Length header: none is added.
	r = run(false, int64(len(doc)), "")
	if _, ok := r.Header["Content-Length"]; ok {
		t.Error("Content-Length was added")
	}

	// A truncated capture: the wire size moves by the delta, never below the prefix.
	r = run(true, 100_000, "100000")
	delta := int64(len(r.Body)) - int64(len(doc))
	if r.BodySize != 100_000+delta || r.Header.Get("Content-Length") != strconv.FormatInt(100_000+delta, 10) {
		t.Errorf("truncated: size=%d cl=%s delta=%d", r.BodySize, r.Header.Get("Content-Length"), delta)
	}
	r = run(true, 1, "")
	if r.BodySize < int64(len(r.Body)) {
		t.Errorf("BodySize %d is smaller than the captured body %d", r.BodySize, len(r.Body))
	}

	// Nothing changed: size and header are left alone, whatever they say.
	clean := &model.Response{Header: http.Header{"Content-Type": {"text/plain"}, "Content-Length": {"999"}}, Body: []byte("hello"), BodySize: 999}
	if err := e.Response(clean); err != nil {
		t.Fatal(err)
	}
	if clean.BodySize != 999 || clean.Header.Get("Content-Length") != "999" {
		t.Errorf("unchanged body: size=%d cl=%s", clean.BodySize, clean.Header.Get("Content-Length"))
	}

	// A non-canonical Content-Length key is found too.
	lower := "content-" + "length"
	resp := &model.Response{Header: rawHeader("Content-Type", "application/json", lower, "1"), Body: []byte(doc), BodySize: 1}
	if err := e.Response(resp); err != nil {
		t.Fatal(err)
	}
	if resp.Header[lower][0] != strconv.Itoa(len(resp.Body)) {
		t.Errorf("content-length = %v", resp.Header[lower])
	}

	// Requests behave the same way.
	req := &model.Request{Header: http.Header{"Content-Type": {"application/json"}, "Content-Length": {strconv.Itoa(len(doc))}}, Body: []byte(doc), BodySize: int64(len(doc))}
	if err := e.Request(req); err != nil {
		t.Fatal(err)
	}
	if req.BodySize != int64(len(req.Body)) || req.Header.Get("Content-Length") != strconv.Itoa(len(req.Body)) {
		t.Errorf("request: size=%d cl=%s len=%d", req.BodySize, req.Header.Get("Content-Length"), len(req.Body))
	}
}

func TestNDJSON(t *testing.T) {
	e := mustEngine(t, testProfile(rl(model.TargetJSON, "$.token", model.StrategyRedact)))
	fakeJane := e.k.fake(model.FakeEmail, "jane@acme.io")
	body := `{"email":"jane@acme.io","token":"t1"}` + "\n" +
		`{"clean":true}` + "\n" +
		"\n" +
		`not json jane@acme.io` + "\n" +
		`  {"token":"t2"}  ` + "\r\n" +
		`{"e":"jane@acme.io"}` // no final newline
	got := anonResp(t, e, "application/x-ndjson", body)
	want := `{"email":"` + fakeJane + `","token":"[REDACTED]"}` + "\n" +
		`{"clean":true}` + "\n" +
		"\n" +
		`not json ` + fakeJane + "\n" +
		`{"token":"[REDACTED]"}` + "\r\n" +
		`{"e":"` + fakeJane + `"}`
	if got != want {
		t.Errorf("\n got:  %q\n want: %q", got, want)
	}

	// Lines that need no change are returned byte for byte.
	unchanged := " {\"a\" : 1}\n{\"b\":2}\n"
	if got := anonResp(t, e, "application/x-ndjson", unchanged); got != unchanged {
		t.Errorf("unchanged NDJSON modified: %q", got)
	}

	// Concatenated documents under a plain JSON type are handled line by line.
	got = anonJSON(t, e, `{"a":"jane@acme.io"}`+"\n"+`{"b":"jane@acme.io"}`)
	if strings.Contains(got, "acme.io") || strings.Count(got, fakeJane) != 2 {
		t.Errorf("concatenated JSON: %q", got)
	}

	// A truncated NDJSON stream: the last (cut) line is handled leniently.
	resp := &model.Response{Header: http.Header{"Content-Type": {"application/x-ndjson"}}, Body: []byte(`{"token":"a"}` + "\n" + `{"token":"b","x":"jane@ac`), BodyTruncated: true, BodySize: 1000}
	if err := e.Response(resp); err != nil {
		t.Fatal(err)
	}
	if got := string(resp.Body); got != `{"token":"[REDACTED]"}`+"\n"+`{"token":"[REDACTED]","x":"jane@ac` {
		t.Errorf("truncated NDJSON: %q", got)
	}
}

func TestFormBodies(t *testing.T) {
	e := defaultEngine(t)
	fake := e.k.fake(model.FakeEmail, "jane@acme.io")
	body := "z=1&email=jane%40acme.io&x=a%2Fb+c&card=4111+1111+1111+1111&&flag&n="
	got := anonReq(t, e, "application/x-www-form-urlencoded; charset=UTF-8", body)
	b := string(got.Body)
	if !strings.HasPrefix(b, "z=1&email="+fake+"&x=a%2Fb+c&card=") || !strings.HasSuffix(b, "&&flag&n=") {
		t.Errorf("form body: %q", b)
	}
	if strings.Contains(b, "4111") {
		t.Errorf("card leaked: %q", b)
	}
	if got.BodySize != int64(len(got.Body)) {
		t.Errorf("BodySize = %d", got.BodySize)
	}

	// Form rules remove, hash by field name, and leave other fields alone.
	e = mustEngine(t, bareProfile(
		rl(model.TargetForm, "password", model.StrategyRemove),
		rl(model.TargetForm, "user", model.StrategyHash),
	))
	got = anonReq(t, e, "application/x-www-form-urlencoded", "user=bob&password=hunter2&remember=on")
	if want := "user=" + e.k.hashToken("bob") + "&remember=on"; string(got.Body) != want {
		t.Errorf("form rules: %q, want %q", got.Body, want)
	}

	// A form rule does not touch a query parameter of the same name.
	ex := newExchange()
	ex.Request.RawQuery = "password=hunter2"
	if err := e.Exchange(ex); err != nil {
		t.Fatal(err)
	}
	if ex.Request.RawQuery != "password=hunter2" {
		t.Errorf("form rule changed the query: %q", ex.Request.RawQuery)
	}
}

func TestTextBodies(t *testing.T) {
	e := defaultEngine(t)
	cases := []struct{ ct, in string }{
		{"text/plain", "Dear jane@acme.io, call 415-555-2671 from 192.168.1.10. SSN 123-45-6789, card 4111 1111 1111 1111."},
		{"text/html", `<p class="x">Mail <a href="mailto:jane@acme.io">jane@acme.io</a></p>`},
		{"application/xml", `<user><email>jane@acme.io</email><phone>+14155552671</phone></user>`},
		{"", "plain text with jane@acme.io inside"},
		{"text/csv", "name,email\nJane,jane@acme.io\n"},
		{"multipart/form-data; boundary=zz", "--zz\r\nContent-Disposition: form-data; name=\"email\"\r\n\r\njane@acme.io\r\n--zz--\r\n"},
		{"application/x-custom", "free text jane@acme.io"},
	}
	for _, c := range cases {
		got := anonResp(t, e, c.ct, c.in)
		for _, leak := range []string{"acme.io", "415-555-2671", "192.168.1.10", "123-45-6789", "4111 1111", "+14155552671"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: %q leaked in %q", c.ct, leak, got)
			}
		}
		// Surrounding text is preserved.
		if strings.HasPrefix(c.in, "<") && !strings.HasPrefix(got, c.in[:3]) {
			t.Errorf("%s: markup changed: %q", c.ct, got)
		}
	}
	if got := anonResp(t, e, "text/html", `<a href="mailto:jane@acme.io">x</a>`); !strings.HasPrefix(got, `<a href="mailto:`) || !strings.HasSuffix(got, `">x</a>`) {
		t.Errorf("markup around the address changed: %q", got)
	}

	// Non-UTF-8 text in a declared text type passes through with the PII removed.
	got := anonResp(t, e, "text/plain; charset=iso-8859-1", "caf\xe9 jane@acme.io")
	if strings.Contains(got, "acme.io") || !strings.HasPrefix(got, "caf\xe9 ") {
		t.Errorf("latin-1 text: %q", got)
	}

	// A truncated text body is processed up to the cut.
	resp := &model.Response{Header: http.Header{"Content-Type": {"text/plain"}}, Body: []byte("mail jane@acme.io then caf\xc3"), BodyTruncated: true, BodySize: 500}
	if err := e.Response(resp); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(resp.Body), "acme.io") || !strings.HasSuffix(string(resp.Body), "caf\xc3") {
		t.Errorf("truncated text: %q", resp.Body)
	}
}

func TestEmptyAndTinyBodies(t *testing.T) {
	e := defaultEngine(t)
	for _, ct := range []string{"", "application/json", "text/plain", "application/x-www-form-urlencoded", "application/x-ndjson"} {
		for _, body := range []string{"", " ", "\n", "a", "{", "[", "\"", "=", "&", "%"} {
			got := anonResp(t, e, ct, body)
			if got != body {
				t.Errorf("%q %q -> %q", ct, body, got)
			}
		}
	}
}

func TestJSONRequestEndToEnd(t *testing.T) {
	e := mustEngine(t, testProfile(
		rl(model.TargetJSON, "$..customer_id", model.StrategyHash),
		rl(model.TargetJSON, "$.card.last4", model.StrategyMask),
	))
	ex := newExchange()
	ex.Request.Method = "POST"
	ex.Request.Header.Set("Content-Type", "application/json")
	ex.Request.Body = []byte(`{"customer_id":"C-1","card":{"last4":"1111","n":"4111 1111 1111 1111"},"email":"jane@acme.io"}`)
	ex.Request.BodySize = int64(len(ex.Request.Body))
	ex.Response.Header.Set("Content-Type", "application/json")
	ex.Response.Body = []byte(`{"customer_id":"C-1","ok":true}`)
	ex.Response.BodySize = int64(len(ex.Response.Body))
	if err := e.Exchange(ex); err != nil {
		t.Fatal(err)
	}
	if !ex.Anonymized {
		t.Error("Anonymized not set")
	}
	var req, resp map[string]any
	if err := json.Unmarshal(ex.Request.Body, &req); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ex.Response.Body, &resp); err != nil {
		t.Fatal(err)
	}
	tok := e.k.hashToken("C-1")
	if req["customer_id"] != tok || resp["customer_id"] != tok {
		t.Errorf("the same customer must map to the same token in request and response: %v / %v", req["customer_id"], resp["customer_id"])
	}
	if req["card"].(map[string]any)["last4"] != "****" {
		t.Errorf("last4 = %v", req["card"])
	}
	if resp["ok"] != true {
		t.Errorf("response ok = %v", resp["ok"])
	}
}
