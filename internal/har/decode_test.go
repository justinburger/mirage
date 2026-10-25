package har

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func fixture(t testing.TB, name string) *HAR {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h, err := Decode(f)
	if err != nil {
		t.Fatalf("Decode(%s): %v", name, err)
	}
	return h
}

func TestDecodeChrome(t *testing.T) {
	h := fixture(t, "chrome.har")
	if h.Log.Version != "1.2" || h.Log.Creator.Name != "WebInspector" || h.Log.Creator.Version != "537.36" {
		t.Errorf("log header = %+v", h.Log)
	}
	if h.Log.Browser != nil {
		t.Errorf("browser = %+v, want nil", h.Log.Browser)
	}
	if len(h.Log.Pages) != 1 || h.Log.Pages[0].ID != "page_1" || h.Log.Pages[0].PageTimings.OnLoad != 980.25 {
		t.Errorf("pages = %+v", h.Log.Pages)
	}
	if got := len(h.Log.Entries); got != 5 {
		t.Fatalf("entries = %d, want 5", got)
	}
	e := h.Log.Entries[0]
	if want := time.Date(2024, 3, 5, 17, 20, 31, 123_000_000, time.UTC); !e.StartedDateTime.Equal(want) {
		t.Errorf("startedDateTime = %v, want %v", e.StartedDateTime, want)
	}
	if e.Time != 187.25 || e.Pageref != "page_1" || e.ServerIPAddress != "203.0.113.7" || e.Connection != "443" {
		t.Errorf("entry = %+v", e)
	}
	if e.Timings.Blocked != 4.2 || e.Timings.DNS != -1 || e.Timings.Wait != 171.85 || e.Timings.Receive != 10.8 {
		t.Errorf("timings = %+v", e.Timings)
	}
	if got := e.Request.QueryString; len(got) != 3 || got[2].Name != "q" || got[2].Value != "a b" {
		t.Errorf("queryString = %+v", got)
	}
	if len(e.Request.Headers) != 10 || len(e.Request.Cookies) != 2 {
		t.Errorf("request headers/cookies = %d/%d", len(e.Request.Headers), len(e.Request.Cookies))
	}
	if e.Response.Content.Compression != -37 || e.Response.Content.Size != 52 {
		t.Errorf("content = %+v", e.Response.Content)
	}
	// Cookie "expires" given as null and -1 must not break decoding.
	if c := e.Response.Cookies; len(c) != 2 || !c[0].HTTPOnly || !c[0].Secure || c[0].Expires != "" || c[1].Expires != "" {
		t.Errorf("response cookies = %+v", c)
	}
	if e.Response.Error != "" || h.Log.Entries[3].Response.Error != "net::ERR_CONNECTION_REFUSED" {
		t.Errorf("_error not decoded")
	}
	if pd := h.Log.Entries[2].Request.PostData; pd == nil || len(pd.Params) != 3 || pd.Params[1].Value != "two words & more" {
		t.Errorf("postData = %+v", pd)
	}
	if got := h.Log.Entries[1].Response.Content; got.Encoding != "base64" || got.MimeType != "image/png" {
		t.Errorf("image content = %+v", got)
	}
}

func TestDecodeFirefox(t *testing.T) {
	h := fixture(t, "firefox.har")
	if h.Log.Browser == nil || h.Log.Browser.Name != "Firefox" {
		t.Errorf("browser = %+v", h.Log.Browser)
	}
	if len(h.Log.Entries) != 2 {
		t.Fatalf("entries = %d", len(h.Log.Entries))
	}
	e := h.Log.Entries[0]
	// Offsets are honoured: 18:20:31.456+01:00 is 17:20:31.456Z.
	if want := time.Date(2024, 3, 5, 17, 20, 31, 456_000_000, time.UTC); !e.StartedDateTime.Equal(want) {
		t.Errorf("startedDateTime = %v, want %v", e.StartedDateTime, want)
	}
	// Float and quoted sizes.
	if e.Request.HeadersSize != 301 || e.Response.Content.Size != 1024 || e.Response.BodySize != 1024 {
		t.Errorf("sizes = %d %d %d", e.Request.HeadersSize, e.Response.Content.Size, e.Response.BodySize)
	}
	// "+0100" without a colon.
	if want := time.Date(2024, 3, 5, 17, 20, 32, 0, time.UTC); !h.Log.Entries[1].StartedDateTime.Equal(want) {
		t.Errorf("second startedDateTime = %v, want %v", h.Log.Entries[1].StartedDateTime, want)
	}
	if h.Log.Pages[0].PageTimings.OnLoad != -1 {
		t.Errorf("onLoad = %v", h.Log.Pages[0].PageTimings.OnLoad)
	}
}

