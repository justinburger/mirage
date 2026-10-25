package pathnorm

import "strings"

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
)

// classify returns the placeholder for one raw path segment, or "" when the
// segment is kept literally.
func classify(seg string) string {
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
	switch {
	case digits == n:
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

// isEmail reports whether a raw path segment looks like an email address:
// a non-empty local part, an "@" (or its escape "%40"), and a dotted domain
// with a TLD of at least two characters containing a letter.
func isEmail(s string) bool {
	at, atLen := strings.LastIndexByte(s, '@'), 1
	if at < 0 {
		at, atLen = strings.LastIndex(s, "%40"), 3
		if at < 0 {
			return false
		}
	}
	local, domain := s[:at], s[at+atLen:]
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
