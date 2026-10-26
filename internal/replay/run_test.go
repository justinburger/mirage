package replay

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// numbered returns n GET exchanges /items/0../items/n-1 gap apart that all
// belong to one endpoint.
func numbered(n int, gap time.Duration) []rx {
	out := make([]rx, n)
	for i := range out {
		out[i] = rx{at: time.Duration(i) * gap, path: fmt.Sprintf("/items/%d", i), endpoint: "GET /items/{id}"}
	}
	return out
}

func TestRunTimelineBasic(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(20, 5*time.Millisecond)...)
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, target.URL)
	cfg.CompareStatus = true

	col := newTestCollector()
	sum, series, err := Run(context.Background(), st, cfg, RunOptions{Collector: col, TickCollector: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Planned != 20 || sum.Sent != 20 || sum.Completed != 20 || sum.Failed != 0 || sum.Errors != 0 || sum.Mismatches != 0 {
		t.Errorf("counts: %+v", sum)
	}
	if sum.StatusCounts["200"] != 20 || len(sum.StatusCounts) != 1 {
		t.Errorf("status counts %v", sum.StatusCounts)
	}
	if sum.Latency.Count != 20 || sum.CorrectedLatency.Count != 20 {
		t.Errorf("latency counts %d / %d", sum.Latency.Count, sum.CorrectedLatency.Count)
	}
	if sum.CorrectedLatency.Min < sum.Latency.Min || sum.CorrectedLatency.Max < sum.Latency.Max {
		t.Errorf("corrected latency below service time: %+v vs %+v", sum.CorrectedLatency, sum.Latency)
	}
	if sum.BytesReceived != 40 || sum.BytesSent != 0 {
		t.Errorf("bytes: sent %d received %d", sum.BytesSent, sum.BytesReceived)
	}
	if len(sum.Endpoints) != 1 || sum.Endpoints[0].Endpoint != "GET /items/{id}" || sum.Endpoints[0].Count != 20 {
		t.Errorf("endpoints: %+v", sum.Endpoints)
	}
	if e := sum.Elapsed.D(); e < 95*time.Millisecond || e > 2*time.Second {
		t.Errorf("elapsed %v for a 95ms recording", e)
	}
	if sum.AchievedRPS <= 0 || sum.TargetRPS <= 0 {
		t.Errorf("rps: target %v achieved %v", sum.TargetRPS, sum.AchievedRPS)
	}
	if sum.Saturated || sum.MaxInFlight < 1 {
		t.Errorf("saturated=%v max in flight=%d", sum.Saturated, sum.MaxInFlight)
	}
	got := target.uris()
	sort.Strings(got)
	var want []string
	for i := 0; i < 20; i++ {
		want = append(want, fmt.Sprintf("/items/%d", i))
	}
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("target saw %v", got)
	}
	// The collector saw the same traffic, like a service would.
	snap := col.Snapshot()
	if snap.Requests != 20 || snap.InFlight != 0 || snap.CorrectedLatency == nil || snap.CorrectedLatency.Count != 20 {
		t.Errorf("collector: requests=%d in_flight=%d corrected=%v", snap.Requests, snap.InFlight, snap.CorrectedLatency)
	}
	var total int64
	for _, p := range series {
		total += p.Requests
	}
	if total != 20 || len(series) == 0 {
		t.Errorf("series has %d points with %d requests, want 20", len(series), total)
	}
}

func TestRunSpeedCompressesTime(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(11, 100*time.Millisecond)...) // 1s of traffic
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, target.URL)
	cfg.Speed = 10
	sum, _ := mustRun(t, st, cfg)
	if sum.Sent != 11 {
		t.Fatalf("sent %d", sum.Sent)
	}
	if e := sum.Elapsed.D(); e < 95*time.Millisecond || e > 600*time.Millisecond {
		t.Errorf("1s of traffic at speed 10 took %v, want ~100ms", e)
	}
	// The target saw the original spacing divided by ten. The first request
	// may arrive late (connection set-up on a loaded CI runner shortens the
	// observed span), so the lower bound only proves the copies were spread
	// out rather than sent in a burst.
	reqs := target.requests()
	span := reqs[len(reqs)-1].at.Sub(reqs[0].at)
	if span < 50*time.Millisecond || span > 500*time.Millisecond {
		t.Errorf("arrival span %v, want ~100ms", span)
	}
}

