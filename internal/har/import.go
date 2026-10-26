package har

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// ErrUnsupported marks entries that are not HTTP(S) request/response pairs
// (data:, blob:, ws:, chrome-extension: URLs and the like). ToExchange wraps
// it; importers should skip such entries rather than fail the import.
var ErrUnsupported = errors.New("unsupported HAR entry")

// noResponseError is the Exchange.Error text for entries without a response.
const noResponseError = "no response in HAR"

// maxInflate bounds how far a gzip-compressed body is expanded: the cap the
// proxy puts on a captured body (a service's default max_body_bytes). A few
// kilobytes of gzip expand a thousandfold, so a larger limit let a small HAR
// turn into gigabytes of memory and disk. What lies beyond is dropped and the
// body is flagged truncated, exactly like a capture cut by the proxy.
const maxInflate = int(model.DefaultMaxBodyBytes)

// EntryError reports the entry that could not be converted by ToExchanges.
type EntryError struct {
	Index int
	Err   error
}

func (e *EntryError) Error() string { return fmt.Sprintf("har: entry %d: %v", e.Index, e.Err) }

// Unwrap returns the underlying error.
func (e *EntryError) Unwrap() error { return e.Err }

// ToExchanges converts every entry of a HAR document. Entries that cannot be
// converted are skipped and reported in skipped; errors.Is(err,
// ErrUnsupported) tells apart entries that are simply not HTTP exchanges
// from corrupt ones.
func ToExchanges(h *HAR) (exchanges []*model.Exchange, skipped []*EntryError) {
	if h == nil {
		return nil, nil
	}
	exchanges = make([]*model.Exchange, 0, len(h.Log.Entries))
	for i := range h.Log.Entries {
		ex, err := ToExchange(&h.Log.Entries[i])
		if err != nil {
			skipped = append(skipped, &EntryError{Index: i, Err: err})
			continue
		}
		exchanges = append(exchanges, ex)
	}
	return exchanges, skipped
}

