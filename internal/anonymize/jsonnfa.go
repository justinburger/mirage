package anonymize

// The JSONPath matcher: for every node the rewriter keeps the set of
// (rule, step) states reached on the way down, and the rules that matched the
// node or one of its ancestors.

func (w *jsonRW) addState(lo int, st jstate) {
	for _, x := range w.states[lo:] {
		if x == st {
			return
		}
	}
	w.states = append(w.states, st)
}

// addActive inserts rule ri into the sorted, duplicate-free window that
// starts at lo.
func (w *jsonRW) addActive(lo int, ri int32) {
	a := w.active[lo:]
	i := 0
	for i < len(a) && a[i] < ri {
		i++
	}
	if i < len(a) && a[i] == ri {
		return
	}
	w.active = append(w.active, 0)
	copy(w.active[lo+i+1:], w.active[lo+i:len(w.active)-1])
	w.active[lo+i] = ri
}

// enter computes the NFA states and active rules of a child of parent (nil
// for the document root): key is the member name for objects, idx the index
// for arrays.
func (w *jsonRW) enter(parent *jframe, key []byte, idx int) childInfo {
	info := childInfo{stLo: len(w.states), acLo: len(w.active)}
	if len(w.prog) == 0 {
		info.stHi, info.acHi = info.stLo, info.acLo
		return info
	}
	// Rules of the form $..name have no automaton state: they are matched by
	// member name below.
	fast := w.r.sd.keyDescent != nil
	if parent == nil {
		for ri, rl := range w.prog {
			if !(fast && rl.keyOnly) {
				w.states = append(w.states, mkState(ri, 0))
			}
		}
	} else {
		for _, st := range w.states[parent.stLo:parent.stHi] {
			ri, p := st.rule(), st.pos()
			steps := w.prog[ri].jp.steps
			if p >= len(steps) {
				continue // the parent itself matched; nothing continues from it
			}
			s := &steps[p]
			if s.descend {
				w.addState(info.stLo, st)
			}
			if s.matches(parent.obj, key, idx) {
				w.addState(info.stLo, mkState(ri, p+1))
			}
		}
	}
	info.stHi = len(w.states)
	if parent != nil {
		w.active = append(w.active, w.active[parent.acLo:parent.acHi]...)
	}
	for _, st := range w.states[info.stLo:info.stHi] {
		ri := st.rule()
		if st.pos() != len(w.prog[ri].jp.steps) {
			continue
		}
		w.addActive(info.acLo, int32(ri))
		// The document root cannot be deleted; there, remove falls back to
		// redacting every leaf.
		if parent != nil && info.remove == nil && w.prog[ri].act.remove() {
			info.remove = w.prog[ri]
		}
	}
	if fast && parent != nil && parent.obj {
		for _, ri := range w.r.sd.keyDescent[string(key)] {
			if sib := w.prog[ri].jp.siblings; sib != nil && !w.siblingCheck(parent, sib) {
				continue
			}
			w.addActive(info.acLo, ri)
			// Of several rules that delete the node, report the lowest.
			if rl := w.prog[ri]; rl.act.remove() && (info.remove == nil || rl.idx < info.remove.idx) {
				info.remove = rl
			}
		}
	}
	info.acHi = len(w.active)
	return info
}

// leave releases the arena windows of a finished scalar or removed child.
func (w *jsonRW) leave(info *childInfo) {
	w.states = w.states[:info.stLo]
	w.active = w.active[:info.acLo]
}

func (w *jsonRW) pop() {
	f := &w.stack[len(w.stack)-1]
	w.states = w.states[:f.stLo]
	w.active = w.active[:f.acLo]
	w.path = w.path[:f.pathLen]
	w.stack = w.stack[:len(w.stack)-1]
}

// leafChain returns the rules that apply to a scalar leaf, in profile order:
// the JSONPath rules matching it or an ancestor, merged with the regex rules.
func (w *jsonRW) leafChain(info *childInfo) []*rule {
	act := w.active[info.acLo:info.acHi]
	rx := w.r.sd.regex
	if len(act) == 0 {
		return rx
	}
	w.chain = w.chain[:0]
	j := 0
	for _, ri := range act {
		rl := w.prog[ri]
		for j < len(rx) && rx[j].idx < rl.idx {
			w.chain = append(w.chain, rx[j])
			j++
		}
		w.chain = append(w.chain, rl)
	}
	w.chain = append(w.chain, rx[j:]...)
	return w.chain
}

// siblingCheck reports whether the object of frame f has a member called one
// of names (the sibling filter, see parseJSONPath). It reads the whole object,
// so the answer does not depend on where in the object the matched member
// stands. The scan does not move the rewriter: it works on its own copy of
// the read position and its output is discarded.
//
// Reading an object once per matching member can cost O(depth * size) on
// nested documents; the bytes read ahead are therefore capped per document
// (about four times its size), and past the cap, or when the object cannot be
// read to its end (a truncated capture), the answer is yes: not knowing
// whether a name belongs to a person errs on the side of anonymizing it.
func (w *jsonRW) siblingCheck(f *jframe, names []string) bool {
	if w.look > w.lookMax {
		return true
	}
	saved := w.pos
	w.pos = f.objLo
	found, complete := w.scanForKeys(names)
	if w.pos > f.objLo {
		w.look += w.pos - f.objLo
	}
	w.pos = saved
	return found || !complete && w.lenient
}

// scanForKeys reads the members of the object whose opening brace was just
// consumed, from w.pos, and reports whether one of them is called one of
// names. complete is false when the input ended (or was malformed) before the
// object did.
func (w *jsonRW) scanForKeys(names []string) (found, complete bool) {
	for {
		w.skipWS()
		if w.pos >= len(w.src) || w.src[w.pos] != '"' {
			return found, w.pos < len(w.src) && w.src[w.pos] == '}'
		}
		start := w.pos
		end, esc, err := scanString(w.src, start)
		if err != nil {
			return found, false
		}
		if !found {
			key := w.src[start+1 : end-1]
			if esc {
				w.sibTmp = unescapeJSON(w.sibTmp[:0], key)
				key = w.sibTmp
			}
			for _, n := range names {
				if string(key) == n {
					found = true
					break
				}
			}
		}
		w.pos = end
		w.skipWS()
		if w.pos >= len(w.src) || w.src[w.pos] != ':' {
			return found, false
		}
		w.pos++
		if found {
			// The answer is known: no need to read the rest.
			return true, true
		}
		if w.skipValue() != nil {
			return false, false
		}
		w.skipWS()
		if w.pos >= len(w.src) {
			return false, false
		}
		switch w.src[w.pos] {
		case ',':
			w.pos++
		case '}':
			return false, true
		default:
			return false, false
		}
	}
}
