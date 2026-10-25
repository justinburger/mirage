package anonymize

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// TestPathologicalInputsStayLinear feeds the engine bodies built to make a
// naive scanner quadratic. The limits are generous (a linear implementation
// takes well under a second on each); a quadratic one takes minutes.
func TestPathologicalInputsStayLinear(t *testing.T) {
	p := testProfile(rl(model.TargetRegex, `a{1,3}b`, model.StrategyRedact))
	p.Detectors = append([]model.Detector{}, model.AllDetectors...) // uuid too
	e := mustEngine(t, p)

	const n = 200_000
	cases := []struct {
		name string
		body string
	}{
		{"percent-encoded at signs, no real one", strings.Repeat("%40", n/3)},
		{"at signs only", strings.Repeat("@", n)},
		{"a hundred thousand e-mail addresses", strings.Repeat("a@b.co ", n/7)},
		{"a hundred thousand encoded addresses", strings.Repeat("a%40b.co ", n/9)},
		{"one-digit groups that look like a card chain", strings.Repeat("1 ", n)},
		{"dashed digit groups", strings.Repeat("1-", n)},
		{"a single endless digit run", strings.Repeat("7", n)},
		{"many phone-like fragments", strings.Repeat("(555) 555-", n/10)},
		{"many IPv4-like fragments", strings.Repeat("1.2.3.", n/6)},
		{"many IPv6-like fragments", strings.Repeat("::1:", n/4)},
		{"colons and hex", strings.Repeat("a:", n)},
		{"jwt-like", strings.Repeat("ey.", n/3)},
		{"jwt-like tokens", strings.Repeat("eyJ.eyJ.", n/8)},
		{"aws-like", strings.Repeat("AKIA", n/4)},
		{"iban-like", strings.Repeat("GB82", n/4)},
		{"uuid-like", strings.Repeat("12345678-", n/9)},
		{"ssn-like", strings.Repeat("123-45-", n/7)},
		{"many real card numbers", strings.Repeat("4111111111111111 ", n/17)},
		{"many real ssn", strings.Repeat("123-45-6789 ", n/12)},
		{"regex hot spots", strings.Repeat("aab", n/3)},
		{"nothing", strings.Repeat("x", n)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := &model.Response{Header: http.Header{"Content-Type": {"text/plain"}}, Body: []byte(c.body), BodySize: int64(len(c.body))}
			start := time.Now()
			if err := e.Response(resp); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Errorf("%d bytes took %v", len(c.body), d)
			}
		})
	}

	// The same patterns inside JSON strings, keys, query strings and paths.
	for _, frag := range []string{"%40", "@", "1 ", "a@b.co ", "(555) 555-"} {
		str := strings.Repeat(frag, n/len(frag))
		ex := newExchange()
		ex.Request.Path = "/" + str
		ex.Request.RawQuery = "k=" + str
		ex.Request.Header.Set("Content-Type", "application/json")
		ex.Request.Body = []byte(`{"` + str + `":"` + str + `"}`)
		start := time.Now()
		if err := e.Exchange(ex); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("fragment %q: exchange took %v", frag, d)
		}
	}
}

func TestManyMatchesAreAllReplaced(t *testing.T) {
	e := defaultEngine(t)
	const n = 20_000
	body := strings.Repeat("mail user@example.org or call 415-555-2671, card 4111111111111111. ", n)
	got := anonResp(t, e, "text/plain", body)
	for _, leak := range []string{"user@example.org", "415-555-2671", "4111111111111111"} {
		if strings.Contains(got, leak) {
			t.Fatalf("%q leaked among %d repetitions", leak, n)
		}
	}
	if strings.Count(got, "@example.") != n {
		t.Errorf("replaced %d addresses, want %d", strings.Count(got, "@example."), n)
	}
}