// ToExchange converts a HAR entry to an exchange. See the package comment for
// the mapping. The returned exchange owns all its data; the entry is not
// modified.
func ToExchange(e *Entry) (*model.Exchange, error) {
	if e == nil {
		return nil, errors.New("har: nil entry")
	}
	u, err := parseRequestURL(e.Request.URL)
	if err != nil {
		return nil, err
	}

	// Request.
	reqHeader, authority := readHeaders(e.Request.Headers)
	host := reqHeader.Get("Host")
	reqHeader.Del("Host")
	if host == "" {
		host = authority
	}
	if host == "" {
		host = u.Host
	}
	if reqHeader.Get("Cookie") == "" && len(e.Request.Cookies) > 0 {
		if c := cookieHeader(e.Request.Cookies); c != "" {
			reqHeader = ensureHeader(reqHeader)
			reqHeader.Set("Cookie", c)
		}
	}
	method := strings.TrimSpace(e.Request.Method)
	if method == "" {
		method = http.MethodGet
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	rawQuery := u.RawQuery
	if rawQuery == "" && len(e.Request.QueryString) > 0 {
		rawQuery = encodeQuery(e.Request.QueryString)
	}
	reqBody, reqTruncated, err := requestBody(e.Request.PostData, &reqHeader)
	if err != nil {
		return nil, fmt.Errorf("har: request body: %w", err)
	}
	reqEncoding := takeContentEncoding(reqHeader)
	if reqEncoding != "" {
		var cut bool
		reqBody, cut = maybeInflate(reqEncoding, reqBody)
		reqTruncated = reqTruncated || cut
	}

	ex := &model.Exchange{
		StartedAt: e.StartedDateTime.Time,
		Request: model.Request{
			Method:          method,
			Scheme:          u.Scheme,
			Host:            host,
			Path:            path,
			RawQuery:        rawQuery,
			Proto:           normalizeProto(e.Request.HTTPVersion),
			Header:          reqHeader,
			Body:            reqBody,
			BodySize:        max(int64(e.Request.BodySize), int64(len(reqBody))),
			ContentEncoding: reqEncoding,
		},
	}
	ex.Request.BodyTruncated = reqTruncated || ex.Request.BodySize > int64(len(reqBody))

	// Response.
	respHeader, _ := readHeaders(e.Response.Headers)
	if len(respHeader["Set-Cookie"]) == 0 && len(e.Response.Cookies) > 0 {
		for _, c := range e.Response.Cookies {
			if s := setCookieString(c); s != "" {
				respHeader = ensureHeader(respHeader)
				respHeader.Add("Set-Cookie", s)
			}
		}
	}
	if respHeader.Get("Location") == "" && e.Response.RedirectURL != "" {
		respHeader = ensureHeader(respHeader)
		respHeader.Set("Location", e.Response.RedirectURL)
	}
	if mt := e.Response.Content.MimeType; respHeader.Get("Content-Type") == "" && mt != "" && mt != "x-unknown" {
		respHeader = ensureHeader(respHeader)
		respHeader.Set("Content-Type", mt)
	}
	respBody, err := decodeText(e.Response.Content.Text, e.Response.Content.Encoding)
	if err != nil {
		return nil, fmt.Errorf("har: response body: %w", err)
	}
	respEncoding := takeContentEncoding(respHeader)
	respCut := false
	if respEncoding != "" {
		respBody, respCut = maybeInflate(respEncoding, respBody)
	}
	ex.Response = model.Response{
		Status:          e.Response.Status,
		Proto:           normalizeProto(e.Response.HTTPVersion),
		Header:          respHeader,
		Body:            respBody,
		BodySize:        max(int64(e.Response.Content.Size), int64(len(respBody))),
		ContentEncoding: respEncoding,
	}
	ex.Response.BodyTruncated = e.Response.Content.Truncated || respCut || ex.Response.BodySize > int64(len(respBody))
	switch {
	case e.Response.Status == 0 && e.Response.Error != "":
		ex.Error = e.Response.Error
		if !strings.Contains(ex.Error, noResponseError) {
			ex.Error = noResponseError + ": " + ex.Error
		}
	case e.Response.Status == 0:
		ex.Error = noResponseError
	default:
		ex.Error = e.Response.Error
	}

	// Timing.
	ex.Duration, ex.TTFB = durations(e)
	return ex, nil
}

// parseRequestURL parses and validates an entry's request URL.
func parseRequestURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("har: entry has no request URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("har: invalid request URL %q: %w", abbreviate(raw), err)
	}
	switch u.Scheme {
	case "http", "https":
		if u.Host == "" {
			return nil, fmt.Errorf("har: request URL %q has no host", abbreviate(raw))
		}
		return u, nil
	case "":
		return nil, fmt.Errorf("har: request URL %q is not absolute", abbreviate(raw))
	default:
		return nil, fmt.Errorf("har: %w: scheme %q (%s)", ErrUnsupported, u.Scheme, abbreviate(raw))
	}
}

