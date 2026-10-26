package virtual

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// fixedNow pins the engine's clock.
func fixedNow(e *Engine, t time.Time) { e.now = func() time.Time { return t } }

func TestRespondHeaders(t *testing.T) {
	ex := mkEx(1, "GET", "/h", 201, `{"ok":true}`,
		withRespHeader("Content-Length", "999"),
		withRespHeader("Date", "Mon, 01 Jan 2001 00:00:00 GMT"),
		withRespHeader("Connection", "close, X-Secret"),
		withRespHeader("Keep-Alive", "timeout=5"),
		withRespHeader("Proxy-Authenticate", "Basic"),
		withRespHeader("Proxy-Connection", "keep-alive"),
		withRespHeader("Te", "trailers"),
		withRespHeader("Trailer", "Expires"),
		withRespHeader("Transfer-Encoding", "chunked"),
		withRespHeader("Upgrade", "h2c"),
		withRespHeader("X-Secret", "named by Connection"),
		withRespHeader("Set-Cookie", "a=1; Path=/", "b=2; Path=/"),
		withRespHeader("X-Custom", "v1", "v2"),
		withRespHeader("etag", `"abc"`), // non-canonical key
	)
	e := newTestEngine(t, Options{}, ex)
	fixedNow(e, time.Date(2026, 7, 4, 12, 30, 15, 0, time.UTC))

	rec, ok := serve(e, mkReq("GET", "/h"), "")
	if !ok {
		t.Fatal("miss")
	}
	h := rec.Header()
	if rec.Code != 201 || bodyOf(rec) != `{"ok":true}` {
		t.Fatalf("code=%d body=%q", rec.Code, bodyOf(rec))
	}
	for _, k := range []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Connection", "Te",
		"Trailer", "Transfer-Encoding", "Upgrade", "X-Secret", "Content-Encoding"} {
		if _, ok := h[k]; ok {
			t.Errorf("header %s should not be sent: %v", k, h[k])
		}
	}
	want := map[string][]string{
		"Content-Type":   {"application/json"},
		"Content-Length": {"11"},
		"Date":           {"Sat, 04 Jul 2026 12:30:15 GMT"},
		"Set-Cookie":     {"a=1; Path=/", "b=2; Path=/"},
		"X-Custom":       {"v1", "v2"},
		"Etag":           {`"abc"`},
		"X-Mirage-Match": {"exact"},
	}
	for k, v := range want {
		if !reflect.DeepEqual(h[k], v) {
			t.Errorf("header %s = %v, want %v", k, h[k], v)
		}
	}
	if _, ok := h[HeaderInjected]; ok {
		t.Error("X-Mirage-Injected set on a normal response")
	}
}

func TestRespondMatchHeaderPerLevel(t *testing.T) {
	e := newTestEngine(t, Options{Match: model.MatchConfig{PathTemplates: true}}, mkEx(1, "GET", "/u/1?a=1", 200, "x"))
	for target, want := range map[string]string{"/u/1?a=1": "exact", "/u/1?a=2": "path", "/u/2": "template"} {
		rec, ok := serve(e, mkReq("GET", target), "")
		if !ok || rec.Header().Get(HeaderMatch) != want {
			t.Errorf("%s: ok=%v X-Mirage-Match=%q, want %q", target, ok, rec.Header().Get(HeaderMatch), want)
		}
	}
}