func TestRunCoordinatedOmission(t *testing.T) {
	// The target answers in 20ms and the job has 2 connections, so it can
	// complete ~100 req/s, but the schedule offers 200 req/s. Service time
	// stays ~20ms; the corrected latency, from the intended send time, grows
	// as the backlog does.
	st := openStore(t)
	rec := seedRecording(t, st, numbered(10, time.Millisecond)...)
	target := newRecorder(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(20 * time.Millisecond) })
	cfg := baseCfg(rec, target.URL)
	cfg.Mode = model.ReplayRate
	cfg.Rate = 200
	cfg.MaxRequests = 100
	cfg.Concurrency = 2
	sum, _ := mustRun(t, st, cfg)

	if sum.Completed != 100 || sum.Failed != 0 {
		t.Fatalf("completed %d failed %d", sum.Completed, sum.Failed)
	}
	if !sum.Saturated || sum.MaxInFlight != 2 {
		t.Errorf("saturated=%v max in flight=%d, want true and 2", sum.Saturated, sum.MaxInFlight)
	}
	if p50 := sum.Latency.P50; p50 < 15 || p50 > 80 {
		t.Errorf("service p50 = %.1fms, want ~20", p50)
	}
	if c99 := sum.CorrectedLatency.P99; c99 < 150 {
		t.Errorf("corrected p99 = %.1fms, want hundreds of ms of queueing", c99)
	}
	if sum.CorrectedLatency.P50 < 3*sum.Latency.P50 {
		t.Errorf("corrected p50 %.1f should be far above service p50 %.1f", sum.CorrectedLatency.P50, sum.Latency.P50)
	}
	if lag := sum.MaxLag.D(); lag < 150*time.Millisecond {
		t.Errorf("max lag %v, want the backlog (~250ms)", lag)
	}
	t.Logf("service p50=%.1fms p99=%.1fms; corrected p50=%.1fms p99=%.1fms; max lag %v",
		sum.Latency.P50, sum.Latency.P99, sum.CorrectedLatency.P50, sum.CorrectedLatency.P99, sum.MaxLag.D())
}

func TestRunNotSaturatedWithRoomToSpare(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(30, 2*time.Millisecond)...)
	target := newRecorder(t, nil)
	sum, _ := mustRun(t, st, baseCfg(rec, target.URL))
	if sum.Saturated {
		t.Error("30 quick requests against 2048 slots must not saturate")
	}
}

func TestRunTimeouts(t *testing.T) {
	st := openStore(t)
	exs := numbered(8, 2*time.Millisecond)
	for _, i := range []int{1, 3, 6} {
		exs[i].path = "/slow"
	}
	rec := seedRecording(t, st, exs...)
	target := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
		}
	})
	cfg := baseCfg(rec, target.URL)
	cfg.Timeout = model.Duration(100 * time.Millisecond)
	start := time.Now()
	sum, _ := mustRun(t, st, cfg)
	if sum.Timeouts != 3 || sum.Failed != 3 || sum.Completed != 5 || sum.Sent != 8 {
		t.Errorf("timeouts=%d failed=%d completed=%d sent=%d", sum.Timeouts, sum.Failed, sum.Completed, sum.Sent)
	}
	if sum.Errors != 3 || sum.ErrorRate != 3.0/8 {
		t.Errorf("errors=%d rate=%v, want 3 and 0.375", sum.Errors, sum.ErrorRate)
	}
	// A request that timed out waited for the target, so it is in the latency
	// summaries, at least Timeout long (see TestTimeoutsReachTheLatencySummaries).
	if sum.Latency.Count != 8 || sum.CorrectedLatency.Count != 8 || sum.Latency.Max < 100 {
		t.Errorf("latency covers %d / %d requests, max %.1fms; want all 8 and the 100ms timeouts", sum.Latency.Count, sum.CorrectedLatency.Count, sum.Latency.Max)
	}
	if time.Since(start) > time.Second {
		t.Errorf("run took %v: timeouts must bound slow requests", time.Since(start))
	}
}

func TestRunTransportErrors(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(6, time.Millisecond)...)
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()

	col := newTestCollector()
	sum, _, err := Run(context.Background(), st, baseCfg(rec, url), RunOptions{Collector: col, TickCollector: true})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Sent != 6 || sum.Failed != 6 || sum.Completed != 0 || sum.Timeouts != 0 || sum.Errors != 6 || sum.ErrorRate != 1 {
		t.Errorf("summary: %+v", sum)
	}
	if len(sum.StatusCounts) != 0 || sum.Latency.Count != 0 || sum.CorrectedLatency.Count != 0 {
		t.Errorf("nothing was sent, so no status or latency: %v %d %d", sum.StatusCounts, sum.Latency.Count, sum.CorrectedLatency.Count)
	}
	// The dashboard still sees them, as errors with no status.
	if snap := col.Snapshot(); snap.Requests != 6 || snap.Errors != 6 || snap.StatusClasses["err"] != 6 {
		t.Errorf("collector: %+v", snap)
	}
	if sum.Endpoints[0].Errors != 6 {
		t.Errorf("endpoint errors = %d", sum.Endpoints[0].Errors)
	}
}

