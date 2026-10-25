package anonymize

import (
	"net/http"
	"net/textproto"
	"sort"
	"strings"
)

// neverTouch lists headers that describe the message itself. Rewriting them
// (a regex rule for digits hitting Content-Length, the card detector
// misreading a Content-Range) would corrupt the exchange, so no rule and no
// detector looks at them.
var neverTouch = map[string]struct{}{
	"Content-Type": {}, "Content-Length": {}, "Content-Encoding": {},
	"Transfer-Encoding": {}, "Connection": {}, "Host": {}, "Keep-Alive": {},
	"Upgrade": {}, "Te": {}, "Trailer": {},
}

// noDetect lists headers whose values are protocol metadata that look like
// PII to the detectors: version numbers that parse as IPv4 addresses
// (Chrome/120.0.0.0), validators made of digits, dates. Explicit rules still
// apply to them; the detectors skip them.
var noDetect = map[string]struct{}{
	"User-Agent": {}, "Server": {}, "Date": {}, "Expires": {}, "Last-Modified": {},
	"Age": {}, "Cache-Control": {}, "Pragma": {}, "Etag": {}, "If-None-Match": {},
	"If-Match": {}, "If-Modified-Since": {}, "If-Unmodified-Since": {}, "If-Range": {},
	"Range": {}, "Content-Range": {}, "Content-Language": {}, "Accept": {},
	"Accept-Encoding": {}, "Accept-Language": {}, "Accept-Charset": {}, "Accept-Ranges": {},
	"Vary": {}, "Allow": {}, "Retry-After": {}, "Strict-Transport-Security": {},
	"Content-Security-Policy": {}, "X-Content-Type-Options": {}, "X-Frame-Options": {},
	"X-Xss-Protection": {}, "Sec-Ch-Ua": {}, "Sec-Ch-Ua-Mobile": {}, "Sec-Ch-Ua-Platform": {},
}

// authSchemes are the authentication schemes whose name is kept when the
// credential after it is replaced, so "Bearer h_..." is still a well-formed
// Authorization header.
var authSchemes = []string{
	"Bearer", "Basic", "Token", "Digest", "Negotiate", "NTLM", "ApiKey", "Api-Key",
	"AWS4-HMAC-SHA256", "OAuth", "JWT", "DPoP", "HOBA", "Mutual", "Hawk", "SCRAM-SHA-256",
}

// splitAuthScheme splits "Bearer xyz" into ("Bearer ", "xyz"). A value that
// does not start with a known scheme and a space is returned as ("", v).
func splitAuthScheme(v string) (scheme, rest string) {
	i := strings.IndexByte(v, ' ')
	if i <= 0 {
		return "", v
	}
	known := false
	for _, s := range authSchemes {
		if strings.EqualFold(v[:i], s) {
			known = true
			break
		}
	}
	if !known {
		return "", v
	}
	j := i + 1
	for j < len(v) && v[j] == ' ' {
		j++
	}
	if j == len(v) {
		return "", v
	}
	return v[:j], v[j:]
}

// headerKey finds the key of h that equals name (canonical form) ignoring
// case, preferring the exact key.
func headerKey(h http.Header, name string) (string, bool) {
	if _, ok := h[name]; ok {
		return name, true
	}
	for k := range h {
		if strings.EqualFold(k, name) {
			return k, true
		}
	}
	return "", false
}

// getHeader returns the first value of the named header (canonical form).
func getHeader(h http.Header, name string) string {
	if k, ok := headerKey(h, name); ok && len(h[k]) > 0 {
		return h[k][0]
	}
	return ""
}

// headers anonymizes every header of the current side.
func (r *run) headers(h http.Header) {
	if len(h) == 0 {
		return
	}
	if r.rep != nil { // deterministic report order
		keys := make([]string, 0, len(h))
		for k := range h {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			r.header(h, k)
		}
		return
	}
	for k := range h {
		r.header(h, k)
	}
}

func (r *run) header(h http.Header, key string) {
	vals := h[key]
	if len(vals) == 0 {
		return
	}
	name := textproto.CanonicalMIMEHeaderKey(key)
	sel := r.sd.header[name]
	_, redacted := r.c.redact[name]
	loc := locator{side: r.side, where: "header", name: name}

	var out []string // copy-on-write: the input slice may be shared
	for i, v := range vals {
		nv, drop := r.headerValue(name, v, sel, redacted, &loc)
		if out == nil && (drop || nv != v) {
			out = append(make([]string, 0, len(vals)), vals[:i]...)
		}
		if out != nil && !drop {
			out = append(out, nv)
		}
	}
	switch {
	case out == nil:
	case len(out) == 0:
		delete(h, key)
	default:
		h[key] = out
	}
}