// Nothing a writer does to the response headers or body may reach the index.
func TestServedDataIsIsolatedFromTheIndex(t *testing.T) {
	ex := mkEx(1, "GET", "/iso", 200, "original-body",
		withRespHeader("X-Multi", "a", "b"), withRespHeader("Set-Cookie", "c1", "c2"))
	wantHdr := ex.Response.Header.Clone()
	wantBody := string(ex.Response.Body)
	e := newTestEngine(t, Options{}, ex)

	for range 3 {
		w := &mutatingWriter{h: http.Header{}}
		if !e.Serve(w, mkReq("GET", "/iso"), nil) {
			t.Fatal("miss")
		}
		if !w.mutated {
			t.Fatal("test writer did not run")
		}
	}
	if !reflect.DeepEqual(ex.Response.Header, wantHdr) {
		t.Fatalf("recorded headers were modified:\n got %v\nwant %v", ex.Response.Header, wantHdr)
	}
	if string(ex.Response.Body) != wantBody {
		t.Fatalf("recorded body was modified: %q", ex.Response.Body)
	}
	rec, _ := serve(e, mkReq("GET", "/iso"), "")
	if bodyOf(rec) != wantBody || !reflect.DeepEqual(rec.Header()["X-Multi"], []string{"a", "b"}) {
		t.Fatalf("later response differs: %q %v", bodyOf(rec), rec.Header()["X-Multi"])
	}
}

// mutatingWriter scribbles on the response headers the way a careless
// middleware might: values in place, appends, Add. (Write must not modify its
// argument by the io.Writer contract, so the body buffer is passed on
// uncopied.)
type mutatingWriter struct {
	h       http.Header
	mutated bool
}

func (w *mutatingWriter) Header() http.Header { return w.h }
func (w *mutatingWriter) WriteHeader(int) {
	for k, v := range w.h {
		for i := range v {
			v[i] = "MUTATED"
		}
		w.h[k] = append(v, "APPENDED")
		w.h.Add(k, "ADDED")
	}
	w.mutated = true
}
func (w *mutatingWriter) Write(b []byte) (int, error) { return len(b), nil }

func TestHeadRequest(t *testing.T) {
	e := newTestEngine(t, Options{}, mkEx(1, "GET", "/h", 200, "twelve bytes"))
	rec, ok := serve(e, mkReq("HEAD", "/h"), "")
	if !ok || rec.Code != 200 {
		t.Fatalf("ok=%v code=%d", ok, rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD wrote a body: %q", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Length"); got != "12" {
		t.Fatalf("Content-Length = %q, want the GET length 12", got)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", rec.Header())
	}
}

func TestHeadRequestGzipLengthMatchesGet(t *testing.T) {
	e := newTestEngine(t, Options{}, mkEx(1, "GET", "/g", 200, strings.Repeat("hello ", 200), withGzipT()))
	get, _ := serve(e, mkReq("GET", "/g", "Accept-Encoding", "gzip"), "")
	head, _ := serve(e, mkReq("HEAD", "/g", "Accept-Encoding", "gzip"), "")
	if head.Header().Get("Content-Length") != get.Header().Get("Content-Length") || head.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("HEAD headers %v differ from GET %v", head.Header(), get.Header())
	}
	if head.Body.Len() != 0 {
		t.Fatal("HEAD wrote a body")
	}
}

func TestBodylessStatuses(t *testing.T) {
	e := newTestEngine(t, Options{},
		mkEx(1, "GET", "/204", 204, ""),
		mkEx(2, "GET", "/304", 304, "should-not-be-sent"),
	)
	for _, p := range []string{"/204", "/304"} {
		rec, _ := serve(e, mkReq("GET", p), "")
		if rec.Body.Len() != 0 {
			t.Errorf("%s wrote a body", p)
		}
		if _, ok := rec.Header()["Content-Length"]; ok {
			t.Errorf("%s has a Content-Length", p)
		}
	}
}

func TestInvalidRecordedStatusBecomes502(t *testing.T) {
	for _, st := range []int{0, 99, 100, 1000} {
		e := newTestEngine(t, Options{}, mkEx(1, "GET", "/s", st, "x"))
		rec, ok := serve(e, mkReq("GET", "/s"), "")
		if !ok || rec.Code != 502 {
			t.Errorf("status %d: ok=%v code=%d, want 502", st, ok, rec.Code)
		}
	}
}

func TestBinaryBodyRoundTrip(t *testing.T) {
	body := make([]byte, 5000)
	for i := range body {
		body[i] = byte(i * 7)
	}
	e := newTestEngine(t, Options{}, mkEx(1, "GET", "/bin", 200, string(body), withRespHeader("Content-Type", "application/octet-stream")))
	rec, _ := serve(e, mkReq("GET", "/bin"), "")
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatal("binary body changed")
	}
	if rec.Header().Get("Content-Length") != "5000" {
		t.Fatalf("Content-Length = %s", rec.Header().Get("Content-Length"))
	}
}

