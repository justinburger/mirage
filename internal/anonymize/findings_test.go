package anonymize

import (
	"encoding/base64"
	"mime/quotedprintable"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

// ---- default profile: names and addresses ---------------------------------

// Shapes of examples/demo-api.
const (
	demoUser    = `{"id":3,"name":"Nora Lee","email":"nora.lee@acme.io","phone":"(447) 555-8926","address":{"street":"902 Pine Rd","city":"Boise","state":"ID","zip":"83700"},"last4":"4242","created_at":"2026-01-02T03:04:05Z"}`
	demoProduct = `{"id":7,"sku":"SKU-0007","name":"Trail Boots","category":"shoes","price_cents":8999,"stock":4,"rating":4.5}`
)

func TestDefaultProfileFakesNamesAndAddressesOfUsers(t *testing.T) {
	e := builtinEngine(t)
	sides(t, func(t *testing.T, anon func(*Engine, string, string) string) {
		out := anon(e, "application/json", demoUser)
		for _, leak := range []string{"Nora Lee", "902 Pine Rd", "Boise", "83700", "nora.lee@acme.io", "555-8926"} {
			if strings.Contains(out, leak) {
				t.Errorf("%q survived: %s", leak, out)
			}
		}
		// Structure and unrelated members are kept.
		if jsonAt(t, out, "id") != float64(3) || jsonAt(t, out, "last4") != "4242" || jsonAt(t, out, "created_at") != "2026-01-02T03:04:05Z" {
			t.Errorf("other members changed: %s", out)
		}
		addr, _ := jsonAt(t, out, "address").(map[string]any)
		street, _ := addr["street"].(string)
		if len(addr) != 4 || len(street) != len("902 Pine Rd") || len(addr["zip"].(string)) != 5 {
			t.Errorf("the fake address lost its shape: %v", addr)
		}
		// Deterministic: the same person is the same fake everywhere.
		if again := anon(e, "application/json", demoUser); again != out {
			t.Errorf("not deterministic:\n%s\n%s", out, again)
		}
		name, _ := jsonAt(t, out, "name").(string)
		if name != e.k.fake(model.FakeName, "Nora Lee") {
			t.Errorf("name = %q, want the consistent fake name", name)
		}
	})
}

func TestDefaultProfileKeepsProductAndFileNames(t *testing.T) {
	e := builtinEngine(t)
	sides(t, func(t *testing.T, anon func(*Engine, string, string) string) {
		for _, body := range []string{
			demoProduct,
			`{"id":"o1","items":[{"product_id":7,"name":"Trail Boots","quantity":2}],"currency":"USD"}`,
			`{"name":"report.pdf","size":1024}`,
			`{"plan":{"name":"Pro","price":10}}`,
		} {
			out := anon(e, "application/json", body)
			if !sameJSON(out, body) {
				t.Errorf("a non-person name was changed:\n in: %s\nout: %s", body, out)
			}
		}
	})
}

func TestDefaultProfileListsAndNestedCustomers(t *testing.T) {
	e := builtinEngine(t)
	body := `{"page":1,"users":[` + demoUser + `,` + strings.Replace(demoUser, "Nora Lee", "Ava Johnson", 1) + `],"products":[` + demoProduct + `]}`
	out := anonResp(t, e, "application/json", body)
	for _, leak := range []string{"Nora Lee", "Ava Johnson", "Pine Rd"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q survived: %s", leak, out)
		}
	}
	if !strings.Contains(out, "Trail Boots") {
		t.Errorf("the product name was changed: %s", out)
	}
	// Customer and cardholder keys are people whatever the siblings.
	out = anonResp(t, e, "application/json", `{"customer_name":"Ava Johnson","cardholder":"A JOHNSON","shipping":{"line1":"1 Main St","line2":"Apt 4","postal_code":"98604"}}`)
	for _, leak := range []string{"Ava Johnson", "A JOHNSON", "1 Main St", "Apt 4", "98604"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q survived: %s", leak, out)
		}
	}
	// A request body (PUT /api/users/3) is covered too.
	put := anonReq(t, e, "application/json", `{"name":"Hazel Thompson","address":{"street":"4511 Oak Ave","city":"Boise","state":"ID","zip":"83701"}}`)
	for _, leak := range []string{"Hazel Thompson", "4511 Oak Ave", "Boise", "83701"} {
		if strings.Contains(string(put.Body), leak) {
			t.Errorf("%q survived in the request: %s", leak, put.Body)
		}
	}
}