func TestDecodeCharles(t *testing.T) {
	h := fixture(t, "charles.har")
	if h.Log.Creator.Comment != "exported" {
		t.Errorf("creator = %+v", h.Log.Creator)
	}
	e := h.Log.Entries[0]
	if want := time.Date(2024, 3, 5, 17, 20, 31, 500_000_000, time.UTC); !e.StartedDateTime.Equal(want) {
		t.Errorf("startedDateTime = %v, want %v", e.StartedDateTime, want)
	}
	// Cookie expiry in HTTP date format is kept as the string it was.
	if got := e.Response.Cookies[0].Expires; got != "Wed, 21 Oct 2037 07:28:00 GMT" {
		t.Errorf("expires = %q", got)
	}
}

func TestDecodeMinimal(t *testing.T) {
	// Almost everything optional is missing.
	in := `{"log":{"entries":[{"request":{"method":"GET","url":"http://x.test/"},"response":{"status":200}}]}}`
	h, err := Decode(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Log.Entries) != 1 {
		t.Fatalf("entries = %d", len(h.Log.Entries))
	}
	ex, err := ToExchange(&h.Log.Entries[0])
	if err != nil {
		t.Fatal(err)
	}
	if ex.Request.Path != "/" || ex.Request.Host != "x.test" || ex.Response.Status != 200 || ex.Duration != 0 || !ex.StartedAt.IsZero() {
		t.Errorf("exchange = %+v", ex)
	}
}

func TestDecodeBOM(t *testing.T) {
	in := "\xef\xbb\xbf" + `{"log":{"version":"1.2","creator":{"name":"Fiddler","version":"5"},"entries":[]}}`
	h, err := Decode(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Decode with BOM: %v", err)
	}
	if h.Log.Creator.Name != "Fiddler" {
		t.Errorf("creator = %+v", h.Log.Creator)
	}
}