func TestRunCompareStatusAndErrorStatuses(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st,
		rx{at: 0, path: "/a", status: 200},
		rx{at: 1, path: "/b", status: 404},
		rx{at: 2, path: "/c", status: 200},
		rx{at: 3, path: "/d", status: 500},
		rx{at: 4, path: "/e", status: 429},
	)
	target := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/c":
			w.WriteHeader(500) // recorded 200: mismatch and an error
		case "/d":
			w.WriteHeader(500) // as recorded
		case "/e":
			w.WriteHeader(200) // recorded 429: mismatch
		}
	})
	cfg := baseCfg(rec, target.URL)
	cfg.CompareStatus = true
	sum, _ := mustRun(t, st, cfg)
	if sum.Mismatches != 3 { // /b answers 200 (recorded 404), /c, /e
		t.Errorf("mismatches = %d, want 3", sum.Mismatches)
	}
	if sum.Errors != 2 || sum.StatusCounts["500"] != 2 || sum.StatusCounts["200"] != 3 {
		t.Errorf("errors=%d statuses=%v", sum.Errors, sum.StatusCounts)
	}

	cfg.CompareStatus = false
	sum, _ = mustRun(t, st, cfg)
	if sum.Mismatches != 0 {
		t.Errorf("without CompareStatus mismatches = %d", sum.Mismatches)
	}
}

func TestRunHeaders(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, rx{path: "/h", header: map[string]string{
		"Authorization": "Bearer recorded", "Cookie": "session=1", "Accept": "application/json",
		"X-Request-Id": "abc", "Host": "prod.example.com", "Connection": "close",
	}})
	tests := []struct {
		name  string
		cfg   func(*model.ReplayConfig)
		jobID string
		check func(t *testing.T, s seen)
	}{
		{"defaults", nil, "", func(t *testing.T, s seen) {
			if s.header.Get("Authorization") != "Bearer recorded" || s.header.Get("Cookie") != "session=1" ||
				s.header.Get("Accept") != "application/json" || s.header.Get("X-Request-Id") != "abc" {
				t.Errorf("recorded headers not preserved: %v", s.header)
			}
			if s.header.Get("X-Mirage-Replay") != "cli" {
				t.Errorf("X-Mirage-Replay = %q, want cli", s.header.Get("X-Mirage-Replay"))
			}
			if s.host == "prod.example.com" || !strings.HasPrefix(s.host, "127.0.0.1:") {
				t.Errorf("Host = %q, want the target's", s.host)
			}
		}},
		{"set remove and host", func(c *model.ReplayConfig) {
			c.SetHeaders = map[string]string{"Authorization": "Bearer staging", "X-Env": "stg"}
			c.RemoveHeaders = []string{"cookie"}
			c.HostHeader = "api.staging.example"
		}, "", func(t *testing.T, s seen) {
			if s.header.Get("Authorization") != "Bearer staging" || s.header.Get("X-Env") != "stg" {
				t.Errorf("SetHeaders not applied: %v", s.header)
			}
			if _, ok := s.header["Cookie"]; ok {
				t.Error("Cookie should have been removed")
			}
			if s.host != "api.staging.example" {
				t.Errorf("Host = %q", s.host)
			}
			if s.header.Get("Accept") != "application/json" {
				t.Error("other headers must survive")
			}
		}},
		{"job id marker", nil, "rpl_abc123", func(t *testing.T, s seen) {
			if s.header.Get("X-Mirage-Replay") != "rpl_abc123" {
				t.Errorf("X-Mirage-Replay = %q", s.header.Get("X-Mirage-Replay"))
			}
		}},
		{"marker can be removed", func(c *model.ReplayConfig) { c.RemoveHeaders = []string{"X-Mirage-Replay"} }, "x", func(t *testing.T, s seen) {
			if _, ok := s.header["X-Mirage-Replay"]; ok {
				t.Error("X-Mirage-Replay should be absent")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := newRecorder(t, nil)
			cfg := baseCfg(rec, target.URL)
			if tt.cfg != nil {
				tt.cfg(&cfg)
			}
			_, _, err := Run(context.Background(), st, cfg, RunOptions{Collector: newTestCollector(), TickCollector: true, JobID: tt.jobID})
			if err != nil {
				t.Fatal(err)
			}
			reqs := target.requests()
			if len(reqs) != 1 {
				t.Fatalf("target got %d requests", len(reqs))
			}
			tt.check(t, reqs[0])
		})
	}
}

