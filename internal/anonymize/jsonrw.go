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
	objLo      int // objects only: offset of the first byte after the opening brace
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
	// kd indexes the keyOnly rules of prog by normalized member name, see
	// sideRules.keyDescent.
	kd map[string][]int32
	// nest is how many JSON documents enclose this one (it was found inside a
	// string of another); nkey is scratch for normalized member names.
	nest    int
	nkey    []byte
	states  []jstate
	active  []int32
	stack   []jframe
	path    []byte
	chain   []*rule
	tmp     []byte
	sibTmp  []byte // scratch of scanForKeys; tmp may be holding the key being entered
	skipStk []byte
	// look counts the bytes scanned ahead for sibling filters in this
	// document; lookMax bounds it, see siblingCheck.
	look, lookMax int
}

var jsonPool = sync.Pool{New: func() any { return new(jsonRW) }}

// maxNestedJSON bounds how many JSON documents are unpacked from strings of
// one another: {"a":"{\"b\":\"{...}\"}"} is two levels deep. A string that is
// nested deeper than this is treated as the plain string it is.
const maxNestedJSON = 3

// rewriteJSON rewrites src, a single JSON value, for the current side of r.
// In lenient mode a document that ends early (a truncated capture) is
// processed as far as it goes. It reports whether anything changed; when
// nothing changed the returned slice is nil and src should be kept as is.
func (r *run) rewriteJSON(src []byte, lenient bool, loc locator) ([]byte, bool, error) {
	return r.rewriteDoc(src, lenient, loc, r.sd.json, r.sd.keyDescent, 0, nil)
}

// rewriteDoc is rewriteJSON for a document with its own rules: prog and kd
// are the rules that apply (all of the side's for a body, the ones that start
// with a recursive descent for a document found inside a string), nest says
// how many documents enclose it, and prefix, when reporting, is written in
// front of the "$" of the locations it reports.
func (r *run) rewriteDoc(src []byte, lenient bool, loc locator, prog []*rule, kd map[string][]int32, nest int, prefix []byte) ([]byte, bool, error) {
	w := jsonPool.Get().(*jsonRW)
	w.r, w.src, w.pos = r, src, 0
	w.lenient, w.changed, w.reporting = lenient, false, r.rep != nil
	w.loc = loc
	w.loc.json = nil
	w.prog, w.kd, w.nest = prog, kd, nest
	w.out = w.out[:0]
	w.look, w.lookMax = 0, 4*len(src)+(64<<10)
	w.states, w.active, w.stack = w.states[:0], w.active[:0], w.stack[:0]
	w.path = w.path[:0]
	if w.reporting {
		w.path = append(w.path, prefix...)
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
	w.r, w.src, w.prog, w.kd, w.loc = nil, nil, nil, nil, locator{}
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
			pathLen: len(w.path), objLo: w.pos,
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
	if !partial && w.nest < maxNestedJSON && !selected(chain) && embeddedJSON(val) {
		if nested, changed, ok := w.nestedDocument(val); ok {
			if !changed {
				return w.verbatim(start, end, false)
			}
			w.changed = true
			w.out = appendJSONString(w.out, string(nested))
			return nil
		}
	}
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

// embeddedJSON reports whether the string value val looks like a JSON object
// or array once the spaces around it are trimmed: queue messages, webhook
// envelopes and log events carry whole documents as strings.
func embeddedJSON(val []byte) bool {
	i, j := 0, len(val)
	for i < j && isJSONSpace(val[i]) {
		i++
	}
	for j > i && isJSONSpace(val[j-1]) {
		j--
	}
	if j-i < 2 {
		return false
	}
	return val[i] == '{' && val[j-1] == '}' || val[i] == '[' && val[j-1] == ']'
}

func isJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// nestedDocument rewrites the JSON document that the string value val holds.
// Inside it the recursive-descent rules and the detectors apply, the way they
// do in a body, and so do the regex rules; an absolute path ($.a.b) is
// relative to the outer document and does not. ok is false when val is not
// one JSON value (the caller then handles it as the plain string it is, and
// whatever the attempt reported is dropped). changed reports whether the
// document differs; out is then its new text, compact.
func (w *jsonRW) nestedDocument(val []byte) (out []byte, changed, ok bool) {
	r := w.r
	if len(r.sd.nested) == 0 && r.c.detMask == 0 && len(r.sd.regex) == 0 {
		return nil, false, false // nothing could change in it
	}
	var prefix []byte
	mark := 0
	if r.rep != nil {
		prefix = append(append(make([]byte, 0, len(w.path)+8), w.path...), " <json> "...)
		mark = r.rep.mark()
	}
	out, changed, err := r.rewriteDoc(val, false, w.loc, r.sd.nested, r.sd.nestedKeyDescent, w.nest+1, prefix)
	if err != nil {
		if r.rep != nil {
			r.rep.rollback(mark)
		}
		return nil, false, false
	}
	return out, changed, true
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
