package anonymize

import (
	"errors"
	"unicode/utf16"
	"unicode/utf8"
)

// Low-level JSON scanning shared by the rewriter. Everything here works on
// byte offsets into the original document and never allocates.

var (
	errJSONSyntax   = errors.New("invalid JSON")
	errJSONEOF      = errors.New("unexpected end of JSON input")
	errJSONTrailing = errors.New("data after the JSON value")
	errJSONDepth    = errors.New("JSON nested too deeply")
	// errLenientEOF ends a rewrite of a truncated document successfully. It
	// is control flow, never returned to callers.
	errLenientEOF = errors.New("end of truncated JSON")
)

// maxJSONDepth bounds nesting. The rewriter is iterative, so depth costs heap
// (a small frame per level) and not stack; the limit only keeps a hostile
// "[[[[..." body from allocating without bound.
const maxJSONDepth = 100_000

const (
	scPlain = iota
	scQuote
	scBackslash
	scControl
)

var strClass [256]uint8

func init() {
	for i := 0; i < 0x20; i++ {
		strClass[i] = scControl
	}
	strClass['"'] = scQuote
	strClass['\\'] = scBackslash
}

// scanString scans the string literal whose opening quote is at src[pos] and
// returns the offset after the closing quote and whether it contained
// escapes. It validates escapes and rejects raw control characters, like
// encoding/json.
func scanString(src []byte, pos int) (end int, esc bool, err error) {
	i := pos + 1
	for i < len(src) {
		switch strClass[src[i]] {
		case scPlain:
			i++
		case scQuote:
			return i + 1, esc, nil
		case scBackslash:
			esc = true
			if i+1 >= len(src) {
				return len(src), true, errJSONEOF
			}
			switch src[i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i += 2
			case 'u':
				for k := i + 2; k < i+6; k++ {
					if k >= len(src) {
						return len(src), true, errJSONEOF
					}
					if !isHex(src[k]) {
						return k, true, errJSONSyntax
					}
				}
				i += 6
			default:
				return i, true, errJSONSyntax
			}
		default:
			return i, esc, errJSONSyntax
		}
	}
	return len(src), esc, errJSONEOF
}

// scanNumber scans a JSON number starting at src[pos] using the strict JSON
// grammar and returns the offset after it.
func scanNumber[T ~string | ~[]byte](src T, pos int) (int, error) {
	n := len(src)
	i := pos
	if i < n && src[i] == '-' {
		i++
	}
	if i >= n {
		return n, errJSONEOF
	}
	switch {
	case src[i] == '0':
		i++
	case src[i] >= '1' && src[i] <= '9':
		for i < n && isDigit(src[i]) {
			i++
		}
	default:
		return i, errJSONSyntax
	}
	if i < n && src[i] == '.' {
		i++
		d := i
		for i < n && isDigit(src[i]) {
			i++
		}
		if i == d {
			if i >= n {
				return n, errJSONEOF
			}
			return i, errJSONSyntax
		}
	}
	if i < n && (src[i] == 'e' || src[i] == 'E') {
		i++
		if i < n && (src[i] == '+' || src[i] == '-') {
			i++
		}
		d := i
		for i < n && isDigit(src[i]) {
			i++
		}
		if i == d {
			if i >= n {
				return n, errJSONEOF
			}
			return i, errJSONSyntax
		}
	}
	return i, nil
}

// validJSONNumber reports whether s is exactly one JSON number.
func validJSONNumber(s string) bool {
	if s == "" {
		return false
	}
	end, err := scanNumber(s, 0)
	return err == nil && end == len(s)
}

func hex4(b []byte) (rune, bool) {
	if len(b) < 4 {
		return 0, false
	}
	var r rune
	for _, c := range b[:4] {
		var d byte
		switch {
		case isDigit(c):
			d = c - '0'
		case c-'a' < 6:
			d = c - 'a' + 10
		case c-'A' < 6:
			d = c - 'A' + 10
		default:
			return 0, false
		}
		r = r<<4 | rune(d)
	}
	return r, true
}

// unescapeJSON appends the decoded form of the string body raw (without the
// quotes) to dst. Lone surrogates decode to U+FFFD, like encoding/json. Raw
// may end in the middle of an escape (a truncated document); the incomplete
// escape is copied through.
func unescapeJSON(dst, raw []byte) []byte {
	for i := 0; i < len(raw); {
		c := raw[i]
		if c != '\\' {
			dst = append(dst, c)
			i++
			continue
		}
		i++
		if i >= len(raw) {
			dst = append(dst, '\\')
			break
		}
		switch raw[i] {
		case '"', '\\', '/':
			dst = append(dst, raw[i])
			i++
		case 'b':
			dst = append(dst, '\b')
			i++
		case 'f':
			dst = append(dst, '\f')
			i++
		case 'n':
			dst = append(dst, '\n')
			i++
		case 'r':
			dst = append(dst, '\r')
			i++
		case 't':
			dst = append(dst, '\t')
			i++
		case 'u':
			r, ok := hex4(raw[i+1:])
			if !ok {
				dst = append(dst, raw[i-1:]...)
				return dst
			}
			i += 5
			if utf16.IsSurrogate(r) {
				if i+1 < len(raw) && raw[i] == '\\' && raw[i+1] == 'u' {
					if r2, ok := hex4(raw[i+2:]); ok {
						if dec := utf16.DecodeRune(r, r2); dec != utf8.RuneError {
							r = dec
							i += 6
						} else {
							r = utf8.RuneError
						}
					} else {
						r = utf8.RuneError
					}
				} else {
					r = utf8.RuneError
				}
			}
			dst = utf8.AppendRune(dst, r)
		default:
			dst = append(dst, '\\', raw[i])
			i++
		}
	}
	return dst
}

// appendJSONString appends s as a JSON string literal. Only the characters
// JSON requires to be escaped are escaped; other bytes (including non-ASCII
// UTF-8) pass through.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		dst = append(dst, s[start:i]...)
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
		}
		start = i + 1
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}