// headerValue anonymizes one header value. drop reports that the value (and
// for a header rule the whole header) must be deleted.
//
// Precedence: a header rule beats everything and sees the original value; then
// Cookie / Set-Cookie are handled per cookie; then redact_headers; then the
// regex rules and detectors.
func (r *run) headerValue(name, v string, sel []*rule, redacted bool, loc *locator) (nv string, drop bool) {
	if v == "" {
		return v, false
	}
	if len(sel) > 0 {
		if rl := removes(sel); rl != nil {
			r.record(loc, rl.source, v, "")
			return "", true
		}
		scheme, rest := splitAuthScheme(v)
		return scheme + r.value(sel, rest, loc), false
	}
	if name == "Cookie" {
		return r.cookies(v, redacted, loc)
	}
	if name == "Set-Cookie" {
		return r.setCookie(v, redacted, loc)
	}
	if redacted {
		scheme, rest := splitAuthScheme(v)
		out := scheme + r.k.hashToken(rest)
		r.record(loc, "redact_headers", v, out)
		return out, false
	}
	if _, ok := neverTouch[name]; ok {
		return v, false
	}
	if _, ok := noDetect[name]; ok {
		return r.regexOnly(v, loc), false
	}
	return r.value(nil, v, loc), false
}

// regexOnly applies the regex rules without the detectors.
func (r *run) regexOnly(s string, loc *locator) string {
	for _, rl := range r.sd.regex {
		s = r.applyRegex(rl, s, loc)
	}
	return s
}

// cookies anonymizes a Cookie request header ("a=1; b=2") one cookie at a
// time. Names and separators are kept, values are replaced.
func (r *run) cookies(v string, redacted bool, loc *locator) (string, bool) {
	var parts []string
	changed, dropped := false, false
	for pos := 0; pos <= len(v); {
		end := strings.IndexByte(v[pos:], ';')
		if end < 0 {
			end = len(v)
		} else {
			end += pos
		}
		seg := v[pos:end]
		pos = end + 1

		lead := 0
		for lead < len(seg) && (seg[lead] == ' ' || seg[lead] == '\t') {
			lead++
		}
		body := seg[lead:]
		name, val, hasEq := strings.Cut(body, "=")
		if body == "" || !hasEq {
			// Empty segment or a bare token: a bare token has no name to
			// match, but it can still hold a credential.
			if body != "" && redacted {
				out := r.k.hashToken(body)
				r.record(loc, "redact_headers", body, out)
				parts = append(parts, seg[:lead]+out)
				changed = true
				continue
			}
			parts = append(parts, seg)
			continue
		}
		// Whitespace around a value is not part of it ("b=2 ;").
		trimmed := strings.TrimRight(val, " \t")
		tail := val[len(trimmed):]
		nv, drop := r.cookiePair(strings.TrimSpace(name), trimmed, redacted, loc)
		if drop {
			changed, dropped = true, true
			continue
		}
		if nv != trimmed {
			changed = true
		}
		parts = append(parts, seg[:lead]+name+"="+nv+tail)
	}
	if !changed {
		return v, false
	}
	// Drop the leading space the first surviving cookie may have inherited.
	if dropped && len(parts) > 0 {
		parts[0] = strings.TrimLeft(parts[0], " \t")
	}
	out := strings.Join(parts, ";")
	return out, strings.TrimSpace(out) == ""
}

// setCookie anonymizes the cookie of a Set-Cookie response header and keeps
// its attributes (Path, Expires, HttpOnly, ...) untouched.
func (r *run) setCookie(v string, redacted bool, loc *locator) (string, bool) {
	pair, attrs, hasAttrs := strings.Cut(v, ";")
	name, val, hasEq := strings.Cut(pair, "=")
	if !hasEq {
		return v, false
	}
	nv, drop := r.cookiePair(strings.TrimSpace(name), val, redacted, loc)
	if drop {
		return "", true
	}
	if nv == val {
		return v, false
	}
	out := name + "=" + nv
	if hasAttrs {
		out += ";" + attrs
	}
	return out, false
}

// cookiePair anonymizes one cookie value. A cookie rule (by name) beats
// redact_headers and sees the original value.
func (r *run) cookiePair(name, val string, redacted bool, loc *locator) (string, bool) {
	if val == "" {
		return val, false
	}
	cl := *loc
	cl.where, cl.name = "cookie", name
	inner, quoted := val, false
	if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
		inner, quoted = val[1:len(val)-1], true
	}
	wrap := func(s string) string {
		if quoted {
			return `"` + s + `"`
		}
		return s
	}
	if sel := r.sd.cookie[name]; len(sel) > 0 {
		if rl := removes(sel); rl != nil {
			r.record(&cl, rl.source, val, "")
			return "", true
		}
		return wrap(r.value(sel, inner, &cl)), false
	}
	if redacted {
		out := wrap(r.k.hashToken(inner))
		r.record(&cl, "redact_headers", val, out)
		return out, false
	}
	return wrap(r.value(nil, inner, &cl)), false
}
