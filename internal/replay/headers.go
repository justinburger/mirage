package replay

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"hash/maphash"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/justinburger/mirage/internal/model"
)

// replayHeader marks every request the replay engine sends so target logs can
// tell load traffic from real traffic.
const replayHeader = "X-Mirage-Replay"

// dropHeader reports whether a recorded request header must not be replayed:
// hop-by-hop headers (RFC 9110 section 7.6.1), headers the HTTP client owns
// (Host, Content-Length) and Expect. k must be canonical.
func dropHeader(k string) bool {
	switch k {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
		"Host", "Content-Length", "Expect":
		return true
	}
	return false
}

// connectionTokens returns the canonical names listed in Connection header
// values; those headers are hop-by-hop for this connection and are dropped.
func connectionTokens(vs []string) []string {
	var out []string
	for _, v := range vs {
		for tok := range strings.SplitSeq(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				out = append(out, http.CanonicalHeaderKey(tok))
			}
		}
	}
	return out
}

// cleanHeaders copies the replayable part of a recorded header map: keys are
// canonicalised, hop-by-hop headers and the user's RemoveHeaders are dropped.
// The result is never nil.
func cleanHeaders(src http.Header, remove map[string]struct{}) http.Header {
	dst := make(http.Header, len(src)+4)
	var conn []string
	if len(src) > 0 {
		for k, vs := range src {
			if http.CanonicalHeaderKey(k) == "Connection" {
				conn = append(conn, connectionTokens(vs)...)
			}
		}
	}
	for k, vs := range src {
		ck := http.CanonicalHeaderKey(k)
		if dropHeader(ck) || len(vs) == 0 {
			continue
		}
		if _, gone := remove[ck]; gone {
			continue
		}
		if len(conn) > 0 && slices.Contains(conn, ck) {
			continue
		}
		if old, dup := dst[ck]; dup {
			dst[ck] = append(slices.Clone(old), vs...)
		} else {
			dst[ck] = vs
		}
	}
	return dst
}

// overlay is what the job adds to (or overrides in) every request's headers:
// the SetHeaders, the X-Mirage-Replay marker and the target's userinfo as
// Basic credentials. It is immutable and shared by all workers.
type overlay struct {
	set    http.Header
	replay []string // nil when the marker is disabled (RemoveHeaders)
	auth   []string // default Authorization from the target URL's userinfo
	host   string   // Host header override ("" = the target's host)
}

func newOverlay(cfg model.ReplayConfig, label string, tgt target) overlay {
	o := overlay{host: cfg.HostHeader, set: http.Header{}}
	for k, v := range cfg.SetHeaders {
		ck := http.CanonicalHeaderKey(strings.TrimSpace(k))
		switch ck {
		case "Host":
			if o.host == "" {
				o.host = v
			}
		case "Content-Length":
			// The client computes it from the body.
		default:
			o.set[ck] = []string{v}
		}
	}
	o.replay = []string{label}
	for _, k := range cfg.RemoveHeaders {
		if http.CanonicalHeaderKey(strings.TrimSpace(k)) == replayHeader {
			o.replay = nil
		}
	}
	if tgt.userinfo != "" {
		o.auth = []string{"Basic " + base64.StdEncoding.EncodeToString([]byte(tgt.userinfo))}
	}
	return o
}

// apply writes the overlay into h, which the caller owns. The value slices
// are shared and must not be modified afterwards.
func (o *overlay) apply(h http.Header) {
	if o.replay != nil {
		h[replayHeader] = o.replay
	}
	for k, v := range o.set {
		h[k] = v
	}
	if o.auth != nil && h["Authorization"] == nil {
		h["Authorization"] = o.auth
	}
}

// removeSet returns the canonical RemoveHeaders as a set.
func removeSet(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[http.CanonicalHeaderKey(strings.TrimSpace(n))] = struct{}{}
	}
	return m
}

// hdrSet is an immutable, shared request header map.
type hdrSet struct {
	h    http.Header
	size int64 // approximate memory footprint
}

// interner deduplicates identical header maps: recorded traffic repeats a few
// dozen distinct header sets millions of times.
type interner struct {
	seed maphash.Seed
	sets map[uint64][]*hdrSet
	n    int
}

