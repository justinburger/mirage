package anonymize

import (
	"cmp"
	"slices"

	"github.com/justinburger/mirage/internal/model"
)

// Detector indexes follow model.AllDetectors so a profile's detector set is a
// bit mask.
const (
	detEmail = iota
	detCard
	detSSN
	detPhone
	detIPv4
	detIPv6
	detJWT
	detAWS
	detIBAN
	detUUID
	numDetectors
)

// detectorOrder is the order in which detectors claim text. Longer and more
// specific shapes go first so that, for example, a JWT is never partly eaten
// by the phone or card detector and an e-mail address keeps its dotted
// domain. Once a byte range is claimed, later detectors cannot touch it.
var detectorOrder = [numDetectors]uint8{
	detJWT, detEmail, detAWS, detUUID, detIBAN, detIPv6, detCard, detSSN, detIPv4, detPhone,
}

// detector is one built-in PII detector.
type detector struct {
	name    model.Detector
	desc    string
	example string
	// fake is the format-preserving fake generated for a match when the
	// detector strategy is fake.
	fake model.FakeKind
}

// gateFor reports whether detector di could match a string with these
// byte-class counts. It is a cheap pre-filter: when it returns false the
// detector's finder is skipped. Detectors are dispatched with switches (not a
// table of funcs) so the compiler can see that nothing escapes and scanning
// stays allocation free.
func gateFor(di int, s textStats) bool {
	return candidates(1<<di, &s) != 0
}

// findFor appends every match of detector di in s to the claims, skipping
// ranges that an earlier detector already claimed.
func findFor(di int, s string, c *claims) {
	switch di {
	case detEmail:
		findEmail(s, c)
	case detCard:
		findCard(s, c)
	case detSSN:
		findSSN(s, c)
	case detPhone:
		findPhone(s, c)
	case detIPv4:
		findIPv4(s, c)
	case detIPv6:
		findIPv6(s, c)
	case detJWT:
		findJWT(s, c)
	case detAWS:
		findAWSKey(s, c)
	case detIBAN:
		findIBAN(s, c)
	case detUUID:
		findUUID(s, c)
	}
}

var detectors = [numDetectors]detector{
	detEmail: {
		name: model.DetectEmail, desc: "E-mail addresses, including percent-encoded ones (%40) inside URLs.",
		example: "jane.doe@example.com",
		fake:    model.FakeEmail,
	},
	detCard: {
		name: model.DetectCreditCard, desc: "Payment card numbers: 13-19 digits, optionally grouped with spaces or dashes, that pass the Luhn check.",
		example: "4111 1111 1111 1111",
		fake:    model.FakeCreditCard,
	},
	detSSN: {
		name: model.DetectSSN, desc: "US social security numbers written NNN-NN-NNNN, excluding invalid ranges.",
		example: "123-45-6789",
		fake:    fakeSSN,
	},
	detPhone: {
		name: model.DetectPhone, desc: "Phone numbers in E.164 (+CC...) and North American formats such as (415) 555-2671.",
		example: "(415) 555-2671",
		fake:    model.FakePhone,
	},
	detIPv4: {
		name: model.DetectIPv4, desc: "Dotted-quad IPv4 addresses with valid octets.",
		example: "192.168.1.10",
		fake:    model.FakeIPv4,
	},
	detIPv6: {
		name: model.DetectIPv6, desc: "IPv6 addresses, validated with net/netip.",
		example: "2001:db8::8a2e:370:7334",
		fake:    fakeIPv6,
	},
	detJWT: {
		name: model.DetectJWT, desc: "JSON Web Tokens: three base64url segments whose header decodes as JSON.",
		example: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk0",
		fake:    fakeJWT,
	},
	detAWS: {
		name: model.DetectAWSKey, desc: "AWS access key IDs starting with AKIA or ASIA.",
		example: "AKIAIOSFODNN7EXAMPLE",
		fake:    fakeAWSKey,
	},
	detIBAN: {
		name: model.DetectIBAN, desc: "International bank account numbers with a valid country length and mod-97 check.",
		example: "GB82 WEST 1234 5698 7654 32",
		fake:    fakeIBAN,
	},
	detUUID: {
		name: model.DetectUUID, desc: "UUIDs in 8-4-4-4-12 form. Off by default because UUIDs are usually harmless identifiers.",
		example: "550e8400-e29b-41d4-a716-446655440000",
		fake:    model.FakeUUID,
	},
}

