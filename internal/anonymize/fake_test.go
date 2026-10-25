package anonymize

import (
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/justinburger/mirage/internal/model"
)

func testKeyer(secret string) *keyer { return newKeyer([]byte(secret)) }

func nonDigits(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func digitsOf(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if isDigit(s[i]) {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func TestFakeEmail(t *testing.T) {
	k := testKeyer("s")
	re := regexp.MustCompile(`^[a-z]+\.[a-z]+[0-9]{2}@example\.(com|net|org)$`)
	seen := map[string]bool{}
	domains := map[string]bool{}
	for _, in := range []string{"jane@acme.io", "JANE@ACME.IO", "a@b.co", "someone.else+tag@sub.example.co.uk", "x@y.zz"} {
		out := k.fake(model.FakeEmail, in)
		if !re.MatchString(out) {
			t.Errorf("fake email(%q) = %q, does not match %v", in, out, re)
		}
		if got := matchesOf(model.DetectEmail, out); len(got) != 1 || got[0] != out {
			t.Errorf("fake email %q is not detected as one e-mail address: %v", out, got)
		}
		seen[out] = true
		domains[out[strings.IndexByte(out, '@')+1:]] = true
	}
	if len(seen) != 5 {
		t.Errorf("5 inputs gave %d distinct fakes", len(seen))
	}
	// Over many inputs all three reserved domains are used, and nothing else.
	for i := 0; i < 200; i++ {
		out := k.fake(model.FakeEmail, "user"+itoa(i)+"@acme.io")
		domains[out[strings.IndexByte(out, '@')+1:]] = true
	}
	if len(domains) != 3 {
		t.Errorf("domains used = %v, want exactly example.com/.net/.org", domains)
	}
}

func TestFakePhone(t *testing.T) {
	k := testKeyer("s")
	for _, in := range []string{
		"(415) 555-2671", "415-555-2671", "415.555.2671", "415 555 2671", "1-415-555-2671",
		"+14155552671", "+1 (415) 555-2671", "+44 20 7946 0958", "+7 495 123 45 67", "+49 (0)30 1234567",
		"020 7946 0958", "5551234",
	} {
		out := k.fake(model.FakePhone, in)
		if len(out) != len(in) || nonDigits(out) != nonDigits(in) {
			t.Errorf("fake phone(%q) = %q: punctuation or length changed", in, out)
		}
		if out == in {
			t.Errorf("fake phone(%q) returned the input", in)
		}
		if strings.HasPrefix(in, "+") {
			// The country code survives: one digit for +1/+7, else two.
			cc := 2
			if in[1] == '1' || in[1] == '7' {
				cc = 1
			}
			if digitsOf(out)[:cc] != digitsOf(in)[:cc] {
				t.Errorf("fake phone(%q) = %q: country code changed", in, out)
			}
		}
	}
	// North American numbers stay valid: area code and exchange start with 2-9.
	for i := 0; i < 300; i++ {
		out := k.fake(model.FakePhone, "(415) 555-"+itoa(1000+i))
		d := digitsOf(out)
		if d[0] < '2' || d[3] < '2' {
			t.Fatalf("fake NANP phone %q has an invalid area code or exchange", out)
		}
	}
	// No digits at all still yields a phone number.
	if out := k.fake(model.FakePhone, "n/a"); len(digitsOf(out)) != 10 {
		t.Errorf("fake phone of a digit-free value = %q", out)
	}
}

func TestFakeCard(t *testing.T) {
	k := testKeyer("s")
	for _, in := range []string{
		"4111111111111111", "4111 1111 1111 1111", "4111-1111-1111-1111", "378282246310005",
		"3782 822463 10005", "4222222222222", makeLuhn("5", 19), "5555555555554444",
	} {
		out := k.fake(model.FakeCreditCard, in)
		if len(out) != len(in) || nonDigits(out) != nonDigits(in) {
			t.Errorf("fake card(%q) = %q: grouping or length changed", in, out)
		}
		if !luhnValid(out) {
			t.Errorf("fake card(%q) = %q is not Luhn valid", in, out)
		}
		if digitsOf(out)[0] != '4' {
			t.Errorf("fake card(%q) = %q does not start with 4", in, out)
		}
		if out == in {
			t.Errorf("fake card(%q) returned the input", in)
		}
	}
	if out := k.fake(model.FakeCreditCard, "x"); len(out) != 16 || !luhnValid(out) {
		t.Errorf("fake card of a non-number = %q", out)
	}
	// The fake of a card is itself detected as a card: it round-trips through
	// the detector, which matters for tools that re-scan anonymized data.
	out := k.fake(model.FakeCreditCard, "4111 1111 1111 1111")
	if got := matchesOf(model.DetectCreditCard, out); len(got) != 1 {
		t.Errorf("fake card %q is not detected as a card: %v", out, got)
	}
}

func TestFakeDigits(t *testing.T) {
	k := testKeyer("s")
	for _, in := range []string{"1", "5", "9", "0", "12345", "007", "10", "987654321", "12.50", "-42", "AB-1234"} {
		out := k.fake(model.FakeDigits, in)
		if len(out) != len(in) || nonDigits(out) != nonDigits(in) {
			t.Errorf("fake digits(%q) = %q: shape changed", in, out)
		}
		if out == in {
			t.Errorf("fake digits(%q) returned the input", in)
		}
	}
	// No leading zero if the input had none.
	for i := 1; i < 500; i++ {
		in := itoa(i * 7919)
		out := k.fake(model.FakeDigits, in)
		if out[0] == '0' {
			t.Fatalf("fake digits(%q) = %q starts with 0", in, out)
		}
	}
	// A leading zero input may produce any first digit and keeps its length.
	if out := k.fake(model.FakeDigits, "00123"); len(out) != 5 {
		t.Errorf("fake digits(00123) = %q", out)
	}
	if out := k.fake(model.FakeDigits, "no digits"); len(out) != len("no digits") {
		t.Errorf("fake digits of text = %q", out)
	}
}

func TestFakeUUID(t *testing.T) {
	k := testKeyer("s")
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	for _, in := range []string{
		"550e8400-e29b-41d4-a716-446655440000", // v4
		"6ba7b810-9dad-11d1-80b4-00c04fd430c8", // v1
		"018f3b1e-7c5a-7d2a-9a6b-0b9f2f5c1e11", // v7
		"00000000-0000-0000-0000-000000000000", // nil: variant nibble 0 is kept
	} {
		out := k.fake(model.FakeUUID, in)
		if !re.MatchString(out) {
			t.Errorf("fake uuid(%q) = %q: bad shape", in, out)
			continue
		}
		if out[14] != in[14] {
			t.Errorf("fake uuid(%q) = %q: version nibble changed", in, out)
		}
		if strings.ContainsRune("89ab", rune(in[19])) != strings.ContainsRune("89ab", rune(out[19])) {
			t.Errorf("fake uuid(%q) = %q: variant class changed", in, out)
		}
		if out == in {
			t.Errorf("fake uuid(%q) returned the input", in)
		}
	}
	// Upper case in, upper case out.
	up := k.fake(model.FakeUUID, "550E8400-E29B-41D4-A716-446655440000")
	if up != strings.ToUpper(up) {
		t.Errorf("fake uuid lost its case: %s", up)
	}
	// Anything else becomes a random v4.
	v4 := k.fake(model.FakeUUID, "user-17")
	if !re.MatchString(v4) || v4[14] != '4' || !strings.ContainsRune("89ab", rune(v4[19])) {
		t.Errorf("fake uuid of a non-uuid = %q", v4)
	}
	if !isUUID(v4) {
		t.Errorf("isUUID rejects its own fake %q", v4)
	}
}

func TestFakeIPv4(t *testing.T) {
	k := testKeyer("s")
	for _, in := range []string{"192.168.1.10", "8.8.8.8", "1.1.1.1", "not an ip"} {
		out := k.fake(model.FakeIPv4, in)
		if !strings.HasPrefix(out, "10.") {
			t.Errorf("fake ipv4(%q) = %q, want 10.0.0.0/8", in, out)
		}
		if got := matchesOf(model.DetectIPv4, out); len(got) != 1 || got[0] != out {
			t.Errorf("fake ipv4 %q is not a valid IPv4 address", out)
		}
	}
}

func TestFakeName(t *testing.T) {
	k := testKeyer("s")
	cases := []struct {
		in    string
		words int
		check func(string) bool
	}{
		{"Jane Smith", 2, func(s string) bool { return s[0] >= 'A' && s[0] <= 'Z' }},
		{"jane", 1, func(s string) bool { return s == strings.ToLower(s) }},
		{"JANE SMITH", 2, func(s string) bool { return s == strings.ToUpper(s) }},
		{"jane q smith", 3, func(s string) bool { return s == strings.ToLower(s) }},
		{"Smith, Jane", 2, func(s string) bool { return strings.Contains(s, ", ") }},
	}
	for _, c := range cases {
		out := k.fake(model.FakeName, c.in)
		if got := len(strings.Fields(out)); got != c.words {
			t.Errorf("fake name(%q) = %q: %d words, want %d", c.in, out, got, c.words)
		}
		if !c.check(out) {
			t.Errorf("fake name(%q) = %q: style not preserved", c.in, out)
		}
		if out == c.in {
			t.Errorf("fake name(%q) returned the input", c.in)
		}
	}
	if k.fake(model.FakeName, "   ") != "   " {
		t.Errorf("blank name should be unchanged")
	}
}

func TestFakeString(t *testing.T) {
	k := testKeyer("s")
	classOf := func(r rune) int {
		switch {
		case unicode.IsUpper(r):
			return 1
		case unicode.IsLower(r):
			return 2
		case unicode.IsDigit(r):
			return 3
		}
		return 0
	}
	for _, in := range []string{"AB-1234", "Hello, World!", "a", "x_y.z/9", "ORDER 0042", "sk_live_ABCdef123"} {
		out := k.fake(model.FakeString, in)
		if utf8.RuneCountInString(out) != utf8.RuneCountInString(in) {
			t.Errorf("fake string(%q) = %q: length changed", in, out)
		}
		ri, ro := []rune(in), []rune(out)
		for i := range ri {
			if classOf(ri[i]) != classOf(ro[i]) {
				t.Errorf("fake string(%q) = %q: character %d changed class", in, out, i)
			}
			if classOf(ri[i]) == 0 && ri[i] != ro[i] {
				t.Errorf("fake string(%q) = %q: punctuation %q not kept", in, out, ri[i])
			}
		}
		if out == in {
			t.Errorf("fake string(%q) returned the input", in)
		}
	}
	// Non-ASCII letters become ASCII letters so nothing of the original
	// survives; combining marks are dropped.
	out := k.fake(model.FakeString, "Müller 北京 é")
	for _, r := range out {
		if r > unicode.MaxASCII {
			t.Errorf("fake string kept non-ASCII %q in %q", r, out)
		}
	}
	// Empty and punctuation-only values come back unchanged.
	if k.fake(model.FakeString, "") != "" || k.fake(model.FakeString, "---") != "---" {
		t.Errorf("empty or punctuation-only values should be unchanged")
	}
}

func TestFakeAutoDetectsShape(t *testing.T) {
	k := testKeyer("s")
	cases := []struct {
		in   string
		want model.FakeKind
	}{
		{"jane@acme.io", model.FakeEmail},
		{"550e8400-e29b-41d4-a716-446655440000", model.FakeUUID},
		{"192.168.1.10", model.FakeIPv4},
		{"(415) 555-2671", model.FakePhone},
		{"+14155552671", model.FakePhone},
		{"4111 1111 1111 1111", model.FakeCreditCard},
		{"4111111111111111", model.FakeCreditCard},
		{"123-45-6789", fakeSSN},
		{"GB82 WEST 1234 5698 7654 32", fakeIBAN},
		{"AKIAIOSFODNN7EXAMPLE", fakeAWSKey},
		{"2001:db8::1", fakeIPv6},
		{sampleJWT, fakeJWT},
		{"12345", model.FakeDigits},
		{"-42.5", model.FakeDigits},
		{"4155552671", model.FakeDigits}, // a plain integer is digits, not a phone
		{"AB-1234", model.FakeString},
		{"hello", model.FakeString},
		{"2024-01-15", model.FakeString},
		{"", model.FakeString},
	}
	for _, c := range cases {
		if got := detectFakeKind(c.in); got != c.want {
			t.Errorf("detectFakeKind(%q) = %q, want %q", c.in, got, c.want)
		}
		// fake auto equals fake of the detected kind (same HMAC domain).
		if c.in != "" {
			if a, b := k.fake(model.FakeAuto, c.in), k.fake(c.want, c.in); a != b {
				t.Errorf("fake auto(%q) = %q but fake %s = %q", c.in, a, c.want, b)
			}
		}
	}
}

func TestFakeSSNIPv6JWTAWSIBAN(t *testing.T) {
	k := testKeyer("s")

	ssn := k.fake(fakeSSN, "123-45-6789")
	if got := matchesOf(model.DetectSSN, ssn); len(got) != 1 || got[0] != ssn {
		t.Errorf("fake ssn %q is not a valid SSN shape", ssn)
	}
	if ssn == "123-45-6789" {
		t.Errorf("fake ssn returned the input")
	}
	if got := k.fake(fakeSSN, "12345"); len(got) != 5 {
		t.Errorf("fake ssn of a non-SSN = %q", got)
	}

	v6 := k.fake(fakeIPv6, "2001:db8::1")
	if got := matchesOf(model.DetectIPv6, v6); len(got) != 1 || !strings.HasPrefix(v6, "fd") {
		t.Errorf("fake ipv6 %q: matches %v", v6, got)
	}

	jwt := k.fake(fakeJWT, sampleJWT)
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 || parts[0] != strings.Split(sampleJWT, ".")[0] {
		t.Errorf("fake jwt %q: header should be kept", jwt)
	}
	if got := matchesOf(model.DetectJWT, jwt); len(got) != 1 || got[0] != jwt {
		t.Errorf("fake jwt %q is not detected as a JWT", jwt)
	}
	if strings.Contains(jwt, strings.Split(sampleJWT, ".")[1]) {
		t.Errorf("fake jwt kept the original payload")
	}
	if got := k.fake(fakeJWT, "nonsense"); !jwtHeaderOK(strings.Split(got, ".")[0]) {
		t.Errorf("fake jwt of a non-jwt = %q: bad header", got)
	}

	for _, in := range []string{"AKIAIOSFODNN7EXAMPLE", "ASIAY34FZKBOKMUTVV7A"} {
		out := k.fake(fakeAWSKey, in)
		if len(out) != 20 || out[:4] != in[:4] || out == in {
			t.Errorf("fake aws key(%q) = %q", in, out)
		}
		if got := matchesOf(model.DetectAWSKey, out); len(got) != 1 {
			t.Errorf("fake aws key %q not detected", out)
		}
	}

	for _, in := range []string{"GB82 WEST 1234 5698 7654 32", "DE89370400440532013000", "FR1420041010050500013M02606", "gb82west12345698765432"} {
		out := k.fake(fakeIBAN, in)
		if len(out) != len(in) || out == in || out[:2] != in[:2] {
			t.Errorf("fake iban(%q) = %q", in, out)
		}
		if got := matchesOf(model.DetectIBAN, out); len(got) != 1 || got[0] != out {
			t.Errorf("fake iban %q is not a valid IBAN: %v", out, got)
		}
		// Spaces stay where they were.
		for i := range in {
			if (in[i] == ' ') != (out[i] == ' ') {
				t.Errorf("fake iban(%q) = %q: spacing changed", in, out)
				break
			}
		}
	}
}

func TestFakeDeterminism(t *testing.T) {
	kinds := []model.FakeKind{
		model.FakeAuto, model.FakeEmail, model.FakeName, model.FakePhone, model.FakeUUID,
		model.FakeDigits, model.FakeIPv4, model.FakeCreditCard, model.FakeString,
		fakeSSN, fakeIPv6, fakeJWT, fakeAWSKey, fakeIBAN,
	}
	a1, a2 := testKeyer("secret-a"), testKeyer("secret-a")
	b := testKeyer("secret-b")
	salted := newKeyer(deriveKey([]byte("secret-a"), "clone-1"))
	salted2 := newKeyer(deriveKey([]byte("secret-a"), "clone-1"))
	other := newKeyer(deriveKey([]byte("secret-a"), "clone-2"))
	for _, kind := range kinds {
		in := "Some Value 4111111111111111 jane@acme.io"
		switch kind {
		case model.FakeCreditCard:
			in = "4111 1111 1111 1111"
		case fakeSSN:
			in = "123-45-6789"
		case fakeIBAN:
			in = "GB82 WEST 1234 5698 7654 32"
		case fakeAWSKey:
			in = "AKIAIOSFODNN7EXAMPLE"
		case fakeJWT:
			in = sampleJWT
		case model.FakeUUID:
			in = "550e8400-e29b-41d4-a716-446655440000"
		}
		x := a1.fake(kind, in)
		if y := a2.fake(kind, in); x != y {
			t.Errorf("%s: same secret gave %q and %q", kind, x, y)
		}
		if y := b.fake(kind, in); x == y {
			t.Errorf("%s: different secrets gave the same fake %q", kind, x)
		}
		s := salted.fake(kind, in)
		if s2 := salted2.fake(kind, in); s != s2 {
			t.Errorf("%s: same salt gave %q and %q", kind, s, s2)
		}
		if s == x {
			t.Errorf("%s: salt did not change the fake %q", kind, x)
		}
		if o := other.fake(kind, in); o == s {
			t.Errorf("%s: different salts gave the same fake %q", kind, s)
		}
	}
}

func TestHashToken(t *testing.T) {
	k := testKeyer("s")
	tok := k.hashToken("customer-42")
	if !regexp.MustCompile(`^h_[0-9a-f]{16}$`).MatchString(tok) {
		t.Fatalf("token %q has the wrong shape", tok)
	}
	if tok != k.hashToken("customer-42") {
		t.Error("hash not deterministic")
	}
	if tok == k.hashToken("customer-43") {
		t.Error("different values share a token")
	}
	if tok == testKeyer("other").hashToken("customer-42") {
		t.Error("different secrets share a token")
	}
	// Long values (more than one HMAC buffer) are hashed completely.
	long := strings.Repeat("a", 5000)
	if k.hashToken(long) == k.hashToken(long[:4999]+"b") {
		t.Error("the tail of a long value is ignored")
	}
	if k.hashToken(long) == k.hashToken(long[:4999]) {
		t.Error("length of a long value is ignored")
	}
	// Domain separation: hash and fake of the same value are unrelated.
	if strings.Contains(k.fake(model.FakeString, "customer-42"), tok[2:]) {
		t.Error("hash and fake share material")
	}
}

func TestPRNGLongStreams(t *testing.T) {
	p := newPRNG(testKeyer("s").digest("x", "y"))
	seen := map[byte]int{}
	for i := 0; i < 5000; i++ { // far beyond the 32 seed bytes
		seen[p.byte()]++
	}
	if len(seen) < 200 {
		t.Errorf("only %d distinct byte values in 5000 draws", len(seen))
	}
	q := newPRNG(testKeyer("s").digest("x", "y"))
	r := newPRNG(testKeyer("s").digest("x", "y"))
	for i := 0; i < 100; i++ {
		if q.intn(1000) != r.intn(1000) {
			t.Fatal("prng is not deterministic")
		}
	}
	for i := 0; i < 1000; i++ {
		if d := p.digit(); d > 9 {
			t.Fatalf("digit() = %d", d)
		}
		if l := p.letter(); l > 25 {
			t.Fatalf("letter() = %d", l)
		}
		if n := p.intn(7); n < 0 || n >= 7 {
			t.Fatalf("intn(7) = %d", n)
		}
	}
}