func TestRunSendsBodiesIntact(t *testing.T) {
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	st := openStore(t)
	rec := seedRecording(t, st,
		rx{at: 0, method: "POST", path: "/json", body: `{"a":1,"nested":{"b":[1,2,3]}}`, header: map[string]string{"Content-Type": "application/json"}},
		rx{at: ms(1), method: "PUT", path: "/binary", body: string(all), header: map[string]string{"Content-Type": "application/octet-stream"}},
		rx{at: ms(2), method: "POST", path: "/big", body: string(big)},
		rx{at: ms(3), method: "GET", path: "/nobody"},
		rx{at: ms(4), method: "PATCH", path: "/form", body: "a=1&b=%20x", header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}},
	)
	target := newRecorder(t, nil)
	sum, _ := mustRun(t, st, baseCfg(rec, target.URL))
	if sum.Completed != 5 {
		t.Fatalf("completed %d", sum.Completed)
	}
	want := map[string]string{
		"/json": `{"a":1,"nested":{"b":[1,2,3]}}`, "/binary": string(all), "/big": string(big), "/nobody": "", "/form": "a=1&b=%20x",
	}
	for _, s := range target.requests() {
		path := strings.SplitN(s.uri, "?", 2)[0]
		if string(s.body) != want[path] {
			t.Errorf("%s: body of %d bytes differs from the recorded %d", path, len(s.body), len(want[path]))
		}
		if path == "/json" && s.header.Get("Content-Type") != "application/json" {
			t.Errorf("content type %q", s.header.Get("Content-Type"))
		}
	}
	if wantBytes := int64(len(want["/json"]) + len(all) + len(big) + len(want["/form"])); sum.BytesSent != wantBytes {
		t.Errorf("BytesSent = %d, want %d", sum.BytesSent, wantBytes)
	}
}

func TestRunRecompressesGzipBodies(t *testing.T) {
	plain := strings.Repeat(`{"event":"click","n":42}`, 40)
	st := openStore(t)
	rec := seedRecording(t, st, rx{method: "POST", path: "/events", body: plain, encoding: "gzip",
		header: map[string]string{"Content-Type": "application/json"}})
	target := newRecorder(t, nil)
	sum, _ := mustRun(t, st, baseCfg(rec, target.URL))
	if sum.Completed != 1 {
		t.Fatalf("completed %d", sum.Completed)
	}
	s := target.requests()[0]
	if s.header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", s.header.Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(bytes.NewReader(s.body))
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != plain {
		t.Error("decompressed body differs from the recorded one")
	}
	if int64(len(s.body)) != sum.BytesSent {
		t.Errorf("BytesSent %d, wire body %d", sum.BytesSent, len(s.body))
	}
}

func TestRunTargetBasePath(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st,
		rx{at: 0, path: "/users/42", query: "x=1&y=a%20b"},
		rx{at: ms(1), path: "/files/a%2Fb/c"},
		rx{at: ms(2), path: "/"},
	)
	target := newRecorder(t, nil)
	_, _ = mustRun(t, st, baseCfg(rec, target.URL+"/api/v1/"))
	got := target.uris()
	sort.Strings(got)
	want := []string{"/api/v1/", "/api/v1/files/a%2Fb/c", "/api/v1/users/42?x=1&y=a%20b"}
	if !slices.Equal(got, want) {
		t.Errorf("target saw %v, want %v", got, want)
	}
}

func TestRunIncludeExclude(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st,
		rx{at: 0, method: "GET", path: "/users/1"},
		rx{at: ms(1), method: "POST", path: "/users"},
		rx{at: ms(2), method: "GET", path: "/health"},
	)
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, target.URL)
	cfg.Include = []string{`^GET `}
	cfg.Exclude = []string{`/health`}
	sum, _ := mustRun(t, st, cfg)
	if sum.Planned != 1 || sum.Sent != 1 || !slices.Equal(target.uris(), []string{"/users/1"}) {
		t.Errorf("planned=%d sent=%d uris=%v", sum.Planned, sum.Sent, target.uris())
	}
}

func TestRunErrorAmplification(t *testing.T) {
	st := openStore(t)
	var exs []rx
	for i := 0; i < 10; i++ {
		exs = append(exs, rx{at: time.Duration(i) * time.Millisecond, path: "/ok"})
	}
	exs = append(exs, rx{at: ms(10), path: "/bad", status: 503}, rx{at: ms(11), path: "/bad", status: 500})
	rec := seedRecording(t, st, exs...)
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, target.URL)
	cfg.ErrorAmplification = 5
	sum, _ := mustRun(t, st, cfg)
	counts := map[string]int{}
	for _, u := range target.uris() {
		counts[u]++
	}
	if counts["/ok"] != 10 || counts["/bad"] != 10 {
		t.Errorf("target saw %v, want 10 /ok and 10 /bad (2 errors x 5)", counts)
	}
	if sum.Planned != 20 || sum.Sent != 20 {
		t.Errorf("planned=%d sent=%d", sum.Planned, sum.Sent)
	}
}

