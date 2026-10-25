package har

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

func exchangeAt(t *testing.T, name string, i int) *model.Exchange {
	t.Helper()
	h := fixture(t, name)
	ex, err := ToExchange(&h.Log.Entries[i])
	if err != nil {
		t.Fatalf("ToExchange(%s[%d]): %v", name, i, err)
	}
	return ex
}

func TestToExchangeChromeJSON(t *testing.T) {
	ex := exchangeAt(t, "chrome.har", 0)
	r := ex.Request
	if r.Method != "GET" || r.Scheme != "https" || r.Host != "api.example.com" || r.Path != "/v1/users/42" {
		t.Errorf("request line = %s %s://%s%s", r.Method, r.Scheme, r.Host, r.Path)
	}
	if r.RawQuery != "expand=orders&limit=5&q=a%20b" || r.URI() != "/v1/users/42?expand=orders&limit=5&q=a%20b" {
		t.Errorf("query = %q", r.RawQuery)
	}
	if r.Proto != "HTTP/2.0" {
		t.Errorf("proto = %q", r.Proto)
	}
	wantReq := http.Header{
		"Accept":          {"application/json"},
		"Accept-Encoding": {"gzip, deflate, br"},
		"Authorization":   {"Bearer abc.def.ghi"},
		"Cookie":          {"sid=s3cr3t; theme=dark"},
		"X-Request-Id":    {"r-1", "r-2"},
	}
	if !reflect.DeepEqual(r.Header, wantReq) {
		t.Errorf("request headers = %v\nwant %v", r.Header, wantReq)
	}
	for k := range r.Header {
		if strings.HasPrefix(k, ":") {
			t.Errorf("pseudo-header %q kept", k)
		}
	}
	if len(r.Body) != 0 || r.BodySize != 0 || r.BodyTruncated {
		t.Errorf("request body = %q size %d truncated %v", r.Body, r.BodySize, r.BodyTruncated)
	}

	if want := time.Date(2024, 3, 5, 17, 20, 31, 123_000_000, time.UTC); !ex.StartedAt.Equal(want) {
		t.Errorf("StartedAt = %v", ex.StartedAt)
	}
	if ex.Duration.D() != 187250*time.Microsecond {
		t.Errorf("Duration = %v, want 187.25ms", ex.Duration)
	}
	if ex.TTFB.D() != 172250*time.Microsecond { // send 0.4 + wait 171.85
		t.Errorf("TTFB = %v, want 172.25ms", ex.TTFB)
	}
	if ex.Endpoint != "" || ex.ID != 0 || ex.RecordingID != "" || ex.Anonymized {
		t.Errorf("store/pathnorm fields set: %+v", ex)
	}

	resp := ex.Response
	const body = `{"id":42,"name":"Jane <jane@example.com>","ok":true}`
	if resp.Status != 200 || string(resp.Body) != body {
		t.Errorf("response = %d %q", resp.Status, resp.Body)
	}
	if resp.BodySize != 52 || resp.BodyTruncated {
		t.Errorf("BodySize = %d truncated %v", resp.BodySize, resp.BodyTruncated)
	}
	// Chrome stores the decoded text but keeps the Content-Encoding header;
	// the exchange form moves it out of the headers.
	if resp.ContentEncoding != "gzip" || resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("content encoding = %q, header %q", resp.ContentEncoding, resp.Header.Get("Content-Encoding"))
	}
	if resp.Proto != "HTTP/2.0" || resp.Header.Get("Content-Type") != "application/json; charset=utf-8" || resp.Header.Get("Content-Length") != "88" {
		t.Errorf("response headers = %v proto %q", resp.Header, resp.Proto)
	}
	if got := resp.Header.Values("Set-Cookie"); len(got) != 2 || got[0] != "sid=n3w; Path=/; HttpOnly; Secure" {
		t.Errorf("Set-Cookie = %q", got)
	}
	if ex.Error != "" || ex.IsError() {
		t.Errorf("Error = %q", ex.Error)
	}
}