func TestDecodeTinyReader(t *testing.T) {
	// A reader that returns one byte at a time (and a short first read for the
	// BOM peek).
	data, err := os.ReadFile("testdata/chrome.har")
	if err != nil {
		t.Fatal(err)
	}
	h, err := Decode(iotest.OneByteReader(bytes.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Log.Entries) != 5 {
		t.Errorf("entries = %d", len(h.Log.Entries))
	}
}

func TestDecodeUnknownAndOrdering(t *testing.T) {
	// "entries" before "version", unknown keys at every level, key repeated.
	in := `{"extra":{"a":[1,2,{"b":null}]},"log":{"entries":[{"request":{"method":"GET","url":"http://x.test/a","extra":1},"response":{"status":201},"_custom":{"x":1}}],
	"version":"1.2","creator":{"name":"x","version":"1"},"_x":[1,2,3]},"trailer":true}`
	h, err := Decode(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if h.Log.Version != "1.2" || len(h.Log.Entries) != 1 || h.Log.Entries[0].Response.Status != 201 {
		t.Errorf("decoded = %+v", h.Log)
	}
}

func TestDecodeErrors(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantIs  error
		wantMsg string
	}{
		{"empty", ``, io.ErrUnexpectedEOF, ""},
		{"whitespace", "  \n", io.ErrUnexpectedEOF, ""},
		{"not json", `hello`, nil, "har:"},
		{"array", `[]`, nil, "expected"},
		{"no log", `{"foo":1}`, ErrNotHAR, ""},
		{"null log", `{"log":null}`, ErrNotHAR, ""},
		{"log is string", `{"log":"x"}`, nil, "must be an object"},
		{"entries is object", `{"log":{"entries":{}}}`, nil, "log.entries"},
		{"truncated", `{"log":{"version":"1.2","entries":[{"request":{"method":"GET"`, nil, ""},
		{"truncated after entries", `{"log":{"entries":[`, nil, "unexpected end"},
		{"bad entry reports index", `{"log":{"entries":[{"request":{"method":"GET","url":"http://x/"}},{"startedDateTime":"yesterday"}]}}`, nil, "entry 1"},
		{"bad size", `{"log":{"entries":[{"request":{"headersSize":"big"}}]}}`, nil, "invalid size"},
		{"startedDateTime not string", `{"log":{"entries":[{"startedDateTime":12345}]}}`, nil, "expected a string"},
		{"startedDateTime garbage", `{"log":{"entries":[{"startedDateTime":"2024-13-45T99:00:00Z"}]}}`, nil, "invalid timestamp"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := Decode(strings.NewReader(tc.in))
			if err == nil {
				t.Fatalf("Decode succeeded: %+v", h)
			}
			if h != nil {
				t.Errorf("Decode returned a document with an error")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("error %v is not %v", err, tc.wantIs)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not contain %q", err, tc.wantMsg)
			}
		})
	}
}

func TestDecodeReaderError(t *testing.T) {
	boom := errors.New("boom")
	r := io.MultiReader(strings.NewReader(`{"log":{"entries":[`), iotest.ErrReader(boom))
	if _, err := Decode(r); !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap %v", err, boom)
	}
}

func TestDecodeEntriesNull(t *testing.T) {
	lg, err := DecodeEntries(strings.NewReader(`{"log":{"version":"1.2","entries":null}}`), func(int, *Entry) error {
		t.Error("callback called")
		return nil
	})
	if err != nil || lg.Version != "1.2" || lg.Entries != nil {
		t.Errorf("lg = %+v, err = %v", lg, err)
	}
	h, err := Decode(strings.NewReader(`{"log":{"entries":[]}}`))
	if err != nil || len(h.Log.Entries) != 0 {
		t.Errorf("empty entries: %+v %v", h, err)
	}
}

