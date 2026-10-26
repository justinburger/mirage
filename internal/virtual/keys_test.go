package virtual

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"math/rand/v2"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func gzipDeflate(t testing.TB, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write([]byte(s))
	_ = zw.Close()
	return buf.Bytes()
}

// ---- random JSON documents -----------------------------------------------

type jnode struct {
	kind  int // 0 object, 1 array, 2 string, 3 number, 4 bool, 5 null
	keys  []string
	kids  []*jnode
	str   string
	num   string
	boolv bool
}

var jstrings = []string{"", "a", "ab", "Hello, World", "tab\there", "quote\"inside", "back\\slash", "unié世", "line\nbreak", "slash/", "\u0001ctl"}
var jnums = []string{"0", "1", "-1", "10", "1.5", "-0.25", "1e3", "2E-2", "123456789012345678901234567890"}

func genJSON(r *rand.Rand, depth int) *jnode {
	k := r.IntN(6)
	if depth <= 0 && k < 2 {
		k = 2 + r.IntN(4)
	}
	n := &jnode{kind: k}
	switch k {
	case 0:
		used := map[string]bool{}
		for range r.IntN(5) {
			key := jstrings[r.IntN(len(jstrings))] + fmt.Sprint(r.IntN(3))
			if used[key] {
				continue
			}
			used[key] = true
			n.keys = append(n.keys, key)
			n.kids = append(n.kids, genJSON(r, depth-1))
		}
	case 1:
		for range r.IntN(5) {
			n.kids = append(n.kids, genJSON(r, depth-1))
		}
	case 2:
		n.str = jstrings[r.IntN(len(jstrings))]
	case 3:
		n.num = jnums[r.IntN(len(jnums))]
	case 4:
		n.boolv = r.IntN(2) == 0
	}
	return n
}

// quote renders s as a JSON string, escaping a random subset of characters
// with \u sequences.
func quote(r *rand.Rand, s string, fancy bool) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			fmt.Fprintf(&b, `\u%04x`, c)
		case fancy && c < 0x10000 && r.IntN(3) == 0:
			fmt.Fprintf(&b, `\u%04x`, c)
		case fancy && c == '/' && r.IntN(2) == 0:
			b.WriteString(`\/`)
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// render serialises the document. In fancy mode object members come in random
// order and whitespace and string escapes are sprinkled in; otherwise it is
// compact in the node's own order.
func render(r *rand.Rand, n *jnode, fancy bool) string {
	ws := func() string {
		if !fancy {
			return ""
		}
		return []string{"", " ", "\n", "\t ", "  \r\n"}[r.IntN(5)]
	}
	switch n.kind {
	case 0:
		idx := make([]int, len(n.keys))
		for i := range idx {
			idx[i] = i
		}
		if fancy {
			r.Shuffle(len(idx), func(i, j int) { idx[i], idx[j] = idx[j], idx[i] })
		}
		parts := make([]string, len(idx))
		for p, i := range idx {
			parts[p] = ws() + quote(r, n.keys[i], fancy) + ws() + ":" + ws() + render(r, n.kids[i], fancy) + ws()
		}
		return "{" + strings.Join(parts, ",") + "}"
	case 1:
		parts := make([]string, len(n.kids))
		for i, k := range n.kids {
			parts[i] = ws() + render(r, k, fancy) + ws()
		}
		return "[" + strings.Join(parts, ",") + "]"
	case 2:
		return quote(r, n.str, fancy)
	case 3:
		return n.num
	case 4:
		if n.boolv {
			return "true"
		}
		return "false"
	}
	return "null"
}

// mutate returns a copy of n with one leaf changed, and reports whether it
// found one to change.
func mutate(r *rand.Rand, n *jnode) (*jnode, bool) {
	c := *n
	switch n.kind {
	case 0, 1:
		if len(n.kids) == 0 {
			return &c, false
		}
		c.kids = append([]*jnode(nil), n.kids...)
		i := r.IntN(len(c.kids))
		k, ok := mutate(r, c.kids[i])
		c.kids[i] = k
		return &c, ok
	case 2:
		c.str = n.str + "x"
	case 3:
		c.num = n.num + "0"
		if strings.ContainsAny(n.num, ".eE") {
			c.num = n.num + "1"
		}
	case 4:
		c.boolv = !n.boolv
	default:
		c.kind = 4
		c.boolv = true
	}
	return &c, true
}

func TestJSONHashProperties(t *testing.T) {
	e := NewEngine(nil, Options{})
	r := rand.New(rand.NewPCG(1, 2))
	for i := range 3000 {
		doc := genJSON(r, 4)
		compact := render(r, doc, false)
		if !isJSON(compact) {
			t.Fatalf("generator produced invalid JSON: %s", compact)
		}
		h1, ok := e.hashJSON([]byte(compact))
		if !ok {
			t.Fatalf("doc %d rejected: %s", i, compact)
		}
		for range 3 {
			fancy := render(r, doc, true)
			h2, ok := e.hashJSON([]byte(fancy))
			if !ok || h1 != h2 {
				t.Fatalf("equivalent documents hash differently:\n%s\n%s", compact, fancy)
			}
		}
		if mut, changed := mutate(r, doc); changed {
			h3, _ := e.hashJSON([]byte(render(r, mut, true)))
			if h3 == h1 {
				t.Fatalf("different documents hash the same:\n%s\n%s", compact, render(r, mut, false))
			}
		}
	}
}