func TestRunAmplifySendsCopiesSpreadOverTheGap(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, rx{at: 0, path: "/a"}, rx{at: 200 * time.Millisecond, path: "/b"})
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, target.URL)
	cfg.Amplify = 4
	sum, _ := mustRun(t, st, cfg)
	if sum.Sent != 8 || sum.Planned != 8 {
		t.Fatalf("sent=%d planned=%d", sum.Sent, sum.Planned)
	}
	var a []time.Time
	for _, s := range target.requests() {
		if s.uri == "/a" {
			a = append(a, s.at)
		}
	}
	if len(a) != 4 {
		t.Fatalf("%d copies of /a", len(a))
	}
	// Copies of /a are 50ms apart (200ms gap / 4), not a burst.
	for i := 1; i < 4; i++ {
		if d := a[i].Sub(a[i-1]); d < 25*time.Millisecond || d > 150*time.Millisecond {
			t.Errorf("copies %d and %d are %v apart, want ~50ms", i-1, i, d)
		}
	}
}

func TestRunDurationStopsALoop(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(5, 10*time.Millisecond)...) // pass = 50ms
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, target.URL)
	cfg.Loop = true
	cfg.Duration = model.Duration(300 * time.Millisecond)
	sum, _ := mustRun(t, st, cfg)
	if e := sum.Elapsed.D(); e < 290*time.Millisecond || e > time.Second {
		t.Errorf("elapsed %v, want ~300ms", e)
	}
	if sum.Sent < 25 || sum.Sent > 35 {
		t.Errorf("sent %d in 300ms of a 100 req/s recording, want ~30", sum.Sent)
	}
	if sum.Planned < 28 || sum.Planned > 32 {
		t.Errorf("planned %d, want ~30", sum.Planned)
	}
}

func TestRunMaxRequestsIsExact(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(7, time.Millisecond)...)
	for _, mode := range []model.ReplayMode{model.ReplayMax, model.ReplayTimeline, model.ReplayRate} {
		t.Run(string(mode), func(t *testing.T) {
			target := newRecorder(t, nil)
			cfg := baseCfg(rec, target.URL)
			cfg.Mode = mode
			cfg.MaxRequests = 37
			cfg.Loop = mode != model.ReplayRate
			cfg.Rate = 2000
			cfg.Speed = 50
			cfg.Concurrency = 8
			sum, _ := mustRun(t, st, cfg)
			if got := target.count(); got != 37 || sum.Sent != 37 || sum.Planned != 37 {
				t.Errorf("target saw %d, sent %d, planned %d, want 37", got, sum.Sent, sum.Planned)
			}
		})
	}
}

func TestRunMaxModeSinglePass(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(25, time.Hour)...) // hours apart: max mode ignores timing
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, target.URL)
	cfg.Mode = model.ReplayMax
	cfg.Concurrency = 4
	sum, _ := mustRun(t, st, cfg)
	if sum.Sent != 25 || sum.Planned != 25 || sum.TargetRPS != 0 {
		t.Errorf("sent=%d planned=%d target rps=%v", sum.Sent, sum.Planned, sum.TargetRPS)
	}
	// Closed model: corrected latency is the service time (no queueing).
	if sum.CorrectedLatency.Count != 25 || sum.CorrectedLatency.P50 < sum.Latency.P50 {
		t.Errorf("corrected %+v service %+v", sum.CorrectedLatency, sum.Latency)
	}
	if sum.MaxInFlight > 4 {
		t.Errorf("max in flight %d with 4 workers", sum.MaxInFlight)
	}
}

func TestRunRateModeStages(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(5, time.Millisecond)...)
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, target.URL)
	cfg.Mode = model.ReplayRate
	cfg.Stages = []model.ReplayStage{stage(400*time.Millisecond, 500, false), stage(200*time.Millisecond, 500, true)}
	sum, _ := mustRun(t, st, cfg)
	// Area: 0.4s ramp to 500 = 100, plus 0.2s at 500 = 100.
	if sum.Planned != 200 || sum.Sent < 199 || sum.Sent > 200 {
		t.Errorf("planned=%d sent=%d, want 200", sum.Planned, sum.Sent)
	}
	if e := sum.Elapsed.D(); e < 590*time.Millisecond || e > 2*time.Second {
		t.Errorf("elapsed %v, the job lasts through its stages (600ms)", e)
	}
	if sum.TargetRPS < 330 || sum.TargetRPS > 340 {
		t.Errorf("target rps %v, want 200/0.6 = 333", sum.TargetRPS)
	}
}

