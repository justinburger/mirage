package replay

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// mustLoader builds a loader for cfg against target; plain=true is the path
// without anonymization.
func mustLoader(t testing.TB, cfg model.ReplayConfig, target string, plain bool) *loader {
	t.Helper()
	cfg = cfg.WithDefaults()
	tgt, err := parseTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	l, err := newLoader(cfg, tgt, newOverlay(cfg, "job-1", tgt), plain, maxPreloadBytes)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func loadAll(t testing.TB, l *loader, exs ...rx) *prepared {
	t.Helper()
	for _, e := range exs {
		if err := l.add(e.exchange()); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	p, err := l.finish(0)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	return p
}

func TestLoaderSortsByStartedAtStablyAndRebases(t *testing.T) {
	// Appended in completion order, which is not arrival order. Ties keep
	// the order they were appended in (Seq).
	l := mustLoader(t, model.ReplayConfig{}, "http://t", true)
	p := loadAll(t, l,
		rx{at: 30 * time.Millisecond, path: "/c"},
		rx{at: 10 * time.Millisecond, path: "/a1"},
		rx{at: 10 * time.Millisecond, path: "/a2"},
		rx{at: 20 * time.Millisecond, path: "/b"},
		rx{at: 10 * time.Millisecond, path: "/a3"},
	)
	var paths []string
	var offs []time.Duration
	for _, it := range p.items {
		paths = append(paths, it.uri)
		offs = append(offs, it.off)
	}
	if want := []string{"/a1", "/a2", "/a3", "/b", "/c"}; !slices.Equal(paths, want) {
		t.Errorf("order = %v, want %v", paths, want)
	}
	if want := []time.Duration{0, 0, 0, ms(10), ms(20)}; !slices.Equal(offs, want) {
		t.Errorf("offsets = %v, want %v (relative to the earliest, not the first appended)", offs, want)
	}
	if p.span != ms(20) {
		t.Errorf("span = %v, want 20ms", p.span)
	}
}

func TestLoaderFilters(t *testing.T) {
	exs := []rx{
		{at: 0, method: "GET", path: "/users/1", query: "x=1"},
		{at: ms(1), method: "POST", path: "/users", body: "{}"},
		{at: ms(2), method: "GET", path: "/health"},
		{at: ms(3), method: "DELETE", path: "/users/1"},
		{at: ms(4), method: "GET", path: "/users/2"},
	}
	tests := []struct {
		name             string
		include, exclude []string
		want             []string
	}{
		{"none", nil, nil, []string{"/users/1?x=1", "/users", "/health", "/users/1", "/users/2"}},
		{"include method", []string{`^GET `}, nil, []string{"/users/1?x=1", "/health", "/users/2"}},
		{"include is an OR", []string{`^POST `, `^DELETE `}, nil, []string{"/users", "/users/1"}},
		{"exclude", nil, []string{`/health`}, []string{"/users/1?x=1", "/users", "/users/1", "/users/2"}},
		{"query is matched", []string{`\?x=1$`}, nil, []string{"/users/1?x=1"}},
		{"exclude wins", []string{`^GET `}, []string{`/users/`}, []string{"/health"}},
		{"anchored on METHOD path", []string{`^GET /users/\d$`}, nil, []string{"/users/2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := mustLoader(t, model.ReplayConfig{Include: tt.include, Exclude: tt.exclude}, "http://t", true)
			p := loadAll(t, l, exs...)
			var got []string
			for _, it := range p.items {
				got = append(got, it.uri)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
			if p.seen != 5 || p.skipped != int64(5-len(tt.want)) {
				t.Errorf("seen=%d skipped=%d", p.seen, p.skipped)
			}
		})
	}
}

func TestLoaderOffsetsAreRelativeToTheEarliestKeptRequest(t *testing.T) {
	l := mustLoader(t, model.ReplayConfig{Exclude: []string{`/warmup`}}, "http://t", true)
	p := loadAll(t, l, rx{at: 0, path: "/warmup"}, rx{at: time.Minute, path: "/a"}, rx{at: time.Minute + ms(5), path: "/b"})
	if p.items[0].off != 0 || p.items[1].off != ms(5) {
		t.Errorf("offsets %v %v: the replay must not wait for a filtered-out lead-in", p.items[0].off, p.items[1].off)
	}
}

func TestLoaderEmptyRecordings(t *testing.T) {
	l := mustLoader(t, model.ReplayConfig{}, "http://t", true)
	_, err := l.finish(0)
	if !errors.Is(err, ErrEmptyRecording) || !contains(err.Error(), "empty") {
		t.Errorf("empty recording: %v", err)
	}
	l = mustLoader(t, model.ReplayConfig{Include: []string{`^PUT `}}, "http://t", true)
	_ = l.add(rx{}.exchange())
	_, err = l.finish(0)
	if !errors.Is(err, ErrEmptyRecording) || !contains(err.Error(), "include/exclude") {
		t.Errorf("everything filtered out: %v", err)
	}
}

func TestLoaderRejectsBadFilter(t *testing.T) {
	cfg := model.ReplayConfig{Include: []string{`(`}}.WithDefaults()
	tgt, _ := parseTarget("http://t")
	if _, err := newLoader(cfg, tgt, overlay{}, true, 1); err == nil {
		t.Error("an invalid regex must be an error")
	}
}

func TestLoaderPreloadCap(t *testing.T) {
	cfg := model.ReplayConfig{}.WithDefaults()
	tgt, _ := parseTarget("http://t")
	l, err := newLoader(cfg, tgt, newOverlay(cfg, "x", tgt), true, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	var got error
	for i := 0; i < 100 && got == nil; i++ {
		got = l.add(rx{at: time.Duration(i), body: strings.Repeat("x", 1000)}.exchange())
	}
	if !errors.Is(got, ErrRecordingTooLarge) {
		t.Fatalf("got %v, want ErrRecordingTooLarge", got)
	}
	msg := got.Error()
	for _, want := range []string{"recording too large to preload (", " items, ", " bytes); narrow it with include/exclude"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

func TestLoaderTargetPathPrefix(t *testing.T) {
	tests := []struct {
		target, path, query, want string
	}{
		{"http://h:80", "/users/42", "x=1", "/users/42?x=1"},
		{"http://h/api/v1", "/users/42", "x=1", "/api/v1/users/42?x=1"},
		{"http://h/api/v1/", "/users/42", "", "/api/v1/users/42"},
		{"http://h/a%20b", "/c", "", "/a%20b/c"},
		{"http://h", "x", "", "/x"},
		{"http://h/p", "/", "", "/p/"},
	}
	for _, tt := range tests {
		l := mustLoader(t, model.ReplayConfig{}, tt.target, true)
		p := loadAll(t, l, rx{path: tt.path, query: tt.query})
		if p.items[0].uri != tt.want {
			t.Errorf("target %q + %q ? %q: uri %q, want %q", tt.target, tt.path, tt.query, p.items[0].uri, tt.want)
		}
	}
}

func TestLoaderEmptyPathIsRoot(t *testing.T) {
	l := mustLoader(t, model.ReplayConfig{}, "http://h/p", true)
	ex := rx{}.exchange()
	ex.Request.Path = ""
	if err := l.add(ex); err != nil {
		t.Fatal(err)
	}
	if got := l.items[0].uri; got != "/p/" {
		t.Errorf("uri = %q, want /p/", got)
	}
}

func TestSplitURIRemovesThePrefix(t *testing.T) {
	l := mustLoader(t, model.ReplayConfig{}, "http://h/api", true)
	p := loadAll(t, l, rx{path: "/users/42", query: "a=1&b=2"}, rx{path: "/plain"})
	path, q := p.items[0].splitURI(len("/api"))
	if path != "/users/42" || q != "a=1&b=2" {
		t.Errorf("split = %q, %q", path, q)
	}
	path, q = p.items[1].splitURI(len("/api"))
	if path != "/plain" || q != "" {
		t.Errorf("split = %q, %q", path, q)
	}
}

func TestLoaderHeaders(t *testing.T) {
	recorded := map[string]string{
		"Content-Type":      "application/json",
		"Authorization":     "Bearer recorded",
		"Host":              "prod.example.com",
		"Content-Length":    "999",
		"Connection":        "keep-alive, X-Hop",
		"X-Hop":             "1",
		"Keep-Alive":        "timeout=5",
		"Transfer-Encoding": "chunked",
		"Upgrade":           "h2c",
		"Te":                "trailers",
		"Expect":            "100-continue",
		"Proxy-Connection":  "keep-alive",
		"X-Keep":            "me",
		"Cookie":            "a=1",
	}
	tests := []struct {
		name string
		cfg  model.ReplayConfig
		want map[string]string // header -> value; "" = must be absent
	}{
		{"hop by hop and client owned headers are dropped", model.ReplayConfig{}, map[string]string{
			"Content-Type": "application/json", "Authorization": "Bearer recorded", "X-Keep": "me", "Cookie": "a=1",
			"Host": "", "Content-Length": "", "Connection": "", "X-Hop": "", "Keep-Alive": "", "Transfer-Encoding": "",
			"Upgrade": "", "Te": "", "Expect": "", "Proxy-Connection": "",
			"X-Mirage-Replay": "job-1",
		}},
		{"set headers override", model.ReplayConfig{SetHeaders: map[string]string{"authorization": "Bearer staging", "X-New": "n"}}, map[string]string{
			"Authorization": "Bearer staging", "X-New": "n", "X-Keep": "me",
		}},
		{"remove headers", model.ReplayConfig{RemoveHeaders: []string{"cookie", "X-KEEP"}}, map[string]string{
			"Cookie": "", "X-Keep": "", "Authorization": "Bearer recorded",
		}},
		{"remove the replay marker", model.ReplayConfig{RemoveHeaders: []string{"x-mirage-replay"}}, map[string]string{
			"X-Mirage-Replay": "",
		}},
		{"set wins over the marker", model.ReplayConfig{SetHeaders: map[string]string{"X-Mirage-Replay": "custom"}}, map[string]string{
			"X-Mirage-Replay": "custom",
		}},
		{"set host and content length are not headers", model.ReplayConfig{SetHeaders: map[string]string{"Host": "h", "Content-Length": "5"}}, map[string]string{
			"Host": "", "Content-Length": "",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := mustLoader(t, tt.cfg, "http://t", true)
			p := loadAll(t, l, rx{header: recorded})
			h := p.items[0].hdr.h
			for k, want := range tt.want {
				got, present := h[http.CanonicalHeaderKey(k)]
				switch {
				case want == "" && present:
					t.Errorf("%s = %v, want absent", k, got)
				case want != "" && (len(got) != 1 || got[0] != want):
					t.Errorf("%s = %v, want %q", k, got, want)
				}
			}
		})
	}
}

func TestHostHeaderOverride(t *testing.T) {
	tgt, _ := parseTarget("http://t")
	if o := newOverlay(model.ReplayConfig{HostHeader: "api.example.com"}, "x", tgt); o.host != "api.example.com" {
		t.Errorf("host = %q", o.host)
	}
	if o := newOverlay(model.ReplayConfig{SetHeaders: map[string]string{"host": "set.example.com"}}, "x", tgt); o.host != "set.example.com" {
		t.Errorf("SetHeaders Host should act as the host override, got %q", o.host)
	}
	if o := newOverlay(model.ReplayConfig{HostHeader: "a", SetHeaders: map[string]string{"Host": "b"}}, "x", tgt); o.host != "a" {
		t.Errorf("HostHeader must win, got %q", o.host)
	}
}

func TestTargetUserinfoBecomesBasicAuth(t *testing.T) {
	l := mustLoader(t, model.ReplayConfig{}, "http://user:pw@t", true)
	p := loadAll(t, l, rx{}, rx{at: 1, header: map[string]string{"Authorization": "Bearer x"}})
	if got := p.items[0].hdr.h.Get("Authorization"); got != "Basic dXNlcjpwdw==" {
		t.Errorf("Authorization = %q", got)
	}
	if got := p.items[1].hdr.h.Get("Authorization"); got != "Bearer x" {
		t.Errorf("a recorded Authorization must win, got %q", got)
	}
}

func TestHeaderSetsAreInterned(t *testing.T) {
	l := mustLoader(t, model.ReplayConfig{}, "http://t", true)
	h := map[string]string{"A": "1", "B": "2"}
	p := loadAll(t, l,
		rx{at: 0, header: h}, rx{at: 1, header: map[string]string{"B": "2", "A": "1"}},
		rx{at: 2, header: map[string]string{"A": "1", "B": "3"}}, rx{at: 3, header: h},
	)
	if p.items[0].hdr != p.items[1].hdr || p.items[0].hdr != p.items[3].hdr {
		t.Error("identical header sets must share one map")
	}
	if p.items[0].hdr == p.items[2].hdr {
		t.Error("different values must not share")
	}
	if l.hdrs.n != 2 {
		t.Errorf("%d distinct sets, want 2", l.hdrs.n)
	}
	if p.items[0].method != p.items[3].method || p.items[0].endpoint != p.items[1].endpoint {
		t.Error("strings should be interned")
	}
}

func TestLoaderGzipRecompression(t *testing.T) {
	plain := strings.Repeat(`{"k":"value"}`, 50)
	t.Run("plain path compresses once at load", func(t *testing.T) {
		l := mustLoader(t, model.ReplayConfig{}, "http://t", true)
		p := loadAll(t, l, rx{method: "POST", body: plain, encoding: "gzip", header: map[string]string{"Content-Type": "application/json"}})
		it := p.items[0]
		if it.hdr.h.Get("Content-Encoding") != "gzip" {
			t.Error("Content-Encoding: gzip missing")
		}
		zr, err := gzip.NewReader(bytes.NewReader(it.body))
		if err != nil {
			t.Fatalf("body is not gzip: %v", err)
		}
		got, _ := io.ReadAll(zr)
		if string(got) != plain {
			t.Error("gunzipped body differs from the recorded body")
		}
		if len(it.body) >= len(plain) {
			t.Errorf("body not compressed: %d >= %d", len(it.body), len(plain))
		}
	})
	t.Run("anonymizing path defers the compression", func(t *testing.T) {
		l := mustLoader(t, model.ReplayConfig{}, "http://t", false)
		p := loadAll(t, l, rx{method: "POST", body: plain, encoding: "gzip"})
		it := p.items[0]
		if string(it.body) != plain || it.flags&flagGzip == 0 {
			t.Error("body must stay plain with flagGzip set")
		}
		if it.hdr.h.Get("Content-Encoding") != "" {
			t.Error("the header is added when the body is compressed")
		}
	})
	t.Run("still-encoded bodies are left alone", func(t *testing.T) {
		l := mustLoader(t, model.ReplayConfig{}, "http://t", true)
		p := loadAll(t, l, rx{method: "POST", body: "BR-BYTES", encoding: "gzip", header: map[string]string{"Content-Encoding": "br"}})
		if string(p.items[0].body) != "BR-BYTES" || p.items[0].hdr.h.Get("Content-Encoding") != "br" {
			t.Error("a body that is still encoded must be sent as recorded")
		}
	})
	t.Run("no body no gzip", func(t *testing.T) {
		l := mustLoader(t, model.ReplayConfig{}, "http://t", true)
		p := loadAll(t, l, rx{encoding: "gzip"})
		if p.items[0].hdr.h.Get("Content-Encoding") != "" {
			t.Error("empty body must not get a Content-Encoding")
		}
	})
}

func TestLoaderEndpointFallsBackToPathnorm(t *testing.T) {
	l := mustLoader(t, model.ReplayConfig{}, "http://t", true)
	ex := rx{method: "get", path: "/users/42/orders/9"}.exchange()
	ex.Endpoint = ""
	if err := l.add(ex); err != nil {
		t.Fatal(err)
	}
	if got := l.items[0].endpoint; got != "GET /users/{id}/orders/{id}" {
		t.Errorf("endpoint = %q", got)
	}
}

func TestLoaderErrorFlag(t *testing.T) {
	l := mustLoader(t, model.ReplayConfig{}, "http://t", true)
	p := loadAll(t, l,
		rx{at: 0, status: 200}, rx{at: 1, status: 503}, rx{at: 2, status: 429}, rx{at: 3, status: 404},
		rx{at: 4, status: 502, errMsg: "upstream down"}, rx{at: 5, status: 200, errMsg: "weird"})
	var got []bool
	for _, it := range p.items {
		got = append(got, it.isErr())
	}
	if want := []bool{false, true, true, false, true, true}; !slices.Equal(got, want) {
		t.Errorf("isErr = %v, want %v", got, want)
	}
}

func TestTargetURLKeepsTheRecordedPathBytes(t *testing.T) {
	tgt, _ := parseTarget("https://h:8443/base")
	tests := []struct {
		uri, wantURI string
	}{
		{"/base/a/b?x=1&y=%20", "/base/a/b?x=1&y=%20"},
		{"/base/a%2Fb/c", "/base/a%2Fb/c"},
		{"/base/a!b(c)", "/base/a!b(c)"}, // valid raw, but Go would escape '!' on its own
		{"/base/caf%C3%A9", "/base/caf%C3%A9"},
		{"/base/", "/base/"},
		{"/base/x?", "/base/x"}, // an empty query is dropped by the URL type
	}
	for _, tt := range tests {
		u, err := tgt.url(tt.uri)
		if err != nil {
			t.Errorf("%q: %v", tt.uri, err)
			continue
		}
		if got := u.RequestURI(); got != tt.wantURI {
			t.Errorf("%q: request URI %q, want %q", tt.uri, got, tt.wantURI)
		}
		if u.Scheme != "https" || u.Host != "h:8443" {
			t.Errorf("%q: %s://%s", tt.uri, u.Scheme, u.Host)
		}
	}
	// A path with a broken escape falls back to the URL parser's verdict.
	if _, err := tgt.url("/base/100%"); err == nil {
		t.Error("an invalid escape must be reported")
	}
}

func BenchmarkLoaderAdd(b *testing.B) {
	cfg := model.ReplayConfig{SetHeaders: map[string]string{"Authorization": "Bearer t"}, Exclude: []string{`/health`}}.WithDefaults()
	tgt, _ := parseTarget("http://target:8080/api")
	l, _ := newLoader(cfg, tgt, newOverlay(cfg, "x", tgt), true, 1<<40)
	hdr := map[string]string{"Content-Type": "application/json", "Accept": "*/*", "User-Agent": "bench", "Cookie": "s=1"}
	exs := make([]*model.Exchange, 64)
	for i := range exs {
		exs[i] = rx{at: time.Duration(i), method: "POST", path: "/users/1234/orders", query: "a=1", body: `{"a":1,"b":"two"}`, header: hdr}.exchange()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := l.add(exs[i%len(exs)].Clone()); err != nil {
			b.Fatal(err)
		}
		if len(l.items) > 1<<16 {
			l.items = l.items[:0]
		}
	}
}