func TestDefaultProfileNameParametersInQueryFormAndHeaders(t *testing.T) {
	e := builtinEngine(t)
	req := &model.Request{Method: "GET", Path: "/api/users", RawQuery: "first_name=Ava&last_name=Johnson&street=9668+Elm+St&page=2&q=boots",
		Header: http.Header{"X-Customer-Name": {"Ava Johnson"}, "X-User-Name": {"ava"}}}
	if err := e.Request(req); err != nil {
		t.Fatal(err)
	}
	q, _ := url.ParseQuery(req.RawQuery)
	if q.Get("first_name") == "Ava" || q.Get("last_name") == "Johnson" || q.Get("street") == "9668 Elm St" {
		t.Errorf("query not anonymized: %s", req.RawQuery)
	}
	if q.Get("page") != "2" || q.Get("q") != "boots" {
		t.Errorf("ordinary parameters changed: %s", req.RawQuery)
	}
	if req.Header.Get("X-Customer-Name") == "Ava Johnson" || req.Header.Get("X-User-Name") == "ava" {
		t.Errorf("name headers not anonymized: %v", req.Header)
	}
	form := anonReq(t, e, "application/x-www-form-urlencoded", "full_name=Ava+Johnson&address=1+Main+St&quantity=2")
	if strings.Contains(string(form.Body), "Johnson") || strings.Contains(string(form.Body), "Main") || !strings.Contains(string(form.Body), "quantity=2") {
		t.Errorf("form body: %s", form.Body)
	}
}

func TestDefaultProfileXMLCredentialElements(t *testing.T) {
	e := builtinEngine(t)
	in := `<?xml version="1.0"?><login><user>jane</user><password>hunter2hunter2</password><Token>opaque-token-abcdef123456</Token>` +
		`<soap:Password>s3cret</soap:Password><note>keep me</note></login>`
	for _, ct := range []string{"application/xml", "text/xml", "text/plain", ""} {
		out := anonResp(t, e, ct, in)
		for _, leak := range []string{"hunter2hunter2", "opaque-token-abcdef123456", "s3cret"} {
			if strings.Contains(out, leak) {
				t.Errorf("[%s] %q survived: %s", ct, leak, out)
			}
		}
		if !strings.Contains(out, "<user>jane</user>") || !strings.Contains(out, "<note>keep me</note>") || !strings.Contains(out, "<password>[REDACTED]</password>") {
			t.Errorf("[%s] unrelated elements changed: %s", ct, out)
		}
	}
}

// ---- default profile: query parameters and headers ------------------------

func TestDefaultProfileMoreCredentialQueryParameters(t *testing.T) {
	e := builtinEngine(t)
	raw := "client_secret=shhhsecret&refresh_token=rt1&id_token=it1&X-Amz-Signature=deadbeefdeadbeef&X-Amz-Security-Token=FwoGZXIvYXdz&sid=abcd&auth=xyz&pwd=hunter2" +
		"&state=open&code=US&page=3"
	req := &model.Request{Method: "GET", Path: "/cb", RawQuery: raw, Header: http.Header{}}
	if err := e.Request(req); err != nil {
		t.Fatal(err)
	}
	q, err := url.ParseQuery(req.RawQuery)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"client_secret": "shhhsecret", "refresh_token": "rt1", "id_token": "it1", "X-Amz-Signature": "deadbeefdeadbeef",
		"X-Amz-Security-Token": "FwoGZXIvYXdz", "sid": "abcd", "auth": "xyz", "pwd": "hunter2",
	} {
		if got := q.Get(k); got != e.k.hashToken(v) {
			t.Errorf("%s = %q, want the hash token", k, got)
		}
	}
	// state and code are ordinary filters as often as credentials.
	if q.Get("state") != "open" || q.Get("code") != "US" || q.Get("page") != "3" {
		t.Errorf("ordinary parameters changed: %s", req.RawQuery)
	}
}

