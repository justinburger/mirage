package har

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

func sampleExchange() *model.Exchange {
	return &model.Exchange{
		ID: 7, RecordingID: "rec_1", Seq: 3, ServiceID: "svc_1",
		StartedAt: time.Date(2024, 3, 5, 17, 20, 31, 123_456_789, time.UTC),
		Duration:  model.Duration(187250 * time.Microsecond),
		TTFB:      model.Duration(172 * time.Millisecond),
		Endpoint:  "GET /v1/users/{id}",
		Request: model.Request{
			Method: "GET", Scheme: "https", Host: "api.example.com", Path: "/v1/users/42",
			RawQuery: "expand=orders&limit=5&q=a%20b&flag", Proto: "HTTP/2.0",
			Header: http.Header{
				"Accept":       {"application/json"},
				"Cookie":       {"sid=abc; theme=dark"},
				"X-Request-Id": {"r-1", "r-2"},
			},
		},
		Response: model.Response{
			Status: 200, Proto: "HTTP/2.0",
			Header: http.Header{
				"Content-Type":  {"application/json; charset=utf-8"},
				"Set-Cookie":    {"sid=n3w; Path=/; HttpOnly; Secure", "seen=1; Path=/app; Expires=Wed, 21 Oct 2037 07:28:00 GMT"},
				"Cache-Control": {"no-store"},
			},
			Body:     []byte(`{"id":42,"name":"Jane <jane@example.com>","ok":true}`),
			BodySize: 52,
		},
	}
}

func TestFromExchange(t *testing.T) {
	ex := sampleExchange()
	e := FromExchange(ex)

	if !e.StartedDateTime.Equal(ex.StartedAt) {
		t.Errorf("startedDateTime = %v", e.StartedDateTime)
	}
	if e.Time != 187.25 {
		t.Errorf("time = %v", e.Time)
	}
	r := e.Request
	if r.Method != "GET" || r.URL != "https://api.example.com/v1/users/42?expand=orders&limit=5&q=a%20b&flag" || r.HTTPVersion != "HTTP/2.0" {
		t.Errorf("request = %s %s %s", r.Method, r.URL, r.HTTPVersion)
	}
	wantQS := []NameValue{{Name: "expand", Value: "orders"}, {Name: "limit", Value: "5"}, {Name: "q", Value: "a b"}, {Name: "flag"}}
	if !reflect.DeepEqual(r.QueryString, wantQS) {
		t.Errorf("queryString = %+v", r.QueryString)
	}
	wantHeaders := []NameValue{
		{Name: "Accept", Value: "application/json"},
		{Name: "Cookie", Value: "sid=abc; theme=dark"},
		{Name: "X-Request-Id", Value: "r-1"},
		{Name: "X-Request-Id", Value: "r-2"},
	}
	if !reflect.DeepEqual(r.Headers, wantHeaders) {
		t.Errorf("headers = %+v", r.Headers)
	}
	if want := []Cookie{{Name: "sid", Value: "abc"}, {Name: "theme", Value: "dark"}}; !reflect.DeepEqual(r.Cookies, want) {
		t.Errorf("cookies = %+v", r.Cookies)
	}
	if r.PostData != nil || r.BodySize != 0 || r.HeadersSize != -1 {
		t.Errorf("postData %v bodySize %d headersSize %d", r.PostData, r.BodySize, r.HeadersSize)
	}

	p := e.Response
	if p.Status != 200 || p.StatusText != "OK" || p.HTTPVersion != "HTTP/2.0" || p.RedirectURL != "" {
		t.Errorf("response = %+v", p)
	}
	if p.Content.Size != 52 || p.Content.MimeType != "application/json; charset=utf-8" || p.Content.Text != string(ex.Response.Body) || p.Content.Encoding != "" {
		t.Errorf("content = %+v", p.Content)
	}
	if p.BodySize != 52 {
		t.Errorf("bodySize = %d", p.BodySize)
	}
	if len(p.Cookies) != 2 {
		t.Fatalf("response cookies = %+v", p.Cookies)
	}
	if c := p.Cookies[0]; c.Name != "sid" || c.Value != "n3w" || c.Path != "/" || !c.HTTPOnly || !c.Secure || c.Expires != "" {
		t.Errorf("cookie 0 = %+v", c)
	}
	if c := p.Cookies[1]; c.Name != "seen" || c.Path != "/app" || c.Expires != "2037-10-21T07:28:00.000Z" {
		t.Errorf("cookie 1 = %+v", c)
	}
	// Headers are sorted by name.
	var names []string
	for _, h := range p.Headers {
		names = append(names, h.Name)
	}
	if got := strings.Join(names, ","); got != "Cache-Control,Content-Type,Set-Cookie,Set-Cookie" {
		t.Errorf("response header order = %s", got)
	}
	if p.Headers[2].Value != "sid=n3w; Path=/; HttpOnly; Secure" {
		t.Errorf("Set-Cookie order lost: %+v", p.Headers)
	}

	tm := e.Timings
	if tm.Blocked != -1 || tm.DNS != -1 || tm.Connect != -1 || tm.SSL != -1 || tm.Send != 0 || tm.Wait != 172 || tm.Receive != 15.25 {
		t.Errorf("timings = %+v", tm)
	}
	if tm.Send+tm.Wait+tm.Receive != e.Time {
		t.Errorf("timings do not add up to time: %+v vs %v", tm, e.Time)
	}
}