func TestToExchangeBase64Image(t *testing.T) {
	ex := exchangeAt(t, "chrome.har", 1)
	want, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ex.Response.Body, want) || !bytes.HasPrefix(ex.Response.Body, []byte("\x89PNG\r\n\x1a\n")) {
		t.Errorf("body = % x", ex.Response.Body)
	}
	if ex.Response.BodySize != 70 || ex.Response.BodyTruncated {
		t.Errorf("BodySize = %d, truncated %v", ex.Response.BodySize, ex.Response.BodyTruncated)
	}
	if ex.Request.Host != "cdn.example.com" || ex.Request.Path != "/img/pixel.png" || ex.Request.RawQuery != "" {
		t.Errorf("request = %+v", ex.Request)
	}
	if ex.Duration.D() != 20500*time.Microsecond || ex.TTFB.D() != 15*time.Millisecond {
		t.Errorf("duration %v ttfb %v", ex.Duration, ex.TTFB)
	}
}

func TestToExchangeFormParams(t *testing.T) {
	ex := exchangeAt(t, "chrome.har", 2)
	if ex.Request.Method != "POST" {
		t.Errorf("method = %q", ex.Request.Method)
	}
	if got, want := string(ex.Request.Body), "user=jane%40example.com&note=two+words+%26+more&empty="; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if ex.Request.BodySize != 54 || ex.Request.BodyTruncated {
		t.Errorf("BodySize = %d truncated %v", ex.Request.BodySize, ex.Request.BodyTruncated)
	}
	if ct := ex.Request.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", ct)
	}
	// The Host header becomes Request.Host and is not kept in Header.
	if ex.Request.Host != "api.example.com" || ex.Request.Header.Get("Host") != "" {
		t.Errorf("host = %q, header %v", ex.Request.Host, ex.Request.Header)
	}
	if ex.Request.Proto != "HTTP/1.1" {
		t.Errorf("proto = %q", ex.Request.Proto)
	}
	// 302 with redirectURL.
	if ex.Response.Status != 302 || ex.Response.Header.Get("Location") != "/home" {
		t.Errorf("response = %d %v", ex.Response.Status, ex.Response.Header)
	}
	// "x-unknown" is Chrome's placeholder, not a content type.
	if ct := ex.Response.Header.Get("Content-Type"); ct != "" {
		t.Errorf("Content-Type = %q", ct)
	}
	if ex.Duration.D() != 55*time.Millisecond || ex.TTFB.D() != 51*time.Millisecond {
		t.Errorf("duration %v ttfb %v", ex.Duration, ex.TTFB)
	}
}

func TestToExchangeNoResponse(t *testing.T) {
	ex := exchangeAt(t, "chrome.har", 3)
	if ex.Response.Status != 0 {
		t.Errorf("status = %d", ex.Response.Status)
	}
	if !strings.HasPrefix(ex.Error, "no response in HAR") || !strings.Contains(ex.Error, "net::ERR_CONNECTION_REFUSED") {
		t.Errorf("Error = %q", ex.Error)
	}
	if !ex.IsError() {
		t.Error("exchange should count as an error")
	}
	// time = -1: duration is the sum of the applicable phases (2 + 3), and
	// there is no wait so no TTFB.
	if ex.Duration.D() != 5*time.Millisecond || ex.TTFB != 0 {
		t.Errorf("duration %v ttfb %v", ex.Duration, ex.TTFB)
	}
	if string(ex.Request.Body) != `{"sku":"A-1","qty":2}` || ex.Request.BodySize != 21 {
		t.Errorf("request body = %q (%d)", ex.Request.Body, ex.Request.BodySize)
	}
	if ex.Response.Proto != "" || len(ex.Response.Body) != 0 {
		t.Errorf("response = %+v", ex.Response)
	}
}

func TestToExchangeNoResponsePlain(t *testing.T) {
	e := &Entry{Request: Request{Method: "GET", URL: "http://x.test/"}}
	ex, err := ToExchange(e)
	if err != nil {
		t.Fatal(err)
	}
	if ex.Error != "no response in HAR" {
		t.Errorf("Error = %q, want exactly %q", ex.Error, "no response in HAR")
	}
}

func TestToExchangeUnsupportedSchemes(t *testing.T) {
	for _, u := range []string{
		"data:image/gif;base64,R0lGODlhAQABAAAAACw=",
		"blob:https://app.example.com/6c1b7d0e-aaaa-bbbb-cccc-000000000000",
		"ws://example.com/socket",
		"wss://example.com/socket",
		"chrome-extension://abcdef/popup.js",
		"file:///tmp/x.html",
		"ftp://example.com/x",
	} {
		_, err := ToExchange(&Entry{Request: Request{Method: "GET", URL: u}})
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("ToExchange(%q) err = %v, want ErrUnsupported", u, err)
		}
	}
}

