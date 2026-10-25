package anonymize

// Byte classification helpers shared by the detectors, the fake generators
// and the URL / JSON scanners. They operate on single bytes so scanning never
// allocates.

func isDigit(c byte) bool { return c-'0' < 10 }
func isUpper(c byte) bool { return c-'A' < 26 }
func isLower(c byte) bool { return c-'a' < 26 }
func isAlpha(c byte) bool { return isUpper(c) || isLower(c) }
func isAlnum(c byte) bool { return isDigit(c) || isAlpha(c) }

func isHex(c byte) bool {
	return isDigit(c) || c-'a' < 6 || c-'A' < 6
}

// wordByte reports whether c can be part of an identifier-like token. Detectors
// refuse to match when the character next to a candidate is a word byte,
// because the candidate is then only a fragment of a larger token (an order
// number, a hash, a version string).
func wordByte(c byte) bool { return isAlnum(c) || c == '_' }

// lowerASCII lower-cases an ASCII letter and leaves everything else alone.
func lowerASCII(c byte) byte {
	if isUpper(c) {
		return c + 'a' - 'A'
	}
	return c
}

// upperASCII upper-cases an ASCII letter and leaves everything else alone.
func upperASCII(c byte) byte {
	if isLower(c) {
		return c - 'a' + 'A'
	}
	return c
}

const hexDigits = "0123456789abcdef"
