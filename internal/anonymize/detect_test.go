package anonymize

import (
	"reflect"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

// matchesOf returns the matched substrings of one detector.
func matchesOf(det model.Detector, s string) []string {
	var out []string
	prefix := string(det) + ":"
	for _, m := range matchTexts(s) {
		if strings.HasPrefix(m, prefix) {
			out = append(out, strings.TrimPrefix(m, prefix))
		}
	}
	return out
}

type detCase struct {
	in   string
	want []string // matched substrings; nil = no match
}

func runDetCases(t *testing.T, det model.Detector, cases []detCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := matchesOf(det, c.in)
			if !reflect.DeepEqual(got, c.want) && !(len(got) == 0 && len(c.want) == 0) {
				t.Errorf("%s(%q) = %q, want %q", det, c.in, got, c.want)
			}
		})
	}
}

func TestDetectorTableMatchesModel(t *testing.T) {
	if len(detectors) != len(model.AllDetectors) {
		t.Fatalf("%d detectors, model lists %d", len(detectors), len(model.AllDetectors))
	}
	for i, name := range model.AllDetectors {
		if detectors[i].name != name {
			t.Errorf("detectors[%d] = %s, want %s (model.AllDetectors order)", i, detectors[i].name, name)
		}
	}
	seen := map[uint8]bool{}
	for _, di := range detectorOrder {
		seen[di] = true
	}
	if len(seen) != numDetectors {
		t.Errorf("detectorOrder covers %d of %d detectors", len(seen), numDetectors)
	}
}

func TestEmailDetector(t *testing.T) {
	runDetCases(t, model.DetectEmail, []detCase{
		{"jane@acme.io", []string{"jane@acme.io"}},
		{"a.b+tag@sub.example.co.uk", []string{"a.b+tag@sub.example.co.uk"}},
		{"mailto:jane@acme.io", []string{"jane@acme.io"}},
		{"<jane@acme.io>", []string{"jane@acme.io"}},
		{"contact jane_doe@acme.io.", []string{"jane_doe@acme.io"}},
		{"o'brien@acme.com", []string{"o'brien@acme.com"}},
		{"q=1&user=jane@acme.io&z=2", []string{"jane@acme.io"}},
		{"two a@b.co and c@d.org", []string{"a@b.co", "c@d.org"}},
		{"jane%40acme.io", []string{"jane%40acme.io"}},
		{"callback=https://x.test/?email=jane%40acme.io&n=1", []string{"jane%40acme.io"}},
		{"user@xn--80ak6aa92e.com", []string{"user@xn--80ak6aa92e.com"}},
		{"josé@müller.de", []string{"josé@müller.de"}},
		{"4111111111111111@x.com", []string{"4111111111111111@x.com"}},
		// near misses
		{"@handle", nil},
		{"jane@", nil},
		{"jane@localhost", nil},
		{"a@b", nil},
		{"a@b.c", nil},
		{"a@1.2", nil},
		{"user@@x.com", nil},
		{"price@5pm", nil},
		{"jane@-acme.io-", nil},
		{"no at sign here", nil},
	})
}

func TestEmailDetectorEncodedFlag(t *testing.T) {
	for _, c := range []struct {
		in  string
		enc bool
	}{{"x=jane%40acme.io", true}, {"x=jane@acme.io", false}} {
		st := classify(c.in)
		var cl claims
		cl.scan(c.in, candidates(1<<detEmail, &st))
		if cl.n != 1 || cl.list()[0].enc != c.enc {
			t.Fatalf("%q: matches=%+v, want one match with enc=%v", c.in, cl.list(), c.enc)
		}
	}
}