func TestRunCancelReturnsPartialSummary(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(50, 20*time.Millisecond)...) // 1s
	var inflight atomic.Int64
	target := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/items/9" {
			inflight.Add(1)
			<-r.Context().Done() // hangs until the client gives up
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	sum, series, err := Run(ctx, st, baseCfg(rec, target.URL), RunOptions{Collector: newTestCollector(), TickCollector: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 900*time.Millisecond {
		t.Errorf("cancel took %v", time.Since(start))
	}
	if sum.Sent < 5 || sum.Sent >= 50 || sum.Planned != 50 {
		t.Errorf("sent=%d planned=%d, want a partial run of 50", sum.Sent, sum.Planned)
	}
	if sum.Failed != 0 || sum.Timeouts != 0 {
		t.Errorf("requests aborted by the cancel must not count as failures: failed=%d timeouts=%d", sum.Failed, sum.Timeouts)
	}
	if sum.Elapsed.D() <= 0 || sum.Completed == 0 || len(series) == 0 {
		t.Errorf("partial results missing: %+v series=%d", sum, len(series))
	}
}

func TestRunCancelBeforeStart(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(5, time.Millisecond)...)
	target := newRecorder(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := Run(ctx, st, baseCfg(rec, target.URL), RunOptions{Collector: newTestCollector(), TickCollector: true})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if target.count() != 0 {
		t.Errorf("target saw %d requests from a cancelled run", target.count())
	}
}

func TestRunErrors(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(3, time.Millisecond)...)
	empty, err := st.CreateRecording(context.Background(), model.Recording{Name: "empty", Source: "proxy"})
	if err != nil {
		t.Fatal(err)
	}
	col := func() RunOptions { return RunOptions{Collector: newTestCollector(), TickCollector: true} }

	t.Run("invalid config", func(t *testing.T) {
		_, _, err := Run(context.Background(), st, model.ReplayConfig{RecordingID: rec, Target: "ftp://x", Mode: model.ReplayRate}, col())
		var ve *model.ValidationError
		if !errors.As(err, &ve) || len(ve.Problems) < 2 {
			t.Errorf("err = %v, want a ValidationError with several problems", err)
		}
	})
	t.Run("no collector", func(t *testing.T) {
		if _, _, err := Run(context.Background(), st, baseCfg(rec, "http://x"), RunOptions{}); err == nil {
			t.Error("a missing collector must be an error")
		}
	})
	t.Run("missing recording", func(t *testing.T) {
		_, _, err := Run(context.Background(), st, baseCfg("rec_nope", "http://127.0.0.1:1"), col())
		if !errors.Is(err, model.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
	t.Run("missing profile", func(t *testing.T) {
		cfg := baseCfg(rec, "http://127.0.0.1:1")
		cfg.ProfileID = "prof_nope"
		_, _, err := Run(context.Background(), st, cfg, col())
		if !errors.Is(err, model.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
	t.Run("empty recording", func(t *testing.T) {
		_, _, err := Run(context.Background(), st, baseCfg(empty.ID, "http://127.0.0.1:1"), col())
		if !errors.Is(err, ErrEmptyRecording) {
			t.Errorf("err = %v, want ErrEmptyRecording", err)
		}
	})
	t.Run("everything filtered", func(t *testing.T) {
		cfg := baseCfg(rec, "http://127.0.0.1:1")
		cfg.Include = []string{"^DELETE "}
		_, _, err := Run(context.Background(), st, cfg, col())
		if !errors.Is(err, ErrEmptyRecording) || !contains(err.Error(), "include/exclude") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("too large to preload", func(t *testing.T) {
		r, err := newRunner(st, baseCfg(rec, "http://127.0.0.1:1"), col())
		if err != nil {
			t.Fatal(err)
		}
		r.maxPreload = 300
		sum, _, err := r.execute(context.Background())
		if !errors.Is(err, ErrRecordingTooLarge) || !contains(err.Error(), "narrow it with include/exclude") {
			t.Errorf("err = %v", err)
		}
		if sum.Sent != 0 || sum.Planned != -1 {
			t.Errorf("a failed load reports an untouched summary: %+v", sum)
		}
	})
}

func TestRunProgressAndSeries(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(24, 100*time.Millisecond)...) // 2.3s
	target := newRecorder(t, nil)
	var mu sync.Mutex
	var progress []model.ReplaySummary
	col := newTestCollector()
	sum, series, err := Run(context.Background(), st, baseCfg(rec, target.URL), RunOptions{
		Collector: col, TickCollector: true,
		Progress: func(s model.ReplaySummary) { mu.Lock(); progress = append(progress, s); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(progress) < 2 {
		t.Fatalf("Progress called %d times in a 2.3s run", len(progress))
	}
	for i := 1; i < len(progress); i++ {
		if progress[i].Sent < progress[i-1].Sent || progress[i].Elapsed < progress[i-1].Elapsed {
			t.Errorf("progress went backwards: %+v then %+v", progress[i-1], progress[i])
		}
	}
	if progress[0].Sent == 0 || progress[len(progress)-1].Sent > sum.Sent {
		t.Errorf("progress sent %d..%d, final %d", progress[0].Sent, progress[len(progress)-1].Sent, sum.Sent)
	}
	if len(series) < 3 {
		t.Fatalf("%d series points for a 2.3s run", len(series))
	}
	var total int64
	for i, p := range series {
		total += p.Requests
		if i > 0 && p.T != series[i-1].T+1000 {
			t.Errorf("point %d at %d does not follow %d by one second", i, p.T, series[i-1].T)
		}
		if p.Requests > 0 && (p.P95 <= 0 || p.CorrectedP95 < p.P95) {
			t.Errorf("point %d: p95=%v corrected p95=%v", i, p.P95, p.CorrectedP95)
		}
	}
	if total != sum.Sent {
		t.Errorf("series requests sum %d, want %d", total, sum.Sent)
	}
	if first := time.UnixMilli(series[0].T); time.Since(first) > 10*time.Second {
		t.Errorf("first point at %v", first)
	}
}

func TestRunTLSTarget(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(3, time.Millisecond)...)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the handshake failures are the point
	srv.StartTLS()
	defer srv.Close()

	cfg := baseCfg(rec, srv.URL)
	sum, _ := mustRun(t, st, cfg)
	if sum.Failed != 3 || sum.Completed != 0 {
		t.Errorf("an untrusted certificate must fail the requests: %+v", sum)
	}
	cfg.InsecureSkipVerify = true
	sum, _ = mustRun(t, st, cfg)
	if sum.Failed != 0 || sum.Completed != 3 {
		t.Errorf("with InsecureSkipVerify: %+v", sum)
	}
}

func TestRunReusesConnections(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(200, time.Millisecond)...)
	var conns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	cfg := baseCfg(rec, srv.URL)
	cfg.Mode = model.ReplayMax
	cfg.Concurrency = 4
	sum, _ := mustRun(t, st, cfg)
	if sum.Completed != 200 {
		t.Fatalf("completed %d", sum.Completed)
	}
	// Bodies are drained, so connections go back to the pool: one per worker
	// (plus a little slack for a racing dial), not one per request.
	if n := conns.Load(); n > 8 {
		t.Errorf("%d connections for 200 requests on 4 workers", n)
	}
}

func TestRunDoesNotFollowRedirects(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, rx{path: "/old", status: 302})
	target := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusFound)
		}
	})
	cfg := baseCfg(rec, target.URL)
	cfg.CompareStatus = true
	sum, _ := mustRun(t, st, cfg)
	if sum.StatusCounts["302"] != 1 || sum.Mismatches != 0 || target.count() != 1 {
		t.Errorf("statuses %v mismatches %d target saw %d", sum.StatusCounts, sum.Mismatches, target.count())
	}
}

func TestRunSurvivesATargetThatHangsUp(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, numbered(10, time.Millisecond)...)
	target := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/items/3" || r.URL.Path == "/items/7" {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close() // no response at all
		}
	})
	sum, _ := mustRun(t, st, baseCfg(rec, target.URL))
	if sum.Sent != 10 || sum.Completed != 8 || sum.Failed != 2 || sum.Timeouts != 0 || sum.Errors != 2 {
		t.Errorf("sent=%d completed=%d failed=%d timeouts=%d errors=%d", sum.Sent, sum.Completed, sum.Failed, sum.Timeouts, sum.Errors)
	}
}