func TestDefaultProfileMoreRedactedHeaders(t *testing.T) {
	e := builtinEngine(t)
	in := http.Header{
		"Api-Key": {"opaque-key-1"}, "X-Amz-Security-Token": {"FwoGZXIvYXdz"}, "X-Session-Id": {"sess-123456"},
		"X-Xsrf-Token": {"xsrf-1"}, "X-Csrftoken": {"csrf-1"}, "X-Goog-Api-Key": {"AIzaSyOpaque"},
		"True-Client-Ip": {"203.0.113.9"}, "Cf-Connecting-Ip": {"203.0.113.10"}, "Accept": {"application/json"},
	}
	h := reqHeaders(t, e, in)
	for k, v := range map[string]string{
		"Api-Key": "opaque-key-1", "X-Amz-Security-Token": "FwoGZXIvYXdz", "X-Session-Id": "sess-123456", "X-Xsrf-Token": "xsrf-1",
		"X-Csrftoken": "csrf-1", "X-Goog-Api-Key": "AIzaSyOpaque", "True-Client-Ip": "203.0.113.9", "Cf-Connecting-Ip": "203.0.113.10",
	} {
		if got := h.Get(k); got != e.k.hashToken(v) {
			t.Errorf("%s = %q, want the hash token", k, got)
		}
	}
	if h.Get("Accept") != "application/json" {
		t.Errorf("Accept changed: %q", h.Get("Accept"))
	}
}

// ---- user agent ------------------------------------------------------------

func TestUserAgentEmailsAreAnonymized(t *testing.T) {
	e := defaultEngine(t)
	ua := "Mozilla/5.0 (compatible; Bot/1.0; +ava.johnson75@example.com) Chrome/120.0.0.0"
	for _, name := range []string{"User-Agent", "Accept-Language", "Server", "Etag"} {
		h := reqHeaders(t, e, http.Header{name: {ua}})
		got := h.Get(name)
		if strings.Contains(got, "ava.johnson75@example.com") {
			t.Errorf("%s kept the e-mail: %q", name, got)
		}
		if !strings.Contains(got, "Chrome/120.0.0.0") || !strings.HasPrefix(got, "Mozilla/5.0 (compatible; Bot/1.0; +") {
			t.Errorf("%s: the version strings must stay: %q", name, got)
		}
	}
	// Other secrets that do not look like versions are caught as well.
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyIn0.c2lnbmF0dXJlLXNpZ25hdHVyZQ"
	h := reqHeaders(t, e, http.Header{"User-Agent": {"app/2 token=" + jwt}})
	if strings.Contains(h.Get("User-Agent"), jwt) {
		t.Errorf("JWT survived in the user agent: %q", h.Get("User-Agent"))
	}
	// Numbers that look like addresses, cards or phones are still left alone.
	plain := "Mozilla/5.0 (Windows NT 10.0) Chrome/120.0.0.0 4111111111111111 (415) 555-2671"
	if got := reqHeaders(t, e, http.Header{"User-Agent": {plain}}).Get("User-Agent"); got != plain {
		t.Errorf("numeric strings in the user agent were rewritten: %q", got)
	}
}

// ---- multipart file parts ---------------------------------------------------

func mpFilePartOutput(t *testing.T, e *Engine, p tpart) string {
	t.Helper()
	in := buildMP(mpBoundary, "\r\n", p)
	req := mpRequest(t, e, mpCT(mpBoundary), in, false, 0)
	parts := readMP(t, string(req.Body), mpBoundary)
	if len(parts) != 1 {
		t.Fatalf("%d parts: %q", len(parts), req.Body)
	}
	return parts[0].body
}

func TestMultipartTextFileParts(t *testing.T) {
	e := defaultEngine(t)
	const text = "Customer Ava Johnson card 4111111111111111 phone (447) 555-8926 ssn 123-45-6789 jane.roe@acme.io"
	for _, c := range []struct{ ct, filename string }{
		{"text/plain", "n.txt"}, {"text/csv", "customers.csv"}, {"application/octet-stream", "export.dat"}, {"", "noext"},
		{"application/xml", "c.xml"}, {"text/plain; charset=utf-8", "u.txt"},
	} {
		got := mpFilePartOutput(t, e, tpart{name: "file", filename: c.filename, ct: c.ct, body: text})
		for _, leak := range []string{"4111111111111111", "555-8926", "123-45-6789", "jane.roe@acme.io"} {
			if strings.Contains(got, leak) {
				t.Errorf("[%s] %q survived in the file part: %q", c.ct, leak, got)
			}
		}
		if !strings.HasPrefix(got, "Customer Ava Johnson card ") {
			t.Errorf("[%s] the text around the matches changed: %q", c.ct, got)
		}
	}
	// JSON files get JSON rules.
	got := mpFilePartOutput(t, builtinEngine(t), tpart{name: "file", filename: "c.json", ct: "application/json", body: `{"password":"hunter2","email":"jane.roe@acme.io"}`})
	if !strings.Contains(got, `"password":"[REDACTED]"`) || strings.Contains(got, "acme.io") {
		t.Errorf("JSON file part: %s", got)
	}
	// Unchanged text parts stay byte for byte.
	in := buildMP(mpBoundary, "\r\n", tpart{name: "file", filename: "n.txt", ct: "text/plain", body: "nothing here"})
	if req := mpRequest(t, e, mpCT(mpBoundary), in, false, 0); string(req.Body) != in {
		t.Errorf("an unchanged body was rewritten: %q", req.Body)
	}
}