func isJSON(s string) bool {
	e := NewEngine(nil, Options{})
	_, ok := e.hashJSON([]byte(s))
	return ok
}

func TestJSONHashRejectsInvalid(t *testing.T) {
	e := NewEngine(nil, Options{})
	for _, s := range []string{``, ` `, `{`, `{"a":}`, `[1,]`, `'a'`, `{"a":1}x`, `nul`, `01`, `"\x"`, "\xef\xbb\xbf{}", `{"a" 1}`} {
		if _, ok := e.hashJSON([]byte(s)); ok {
			t.Errorf("%q accepted as JSON", s)
		}
	}
}

func TestJSONHashDeepNesting(t *testing.T) {
	e := NewEngine(nil, Options{})
	deep := strings.Repeat("[", 5000) + "1" + strings.Repeat("]", 5000)
	if _, ok := e.hashJSON([]byte(deep)); !ok {
		t.Fatal("5000 levels should be accepted")
	}
	tooDeep := strings.Repeat("[", 20000) + strings.Repeat("]", 20000)
	if _, ok := e.hashJSON([]byte(tooDeep)); ok {
		t.Fatal("20000 levels are beyond encoding/json's limit and must fall back to bytes")
	}
	// As a request body the over-deep document still matches by bytes.
	e2 := newTestEngine(t, Options{}, mkEx(1, "POST", "/d", 200, "ok", withReqBody(tooDeep)))
	if l, _ := lookup(e2, "POST", "/d", tooDeep); l != LevelExact {
		t.Fatalf("level = %s", l)
	}
}

func FuzzHashJSON(f *testing.F) {
	for _, s := range []string{`{"a":[1,2,{"b":null}],"c":"é\"x"}`, `[]`, `{}`, `"s"`, `-1.5e+3`, `true`, ` [ 1 , 2 ] `, `{"a":1,"a":2}`, `"😀"`, `{`, ``} {
		f.Add([]byte(s))
	}
	e := NewEngine(nil, Options{})
	f.Fuzz(func(t *testing.T, b []byte) {
		h1, ok := e.hashJSON(b)
		h2, ok2 := e.hashJSON(b)
		if ok != ok2 || h1 != h2 {
			t.Fatal("hash is not deterministic")
		}
		// Body keys must not panic whatever the input.
		_ = e.appendBodyKey(nil, b)
	})
}

func FuzzLookupQuery(f *testing.F) {
	for _, s := range []string{"a=1&b=2", "", "&&&", "a", "a=%zz", "%", "+", "a=b=c&d", "x=%E4%B8%96", strings.Repeat("k=v&", 40)} {
		f.Add(s, s)
	}
	cfgs := []model.MatchConfig{
		{Query: model.QueryExact, IgnoreParams: []string{"a"}},
		{Query: model.QuerySubset, Body: model.BodyIgnore},
		{Query: model.QueryIgnore},
	}
	f.Fuzz(func(t *testing.T, recorded, request string) {
		for _, c := range cfgs {
			e := NewEngine(nil, Options{Match: c, Latency: model.LatencyConfig{Mode: model.LatencyNone}})
			ex := mkEx(1, "GET", "/f", 200, "x")
			ex.Request.RawQuery = recorded
			e.Add(ex)
			r := mkReq("GET", "/f")
			r.URL.RawQuery = request
			e.Lookup(r, nil)
			// A request always matches itself.
			r2 := mkReq("GET", "/f")
			r2.URL.RawQuery = recorded
			if m := e.Lookup(r2, nil); m == nil {
				t.Fatalf("config %+v: recorded query %q does not match itself", c, recorded)
			}
		}
	})
}

// ---- query keys --------------------------------------------------------------

// Two queries have the same exact-level key exactly when their decoded
// parameter multisets are equal.
func TestQueryKeyEquivalence(t *testing.T) {
	e := NewEngine(nil, Options{Match: model.MatchConfig{Query: model.QueryExact, Body: model.BodyIgnore}})
	key := func(q string) string { return string(e.appendSuffix(nil, q, nil, nil)) }
	canon := func(q string) string {
		var ps []string
		for _, piece := range strings.Split(q, "&") {
			if piece == "" {
				continue
			}
			k, v, _ := strings.Cut(piece, "=")
			uk, _ := url.QueryUnescape(k)
			uv, _ := url.QueryUnescape(v)
			ps = append(ps, fmt.Sprintf("%d:%s=%d:%s", len(uk), uk, len(uv), uv))
		}
		sort.Strings(ps)
		return strings.Join(ps, "&")
	}
	r := rand.New(rand.NewPCG(5, 6))
	alphabet := []string{"a", "b", "c=", "&", "=", "%26", "%3D", "%20", "+", " ", "1", "x", "%41", "A"}
	gen := func() string {
		var b strings.Builder
		for range r.IntN(7) {
			b.WriteString(alphabet[r.IntN(len(alphabet))])
		}
		return b.String()
	}
	seen := map[string]string{} // key -> canonical form
	for range 20000 {
		q := gen()
		k := key(q)
		c := canon(q)
		if prev, ok := seen[k]; ok && prev != c {
			t.Fatalf("query %q shares a key with a different parameter set (%q vs %q)", q, c, prev)
		}
		seen[k] = c
	}
	// And the converse for permutations.
	if key("a=1&b=2&c=3") != key("c=3&a=1&b=2") {
		t.Fatal("permutation changed the key")
	}
}

