package anonymize

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

var (
	fuzzOnce    sync.Once
	fuzzEngines [4]*Engine // default, rules, rules (second instance), detectors off
)

// fuzzProfile exercises every kind of rule and every detector.
func fuzzProfile() model.AnonymizationProfile {
	p := testProfile(
		rl(model.TargetJSON, "$..email", model.StrategyHash),
		rl(model.TargetJSON, "$.items[*].card", model.StrategyMask),
		rl(model.TargetJSON, "$..secret", model.StrategyRemove),
		withFake(rl(model.TargetJSON, "$.user.*", model.StrategyFake), model.FakeAuto),
		rl(model.TargetJSON, "$['odd key'][0]", model.StrategyRedact),
		rl(model.TargetRegex, `(?i)password=(\S+)`, model.StrategyRedact),
		rl(model.TargetRegex, `\b[A-Z]{2}-\d{4}\b`, model.StrategyHash),
		withFake(rl(model.TargetPath, "/users/{id}/orders/{order}", model.StrategyFake), model.FakeName),
		rl(model.TargetPath, "/files/{rest...}", model.StrategyMask),
		rl(model.TargetHeader, "X-Token", model.StrategyRemove),
		rl(model.TargetQuery, "token", model.StrategyRemove),
		rl(model.TargetForm, "password", model.StrategyRedact),
		rl(model.TargetCookie, "sid", model.StrategyRemove),
	)
	p.Detectors = append([]model.Detector{}, model.AllDetectors...)
	return p
}

func fuzzSetup(t testing.TB) {
	fuzzOnce.Do(func() {
		var err error
		if fuzzEngines[0], err = New(testProfile()); err != nil {
			panic(err)
		}
		if fuzzEngines[1], err = New(fuzzProfile()); err != nil {
			panic(err)
		}
		if fuzzEngines[2], err = New(fuzzProfile()); err != nil {
			panic(err)
		}
		off := fuzzProfile()
		off.Detectors = nil
		off.DetectorStrategy = model.StrategyMask
		if fuzzEngines[3], err = New(off); err != nil {
			panic(err)
		}
	})
}

