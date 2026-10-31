package anonymize

// Key names in query, cookie and form selectors and in JSONPath recursive
// descent ("$..api_key") are compared ignoring ASCII case and the characters
// "_" and "-": api_key matches apiKey, API-KEY and apikey. Anything else
// (including non-ASCII letters) is compared as written. A form or query name
// like "user[password]" also matches by its last bracketed part.

// keyFold maps a byte of a key to its normalized form, or to -1 for a byte
// that is ignored.
var keyFold [256]int16

func init() {
	for i := range keyFold {
		keyFold[i] = int16(i)
	}
	for c := 'A'; c <= 'Z'; c++ {
		keyFold[c] = int16(c + 'a' - 'A')
	}
	keyFold['_'], keyFold['-'] = -1, -1
}

// appendNormKey appends the normalized form of key to dst.
func appendNormKey[T ~string | ~[]byte](dst []byte, key T) []byte {
	i := 0
	for ; i < len(key); i++ {
		if keyFold[key[i]] != int16(key[i]) {
			break
		}
	}
	dst = append(dst, key[:i]...)
	for ; i < len(key); i++ {
		if f := keyFold[key[i]]; f >= 0 {
			dst = append(dst, byte(f))
		}
	}
	return dst
}

// foldKey returns the normalized form of key. A key that is already
// normalized (the usual plain lower-case name) is returned as it is, without
// a copy; otherwise the result is built in *scratch.
//
//go:noinline
func foldKey(scratch *[]byte, key []byte) []byte {
	for i, c := range key {
		if keyFold[c] != int16(c) {
			*scratch = append((*scratch)[:0], key[:i]...)
			for _, c := range key[i:] {
				if f := keyFold[c]; f >= 0 {
					*scratch = append(*scratch, byte(f))
				}
			}
			return *scratch
		}
	}
	return key
}

// normKey returns the normalized form of a key name, see the comment above.
func normKey(s string) string {
	for i := 0; i < len(s); i++ {
		if keyFold[s[i]] != int16(s[i]) {
			return string(appendNormKey(make([]byte, 0, len(s)), s))
		}
	}
	return s
}

// normEqual reports whether the key, normalized, equals norm (which must
// already be normalized). It allocates nothing.
func normEqual(key []byte, norm string) bool {
	j := 0
	for _, c := range key {
		f := keyFold[c]
		if f < 0 {
			continue
		}
		if j >= len(norm) || norm[j] != byte(f) {
			return false
		}
		j++
	}
	return j == len(norm)
}

// bracketPart returns the last non-empty bracketed part of a form or query
// name: "user[password]" gives "password", "a[b][c]" gives "c", and trailing
// empty brackets are skipped ("passwords[]" has none, "a[b][]" gives "b").
// It returns "" when there is none.
func bracketPart(name string) string {
	for len(name) >= 2 && name[len(name)-2:] == "[]" {
		name = name[:len(name)-2]
	}
	if len(name) < 3 || name[len(name)-1] != ']' {
		return ""
	}
	lb := -1
	for i := len(name) - 2; i >= 0; i-- {
		if name[i] == '[' {
			lb = i
			break
		}
	}
	if lb < 0 || lb+1 >= len(name)-1 {
		return ""
	}
	return name[lb+1 : len(name)-1]
}

// keyedRules returns the rules a query parameter, cookie, form field or
// multipart part called name selects: those registered under its normalized
// name and, when brackets is set, those registered under the normalized last
// bracketed part of the name ("user[password]" is selected by a rule for
// "password"). The rules come in profile order. The result may alias scratch
// memory and is valid until the next call.
func (r *run) keyedRules(m map[string][]*rule, name string, brackets bool) []*rule {
	if len(m) == 0 || name == "" {
		return nil
	}
	var buf, buf2 [48]byte
	nk := appendNormKey(buf[:0], name)
	sel := m[string(nk)]
	if brackets {
		if part := bracketPart(name); part != "" {
			np := appendNormKey(buf2[:0], part)
			if other := m[string(np)]; len(other) > 0 && string(np) != string(nk) {
				sel = r.mergeSel(sel, other)
			}
		}
	}
	return sel
}

// mergeSel merges two lists of selector rules, each in profile order, into
// one. The result aliases scratch memory.
func (r *run) mergeSel(a, b []*rule) []*rule {
	if len(a) == 0 {
		return b
	}
	out := r.selBuf[:0]
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i].idx < b[j].idx {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	out = append(out, a[i:]...)
	out = append(out, b[j:]...)
	r.selBuf = out
	return out
}
