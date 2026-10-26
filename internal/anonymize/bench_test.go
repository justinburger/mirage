package anonymize

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

// benchExchange replays one exchange template without allocating in the
// harness: headers and bodies are copied back into reusable storage before
// every iteration, so the reported allocations belong to the engine.
type benchExchange struct {
	tmpl             *model.Exchange
	ex               model.Exchange
	reqBody, resBody []byte
}

func newBenchExchange(tmpl *model.Exchange) *benchExchange {
	b := &benchExchange{tmpl: tmpl}
	b.ex = *tmpl
	b.ex.Request.Header = http.Header{}
	b.ex.Response.Header = http.Header{}
	return b
}

func (b *benchExchange) reset() *model.Exchange {
	t, e := b.tmpl, &b.ex
	for k, v := range t.Request.Header {
		e.Request.Header[k] = v
	}
	for k, v := range t.Response.Header {
		e.Response.Header[k] = v
	}
	b.reqBody = append(b.reqBody[:0], t.Request.Body...)
	b.resBody = append(b.resBody[:0], t.Response.Body...)
	e.Request.Body, e.Response.Body = b.reqBody, b.resBody
	e.Request.Path, e.Request.RawQuery = t.Request.Path, t.Request.RawQuery
	e.Request.BodySize, e.Response.BodySize = t.Request.BodySize, t.Response.BodySize
	e.Anonymized = false
	return e
}

func benchProfile() model.AnonymizationProfile {
	return testProfile(
		rl(model.TargetJSON, "$..customer_id", model.StrategyHash),
		rl(model.TargetJSON, "$.data.users[*].address.street", model.StrategyRedact),
		rl(model.TargetJSON, "$..notes", model.StrategyRemove),
		rl(model.TargetRegex, `req-[0-9a-f]{4}`, model.StrategyHash),
		rl(model.TargetPath, "/v1/users/{action}", model.StrategyHash),
		rl(model.TargetQuery, "include", model.StrategyHash),
	)
}