func TestRespondHandBuiltMatchAndNil(t *testing.T) {
	e := newTestEngine(t, Options{})
	e.Respond(httptest.NewRecorder(), mkReq("GET", "/"), nil) // must not panic
	e.Respond(httptest.NewRecorder(), mkReq("GET", "/"), &Match{})
	rec := httptest.NewRecorder()
	e.Respond(rec, mkReq("GET", "/"), &Match{Exchange: mkEx(1, "GET", "/", 202, "hand"), Level: LevelPath})
	if rec.Code != 202 || bodyOf(rec) != "hand" || rec.Header().Get(HeaderMatch) != "path" {
		t.Fatalf("hand-built match: %d %q %v", rec.Code, bodyOf(rec), rec.Header())
	}
}

func TestResponseWithoutFlusher(t *testing.T) {
	e := newTestEngine(t, Options{Latency: model.LatencyConfig{Mode: model.LatencyRecorded}},
		mkEx(1, "GET", "/nf", 200, "x", withLatency(time.Millisecond, 3*time.Millisecond)))
	(&fakeClock{}).install(e)
	rec := httptest.NewRecorder()
	// Hide the recorder's Flush method.
	w := struct{ http.ResponseWriter }{rec}
	if !e.Serve(w, mkReq("GET", "/nf"), nil) || bodyOf(rec) != "x" {
		t.Fatalf("code=%d body=%q", rec.Code, bodyOf(rec))
	}
}

// ---- gzip ----------------------------------------------------------------

func withGzipT() exOpt { return withGzip() }

func gunzip(t testing.TB, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestGzipReEncoding(t *testing.T) {
	plain := string(bytes.Repeat([]byte(`{"k":"value"},`), 100))
	tests := []struct {
		name       string
		recorded   []exOpt
		acceptEnc  string
		wantGzip   bool
		wantHeader string
	}{
		{"accepts gzip", []exOpt{withGzip()}, "gzip", true, ""},
		{"accepts among others", []exOpt{withGzip()}, "br, gzip;q=0.8, deflate", true, ""},
		{"star", []exOpt{withGzip()}, "*", true, ""},
		{"uppercase", []exOpt{withGzip()}, "GZIP", true, ""},
		{"gzip refused with q=0", []exOpt{withGzip()}, "gzip;q=0", false, ""},
		{"gzip refused beats star", []exOpt{withGzip()}, "gzip;q=0, *", false, ""},
		{"star refused", []exOpt{withGzip()}, "*;q=0", false, ""},
		{"only identity", []exOpt{withGzip()}, "identity", false, ""},
		{"no header", []exOpt{withGzip()}, "", false, ""},
		{"q with spaces", []exOpt{withGzip()}, "gzip ; q=0.5", true, ""},
		{"recorded was not gzip", nil, "gzip", false, ""},
		{"recorded as deflate is not re-encoded", []exOpt{func(e *model.Exchange) { e.Response.ContentEncoding = "deflate" }}, "gzip", false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t, Options{}, mkEx(1, "GET", "/g", 200, plain, tc.recorded...))
			var hdr []string
			if tc.acceptEnc != "" {
				hdr = []string{"Accept-Encoding", tc.acceptEnc}
			}
			rec, ok := serve(e, mkReq("GET", "/g", hdr...), "")
			if !ok {
				t.Fatal("miss")
			}
			ce := rec.Header().Get("Content-Encoding")
			if tc.wantGzip {
				if ce != "gzip" {
					t.Fatalf("Content-Encoding = %q, want gzip", ce)
				}
				if got := gunzip(t, rec.Body.Bytes()); got != plain {
					t.Fatalf("gunzipped body differs")
				}
				if rec.Header().Get("Content-Length") != strconv.Itoa(rec.Body.Len()) {
					t.Fatalf("Content-Length %s != compressed size %d", rec.Header().Get("Content-Length"), rec.Body.Len())
				}
				if rec.Body.Len() >= len(plain) {
					t.Fatalf("compressed body (%d) is not smaller than the plain one (%d)", rec.Body.Len(), len(plain))
				}
			} else {
				if ce != "" {
					t.Fatalf("Content-Encoding = %q, want none", ce)
				}
				if bodyOf(rec) != plain || rec.Header().Get("Content-Length") != strconv.Itoa(len(plain)) {
					t.Fatalf("plain body expected, got %d bytes", rec.Body.Len())
				}
			}
		})
	}
}

