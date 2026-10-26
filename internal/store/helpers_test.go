package store

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// baseTime is a fixed instant used to build deterministic exchanges.
var baseTime = time.Date(2026, 3, 14, 15, 9, 26, 123456789, time.UTC)

// openMem opens a private in-memory store that is closed with the test.
func openMem(t testing.TB) *SQLite {
	t.Helper()
	s, err := Open(MemoryPath)
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// openFile opens a file-backed store in a temp dir and returns its path.
func openFile(t testing.TB) (*SQLite, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mirage.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

// backends runs fn once against an in-memory and once against a file store, so
// behaviour that must not depend on the connection setup is tested on both.
func backends(t *testing.T, fn func(t *testing.T, s *SQLite)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { fn(t, openMem(t)) })
	t.Run("file", func(t *testing.T) {
		s, _ := openFile(t)
		fn(t, s)
	})
}

func ctx() context.Context { return context.Background() }

// newRecording creates a recording and fails the test on error.
func newRecording(t testing.TB, s *SQLite, name string) model.Recording {
	t.Helper()
	r, err := s.CreateRecording(ctx(), model.Recording{Name: name, Source: "proxy"})
	if err != nil {
		t.Fatalf("CreateRecording(%q): %v", name, err)
	}
	return r
}

// exOpt customises a test exchange.
type exOpt func(*model.Exchange)

// mkEx builds a deterministic exchange: the i-th request of a test, GET
// /items/<i> answered 200 with a small JSON body, started i seconds after
// baseTime.
func mkEx(i int, opts ...exOpt) *model.Exchange {
	body := []byte(fmt.Sprintf(`{"id":%d}`, i))
	e := &model.Exchange{
		ServiceID: "svc_test",
		StartedAt: baseTime.Add(time.Duration(i) * time.Second),
		Duration:  model.Duration(time.Duration(i+1) * time.Millisecond),
		TTFB:      model.Duration(time.Duration(i+1) * time.Millisecond / 2),
		Endpoint:  "GET /items/{id}",
		Request: model.Request{
			Method: "GET", Scheme: "https", Host: "api.example.com",
			Path: fmt.Sprintf("/items/%d", i), Proto: "HTTP/1.1",
			Header: http.Header{"Accept": {"application/json"}},
		},
		Response: model.Response{
			Status: 200, Proto: "HTTP/1.1",
			Header:   http.Header{"Content-Type": {"application/json"}},
			Body:     body,
			BodySize: int64(len(body)),
		},
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

func withStatus(status int) exOpt { return func(e *model.Exchange) { e.Response.Status = status } }
func withMethod(m string) exOpt   { return func(e *model.Exchange) { e.Request.Method = m } }
func withEndpoint(ep string) exOpt {
	return func(e *model.Exchange) { e.Endpoint = ep }
}
func withPath(p, q string) exOpt {
	return func(e *model.Exchange) { e.Request.Path, e.Request.RawQuery = p, q }
}
func withDuration(d time.Duration) exOpt {
	return func(e *model.Exchange) { e.Duration = model.Duration(d) }
}
func withError(msg string) exOpt { return func(e *model.Exchange) { e.Error = msg } }
func withReqBody(b []byte) exOpt {
	return func(e *model.Exchange) { e.Request.Body, e.Request.BodySize = b, int64(len(b)) }
}
func withRespBody(b []byte) exOpt {
	return func(e *model.Exchange) { e.Response.Body, e.Response.BodySize = b, int64(len(b)) }
}
func withStart(t time.Time) exOpt { return func(e *model.Exchange) { e.StartedAt = t } }
func withAnonymized(a bool) exOpt { return func(e *model.Exchange) { e.Anonymized = a } }

// appendN appends n default exchanges to a recording and returns them.
func appendN(t testing.TB, s *SQLite, rec string, n int, opts ...exOpt) []*model.Exchange {
	t.Helper()
	exs := make([]*model.Exchange, n)
	for i := range exs {
		exs[i] = mkEx(i, opts...)
	}
	if err := s.AppendExchanges(ctx(), rec, exs); err != nil {
		t.Fatalf("AppendExchanges: %v", err)
	}
	return exs
}

// randomBytes returns n deterministic pseudo-random (incompressible) bytes.
func randomBytes(seed int64, n int) []byte {
	rng := rand.New(rand.NewPCG(uint64(seed), 0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := 0; i < n; i += 8 {
		v := rng.Uint64()
		for j := 0; j < 8 && i+j < n; j++ {
			b[i+j] = byte(v >> (8 * j))
		}
	}
	return b
}
