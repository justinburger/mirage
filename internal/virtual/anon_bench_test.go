package virtual

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

// benchAnonLookup runs Lookup for r (the engine counts hits and misses like in
// production) and fails unless every lookup gives wantLevel.
func benchAnonLookup(b *testing.B, e *Engine, method, target, body, wantLevel string) {
	b.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("Content-Type", "application/json")
	bb := []byte(body)
	var m Match
	if !e.lookup(r, bb, &m) || m.Level != wantLevel {
		b.Fatalf("setup: lookup %s %s = %+v, want %s", method, target, m, wantLevel)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !e.lookup(r, bb, &m) {
			b.Fatal("miss")
		}
	}
}

// BenchmarkLookupAnonymizer compares a lookup without and with an Anonymizer
// on the same corpus.
//
//	plain/*       the recording is not anonymized (Anonymizer nil)
//	plain-anon/*  the same plain requests, with an Anonymizer that changes
//	              nothing (one form is probed)
//	raw-pii/*     raw requests carrying PII against an anonymized recording
//	replay/*      requests that are already anonymized (the second form hits)
func BenchmarkLookupAnonymizer(b *testing.B) {
	c := newAnonCorpus(b)
	mc := model.MatchConfig{Body: model.BodyJSON}
	anon := c.engine(b, Options{Match: mc}, true)
	noAnon := c.engine(b, Options{Match: mc}, false)

	// A corpus without PII: the anonymizer finds nothing to change.
	plain := func(withAnon bool) *Engine {
		o := Options{Match: mc}
		if withAnon {
			o.Anonymizer = c.anon
		}
		e := NewEngine(nil, noLatency(o))
		for i := range 1000 {
			e.Add(mkEx(int64(i+1), "GET", fmt.Sprintf("/api/v1/items/%d?page=%d", i, i%7), 200, `{"ok":true}`))
		}
		e.Add(mkEx(2000, "POST", "/api/v1/items", 201, `{"ok":true}`, withReqBody(`{"sku":"A","qty":2}`)))
		b.Cleanup(func() { _ = e.Close() })
		return e
	}

	b.Run("plain/get", func(b *testing.B) {
		benchAnonLookup(b, plain(false), "GET", "/api/v1/items/500?page=3", "", LevelExact)
	})
	b.Run("plain-anon/get", func(b *testing.B) {
		benchAnonLookup(b, plain(true), "GET", "/api/v1/items/500?page=3", "", LevelExact)
	})
	b.Run("plain/post", func(b *testing.B) {
		benchAnonLookup(b, plain(false), "POST", "/api/v1/items", `{"sku":"A","qty":2}`, LevelExact)
	})
	b.Run("plain-anon/post", func(b *testing.B) {
		benchAnonLookup(b, plain(true), "POST", "/api/v1/items", `{"sku":"A","qty":2}`, LevelExact)
	})

	b.Run("raw-pii/path", func(b *testing.B) {
		benchAnonLookup(b, anon, "GET", "/users/alice@example.com/profile", "", LevelExact)
	})
	b.Run("raw-pii/query", func(b *testing.B) {
		benchAnonLookup(b, anon, "GET", "/search?email=alice%40example.com&card=4111111111111111&page=1", "", LevelExact)
	})
	b.Run("raw-pii/body", func(b *testing.B) {
		benchAnonLookup(b, anon, "POST", "/orders", `{"email":"alice@example.com","card":"4111 1111 1111 1111","sku":"A"}`, LevelExact)
	})
	b.Run("raw-pii/miss", func(b *testing.B) {
		r := httptest.NewRequest("GET", "/users/carol@example.com/profile", nil)
		var m Match
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if anon.lookup(r, nil, &m) {
				b.Fatal("hit")
			}
		}
	})

	// What the anonymizer alone costs on the same inputs: the part of a lookup
	// that is not the engine's.
	b.Run("anonymizer-alone/path", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			q := model.Request{Method: "GET", Path: "/users/alice@example.com/profile"}
			_ = c.anon.Request(&q)
		}
	})
	b.Run("anonymizer-alone/body", func(b *testing.B) {
		body := []byte(`{"email":"alice@example.com","card":"4111 1111 1111 1111","sku":"A"}`)
		h := http.Header{"Content-Type": {"application/json"}}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			q := model.Request{Method: "POST", Path: "/orders", Header: h.Clone(), Body: bytes.Clone(body)}
			_ = c.anon.Request(&q)
		}
	})

	replay := func(name string) storedRequest { return c.stored[name] }
	b.Run("replay/path", func(b *testing.B) {
		s := replay("profile-alice")
		benchAnonLookup(b, anon, s.method, s.path, "", LevelExact)
	})
	b.Run("replay/body", func(b *testing.B) {
		s := replay("order-alice")
		benchAnonLookup(b, anon, s.method, s.path, string(s.body), LevelExact)
	})
	b.Run("baseline-no-anonymizer/replay-path", func(b *testing.B) {
		s := replay("profile-alice")
		benchAnonLookup(b, noAnon, s.method, s.path, "", LevelExact)
	})
	b.Run("baseline-no-anonymizer/replay-body", func(b *testing.B) {
		s := replay("order-alice")
		benchAnonLookup(b, noAnon, s.method, s.path, string(s.body), LevelExact)
	})
}
