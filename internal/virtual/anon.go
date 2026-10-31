package virtual

import (
	"bytes"
	"hash/maphash"
	"net/http"
	"reflect"
	"slices"

	"github.com/justinburger/mirage/internal/bodycodec"
	"github.com/justinburger/mirage/internal/model"
)

// Matching against a recording captured with anonymize_capture.
//
// Such a recording stores the anonymized path, query, body and headers of
// every request, so the raw request of a client (an email in the path, a card
// number in the body) never matches it directly. With Options.Anonymizer set
// a lookup therefore works on two forms of the request:
//
//   - the anonymized copy: a model.Request built from the incoming request
//     (escaped path, raw query, cloned headers, a copy of the body, decoded
//     the way the capture stores it) and run through the Anonymizer, which is
//     the very engine the capture used, so the same input gives the same
//     tokens; and
//   - the request as received, which still matches what is already
//     anonymized (a replay of the recording sends anonymized values, and
//     anonymizing those again would change them once more) and raw
//     exchanges of a mixed recording.
//
// The index is probed level by level, most specific first, with the
// anonymized form before the request as received at each level. A form that
// the anonymizer left unchanged is probed once.

// anonForm is one form of a request the index is probed with. Its path is kept
// apart (see lookupForms): the endpoint lookup lets strings escape, and in the
// same struct that would force the key buffers to the heap.
type anonForm struct {
	anon   bool    // the anonymized copy (the template key follows the capture's endpoint rule)
	suffix []byte  // query, body and header part of the exact-level key
	params []param // query parameters, for subset matching
}

// isNilAnonymizer reports whether a is nil or an interface holding a nil
// pointer.
func isNilAnonymizer(a RequestAnonymizer) bool {
	if a == nil {
		return true
	}
	v := reflect.ValueOf(a)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
		return v.IsNil()
	}
	return false
}

// appendExactKey appends the exact-level key of a request.
func appendExactKey(dst []byte, method, path string, suffix []byte) []byte {
	dst = append(dst, keyExact)
	dst = append(dst, method...)
	dst = append(dst, 0)
	dst = append(dst, path...)
	dst = append(dst, 0)
	return append(dst, suffix...)
}

// lookupForms is the part of lookup that runs when an Anonymizer is set: it
// builds the anonymized copy of the request and probes the index with it and
// with the request as received. orig is the request body as the caller read
// it (not decoded). It returns the counter stripe the lookup is accounted to
// and whether it found a match.
func (e *Engine) lookupForms(r *http.Request, orig []byte, method, path, rawQuery string, m *Match) (*counters, bool) {
	var (
		sb    [2][192]byte
		pb    [2][8]param
		fs    [2]anonForm
		paths [2]string
		n     int
		aRQ   string
	)
	if q, ok := e.anonymizedCopy(r, orig); ok {
		ap := q.Path
		if ap == "" {
			ap = "/"
		}
		f := anonForm{anon: true}
		f.suffix = e.appendSuffix(sb[0][:0], q.RawQuery, q.Body, q.Header)
		if e.cfg.subset {
			f.params = e.parseParams(q.RawQuery, pb[0][:0], false, false)
		}
		fs[0], paths[0], aRQ, n = f, ap, q.RawQuery, 1
	}

	var raw anonForm
	raw.suffix = e.appendSuffix(sb[1][:0], rawQuery, e.requestBody(r, orig), r.Header)
	if e.cfg.subset {
		raw.params = e.parseParams(rawQuery, pb[1][:0], false, false)
	}
	if n == 1 && paths[0] == path && aRQ == rawQuery && bytes.Equal(fs[0].suffix, raw.suffix) {
		// The anonymizer changed nothing the index looks at: one form.
		n = 0
	}
	fs[n], paths[n] = raw, path
	n++

	var kb [320]byte
	k := appendExactKey(kb[:0], method, paths[0], fs[0].suffix)
	stripe := &e.stats[maphash.Bytes(e.seed, k)&(numShards-1)]

	ok := e.findForms(method, path, paths, fs[:n], m)
	if !ok && method == http.MethodHead {
		// A HEAD request is answered from the GET recording when no HEAD was
		// recorded.
		ok = e.findForms(http.MethodGet, path, paths, fs[:n], m)
	}
	return stripe, ok
}

// findForms walks the match levels for one method, most specific first. At
// each level the forms are probed in order; paths[i] is the path of forms[i].
// rawPath is the path as received, which the endpoint of an anonymized form is
// derived from.
func (e *Engine) findForms(method, rawPath string, paths [2]string, forms []anonForm, m *Match) bool {
	var kb [320]byte
	for i := range forms {
		f := &forms[i]
		k := appendExactKey(kb[:0], method, paths[i], f.suffix)
		if it := e.probe(maphash.Bytes(e.seed, k), k, f.params); it != nil {
			m.set(it, LevelExact)
			return true
		}
	}
	if !e.cfg.pathSame {
		for i := range forms {
			f := &forms[i]
			k := append(kb[:0], keyPath)
			k = append(k, method...)
			k = append(k, 0)
			k = append(k, paths[i]...)
			if it := e.probe(maphash.Bytes(e.seed, k), k, f.params); it != nil {
				m.set(it, LevelPath)
				return true
			}
		}
	}
	if e.cfg.templates {
		for i := range forms {
			f := &forms[i]
			k := append(kb[:0], keyTemplate)
			if f.anon {
				// The endpoint the capture stored for this request.
				k = append(k, e.norm.AnonymizedEndpoint(method, rawPath, paths[i])...)
			} else {
				k = append(k, method...)
				k = append(k, ' ')
				k = append(k, e.norm.Template(paths[i])...)
			}
			if it := e.probe(maphash.Bytes(e.seed, k), k, f.params); it != nil {
				m.set(it, LevelTemplate)
				return true
			}
		}
	}
	return false
}

// anonymizedCopy returns the anonymized form of the incoming request, built
// the way the capture stores a request: a copy that never shares memory with
// r or orig, with a gzip or deflate body decoded and a body that is still
// encoded after that dropped (bodycodec.PrepareForAnonymization). ok is false
// when the anonymizer fails; the request is then looked up as received only
// (the capture drops an exchange it cannot anonymize, so nothing would match).
func (e *Engine) anonymizedCopy(r *http.Request, orig []byte) (q model.Request, ok bool) {
	q.Method = r.Method
	q.Path, q.RawQuery = "/", ""
	if r.URL != nil {
		q.Path, q.RawQuery = r.URL.EscapedPath(), r.URL.RawQuery
	}
	needBody := e.cfg.body != model.BodyIgnore && len(orig) > 0
	switch {
	case len(e.cfg.headers) > 0:
		q.Header = r.Header.Clone()
	case needBody:
		// Only the headers that decide how the body is read.
		for _, k := range [...]string{"Content-Type", "Content-Encoding", "Content-Length"} {
			if v, has := r.Header[k]; has {
				if q.Header == nil {
					q.Header = make(http.Header, 3)
				}
				q.Header[k] = slices.Clone(v)
			}
		}
	}
	if needBody {
		q.Body = slices.Clone(orig)
		q.BodySize = max(int64(len(orig)), r.ContentLength)
		// The proxy hands over at most max_body_bytes of the body, as the
		// capture does; a longer declared length means it was cut.
		q.BodyTruncated = r.ContentLength > int64(len(orig))
		ex := model.Exchange{Request: q}
		bodycodec.PrepareForAnonymization(&ex, maxRequestBody)
		q = ex.Request
	}
	if e.anon.Request(&q) != nil {
		return model.Request{}, false
	}
	return q, true
}