func TestRunTargetUserinfoSendsBasicAuth(t *testing.T) {
	st := openStore(t)
	rec := seedRecording(t, st, rx{path: "/a"})
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, strings.Replace(target.URL, "http://", "http://bob:s3cret@", 1))
	_, _ = mustRun(t, st, cfg)
	user, pw, ok := (&http.Request{Header: target.requests()[0].header}).BasicAuth()
	if !ok || user != "bob" || pw != "s3cret" {
		t.Errorf("basic auth = %q %q %v", user, pw, ok)
	}
}

func TestExecRecoversFromPanics(t *testing.T) {
	// A bug in a worker must not take the process down.
	st := openStore(t)
	r, err := newRunner(st, baseCfg("rec", "http://127.0.0.1:1"), RunOptions{Collector: newTestCollector()})
	if err != nil {
		t.Fatal(err)
	}
	r.reqCtx = context.Background()
	r.start = time.Now()
	r.tr = newTransport(r.cfg)
	// A nil item panics inside exec.
	r.exec(job{})
	if r.failed.Load() != 1 || r.busy.Load() != 0 {
		t.Errorf("failed=%d busy=%d", r.failed.Load(), r.busy.Load())
	}
}

func TestStatusCountsOutsideTheRange(t *testing.T) {
	r, _ := newRunner(openStore(t), baseCfg("rec", "http://x"), RunOptions{Collector: newTestCollector()})
	r.statuses[204].Add(2)
	r.statuses[999].Add(1)
	got := r.summary().StatusCounts
	if got["204"] != 2 || got["999"] != 1 || len(got) != 2 {
		t.Errorf("status counts %v", got)
	}
}