func TestClaimsOverflowAndOverlap(t *testing.T) {
	var c claims
	c.cur = detEmail
	// Far more matches than the inline storage holds, past the bitmap threshold.
	const total = 200
	for i := 0; i < total; i++ {
		if !c.add(i*10, i*10+5) {
			t.Fatalf("add %d failed", i)
		}
	}
	if c.n != total || len(c.list()) != total || c.bits == nil {
		t.Fatalf("n=%d list=%d bitmap=%v", c.n, len(c.list()), c.bits != nil)
	}
	for i, m := range c.list() {
		if m.start != i*10 || m.end != i*10+5 {
			t.Fatalf("match %d = %+v", i, m)
		}
	}
	// Overlaps are refused, touching ranges are not (on both code paths).
	if c.add(2, 3) || c.add(4, 12) || c.add(1992, 2001) || c.add(0, 5000) {
		t.Error("overlapping range accepted")
	}
	if !c.add(5, 10) || !c.add(1995, 2000) {
		t.Error("a range that only touches others was refused")
	}
	var small claims
	small.add(10, 20)
	if small.add(15, 25) || small.add(5, 11) || !small.add(20, 30) || !small.add(0, 10) {
		t.Error("overlap handling with inline storage")
	}

	// Matches found in priority order come out sorted by position.
	var d claims
	d.cur = detPhone
	d.add(50, 60)
	d.cur = detEmail
	d.add(10, 20)
	d.cur = detCard
	d.add(30, 40)
	d.scan("", 0)
	if l := d.list(); l[0].start != 10 || l[1].start != 30 || l[2].start != 50 {
		t.Errorf("not sorted: %+v", l)
	}
	var e claims
	for i := 49; i >= 0; i-- {
		e.add(i*3, i*3+2)
	}
	e.scan("", 0)
	for i, m := range e.list() {
		if m.start != i*3 {
			t.Fatalf("spilled matches not sorted at %d: %+v", i, m)
		}
	}
}

// makeLuhn returns a Luhn-valid number: prefix, filler digits, check digit.
func makeLuhn(prefix string, length int) string {
	b := []byte(prefix)
	for len(b) < length-1 {
		b = append(b, byte('0'+(len(b)*7+3)%10))
	}
	for d := byte('0'); d <= '9'; d++ {
		if luhnValid(string(b) + string(d)) {
			return string(b) + string(d)
		}
	}
	panic("no check digit")
}

func TestCardDetector(t *testing.T) {
	runDetCases(t, model.DetectCreditCard, []detCase{
		{"4111111111111111", []string{"4111111111111111"}},
		{"4111 1111 1111 1111", []string{"4111 1111 1111 1111"}},
		{"4111-1111-1111-1111", []string{"4111-1111-1111-1111"}},
		{"card 4111111111111111 ok", []string{"4111111111111111"}},
		{"card=4111111111111111&x=1", []string{"4111111111111111"}},
		{"378282246310005", []string{"378282246310005"}},              // amex 15
		{"3782 822463 10005", []string{"3782 822463 10005"}},          // amex 4-6-5
		{"3056 930902 5904", []string{"3056 930902 5904"}},            // diners 4-6-4
		{"30569309025904", []string{"30569309025904"}},                // diners 14
		{"5555555555554444", []string{"5555555555554444"}},            // mastercard
		{"6011111111111117", []string{"6011111111111117"}},            // discover
		{"4222222222222", []string{"4222222222222"}},                  // visa 13
		{makeLuhn("4", 19), []string{makeLuhn("4", 19)}},              // 19 digits
		{"2024-01-15 4111111111111111", []string{"4111111111111111"}}, // card after a date
		{"4111111111111111 2024-01-15", []string{"4111111111111111"}},
		{"a 4111 1111 1111 1111, b 5555 5555 5555 4444", []string{"4111 1111 1111 1111", "5555 5555 5555 4444"}},
		// near misses
		{"4111111111111112", nil},        // fails Luhn
		{"1234567890123456", nil},        // fails Luhn
		{"411111111111", nil},            // 12 digits
		{"41111111111111111111", nil},    // 20 digits
		{"0000000000000000", nil},        // all the same digit
		{"ID4111111111111111", nil},      // glued to letters
		{"4111111111111111abc", nil},     // glued to letters
		{"_4111111111111111", nil},       // glued to an underscore
		{"3.4111111111111111", nil},      // fraction digits
		{"4111 1111 1111 111", nil},      // 15 digits, fails Luhn as 4-4-4-3 grouping
		{"41 11 11 11 11 11 11 11", nil}, // not card grouping
		{makeLuhn("1", 13), nil},         // epoch milliseconds shape
		{makeLuhn("1", 16), nil},         // epoch microseconds shape
		{makeLuhn("1", 19), nil},         // epoch nanoseconds / snowflake shape
		{makeLuhn("0", 16), nil},         // leading zero
		{"2024-01-15T10:30:00Z", nil},    // timestamp
		{"+14155552671", nil},            // phone
		{"555-55-5555", nil},             // SSN shaped
	})
	// A UATP card (15 digits starting with 1) is still a card.
	uatp := makeLuhn("1", 15)
	if got := matchesOf(model.DetectCreditCard, uatp); len(got) != 1 {
		t.Errorf("UATP %s: got %v", uatp, got)
	}
}

