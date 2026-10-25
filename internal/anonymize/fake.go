package anonymize

import (
	"encoding/base64"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/justinburger/mirage/internal/model"
)

// Fake kinds that exist only inside the package. They back the detectors whose
// natural fake is not one of the public model.FakeKind values, and "auto"
// recognises them too.
const (
	fakeSSN    model.FakeKind = "ssn"
	fakeIPv6   model.FakeKind = "ipv6"
	fakeJWT    model.FakeKind = "jwt"
	fakeAWSKey model.FakeKind = "aws_key"
	fakeIBAN   model.FakeKind = "iban"
)

// fakeDomain returns the HMAC domain for a resolved fake kind. Keying on the
// resolved kind (not on "auto") means `fake auto` on an e-mail address and
// `fake email` on the same address produce the same fake.
func fakeDomain(kind model.FakeKind) string {
	switch kind {
	case model.FakeEmail:
		return "fake/email"
	case model.FakeName:
		return "fake/name"
	case model.FakePhone:
		return "fake/phone"
	case model.FakeUUID:
		return "fake/uuid"
	case model.FakeDigits:
		return "fake/digits"
	case model.FakeIPv4:
		return "fake/ipv4"
	case model.FakeCreditCard:
		return "fake/credit_card"
	case fakeSSN:
		return "fake/ssn"
	case fakeIPv6:
		return "fake/ipv6"
	case fakeJWT:
		return "fake/jwt"
	case fakeAWSKey:
		return "fake/aws_key"
	case fakeIBAN:
		return "fake/iban"
	}
	return "fake/string"
}

// fake returns the deterministic, format-preserving fake of v for kind.
func (k *keyer) fake(kind model.FakeKind, v string) string {
	if kind == "" || kind == model.FakeAuto {
		kind = detectFakeKind(v)
	}
	p := newPRNG(k.digest(fakeDomain(kind), v))
	switch kind {
	case model.FakeEmail:
		return genEmail(&p)
	case model.FakeName:
		return genName(&p, v)
	case model.FakePhone:
		return genPhone(&p, v)
	case model.FakeUUID:
		return genUUID(&p, v)
	case model.FakeDigits:
		return genDigits(&p, v)
	case model.FakeIPv4:
		return genIPv4(&p)
	case model.FakeCreditCard:
		return genCard(&p, v)
	case fakeSSN:
		return genSSN(&p, v)
	case fakeIPv6:
		return genIPv6(&p)
	case fakeJWT:
		return genJWT(&p, v)
	case fakeAWSKey:
		return genAWSKey(&p, v)
	case fakeIBAN:
		return genIBAN(&p, v)
	}
	return genString(&p, v)
}

// detectFakeKind picks the fake for `fake auto` by recognising the shape of v:
// e-mail, UUID, JWT, AWS key, IBAN, IPv6, IPv4, SSN, card, phone, plain
// number. Anything else keeps its length and character classes.
func detectFakeKind(v string) model.FakeKind {
	if v == "" {
		return model.FakeString
	}
	st := classify(v)
	switch {
	case wholeMatch(detEmail, v, st):
		return model.FakeEmail
	case isUUID(v):
		return model.FakeUUID
	case wholeMatch(detJWT, v, st):
		return fakeJWT
	case wholeMatch(detAWS, v, st):
		return fakeAWSKey
	case wholeMatch(detIBAN, v, st):
		return fakeIBAN
	case wholeMatch(detIPv6, v, st):
		return fakeIPv6
	case wholeMatch(detIPv4, v, st):
		return model.FakeIPv4
	case wholeMatch(detSSN, v, st):
		return fakeSSN
	case wholeMatch(detCard, v, st):
		return model.FakeCreditCard
	case wholeMatch(detPhone, v, st):
		return model.FakePhone
	case isNumeric(v):
		return model.FakeDigits
	}
	return model.FakeString
}

// wholeMatch reports whether detector di matches all of v.
func wholeMatch(di int, v string, st textStats) bool {
	if !gateFor(di, st) {
		return false
	}
	var c claims
	c.cur = uint8(di)
	findFor(di, v, &c)
	return c.n == 1 && c.inl[0].start == 0 && c.inl[0].end == len(v)
}