func abbreviate(s string) string {
	const limit = 80
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

// readHeaders converts HAR headers to an http.Header, skipping HTTP/2 pseudo
// headers. The value of ":authority" is returned separately. The result is
// nil when there are no headers.
func readHeaders(in []NameValue) (h http.Header, authority string) {
	for _, nv := range in {
		switch {
		case nv.Name == "":
			continue
		case nv.Name[0] == ':':
			if authority == "" && strings.EqualFold(nv.Name, ":authority") {
				authority = nv.Value
			}
			continue
		}
		if h == nil {
			h = make(http.Header, len(in))
		}
		h.Add(nv.Name, nv.Value)
	}
	return h, authority
}

func ensureHeader(h http.Header) http.Header {
	if h == nil {
		return make(http.Header, 2)
	}
	return h
}

// takeContentEncoding removes Content-Encoding from h and returns its
// lower-cased value ("" for none or "identity").
func takeContentEncoding(h http.Header) string {
	v := strings.ToLower(strings.TrimSpace(h.Get("Content-Encoding")))
	if v == "" {
		return ""
	}
	h.Del("Content-Encoding")
	if v == "identity" {
		return ""
	}
	return v
}

// requestBody returns the request body described by pd and whether it was
// truncated. A params-only body is re-encoded; a Content-Type header is
// filled in from pd when the headers lack one (hdr may be allocated).
func requestBody(pd *PostData, hdr *http.Header) (body []byte, truncated bool, err error) {
	if pd == nil {
		return nil, false, nil
	}
	if pd.MimeType != "" && hdr.Get("Content-Type") == "" {
		*hdr = ensureHeader(*hdr)
		hdr.Set("Content-Type", pd.MimeType)
	}
	if pd.Text != "" {
		body, err = decodeText(pd.Text, pd.Encoding)
		return body, pd.Truncated, err
	}
	if len(pd.Params) == 0 {
		return nil, pd.Truncated, nil
	}
	body, contentType := paramsBody(pd.Params, pd.MimeType, *hdr)
	if hdr.Get("Content-Type") == "" {
		*hdr = ensureHeader(*hdr)
		hdr.Set("Content-Type", contentType)
	}
	return body, pd.Truncated, nil
}

// paramsBody re-encodes posted form fields. It builds a multipart body when
// the MIME type (or the Content-Type header) names a boundary, otherwise a
// urlencoded form. The second result is the Content-Type of the body built.
func paramsBody(params []Param, mimeType string, hdr http.Header) ([]byte, string) {
	ct := mimeType
	if ct == "" {
		ct = hdr.Get("Content-Type")
	}
	if media, p, err := mime.ParseMediaType(ct); err == nil && media == "multipart/form-data" && p["boundary"] != "" {
		if body, ok := multipartBody(params, p["boundary"]); ok {
			return body, ct
		}
	}
	var sb strings.Builder
	for i, p := range params {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(url.QueryEscape(p.Name))
		sb.WriteByte('=')
		sb.WriteString(url.QueryEscape(p.Value))
	}
	return []byte(sb.String()), "application/x-www-form-urlencoded"
}

var quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

func multipartBody(params []Param, boundary string) ([]byte, bool) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.SetBoundary(boundary); err != nil {
		return nil, false
	}
	for _, p := range params {
		h := make(textproto.MIMEHeader, 2)
		disp := `form-data; name="` + quoteEscaper.Replace(p.Name) + `"`
		if p.FileName != "" {
			disp += `; filename="` + quoteEscaper.Replace(p.FileName) + `"`
		}
		h.Set("Content-Disposition", disp)
		if p.ContentType != "" {
			h.Set("Content-Type", p.ContentType)
		}
		w, err := mw.CreatePart(h)
		if err != nil {
			return nil, false
		}
		if _, err := io.WriteString(w, p.Value); err != nil {
			return nil, false
		}
	}
	if err := mw.Close(); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

// encodeQuery joins query string pairs into a raw query.
func encodeQuery(qs []NameValue) string {
	var sb strings.Builder
	for i, nv := range qs {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(url.QueryEscape(nv.Name))
		sb.WriteByte('=')
		sb.WriteString(url.QueryEscape(nv.Value))
	}
	return sb.String()
}

// decodeText returns the bytes a HAR text field stands for.
func decodeText(text, encoding string) ([]byte, error) {
	if text == "" {
		return nil, nil
	}
	if strings.EqualFold(strings.TrimSpace(encoding), "base64") {
		return decodeBase64(text)
	}
	return []byte(text), nil
}

// decodeBase64 accepts standard and URL-safe alphabets, with or without
// padding, and ignores line breaks.
func decodeBase64(s string) ([]byte, error) {
	if strings.ContainsAny(s, "\r\n \t") {
		s = strings.Map(func(r rune) rune {
			switch r {
			case '\r', '\n', ' ', '\t':
				return -1
			}
			return r
		}, s)
	}
	var firstErr error
	for _, enc := range [...]*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		b, err := enc.DecodeString(s)
		if err == nil {
			return b, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, fmt.Errorf("invalid base64: %w", firstErr)
}

// maybeInflate decompresses a body that is still gzip-compressed even though
// HAR bodies are normally stored decoded. Anything else, and a stream that
// does not decompress, is returned as is. At most maxInflate bytes are
// produced; truncated reports that the stream held more.
func maybeInflate(encoding string, body []byte) (out []byte, truncated bool) {
	if encoding != "gzip" && encoding != "x-gzip" {
		return body, false
	}
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		return body, false
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return body, false
	}
	defer zr.Close()
	out, err = io.ReadAll(io.LimitReader(zr, int64(maxInflate)+1))
	if err != nil {
		return body, false
	}
	if len(out) > maxInflate {
		return out[:maxInflate:maxInflate], true
	}
	return out, false
}

// cookieHeader renders request cookies as a Cookie header value.
func cookieHeader(cs []Cookie) string {
	var sb strings.Builder
	for _, c := range cs {
		s := (&http.Cookie{Name: c.Name, Value: c.Value}).String()
		if s == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("; ")
		}
		sb.WriteString(s)
	}
	return sb.String()
}