func TestSSNDetector(t *testing.T) {
	runDetCases(t, model.DetectSSN, []detCase{
		{"123-45-6789", []string{"123-45-6789"}},
		{"SSN: 123-45-6789.", []string{"123-45-6789"}},
		{"ssn=078-05-1120&x=1", []string{"078-05-1120"}},
		{"912-78-1234", []string{"912-78-1234"}}, // ITIN
		{"a 123-45-6789 b 234-56-7890", []string{"123-45-6789", "234-56-7890"}},
		// invalid ranges and near misses
		{"000-12-3456", nil},
		{"666-12-3456", nil},
		{"900-12-3456", nil}, // 9xx with a non-ITIN group
		{"123-00-6789", nil},
		{"123-45-0000", nil},
		{"123456789", nil},
		{"1234-56-7890", nil},
		{"123-45-67890", nil},
		{"12-345-6789", nil},
		{"A123-45-6789", nil},
		{"2024-01-15", nil},
		{"123-45-6789-1", nil},
	})
}

func TestPhoneDetector(t *testing.T) {
	runDetCases(t, model.DetectPhone, []detCase{
		{"+14155552671", []string{"+14155552671"}},
		{"+1 415 555 2671", []string{"+1 415 555 2671"}},
		{"+44 20 7946 0958", []string{"+44 20 7946 0958"}},
		{"+1 (415) 555-2671", []string{"+1 (415) 555-2671"}},
		{"+49 (0)30 1234567", []string{"+49 (0)30 1234567"}},
		{"(415) 555-2671", []string{"(415) 555-2671"}},
		{"(415)555-2671", []string{"(415)555-2671"}},
		{"415-555-2671", []string{"415-555-2671"}},
		{"415.555.2671", []string{"415.555.2671"}},
		{"415 555 2671", []string{"415 555 2671"}},
		{"1-415-555-2671", []string{"1-415-555-2671"}},
		{"1 (415) 555-2671", []string{"1 (415) 555-2671"}},
		{"Call 415-555-2671 now", []string{"415-555-2671"}},
		{"Call +1 415 555 2671, thanks", []string{"+1 415 555 2671"}},
		{"phone=%2B14155552671", nil}, // still percent-encoded: decoded by the query layer first
		// near misses: plain integers, dates, IDs, versions
		{"4155552671", nil},
		{"14155552671", nil},
		{"1698765432", nil},
		{"2024-01-15", nil},
		{"12345678", nil},
		{"1.2.3", nil},
		{"v1.2.3.4", nil},
		{"123-456-7890", nil},  // area code cannot start with 1
		{"555-1234", nil},      // no area code
		{"415-555-26711", nil}, // too many digits
		{"(415) 555-2671x", nil},
		{"A415-555-2671", nil},
		{"+1", nil},
		{"+123", nil},
		{"+1234567890123456", nil}, // 16 digits
		{"2024-415-555-2671", nil},
		{"415-555.2671", nil}, // mixed separators without parentheses
		{"1+1=2", nil},
		{"2024-01-15T10:30:00+05:30", nil},
	})
}

func TestIPv4Detector(t *testing.T) {
	runDetCases(t, model.DetectIPv4, []detCase{
		{"192.168.1.10", []string{"192.168.1.10"}},
		{"10.0.0.1", []string{"10.0.0.1"}},
		{"255.255.255.255", []string{"255.255.255.255"}},
		{"0.0.0.0", []string{"0.0.0.0"}},
		{"ip=8.8.8.8:53", []string{"8.8.8.8"}},
		{"(1.2.3.4)", []string{"1.2.3.4"}},
		{"from 1.2.3.4, 5.6.7.8", []string{"1.2.3.4", "5.6.7.8"}},
		{"http://203.0.113.5/x", []string{"203.0.113.5"}},
		{"10.0.0.0/8", []string{"10.0.0.0"}},
		// near misses
		{"256.1.1.1", nil},
		{"1.2.3", nil},
		{"1.2.3.4.5", nil},
		{"01.02.03.04", nil},
		{"1.2.3.04", nil},
		{"v1.2.3.4", nil},
		{"Chrome/120.0.0.0", nil},
		{"1.2.3.456", nil},
		{"1.2.3.4a", nil},
		{"1.2.3.", nil},
		{"3.14159", nil},
	})
}