// isNumeric reports whether v looks like a plain number: an optional minus
// sign, digits and an optional fraction.
func isNumeric(v string) bool {
	i := 0
	if i < len(v) && v[i] == '-' {
		i++
	}
	d := 0
	for i < len(v) && isDigit(v[i]) {
		i++
		d++
	}
	if d == 0 {
		return false
	}
	if i < len(v) && v[i] == '.' {
		i++
		f := 0
		for i < len(v) && isDigit(v[i]) {
			i++
			f++
		}
		if f == 0 {
			return false
		}
	}
	return i == len(v)
}

// ---- generators -----------------------------------------------------------

func twoDigits(b []byte, n int) []byte { return append(b, byte('0'+n/10), byte('0'+n%10)) }

func genEmail(p *prng) string {
	f := firstNamesLower[p.intn(len(firstNamesLower))]
	l := lastNamesLower[p.intn(len(lastNamesLower))]
	n := p.intn(100)
	dom := emailDomains[p.intn(len(emailDomains))]
	b := make([]byte, 0, len(f)+len(l)+len(dom)+4)
	b = append(b, f...)
	b = append(b, '.')
	b = append(b, l...)
	b = twoDigits(b, n)
	b = append(b, '@')
	b = append(b, dom...)
	return string(b)
}

// genName makes a name with the same number of words and the same letter case
// style as v ("JANE SMITH" stays upper case, "Smith, Jane" keeps its comma).
func genName(p *prng, v string) string {
	words := strings.Fields(v)
	if len(words) == 0 {
		return v
	}
	first := func() string { return firstNames[p.intn(len(firstNames))] }
	last := func() string { return lastNames[p.intn(len(lastNames))] }
	var out string
	switch {
	case strings.Contains(v, ","):
		out = last() + ", " + first()
	case len(words) == 1:
		out = first()
	default:
		parts := make([]string, 0, len(words))
		parts = append(parts, first())
		for i := 1; i < len(words)-1; i++ {
			parts = append(parts, first())
		}
		parts = append(parts, last())
		out = strings.Join(parts, " ")
	}
	switch {
	case v == strings.ToUpper(v) && v != strings.ToLower(v):
		return strings.ToUpper(out)
	case v == strings.ToLower(v) && v != strings.ToUpper(v):
		return strings.ToLower(out)
	}
	return out
}

// digitPositions returns the byte offsets of the ASCII digits of b.
func digitPositions(b []byte) []int {
	pos := make([]int, 0, 20)
	for i, ch := range b {
		if isDigit(ch) {
			pos = append(pos, i)
		}
	}
	return pos
}

// genPhone keeps punctuation, length and (for E.164) the country code, and
// keeps North American numbers valid by never starting an area code or an
// exchange with 0 or 1.
func genPhone(p *prng, v string) string {
	b := []byte(v)
	pos := digitPositions(b)
	nd := len(pos)
	if nd == 0 {
		return "(" + string(rune('2'+p.intn(8))) + digitStr(p, 2) + ") " + string(rune('2'+p.intn(8))) + digitStr(p, 2) + "-" + digitStr(p, 4)
	}
	first := b[pos[0]]
	plus := strings.HasPrefix(strings.TrimLeft(v, " "), "+")
	keep, nanp := 0, false
	switch {
	case plus && first == '1':
		keep, nanp = 1, nd == 11
	case plus && first == '7':
		keep = 1
	case plus:
		keep = min(2, nd-1)
	case nd == 11 && first == '1':
		keep, nanp = 1, true
	case nd == 10:
		nanp = true
	}
	for k := keep; k < nd; k++ {
		local := k - keep
		d := p.digit()
		switch {
		case nanp && (local == 0 || local == 3):
			d = byte(2 + p.intn(8))
		case k == 0 && first != '0' && d == 0:
			d = byte(1 + p.intn(9))
		}
		b[pos[k]] = '0' + d
	}
	return string(b)
}

func digitStr(p *prng, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '0' + p.digit()
	}
	return string(b)
}