func TestGzipCompressedOncePerExchange(t *testing.T) {
	plain := string(bytes.Repeat([]byte("abc"), 1000))
	e := newTestEngine(t, Options{}, mkEx(1, "GET", "/g", 200, plain, withGzip()), mkEx(2, "GET", "/g2", 200, plain, withGzip()))
	done := make(chan []byte, 64)
	for range 64 {
		go func() {
			rec, _ := serve(e, mkReq("GET", "/g", "Accept-Encoding", "gzip"), "")
			done <- rec.Body.Bytes()
		}()
	}
	first := <-done
	for range 63 {
		if !bytes.Equal(<-done, first) {
			t.Fatal("responses differ")
		}
	}
	if n := e.gzips.Load(); n != 1 {
		t.Fatalf("compressed %d times, want 1", n)
	}
	serve(e, mkReq("GET", "/g2", "Accept-Encoding", "gzip"), "")
	if n := e.gzips.Load(); n != 2 {
		t.Fatalf("second exchange: compressed %d times in total, want 2", n)
	}
	// Plain requests never compress.
	serve(e, mkReq("GET", "/g2"), "")
	if n := e.gzips.Load(); n != 2 {
		t.Fatalf("plain request triggered compression (%d)", n)
	}
}

func TestGzipEmptyBodyIsNotEncoded(t *testing.T) {
	e := newTestEngine(t, Options{}, mkEx(1, "GET", "/g", 200, "", withGzip()))
	rec, _ := serve(e, mkReq("GET", "/g", "Accept-Encoding", "gzip"), "")
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.Len() != 0 {
		t.Fatalf("empty body encoded: %v", rec.Header())
	}
}

func TestRecordedEncodingHeaders(t *testing.T) {
	// A body still encoded as recorded (ContentEncoding empty) keeps its header.
	e := newTestEngine(t, Options{}, mkEx(1, "GET", "/br", 200, "brotli-bytes", withRespHeader("Content-Encoding", "br")))
	rec, _ := serve(e, mkReq("GET", "/br"), "")
	if rec.Header().Get("Content-Encoding") != "br" || bodyOf(rec) != "brotli-bytes" {
		t.Fatalf("pass-through encoding lost: %v", rec.Header())
	}
	// A decoded body must not claim an encoding even if the header lingers.
	e2 := newTestEngine(t, Options{}, mkEx(1, "GET", "/d", 200, "plain",
		withGzip(), withRespHeader("Content-Encoding", "gzip")))
	rec, _ = serve(e2, mkReq("GET", "/d"), "")
	if rec.Header().Get("Content-Encoding") != "" || bodyOf(rec) != "plain" {
		t.Fatalf("stale Content-Encoding sent: %v", rec.Header())
	}
}

func TestGzipDeterministic(t *testing.T) {
	e := NewEngine(nil, Options{})
	a, b := e.compress([]byte("same input")), e.compress([]byte("same input"))
	if !bytes.Equal(a, b) {
		t.Fatal("gzip output is not deterministic")
	}
}

// ---- latency -------------------------------------------------------------