func TestLongKeysAndManyParams(t *testing.T) {
	var q []string
	for i := range 40 {
		q = append(q, fmt.Sprintf("p%02d=%s", i, strings.Repeat("v", 20)))
	}
	recorded := strings.Join(q, "&")
	path := "/" + strings.Repeat("segment/", 60) + "end"
	e := newTestEngine(t, Options{Match: model.MatchConfig{PathTemplates: true}}, mkEx(1, "GET", path+"?"+recorded, 200, "long"))
	// Reverse the parameter order.
	for i, j := 0, len(q)-1; i < j; i, j = i+1, j-1 {
		q[i], q[j] = q[j], q[i]
	}
	if l, b := lookup(e, "GET", path+"?"+strings.Join(q, "&"), ""); l != LevelExact || b != "long" {
		t.Fatalf("long key: (%s,%q)", l, b)
	}
	if l, _ := lookup(e, "GET", path+"?"+strings.Join(q[1:], "&"), ""); l != LevelPath {
		t.Fatalf("one param fewer: %s", l)
	}
}

func TestIgnoreParamsIsCopied(t *testing.T) {
	ignore := []string{"_ts"}
	hdrs := []string{"X-A"}
	e := newTestEngine(t, Options{Match: model.MatchConfig{IgnoreParams: ignore, Headers: hdrs}},
		mkEx(1, "GET", "/i?a=1", 200, "x", withReqHeader("X-A", "1")))
	ignore[0], hdrs[0] = "a", "X-B" // caller reuses its slices
	if l, _ := lookup(e, "GET", "/i?a=1&_ts=5", "", "X-A", "1"); l != LevelExact {
		t.Fatalf("engine configuration followed the caller's slices: %s", l)
	}
}

func TestMatchHeadersAreCanonicalisedAndDeduplicated(t *testing.T) {
	e := NewEngine(nil, Options{Match: model.MatchConfig{Headers: []string{" x-tenant ", "X-Tenant", "", "accept"}}})
	if got := strings.Join(e.cfg.headers, ","); got != "X-Tenant,Accept" {
		t.Fatalf("headers = %s", got)
	}
}

func TestDeflateRequestBodyMatching(t *testing.T) {
	e := newTestEngine(t, Options{}, mkEx(1, "POST", "/b", 200, "R", withReqBody(`{"a":1}`)))
	m := e.Lookup(mkReq("POST", "/b", "Content-Encoding", "deflate"), gzipDeflate(t, `{"a": 1}`))
	if m == nil || m.Level != LevelExact {
		t.Fatalf("deflate body not decoded: %+v", m)
	}
	// An unknown encoding is matched as is.
	if m := e.Lookup(mkReq("POST", "/b", "Content-Encoding", "br"), []byte(`{"a":1}`)); m == nil || m.Level != LevelExact {
		t.Fatalf("br: %+v", m)
	}
	// A corrupt gzip body is matched as is (and so falls back to the path).
	if m := e.Lookup(mkReq("POST", "/b", "Content-Encoding", "gzip"), []byte("not gzip at all")); m == nil || m.Level != LevelPath {
		t.Fatalf("corrupt gzip: %+v", m)
	}
	// Body matching off: no decoding work and no effect.
	e2 := newTestEngine(t, Options{Match: model.MatchConfig{Body: model.BodyIgnore}}, mkEx(1, "POST", "/b", 200, "R"))
	if m := e2.Lookup(mkReq("POST", "/b", "Content-Encoding", "gzip"), []byte("junk")); m == nil || m.Level != LevelExact {
		t.Fatalf("ignore: %+v", m)
	}
}

func TestParseParamsDetails(t *testing.T) {
	e := NewEngine(nil, Options{Match: model.MatchConfig{IgnoreParams: []string{"skip"}}})
	got := e.parseParams("b=2&a=%31&skip=x&&c&a=0&d=%zz", nil, true, true)
	var s []string
	for _, p := range got {
		s = append(s, p.name+"="+p.value)
	}
	want := "a=0,a=1,b=2,c=,d=%zz"
	if strings.Join(s, ",") != want {
		t.Fatalf("got %s, want %s", strings.Join(s, ","), want)
	}
	if n := len(e.parseParams("skip=1", nil, false, false)); n != 1 {
		t.Fatalf("prune=false must keep ignored params, got %d", n)
	}
}