func TestToExchangeInvalidURLs(t *testing.T) {
	for _, u := range []string{"", "   ", "/relative/path", "example.com/x", "http://", "https:///path", "http://[::1", "http://a b.test/", ":bad"} {
		_, err := ToExchange(&Entry{Request: Request{Method: "GET", URL: u}})
		if err == nil {
			t.Errorf("ToExchange(%q) succeeded", u)
			continue
		}
		if errors.Is(err, ErrUnsupported) {
			t.Errorf("ToExchange(%q) = %v, should not be ErrUnsupported", u, err)
		}
	}
	if _, err := ToExchange(nil); err == nil {
		t.Error("ToExchange(nil) succeeded")
	}
}

func TestToExchangeLongBadURLIsAbbreviated(t *testing.T) {
	_, err := ToExchange(&Entry{Request: Request{URL: "data:text/plain;base64," + strings.Repeat("A", 10000)}})
	if err == nil || len(err.Error()) > 300 {
		t.Errorf("error = %.120q (len %d)", err, len(err.Error()))
	}
}

func TestToExchanges(t *testing.T) {
	h := fixture(t, "chrome.har")
	exs, skipped := ToExchanges(h)
	if len(exs) != 4 || len(skipped) != 1 {
		t.Fatalf("exchanges = %d, skipped = %d", len(exs), len(skipped))
	}
	if skipped[0].Index != 4 || !errors.Is(skipped[0], ErrUnsupported) || !strings.Contains(skipped[0].Error(), "entry 4") {
		t.Errorf("skipped = %v", skipped[0])
	}
	if exs, sk := ToExchanges(nil); exs != nil || sk != nil {
		t.Errorf("ToExchanges(nil) = %v, %v", exs, sk)
	}
}

func TestToExchangeFirefox(t *testing.T) {
	ex := exchangeAt(t, "firefox.har", 0)
	if ex.Request.Scheme != "http" || ex.Request.Host != "shop.example.com:8080" || ex.Request.Path != "/search" {
		t.Errorf("request = %+v", ex.Request)
	}
	if ex.Request.RawQuery != "term=caf%C3%A9&page=2" {
		t.Errorf("raw query = %q", ex.Request.RawQuery)
	}
	if want := time.Date(2024, 3, 5, 17, 20, 31, 456_000_000, time.UTC); !ex.StartedAt.Equal(want) {
		t.Errorf("StartedAt = %v", ex.StartedAt)
	}
	if string(ex.Response.Body) != "<html><body>café & <b>more</b></body></html>" {
		t.Errorf("body = %q", ex.Response.Body)
	}
	// content.size says 1024 but the text is shorter: the captured body is
	// incomplete.
	if ex.Response.BodySize != 1024 || !ex.Response.BodyTruncated {
		t.Errorf("BodySize = %d truncated %v", ex.Response.BodySize, ex.Response.BodyTruncated)
	}
	if ex.Duration.D() != 94*time.Millisecond || ex.TTFB.D() != 70*time.Millisecond {
		t.Errorf("duration %v ttfb %v", ex.Duration, ex.TTFB)
	}

	ex = exchangeAt(t, "firefox.har", 1)        // time = -1, bodySize = -1
	if ex.Duration.D() != 25*time.Millisecond { // 1.5 + 0.5 + 20 + 3
		t.Errorf("Duration = %v, want 25ms", ex.Duration)
	}
	if ex.TTFB.D() != 20500*time.Microsecond {
		t.Errorf("TTFB = %v", ex.TTFB)
	}
	if string(ex.Request.Body) != `{"qty":3}` || ex.Request.BodySize != 9 || ex.Request.BodyTruncated {
		t.Errorf("request body = %q size %d", ex.Request.Body, ex.Request.BodySize)
	}
	if ex.Response.Status != 204 || len(ex.Response.Body) != 0 || ex.Response.BodySize != 0 || ex.Response.BodyTruncated {
		t.Errorf("response = %+v", ex.Response)
	}
	if ex.Error != "" {
		t.Errorf("Error = %q", ex.Error)
	}
}