func BenchmarkExchangeDefaultProfile(b *testing.B) {
	e := defaultEngine(b)
	h := newBenchExchange(sampleExchange())
	b.SetBytes(int64(len(h.tmpl.Request.Body) + len(h.tmpl.Response.Body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := e.Exchange(h.reset()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExchangeWithRules(b *testing.B) {
	e := mustEngine(b, benchProfile())
	h := newBenchExchange(sampleExchange())
	b.SetBytes(int64(len(h.tmpl.Request.Body) + len(h.tmpl.Response.Body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := e.Exchange(h.reset()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExchangeSalted(b *testing.B) {
	e := defaultEngine(b)
	h := newBenchExchange(sampleExchange())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// A fresh salted engine per call, as replay does with per-clone identities.
		if err := e.WithSalt(fmt.Sprint(i & 15)).Exchange(h.reset()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExchangeParallel(b *testing.B) {
	e := defaultEngine(b)
	tmpl := sampleExchange()
	b.SetBytes(int64(len(tmpl.Request.Body) + len(tmpl.Response.Body)))
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		h := newBenchExchange(tmpl)
		for pb.Next() {
			if err := e.Exchange(h.reset()); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkExchangeWithReport(b *testing.B) {
	e := defaultEngine(b)
	h := newBenchExchange(sampleExchange())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.ExchangeWithReport(h.reset()); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHarnessReset is the cost of the benchmark harness itself; subtract
// it from the exchange benchmarks above.
func BenchmarkHarnessReset(b *testing.B) {
	h := newBenchExchange(sampleExchange())
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.reset()
	}
}

func BenchmarkRewriteJSONNoPII(b *testing.B) {
	e := defaultEngine(b)
	body := strings.Repeat(`{"id":12,"name":"Widget","tags":["a","b"],"price":19.99,"in_stock":true,"desc":"A very ordinary product description"},`, 20)
	body = "[" + strings.TrimSuffix(body, ",") + "]"
	resp := &model.Response{Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body), BodySize: int64(len(body))}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := e.Response(resp); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRewriteJSONManyRules(b *testing.B) {
	var rules []model.AnonymizationRule
	for i := 0; i < 50; i++ {
		rules = append(rules, rl(model.TargetJSON, fmt.Sprintf("$.data.users[*].field%d", i), model.StrategyHash))
	}
	rules = append(rules, rl(model.TargetJSON, "$..email", model.StrategyHash))
	e := mustEngine(b, bareProfile(rules...))
	tmpl := sampleExchange().Response
	b.SetBytes(int64(len(tmpl.Body)))
	b.ReportAllocs()
	body := make([]byte, 0, len(tmpl.Body))
	for i := 0; i < b.N; i++ {
		body = append(body[:0], tmpl.Body...)
		resp := &model.Response{Header: http.Header{"Content-Type": {"application/json"}}, Body: body, BodySize: int64(len(body))}
		if err := e.Response(resp); err != nil {
			b.Fatal(err)
		}
	}
}

var detectBenchInputs = map[string]string{
	"clean-short":  "Springfield",
	"clean-prose":  "Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor",
	"timestamp":    "2024-03-12T10:15:30Z",
	"url":          "https://api.example.com/v1/users?page=2&include=prefs",
	"email":        "mail jane.doe@acme-corp.io please",
	"phone":        "call (415) 555-2671 today",
	"card":         "pay with 4111 1111 1111 1111 now",
	"mixed":        "jane@acme.io 415-555-2671 192.168.1.10 123-45-6789",
	"jwt":          sampleJWT,
	"uuid-default": "550e8400-e29b-41d4-a716-446655440000",
}

func BenchmarkDetect(b *testing.B) {
	e := defaultEngine(b)
	for name, in := range detectBenchInputs {
		b.Run(name, func(b *testing.B) {
			r := e.acquire(nil)
			defer r.done()
			r.setSide(false)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = r.detect(in, nil, nil)
			}
		})
	}
}

func BenchmarkHashToken(b *testing.B) {
	k := testKeyer("bench")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = k.hashToken("customer-1234567")
	}
}

func BenchmarkFake(b *testing.B) {
	k := testKeyer("bench")
	for _, c := range []struct {
		name string
		kind model.FakeKind
		in   string
	}{
		{"email", model.FakeEmail, "jane@acme.io"},
		{"phone", model.FakePhone, "(415) 555-2671"},
		{"card", model.FakeCreditCard, "4111 1111 1111 1111"},
		{"uuid", model.FakeUUID, "550e8400-e29b-41d4-a716-446655440000"},
		{"digits", model.FakeDigits, "1234567890"},
		{"string", model.FakeString, "Springfield-42"},
		{"name", model.FakeName, "Jane Smith"},
		{"auto-email", model.FakeAuto, "jane@acme.io"},
		{"auto-string", model.FakeAuto, "Springfield-42"},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = k.fake(c.kind, c.in)
			}
		})
	}
}

func BenchmarkParseJSONPath(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := parseJSONPath("$.data.users[*].address['street-name']"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkExchangeBuiltinProfile is the sample exchange under the real
// built-in profile, with its credential and name rules (the benchmarks above
// that say "default" use the default detectors without rules).
func BenchmarkExchangeBuiltinProfile(b *testing.B) {
	e := builtinEngine(b)
	h := newBenchExchange(sampleExchange())
	b.SetBytes(int64(len(h.tmpl.Request.Body) + len(h.tmpl.Response.Body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := e.Exchange(h.reset()); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBuiltinLogin is a small request that matches several built-in rules.
func BenchmarkBuiltinLogin(b *testing.B) {
	e := builtinEngine(b)
	tmpl := `{"email":"jane.doe@acme.io","password":"hunter2","profile":{"firstName":"Jane","lastName":"Doe"},"accessToken":"abc","refresh_token":"def"}`
	body := make([]byte, 0, len(tmpl))
	b.SetBytes(int64(len(tmpl)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		body = append(body[:0], tmpl...)
		req := &model.Request{Method: "POST", Path: "/login", RawQuery: "api_key=XYZ", Header: http.Header{"Content-Type": {"application/json"}}, Body: body, BodySize: int64(len(body))}
		if err := e.Request(req); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOpaqueFill(b *testing.B) {
	for _, n := range []int{4 << 10, 64 << 10, 1 << 20} {
		b.Run(fmt.Sprint(n>>10, "KiB"), func(b *testing.B) {
			e := fillEngine(b)
			tmpl := patterned(n, "bench")
			body := make([]byte, n)
			b.SetBytes(int64(n))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				copy(body, tmpl)
				resp := &model.Response{Header: http.Header{"Content-Type": {"image/png"}}, Body: body, BodySize: int64(n)}
				if err := e.Response(resp); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkMultipart(b *testing.B) {
	file := string(patterned(100<<10, "upload"))
	in := buildMP(mpBoundary, "\r\n",
		tpart{name: "email", body: "jane.doe@acme.io"},
		tpart{name: "password", body: "hunter2"},
		tpart{name: "comment", ct: "text/plain", body: "Please call 415-555-2671 about the order, thanks."},
		tpart{name: "avatar", filename: "me.jpg", ct: "image/jpeg", body: file},
	)
	for _, c := range []struct {
		name string
		e    *Engine
	}{
		{"keep", builtinEngine(b)},
		{"fill", func() *Engine {
			p := model.DefaultAnonymizationProfile()
			p.Secret, p.OpaqueBodies = testSecret, model.OpaqueFill
			return mustEngine(b, p)
		}()},
	} {
		b.Run(c.name, func(b *testing.B) {
			body := make([]byte, len(in))
			b.SetBytes(int64(len(in)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				copy(body, in)
				req := &model.Request{Method: "POST", Path: "/u", Header: http.Header{"Content-Type": {mpCT(mpBoundary)}}, Body: body[:len(in)], BodySize: int64(len(in))}
				if err := c.e.Request(req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSplitMultipart(b *testing.B) {
	in := []byte(buildMP(mpBoundary, "\r\n",
		tpart{name: "a", body: "1"}, tpart{name: "b", body: "2"},
		tpart{name: "file", filename: "f.bin", ct: "image/png", body: string(patterned(1<<20, "split"))},
	))
	b.SetBytes(int64(len(in)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := splitMultipart(in, mpBoundary, false); !ok {
			b.Fatal("not multipart")
		}
	}
}