func TestMultipartBinaryFilePartsStayOpaque(t *testing.T) {
	keep := defaultEngine(t)
	fillP := testProfile()
	fillP.OpaqueBodies = model.OpaqueFill
	fill := mustEngine(t, fillP)
	for _, c := range []tpart{
		{name: "f", filename: "a.png", ct: "image/png", body: "\x89PNG\r\n\x1a\n jane@acme.io"},
		{name: "f", filename: "a.pdf", ct: "application/pdf", body: "%PDF-1.4 jane@acme.io"},
		{name: "f", filename: "a.bin", ct: "application/octet-stream", body: "\x00\x01\x02 jane@acme.io"},
		{name: "f", filename: "a.bin", ct: "", body: "\xff\xfe\x00 jane@acme.io"},
		{name: "f", filename: "a.zip", ct: "application/zip", body: "PK\x03\x04 jane@acme.io"},
	} {
		if got := mpFilePartOutput(t, keep, c); got != c.body {
			t.Errorf("[%s] keep changed a binary part: %q", c.ct, got)
		}
		got := mpFilePartOutput(t, fill, c)
		if got == c.body || len(got) != len(c.body) {
			t.Errorf("[%s] fill did not fill a binary part of the same length: %q", c.ct, got)
		}
	}
}

func TestMultipartEncodedFileParts(t *testing.T) {
	e := defaultEngine(t)
	const text = "Customer card 4111111111111111 mail jane.roe@acme.io\r\nsecond line 415-555-2671\r\n"

	// base64 text, wrapped at 76 characters like mail clients do.
	b64 := base64.StdEncoding.EncodeToString([]byte(strings.Repeat(text, 3)))
	var wrapped []string
	for len(b64) > 0 {
		n := min(76, len(b64))
		wrapped = append(wrapped, b64[:n])
		b64 = b64[n:]
	}
	got := mpFilePartOutput(t, e, tpart{name: "file", filename: "c.txt", ct: "text/plain",
		extra: "Content-Transfer-Encoding: base64\r\n", body: strings.Join(wrapped, "\r\n")})
	dec, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(got, "\r\n", ""))
	if err != nil {
		t.Fatalf("output is not base64: %v\n%q", err, got)
	}
	if strings.Contains(string(dec), "4111111111111111") || strings.Contains(string(dec), "acme.io") || !strings.Contains(string(dec), "Customer card ") {
		t.Errorf("decoded base64 part: %q", dec)
	}
	for _, line := range strings.Split(got, "\r\n") {
		if len(line) > 76 {
			t.Errorf("re-encoded line is %d characters: %q", len(line), line)
		}
	}

	// base64 binary: kept.
	bin := base64.StdEncoding.EncodeToString([]byte("\x89PNG\x00\x01\x02 jane@acme.io"))
	if got := mpFilePartOutput(t, e, tpart{name: "file", filename: "a.png", ct: "image/png", extra: "Content-Transfer-Encoding: base64\r\n", body: bin}); got != bin {
		t.Errorf("a base64 image was changed: %q", got)
	}
	// base64 that does not decode: kept (opaque).
	if got := mpFilePartOutput(t, e, tpart{name: "file", filename: "a.txt", ct: "text/plain", extra: "Content-Transfer-Encoding: base64\r\n", body: "!!!not base64 jane@acme.io"}); got != "!!!not base64 jane@acme.io" {
		t.Errorf("undecodable base64 part changed: %q", got)
	}

	// quoted-printable text.
	var qp strings.Builder
	w := quotedprintable.NewWriter(&qp)
	_, _ = w.Write([]byte("Gr\xc3\xbc\xc3\x9fe, mail jane.roe@acme.io please\r\n"))
	_ = w.Close()
	got = mpFilePartOutput(t, e, tpart{name: "file", filename: "m.txt", ct: "text/plain; charset=utf-8",
		extra: "Content-Transfer-Encoding: quoted-printable\r\n", body: strings.TrimRight(qp.String(), "\r\n")})
	dq, err := quotedprintable.NewReader(strings.NewReader(got)).Read(make([]byte, 0))
	_ = dq
	_ = err
	raw := make([]byte, 4096)
	n, _ := quotedprintable.NewReader(strings.NewReader(got)).Read(raw)
	if s := string(raw[:n]); strings.Contains(s, "acme.io") || !strings.Contains(s, "Grüße, mail ") {
		t.Errorf("decoded quoted-printable part: %q", s)
	}
}

