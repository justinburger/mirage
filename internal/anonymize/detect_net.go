package anonymize

import (
	"encoding/base64"
	"encoding/json"
	"net/netip"
	"strings"
)

// ---- e-mail ---------------------------------------------------------------

// isLocalByte reports whether c may appear in the local part of an address as
// Mirage recognises it. This is a deliberately practical subset of RFC 5322:
// the exotic atext characters (= & ! # $ * / ? ^ ` { | } ~) are excluded so
// that "q=1&user=jane@example.com" yields "jane@example.com" and not the whole
// query fragment.
func isLocalByte(c byte) bool {
	return isAlnum(c) || c == '.' || c == '_' || c == '%' || c == '+' || c == '-' || c == '\'' || c >= 0x80
}

func isLabelByte(c byte) bool { return isAlnum(c) || c == '-' || c >= 0x80 }

// atFinder walks the "@" and percent-encoded "%40" positions of a string in
// order. It remembers the next hit of each kind, so the whole walk is linear
// however many of either there are.
type atFinder struct {
	s       string
	nextAt  int // next '@' at or after the cursor, -1 if none
	nextPct int // next "%40" at or after the cursor, -1 if none
}

func newAtFinder(s string) atFinder {
	return atFinder{s: s, nextAt: strings.IndexByte(s, '@'), nextPct: strings.Index(s, "%40")}
}

// next returns the position and width of the first marker at or after from.
func (f *atFinder) next(from int) (pos, width int) {
	if f.nextAt >= 0 && f.nextAt < from {
		if i := strings.IndexByte(f.s[from:], '@'); i >= 0 {
			f.nextAt = from + i
		} else {
			f.nextAt = -1
		}
	}
	if f.nextPct >= 0 && f.nextPct < from {
		if i := strings.Index(f.s[from:], "%40"); i >= 0 {
			f.nextPct = from + i
		} else {
			f.nextPct = -1
		}
	}
	switch {
	case f.nextAt < 0 && f.nextPct < 0:
		return -1, 0
	case f.nextPct < 0 || (f.nextAt >= 0 && f.nextAt < f.nextPct):
		return f.nextAt, 1
	default:
		return f.nextPct, 3
	}
}

func findEmail(s string, c *claims) {
	from := 0
	f := newAtFinder(s)
	for from < len(s) {
		at, width := f.next(from)
		if at < 0 {
			return
		}
		from = at + width
		// Local part: walk left over local-part bytes, then trim leading
		// punctuation that cannot start an address.
		ls := at
		for ls > 0 && isLocalByte(s[ls-1]) && at-ls < 254 {
			ls--
		}
		for ls < at && !(isAlnum(s[ls]) || s[ls] == '_' || s[ls] >= 0x80) {
			ls++
		}
		if ls == at {
			continue
		}
		// Domain: dot-separated labels.
		de := at + width
		dots := 0
		lastLabel := de
		for {
			l0 := de
			if de < len(s) && s[de] == '-' { // a label cannot start with a hyphen
				break
			}
			for de < len(s) && isLabelByte(s[de]) {
				de++
			}
			if de == l0 {
				break
			}
			lastLabel = l0
			if de+1 < len(s) && s[de] == '.' && isLabelByte(s[de+1]) {
				dots++
				de++
				continue
			}
			break
		}
		for de > lastLabel && s[de-1] == '-' {
			de--
		}
		if dots == 0 || de-lastLabel < 2 {
			continue
		}
		tld := s[lastLabel:de]
		hasLetter := false
		for i := 0; i < len(tld); i++ {
			if !isDigit(tld[i]) {
				hasLetter = true
				break
			}
		}
		if !hasLetter {
			continue
		}
		if c.addEnc(ls, de, width == 3) {
			from = de
		}
	}
}

// ---- IPv4 -----------------------------------------------------------------

func findIPv4(s string, c *claims) {
	n := len(s)
	for i := 0; i < n; {
		if !isDigit(s[i]) {
			i++
			continue
		}
		runEnd := i
		for runEnd < n && isDigit(s[runEnd]) {
			runEnd++
		}
		if end, ok := parseIPv4(s, i); ok && ipv4Boundary(s, i, end) && c.add(i, end) {
			i = end
			continue
		}
		i = runEnd
	}
}

// parseIPv4 parses a dotted quad starting at s[i]. Octets have 1-3 digits, no
// leading zeros and a value of at most 255.
func parseIPv4(s string, i int) (end int, ok bool) {
	n := len(s)
	for k := 0; k < 4; k++ {
		if k > 0 {
			if i >= n || s[i] != '.' {
				return 0, false
			}
			i++
		}
		st, v := i, 0
		for i < n && isDigit(s[i]) && i-st < 3 {
			v = v*10 + int(s[i]-'0')
			i++
		}
		if i == st || v > 255 || (i-st > 1 && s[st] == '0') {
			return 0, false
		}
		if k == 3 && i < n && isDigit(s[i]) {
			return 0, false
		}
	}
	return i, true
}