func TestFromExchangeDoesNotModifyInput(t *testing.T) {
	ex := sampleExchange()
	ex.Response.ContentEncoding = "gzip"
	want := ex.Clone()
	_ = FromExchange(ex)
	if !reflect.DeepEqual(ex, want) {
		t.Error("FromExchange modified its input")
	}
}

func TestFromExchangeBodies(t *testing.T) {
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")
	tests := []struct {
		name         string
		body         []byte
		wantText     string
		wantEncoding string
	}{
		{"empty", nil, "", ""},
		{"ascii", []byte("hello"), "hello", ""},
		{"utf8", []byte("héllo ✓ 日本"), "héllo ✓ 日本", ""},
		{"html is not escaped", []byte("<a href=\"x\">&</a>"), "<a href=\"x\">&</a>", ""},
		{"control chars", []byte("a\tb\nc\r\n\x01"), "a\tb\nc\r\n\x01", ""},
		{"png", png, base64.StdEncoding.EncodeToString(png), "base64"},
		{"invalid utf8", []byte{'a', 0xff, 'b'}, base64.StdEncoding.EncodeToString([]byte{'a', 0xff, 'b'}), "base64"},
		{"valid utf8 with NUL", []byte("ab\x00cd"), base64.StdEncoding.EncodeToString([]byte("ab\x00cd")), "base64"},
		{"lone continuation", []byte{0x80}, "gA==", "base64"},
		{"overlong", []byte{0xc0, 0xaf}, "wK8=", "base64"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ex := &model.Exchange{
				Request:  model.Request{Method: "POST", Host: "x.test", Path: "/", Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: tc.body, BodySize: int64(len(tc.body))},
				Response: model.Response{Status: 200, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: tc.body, BodySize: int64(len(tc.body))},
			}
			e := FromExchange(ex)
			if got := e.Response.Content; got.Text != tc.wantText || got.Encoding != tc.wantEncoding || int(got.Size) != len(tc.body) {
				t.Errorf("content = %+v", got)
			}
			if len(tc.body) == 0 {
				if e.Request.PostData != nil {
					t.Errorf("postData = %+v for empty body", e.Request.PostData)
				}
				return
			}
			if pd := e.Request.PostData; pd == nil || pd.Text != tc.wantText || pd.Encoding != tc.wantEncoding || pd.MimeType != "application/octet-stream" {
				t.Errorf("postData = %+v", pd)
			}
			// Whatever was written must survive JSON.
			b, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			var back Entry
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatal(err)
			}
			got, err := decodeText(back.Response.Content.Text, back.Response.Content.Encoding)
			if err != nil || !bytes.Equal(got, tc.body) {
				t.Errorf("round trip through JSON: % x, %v", got, err)
			}
		})
	}
}

