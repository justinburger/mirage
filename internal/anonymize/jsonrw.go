package anonymize

import (
	"bytes"
	"errors"
	"sync"
)

// The JSON rewriter walks a document once, left to right, and writes a
// compact copy with the requested changes. It never builds a tree, so it
// keeps member order, number literals and anything it does not touch exactly
// as they were, handles documents of any depth (the walk is iterative), and
// costs O(size) no matter how many rules there are.
//
// JSONPath rules run as a small NFA: every container carries the set of
// (rule, step) states reached on the way down; a child's states come from its
// parent's states and the child's key or index. A state past the last step is
// a match. A match on a container is inherited by everything below it (the
// "active" rules), so the strategy applies to every string and number leaf
// beneath. Remove deletes the matched member, element or subtree.
//
// If the walk fails (the input is not valid JSON) the caller discards the
// result and falls back to text processing.

// jstate packs a rule index and a step position.
type jstate uint32

func mkState(rule, pos int) jstate { return jstate(rule)<<16 | jstate(pos) }
func (s jstate) rule() int         { return int(s >> 16) }
func (s jstate) pos() int          { return int(s & 0xffff) }

type jframe struct {
	obj     bool
	emitted bool // a member or element has been written (a comma is due)
	idx     int  // next array index
	// Windows into jsonRW.states and jsonRW.active for this container node.
	stLo, stHi int
	acLo, acHi int
	pathLen    int // len(path) including this container's own segment
}

// childInfo is the result of entering a child node: its state window, its
// active-rule window, and the rule that deletes it, if any.
type childInfo struct {
	stLo, stHi int
	acLo, acHi int
	remove     *rule
}

type jsonRW struct {
	r         *run
	src       []byte
	pos       int
	out       []byte
	lenient   bool
	changed   bool
	reporting bool
	loc       locator
	prog      []*rule
	states    []jstate
	active    []int32
	stack     []jframe
	path      []byte
	chain     []*rule
	tmp       []byte
	skipStk   []byte
}

var jsonPool = sync.Pool{New: func() any { return new(jsonRW) }}

// rewriteJSON rewrites src, a single JSON value, for the current side of r.
// In lenient mode a document that ends early (a truncated capture) is
// processed as far as it goes. It reports whether anything changed; when
// nothing changed the returned slice is nil and src should be kept as is.
func (r *run) rewriteJSON(src []byte, lenient bool, loc locator) ([]byte, bool, error) {
	w := jsonPool.Get().(*jsonRW)
	w.r, w.src, w.pos = r, src, 0
	w.lenient, w.changed, w.reporting = lenient, false, r.rep != nil
	w.loc = loc
	w.loc.json = nil
	w.prog = r.sd.json
	w.out = w.out[:0]
	w.states, w.active, w.stack = w.states[:0], w.active[:0], w.stack[:0]
	w.path = w.path[:0]
	if w.reporting {
		w.path = append(w.path, '$')
	}
	err := w.run()
	var out []byte
	changed := err == nil && w.changed
	if changed {
		out = bytes.Clone(w.out)
	}
	w.release()
	jsonPool.Put(w)
	return out, changed, err
}

func (w *jsonRW) release() {
	w.r, w.src, w.prog, w.loc = nil, nil, nil, locator{}
	for i := range w.chain {
		w.chain[i] = nil
	}
	if cap(w.out) > 1<<20 {
		w.out = nil
	}
	if cap(w.stack) > 4096 {
		w.stack = nil
	}
}

func (w *jsonRW) run() error {
	err := w.loop()
	if errors.Is(err, errLenientEOF) {
		return nil
	}
	return err
}

func (w *jsonRW) eof() error {
	if w.lenient {
		return errLenientEOF
	}
	return errJSONEOF
}

func (w *jsonRW) skipWS() {
	for w.pos < len(w.src) {
		switch w.src[w.pos] {
		case ' ', '\t', '\n', '\r':
			w.pos++
		default:
			return
		}
	}
}

const (
	stRoot = iota
	stObjFirst
	stObjKey
	stArrFirst
	stArrElem
	stAfter
)

func (w *jsonRW) openState() int {
	if w.stack[len(w.stack)-1].obj {
		return stObjFirst
	}
	return stArrFirst
}