func TestLatencyPlan(t *testing.T) {
	ms := time.Millisecond
	tests := []struct {
		name        string
		lat         model.LatencyConfig
		ttfb, dur   time.Duration
		first, rest time.Duration
	}{
		{"none", model.LatencyConfig{Mode: model.LatencyNone}, 10 * ms, 50 * ms, 0, 0},
		{"recorded", model.LatencyConfig{Mode: model.LatencyRecorded, Multiplier: 1}, 10 * ms, 50 * ms, 10 * ms, 40 * ms},
		{"recorded default multiplier", model.LatencyConfig{Mode: model.LatencyRecorded}, 10 * ms, 50 * ms, 10 * ms, 40 * ms},
		{"recorded x2", model.LatencyConfig{Mode: model.LatencyRecorded, Multiplier: 2}, 10 * ms, 50 * ms, 20 * ms, 80 * ms},
		{"recorded x0.5", model.LatencyConfig{Mode: model.LatencyRecorded, Multiplier: 0.5}, 10 * ms, 50 * ms, 5 * ms, 20 * ms},
		{"recorded ttfb equals duration", model.LatencyConfig{Mode: model.LatencyRecorded}, 30 * ms, 30 * ms, 30 * ms, 0},
		{"recorded ttfb beyond duration is clamped", model.LatencyConfig{Mode: model.LatencyRecorded}, 40 * ms, 30 * ms, 40 * ms, 0},
		{"recorded zero", model.LatencyConfig{Mode: model.LatencyRecorded}, 0, 0, 0, 0},
		{"recorded no ttfb", model.LatencyConfig{Mode: model.LatencyRecorded}, 0, 25 * ms, 0, 25 * ms},
		{"fixed", model.LatencyConfig{Mode: model.LatencyFixed, Fixed: model.Duration(70 * ms)}, 10 * ms, 50 * ms, 70 * ms, 0},
		{"fixed ignores multiplier", model.LatencyConfig{Mode: model.LatencyFixed, Fixed: model.Duration(70 * ms), Multiplier: 3}, 0, 0, 70 * ms, 0},
		{"fixed zero", model.LatencyConfig{Mode: model.LatencyFixed}, 10 * ms, 50 * ms, 0, 0},
		{"unknown mode is none", model.LatencyConfig{Mode: "bogus"}, 10 * ms, 50 * ms, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEngine(nil, Options{Latency: tc.lat})
			first, rest := e.latency(tc.ttfb, tc.dur)
			if first != tc.first || rest != tc.rest {
				t.Fatalf("got (%v, %v), want (%v, %v)", first, rest, tc.first, tc.rest)
			}
		})
	}
}

func TestLatencyJitter(t *testing.T) {
	ms := time.Millisecond
	t.Run("fixed stays in range and averages out", func(t *testing.T) {
		e := NewEngine(nil, Options{Seed: 7, Latency: model.LatencyConfig{
			Mode: model.LatencyFixed, Fixed: model.Duration(100 * ms), Jitter: model.Duration(30 * ms)}})
		var sum, lo, hi time.Duration = 0, time.Hour, 0
		const n = 4000
		for range n {
			d, rest := e.latency(0, 0)
			if rest != 0 || d < 70*ms || d > 130*ms {
				t.Fatalf("latency %v out of [70ms,130ms]", d)
			}
			sum += d
			lo, hi = min(lo, d), max(hi, d)
		}
		if mean := sum / n; mean < 97*ms || mean > 103*ms {
			t.Fatalf("mean %v, want about 100ms", mean)
		}
		if lo > 75*ms || hi < 125*ms {
			t.Fatalf("jitter does not cover the range: [%v, %v]", lo, hi)
		}
	})
	t.Run("never negative", func(t *testing.T) {
		e := NewEngine(nil, Options{Seed: 9, Latency: model.LatencyConfig{
			Mode: model.LatencyFixed, Fixed: model.Duration(10 * ms), Jitter: model.Duration(50 * ms)}})
		zero := 0
		const n = 4000
		for range n {
			d, _ := e.latency(0, 0)
			if d < 0 || d > 60*ms {
				t.Fatalf("latency %v out of [0,60ms]", d)
			}
			if d == 0 {
				zero++
			}
		}
		// P(offset <= -10ms) = 41/101.
		if frac := float64(zero) / n; frac < 0.35 || frac > 0.46 {
			t.Fatalf("zero-latency fraction %.3f, want about 0.41", frac)
		}
	})
	t.Run("recorded jitter applies to the total and keeps ttfb within it", func(t *testing.T) {
		e := NewEngine(nil, Options{Seed: 3, Latency: model.LatencyConfig{
			Mode: model.LatencyRecorded, Multiplier: 1, Jitter: model.Duration(20 * ms)}})
		for range 2000 {
			first, rest := e.latency(10*ms, 50*ms)
			total := first + rest
			if total < 30*ms || total > 70*ms || first > 10*ms || first < 0 || rest < 0 {
				t.Fatalf("first=%v rest=%v", first, rest)
			}
		}
		// A small total can shrink below the recorded TTFB: ttfb is capped.
		sawCap := false
		for range 2000 {
			first, rest := e.latency(10*ms, 12*ms)
			if first+rest < 10*ms && first == first+rest {
				sawCap = true
			}
			if first < 0 || rest < 0 {
				t.Fatalf("negative: %v %v", first, rest)
			}
		}
		if !sawCap {
			t.Fatal("expected some totals below the TTFB")
		}
	})
}