func TestFromExchangeTruncated(t *testing.T) {
	ex := &model.Exchange{
		Request:  model.Request{Method: "POST", Host: "x.test", Path: "/", Body: []byte("abc"), BodySize: 1000, BodyTruncated: true},
		Response: model.Response{Status: 200, Body: []byte("xyz"), BodySize: 5000, BodyTruncated: true},
	}
	e := FromExchange(ex)
	if !e.Request.PostData.Truncated || !e.Response.Content.Truncated {
		t.Fatalf("truncated flags lost: %+v / %+v", e.Request.PostData, e.Response.Content)
	}
	if e.Request.BodySize != 1000 || e.Response.Content.Size != 5000 {
		t.Errorf("sizes = %d / %d", e.Request.BodySize, e.Response.Content.Size)
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), `"_mirageTruncated":true`); got != 2 {
		t.Errorf("_mirageTruncated appears %d times in %s", got, b)
	}
	// Not truncated: the field is absent.
	b, _ = json.Marshal(FromExchange(sampleExchange()))
	if strings.Contains(string(b), "_mirageTruncated") {
		t.Errorf("unexpected _mirageTruncated in %s", b)
	}
	// A body that was not captured at all still yields a postData with size.
	ex = &model.Exchange{Request: model.Request{Method: "POST", Host: "x.test", Path: "/", BodySize: 77, BodyTruncated: true}}
	if pd := FromExchange(ex).Request.PostData; pd == nil || !pd.Truncated || pd.Text != "" {
		t.Errorf("postData = %+v", pd)
	}
}

func TestFromExchangeDefaultsAndURL(t *testing.T) {
	tests := []struct {
		name    string
		req     model.Request
		wantURL string
		wantVer string
	}{
		{"all defaults", model.Request{Method: "GET"}, "http://localhost/", "HTTP/1.1"},
		{"no scheme", model.Request{Method: "GET", Host: "a.test", Path: "/x"}, "http://a.test/x", "HTTP/1.1"},
		{"relative path", model.Request{Method: "GET", Scheme: "https", Host: "a.test", Path: "x/y"}, "https://a.test/x/y", "HTTP/1.1"},
		{"port and query", model.Request{Method: "GET", Scheme: "http", Host: "a.test:8080", Path: "/x", RawQuery: "a=1&b=2", Proto: "HTTP/1.0"}, "http://a.test:8080/x?a=1&b=2", "HTTP/1.0"},
		{"escaped path", model.Request{Method: "GET", Scheme: "https", Host: "a.test", Path: "/a%2Fb/%E2%82%AC"}, "https://a.test/a%2Fb/%E2%82%AC", "HTTP/1.1"},
	}
	for _, tc := range tests {
		e := FromExchange(&model.Exchange{Request: tc.req})
		if e.Request.URL != tc.wantURL || e.Request.HTTPVersion != tc.wantVer {
			t.Errorf("%s: url %q version %q, want %q %q", tc.name, e.Request.URL, e.Request.HTTPVersion, tc.wantURL, tc.wantVer)
		}
		// No response (status 0): the protocol stays empty, as in Chrome.
		if e.Response.HTTPVersion != "" {
			t.Errorf("%s: response version %q", tc.name, e.Response.HTTPVersion)
		}
	}
}

func TestFromExchangeResponseProtoDefault(t *testing.T) {
	e := FromExchange(&model.Exchange{Response: model.Response{Status: 200}})
	if e.Response.HTTPVersion != "HTTP/1.1" {
		t.Errorf("response version = %q, want the HTTP/1.1 default for a real response", e.Response.HTTPVersion)
	}
}

func TestFromExchangeNil(t *testing.T) {
	e := FromExchange(nil)
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(b) {
		t.Errorf("invalid JSON: %s", b)
	}
}

