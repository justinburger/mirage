package anonymize

import (
	"strings"
)

// ---- payment cards --------------------------------------------------------

const maxCardGroups = 24

// findCard finds Luhn-valid 13-19 digit numbers, written as one run of digits
// or in groups separated by single spaces or dashes.
//
// A "chain" is a maximal sequence of digit groups joined by single separators.
// Every contiguous window of groups inside a chain is tried, leftmost and
// longest first, so a card that follows a date ("2024-01-15 4111111111111111")
// is still found even though the whole chain is longer than 19 digits.
func findCard(s string, c *claims) {
	n := len(s)
	var gs, ge [maxCardGroups]int
	for i := 0; i < n; {
		if !isDigit(s[i]) {
			i++
			continue
		}
		if i > 0 && (wordByte(s[i-1]) || (s[i-1] == '.' && i >= 2 && isDigit(s[i-2]))) {
			for i < n && isDigit(s[i]) {
				i++
			}
			continue
		}
		ng, j := 0, i
		for ng < maxCardGroups {
			st := j
			for j < n && isDigit(s[j]) {
				j++
			}
			gs[ng], ge[ng] = st, j
			ng++
			if j+1 < n && (s[j] == ' ' || s[j] == '-') && isDigit(s[j+1]) {
				j++
				continue
			}
			break
		}
		endOK := j >= n || !(wordByte(s[j]) || (s[j] == '.' && j+1 < n && isDigit(s[j+1])))
		for a := 0; a < ng; {
			found := false
			for b := ng - 1; b >= a; b-- {
				if b == ng-1 && !endOK {
					continue
				}
				if cardWindow(s, gs[:], ge[:], a, b) && c.add(gs[a], ge[b]) {
					a, found = b+1, true
					break
				}
			}
			if !found {
				a++
			}
		}
		i = j
	}
}

// cardWindow reports whether groups a..b of a chain form a payment card
// number: 13-19 digits in a card-like grouping, a plausible leading digit and
// a valid Luhn checksum.
func cardWindow(s string, gs, ge []int, a, b int) bool {
	total := cardDigits(gs, ge, a, b)
	if total < 13 || total > 19 || !cardGrouping(gs, ge, a, b) {
		return false
	}
	first := s[gs[a]]
	// A card number never starts with 0. A leading 1 is only used by 15-digit
	// UATP cards; rejecting it for other lengths keeps Unix timestamps in
	// milliseconds, microseconds and nanoseconds and Snowflake IDs out.
	if first == '0' || (first == '1' && total != 15) {
		return false
	}
	sum, alt, same := 0, false, true
	for k := b; k >= a; k-- {
		for p := ge[k] - 1; p >= gs[k]; p-- {
			if s[p] != first {
				same = false
			}
			d := int(s[p] - '0')
			if alt {
				if d *= 2; d > 9 {
					d -= 9
				}
			}
			sum += d
			alt = !alt
		}
	}
	return !same && sum%10 == 0
}

// cardDigits counts the digits of groups a..b. Groups are consecutive in the
// string except for one separator byte between neighbours, so the count is
// the span minus the separators.
func cardDigits(gs, ge []int, a, b int) int { return ge[b] - gs[a] - (b - a) }

// cardGrouping accepts the layouts cards are printed in: a single run, 4-4-4-4
// (the last group may be shorter for 13- and 19-digit numbers), and the
// 4-6-5 / 4-6-4 layouts of American Express and Diners Club.
func cardGrouping(gs, ge []int, a, b int) bool {
	cnt := b - a + 1
	if cnt == 1 {
		return true
	}
	ln := func(k int) int { return ge[k] - gs[k] }
	if cnt == 3 && ln(a) == 4 && ln(a+1) == 6 && (ln(a+2) == 4 || ln(a+2) == 5) {
		return true
	}
	for k := a; k < b; k++ {
		if ln(k) != 4 {
			return false
		}
	}
	return ln(b) >= 1 && ln(b) <= 4
}