func TestToExchangeCharlesCookies(t *testing.T) {
	// No Cookie / Set-Cookie headers, only the parsed arrays: the headers are
	// rebuilt from them.
	ex := exchangeAt(t, "charles.har", 0)
	if got := ex.Request.Header.Get("Cookie"); got != "sid=abc" {
		t.Errorf("Cookie = %q", got)
	}
	sc := ex.Response.Header.Get("Set-Cookie")
	for _, want := range []string{"srv=v1", "Path=/", "Domain=example.com", "Expires=Wed, 21 Oct 2037 07:28:00 GMT", "HttpOnly", "Secure"} {
		if !strings.Contains(sc, want) {
			t.Errorf("Set-Cookie %q lacks %q", sc, want)
		}
	}
	if ex.Request.Proto != "HTTP/2.0" || ex.Response.Proto != "HTTP/2.0" {
		t.Errorf("proto = %q / %q", ex.Request.Proto, ex.Response.Proto)
	}
	if want := time.Date(2024, 3, 5, 17, 20, 31, 500_000_000, time.UTC); !ex.StartedAt.Equal(want) {
		t.Errorf("StartedAt = %v", ex.StartedAt)
	}
}

func TestToExchangeDoesNotModifyEntry(t *testing.T) {
	h := fixture(t, "chrome.har")
	before := fixture(t, "chrome.har")
	for i := range h.Log.Entries {
		_, _ = ToExchange(&h.Log.Entries[i])
	}
	if !reflect.DeepEqual(h, before) {
		t.Error("ToExchange modified its input")
	}
	// The exchange does not alias the entry's slices either.
	ex, err := ToExchange(&h.Log.Entries[0])
	if err != nil {
		t.Fatal(err)
	}
	ex.Request.Header.Set("X-Mutated", "1")
	ex.Response.Body[0] = 'X'
	if !reflect.DeepEqual(h, before) {
		t.Error("mutating the exchange changed the entry")
	}
}

