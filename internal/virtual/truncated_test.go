package virtual

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

// withTruncatedResponse marks the recorded response body as cut at capture:
// Body keeps the first len(kept) bytes of a body that was full bytes long.
func withTruncatedResponse(full int64) exOpt {
	return func(e *model.Exchange) {
		e.Response.BodySize = full
		e.Response.BodyTruncated = true
	}
}

// A recorded response whose body was cut by max_body_bytes must never be
// served as if it were complete: the client would get a 200 with a short body
// (and a Content-Length to match) that it cannot tell from the real thing.
func TestTruncatedResponseIsNotServedAsComplete(t *testing.T) {
	e := newTestEngine(t, noLatency(Options{}),
		mkEx(1, "GET", "/big", 200, strings.Repeat("x", 64), withTruncatedResponse(1<<20)),
	)
	for _, method := range []string{"GET", "HEAD"} {
		rec, ok := serve(e, mkReq(method, "/big"), "")
		if !ok {
			t.Fatalf("%s: a truncated recording is still a match, got a miss", method)
		}
		if rec.Code != http.StatusBadGateway {
			t.Errorf("%s: status %d, want 502", method, rec.Code)
		}
		if got := rec.Header().Get(HeaderError); got != ErrorTruncated {
			t.Errorf("%s: %s = %q, want %q", method, HeaderError, got, ErrorTruncated)
		}
		if got := rec.Header().Get(HeaderMatch); got != LevelExact {
			t.Errorf("%s: %s = %q, want exact", method, HeaderMatch, got)
		}
		if strings.Contains(bodyOf(rec), "xxxx") {
			t.Errorf("%s: the truncated recorded body was served", method)
		}
		if method == "HEAD" {
			if rec.Body.Len() != 0 {
				t.Errorf("HEAD wrote a body")
			}
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("body is not JSON: %v: %q", err, rec.Body.String())
		}
		if doc["kept_bytes"] != float64(64) || doc["recorded_bytes"] != float64(1<<20) {
			t.Errorf("body sizes = %v / %v, want 64 / %d", doc["kept_bytes"], doc["recorded_bytes"], 1<<20)
		}
		if doc["recorded_bytes_exact"] != true {
			t.Errorf("recorded_bytes_exact = %v, want true for a body stored decoded", doc["recorded_bytes_exact"])
		}
		if !strings.Contains(doc["error"].(string), "max_body_bytes") {
			t.Errorf("error %q does not tell the user what to change", doc["error"])
		}
	}
	if s := e.Stats(); s.Truncated != 2 || s.Hits != 2 {
		t.Errorf("stats truncated=%d hits=%d, want 2 and 2", s.Truncated, s.Hits)
	}
}

// When the same request was also recorded completely, only the complete
// recordings are chosen, and the selection rotates over them exactly as if
// the truncated ones were not there.
func TestCompleteCandidatesRotateAsIfTruncatedWereAbsent(t *testing.T) {
	mk := func(sel model.Selection, query model.QueryMatch) *Engine {
		return newTestEngine(t, noLatency(Options{Match: model.MatchConfig{Selection: sel, Query: query}, Seed: 7}),
			mkEx(1, "GET", "/doc", 200, strings.Repeat("a", 16), withTruncatedResponse(4096)),
			mkEx(2, "GET", "/doc", 200, "B"),
			mkEx(3, "GET", "/doc", 200, strings.Repeat("b", 16), withTruncatedResponse(4096)),
			mkEx(4, "GET", "/doc", 200, "C"),
		)
	}
	sequence := func(e *Engine, n int) string {
		var b strings.Builder
		for range n {
			rec, ok := serve(e, mkReq("GET", "/doc"), "")
			if !ok || rec.Code != http.StatusOK {
				t.Fatalf("ok=%v status=%d body=%q", ok, rec.Code, bodyOf(rec))
			}
			b.WriteString(bodyOf(rec))
		}
		return b.String()
	}
	for _, q := range []model.QueryMatch{model.QueryExact, model.QuerySubset} {
		if got := sequence(mk(model.SelectSequential, q), 8); got != "BCBCBCBC" {
			t.Errorf("sequential (%s query): %q, want BCBCBCBC", q, got)
		}
		if got := sequence(mk(model.SelectFirst, q), 4); got != "BBBB" {
			t.Errorf("first (%s query): %q, want BBBB", q, got)
		}
		seq := sequence(mk(model.SelectRandom, q), 4000)
		if b := strings.Count(seq, "B"); b < 1800 || b > 2200 {
			t.Errorf("random (%s query): B chosen %d of 4000 times, want ~2000", q, b)
		}
	}
	if s := mk(model.SelectSequential, model.QueryExact).Stats(); s.Truncated != 0 {
		t.Errorf("stats truncated = %d, want 0", s.Truncated)
	}
}

// A recording that kept no body at all (max_body_bytes -1, a HAR without
// response content) is flagged truncated but has nothing misleading to serve:
// it is answered as recorded, as before.
func TestTruncatedWithoutKeptBodyIsServedAsRecorded(t *testing.T) {
	e := newTestEngine(t, noLatency(Options{}),
		mkEx(1, "GET", "/nobody", 200, "", withTruncatedResponse(1234)),
	)
	rec, ok := serve(e, mkReq("GET", "/nobody"), "")
	if !ok || rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get(HeaderError) != "" {
		t.Fatalf("ok=%v status=%d body=%q %s=%q", ok, rec.Code, bodyOf(rec), HeaderError, rec.Header().Get(HeaderError))
	}
	if s := e.Stats(); s.Truncated != 0 {
		t.Errorf("stats truncated = %d, want 0", s.Truncated)
	}
}

