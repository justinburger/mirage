package pathnorm

import (
	"net/url"
	"strings"
)

// Placeholders emitted by the heuristic classifier.
const (
	tokID    = "{id}"
	tokUUID  = "{uuid}"
	tokHex   = "{hex}"
	tokToken = "{token}"
	tokEmail = "{email}"
	tokDate  = "{date}"
)

const (
	minHexLen   = 16
	minTokenLen = 20
	uuidLen     = 36
	dateLen     = len("2024-01-31")

	// Mirage's own anonymization hash tokens: "h_" followed by exactly
	// hashHexLen lowercase hex characters.
	hashPrefix   = "h_"
	hashHexLen   = 16
	hashTokenLen = len(hashPrefix) + hashHexLen
)

// classify returns the placeholder for one path segment as it appears in an
// escaped path, or "" when the segment is kept literally. The segment is
// classified percent-decoded ("jane%40acme.io" is an {email}); one that fails
// to decode is classified as it is. Segments without a '%' are not copied.
func classify(seg string) string { return classifyText(seg, true) }

// classifyText classifies seg. With decode set, a seg containing a '%' is
// first percent-decoded and the result classified (never decoded again); a
// seg that fails to decode, or any seg with decode unset, is classified as
// it is, a '%' being an ordinary character.
func classifyText(seg string, decode bool) string {
	n := len(seg)
	if n == 0 {
		return ""
	}
	var (
		digits   int // 0-9
		hexAlpha int // a-f, A-F
		alpha    int // every ASCII letter, hex or not
		dashes   int
		other    int // anything not counted above (underscore counts as token-safe)
		unders   int
	)
	for i := 0; i < n; i++ {
		c := seg[i]
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
			hexAlpha++
			alpha++
		case c >= 'g' && c <= 'z', c >= 'G' && c <= 'Z':
			alpha++
		case c == '-':
			dashes++
		case c == '_':
			unders++
		default:
			other++
		}
	}
	if digits == n {
		return tokID
	}
	// A '%' counts as "other", so segments made only of letters, digits, '-'
	// and '_' (most of them) never get here.
	if decode && other > 0 && strings.IndexByte(seg, '%') >= 0 {
		if dec, err := url.PathUnescape(seg); err == nil {
			return classifyText(dec, false)
		}
	}
	switch {
	case n == hashTokenLen && unders == 1 && other == 0 && isHashToken(seg):
		return tokID
	case other == 0 && dashes == 0 && unders == 0 && alpha == hexAlpha:
		// Only hex digits. All-digit segments were handled above.
		if n >= minHexLen {
			return tokHex
		}
		return ""
	case n == uuidLen && dashes == 4 && other == 0 && unders == 0 && isUUID(seg):
		return tokUUID
	case n >= minTokenLen && other == 0 && alpha > 0 && digits > 0:
		return tokToken
	case other > 0 && isEmail(seg):
		return tokEmail
	case n == dateLen && dashes == 2 && digits == 8 && isISODate(seg):
		return tokDate
	}
	return ""
}

// isUUID reports whether s is the canonical 8-4-4-4-12 hexadecimal form.
// Any version and any letter case is accepted.
func isUUID(s string) bool {
	if len(s) != uuidLen {
		return false
	}
	for i := 0; i < uuidLen; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHexDigit(c) {
				return false
			}
		}
	}
	return true
}

// isHashToken reports whether s is one of Mirage's anonymization hash tokens:
// "h_" followed by exactly 16 lowercase hex characters.
func isHashToken(s string) bool {
	if len(s) != hashTokenLen || s[:len(hashPrefix)] != hashPrefix {
		return false
	}
	for i := len(hashPrefix); i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// isISODate reports whether s is a valid calendar date in YYYY-MM-DD form.
func isISODate(s string) bool {
	if len(s) != dateLen || s[4] != '-' || s[7] != '-' {
		return false
	}
	for _, i := range [...]int{0, 1, 2, 3, 5, 6, 8, 9} {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	year := int(s[0]-'0')*1000 + int(s[1]-'0')*100 + int(s[2]-'0')*10 + int(s[3]-'0')
	month := int(s[5]-'0')*10 + int(s[6]-'0')
	day := int(s[8]-'0')*10 + int(s[9]-'0')
	if month < 1 || month > 12 || day < 1 {
		return false
	}
	return day <= daysIn(year, month)
}

func daysIn(year, month int) int {
	switch month {
	case 2:
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	}
	return 31
}

// isEmail reports whether a decoded path segment looks like an email
// address: a non-empty local part, an "@", and a dotted domain with a TLD of
// at least two characters containing a letter.
func isEmail(s string) bool {
	at := strings.LastIndexByte(s, '@')
	if at < 0 {
		return false
	}
	local, domain := s[:at], s[at+1:]
	if local == "" || domain == "" {
		return false
	}
	for i := 0; i < len(local); i++ {
		if c := local[i]; c <= ' ' || c == 0x7f || c == '@' {
			return false
		}
	}
	dot := strings.LastIndexByte(domain, '.')
	if dot < 0 {
		return false
	}
	tld := domain[dot+1:]
	if len(tld) < 2 || !hasLetter(tld) {
		return false
	}
	// Every label: non-empty, no leading or trailing hyphen, hostname bytes
	// only (bytes >= 0x80 are allowed for internationalised names).
	for label := range strings.SplitSeq(domain, ".") {
		if label == "" || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c >= 0x80) {
				return false
			}
		}
	}
	return true
}

func hasLetter(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80 {
			return true
		}
	}
	return false
}