func TestToExchangeRequestVariants(t *testing.T) {
	base := func() Entry {
		return Entry{Request: Request{Method: "POST", URL: "https://api.test/items?a=1"}, Response: Response{Status: 200}}
	}
	t.Run("empty method defaults to GET", func(t *testing.T) {
		e := base()
		e.Request.Method = ""
		ex := mustConvert(t, &e)
		if ex.Request.Method != "GET" {
			t.Errorf("method = %q", ex.Request.Method)
		}
	})
	t.Run("query only in queryString", func(t *testing.T) {
		e := base()
		e.Request.URL = "https://api.test/items"
		e.Request.QueryString = []NameValue{{Name: "a b", Value: "c&d"}, {Name: "e", Value: ""}}
		ex := mustConvert(t, &e)
		if ex.Request.RawQuery != "a+b=c%26d&e=" {
			t.Errorf("RawQuery = %q", ex.Request.RawQuery)
		}
	})
	t.Run("URL query wins over queryString", func(t *testing.T) {
		e := base()
		e.Request.QueryString = []NameValue{{Name: "zzz", Value: "1"}}
		ex := mustConvert(t, &e)
		if ex.Request.RawQuery != "a=1" {
			t.Errorf("RawQuery = %q", ex.Request.RawQuery)
		}
	})
	t.Run("escaped path preserved and fragment dropped", func(t *testing.T) {
		e := base()
		e.Request.URL = "https://api.test/a%2Fb/c%20d/%E2%82%AC#frag"
		ex := mustConvert(t, &e)
		if ex.Request.Path != "/a%2Fb/c%20d/%E2%82%AC" || ex.Request.RawQuery != "" {
			t.Errorf("Path = %q, RawQuery = %q", ex.Request.Path, ex.Request.RawQuery)
		}
	})
	t.Run("userinfo is dropped", func(t *testing.T) {
		e := base()
		e.Request.URL = "https://user:secret@api.test:8443/x"
		ex := mustConvert(t, &e)
		if ex.Request.Host != "api.test:8443" {
			t.Errorf("Host = %q", ex.Request.Host)
		}
	})
	t.Run("empty path becomes slash", func(t *testing.T) {
		e := base()
		e.Request.URL = "http://api.test"
		if ex := mustConvert(t, &e); ex.Request.Path != "/" {
			t.Errorf("Path = %q", ex.Request.Path)
		}
	})
	t.Run("ipv6 host", func(t *testing.T) {
		e := base()
		e.Request.URL = "http://[::1]:9000/x"
		if ex := mustConvert(t, &e); ex.Request.Host != "[::1]:9000" {
			t.Errorf("Host = %q", ex.Request.Host)
		}
	})
	t.Run("uppercase scheme", func(t *testing.T) {
		e := base()
		e.Request.URL = "HTTPS://API.TEST/x"
		if ex := mustConvert(t, &e); ex.Request.Scheme != "https" {
			t.Errorf("Scheme = %q", ex.Request.Scheme)
		}
	})
	t.Run("Host header beats URL host", func(t *testing.T) {
		e := base()
		e.Request.URL = "http://10.0.0.5/x"
		e.Request.Headers = []NameValue{{Name: "host", Value: "api.test"}}
		ex := mustConvert(t, &e)
		if ex.Request.Host != "api.test" || len(ex.Request.Header) != 0 {
			t.Errorf("Host = %q, headers %v", ex.Request.Host, ex.Request.Header)
		}
	})
	t.Run("authority used without Host", func(t *testing.T) {
		e := base()
		e.Request.URL = "http://10.0.0.5/x"
		e.Request.Headers = []NameValue{{Name: ":authority", Value: "api.test"}, {Name: ":path", Value: "/x"}}
		if ex := mustConvert(t, &e); ex.Request.Host != "api.test" || len(ex.Request.Header) != 0 {
			t.Errorf("Host = %q, headers %v", ex.Request.Host, ex.Request.Header)
		}
	})
	t.Run("header names are canonical and values kept in order", func(t *testing.T) {
		e := base()
		e.Request.Headers = []NameValue{
			{Name: "x-multi", Value: "1"}, {Name: "X-MULTI", Value: "2"}, {Name: "content-TYPE", Value: "text/plain"}, {Name: "", Value: "ignored"},
		}
		ex := mustConvert(t, &e)
		if got := ex.Request.Header["X-Multi"]; !reflect.DeepEqual(got, []string{"1", "2"}) {
			t.Errorf("X-Multi = %v", got)
		}
		if ex.Request.Header.Get("Content-Type") != "text/plain" || len(ex.Request.Header) != 2 {
			t.Errorf("headers = %v", ex.Request.Header)
		}
	})
	t.Run("Cookie header is not duplicated from cookies", func(t *testing.T) {
		e := base()
		e.Request.Headers = []NameValue{{Name: "Cookie", Value: "a=1"}}
		e.Request.Cookies = []Cookie{{Name: "a", Value: "1"}, {Name: "b", Value: "2"}}
		if ex := mustConvert(t, &e); !reflect.DeepEqual(ex.Request.Header["Cookie"], []string{"a=1"}) {
			t.Errorf("Cookie = %v", ex.Request.Header["Cookie"])
		}
	})
	t.Run("proto spellings", func(t *testing.T) {
		for in, want := range map[string]string{
			"": "", "HTTP/1.1": "HTTP/1.1", "http/1.1": "HTTP/1.1", "http/1.0": "HTTP/1.0", "h2": "HTTP/2.0", "H2": "HTTP/2.0",
			"HTTP/2": "HTTP/2.0", "http/2.0": "HTTP/2.0", "h3": "HTTP/3.0", "h3-29": "HTTP/3.0", "spdy/3.1": "spdy/3.1",
		} {
			if got := normalizeProto(in); got != want {
				t.Errorf("normalizeProto(%q) = %q, want %q", in, got, want)
			}
		}
	})
}