func TestIPv6Detector(t *testing.T) {
	runDetCases(t, model.DetectIPv6, []detCase{
		{"2001:db8::1", []string{"2001:db8::1"}},
		{"::1", []string{"::1"}},
		{"fe80::1", []string{"fe80::1"}},
		{"2001:0db8:85a3:0000:0000:8a2e:0370:7334", []string{"2001:0db8:85a3:0000:0000:8a2e:0370:7334"}},
		{"::ffff:192.168.1.1", []string{"::ffff:192.168.1.1"}},
		{"[2001:db8::1]:8080", []string{"2001:db8::1"}},
		{"addr 2001:db8::1.", []string{"2001:db8::1"}},
		{"2001:DB8::ABCD", []string{"2001:DB8::ABCD"}},
		// near misses
		{"10:30:00", nil},
		{"12:34:56:78:9a:bc", nil}, // MAC address
		{"std::vector", nil},
		{"Foo::Bar", nil},
		{"::", nil},
		{"a:b", nil},
		{"1:2:3", nil},
		{"2001:db8::g", nil},
		{"12:30:45.123", nil},
		{"1:2:3:4:5:6:7:8:9", nil},
	})
}

const sampleJWT = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwiZW1haWwiOiJqYW5lQGFjbWUuaW8ifQ.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk0"

func TestJWTDetector(t *testing.T) {
	runDetCases(t, model.DetectJWT, []detCase{
		{sampleJWT, []string{sampleJWT}},
		{"Bearer " + sampleJWT, []string{sampleJWT}},
		{"token=" + sampleJWT + "&x=1", []string{sampleJWT}},
		{"jwt " + sampleJWT + ".", []string{sampleJWT}},
		// near misses
		{"eyJhbGciOiJIUzI1NiJ9.e30", nil}, // two segments
		{"eyJ4eHg.eyJ5eXk.enp6", nil},     // header does not decode to JSON
		{"abc.def.ghi", nil},
		{sampleJWT + ".extra", nil},                                        // four segments
		{"xeyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxIn0.sig", nil}, // glued to a token
		{"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxIn0.", nil},     // empty signature
	})
}

func TestAWSKeyDetector(t *testing.T) {
	runDetCases(t, model.DetectAWSKey, []detCase{
		{"AKIAIOSFODNN7EXAMPLE", []string{"AKIAIOSFODNN7EXAMPLE"}},
		{"ASIAY34FZKBOKMUTVV7A", []string{"ASIAY34FZKBOKMUTVV7A"}},
		{"key=AKIAIOSFODNN7EXAMPLE&x", []string{"AKIAIOSFODNN7EXAMPLE"}},
		{"AWS_KEY=AKIAIOSFODNN7EXAMPLE", []string{"AKIAIOSFODNN7EXAMPLE"}},
		// near misses
		{"AKIAIOSFODNN7EXAMPL", nil},
		{"akiaiosfodnn7example", nil},
		{"XAKIAIOSFODNN7EXAMPLE", nil},
		{"AKIAIOSFODNN7EXAMPLE1", nil},
		{"AKIA", nil},
		{"AKIAIOSFODNN7EXAMPL!", nil},
	})
}

