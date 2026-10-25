package har

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/justinburger/mirage/internal/model"
)

// FromExchange converts an exchange to a HAR entry. The result always has
// non-nil cookies, headers and queryString arrays so it marshals the way
// Chrome devtools expects. A nil exchange yields an empty entry.
func FromExchange(ex *model.Exchange) Entry {
	if ex == nil {
		ex = &model.Exchange{}
	}
	dur := max(ex.Duration.Ms(), 0)
	ttfb := min(max(ex.TTFB.Ms(), 0), dur)

	return Entry{
		StartedDateTime: Timestamp{Time: ex.StartedAt},
		Time:            dur,
		Request:         requestFromExchange(&ex.Request),
		Response:        responseFromExchange(ex),
		Timings: Timings{
			Blocked: -1, DNS: -1, Connect: -1, SSL: -1,
			Send: 0, Wait: ttfb, Receive: dur - ttfb,
		},
	}
}

func requestFromExchange(r *model.Request) Request {
	scheme, host, path := r.Scheme, r.Host, r.Path
	if scheme == "" {
		scheme = "http"
	}
	if host == "" {
		host = "localhost"
	}
	if path == "" || path[0] != '/' {
		path = "/" + path
	}
	rawURL := scheme + "://" + host + path
	if r.RawQuery != "" {
		rawURL += "?" + r.RawQuery
	}
	req := Request{
		Method:      r.Method,
		URL:         rawURL,
		HTTPVersion: cmp.Or(r.Proto, "HTTP/1.1"),
		Cookies:     requestCookies(r.Header),
		Headers:     headerList(r.Header, r.ContentEncoding),
		QueryString: queryPairs(r.RawQuery),
		HeadersSize: -1,
		BodySize:    Size(max(r.BodySize, int64(len(r.Body)))),
	}
	if len(r.Body) > 0 || r.BodySize > 0 || r.BodyTruncated {
		pd := &PostData{MimeType: r.Header.Get("Content-Type"), Truncated: r.BodyTruncated}
		pd.Text, pd.Encoding = encodeBody(r.Body)
		req.PostData = pd
	}
	return req
}

func responseFromExchange(ex *model.Exchange) Response {
	r := &ex.Response
	size := max(r.BodySize, int64(len(r.Body)))
	c := Content{
		Size:      Size(size),
		MimeType:  r.Header.Get("Content-Type"),
		Truncated: r.BodyTruncated,
	}
	c.Text, c.Encoding = encodeBody(r.Body)

	wire := Size(size)
	if r.ContentEncoding != "" {
		wire = -1 // the compressed size was not recorded
	}
	proto := r.Proto
	if proto == "" && r.Status != 0 {
		proto = "HTTP/1.1"
	}
	resp := Response{
		Status:      r.Status,
		StatusText:  http.StatusText(r.Status),
		HTTPVersion: proto,
		Cookies:     responseCookies(r.Header),
		Headers:     headerList(r.Header, r.ContentEncoding),
		Content:     c,
		HeadersSize: -1,
		BodySize:    wire,
		Error:       ex.Error,
	}
	if r.Status >= 300 && r.Status < 400 {
		resp.RedirectURL = r.Header.Get("Location")
	}
	return resp
}

// encodeBody returns a body as HAR text. Bodies that are not valid UTF-8, or
// that contain NUL bytes near the start, are base64 encoded.
func encodeBody(b []byte) (text, encoding string) {
	if len(b) == 0 {
		return "", ""
	}
	if !utf8.Valid(b) || bytes.IndexByte(b[:min(len(b), 512)], 0) >= 0 {
		return base64.StdEncoding.EncodeToString(b), "base64"
	}
	return string(b), ""
}

// headerList flattens h into name/value pairs sorted by name (values of one
// name keep their order). A non-empty contentEncoding that h no longer
// carries is added back as a Content-Encoding header.
func headerList(h http.Header, contentEncoding string) []NameValue {
	out := make([]NameValue, 0, len(h)+1)
	for name, values := range h {
		for _, v := range values {
			out = append(out, NameValue{Name: name, Value: v})
		}
	}
	if contentEncoding != "" && len(h["Content-Encoding"]) == 0 {
		out = append(out, NameValue{Name: "Content-Encoding", Value: contentEncoding})
	}
	slices.SortStableFunc(out, func(a, b NameValue) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// queryPairs splits a raw query into decoded name/value pairs, in order.
func queryPairs(raw string) []NameValue {
	out := make([]NameValue, 0, strings.Count(raw, "&")+1)
	for raw != "" {
		var pair string
		pair, raw, _ = strings.Cut(raw, "&")
		if pair == "" {
			continue
		}
		name, value, _ := strings.Cut(pair, "=")
		out = append(out, NameValue{Name: unescapeQuery(name), Value: unescapeQuery(value)})
	}
	return out
}

func unescapeQuery(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	if u, err := url.QueryUnescape(s); err == nil {
		return u
	}
	return s
}

func requestCookies(h http.Header) []Cookie {
	out := []Cookie{}
	for _, line := range h.Values("Cookie") {
		cs, err := http.ParseCookie(line)
		if err != nil {
			continue
		}
		for _, c := range cs {
			out = append(out, Cookie{Name: c.Name, Value: c.Value})
		}
	}
	return out
}

func responseCookies(h http.Header) []Cookie {
	out := []Cookie{}
	for _, line := range h.Values("Set-Cookie") {
		c, err := http.ParseSetCookie(line)
		if err != nil {
			continue
		}
		hc := Cookie{Name: c.Name, Value: c.Value, Path: c.Path, Domain: c.Domain, HTTPOnly: c.HttpOnly, Secure: c.Secure}
		if !c.Expires.IsZero() {
			hc.Expires = c.Expires.UTC().Format("2006-01-02T15:04:05.000Z07:00")
		}
		out = append(out, hc)
	}
	return out
}
