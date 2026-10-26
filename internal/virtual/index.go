package virtual

import (
	"hash/maphash"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// Match levels as indices into counters.level.
const (
	levelExact = iota
	levelPath
	levelTemplate
)

// Key prefixes keep the three levels apart inside one map.
const (
	keyExact    = 'e'
	keyPath     = 'p'
	keyTemplate = 't'
)

// shard is one slice of the index. Padded so neighbouring shards never share
// a cache line (the RWMutex reader count is written on every lookup).
type shard struct {
	mu sync.RWMutex
	m  map[string]*entry
	_  [32]byte
}

// counters are the activity counters of one stripe, padded to two cache
// lines. A lookup bumps the stripe chosen by its exact-level key hash.
type counters struct {
	hits, misses, injected atomic.Int64
	suppressed             atomic.Int64
	level                  [3]atomic.Int64
	_                      [72]byte
}

// entry holds the candidates of one key. cands is guarded by the shard lock
// (readers copy the slice header under RLock and then use it lock-free): it is
// replaced, never edited in place, once a reader may have seen it. cursor is
// the sequential position.
type entry struct {
	cands    []*item
	cursor   atomic.Uint64
	unsorted bool // Build only: cands needs sorting before use
	// mixed is set (under the shard lock, like cands) once a truncated
	// candidate is added. complete then holds the complete candidates in the
	// same order, maintained like cands, so a lookup can pick among them in
	// O(1).
	mixed    bool
	complete []*item
}

// item is one servable exchange. It is immutable once indexed.
type item struct {
	ex        *model.Exchange // full when resident, without bodies when lazy
	id        int64
	status    int
	isErr     bool
	truncated bool // response body was cut at capture; never served as a body
	gzip      bool // response may be re-encoded as gzip
	ttfb, dur time.Duration
	ep        *epool
	hdrs      []hdr      // response headers to copy
	body      *bodyEntry // resident body; nil in lazy mode
	params    []param    // recorded query parameters (subset matching only)
}

// hdr is one response header to copy to the client.
type hdr struct {
	key  string
	vals []string
}

// before reports whether a sorts before b in (StartedAt, Seq) order.
func before(a, b *item) bool {
	if c := a.ex.StartedAt.Compare(b.ex.StartedAt); c != 0 {
		return c < 0
	}
	return a.ex.Seq < b.ex.Seq
}

func cmpItems(a, b *item) int {
	switch {
	case before(a, b):
		return -1
	case before(b, a):
		return 1
	}
	return 0
}

// add inserts it in (StartedAt, Seq) order. With bulk set (Build, before the
// engine is shared) out-of-order items are appended and the entry is marked
// for sorting by finalize.
func (en *entry) add(it *item, bulk bool) {
	if it.truncated && !en.mixed {
		// From now on complete mirrors cands without the truncated items.
		en.mixed = true
		en.complete = slices.Clone(en.cands)
	}
	var unsorted bool
	en.cands, unsorted = insertItem(en.cands, it, bulk)
	en.unsorted = en.unsorted || unsorted
	if en.mixed && !it.truncated {
		en.complete, unsorted = insertItem(en.complete, it, bulk)
		en.unsorted = en.unsorted || unsorted
	}
}

// insertItem inserts it into list in (StartedAt, Seq) order and returns the
// new list. With bulk set out-of-order items are appended and reported as
// unsorted (finalize sorts them).
func insertItem(list []*item, it *item, bulk bool) ([]*item, bool) {
	n := len(list)
	if n == 0 || !before(it, list[n-1]) {
		// Appending is safe for readers holding the old slice header: they
		// never look beyond their own length.
		return append(list, it), false
	}
	if bulk {
		return append(list, it), true
	}
	i := sort.Search(n, func(i int) bool { return before(it, list[i]) })
	nl := make([]*item, n+1)
	copy(nl, list[:i])
	nl[i] = it
	copy(nl[i+1:], list[i:])
	return nl, false
}

// put adds it to the entry for key.
func (e *Engine) put(key []byte, it *item, bulk bool) {
	sh := &e.shards[maphash.Bytes(e.seed, key)&(numShards-1)]
	sh.mu.Lock()
	en := sh.m[string(key)]
	if en == nil {
		en = new(entry)
		sh.m[string(key)] = en
	}
	en.add(it, bulk)
	sh.mu.Unlock()
}

// finalize sorts the candidate lists Build left unsorted and publishes the
// endpoint pools.
func (e *Engine) finalize() {
	for i := range e.shards {
		sh := &e.shards[i]
		sh.mu.Lock()
		for _, en := range sh.m {
			if en.unsorted {
				slices.SortStableFunc(en.cands, cmpItems)
				slices.SortStableFunc(en.complete, cmpItems)
				en.unsorted = false
			}
		}
		sh.mu.Unlock()
	}
	e.epMu.Lock()
	for _, ep := range e.eps {
		ep.publish()
	}
	e.epMu.Unlock()
}

// upperMethod returns the request method in canonical (upper) case; "" is GET.
func upperMethod(m string) string {
	if m == "" {
		return http.MethodGet
	}
	return strings.ToUpper(m)
}

// insert indexes one exchange. With bulk set the caller is Build and the
// engine is not shared yet.
func (e *Engine) insert(ex *model.Exchange, bulk bool) {
	method := upperMethod(ex.Request.Method)
	path := ex.Request.Path
	if path == "" {
		path = "/"
	}
	endpoint := ex.Endpoint
	if endpoint == "" {
		endpoint = e.norm.Endpoint(method, path)
	}
	ep := e.endpointPool(endpoint)
	e.count.Add(1)

	isErr := ex.IsError()
	if ex.Error != "" {
		// No upstream response: never served, but part of the error rate.
		ep.record(nil, isErr, !bulk)
		return
	}

	// Request-side keys come from the request body, so compute them before
	// the exchange is trimmed.
	var sb [192]byte
	suffix := e.appendSuffix(sb[:0], ex.Request.RawQuery, ex.Request.Body, ex.Request.Header)
	var params []param
	if e.cfg.subset {
		params = e.parseParams(ex.Request.RawQuery, nil, true, true)
	}

	size := int64(len(ex.Request.Body) + len(ex.Response.Body))
	lazyItem := e.st != nil && ex.ID != 0 && (e.lazy || e.resident.Load()+size > e.opts.MaxResidentBytes)
	it := e.indexItem(ex, lazyItem)
	it.ep = ep
	it.params = params
	if lazyItem {
		it.id = ex.ID
	} else {
		e.resident.Add(size)
	}
	// A truncated recording is never used to replace another response
	// during error amplification, but it still counts toward the rate.
	if it.truncated {
		ep.record(nil, isErr, !bulk)
	} else {
		ep.record(it, isErr, !bulk)
	}

	var kb [320]byte
	key := append(kb[:0], keyExact)
	key = append(key, method...)
	key = append(key, 0)
	key = append(key, path...)
	key = append(key, 0)
	key = append(key, suffix...)
	e.put(key, it, bulk)

	if !e.cfg.pathSame {
		key = append(kb[:0], keyPath)
		key = append(key, method...)
		key = append(key, 0)
		key = append(key, path...)
		e.put(key, it, bulk)
	}
	if e.cfg.templates {
		key = append(kb[:0], keyTemplate)
		key = append(key, endpoint...)
		e.put(key, it, bulk)
		// A request is looked up under the endpoint of the engine's current
		// normalizer. A stored endpoint that is one of the current
		// endpoint_patterns is authoritative (the stored path may have been
		// anonymized). Any other was produced by another normalizer (a HAR
		// import, endpoint_patterns edited after recording): index the item
		// under the current normalizer's template of the path too.
		if _, known := e.patterns[strings.TrimPrefix(endpoint, method+" ")]; !known {
			key = append(kb[:0], keyTemplate)
			key = append(key, method...)
			key = append(key, ' ')
			key = append(key, e.norm.Template(path)...)
			if string(key[1:]) != endpoint {
				e.put(key, it, bulk)
			}
		}
	}
}

// indexItem builds the indexed form of ex. In lazy mode the item keeps a copy
// of the exchange without bodies.
//
// What the index needs from an exchange besides its response body is the
// request-side keys (computed by the caller before this) and the response
// headers (copied into the item), so the item keeps a slim exchange: the
// request headers, the request body and the response header map are released.
// The item holds a trimmed copy; ex itself is left as it was. An exchange
// reached through a Match no longer carries the request headers or body nor
// the response header map.
func (e *Engine) indexItem(ex *model.Exchange, lazy bool) *item {
	it := newItem(ex, lazy)
	if it.ex == ex {
		c := *ex
		it.ex = &c
	}
	it.ex.Request.Header, it.ex.Request.Body, it.ex.Response.Header = nil, nil, nil
	it.hdrs = e.internHeaders(it.hdrs)
	return it
}

// maxInternedHeaderSets bounds the table of shared header sets: API responses
// that all differ (request ids, etags) would otherwise make the table as big
// as the index it is meant to shrink.
const maxInternedHeaderSets = 4096

// internHeaders returns a shared copy of hs when an identical header set was
// seen before, so that thousands of responses with the same headers hold one
// set. hs must not be modified afterwards.
func (e *Engine) internHeaders(hs []hdr) []hdr {
	if len(hs) == 0 {
		return hs
	}
	var kb [256]byte
	k := kb[:0]
	for _, h := range hs {
		k = append(k, h.key...)
		k = append(k, 0)
		for _, v := range h.vals {
			k = append(k, v...)
			k = append(k, 0)
		}
		k = append(k, 1)
	}
	e.internMu.Lock()
	defer e.internMu.Unlock()
	if shared, ok := e.interned[string(k)]; ok {
		return shared
	}
	if len(e.interned) < maxInternedHeaderSets {
		if e.interned == nil {
			e.interned = make(map[string][]hdr)
		}
		e.interned[string(k)] = hs
	}
	return hs
}

// newItem builds the item for ex without changing it (lazy: the item holds a
// copy without bodies).
func newItem(ex *model.Exchange, lazy bool) *item {
	it := &item{
		status:    ex.Response.Status,
		isErr:     ex.IsError(),
		truncated: ex.Response.BodyTruncated,
		gzip:      ex.Response.ContentEncoding == "gzip",
		ttfb:      ex.TTFB.D(),
		dur:       ex.Duration.D(),
		hdrs:      buildHeaders(ex.Response.Header, ex.Response.ContentEncoding != ""),
	}
	if lazy {
		c := *ex
		c.Request.Body = nil
		c.Response.Body = nil
		it.ex = &c
	} else {
		it.ex = ex
		it.body = &bodyEntry{raw: ex.Response.Body}
	}
	return it
}

// buildHeaders returns the response headers worth copying: canonical keys
// without hop-by-hop headers, Content-Length and Date (recomputed), the
// headers a Connection header names, and Content-Encoding when the body was
// stored decoded.
func buildHeaders(h http.Header, decoded bool) []hdr {
	if len(h) == 0 {
		return nil
	}
	var named []string
	if conn := h["Connection"]; len(conn) > 0 {
		for _, v := range conn {
			for tok := range strings.SplitSeq(v, ",") {
				if tok = strings.TrimSpace(tok); tok != "" {
					named = append(named, http.CanonicalHeaderKey(tok))
				}
			}
		}
	}
	out := make([]hdr, 0, len(h))
	for k, vals := range h {
		ck := http.CanonicalHeaderKey(k)
		if len(vals) == 0 || skipHeader(ck, decoded) || slices.Contains(named, ck) {
			continue
		}
		if ck != k {
			// Merge keys that differ only in case.
			if i := slices.IndexFunc(out, func(x hdr) bool { return x.key == ck }); i >= 0 {
				out[i].vals = append(slices.Clone(out[i].vals), vals...)
				continue
			}
		}
		out = append(out, hdr{key: ck, vals: vals})
	}
	slices.SortFunc(out, func(a, b hdr) int { return strings.Compare(a.key, b.key) })
	return out
}

// skipHeader reports whether the canonical header key must not be copied from
// a recorded response.
func skipHeader(k string, decoded bool) bool {
	switch k {
	case "Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
		"Content-Length", "Date":
		return true
	case "Content-Encoding":
		return decoded
	}
	return strings.HasPrefix(k, "Proxy-")
}

// lookup finds the response for r and fills m. It counts hits and misses.
func (e *Engine) lookup(r *http.Request, body []byte, m *Match) bool {
	method := upperMethod(r.Method)
	path, rawQuery := "/", ""
	if r.URL != nil {
		path, rawQuery = r.URL.EscapedPath(), r.URL.RawQuery
		if path == "" {
			path = "/"
		}
	}
	body = e.requestBody(r, body)

	var sb [192]byte
	suffix := e.appendSuffix(sb[:0], rawQuery, body, r.Header)
	var req []param
	if e.cfg.subset {
		var pb [8]param
		req = e.parseParams(rawQuery, pb[:0], false, false)
	}

	var kb [320]byte
	key := append(kb[:0], keyExact)
	key = append(key, method...)
	key = append(key, 0)
	key = append(key, path...)
	key = append(key, 0)
	key = append(key, suffix...)
	h := maphash.Bytes(e.seed, key)
	stripe := &e.stats[h&(numShards-1)]

	ok := e.find(h, key, path, method, req, m)
	if !ok && method == http.MethodHead {
		// A HEAD request is answered from the GET recording when no HEAD was
		// recorded.
		key = append(kb[:0], keyExact)
		key = append(key, http.MethodGet...)
		key = append(key, 0)
		key = append(key, path...)
		key = append(key, 0)
		key = append(key, suffix...)
		ok = e.find(maphash.Bytes(e.seed, key), key, path, http.MethodGet, req, m)
	}
	if ok && e.opts.TruncatedAsMiss && m.it != nil && m.it.truncated {
		// Hybrid: let the upstream answer (and the capture repair the
		// recording) instead of answering from an incomplete body.
		*m = Match{}
		ok = false
	}
	if !ok {
		stripe.misses.Add(1)
		return false
	}
	stripe.hits.Add(1)
	stripe.level[levelOf(m.Level)].Add(1)
	if e.cfg.ampOn {
		e.amplify(m)
		switch {
		case !m.Injected:
		case m.Synthetic || (m.it != nil && m.it.isErr):
			stripe.injected.Add(1)
		default:
			// An error swapped for a recorded success: nothing was injected.
			stripe.suppressed.Add(1)
		}
	}
	return true
}

func levelOf(name string) int {
	switch name {
	case LevelExact:
		return levelExact
	case LevelPath:
		return levelPath
	}
	return levelTemplate
}

// find walks the match levels for one method. key is the exact-level key and
// h its hash.
func (e *Engine) find(h uint64, key []byte, path, method string, req []param, m *Match) bool {
	if it := e.probe(h, key, req); it != nil {
		m.set(it, LevelExact)
		return true
	}
	var kb [320]byte
	if !e.cfg.pathSame {
		k := append(kb[:0], keyPath)
		k = append(k, method...)
		k = append(k, 0)
		k = append(k, path...)
		if it := e.probe(maphash.Bytes(e.seed, k), k, req); it != nil {
			m.set(it, LevelPath)
			return true
		}
	}
	if e.cfg.templates {
		k := append(kb[:0], keyTemplate)
		k = append(k, method...)
		k = append(k, ' ')
		k = append(k, e.norm.Template(path)...)
		if it := e.probe(maphash.Bytes(e.seed, k), k, req); it != nil {
			m.set(it, LevelTemplate)
			return true
		}
	}
	return false
}

func (m *Match) set(it *item, level string) {
	*m = Match{Exchange: it.ex, Level: level, it: it, ttfb: it.ttfb, dur: it.dur}
}

// probe returns the candidate chosen for key, or nil.
func (e *Engine) probe(h uint64, key []byte, req []param) *item {
	sh := &e.shards[h&(numShards-1)]
	sh.mu.RLock()
	en := sh.m[string(key)]
	var cands, complete []*item
	if en != nil {
		cands = en.cands
		if en.mixed {
			complete = en.complete
		}
	}
	sh.mu.RUnlock()
	if len(cands) == 0 {
		return nil
	}
	// When some candidates hold a truncated body, choose among the complete
	// ones only, so the selection rotates over them exactly as if the
	// truncated ones were not there; fall back to all candidates (and so to
	// a truncated one) only when nothing complete matches.
	if !e.cfg.subset {
		if len(complete) > 0 {
			return complete[e.pick(en, len(complete))]
		}
		return cands[e.pick(en, len(cands))]
	}
	if len(complete) > 0 {
		if it := e.pickSubset(en, complete, req); it != nil {
			return it
		}
	}
	return e.pickSubset(en, cands, req)
}

// pickSubset picks among the candidates whose recorded query parameters are a
// subset of the request's. It returns nil when none qualifies.
func (e *Engine) pickSubset(en *entry, cands []*item, req []param) *item {
	n := 0
	for _, c := range cands {
		if subsetOf(c.params, req) {
			n++
		}
	}
	if n == 0 {
		return nil
	}
	k := e.pick(en, n)
	for _, c := range cands {
		if subsetOf(c.params, req) {
			if k == 0 {
				return c
			}
			k--
		}
	}
	return nil
}

// pick returns an index in [0, n) according to the selection strategy.
func (e *Engine) pick(en *entry, n int) int {
	if n == 1 {
		if e.cfg.selection == model.SelectSequential {
			en.cursor.Add(1)
		}
		return 0
	}
	switch e.cfg.selection {
	case model.SelectFirst:
		return 0
	case model.SelectRandom:
		return e.rng.intn(n)
	}
	return int((en.cursor.Add(1) - 1) % uint64(n))
}