func TestToExchangeBodies(t *testing.T) {
	t.Run("postData text wins over params", func(t *testing.T) {
		e := Entry{Request: Request{Method: "POST", URL: "http://x.test/", PostData: &PostData{
			MimeType: "text/plain", Text: "hello", Params: []Param{{Name: "a", Value: "b"}},
		}}}
		ex := mustConvert(t, &e)
		if string(ex.Request.Body) != "hello" || ex.Request.Header.Get("Content-Type") != "text/plain" {
			t.Errorf("body %q headers %v", ex.Request.Body, ex.Request.Header)
		}
	})
	t.Run("params keep an existing Content-Type", func(t *testing.T) {
		e := Entry{Request: Request{Method: "POST", URL: "http://x.test/",
			Headers:  []NameValue{{Name: "Content-Type", Value: "application/x-www-form-urlencoded; charset=UTF-8"}},
			PostData: &PostData{MimeType: "application/x-www-form-urlencoded", Params: []Param{{Name: "a", Value: "1"}}}}}
		ex := mustConvert(t, &e)
		if ex.Request.Header.Get("Content-Type") != "application/x-www-form-urlencoded; charset=UTF-8" || string(ex.Request.Body) != "a=1" {
			t.Errorf("body %q headers %v", ex.Request.Body, ex.Request.Header)
		}
	})
	t.Run("params without any mime type", func(t *testing.T) {
		e := Entry{Request: Request{Method: "POST", URL: "http://x.test/", PostData: &PostData{Params: []Param{{Name: "a b", Value: "ü"}}}}}
		ex := mustConvert(t, &e)
		if string(ex.Request.Body) != "a+b=%C3%BC" || ex.Request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("body %q headers %v", ex.Request.Body, ex.Request.Header)
		}
	})
	t.Run("multipart params", func(t *testing.T) {
		e := Entry{Request: Request{Method: "POST", URL: "http://x.test/up", PostData: &PostData{
			MimeType: "multipart/form-data; boundary=----XyZ",
			Params: []Param{
				{Name: "title", Value: "hi"},
				{Name: "file", FileName: `we"ird.txt`, ContentType: "text/plain", Value: "line1\nline2"},
			}}}}
		ex := mustConvert(t, &e)
		body := string(ex.Request.Body)
		for _, frag := range []string{
			"------XyZ\r\nContent-Disposition: form-data; name=\"title\"\r\n\r\nhi\r\n",
			"Content-Disposition: form-data; name=\"file\"; filename=\"we\\\"ird.txt\"\r\nContent-Type: text/plain\r\n\r\nline1\nline2\r\n",
			"------XyZ--\r\n",
		} {
			if !strings.Contains(body, frag) {
				t.Errorf("multipart body lacks %q:\n%q", frag, body)
			}
		}
		if ct := ex.Request.Header.Get("Content-Type"); ct != "multipart/form-data; boundary=----XyZ" {
			t.Errorf("Content-Type = %q", ct)
		}
	})
	t.Run("multipart without boundary falls back to urlencoded", func(t *testing.T) {
		e := Entry{Request: Request{Method: "POST", URL: "http://x.test/", PostData: &PostData{
			MimeType: "multipart/form-data", Params: []Param{{Name: "a", Value: "1"}}}}}
		ex := mustConvert(t, &e)
		if string(ex.Request.Body) != "a=1" {
			t.Errorf("body = %q", ex.Request.Body)
		}
	})
	t.Run("request binary body", func(t *testing.T) {
		raw := []byte{0, 1, 2, 0xff, 0xfe}
		e := Entry{Request: Request{Method: "PUT", URL: "http://x.test/", BodySize: 5, PostData: &PostData{
			MimeType: "application/octet-stream", Text: base64.StdEncoding.EncodeToString(raw), Encoding: "base64"}}}
		ex := mustConvert(t, &e)
		if !bytes.Equal(ex.Request.Body, raw) || ex.Request.BodySize != 5 {
			t.Errorf("body = % x size %d", ex.Request.Body, ex.Request.BodySize)
		}
	})
	t.Run("truncated flag", func(t *testing.T) {
		e := Entry{
			Request:  Request{Method: "POST", URL: "http://x.test/", PostData: &PostData{Text: "abc", Truncated: true}},
			Response: Response{Status: 200, Content: Content{Text: "xyz", Size: 3, Truncated: true}},
		}
		ex := mustConvert(t, &e)
		if !ex.Request.BodyTruncated || !ex.Response.BodyTruncated {
			t.Errorf("truncated = %v / %v", ex.Request.BodyTruncated, ex.Response.BodyTruncated)
		}
	})
	t.Run("base64 variants", func(t *testing.T) {
		want := []byte{0xfb, 0xff, 0xfe, 'a'}
		for name, enc := range map[string]string{
			"std":          base64.StdEncoding.EncodeToString(want),
			"std unpadded": base64.RawStdEncoding.EncodeToString(want),
			"url":          base64.URLEncoding.EncodeToString(want),
			"url unpadded": base64.RawURLEncoding.EncodeToString(want),
			"wrapped":      base64.StdEncoding.EncodeToString(want)[:4] + "\r\n" + base64.StdEncoding.EncodeToString(want)[4:],
		} {
			got, err := decodeText(enc, "BASE64")
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("%s: decodeText(%q) = % x, %v", name, enc, got, err)
			}
		}
	})
	t.Run("bad base64 is an error", func(t *testing.T) {
		e := Entry{Request: Request{Method: "GET", URL: "http://x.test/"}, Response: Response{Status: 200, Content: Content{Text: "!!!not base64!!!", Encoding: "base64"}}}
		if _, err := ToExchange(&e); err == nil || !strings.Contains(err.Error(), "base64") {
			t.Errorf("err = %v", err)
		}
		e = Entry{Request: Request{Method: "POST", URL: "http://x.test/", PostData: &PostData{Text: "@@@", Encoding: "base64"}}}
		if _, err := ToExchange(&e); err == nil {
			t.Error("bad request base64 accepted")
		}
	})
	t.Run("encoding other than base64 is plain text", func(t *testing.T) {
		got, err := decodeText("héllo", "utf-8")
		if err != nil || string(got) != "héllo" {
			t.Errorf("got %q, %v", got, err)
		}
	})
	t.Run("size smaller than text is raised", func(t *testing.T) {
		e := Entry{Request: Request{Method: "GET", URL: "http://x.test/"}, Response: Response{Status: 200, Content: Content{Text: "twelve bytes", Size: 5}}}
		ex := mustConvert(t, &e)
		if ex.Response.BodySize != 12 || ex.Response.BodyTruncated {
			t.Errorf("BodySize = %d truncated %v", ex.Response.BodySize, ex.Response.BodyTruncated)
		}
	})
	t.Run("size without text is truncated", func(t *testing.T) {
		e := Entry{Request: Request{Method: "GET", URL: "http://x.test/"}, Response: Response{Status: 200, Content: Content{Size: 5000}}}
		ex := mustConvert(t, &e)
		if ex.Response.BodySize != 5000 || !ex.Response.BodyTruncated || len(ex.Response.Body) != 0 {
			t.Errorf("response = %+v", ex.Response)
		}
	})
	t.Run("negative size means unknown", func(t *testing.T) {
		e := Entry{Request: Request{Method: "GET", URL: "http://x.test/", BodySize: -1}, Response: Response{Status: 200, Content: Content{Text: "abc", Size: -1}}}
		ex := mustConvert(t, &e)
		if ex.Response.BodySize != 3 || ex.Response.BodyTruncated || ex.Request.BodySize != 0 || ex.Request.BodyTruncated {
			t.Errorf("exchange = %+v", ex)
		}
	})
}