func TestRespondLatencyOrdering(t *testing.T) {
	ms := time.Millisecond
	t.Run("recorded: wait ttfb, headers, flush, wait rest, body", func(t *testing.T) {
		e := newTestEngine(t, Options{Latency: model.LatencyConfig{Mode: model.LatencyRecorded, Multiplier: 2}},
			mkEx(1, "GET", "/l", 200, "body", withLatency(10*ms, 50*ms)))
		log := &eventLog{}
		(&fakeClock{log: log}).install(e)
		w := newLogWriter(log)
		if !e.Serve(w, mkReq("GET", "/l"), nil) {
			t.Fatal("miss")
		}
		want := "wait 20ms,header,flush,wait 80ms,body"
		if log.String() != want {
			t.Fatalf("events = %s\nwant     %s", log, want)
		}
		if w.body.String() != "body" || w.code != 200 {
			t.Fatalf("code=%d body=%q", w.code, w.body.String())
		}
	})
	t.Run("recorded without a gap: no flush", func(t *testing.T) {
		e := newTestEngine(t, Options{Latency: model.LatencyConfig{Mode: model.LatencyRecorded}},
			mkEx(1, "GET", "/l", 200, "body", withLatency(10*ms, 10*ms)))
		log := &eventLog{}
		(&fakeClock{log: log}).install(e)
		e.Serve(newLogWriter(log), mkReq("GET", "/l"), nil)
		if want := "wait 10ms,header,body"; log.String() != want {
			t.Fatalf("events = %s, want %s", log, want)
		}
	})
	t.Run("recorded zero ttfb: headers at once", func(t *testing.T) {
		e := newTestEngine(t, Options{Latency: model.LatencyConfig{Mode: model.LatencyRecorded}},
			mkEx(1, "GET", "/l", 200, "body", withLatency(0, 30*ms)))
		log := &eventLog{}
		(&fakeClock{log: log}).install(e)
		e.Serve(newLogWriter(log), mkReq("GET", "/l"), nil)
		if want := "header,flush,wait 30ms,body"; log.String() != want {
			t.Fatalf("events = %s, want %s", log, want)
		}
	})
	t.Run("fixed: one wait then everything", func(t *testing.T) {
		e := newTestEngine(t, Options{Latency: model.LatencyConfig{Mode: model.LatencyFixed, Fixed: model.Duration(25 * ms)}},
			mkEx(1, "GET", "/l", 200, "body", withLatency(10*ms, 50*ms)))
		log := &eventLog{}
		(&fakeClock{log: log}).install(e)
		e.Serve(newLogWriter(log), mkReq("GET", "/l"), nil)
		if want := "wait 25ms,header,body"; log.String() != want {
			t.Fatalf("events = %s, want %s", log, want)
		}
	})
	t.Run("none: no waits", func(t *testing.T) {
		e := newTestEngine(t, Options{}, mkEx(1, "GET", "/l", 200, "body", withLatency(10*ms, 50*ms)))
		log := &eventLog{}
		(&fakeClock{log: log}).install(e)
		e.Serve(newLogWriter(log), mkReq("GET", "/l"), nil)
		if want := "header,body"; log.String() != want {
			t.Fatalf("events = %s, want %s", log, want)
		}
	})
	t.Run("HEAD waits but sends no body", func(t *testing.T) {
		e := newTestEngine(t, Options{Latency: model.LatencyConfig{Mode: model.LatencyRecorded}},
			mkEx(1, "GET", "/l", 200, "body", withLatency(5*ms, 15*ms)))
		log := &eventLog{}
		(&fakeClock{log: log}).install(e)
		e.Serve(newLogWriter(log), mkReq("HEAD", "/l"), nil)
		if want := "wait 5ms,header,flush,wait 10ms"; log.String() != want {
			t.Fatalf("events = %s, want %s", log, want)
		}
	})
}