func (w *jsonRW) loop() error {
	state := stRoot
	for {
		switch state {
		case stRoot:
			w.skipWS()
			if w.pos >= len(w.src) {
				return w.eof()
			}
			info := w.enter(nil, nil, 0)
			pushed, err := w.value(&info)
			if err != nil {
				return err
			}
			if pushed {
				state = w.openState()
			} else {
				w.leave(&info)
				state = stAfter
			}

		case stObjFirst, stObjKey:
			w.skipWS()
			if w.pos >= len(w.src) {
				return w.eof()
			}
			if state == stObjFirst && w.src[w.pos] == '}' {
				w.pos++
				w.out = append(w.out, '}')
				w.pop()
				state = stAfter
				continue
			}
			if w.src[w.pos] != '"' {
				return errJSONSyntax
			}
			pushed, err := w.member()
			if err != nil {
				return err
			}
			if pushed {
				state = w.openState()
			} else {
				state = stAfter
			}

		case stArrFirst, stArrElem:
			w.skipWS()
			if w.pos >= len(w.src) {
				return w.eof()
			}
			if state == stArrFirst && w.src[w.pos] == ']' {
				w.pos++
				w.out = append(w.out, ']')
				w.pop()
				state = stAfter
				continue
			}
			pushed, err := w.element()
			if err != nil {
				return err
			}
			if pushed {
				state = w.openState()
			} else {
				state = stAfter
			}

		case stAfter:
			w.skipWS()
			if len(w.stack) == 0 {
				if w.pos < len(w.src) {
					return errJSONTrailing
				}
				return nil
			}
			if w.pos >= len(w.src) {
				return w.eof()
			}
			f := &w.stack[len(w.stack)-1]
			switch c := w.src[w.pos]; {
			case c == ',':
				w.pos++
				if f.obj {
					state = stObjKey
				} else {
					state = stArrElem
				}
			case (c == '}' && f.obj) || (c == ']' && !f.obj):
				w.pos++
				w.out = append(w.out, c)
				w.pop()
			default:
				return errJSONSyntax
			}
		}
	}
}

// ---- tree walk ------------------------------------------------------------

// sep writes the comma between siblings.
func (w *jsonRW) sep(f *jframe) {
	if f.emitted {
		w.out = append(w.out, ',')
	}
	f.emitted = true
}

// member handles one object member: its key and its value.
func (w *jsonRW) member() (pushed bool, err error) {
	f := &w.stack[len(w.stack)-1]
	keyStart := w.pos
	end, esc, err := scanString(w.src, keyStart)
	if err != nil {
		if err == errJSONEOF && w.lenient {
			w.sep(f)
			w.out = append(w.out, w.src[keyStart:]...)
			w.pos = len(w.src)
			return false, errLenientEOF
		}
		return false, err
	}
	w.pos = end
	w.skipWS()
	if w.pos >= len(w.src) {
		if w.lenient {
			w.sep(f)
			w.out = append(w.out, w.src[keyStart:end]...)
		}
		return false, w.eof()
	}
	if w.src[w.pos] != ':' {
		return false, errJSONSyntax
	}
	w.pos++
	w.skipWS()
	if w.pos >= len(w.src) {
		if w.lenient {
			w.sep(f)
			w.out = append(w.out, w.src[keyStart:end]...)
			w.out = append(w.out, ':')
		}
		return false, w.eof()
	}
	key := w.src[keyStart+1 : end-1]
	if esc {
		w.tmp = unescapeJSON(w.tmp[:0], key)
		key = w.tmp
	}
	if w.reporting {
		w.path = appendJSONPathKey(w.path[:f.pathLen], key)
	}
	info := w.enter(f, key, 0)
	if info.remove != nil {
		return false, w.removeValue(&info)
	}
	w.sep(f)
	w.writeKey(keyStart, end, key)
	w.out = append(w.out, ':')
	pushed, err = w.value(&info)
	if err != nil {
		return false, err
	}
	if !pushed {
		w.leave(&info)
	}
	return pushed, nil
}

// writeKey writes an object key, anonymizing it if a detector matches (maps
// keyed by e-mail address or card number exist).
func (w *jsonRW) writeKey(keyStart, end int, key []byte) {
	if w.r.c.detMask != 0 && len(key) >= 3 {
		st := classify(key)
		if candidates(w.r.c.detMask, &st) != 0 {
			s := string(key)
			if w.reporting {
				w.loc.json = w.path
			}
			if ns := w.r.detect(s, &st, &w.loc); ns != s {
				w.out = appendJSONString(w.out, ns)
				w.changed = true
				return
			}
		}
	}
	w.out = append(w.out, w.src[keyStart:end]...)
}

// element handles one array element.
func (w *jsonRW) element() (pushed bool, err error) {
	f := &w.stack[len(w.stack)-1]
	idx := f.idx
	f.idx++
	if w.reporting {
		w.path = appendJSONPathIndex(w.path[:f.pathLen], idx)
	}
	info := w.enter(f, nil, idx)
	if info.remove != nil {
		return false, w.removeValue(&info)
	}
	w.sep(f)
	pushed, err = w.value(&info)
	if err != nil {
		return false, err
	}
	if !pushed {
		w.leave(&info)
	}
	return pushed, nil
}

// removeValue skips the value at w.pos (validating it) and drops it from the
// output.
func (w *jsonRW) removeValue(info *childInfo) error {
	start := w.pos
	if err := w.skipValue(); err != nil {
		if errors.Is(err, errLenientEOF) {
			// The capture ended inside the removed value. What was written
			// so far already lacks it; make sure the caller keeps that
			// output instead of the original bytes.
			w.changed = true
		}
		return err
	}
	w.changed = true
	if w.reporting {
		w.loc.json = w.path
		w.r.rep.add(&w.loc, info.remove.source, string(w.src[start:min(w.pos, start+4*maxReportValue)]), "")
	}
	w.leave(info)
	return nil
}