func TestToExchangeContentEncoding(t *testing.T) {
	gz := func(s string) []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte(s))
		_ = zw.Close()
		return buf.Bytes()
	}
	const payload = `{"hello":"world","pad":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	mk := func(enc string, content Content) *Entry {
		return &Entry{Request: Request{Method: "GET", URL: "http://x.test/"}, Response: Response{
			Status:  200,
			Headers: []NameValue{{Name: "Content-Encoding", Value: enc}, {Name: "Content-Length", Value: "999"}},
			Content: content,
		}}
	}
	t.Run("decoded text keeps body, header moves", func(t *testing.T) {
		ex := mustConvert(t, mk("GZIP", Content{Text: payload, Size: int64len(payload)}))
		if string(ex.Response.Body) != payload || ex.Response.ContentEncoding != "gzip" || ex.Response.Header.Get("Content-Encoding") != "" {
			t.Errorf("response = %+v", ex.Response)
		}
		if ex.Response.Header.Get("Content-Length") != "999" {
			t.Errorf("Content-Length dropped: %v", ex.Response.Header)
		}
	})
	t.Run("raw gzip in base64 is inflated", func(t *testing.T) {
		raw := gz(payload)
		ex := mustConvert(t, mk("gzip", Content{Text: base64.StdEncoding.EncodeToString(raw), Encoding: "base64", Size: Size(len(raw))}))
		if string(ex.Response.Body) != payload || ex.Response.ContentEncoding != "gzip" {
			t.Errorf("body = %q enc %q", ex.Response.Body, ex.Response.ContentEncoding)
		}
	})
	t.Run("corrupt gzip is left alone", func(t *testing.T) {
		raw := gz(payload)[:15]
		ex := mustConvert(t, mk("gzip", Content{Text: base64.StdEncoding.EncodeToString(raw), Encoding: "base64"}))
		if !bytes.Equal(ex.Response.Body, raw) {
			t.Errorf("body = % x", ex.Response.Body)
		}
	})
	t.Run("other encodings are not touched", func(t *testing.T) {
		ex := mustConvert(t, mk("br", Content{Text: base64.StdEncoding.EncodeToString([]byte{0x1f, 0x8b, 1}), Encoding: "base64"}))
		if ex.Response.ContentEncoding != "br" || !bytes.Equal(ex.Response.Body, []byte{0x1f, 0x8b, 1}) {
			t.Errorf("response = %+v", ex.Response)
		}
	})
	t.Run("identity is no encoding", func(t *testing.T) {
		ex := mustConvert(t, mk("identity", Content{Text: "x"}))
		if ex.Response.ContentEncoding != "" || ex.Response.Header.Get("Content-Encoding") != "" {
			t.Errorf("response = %+v", ex.Response)
		}
	})
	t.Run("request side too", func(t *testing.T) {
		e := &Entry{Request: Request{Method: "POST", URL: "http://x.test/",
			Headers:  []NameValue{{Name: "content-encoding", Value: "gzip"}},
			PostData: &PostData{Text: payload}}}
		ex := mustConvert(t, e)
		if ex.Request.ContentEncoding != "gzip" || ex.Request.Header.Get("Content-Encoding") != "" || string(ex.Request.Body) != payload {
			t.Errorf("request = %+v", ex.Request)
		}
	})
}

func TestDurations(t *testing.T) {
	tests := []struct {
		name     string
		e        Entry
		dur, ttf time.Duration
	}{
		{"time given", Entry{Time: 100, Timings: Timings{Send: 1, Wait: 50, Receive: 10}}, 100 * time.Millisecond, 51 * time.Millisecond},
		{"time fractional", Entry{Time: 0.001234}, 1234 * time.Nanosecond, 0},
		{"time -1 sums phases", Entry{Time: -1, Timings: Timings{Blocked: 1, DNS: 2, Connect: 3, SSL: 2, Send: 4, Wait: 5, Receive: 6}}, 21 * time.Millisecond, 9 * time.Millisecond},
		{"time missing sums phases", Entry{Timings: Timings{Blocked: -1, DNS: -1, Connect: -1, SSL: -1, Send: 1, Wait: 2, Receive: 3}}, 6 * time.Millisecond, 3 * time.Millisecond},
		{"time zero and no timings", Entry{}, 0, 0},
		{"no wait no ttfb", Entry{Time: 10, Timings: Timings{Send: 1, Wait: -1, Receive: -1}}, 10 * time.Millisecond, 0},
		{"ttfb clamped to duration", Entry{Time: 5, Timings: Timings{Send: 2, Wait: 20}}, 5 * time.Millisecond, 5 * time.Millisecond},
		{"negative send ignored", Entry{Time: 30, Timings: Timings{Send: -1, Wait: 12}}, 30 * time.Millisecond, 12 * time.Millisecond},
		{"huge saturates", Entry{Time: 1e300}, time.Duration(1<<63 - 1), 0},
		{"rounding", Entry{Time: 1.2345675}, 1234568 * time.Nanosecond, 0},
	}
	for _, tc := range tests {
		dur, ttfb := durations(&tc.e)
		if dur.D() != tc.dur || ttfb.D() != tc.ttf {
			t.Errorf("%s: durations = %v, %v; want %v, %v", tc.name, dur, ttfb, tc.dur, tc.ttf)
		}
	}
}

func int64len(s string) Size { return Size(len(s)) }

func mustConvert(t *testing.T, e *Entry) *model.Exchange {
	t.Helper()
	ex, err := ToExchange(e)
	if err != nil {
		t.Fatalf("ToExchange: %v", err)
	}
	return ex
}