func TestRealWaitsAreHonoured(t *testing.T) {
	ms := time.Millisecond
	e := newTestEngine(t, Options{Latency: model.LatencyConfig{Mode: model.LatencyFixed, Fixed: model.Duration(60 * ms)}},
		mkEx(1, "GET", "/w", 200, "x"))
	start := time.Now()
	rec, ok := serve(e, mkReq("GET", "/w"), "")
	if el := time.Since(start); !ok || el < 55*ms {
		t.Fatalf("returned after %v, want at least 60ms", el)
	} else if el > 3*time.Second {
		t.Fatalf("waited far too long: %v", el)
	}
	if bodyOf(rec) != "x" {
		t.Fatal("body")
	}

	// Recorded: the headers are observable before the body.
	e2 := newTestEngine(t, Options{Latency: model.LatencyConfig{Mode: model.LatencyRecorded, Multiplier: 1}},
		mkEx(1, "GET", "/w", 200, "x", withLatency(20*ms, 120*ms)))
	log := &eventLog{}
	var headerAt, bodyAt time.Time
	w := &stampWriter{logWriter: newLogWriter(log), onHeader: func() { headerAt = time.Now() }, onWrite: func() { bodyAt = time.Now() }}
	start = time.Now()
	e2.Serve(w, mkReq("GET", "/w"), nil)
	if h := headerAt.Sub(start); h < 18*ms {
		t.Fatalf("headers after %v, want at least ~20ms", h)
	}
	if gap := bodyAt.Sub(headerAt); gap < 90*ms {
		t.Fatalf("body %v after the headers, want at least ~100ms", gap)
	}
	if log.String() != "header,flush,body" {
		t.Fatalf("events = %s", log)
	}
}

type stampWriter struct {
	*logWriter
	onHeader, onWrite func()
}

func (w *stampWriter) WriteHeader(c int) { w.logWriter.WriteHeader(c); w.onHeader() }
func (w *stampWriter) Write(b []byte) (int, error) {
	w.onWrite()
	return w.logWriter.Write(b)
}