// value processes the value at w.pos. A scalar is written and pushed is
// false. A container writes its opening bracket and pushes a frame.
func (w *jsonRW) value(info *childInfo) (pushed bool, err error) {
	c := w.src[w.pos]
	switch {
	case c == '{' || c == '[':
		if len(w.stack) >= maxJSONDepth {
			return false, errJSONDepth
		}
		w.out = append(w.out, c)
		w.pos++
		w.stack = append(w.stack, jframe{
			obj:  c == '{',
			stLo: info.stLo, stHi: info.stHi, acLo: info.acLo, acHi: info.acHi,
			pathLen: len(w.path),
		})
		return true, nil
	case c == '"':
		return false, w.stringLeaf(info)
	case c == '-' || isDigit(c):
		return false, w.numberLeaf(info)
	case c == 't':
		return false, w.literal("true")
	case c == 'f':
		return false, w.literal("false")
	case c == 'n':
		return false, w.literal("null")
	}
	return false, errJSONSyntax
}

func (w *jsonRW) literal(lit string) error {
	rest := w.src[w.pos:]
	if len(rest) < len(lit) {
		if string(rest) != lit[:len(rest)] {
			return errJSONSyntax
		}
		if w.lenient {
			w.out = append(w.out, rest...)
			w.pos = len(w.src)
		}
		return w.eof()
	}
	if string(rest[:len(lit)]) != lit {
		return errJSONSyntax
	}
	w.out = append(w.out, lit...)
	w.pos += len(lit)
	return nil
}

func (w *jsonRW) stringLeaf(info *childInfo) error {
	start := w.pos
	end, esc, err := scanString(w.src, start)
	partial := false
	if err != nil {
		if err != errJSONEOF || !w.lenient {
			return err
		}
		partial, end = true, len(w.src)
	}
	w.pos = end
	rawEnd := end - 1
	if partial {
		rawEnd = end
	}
	raw := w.src[start+1 : rawEnd]
	val := raw
	if esc {
		w.tmp = unescapeJSON(w.tmp[:0], raw)
		val = w.tmp
	}

	chain := w.leafChain(info)
	var st textStats
	hasStats := false
	if len(chain) == 0 {
		if w.r.c.detMask == 0 || len(val) < 3 {
			return w.verbatim(start, end, partial)
		}
		st, hasStats = classify(val), true
		if candidates(w.r.c.detMask, &st) == 0 {
			return w.verbatim(start, end, partial)
		}
	}
	s := string(val)
	orig := s
	if w.reporting {
		w.loc.json = w.path
	}
	if len(chain) > 0 {
		s = w.r.applyChain(chain, s, &w.loc)
	}
	if !selected(chain) {
		if hasStats {
			s = w.r.detect(s, &st, &w.loc)
		} else {
			s = w.r.detect(s, nil, &w.loc)
		}
	}
	if s == orig {
		return w.verbatim(start, end, partial)
	}
	w.changed = true
	w.out = appendJSONString(w.out, s)
	if partial {
		w.out = w.out[:len(w.out)-1] // the source string was never closed
		return errLenientEOF
	}
	return nil
}

// verbatim copies source bytes [start,end) unchanged.
func (w *jsonRW) verbatim(start, end int, partial bool) error {
	w.out = append(w.out, w.src[start:end]...)
	if partial {
		return errLenientEOF
	}
	return nil
}

func (w *jsonRW) numberLeaf(info *childInfo) error {
	start := w.pos
	end, err := scanNumber(w.src, start)
	if err != nil {
		if err == errJSONEOF && w.lenient {
			w.out = append(w.out, w.src[start:]...)
			w.pos = len(w.src)
			return errLenientEOF
		}
		return err
	}
	w.pos = end
	tok := w.src[start:end]
	chain := w.leafChain(info)
	// Of the detectors only the card detector can match a bare number: phone
	// and SSN need punctuation, and treating every nine or ten digit integer
	// as one would corrupt IDs and timestamps.
	card := w.r.c.detMask&(1<<detCard) != 0 && plainCardNumber(tok)
	if len(chain) == 0 && !card {
		return w.verbatim(start, end, false)
	}
	s := string(tok)
	orig := s
	if w.reporting {
		w.loc.json = w.path
	}
	if len(chain) > 0 {
		s = w.r.applyChain(chain, s, &w.loc)
	}
	if card && !selected(chain) {
		s = w.r.detect(s, nil, &w.loc)
	}
	if s == orig {
		return w.verbatim(start, end, false)
	}
	w.changed = true
	if validJSONNumber(s) {
		w.out = append(w.out, s...) // a fake of digits stays a number
	} else {
		w.out = appendJSONString(w.out, s)
	}
	return nil
}