func TestRunOverHTTP2(t *testing.T) {
	// Header maps are shared between concurrent requests; HTTP/2 encodes them
	// on its own goroutines, so run a burst through it under the race detector.
	st := openStore(t)
	rec := seedRecording(t, st, numbered(200, 100*time.Microsecond)...)
	var protos sync.Map
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protos.Store(r.Proto, true)
		_, _ = io.WriteString(w, "ok")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	cfg := baseCfg(rec, srv.URL)
	cfg.InsecureSkipVerify = true
	cfg.Mode = model.ReplayMax
	cfg.Loop = true
	cfg.MaxRequests = 2000
	cfg.Concurrency = 32
	cfg.SetHeaders = map[string]string{"X-Env": "stg"}
	sum, _ := mustRun(t, st, cfg)
	if sum.Completed != 2000 || sum.Failed != 0 {
		t.Fatalf("completed %d failed %d", sum.Completed, sum.Failed)
	}
	if _, ok := protos.Load("HTTP/2.0"); !ok {
		t.Error("the target never saw HTTP/2")
	}
}

// apiReplayController mirrors api.ReplayController, which this package must
// not import; keep the two in sync.
type apiReplayController interface {
	Create(ctx context.Context, name string, cfg model.ReplayConfig) (model.ReplayJob, error)
	Start(ctx context.Context, id string) error
	Stop(id string) error
	Get(ctx context.Context, id string) (model.ReplayJob, error)
	List(ctx context.Context) ([]model.ReplayJob, error)
	Delete(ctx context.Context, id string) error
	Series(ctx context.Context, id string, sinceMs int64) ([]model.SeriesPoint, error)
	Running() []model.ReplayJob
}

var _ apiReplayController = (*Manager)(nil)

// stallGen delays the scheduler once, as a stalled machine would.
type stallGen struct {
	generator
	at, calls int
	d         time.Duration
}

func (g *stallGen) next() (job, bool) {
	if g.calls++; g.calls == g.at {
		time.Sleep(g.d)
	}
	return g.generator.next()
}

func TestLateSchedulerStillSendsWhatWasDueBeforeDuration(t *testing.T) {
	// The scheduler is frozen from 50ms to 550ms, past the 400ms Duration.
	// Nothing is wrong with the target, so every request that was due before
	// Duration must still go out (late, and the lag says so) rather than be
	// dropped.
	st := openStore(t)
	rec := seedRecording(t, st, numbered(5, time.Millisecond)...)
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, target.URL)
	cfg.Mode = model.ReplayRate
	cfg.Rate = 100
	cfg.Duration = model.Duration(400 * time.Millisecond)
	r, err := newRunner(st, cfg, RunOptions{Collector: newTestCollector(), TickCollector: true})
	if err != nil {
		t.Fatal(err)
	}
	prep, err := r.load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r.runLoad(context.Background(), &stallGen{generator: newRateGen(prep, r.cfg), at: 5, d: 500 * time.Millisecond})
	sum := r.summary()
	if sum.Sent != 40 || sum.Completed != 40 {
		t.Errorf("sent %d completed %d, want all 40 requests due before 400ms", sum.Sent, sum.Completed)
	}
	if lag := sum.MaxLag.D(); lag < 400*time.Millisecond {
		t.Errorf("max lag %v does not show the stall", lag)
	}
}

func TestDurationCutsTheBacklogOfASaturatedJob(t *testing.T) {
	// The target answers in 100ms through one slot (10 req/s) while the
	// schedule offers 100 req/s for 300ms. At Duration the scheduler stops
	// waiting for a slot: the backlog is dropped, and Saturated says why
	// Sent is below Planned.
	st := openStore(t)
	rec := seedRecording(t, st, numbered(5, time.Millisecond)...)
	target := newRecorder(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(100 * time.Millisecond) })
	cfg := baseCfg(rec, target.URL)
	cfg.Mode = model.ReplayRate
	cfg.Rate = 100
	cfg.Duration = model.Duration(300 * time.Millisecond)
	cfg.Concurrency = 1
	start := time.Now()
	sum, _ := mustRun(t, st, cfg)
	if sum.Planned != 30 || sum.Sent < 2 || sum.Sent > 5 || !sum.Saturated {
		t.Errorf("planned=%d sent=%d saturated=%v", sum.Planned, sum.Sent, sum.Saturated)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("a saturated job must still end near Duration, took %v", d)
	}
	if sum.Failed != 0 {
		t.Errorf("failed %d", sum.Failed)
	}
}