// ---- content-type gating ----------------------------------------------------

const credentialJSON = `{"email":"jane.roe@acme.io","password":"hunter2hunter2","access_token":"opaque-token-abcdef123456","first_name":"Jane","session_id":"sess-123456"}`

func TestCredentialRulesDoNotDependOnTheDeclaredType(t *testing.T) {
	e := builtinEngine(t)
	for _, ct := range []string{
		"application/json", "text/plain", "text/plain; charset=utf-8", "text/html", "application/octet-stream",
		"application/x-www-form-urlencoded", "application/x-amz-json-1.1", "application/vnd.acme.customer.v2",
		"application/vnd.acme.customer.v2.json", "application/x-custom", "", "application/jose+json",
	} {
		sides(t, func(t *testing.T, anon func(*Engine, string, string) string) {
			out := anon(e, ct, credentialJSON)
			for _, leak := range []string{"hunter2hunter2", "opaque-token-abcdef123456", "sess-123456", "jane.roe@acme.io", `"first_name":"Jane"`} {
				if strings.Contains(out, leak) {
					t.Errorf("[%s] %q survived: %s", ct, leak, out)
				}
			}
			if !strings.Contains(out, `"password":"[REDACTED]"`) || !strings.Contains(out, e.k.hashToken("opaque-token-abcdef123456")) {
				t.Errorf("[%s] rules did not apply: %s", ct, out)
			}
		})
	}
}

func TestMislabelledBodiesThatAreNotJSONStillGetDetectors(t *testing.T) {
	e := builtinEngine(t)
	for _, c := range []struct{ ct, body string }{
		{"text/plain", "[INFO] started for jane.roe@acme.io"},
		{"text/plain", "{not json} jane.roe@acme.io"},
		{"application/octet-stream", "[x] jane.roe@acme.io"},
		{"application/x-www-form-urlencoded", "{weird=1&email=jane.roe%40acme.io"},
		{"application/vnd.acme.export", "jane.roe@acme.io;4111111111111111"},
	} {
		out := anonResp(t, e, c.ct, c.body)
		if strings.Contains(out, "acme.io") || strings.Contains(out, "4111111111111111") || strings.Contains(out, "acme%2E") {
			t.Errorf("[%s] %q survived: %q", c.ct, c.body, out)
		}
	}
	// A real form is still a form.
	out := anonResp(t, e, "application/x-www-form-urlencoded", "password=hunter2&page=2&email=jane.roe%40acme.io")
	if !strings.Contains(out, "password=%5BREDACTED%5D") && !strings.Contains(out, "password=[REDACTED]") || !strings.Contains(out, "page=2") || strings.Contains(out, "acme") {
		t.Errorf("form body: %q", out)
	}
	// A truncated capture of JSON under a wrong type is rewritten as far as it goes.
	resp := newExchange().Response
	resp.Header.Set("Content-Type", "text/plain")
	resp.Body = []byte(`{"password":"hunter2hunter2","note":"a long no`)
	resp.BodySize = 4000
	resp.BodyTruncated = true
	if err := e.Response(&resp); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(resp.Body), "hunter2hunter2") {
		t.Errorf("truncated body: %s", resp.Body)
	}
}

// Vendor types are no longer assumed to be binary, but binary ones still are.
func TestVendorTypedBinaryBodiesAreUntouched(t *testing.T) {
	e := builtinEngine(t)
	for _, c := range []struct{ ct, body string }{
		{"application/vnd.acme.blob", "\x00\x01\x02 jane@acme.io"},
		{"application/vnd.ms-excel", "jane@acme.io 4111111111111111"},
		{"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "PK jane@acme.io"},
	} {
		if got := anonResp(t, e, c.ct, c.body); got != c.body {
			t.Errorf("[%s] a binary body was rewritten: %q", c.ct, got)
		}
	}
}