// textStats counts byte classes in a string. One pass over a value is enough
// to rule out most detectors, which keeps clean values cheap.
type textStats struct {
	n        int
	digits   int
	ats      int
	dots     int
	colons   int
	dashes   int
	pluses   int
	percents int
	uppers   int
	letters  int
}

const (
	clsOther = iota
	clsDigit
	clsAt
	clsDot
	clsColon
	clsDash
	clsPlus
	clsPercent
	clsUpper
	clsLower
	numClasses
)

var classTable [256]uint8

func init() {
	for c := '0'; c <= '9'; c++ {
		classTable[c] = clsDigit
	}
	for c := 'A'; c <= 'Z'; c++ {
		classTable[c] = clsUpper
	}
	for c := 'a'; c <= 'z'; c++ {
		classTable[c] = clsLower
	}
	classTable['@'] = clsAt
	classTable['.'] = clsDot
	classTable[':'] = clsColon
	classTable['-'] = clsDash
	classTable['+'] = clsPlus
	classTable['%'] = clsPercent
}

// classify counts the byte classes of v.
func classify[T ~string | ~[]byte](v T) textStats {
	var cnt [numClasses]int
	for i := 0; i < len(v); i++ {
		cnt[classTable[v[i]]]++
	}
	return textStats{
		n: len(v), digits: cnt[clsDigit], ats: cnt[clsAt], dots: cnt[clsDot],
		colons: cnt[clsColon], dashes: cnt[clsDash], pluses: cnt[clsPlus],
		percents: cnt[clsPercent], uppers: cnt[clsUpper],
		letters: cnt[clsUpper] + cnt[clsLower],
	}
}

// candidates returns the bit mask of enabled detectors that could match a
// value with these statistics. The conditions are necessary, not sufficient:
// each says what the shape the detector looks for must contain at least.
func candidates(enabled uint16, st *textStats) uint16 {
	var m uint16
	if enabled&(1<<detEmail) != 0 && (st.ats > 0 || st.percents > 0) {
		m |= 1 << detEmail
	}
	if enabled&(1<<detCard) != 0 && st.digits >= 13 {
		m |= 1 << detCard
	}
	if enabled&(1<<detSSN) != 0 && st.dashes >= 2 && st.digits >= 9 {
		m |= 1 << detSSN
	}
	if enabled&(1<<detPhone) != 0 && st.digits >= 7 && (st.pluses > 0 || st.digits >= 10) {
		m |= 1 << detPhone
	}
	if enabled&(1<<detIPv4) != 0 && st.dots >= 3 && st.digits >= 4 {
		m |= 1 << detIPv4
	}
	if enabled&(1<<detIPv6) != 0 && st.colons >= 2 {
		m |= 1 << detIPv6
	}
	if enabled&(1<<detJWT) != 0 && st.dots >= 2 && st.n >= 20 {
		m |= 1 << detJWT
	}
	if enabled&(1<<detAWS) != 0 && st.n >= 20 && st.uppers >= 4 {
		m |= 1 << detAWS
	}
	if enabled&(1<<detIBAN) != 0 && st.n >= 15 && st.digits >= 2 && st.letters >= 2 {
		m |= 1 << detIBAN
	}
	if enabled&(1<<detUUID) != 0 && st.dashes >= 4 && st.n >= 36 {
		m |= 1 << detUUID
	}
	return m
}

// match is one detector hit: s[start:end].
type match struct {
	start, end int
	det        uint8
	// enc is set for an e-mail address whose "@" was written %40; its
	// replacement keeps that encoding.
	enc bool
}