func TestDecodeEntriesStreaming(t *testing.T) {
	data, err := os.ReadFile("testdata/chrome.har")
	if err != nil {
		t.Fatal(err)
	}
	var urls []string
	lg, err := DecodeEntries(bytes.NewReader(data), func(i int, e *Entry) error {
		if i != len(urls) {
			t.Errorf("index %d, want %d", i, len(urls))
		}
		urls = append(urls, e.Request.URL)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 5 || lg.Entries != nil || lg.Creator.Name != "WebInspector" || len(lg.Pages) != 1 {
		t.Errorf("urls = %d, log = %+v", len(urls), lg)
	}

	// An error from the callback stops the walk and is wrapped.
	stop := errors.New("stop here")
	calls := 0
	_, err = DecodeEntries(bytes.NewReader(data), func(i int, _ *Entry) error {
		calls++
		if i == 1 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || calls != 2 || !strings.Contains(err.Error(), "entry 1") {
		t.Errorf("err = %v after %d calls", err, calls)
	}
}

func TestDecodeEntriesDoesNotRetain(t *testing.T) {
	// The callback entry is reused by value copies only: a fresh Entry per
	// callback means retaining the pointer is safe, but Decode must hold
	// independent copies.
	h, err := Decode(strings.NewReader(`{"log":{"entries":[
		{"request":{"method":"GET","url":"http://a.test/1"},"response":{"status":1}},
		{"request":{"method":"GET","url":"http://a.test/2"},"response":{"status":2}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if h.Log.Entries[0].Request.URL != "http://a.test/1" || h.Log.Entries[1].Request.URL != "http://a.test/2" {
		t.Errorf("entries aliased: %+v", h.Log.Entries)
	}
}

func TestTimestampParse(t *testing.T) {
	utc := func(y int, mo time.Month, d, h, mi, s, ns int) time.Time {
		return time.Date(y, mo, d, h, mi, s, ns, time.UTC)
	}
	tests := []struct {
		in   string
		want time.Time
	}{
		{"2024-03-05T17:20:31Z", utc(2024, 3, 5, 17, 20, 31, 0)},
		{"2024-03-05T17:20:31.123Z", utc(2024, 3, 5, 17, 20, 31, 123_000_000)},
		{"2024-03-05T17:20:31.1Z", utc(2024, 3, 5, 17, 20, 31, 100_000_000)},
		{"2024-03-05T17:20:31.123456Z", utc(2024, 3, 5, 17, 20, 31, 123_456_000)},
		{"2024-03-05T17:20:31.1234567Z", utc(2024, 3, 5, 17, 20, 31, 123_456_700)},
		{"2024-03-05T17:20:31.123456789Z", utc(2024, 3, 5, 17, 20, 31, 123_456_789)},
		{"2024-03-05T18:20:31.123+01:00", utc(2024, 3, 5, 17, 20, 31, 123_000_000)},
		{"2024-03-05T09:20:31.123-08:00", utc(2024, 3, 5, 17, 20, 31, 123_000_000)},
		{"2024-03-05T23:50:31+05:30", utc(2024, 3, 5, 18, 20, 31, 0)},
		{"2024-03-05T18:20:31.123+0100", utc(2024, 3, 5, 17, 20, 31, 123_000_000)},
		{"2024-03-05T18:20:31+0100", utc(2024, 3, 5, 17, 20, 31, 0)},
		{"2024-03-05T18:20:31+01", utc(2024, 3, 5, 17, 20, 31, 0)},
		{"2024-03-05T17:20:31.5+00:00", utc(2024, 3, 5, 17, 20, 31, 500_000_000)},
		{"2024-03-05 17:20:31Z", utc(2024, 3, 5, 17, 20, 31, 0)},
		{"2024-03-05 18:20:31.250+01:00", utc(2024, 3, 5, 17, 20, 31, 250_000_000)},
		{"2024-03-05T17:20:31", utc(2024, 3, 5, 17, 20, 31, 0)},
		{"2024-03-05T17:20:31.75", utc(2024, 3, 5, 17, 20, 31, 750_000_000)},
		{"2024-03-05 17:20:31", utc(2024, 3, 5, 17, 20, 31, 0)},
		{"  2024-03-05T17:20:31Z  ", utc(2024, 3, 5, 17, 20, 31, 0)},
		{"Wed, 21 Oct 2037 07:28:00 GMT", utc(2037, 10, 21, 7, 28, 0, 0)},
		{"2024-02-29T00:00:00Z", utc(2024, 2, 29, 0, 0, 0, 0)},
		{"", time.Time{}},
	}
	for _, tc := range tests {
		got, err := parseTimestamp(tc.in)
		if err != nil {
			t.Errorf("parseTimestamp(%q): %v", tc.in, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("parseTimestamp(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"yesterday", "2024-03-05", "2024-02-30T00:00:00Z", "17:20:31", "2024-03-05T25:00:00Z", "1709659231"} {
		if got, err := parseTimestamp(bad); err == nil {
			t.Errorf("parseTimestamp(%q) = %v, want error", bad, got)
		}
	}
}

func TestTimestampJSON(t *testing.T) {
	ts := Timestamp{Time: time.Date(2024, 3, 5, 18, 20, 31, 123_000_000, time.FixedZone("x", 3600))}
	b, err := json.Marshal(ts)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"2024-03-05T17:20:31.123Z"` {
		t.Errorf("Marshal = %s", b)
	}
	b, _ = json.Marshal(Timestamp{Time: time.Date(2024, 3, 5, 17, 20, 31, 0, time.UTC)})
	if string(b) != `"2024-03-05T17:20:31Z"` {
		t.Errorf("Marshal whole second = %s", b)
	}
	nano := Timestamp{Time: time.Date(2024, 3, 5, 17, 20, 31, 123_456_789, time.UTC)}
	b, _ = json.Marshal(nano)
	var back Timestamp
	if err := json.Unmarshal(b, &back); err != nil || !back.Equal(nano.Time) {
		t.Errorf("nanosecond round trip: %s -> %v, %v", b, back, err)
	}
	for _, in := range []string{`null`, `""`, `"   "`} {
		var z Timestamp
		z.Time = time.Now()
		_ = json.Unmarshal([]byte(in), &z)
		if in == `null` {
			continue // null leaves the value alone, as encoding/json does
		}
		if !z.IsZero() {
			t.Errorf("Unmarshal(%s) = %v, want zero", in, z)
		}
	}
	// Zero marshals to something Chrome can read.
	b, _ = json.Marshal(Timestamp{})
	if string(b) != `"0001-01-01T00:00:00Z"` {
		t.Errorf("zero = %s", b)
	}
}

func TestSizeUnmarshal(t *testing.T) {
	tests := []struct {
		in      string
		want    Size
		wantErr bool
	}{
		{`123`, 123, false},
		{`0`, 0, false},
		{`-1`, -1, false},
		{`123.0`, 123, false},
		{`123.4`, 123, false},
		{`123.5`, 124, false},
		{`1e3`, 1000, false},
		{`"456"`, 456, false},
		{`"456.0"`, 456, false},
		{`" 7 "`, 7, false},
		{`""`, 0, false},
		{`null`, 0, false},
		{`9007199254740993`, 9007199254740993, false},
		{`1e30`, 0, true},
		{`"abc"`, 0, true},
		{`true`, 0, true},
		{`[1]`, 0, true},
		{`{}`, 0, true},
	}
	for _, tc := range tests {
		var got Size
		err := json.Unmarshal([]byte(tc.in), &got)
		if (err != nil) != tc.wantErr {
			t.Errorf("Unmarshal(%s) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("Unmarshal(%s) = %d, want %d", tc.in, got, tc.want)
		}
	}
	if b, _ := json.Marshal(Size(42)); string(b) != "42" {
		t.Errorf("Marshal(Size(42)) = %s", b)
	}
}

func TestCookieUnmarshal(t *testing.T) {
	tests := []struct {
		in      string
		want    Cookie
		wantErr bool
	}{
		{`{"name":"a","value":"b"}`, Cookie{Name: "a", Value: "b"}, false},
		{`{"name":"a","value":"b","expires":"2030-01-01T00:00:00Z","httpOnly":true}`, Cookie{Name: "a", Value: "b", Expires: "2030-01-01T00:00:00Z", HTTPOnly: true}, false},
		{`{"name":"a","value":"b","expires":null}`, Cookie{Name: "a", Value: "b"}, false},
		{`{"name":"a","value":"b","expires":-1}`, Cookie{Name: "a", Value: "b"}, false},
		{`{"name":"a","value":"b","expires":1893456000}`, Cookie{Name: "a", Value: "b"}, false},
		{`{"name":"a","value":"b","path":"/","domain":"x.test","secure":true,"comment":"c"}`, Cookie{Name: "a", Value: "b", Path: "/", Domain: "x.test", Secure: true, Comment: "c"}, false},
		{`{"name":1}`, Cookie{}, true},
		{`"x"`, Cookie{}, true},
	}
	for _, tc := range tests {
		var got Cookie
		err := json.Unmarshal([]byte(tc.in), &got)
		if (err != nil) != tc.wantErr {
			t.Errorf("Unmarshal(%s) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("Unmarshal(%s) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}
