package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func TestDecodeJSONBytes(t *testing.T) {
	type target struct {
		Name   string            `json:"name"`
		Count  int               `json:"count"`
		Nested struct{ A int }   `json:"nested"`
		Dur    model.Duration    `json:"dur"`
		Tags   map[string]string `json:"tags"`
	}
	tests := []struct {
		name      string
		in        string
		optional  bool
		extra     []string
		want      target
		wantErr   string // substring of the error; empty = success
		wantProbs int
	}{
		{name: "plain", in: `{"name":"a","count":3}`, want: target{Name: "a", Count: 3}},
		{name: "whitespace around", in: " \n\t{\"name\":\"a\"}\n ", want: target{Name: "a"}},
		{name: "duration string", in: `{"dur":"1500ms"}`, want: target{Dur: model.Duration(1500e6)}},
		{name: "duration number is milliseconds", in: `{"dur":250}`, want: target{Dur: model.Duration(250e6)}},
		{name: "common echo fields are dropped", in: `{"name":"a","created_at":"x","updated_at":7,"has_secret":true}`, want: target{Name: "a"}},
		{name: "echo fields match case-insensitively", in: `{"name":"a","Created_At":"x","HAS_SECRET":1}`, want: target{Name: "a"}},
		{name: "extra echo field", in: `{"name":"a","status":{"running":true}}`, extra: []string{"status"}, want: target{Name: "a"}},
		{name: "extra echo field not given", in: `{"name":"a","status":1}`, wantErr: `unknown field "status"`},
		{name: "another endpoint's echo field", in: `{"name":"a","fired":1}`, extra: []string{"status"}, wantErr: `unknown field "fired"`},
		{name: "echo fields are only top level", in: `{"nested":{"created_at":1}}`, wantErr: `unknown field "created_at"`},
		{name: "unknown field", in: `{"name":"a","bogus":1}`, wantErr: `unknown field "bogus"`},
		{name: "wrong type names the field", in: `{"count":"three"}`, wantErr: `"count"`},
		{name: "wrong nested type", in: `{"nested":{"A":"x"}}`, wantErr: "nested.A"},
		{name: "bad duration", in: `{"dur":"soon"}`, wantErr: "duration"},
		{name: "not an object: array", in: `[1]`, wantErr: "JSON object"},
		{name: "not an object: string", in: `"x"`, wantErr: "JSON object"},
		{name: "not an object: null", in: `null`, wantErr: "JSON object"},
		{name: "not an object: number", in: `42`, wantErr: "JSON object"},
		{name: "empty", in: ``, wantErr: "empty"},
		{name: "blank", in: "  \n ", wantErr: "empty"},
		{name: "empty but optional", in: ``, optional: true, want: target{}},
		{name: "truncated", in: `{"name":"a"`, wantErr: "JSON"},
		{name: "syntax error reports the offset", in: `{"name":a}`, wantErr: "byte"},
		{name: "trailing garbage", in: `{"name":"a"} {"name":"b"}`, wantErr: "JSON"},
		{name: "duplicate key: the last wins, like encoding/json", in: `{"name":"a","name":"b"}`, want: target{Name: "b"}},
		{name: "null members are fine", in: `{"name":null,"tags":null}`, want: target{}},
		{name: "unicode and escapes", in: `{"name":"café \"x\" <b>"}`, want: target{Name: `café "x" <b>`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got target
			err := decodeJSONBytes([]byte(tc.in), &got, tc.optional, tc.extra...)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want an error containing %q, got %+v", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				if status, _ := classify(err); status != http.StatusBadRequest {
					t.Errorf("status = %d, want 400", status)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDecodeJSONDoesNotLetEchoFieldsReachTheTarget(t *testing.T) {
	var svc model.Service
	err := decodeJSONBytes([]byte(`{"name":"a","created_at":"2001-01-01T00:00:00Z","updated_at":"2001-01-01T00:00:00Z"}`), &svc, false)
	if err != nil {
		t.Fatal(err)
	}
	if !svc.CreatedAt.IsZero() || !svc.UpdatedAt.IsZero() {
		t.Errorf("timestamps leaked into the target: %v %v", svc.CreatedAt, svc.UpdatedAt)
	}
	var p model.AnonymizationProfile
	if err := decodeJSONBytes([]byte(`{"name":"a","has_secret":true,"secret":"s"}`), &p, false); err != nil {
		t.Fatal(err)
	}
	if p.HasSecret || p.Secret != "s" {
		t.Errorf("HasSecret=%v Secret=%q", p.HasSecret, p.Secret)
	}
}

func TestDecodeJSONBodyLimit(t *testing.T) {
	big := `{"name":"` + strings.Repeat("x", maxJSONBody) + `"}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(big))
	var dst struct{ Name string }
	err := decodeJSON(httptest.NewRecorder(), req, &dst, false)
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) {
		t.Fatalf("err = %v, want a *http.MaxBytesError", err)
	}
	if status, body := classify(err); status != http.StatusRequestEntityTooLarge || !strings.Contains(body.Error, "10 MiB") {
		t.Errorf("classify = %d %q", status, body.Error)
	}
}

func TestWriteJSON(t *testing.T) {
	t.Run("sets headers and keeps HTML characters readable", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeJSON(rec, 201, map[string]string{"a": "<b>&"})
		if rec.Code != 201 || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("code=%d headers=%v", rec.Code, rec.Header())
		}
		if got := strings.TrimSpace(rec.Body.String()); got != `{"a":"<b>&"}` {
			t.Errorf("body = %s", got)
		}
		if rec.Header().Get("Content-Length") != fmt.Sprint(rec.Body.Len()) {
			t.Errorf("Content-Length = %q for %d bytes", rec.Header().Get("Content-Length"), rec.Body.Len())
		}
	})
	t.Run("an unencodable value becomes a clean 500", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeJSON(rec, 200, map[string]any{"c": make(chan int)})
		if rec.Code != 500 || !strings.Contains(rec.Body.String(), "internal server error") {
			t.Errorf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})
}

func TestNonNil(t *testing.T) {
	var nilSlice []int
	if got := nonNil(nilSlice); got == nil || len(got) != 0 {
		t.Errorf("nonNil(nil) = %#v", got)
	}
	in := []int{1}
	if got := nonNil(in); len(got) != 1 || got[0] != 1 {
		t.Errorf("nonNil(%v) = %v", in, got)
	}
}

func TestQueryParsing(t *testing.T) {
	req := httptest.NewRequest("GET", "/?n=12&bad=x&neg=-3&f1=1&f2=TRUE&f3=false&f4=yes&f5=0", nil)
	if n, err := queryInt(req, "n", 5); err != nil || n != 12 {
		t.Errorf("n = %d, %v", n, err)
	}
	if n, err := queryInt(req, "missing", 5); err != nil || n != 5 {
		t.Errorf("missing = %d, %v", n, err)
	}
	if n, err := queryInt(req, "neg", 0); err != nil || n != -3 {
		t.Errorf("neg = %d, %v", n, err)
	}
	if _, err := queryInt(req, "bad", 0); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Errorf("bad: %v", err)
	}
	for name, want := range map[string]bool{"f1": true, "f2": true, "f3": false, "f4": true, "f5": false, "absent": false} {
		if got := queryFlag(req, name); got != want {
			t.Errorf("queryFlag(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestCheckID(t *testing.T) {
	for id, ok := range map[string]bool{
		"svc_abc123": true, "web-1": true, "a.b": true, "A": true, "9": true,
		"": false, "-x": false, ".x": false, "_x": false, "a b": false, "a/b": false, "a?b": false, "é": false,
		strings.Repeat("a", 64): true, strings.Repeat("a", 65): false,
	} {
		if got := checkID(id) == nil; got != ok {
			t.Errorf("checkID(%q) ok = %v, want %v", id, got, ok)
		}
	}
}

func TestPublicMessage(t *testing.T) {
	for in, want := range map[string]string{
		`store: recording "rec_x": not found`:                        `recording "rec_x": not found`,
		`ops: copy recording rec_x: store: recording "r": not found`: `copy recording rec_x: recording "r": not found`,
		`store: service name "x" already in use: conflict`:           `service name "x" already in use`,
		`replay: job rpl_1 is running: conflict`:                     `job rpl_1 is running`,
		`plain message`:                                              `plain message`,
		`the restore: point is gone`:                                 `the restore: point is gone`, // "restore" is not a package prefix
	} {
		if got := publicMessage(errors.New(in)); got != want {
			t.Errorf("publicMessage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeyedMutex(t *testing.T) {
	var k keyedMutex
	var wg sync.WaitGroup
	// Two plain counters, one per key: only the key's lock protects each, so a
	// lost update (or a race report) means the lock does not exclude.
	var a, b int
	for i := 0; i < 8; i++ {
		for key, counter := range map[string]*int{"a": &a, "b": &b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 200; j++ {
					unlock := k.Lock(key)
					*counter++
					unlock()
				}
			}()
		}
	}
	wg.Wait()
	if a != 1600 || b != 1600 {
		t.Errorf("a=%d b=%d, want 1600 each", a, b)
	}
	if k.size() != 0 {
		t.Errorf("%d keys leaked", k.size())
	}

	// Different keys do not block each other.
	unlockA := k.Lock("a")
	done := make(chan struct{})
	go func() {
		defer close(done)
		k.Lock("b")()
	}()
	<-done
	unlockA()
}

func TestRequestContextIsDetachedForApply(t *testing.T) {
	// persistService must finish the Apply even when the request is cancelled.
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc := model.Service{Name: "detached", ListenAddr: ":0", Upstream: "http://u"}.WithDefaults()
	out, err := e.srv.persistService(ctx, svc)
	if err != nil {
		t.Fatalf("persistService with a cancelled context: %v", err)
	}
	if _, ok := e.svcs.lastApplied(out.ID); !ok {
		t.Error("Apply did not run")
	}
}
