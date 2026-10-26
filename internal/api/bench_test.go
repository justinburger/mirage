package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
)

// serve runs one request against the handler without a network.
func serve(b *testing.B, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	b.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func benchEnv(b *testing.B, tweak ...func(*Deps)) *env {
	b.Helper()
	return newEnv(b, tweak...)
}

func BenchmarkGetService(b *testing.B) {
	e := benchEnv(b)
	svc := e.seedService(model.Service{Name: "bench"})
	h := e.srv.Handler()
	req := benchRequest("GET", "/api/v1/services/"+svc.ID, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rec := serve(b, h, req); rec.Code != 200 {
			b.Fatal(rec.Code)
		}
	}
}

func BenchmarkListServices100(b *testing.B) {
	e := benchEnv(b)
	for i := 0; i < 100; i++ {
		e.seedService(model.Service{Name: fmt.Sprintf("svc-%03d", i), ListenAddr: ":0"})
	}
	h := e.srv.Handler()
	req := benchRequest("GET", "/api/v1/services", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rec := serve(b, h, req); rec.Code != 200 {
			b.Fatal(rec.Code)
		}
	}
}

func BenchmarkListExchangesPage100(b *testing.B) {
	e := benchEnv(b)
	rec := e.seedRecording("bench", 5000)
	h := e.srv.Handler()
	req := benchRequest("GET", "/api/v1/recordings/"+rec.ID+"/exchanges?limit=100&offset=2000", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r := serve(b, h, req); r.Code != 200 {
			b.Fatal(r.Code)
		}
	}
}

func BenchmarkGetExchange(b *testing.B) {
	e := benchEnv(b)
	rec := e.seedRecording("bench", 100)
	items, _, _ := e.st.ListExchanges(context.Background(), rec.ID, model.ExchangeQuery{Limit: 1})
	h := e.srv.Handler()
	req := benchRequest("GET", fmt.Sprintf("/api/v1/exchanges/%d", items[0].ID), nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r := serve(b, h, req); r.Code != 200 {
			b.Fatal(r.Code)
		}
	}
}

func BenchmarkServiceStats(b *testing.B) {
	e := benchEnv(b)
	svc := e.seedService(model.Service{Name: "bench"})
	col := e.reg.Collector(metrics.ServiceCollectorName(svc.ID), model.KindService, svc.ID)
	for i := 0; i < 50; i++ {
		col.Observe(metrics.Observation{Endpoint: fmt.Sprintf("GET /e%d", i), Status: 200, Duration: time.Millisecond})
	}
	h := e.srv.Handler()
	req := benchRequest("GET", "/api/v1/services/"+svc.ID+"/stats", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r := serve(b, h, req); r.Code != 200 {
			b.Fatal(r.Code)
		}
	}
}

func BenchmarkCreateFaultRule(b *testing.B) {
	e := benchEnv(b)
	h := e.srv.Handler()
	payload := []byte(`{"name":"bench","enabled":true,"probability":0.1,"type":"error","status":503,"path_pattern":"/checkout/**"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := benchRequest("POST", "/api/v1/faults", bytes.NewReader(payload))
		if r := serve(b, h, req); r.Code != 201 {
			b.Fatal(r.Code, r.Body.String())
		}
		if i%50 == 49 { // keep the rule set (and the reload) small
			b.StopTimer()
			rules, _ := e.st.ListFaultRules(context.Background())
			for _, r := range rules {
				_ = e.st.DeleteFaultRule(context.Background(), r.ID)
			}
			_ = e.srv.reloadFaults(context.Background())
			b.StartTimer()
		}
	}
}

func BenchmarkDecodeJSONService(b *testing.B) {
	body := []byte(`{"id":"svc_x","name":"payments","listen_addr":":9001","upstream":"https://payments.internal:8443","mode":"record","enabled":true,
	"recording_id":"rec_1","anonymize_capture":true,"profile_id":"default","endpoint_patterns":["/v1/customers/{id}"],"timeout":"60s","max_body_bytes":1048576,
	"match":{"query":"exact","ignore_params":["_ts"],"body":"json","path_templates":true,"selection":"sequential","miss_status":404},
	"latency":{"mode":"recorded","multiplier":1,"fixed":"0s","jitter":"0s"},"error_amplification":1,"error_floor":0,
	"status":{"service_id":"svc_x","running":true},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var svc model.Service
		if err := decodeJSONBytes(body, &svc, false, echoServiceStatus); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAuthOverhead(b *testing.B) {
	for _, withToken := range []bool{false, true} {
		b.Run(fmt.Sprintf("token=%v", withToken), func(b *testing.B) {
			e := benchEnv(b, func(d *Deps) {
				if withToken {
					d.AdminToken = testToken
				}
			})
			h := e.srv.Handler()
			req := benchRequest("GET", "/api/v1/info", nil)
			if withToken {
				req.Header.Set("Authorization", "Bearer "+testToken)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if r := serve(b, h, req); r.Code != 200 {
					b.Fatal(r.Code)
				}
			}
		})
	}
}

func BenchmarkStaticUI(b *testing.B) {
	e := benchEnv(b)
	h := e.srv.Handler()
	first := httptest.NewRecorder()
	h.ServeHTTP(first, benchRequest("GET", "/assets/app.js", nil))
	etag := first.Header().Get("ETag")

	b.Run("200", func(b *testing.B) {
		req := benchRequest("GET", "/assets/app.js", nil)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if r := serve(b, h, req); r.Code != 200 {
				b.Fatal(r.Code)
			}
		}
	})
	b.Run("304", func(b *testing.B) {
		req := benchRequest("GET", "/assets/app.js", nil)
		req.Header.Set("If-None-Match", etag)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if r := serve(b, h, req); r.Code != 304 {
				b.Fatal(r.Code)
			}
		}
	})
	b.Run("spa-fallback", func(b *testing.B) {
		req := benchRequest("GET", "/services/svc_1", nil)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if r := serve(b, h, req); r.Code != 200 {
				b.Fatal(r.Code)
			}
		}
	})
}

// benchHub measures one broadcast tick (marshalling once, fan-out to n clients
// that all keep up).
func BenchmarkHubTick(b *testing.B) {
	for _, clients := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("clients=%d", clients), func(b *testing.B) {
			e := benchEnv(b)
			for i := 0; i < 20; i++ {
				name := fmt.Sprintf("service:svc_%d", i)
				col := e.reg.Collector(name, model.KindService, fmt.Sprintf("svc_%d", i))
				for j := 0; j < 10; j++ {
					col.Observe(metrics.Observation{Endpoint: fmt.Sprintf("GET /e%d", j), Status: 200, Duration: time.Millisecond})
				}
			}
			for i := 0; i < 20; i++ {
				e.seedService(model.Service{Name: fmt.Sprintf("s%d", i), ListenAddr: ":0"})
			}
			h := e.srv.hub
			stop := make(chan struct{})
			done := make(chan struct{}, clients)
			for i := 0; i < clients; i++ {
				c, unsub := h.subscribe()
				defer unsub()
				go func() {
					defer func() { done <- struct{}{} }()
					for {
						select {
						case <-c.ch:
						case <-stop:
							return
						}
					}
				}()
			}
			now := time.Now()
			var total int
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				u := e.reg.Tick(now.Add(time.Duration(i) * time.Second))
				var frame []byte
				frame = h.appendStats(frame, u)
				frame = appendEvent(frame, "services", e.srv.servicesJSON())
				frame, _ = h.appendReplays(context.Background(), frame, nil)
				h.broadcast(frame)
				total += len(frame)
			}
			b.StopTimer()
			b.ReportMetric(float64(total)/float64(b.N), "bytes/tick")
			close(stop)
			for i := 0; i < clients; i++ {
				<-done
			}
		})
	}
}

func BenchmarkHubBroadcastOnly(b *testing.B) {
	e := benchEnv(b)
	h := e.srv.hub
	const clients = 100
	stop := make(chan struct{})
	for i := 0; i < clients; i++ {
		c, unsub := h.subscribe()
		defer unsub()
		go func() {
			for {
				select {
				case <-c.ch:
				case <-stop:
					return
				}
			}
		}()
	}
	defer close(stop)
	msg := []byte(strings.Repeat("x", 4096))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.broadcast(msg)
	}
}