// luhnValid reports whether the digits of s (non-digits ignored) pass the Luhn
// checksum. It is used to validate test fixtures.
func luhnValid(s string) bool {
	sum, alt, n := 0, false, 0
	for i := len(s) - 1; i >= 0; i-- {
		if !isDigit(s[i]) {
			continue
		}
		d := int(s[i] - '0')
		if alt {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
		n++
	}
	return n > 0 && sum%10 == 0
}

// plainCardNumber reports whether a JSON number token is an integer literal
// that the card detector would accept.
func plainCardNumber(tok []byte) bool {
	if len(tok) < 13 || len(tok) > 19 {
		return false
	}
	for _, ch := range tok {
		if !isDigit(ch) {
			return false
		}
	}
	var gs, ge [1]int
	gs[0], ge[0] = 0, len(tok)
	return cardWindow(string(tok), gs[:], ge[:], 0, 0)
}

// ---- US social security numbers -------------------------------------------

func findSSN(s string, c *claims) {
	n := len(s)
	from := 0
	for from < n {
		p := strings.IndexByte(s[from:], '-')
		if p < 0 {
			return
		}
		p += from
		from = p + 1
		st, end := p-3, p+8
		if st < 0 || end > n || s[p+3] != '-' {
			continue
		}
		if !digitsAt(s, st, 3) || !digitsAt(s, p+1, 2) || !digitsAt(s, p+4, 4) {
			continue
		}
		if !boundaryBefore(s, st) || !boundaryAfter(s, end) {
			continue
		}
		if !ssnRangeOK(s[st:st+3]) || !ssnGroupOK(s[st:st+3], s[p+1:p+3]) || s[p+4:end] == "0000" {
			continue
		}
		if c.add(st, end) {
			from = end
		}
	}
}

func digitsAt(s string, i, n int) bool {
	if i < 0 || i+n > len(s) {
		return false
	}
	for k := i; k < i+n; k++ {
		if !isDigit(s[k]) {
			return false
		}
	}
	return true
}

// ssnRangeOK applies the invalid-range exclusions: area 000 and 666 are never
// issued. Areas 900-999 are not social security numbers either, except that
// individual taxpayer identification numbers (ITINs, 9xx-7x-xxxx and friends)
// are just as sensitive, so those are checked by ssnGroupOK.
func ssnRangeOK(area string) bool { return area != "000" && area != "666" }

func ssnGroupOK(area, group string) bool {
	if group == "00" {
		return false
	}
	if area[0] != '9' {
		return true
	}
	g := int(group[0]-'0')*10 + int(group[1]-'0')
	return (g >= 50 && g <= 65) || (g >= 70 && g <= 88) || (g >= 90 && g <= 92) || (g >= 94 && g <= 99)
}

// ---- phone numbers --------------------------------------------------------

func findPhone(s string, c *claims) {
	n := len(s)
	for i := 0; i < n; {
		ch := s[i]
		switch {
		case ch == '+':
			if end, ok := matchE164(s, i); ok && c.add(i, end) {
				i = end
				continue
			}
			i++
		case ch == '(' || isDigit(ch):
			if end, ok := matchNANP(s, i); ok && c.add(i, end) {
				i = end
				continue
			}
			if isDigit(ch) {
				for i < n && isDigit(s[i]) {
					i++
				}
			} else {
				i++
			}
		default:
			i++
		}
	}
}

func phoneBoundaryBefore(s string, i int) bool {
	return i == 0 || (s[i-1] != '+' && boundaryBefore(s, i))
}

func isPhoneSep(c byte) bool { return c == ' ' || c == '-' || c == '.' }

// matchE164 matches "+" followed by 7-15 digits, which may be separated by
// spaces, dashes, dots and parentheses ("+44 20 7946 0958", "+1 (415) 555-2671").
func matchE164(s string, i int) (end int, ok bool) {
	n := len(s)
	if i > 0 && wordByte(s[i-1]) {
		return 0, false
	}
	j := i + 1
	if j >= n || s[j] < '1' || s[j] > '9' {
		return 0, false
	}
	digits, last, seps := 0, j, 0
	for j < n {
		ch := s[j]
		switch {
		case isDigit(ch):
			if digits++; digits > 15 {
				return 0, false
			}
			j++
			last, seps = j, 0
		case (isPhoneSep(ch) || ch == '(' || ch == ')') && seps < 2:
			seps++
			j++
		default:
			j = n
		}
	}
	if digits < 7 || (last < n && wordByte(s[last])) {
		return 0, false
	}
	return last, true
}

func nanpArea(s string, p int) bool {
	return p+3 <= len(s) && s[p] >= '2' && s[p] <= '9' && isDigit(s[p+1]) && isDigit(s[p+2])
}

// matchNANP matches North American numbers: (415) 555-2671, 415-555-2671,
// 415.555.2671, 415 555 2671, each optionally preceded by "1" and a
// separator. Separators are required, so a bare run of ten digits (an ID, a
// timestamp) is never a phone number, and without parentheses the two
// separators must be the same character.
func matchNANP(s string, i int) (end int, ok bool) {
	n := len(s)
	if !phoneBoundaryBefore(s, i) {
		return 0, false
	}
	p := i
	if s[p] == '1' && p+2 < n && isPhoneSep(s[p+1]) && (s[p+2] == '(' || isDigit(s[p+2])) {
		p += 2
	}
	var sep byte
	paren := false
	if p < n && s[p] == '(' {
		paren = true
		p++
		if !nanpArea(s, p) {
			return 0, false
		}
		p += 3
		if p >= n || s[p] != ')' {
			return 0, false
		}
		p++
		if p < n && isPhoneSep(s[p]) {
			p++
		}
	} else {
		if p >= n || !nanpArea(s, p) {
			return 0, false
		}
		p += 3
		if p >= n || !isPhoneSep(s[p]) {
			return 0, false
		}
		sep = s[p]
		p++
	}
	if !nanpArea(s, p) { // exchange has the same shape: [2-9]xx
		return 0, false
	}
	p += 3
	if p >= n || !isPhoneSep(s[p]) || (!paren && s[p] != sep) {
		return 0, false
	}
	p++
	if !digitsAt(s, p, 4) {
		return 0, false
	}
	p += 4
	if !boundaryAfter(s, p) {
		return 0, false
	}
	return p, true
}

// ---- IBAN -----------------------------------------------------------------

// ibanLengths maps a country code to its IBAN length. It covers the SWIFT
// registry, including the countries that use the format without having
// joined the IBAN scheme.
const ibanLengths = "AD24AE23AL28AT20AZ28BA20BE16BG22BH22BI27BR29BY28CH21CR22CY28CZ24DE22DJ27DK18DO28DZ26" +
	"EE20EG29ES24FI18FK18FO18FR27GB22GE22GI23GL18GR27GT28HN28HR21HU28IE22IL23IQ23IR26IS26IT27JO30KW30KZ20" +
	"LB28LC32LI21LT20LU20LV21LY25MA28MC27MD24ME22MG27MK19ML28MN20MR27MT31MU30NE28NI28NL18NO15OM23PK24PL28" +
	"PS29PT25QA29RO24RS22RU33SA24SC31SD18SE24SI19SK24SM27SN28SO23ST25SV28TL23TN24TR26UA29VA22VG24XK20YE30" +
	"BF28BJ28CF27CG27CI28CM27CV25GA27GQ27GW25KM27TD27TG28"

var ibanLen [26][26]uint8

func init() {
	for i := 0; i+4 <= len(ibanLengths); i += 4 {
		l := int(ibanLengths[i+2]-'0')*10 + int(ibanLengths[i+3]-'0')
		ibanLen[ibanLengths[i]-'A'][ibanLengths[i+1]-'A'] = uint8(l)
	}
}

func findIBAN(s string, c *claims) {
	n := len(s)
	for i := 0; i+4 <= n; i++ {
		a, b := upperASCII(s[i]), upperASCII(s[i+1])
		if !isUpper(a) || !isUpper(b) || !isDigit(s[i+2]) || !isDigit(s[i+3]) {
			continue
		}
		if i > 0 && isAlnum(s[i-1]) {
			continue
		}
		want := int(ibanLen[a-'A'][b-'A'])
		if want == 0 {
			continue
		}
		var buf [34]byte
		cnt, j := 0, i
		for j < n && cnt < want {
			ch := s[j]
			if isAlnum(ch) {
				buf[cnt] = upperASCII(ch)
				cnt++
				j++
			} else if ch == ' ' && cnt%4 == 0 && cnt > 0 && j+1 < n && isAlnum(s[j+1]) {
				j++
			} else {
				break
			}
		}
		if cnt != want || (j < n && isAlnum(s[j])) || !ibanChecksum(buf[:cnt]) {
			continue
		}
		if c.add(i, j) {
			i = j - 1
		}
	}
}

// ibanChecksum validates the mod-97 check of an upper-case compact IBAN.
func ibanChecksum(b []byte) bool {
	rem := 0
	feed := func(ch byte) {
		if isDigit(ch) {
			rem = (rem*10 + int(ch-'0')) % 97
		} else {
			rem = (rem*100 + int(ch-'A') + 10) % 97
		}
	}
	for _, ch := range b[4:] {
		feed(ch)
	}
	for _, ch := range b[:4] {
		feed(ch)
	}
	return rem == 1
}
