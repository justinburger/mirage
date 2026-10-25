package anonymize

// Skipping a value: used when a remove rule deletes a member or element. The
// skipped text is still validated, so a malformed document is detected (and
// falls back to text processing) even inside the part that is dropped.

func (w *jsonRW) skipKey() error {
	if w.src[w.pos] != '"' {
		return errJSONSyntax
	}
	end, _, err := scanString(w.src, w.pos)
	if err != nil {
		return w.strErr(err)
	}
	w.pos = end
	w.skipWS()
	if w.pos >= len(w.src) {
		return w.eof()
	}
	if w.src[w.pos] != ':' {
		return errJSONSyntax
	}
	w.pos++
	return nil
}

func (w *jsonRW) strErr(err error) error {
	if err == errJSONEOF {
		return w.eof()
	}
	return err
}

func (w *jsonRW) skipLiteral(lit string) error {
	rest := w.src[w.pos:]
	if len(rest) < len(lit) {
		if string(rest) != lit[:len(rest)] {
			return errJSONSyntax
		}
		return w.eof()
	}
	if string(rest[:len(lit)]) != lit {
		return errJSONSyntax
	}
	w.pos += len(lit)
	return nil
}

// skipValue validates and consumes one value without producing output.
func (w *jsonRW) skipValue() error {
	stk := w.skipStk[:0]
	defer func() { w.skipStk = stk[:0] }()
	for {
		w.skipWS()
		if w.pos >= len(w.src) {
			return w.eof()
		}
		c := w.src[w.pos]
		switch {
		case c == '{' || c == '[':
			if len(stk) >= maxJSONDepth {
				return errJSONDepth
			}
			w.pos++
			w.skipWS()
			if w.pos >= len(w.src) {
				return w.eof()
			}
			closer := byte('}')
			if c == '[' {
				closer = ']'
			}
			if w.src[w.pos] == closer {
				w.pos++ // empty container: the value is complete
				break
			}
			stk = append(stk, c)
			if c == '{' {
				if err := w.skipKey(); err != nil {
					return err
				}
			}
			continue
		case c == '"':
			end, _, err := scanString(w.src, w.pos)
			if err != nil {
				return w.strErr(err)
			}
			w.pos = end
		case c == '-' || isDigit(c):
			end, err := scanNumber(w.src, w.pos)
			if err != nil {
				return w.strErr(err)
			}
			w.pos = end
		case c == 't':
			if err := w.skipLiteral("true"); err != nil {
				return err
			}
		case c == 'f':
			if err := w.skipLiteral("false"); err != nil {
				return err
			}
		case c == 'n':
			if err := w.skipLiteral("null"); err != nil {
				return err
			}
		default:
			return errJSONSyntax
		}
		// A value is complete: close containers or move to the next sibling.
		for {
			if len(stk) == 0 {
				return nil
			}
			w.skipWS()
			if w.pos >= len(w.src) {
				return w.eof()
			}
			top, ch := stk[len(stk)-1], w.src[w.pos]
			if ch == ',' {
				w.pos++
				if top == '{' {
					w.skipWS()
					if w.pos >= len(w.src) {
						return w.eof()
					}
					if err := w.skipKey(); err != nil {
						return err
					}
				}
				break
			}
			if (ch == '}' && top == '{') || (ch == ']' && top == '[') {
				w.pos++
				stk = stk[:len(stk)-1]
				continue
			}
			return errJSONSyntax
		}
	}
}
