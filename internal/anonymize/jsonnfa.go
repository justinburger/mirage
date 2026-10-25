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
	if parent == nil {
		for ri := range w.prog {
			w.states = append(w.states, mkState(ri, 0))
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