// TestOrdinaryValuesAreLeftAlone guards against false positives: identifiers,
// timestamps, versions, hashes and numbers that merely look a little like PII
// must pass through the default profile unchanged.
func TestOrdinaryValuesAreLeftAlone(t *testing.T) {
	values := []string{
		"a94a8fe5ccb19ba61c4c0873d391e987982fbbd3", "2024-03-12T10:15:30.123Z", "2024-03-12", "12:30:45", "P3Y6M4DT12H30M5S",
		"#ff00ff", "v1.2.3", "1.2.3-beta.4", "3.14159265358979", "9007199254740993", "1698765432123", "1698765432",
		"0.0.1", "10:30", "192.168.1", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)", "application/json; charset=utf-8",
		"Wed, 21 Oct 2015 07:28:00 GMT", "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		"ISBN 978-3-16-148410-0", "ORD-2024-000123", "$1,234.56", "100%", "ref=abc123&x=987654321", "1234567890123456789",
		"9876543210", "123456789", "12-34-56", "2024-1-5", "555-1234", "(555) 1234", "5551234567", "00:1a:2b:3c:4d:5e",
		"fe80", "1:2", "a1b2c3d4-e5f6", "user_name", "node-12.4.0", "https://api.example.com/v1/users/123/orders?page=2&per_page=50",
		"build 20240312.1", "x86_64-linux-gnu", "1,2,3,4,5,6,7,8,9,10,11,12,13,14,15", "0x7fffffffe000", "1e10",
		"12345678901234567890", "1111 2222 3333 4444", "10.0.0.256", "AAAAAAAAAAAAAAAAAAAA", "GBP 1234 5678 9012 3456",
		"550e8400-e29b-41d4-a716-446655440000", // a UUID: off in the default profile
		"@scope/package@1.2.3", "foo@bar", "Springfield", "",
	}
	e := defaultEngine(t)
	for _, v := range values {
		if got := anonResp(t, e, "text/plain", v); got != v {
			t.Errorf("text %q was changed to %q", v, got)
		}
		doc := `{"v":` + mustJSON(v) + `}`
		if got := anonJSON(t, e, doc); got != doc {
			t.Errorf("JSON string %q was changed: %s", v, got)
		}
		ex := newExchange()
		ex.Request.Header.Set("X-Value", v)
		ex.Request.RawQuery = "v=" + escapeQuery(v)
		if err := e.Exchange(ex); err != nil {
			t.Fatal(err)
		}
		if ex.Request.Header.Get("X-Value") != v || (v != "" && ex.Request.RawQuery != "v="+escapeQuery(v)) {
			t.Errorf("header or query value %q was changed", v)
		}
	}

	// A four-part dotted number is indistinguishable from an IPv4 address, so
	// it is replaced. This is the one known trade-off for IPv4 detection.
	for _, v := range []string{"2.5.0.0", "see section 4.1.2.3"} {
		if got := anonResp(t, e, "text/plain", v); got == v {
			t.Errorf("%q should be treated as an address", v)
		}
	}

	// A realistic payload with identifiers of every kind stays byte for byte.
	payload := `{"id":"ch_1Mq3nZ2eZvKYlo2C","object":"charge","amount":2000,"created":1698765432,"currency":"usd",` +
		`"payment_method_details":{"card":{"brand":"visa","last4":"4242","exp_month":12,"exp_year":2030,"fingerprint":"Xt5EWLLDS7FJjR1c"}},` +
		`"balance_transaction":"txn_1Mq3nZ2eZvKYlo2C","receipt_url":"https://pay.stripe.com/receipts/payment/CAcaFwoVYWNjdF8x","sha":"a94a8fe5ccb19ba61c4c0873d391e987982fbbd3",` +
		`"version":"2023-10-16","metadata":{"order_id":"6735","cart":[1,2,3],"ratio":0.123456789,"big":9007199254740993}}`
	if got := anonJSON(t, e, payload); got != payload {
		t.Errorf("an ordinary payload was modified:\n%s", got)
	}
}