func TestFromExchangeContentEncodingAndRedirect(t *testing.T) {
	ex := &model.Exchange{
		Request:  model.Request{Method: "POST", Host: "x.test", Path: "/", ContentEncoding: "gzip", Body: []byte("{}"), Header: http.Header{"Content-Type": {"application/json"}}},
		Response: model.Response{Status: 301, ContentEncoding: "br", Header: http.Header{"Location": {"https://x.test/new"}}, Body: []byte("moved"), BodySize: 5},
	}
	e := FromExchange(ex)
	if !hasHeader(e.Request.Headers, "Content-Encoding", "gzip") || !hasHeader(e.Response.Headers, "Content-Encoding", "br") {
		t.Errorf("Content-Encoding not restored: %+v / %+v", e.Request.Headers, e.Response.Headers)
	}
	if e.Response.RedirectURL != "https://x.test/new" || e.Response.StatusText != "Moved Permanently" {
		t.Errorf("response = %+v", e.Response)
	}
	if e.Response.BodySize != -1 {
		t.Errorf("wire bodySize = %d, want -1 for an encoded body", e.Response.BodySize)
	}
	// A header that still carries Content-Encoding is not duplicated.
	ex.Response.Header.Set("Content-Encoding", "br")
	e = FromExchange(ex)
	n := 0
	for _, h := range e.Response.Headers {
		if h.Name == "Content-Encoding" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("Content-Encoding appears %d times", n)
	}
	// A redirect URL is only reported for 3xx.
	ex.Response.Status = 200
	if got := FromExchange(ex).Response.RedirectURL; got != "" {
		t.Errorf("redirectURL = %q for 200", got)
	}
}

func hasHeader(hs []NameValue, name, value string) bool {
	for _, h := range hs {
		if h.Name == name && h.Value == value {
			return true
		}
	}
	return false
}

func TestFromExchangeTimingsEdgeCases(t *testing.T) {
	tests := []struct {
		name      string
		dur, ttfb time.Duration
		wantTime  float64
		wantWait  float64
		wantRecv  float64
	}{
		{"normal", 100 * time.Millisecond, 40 * time.Millisecond, 100, 40, 60},
		{"no ttfb", 100 * time.Millisecond, 0, 100, 0, 100},
		{"ttfb longer than duration", 10 * time.Millisecond, 50 * time.Millisecond, 10, 10, 0},
		{"zero", 0, 0, 0, 0, 0},
		{"negative duration", -5 * time.Millisecond, 0, 0, 0, 0},
		{"negative ttfb", 10 * time.Millisecond, -1, 10, 0, 10},
	}
	for _, tc := range tests {
		e := FromExchange(&model.Exchange{Duration: model.Duration(tc.dur), TTFB: model.Duration(tc.ttfb)})
		if e.Time != tc.wantTime || e.Timings.Wait != tc.wantWait || e.Timings.Receive != tc.wantRecv || e.Timings.Send != 0 {
			t.Errorf("%s: time %v timings %+v", tc.name, e.Time, e.Timings)
		}
	}
}

// TestEntryJSONShape checks the output against what HAR 1.2 requires and
// what Chrome devtools reads: required keys present, arrays never null.
func TestEntryJSONShape(t *testing.T) {
	exs := []*model.Exchange{
		sampleExchange(),
		{Request: model.Request{Method: "GET"}}, // as bare as it gets
		{Request: model.Request{Method: "POST", Host: "x.test", Path: "/", Body: []byte{0xff}}, Response: model.Response{Status: 502}, Error: "dial tcp: refused"},
	}
	for i, ex := range exs {
		b, err := json.Marshal(FromExchange(ex))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		requireKeys(t, i, "entry", m, "startedDateTime", "time", "request", "response", "cache", "timings")
		req := m["request"].(map[string]any)
		requireKeys(t, i, "request", req, "method", "url", "httpVersion", "cookies", "headers", "queryString", "headersSize", "bodySize")
		resp := m["response"].(map[string]any)
		requireKeys(t, i, "response", resp, "status", "statusText", "httpVersion", "cookies", "headers", "content", "redirectURL", "headersSize", "bodySize")
		requireKeys(t, i, "content", resp["content"].(map[string]any), "size", "mimeType")
		requireKeys(t, i, "timings", m["timings"].(map[string]any), "send", "wait", "receive")
		for _, arr := range []any{req["cookies"], req["headers"], req["queryString"], resp["cookies"], resp["headers"]} {
			if _, ok := arr.([]any); !ok {
				t.Errorf("#%d: expected a JSON array, got %T (%s)", i, arr, b)
			}
		}
		if _, ok := m["startedDateTime"].(string); !ok {
			t.Errorf("#%d: startedDateTime is %T", i, m["startedDateTime"])
		}
	}
}