// genCard keeps length and grouping, starts with 4 (a test-style Visa number)
// and ends with a Luhn check digit.
func genCard(p *prng, v string) string {
	b := []byte(v)
	pos := digitPositions(b)
	nd := len(pos)
	if nd < 2 {
		b = []byte(digitStr(p, 16))
		pos = digitPositions(b)
		nd = len(pos)
	}
	b[pos[0]] = '4'
	for k := 1; k < nd-1; k++ {
		b[pos[k]] = '0' + p.digit()
	}
	// Luhn check digit for the payload b[pos[0..nd-2]].
	sum, alt := 0, true
	for k := nd - 2; k >= 0; k-- {
		d := int(b[pos[k]] - '0')
		if alt {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	b[pos[nd-1]] = byte('0' + (10-sum%10)%10)
	return string(b)
}

// genDigits replaces every digit and keeps every other character, so "007" and
// "12.50" and "AB-1234" keep their shape. A value that did not start with 0
// does not start with 0 afterwards.
func genDigits(p *prng, v string) string {
	b := []byte(v)
	pos := digitPositions(b)
	if len(pos) == 0 {
		return genString(p, v)
	}
	for k, i := range pos {
		d := p.digit()
		if k == 0 && b[i] != '0' && d == 0 {
			d = byte(1 + p.intn(9))
		}
		b[i] = '0' + d
	}
	if string(b) == v { // never hand back the original
		i := pos[len(pos)-1]
		b[i] = '0' + (b[i]-'0'+1)%10
		if len(pos) == 1 && b[i] == '0' && v != "0" {
			b[i] = '1'
		}
	}
	return string(b)
}

// genUUID keeps the 8-4-4-4-12 shape, the letter case, the version nibble and
// the variant class of a UUID. Any other input yields a random-looking v4.
func genUUID(p *prng, v string) string {
	const tmpl = "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx"
	b := []byte(tmpl)
	upper := false
	if isUUID(v) {
		b = []byte(v)
		for i := 0; i < len(v); i++ {
			if isUpper(v[i]) {
				upper = true
			}
		}
	}
	valid := isUUID(v)
	for i := 0; i < len(b); i++ {
		switch i {
		case 8, 13, 18, 23:
			continue
		case 14:
			continue // version nibble kept (4 for the template)
		case 19:
			if valid {
				switch lowerASCII(v[19]) {
				case '8', '9', 'a', 'b':
					b[i] = "89ab"[p.intn(4)]
				}
				continue
			}
			b[i] = "89ab"[p.intn(4)]
			continue
		}
		b[i] = hexDigits[p.byte()&0xf]
	}
	if upper {
		for i := range b {
			b[i] = upperASCII(b[i])
		}
	}
	return string(b)
}

// genIPv4 maps into 10.0.0.0/8.
func genIPv4(p *prng) string {
	return "10." + strconv.Itoa(int(p.byte())) + "." + strconv.Itoa(int(p.byte())) + "." + strconv.Itoa(1+p.intn(254))
}

// genIPv6 produces an address in the unique-local range fd00::/8.
func genIPv6(p *prng) string {
	var b [39]byte
	for g := 0; g < 8; g++ {
		o := g * 5
		for i := 0; i < 4; i++ {
			b[o+i] = hexDigits[p.byte()&0xf]
		}
		if g < 7 {
			b[o+4] = ':'
		}
	}
	b[0], b[1] = 'f', 'd'
	return string(b[:])
}

// genSSN keeps the punctuation of an SSN and always produces a number in a
// valid range (area 001-899 except 666, group 01-99, serial 0001-9999).
func genSSN(p *prng, v string) string {
	b := []byte(v)
	pos := digitPositions(b)
	if len(pos) != 9 {
		return genDigits(p, v)
	}
	area := 1 + p.intn(899)
	if area == 666 {
		area = 667
	}
	group := 1 + p.intn(99)
	serial := 1 + p.intn(9999)
	digits := [9]byte{
		byte(area / 100), byte(area / 10 % 10), byte(area % 10),
		byte(group / 10), byte(group % 10),
		byte(serial / 1000), byte(serial / 100 % 10), byte(serial / 10 % 10), byte(serial % 10),
	}
	for k, i := range pos {
		b[i] = '0' + digits[k]
	}
	return string(b)
}

var b64 = base64.RawURLEncoding

// genJWT keeps the (non-sensitive) header and replaces the payload and the
// signature, so the result is still three base64url segments with a JSON
// header.
func genJWT(p *prng, v string) string {
	header := ""
	if parts := strings.SplitN(v, ".", 3); len(parts) == 3 && jwtHeaderOK(parts[0]) {
		header = parts[0]
	} else {
		header = b64.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	}
	var sub [6]byte
	for i := range sub {
		sub[i] = p.byte()
	}
	payload := b64.EncodeToString([]byte(`{"sub":"u_` + hexString(sub[:]) + `"}`))
	var sig [32]byte
	for i := range sig {
		sig[i] = p.byte()
	}
	return header + "." + payload + "." + b64.EncodeToString(sig[:])
}

func hexString(b []byte) string {
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, hexDigits[c>>4], hexDigits[c&0xf])
	}
	return string(out)
}