func TestContextCancellationStopsWaits(t *testing.T) {
	ms := time.Millisecond
	t.Run("during the wait for the first byte", func(t *testing.T) {
		e := newTestEngine(t, Options{Latency: model.LatencyConfig{Mode: model.LatencyRecorded}},
			mkEx(1, "GET", "/c", 200, "x", withLatency(10*time.Second, 20*time.Second)))
		ctx, cancel := context.WithCancel(context.Background())
		r := mkReq("GET", "/c").WithContext(ctx)
		log := &eventLog{}
		time.AfterFunc(30*ms, cancel)
		start := time.Now()
		e.Serve(newLogWriter(log), r, nil)
		if el := time.Since(start); el > 5*time.Second {
			t.Fatalf("Respond kept waiting %v after cancel", el)
		}
		if log.String() != "" {
			t.Fatalf("something was written after cancel: %s", log)
		}
	})
	t.Run("between headers and body", func(t *testing.T) {
		e := newTestEngine(t, Options{Latency: model.LatencyConfig{Mode: model.LatencyRecorded}},
			mkEx(1, "GET", "/c", 200, "x", withLatency(5*ms, 30*time.Second)))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := mkReq("GET", "/c").WithContext(ctx)
		log := &eventLog{}
		w := newLogWriter(log)
		w.onFlush = cancel // the client goes away right after seeing the headers
		start := time.Now()
		e.Serve(w, r, nil)
		if el := time.Since(start); el > 5*time.Second {
			t.Fatalf("Respond kept waiting %v after cancel", el)
		}
		if log.String() != "header,flush" {
			t.Fatalf("events = %q, want headers flushed and no body", log)
		}
	})
	t.Run("fixed", func(t *testing.T) {
		e := newTestEngine(t, Options{Latency: model.LatencyConfig{Mode: model.LatencyFixed, Fixed: model.Duration(time.Hour)}},
			mkEx(1, "GET", "/c", 200, "x"))
		ctx, cancel := context.WithTimeout(context.Background(), 30*ms)
		defer cancel()
		log := &eventLog{}
		start := time.Now()
		e.Serve(newLogWriter(log), mkReq("GET", "/c").WithContext(ctx), nil)
		if time.Since(start) > 5*time.Second || log.String() != "" {
			t.Fatalf("events = %q after %v", log, time.Since(start))
		}
	})
	t.Run("already cancelled with a wait pending", func(t *testing.T) {
		e := newTestEngine(t, Options{Latency: model.LatencyConfig{Mode: model.LatencyFixed, Fixed: model.Duration(50 * ms)}},
			mkEx(1, "GET", "/c", 200, "x"))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		log := &eventLog{}
		e.Serve(newLogWriter(log), mkReq("GET", "/c").WithContext(ctx), nil)
		if log.String() != "" {
			t.Fatalf("events = %q", log)
		}
	})
}

// ---- misses ----------------------------------------------------------------

func TestWriteMiss(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantStatus int
	}{
		{"default", 0, 404},
		{"custom", 418, 418},
		{"out of range falls back", 42, 404},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t, Options{RecordingID: "rec_abc", Match: model.MatchConfig{MissStatus: tc.status}})
			rec := httptest.NewRecorder()
			e.WriteMiss(rec, mkReq("POST", "/a%20b/c?x=1"))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if rec.Header().Get(HeaderMatch) != "miss" || rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("headers = %v", rec.Header())
			}
			var got map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("body is not JSON: %v: %s", err, rec.Body.String())
			}
			want := map[string]string{
				"error":        "no recorded response matches this request",
				"method":       "POST",
				"path":         "/a%20b/c",
				"recording_id": "rec_abc",
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("body = %v, want %v", got, want)
			}
			if rec.Header().Get("Content-Length") != strconv.Itoa(rec.Body.Len()) {
				t.Fatalf("Content-Length = %s, body %d", rec.Header().Get("Content-Length"), rec.Body.Len())
			}
		})
	}
	t.Run("HEAD has no body", func(t *testing.T) {
		e := newTestEngine(t, Options{})
		rec := httptest.NewRecorder()
		e.WriteMiss(rec, mkReq("HEAD", "/x"))
		if rec.Code != 404 || rec.Body.Len() != 0 {
			t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
		}
	})
	t.Run("escapes hostile paths into valid JSON", func(t *testing.T) {
		e := newTestEngine(t, Options{})
		rec := httptest.NewRecorder()
		r := mkReq("GET", "/x")
		r.URL.Path = "/\"quoted\"\\</script>"
		e.WriteMiss(rec, r)
		var got map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("invalid JSON: %v: %s", err, rec.Body.String())
		}
	})
}

func TestServeMissWritesNothing(t *testing.T) {
	e := newTestEngine(t, Options{})
	rec, ok := serve(e, mkReq("GET", "/none"), "")
	if ok || rec.Body.Len() != 0 || len(rec.Header()) != 0 || rec.Flushed {
		t.Fatalf("ok=%v body=%q hdr=%v", ok, rec.Body.String(), rec.Header())
	}
}