func requireKeys(t *testing.T, i int, what string, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("#%d: %s lacks %q", i, what, k)
		}
	}
}

func TestErrorFieldOnlyWhenSet(t *testing.T) {
	b, _ := json.Marshal(FromExchange(sampleExchange()))
	if strings.Contains(string(b), "_error") {
		t.Errorf("_error present for a clean exchange: %s", b)
	}
	ex := sampleExchange()
	ex.Error = "upstream timeout"
	ex.Response.Status = 504
	b, _ = json.Marshal(FromExchange(ex))
	if !strings.Contains(string(b), `"_error":"upstream timeout"`) {
		t.Errorf("_error missing: %s", b)
	}
}

func TestQueryPairs(t *testing.T) {
	tests := []struct {
		raw  string
		want []NameValue
	}{
		{"", []NameValue{}},
		{"a=1", []NameValue{{Name: "a", Value: "1"}}},
		{"a=1&&b=2&", []NameValue{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}}},
		{"a=1=2", []NameValue{{Name: "a", Value: "1=2"}}},
		{"flag", []NameValue{{Name: "flag"}}},
		{"=v", []NameValue{{Name: "", Value: "v"}}},
		{"a+b=c+d&e%20f=g%26h", []NameValue{{Name: "a b", Value: "c d"}, {Name: "e f", Value: "g&h"}}},
		{"bad=%zz&ok=%41", []NameValue{{Name: "bad", Value: "%zz"}, {Name: "ok", Value: "A"}}},
		{"x=1&x=2", []NameValue{{Name: "x", Value: "1"}, {Name: "x", Value: "2"}}},
	}
	for _, tc := range tests {
		got := queryPairs(tc.raw)
		if got == nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("queryPairs(%q) = %#v, want %#v", tc.raw, got, tc.want)
		}
	}
}

func TestCookieParsing(t *testing.T) {
	// Malformed cookie headers are skipped, not fatal.
	h := http.Header{"Cookie": {"a=1; b=2", ";;;", "c"}, "Set-Cookie": {"ok=1; Path=/", "=", "bad name=1"}}
	req := requestCookies(h)
	if len(req) < 2 || req[0].Name != "a" || req[1].Name != "b" {
		t.Errorf("request cookies = %+v", req)
	}
	resp := responseCookies(h)
	if len(resp) != 1 || resp[0].Name != "ok" {
		t.Errorf("response cookies = %+v", resp)
	}
	if got := cookieHeader([]Cookie{{Name: "bad name", Value: "x"}, {Name: "a", Value: "1"}, {Name: "b", Value: "has space"}}); !strings.HasPrefix(got, "a=1; b=") {
		t.Errorf("cookieHeader = %q", got)
	}
	if got := setCookieString(Cookie{Name: "", Value: "x"}); got != "" {
		t.Errorf("setCookieString(empty name) = %q", got)
	}
}

func TestMultipartInvalidBoundaryFallsBack(t *testing.T) {
	body, ct := paramsBody([]Param{{Name: "a", Value: "1"}}, "multipart/form-data; boundary="+strings.Repeat("x", 100), nil)
	if string(body) != "a=1" || ct != "application/x-www-form-urlencoded" {
		t.Errorf("body %q content-type %q", body, ct)
	}
}