func TestIBANDetector(t *testing.T) {
	runDetCases(t, model.DetectIBAN, []detCase{
		{"GB82 WEST 1234 5698 7654 32", []string{"GB82 WEST 1234 5698 7654 32"}},
		{"GB82WEST12345698765432", []string{"GB82WEST12345698765432"}},
		{"DE89370400440532013000", []string{"DE89370400440532013000"}},
		{"DE89 3704 0044 0532 0130 00", []string{"DE89 3704 0044 0532 0130 00"}},
		{"FR1420041010050500013M02606", []string{"FR1420041010050500013M02606"}},
		{"NL91 ABNA 0417 1643 00", []string{"NL91 ABNA 0417 1643 00"}},
		{"iban: gb82west12345698765432.", []string{"gb82west12345698765432"}},
		{"NO9386011117947", []string{"NO9386011117947"}},
		// near misses
		{"GB82WEST12345698765433", nil}, // bad check digits
		{"XX82WEST12345698765432", nil}, // unknown country
		{"GB82 WEST 1234 5698 7654 3", nil},
		{"DE8937040044053201300", nil},
		{"DE893704004405320130001", nil},
		{"XGB82WEST12345698765432", nil},
		{"hello world 12", nil},
	})
}

func TestUUIDDetector(t *testing.T) {
	const u = "550e8400-e29b-41d4-a716-446655440000"
	runDetCases(t, model.DetectUUID, []detCase{
		{u, []string{u}},
		{strings.ToUpper(u), []string{strings.ToUpper(u)}},
		{"id=" + u + "&x", []string{u}},
		{"{" + u + "}", []string{u}},
		{"urn:uuid:" + u, []string{u}},
		{"order-" + u, []string{u}},
		{"00000000-0000-0000-0000-000000000000", []string{"00000000-0000-0000-0000-000000000000"}},
		{u + " " + u, []string{u, u}},
		// near misses
		{"550e8400-e29b-41d4-a716-44665544000", nil},
		{"550e8400e29b41d4a716446655440000", nil},
		{"550e8400-e29b-41d4-a716-44665544000g", nil},
		{"x550e8400-e29b-41d4-a716-446655440000", nil},
		{"550e8400-e29b-41d4-a716-4466554400001", nil},
	})
}

func TestDetectorPriorityAndOverlap(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		// A JWT contains runs of digits and letters other detectors could eat.
		{"t=" + sampleJWT, []string{"jwt:" + sampleJWT}},
		// Digits inside an e-mail address stay with the address.
		{"4111111111111111@example.com", []string{"email:4111111111111111@example.com"}},
		// A card is not also a phone, and a phone is not also an SSN.
		{"4111 1111 1111 1111 and 415-555-2671 and 123-45-6789",
			[]string{"credit_card:4111 1111 1111 1111", "phone:415-555-2671", "ssn:123-45-6789"}},
		// An IPv4-mapped IPv6 address is one IPv6 match, not IPv6 plus IPv4.
		{"::ffff:192.168.1.1", []string{"ipv6:::ffff:192.168.1.1"}},
		// An e-mail whose domain looks like an IP is just an e-mail.
		{"a@b.example.com 10.0.0.1", []string{"email:a@b.example.com", "ipv4:10.0.0.1"}},
	}
	for _, c := range cases {
		if got := matchTexts(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q\n got  %q\n want %q", c.in, got, c.want)
		}
	}
}

func TestDetectorExamplesMatch(t *testing.T) {
	for _, d := range Detectors() {
		if got := matchesOf(d.Name, d.Example); len(got) != 1 || got[0] != d.Example {
			t.Errorf("%s example %q: matches = %q", d.Name, d.Example, got)
		}
	}
}

func TestDetectorsListing(t *testing.T) {
	ds := Detectors()
	if len(ds) != len(model.AllDetectors) {
		t.Fatalf("Detectors() returned %d, want %d", len(ds), len(model.AllDetectors))
	}
	for i, d := range ds {
		if d.Name != model.AllDetectors[i] {
			t.Errorf("Detectors()[%d].Name = %s, want %s", i, d.Name, model.AllDetectors[i])
		}
		if d.Description == "" || d.Example == "" {
			t.Errorf("%s: empty description or example", d.Name)
		}
	}
}

func TestLuhnValid(t *testing.T) {
	good := []string{"4111111111111111", "378282246310005", "5555 5555 5555 4444", "0"}
	bad := []string{"4111111111111112", "", "abc", "1234"}
	for _, s := range good {
		if !luhnValid(s) {
			t.Errorf("luhnValid(%q) = false", s)
		}
	}
	for _, s := range bad {
		if luhnValid(s) {
			t.Errorf("luhnValid(%q) = true", s)
		}
	}
}