func FuzzExchange(f *testing.F) {
	seeds := []struct {
		path, query, hname, hval, ct string
		body                         string
		truncated                    bool
		mode                         uint8
	}{
		{"/users/42/orders/9", "email=jane%40acme.io&token=abc", "Authorization", "Bearer xyz", "application/json",
			`{"email":"jane@acme.io","items":[{"card":"4111111111111111"}],"user":{"name":"Jane","id":7},"secret":1}`, false, 1},
		{"/files/a/b/c", "a=1&&b", "Cookie", "sid=1; b=2", "application/json", `[1,2.5e3,"x",null,true,{"a":[]}]`, false, 1},
		{"/", "", "X-Token", "t", "application/x-ndjson", "{\"email\":\"a@b.co\"}\n\nbad {\n{\"secret\":1}", false, 1},
		{"/a", "x=%zz", "Content-Length", "10", "application/x-www-form-urlencoded", "password=hunter2&card=4111+1111+1111+1111&n=", false, 1},
		{"/ip/10.0.0.1", "", "Set-Cookie", "sid=a; Path=/", "text/plain", "call 415-555-2671 ssn 123-45-6789 iban GB82 WEST 1234 5698 7654 32", false, 0},
		{"/x", "", "X", "y", "application/json", `{"email":"jane@ac`, true, 2},
		{"//a//b/", "&=&", "Y", "", "", "plain jane@acme.io", false, 3},
		{"/u", "", "Content-Encoding", "gzip", "application/json", `{"email":"jane@acme.io"}`, false, 0},
		{"/", "", "Content-Type", "application/json", "application/json", strings.Repeat("[", 300) + strings.Repeat("]", 300), false, 1},
		{"/", "", "A", "B", "application/json", `{"odd key":["x"],"user":{"a":"b@c.de","b":12345678901234567890}}`, false, 2},
		{"/", "", "A", "B", "application/json", "{\"a\":\"\\ud83d\\ude00 \\u0040 \\ud800\"}", false, 0},
		{"/", "", "A", "B", "application/json", "\xef\xbb\xbf{}", false, 0},
	}
	for _, s := range seeds {
		f.Add(s.path, s.query, s.hname, s.hval, s.ct, []byte(s.body), s.truncated, s.mode)
	}

	f.Fuzz(func(t *testing.T, path, query, hname, hval, ct string, body []byte, truncated bool, mode uint8) {
		fuzzSetup(t)
		e := fuzzEngines[int(mode)%len(fuzzEngines)]
		twin := fuzzEngines[2] // same profile and secret as engine 1
		if int(mode)%len(fuzzEngines) != 1 {
			twin = e
		}

		build := func() *model.Exchange {
			mkHeader := func() http.Header {
				h := http.Header{}
				if hname != "" {
					h[hname] = []string{hval}
				}
				if ct != "" {
					h["Content-Type"] = []string{ct}
				}
				return h
			}
			return &model.Exchange{
				Request: model.Request{
					Method: "POST", Path: path, RawQuery: query, Header: mkHeader(),
					Body: bytes.Clone(body), BodySize: int64(len(body)), BodyTruncated: truncated,
				},
				Response: model.Response{
					Status: 200, Header: mkHeader(),
					Body: bytes.Clone(body), BodySize: int64(len(body)), BodyTruncated: truncated,
				},
				Error: query,
			}
		}

		ex := build()
		if err := e.Exchange(ex); err != nil {
			t.Fatalf("Exchange failed: %v", err)
		}
		if !ex.Anonymized {
			t.Fatal("Anonymized not set")
		}

		// Valid JSON in, valid JSON out.
		mt := mediaType(ct)
		if isJSONType(mt) && !truncated && json.Valid(body) && getHeader(ex.Request.Header, "Content-Encoding") == "" && hname != "Content-Encoding" {
			if !json.Valid(ex.Request.Body) || !json.Valid(ex.Response.Body) {
				t.Fatalf("valid JSON in, invalid JSON out:\n in:  %q\n out: %q", body, ex.Request.Body)
			}
		}

		// Deterministic, and independent of the engine instance.
		ex2 := build()
		if err := twin.Exchange(ex2); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ex, ex2) {
			t.Fatalf("not deterministic:\n%+v\n%+v", ex, ex2)
		}

		// Reporting never changes the result.
		ex3 := build()
		if _, err := e.ExchangeWithReport(ex3); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ex, ex3) {
			t.Fatalf("report changed the result:\n%+v\n%+v", ex, ex3)
		}

		// Body bookkeeping.
		for _, side := range []struct {
			name      string
			before    []byte
			after     []byte
			size      int64
			header    http.Header
			truncated bool
		}{
			{"request", body, ex.Request.Body, ex.Request.BodySize, ex.Request.Header, ex.Request.BodyTruncated},
			{"response", body, ex.Response.Body, ex.Response.BodySize, ex.Response.Header, ex.Response.BodyTruncated},
		} {
			if bytes.Equal(side.before, side.after) {
				if side.size != int64(len(body)) {
					t.Fatalf("%s: BodySize changed without a body change", side.name)
				}
				continue
			}
			if !side.truncated && side.size != int64(len(side.after)) {
				t.Fatalf("%s: BodySize %d, body %d bytes", side.name, side.size, len(side.after))
			}
			if side.size < int64(len(side.after)) {
				t.Fatalf("%s: BodySize %d smaller than the body %d", side.name, side.size, len(side.after))
			}
			if k, ok := headerKey(side.header, "Content-Length"); ok && side.header[k][0] != strconv.FormatInt(side.size, 10) {
				t.Fatalf("%s: Content-Length %q, BodySize %d", side.name, side.header[k][0], side.size)
			}
		}

		// The path keeps its shape: same number of segments, no new delimiters.
		if strings.Count(ex.Request.Path, "/") != strings.Count(path, "/") {
			t.Fatalf("path segments changed: %q -> %q", path, ex.Request.Path)
		}
		for _, c := range "?# \t\r\n" {
			if strings.ContainsRune(ex.Request.Path, c) && !strings.ContainsRune(path, c) {
				t.Fatalf("path gained %q: %q -> %q", c, path, ex.Request.Path)
			}
		}

		// A second pass over anonymized data must not fail either.
		if err := e.Exchange(ex); err != nil {
			t.Fatalf("second pass failed: %v", err)
		}
	})
}

// TestFuzzSeedsRun makes the seed corpus part of the ordinary test run.
func TestFuzzSeedsRun(t *testing.T) {
	// `go test` runs FuzzExchange's seeds automatically; this test only
	// guards the shared engines against construction errors.
	fuzzSetup(t)
	for i, e := range fuzzEngines {
		if e == nil {
			t.Errorf("engine %d is nil", i)
		}
	}
}