// claims collects non-overlapping matches for one string. The first few live
// inline so the common cases (no match, one match) never allocate. Past a
// threshold overlap checks switch from a scan of the matches to a bitmap of
// claimed bytes, so a hostile string with tens of thousands of matches costs
// O(n) instead of O(n^2).
type claims struct {
	n     int
	inl   [6]match
	spill []match // once more than len(inl) matches exist, holds all of them
	bits  []uint64
	cur   uint8
}

const bitmapThreshold = 32

// list returns the matches found so far.
func (c *claims) list() []match {
	if c.spill != nil {
		return c.spill
	}
	return c.inl[:c.n]
}

func (c *claims) setRange(a, b int) {
	if need := (b + 63) / 64; need > len(c.bits) {
		c.bits = append(c.bits, make([]uint64, need-len(c.bits))...)
	}
	for i := a; i < b; i++ {
		c.bits[i>>6] |= 1 << (uint(i) & 63)
	}
}

func (c *claims) anySet(a, b int) bool {
	for i := a; i < b && i>>6 < len(c.bits); i++ {
		if c.bits[i>>6]&(1<<(uint(i)&63)) != 0 {
			return true
		}
	}
	return false
}

func (c *claims) free(a, b int) bool {
	if c.bits != nil {
		return !c.anySet(a, b)
	}
	for _, m := range c.list() {
		if a < m.end && m.start < b {
			return false
		}
	}
	return true
}

func (c *claims) add(a, b int) bool { return c.addEnc(a, b, false) }

func (c *claims) addEnc(a, b int, enc bool) bool {
	if !c.free(a, b) {
		return false
	}
	m := match{start: a, end: b, det: c.cur, enc: enc}
	switch {
	case c.spill != nil:
		c.spill = append(c.spill, m)
	case c.n < len(c.inl):
		c.inl[c.n] = m
	default:
		c.spill = append(make([]match, 0, 4*len(c.inl)), c.inl[:]...)
		c.spill = append(c.spill, m)
	}
	c.n++
	switch {
	case c.bits != nil:
		c.setRange(a, b)
	case c.n > bitmapThreshold:
		c.bits = []uint64{}
		for _, x := range c.list() {
			c.setRange(x.start, x.end)
		}
	}
	return true
}

// scan runs the candidate detectors in priority order over s and leaves the
// matches in c sorted by position.
func (c *claims) scan(s string, mask uint16) {
	for _, di := range detectorOrder {
		if mask&(1<<di) == 0 {
			continue
		}
		c.cur = di
		findFor(int(di), s, c)
	}
	if c.spill != nil {
		slices.SortFunc(c.spill, func(x, y match) int { return cmp.Compare(x.start, y.start) })
		return
	}
	for i := 1; i < c.n; i++ { // insertion sort: at most len(inl) matches
		for j := i; j > 0 && c.inl[j].start < c.inl[j-1].start; j-- {
			c.inl[j], c.inl[j-1] = c.inl[j-1], c.inl[j]
		}
	}
}

// boundaryBefore and boundaryAfter decide whether a numeric candidate that
// spans s[start:end] is a standalone token. They reject a candidate that
// touches a letter, digit or underscore, or that continues a longer dotted or
// dashed number ("1.2.3.4.5", "2024-01-15-9"), which marks it as a fragment
// of a version string, an identifier or a date.
func boundaryBefore(s string, start int) bool {
	if start == 0 {
		return true
	}
	b := s[start-1]
	if wordByte(b) {
		return false
	}
	if (b == '.' || b == '-') && start >= 2 && isDigit(s[start-2]) {
		return false
	}
	return true
}

func boundaryAfter(s string, end int) bool {
	if end >= len(s) {
		return true
	}
	b := s[end]
	if wordByte(b) {
		return false
	}
	if (b == '.' || b == '-') && end+1 < len(s) && isDigit(s[end+1]) {
		return false
	}
	return true
}