// setCookieString renders a response cookie as a Set-Cookie header value.
func setCookieString(c Cookie) string {
	hc := &http.Cookie{Name: c.Name, Value: c.Value, Path: c.Path, Domain: c.Domain, HttpOnly: c.HTTPOnly, Secure: c.Secure}
	if c.Expires != "" {
		if t, err := parseTimestamp(c.Expires); err == nil {
			hc.Expires = t
		}
	}
	return hc.String()
}

// normalizeProto maps the protocol spellings found in HAR files to the form
// net/http uses ("HTTP/1.1", "HTTP/2.0"). Unknown values are returned as is.
func normalizeProto(v string) string {
	switch l := strings.ToLower(strings.TrimSpace(v)); {
	case l == "":
		return ""
	case l == "h2" || l == "h2c" || l == "http/2" || l == "http/2.0":
		return "HTTP/2.0"
	case l == "h3" || strings.HasPrefix(l, "h3-") || l == "http/3" || l == "http/3.0":
		return "HTTP/3.0"
	case l == "http/1.1" || l == "http/1.0" || l == "http/0.9":
		return strings.ToUpper(l)
	}
	return v
}

// durations derives Duration and TTFB from an entry. Duration is the entry's
// time; when that is missing it is the sum of the timing phases. TTFB is send
// plus wait when a wait is present, never longer than Duration.
func durations(e *Entry) (dur, ttfb model.Duration) {
	t := &e.Timings
	sum := pos(t.Blocked) + pos(t.DNS) + pos(t.Connect) + pos(t.Send) + pos(t.Wait) + pos(t.Receive)
	ms := e.Time
	if ms < 0 || (ms == 0 && sum > 0) {
		ms = sum
	}
	dur = msToDuration(ms)
	if t.Wait >= 0 {
		ttfb = min(msToDuration(pos(t.Send)+t.Wait), dur)
	}
	return dur, ttfb
}

// pos clamps the "not applicable" value -1 (and anything below zero) to 0.
func pos(ms float64) float64 { return math.Max(ms, 0) }

// msToDuration converts fractional milliseconds to a duration, rounding to
// the nearest nanosecond and saturating instead of overflowing.
func msToDuration(ms float64) model.Duration {
	const maxMs = float64(math.MaxInt64) / float64(time.Millisecond)
	switch {
	case ms <= 0 || math.IsNaN(ms):
		return 0
	case ms >= maxMs:
		return model.Duration(math.MaxInt64)
	}
	return model.Duration(math.Round(ms * float64(time.Millisecond)))
}