// genAWSKey keeps the AKIA/ASIA prefix and the 20 character length.
func genAWSKey(p *prng, v string) string {
	b := make([]byte, 20)
	prefix := "AKIA"
	if len(v) >= 4 && (v[:4] == "AKIA" || v[:4] == "ASIA") {
		prefix = v[:4]
	}
	copy(b, prefix)
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	for i := 4; i < 20; i++ {
		b[i] = alphabet[p.intn(len(alphabet))]
	}
	return string(b)
}

// genIBAN keeps the country code, the grouping and the length, randomises the
// account part (digits stay digits, letters stay letters) and recomputes the
// two check digits so the result still passes mod-97.
func genIBAN(p *prng, v string) string {
	b := []byte(v)
	idx := make([]int, 0, 34)
	for i, ch := range b {
		if isAlnum(ch) {
			idx = append(idx, i)
		}
	}
	if len(idx) < 5 {
		return genString(p, v)
	}
	for _, i := range idx[4:] {
		switch {
		case isDigit(b[i]):
			b[i] = '0' + p.digit()
		case isUpper(b[i]):
			b[i] = 'A' + p.letter()
		default:
			b[i] = 'a' + p.letter()
		}
	}
	// Check digits: 98 - (bban + country + "00") mod 97.
	b[idx[2]], b[idx[3]] = '0', '0'
	compact := make([]byte, 0, len(idx))
	for _, i := range idx {
		compact = append(compact, upperASCII(b[i]))
	}
	rem := 0
	feed := func(ch byte) {
		if isDigit(ch) {
			rem = (rem*10 + int(ch-'0')) % 97
		} else {
			rem = (rem*100 + int(ch-'A') + 10) % 97
		}
	}
	for _, ch := range compact[4:] {
		feed(ch)
	}
	for _, ch := range compact[:4] {
		feed(ch)
	}
	check := 98 - rem
	b[idx[2]], b[idx[3]] = byte('0'+check/10), byte('0'+check%10)
	return string(b)
}

// genString keeps the length and the character classes of v: upper case
// letters become upper case letters, lower case become lower case, digits
// become digits, and punctuation and whitespace are kept. Letters outside
// ASCII (accents, CJK, Cyrillic) become ASCII letters so nothing of the
// original survives; combining marks are dropped.
func genString(p *prng, v string) string {
	var sb strings.Builder
	sb.Grow(len(v))
	for _, r := range v {
		switch {
		case r < utf8.RuneSelf && isDigit(byte(r)), unicode.IsDigit(r):
			sb.WriteByte('0' + p.digit())
		case unicode.IsUpper(r):
			sb.WriteByte('A' + p.letter())
		case unicode.IsLetter(r):
			sb.WriteByte('a' + p.letter())
		case unicode.IsMark(r):
		default:
			sb.WriteRune(r)
		}
	}
	out := sb.String()
	if out == v { // never hand back the original
		b := []byte(out)
		for i, ch := range b {
			switch {
			case isDigit(ch):
				b[i] = '0' + (ch-'0'+1)%10
			case isUpper(ch):
				b[i] = 'A' + (ch-'A'+1)%26
			case isLower(ch):
				b[i] = 'a' + (ch-'a'+1)%26
			default:
				continue
			}
			return string(b)
		}
	}
	return out
}