func newInterner() *interner {
	return &interner{seed: maphash.MakeSeed(), sets: map[uint64][]*hdrSet{}}
}

// hash is order independent across keys.
func (in *interner) hash(h http.Header) uint64 {
	var mh maphash.Hash
	mh.SetSeed(in.seed)
	var sum uint64
	for k, vs := range h {
		mh.Reset()
		mh.WriteString(k)
		for _, v := range vs {
			mh.WriteByte(0)
			mh.WriteString(v)
		}
		sum += mh.Sum64()
	}
	return sum ^ uint64(len(h))
}

func headersEqual(a, b http.Header) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !slices.Equal(av, bv) {
			return false
		}
	}
	return true
}

// intern returns the shared set equal to h, adding h as a new set (and
// reporting added) when there is none. The caller must not modify h after.
func (in *interner) intern(h http.Header) (set *hdrSet, added bool) {
	k := in.hash(h)
	for _, s := range in.sets[k] {
		if headersEqual(s.h, h) {
			return s, false
		}
	}
	size := int64(96)
	for key, vs := range h {
		size += int64(len(key)) + 64
		for _, v := range vs {
			size += int64(len(v)) + 16
		}
	}
	s := &hdrSet{h: h, size: size}
	in.sets[k] = append(in.sets[k], s)
	in.n++
	return s, true
}

// target is the parsed Target URL.
type target struct {
	scheme, host string
	// prefix is the target's escaped path without a trailing slash; recorded
	// paths are appended to it.
	prefix   string
	userinfo string
}

func parseTarget(raw string) (target, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return target{}, err
	}
	t := target{scheme: u.Scheme, host: u.Host, prefix: strings.TrimRight(u.EscapedPath(), "/")}
	if u.User != nil {
		pw, _ := u.User.Password()
		t.userinfo = u.User.Username() + ":" + pw
	}
	return t, nil
}

// url builds the request URL for a prepared uri ("<prefix><path>[?query]").
// The path is kept byte for byte (RawPath), so the request line matches the
// recording even for characters Go would normally re-escape.
func (t *target) url(uri string) (*url.URL, error) {
	p, q := uri, ""
	if i := strings.IndexByte(uri, '?'); i >= 0 {
		p, q = uri[:i], uri[i+1:]
	}
	dec, err := url.PathUnescape(p)
	if err != nil {
		return url.Parse(t.scheme + "://" + t.host + uri)
	}
	return &url.URL{Scheme: t.scheme, Host: t.host, Path: dec, RawPath: p, RawQuery: q}, nil
}

// bodyReader is a request body over an immutable byte slice. One allocation
// per request; Close is a no-op because the slice is never recycled.
type bodyReader struct{ bytes.Reader }

func (*bodyReader) Close() error { return nil }

func newBody(b []byte) *bodyReader {
	r := &bodyReader{}
	r.Reset(b)
	return r
}

// gzipper compresses bodies, reusing its writer and buffer.
type gzipper struct {
	w   *gzip.Writer
	buf bytes.Buffer
}

func newGzipper(level int) *gzipper {
	g := &gzipper{}
	g.w, _ = gzip.NewWriterLevel(io.Discard, level) // level is a valid constant
	return g
}

// compress returns b gzipped in a fresh slice.
func (g *gzipper) compress(b []byte) []byte {
	g.buf.Reset()
	g.w.Reset(&g.buf)
	_, _ = g.w.Write(b) // writes to a bytes.Buffer cannot fail
	_ = g.w.Close()
	return bytes.Clone(g.buf.Bytes())
}

// fastGzip pools BestSpeed gzippers for the per-request (anonymized) path.
var fastGzip = sync.Pool{New: func() any { return newGzipper(gzip.BestSpeed) }}

func gzipFast(b []byte) []byte {
	g := fastGzip.Get().(*gzipper)
	out := g.compress(b)
	fastGzip.Put(g)
	return out
}

// wantsGzip reports whether a recorded request body was gzip encoded on the
// wire and decoded by the recorder, so replay must encode it again.
func wantsGzip(r *model.Request) bool {
	if len(r.Body) == 0 || r.Header.Get("Content-Encoding") != "" {
		return false
	}
	return strings.EqualFold(r.ContentEncoding, "gzip") || strings.EqualFold(r.ContentEncoding, "x-gzip")
}