func ipv4Boundary(s string, start, end int) bool {
	if !boundaryBefore(s, start) || !boundaryAfter(s, end) {
		return false
	}
	// "Chrome/120.0.0.0" is a product version, not an address.
	if start >= 2 && s[start-1] == '/' && isAlpha(s[start-2]) {
		return false
	}
	return true
}

// ---- IPv6 -----------------------------------------------------------------

func isIPv6Byte(c byte) bool { return isHex(c) || c == ':' || c == '.' }

func findIPv6(s string, c *claims) {
	n := len(s)
	for i := 0; i < n; {
		if !isIPv6Byte(s[i]) {
			i++
			continue
		}
		j := i
		for j < n && isIPv6Byte(s[j]) {
			j++
		}
		if (i == 0 || !wordByte(s[i-1])) && (j == n || !wordByte(s[j])) {
			start, end := i, j
			for start < end && s[start] == '.' {
				start++
			}
			for end > start && s[end-1] == '.' {
				end--
			}
			if end-start >= 2 && s[end-1] == ':' && s[end-2] != ':' {
				end--
			}
			if cand := s[start:end]; plausibleIPv6(cand) {
				if a, err := netip.ParseAddr(cand); err == nil && a.Is6() {
					c.add(start, end)
				}
			}
		}
		i = j
	}
}

// plausibleIPv6 is a cheap structural test run before netip.ParseAddr so that
// timestamps ("10:30:00") and MAC addresses never reach the parser, whose
// failures allocate.
func plausibleIPv6(s string) bool {
	if len(s) < 2 || len(s) > 45 {
		return false
	}
	colons, hasDouble, hasDot, hasHex := 0, false, false, false
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; {
		case ch == ':':
			colons++
			if i > 0 && s[i-1] == ':' {
				hasDouble = true
			}
		case ch == '.':
			hasDot = true
		case isHex(ch):
			hasHex = true
		}
	}
	if colons < 2 || !hasHex {
		return false
	}
	if hasDouble {
		return colons <= 8
	}
	return colons == 7 || (hasDot && colons == 6)
}

// ---- UUID -----------------------------------------------------------------

func findUUID(s string, c *claims) {
	from := 0
	for from < len(s) {
		p := strings.IndexByte(s[from:], '-')
		if p < 0 {
			return
		}
		p += from
		from = p + 1
		st := p - 8
		if st < 0 || st+36 > len(s) || !isUUID(s[st:st+36]) {
			continue
		}
		if st > 0 && isAlnum(s[st-1]) {
			continue
		}
		if st+36 < len(s) && isAlnum(s[st+36]) {
			continue
		}
		if c.add(st, st+36) {
			from = st + 36
		}
	}
}

// isUUID reports whether s is exactly a UUID in 8-4-4-4-12 form.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if !isHex(s[i]) {
				return false
			}
		}
	}
	return true
}

// ---- JWT ------------------------------------------------------------------

func isB64URL(c byte) bool { return isAlnum(c) || c == '-' || c == '_' }

func findJWT(s string, c *claims) {
	n := len(s)
	from := 0
	for from < n {
		i := strings.Index(s[from:], "ey")
		if i < 0 {
			return
		}
		st := from + i
		from = st + 2
		if st > 0 && (isB64URL(s[st-1]) || s[st-1] == '.') {
			continue
		}
		j := st
		for j < n && isB64URL(s[j]) {
			j++
		}
		if j >= n || s[j] != '.' {
			continue
		}
		k := j + 1
		for k < n && isB64URL(s[k]) {
			k++
		}
		if k == j+1 || k >= n || s[k] != '.' {
			continue
		}
		m := k + 1
		for m < n && isB64URL(s[m]) {
			m++
		}
		if m == k+1 {
			continue
		}
		// A fourth segment means this is not a three-part JWT.
		if m+1 < n && s[m] == '.' && isB64URL(s[m+1]) {
			continue
		}
		if !jwtHeaderOK(s[st:j]) {
			continue
		}
		if c.add(st, m) {
			from = m
		}
	}
}

// jwtHeaderOK reports whether seg is a base64url-encoded JSON object.
func jwtHeaderOK(seg string) bool {
	if len(seg) < 4 || len(seg) > 2048 {
		return false
	}
	dec, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil || len(dec) < 2 || dec[0] != '{' {
		return false
	}
	return json.Valid(dec)
}

// ---- AWS access key IDs ---------------------------------------------------

func findAWSKey(s string, c *claims) {
	n := len(s)
	for _, pfx := range [...]string{"AKIA", "ASIA"} {
		from := 0
		for from < n {
			i := strings.Index(s[from:], pfx)
			if i < 0 {
				break
			}
			st := from + i
			from = st + 1
			if st+20 > n || (st > 0 && isAlnum(s[st-1])) || (st+20 < n && isAlnum(s[st+20])) {
				continue
			}
			ok := true
			for k := st + 4; k < st+20; k++ {
				if !isUpper(s[k]) && !isDigit(s[k]) {
					ok = false
					break
				}
			}
			if ok && c.add(st, st+20) {
				from = st + 20
			}
		}
	}
}