// In hybrid mode a request whose only recordings are truncated is a miss, so
// the proxy forwards it to the upstream and records it again.
func TestTruncatedAsMissForHybrid(t *testing.T) {
	trunc := mkEx(1, "GET", "/big", 200, strings.Repeat("x", 64), withTruncatedResponse(1<<20))
	e := newTestEngine(t, noLatency(Options{TruncatedAsMiss: true}), trunc,
		mkEx(2, "GET", "/both", 200, strings.Repeat("y", 8), withTruncatedResponse(1<<20)),
		mkEx(3, "GET", "/both", 200, "whole"),
	)
	if rec, ok := serve(e, mkReq("GET", "/big"), ""); ok || rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("truncated-only match: served=%v (wrote %d %q), want a miss that writes nothing", ok, rec.Code, bodyOf(rec))
	}
	if rec, ok := serve(e, mkReq("GET", "/both"), ""); !ok || bodyOf(rec) != "whole" {
		t.Fatalf("a complete recording must still be served: ok=%v body=%q", ok, bodyOf(rec))
	}
	if s := e.Stats(); s.Misses != 1 || s.Hits != 1 || s.Truncated != 0 {
		t.Errorf("stats misses=%d hits=%d truncated=%d, want 1, 1, 0", s.Misses, s.Hits, s.Truncated)
	}
	if !OptionsFromService(model.Service{Mode: model.ModeHybrid, RecordingID: "r"}, nil).TruncatedAsMiss ||
		OptionsFromService(model.Service{Mode: model.ModeVirtualize, RecordingID: "r"}, nil).TruncatedAsMiss {
		t.Error("OptionsFromService must set TruncatedAsMiss for hybrid services only")
	}
}

// A body cut while being decompressed only knows it was bigger than the cap.
func TestTruncatedRecordedSizeExactness(t *testing.T) {
	for _, tc := range []struct {
		encoding string
		exact    bool
	}{{"", true}, {"gzip", false}} {
		e := newTestEngine(t, noLatency(Options{}),
			mkEx(1, "GET", "/z", 200, strings.Repeat("z", 32), withTruncatedResponse(1001), func(x *model.Exchange) {
				x.Response.ContentEncoding = tc.encoding
			}),
		)
		rec, _ := serve(e, mkReq("GET", "/z"), "")
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc["recorded_bytes_exact"] != tc.exact || doc["kept_bytes"] != float64(32) {
			t.Errorf("encoding %q: exact=%v kept=%v, want %v and 32", tc.encoding, doc["recorded_bytes_exact"], doc["kept_bytes"], tc.exact)
		}
	}
}

// Error amplification swaps responses for others from the endpoint's pools;
// a truncated recording must not be used as a replacement.
func TestTruncatedNotUsedForErrorAmplification(t *testing.T) {
	e := newTestEngine(t, noLatency(Options{ErrorAmplification: 1, ErrorFloor: 1, Seed: 3}),
		mkEx(1, "GET", "/items/1", 200, `{"ok":true}`),
		mkEx(2, "GET", "/items/1", 500, strings.Repeat("e", 8), withTruncatedResponse(9000)),
	)
	// With error_floor 1 every response becomes an error. The only recorded
	// error is truncated, so the replacement must be the synthetic 503, not
	// the cut-off 500 body.
	for range 20 {
		rec, ok := serve(e, mkReq("GET", "/items/1"), "")
		if !ok {
			t.Fatal("miss")
		}
		if strings.Contains(bodyOf(rec), "eeee") {
			t.Fatalf("served the truncated error body: %d %q", rec.Code, bodyOf(rec))
		}
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get(HeaderInjected) != "error" {
			t.Fatalf("status %d %s=%q, want the injected synthetic 503", rec.Code, HeaderInjected, rec.Header().Get(HeaderInjected))
		}
	}
	if s := e.Stats(); s.Truncated != 0 {
		t.Errorf("stats truncated = %d, want 0: no truncated recording should have been chosen", s.Truncated)
	}
}

// The flag survives lazy mode, where the index keeps no bodies and loads
// them from the store on demand.
func TestTruncatedDetectedInLazyMode(t *testing.T) {
	st := openStore(t)
	rec := fill(t, st,
		mkEx(1, "GET", "/big", 200, strings.Repeat("x", 32), withTruncatedResponse(1<<20)),
	)
	e, err := Build(context.Background(), st, noLatency(Options{RecordingID: rec.ID, MaxResidentBytes: 1}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if !e.Lazy() {
		t.Fatal("engine is not lazy")
	}
	res, ok := serve(e, mkReq("GET", "/big"), "")
	if !ok || res.Code != http.StatusBadGateway || res.Header().Get(HeaderError) != ErrorTruncated {
		t.Fatalf("ok=%v status=%d %s=%q", ok, res.Code, HeaderError, res.Header().Get(HeaderError))
	}
}
