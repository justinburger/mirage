package store

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

func TestAppendExchangesAssignsIDsAndSeq(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		a, b := newRecording(t, s, "a"), newRecording(t, s, "b")

		first := []*model.Exchange{mkEx(0), mkEx(1), mkEx(2)}
		if err := s.AppendExchanges(ctx(), a.ID, first); err != nil {
			t.Fatal(err)
		}
		second := []*model.Exchange{mkEx(3), mkEx(4)}
		if err := s.AppendExchanges(ctx(), a.ID, second); err != nil {
			t.Fatal(err)
		}
		other := []*model.Exchange{mkEx(0), mkEx(1)}
		if err := s.AppendExchanges(ctx(), b.ID, other); err != nil {
			t.Fatal(err)
		}

		seen := map[int64]bool{}
		check := func(rec string, exs []*model.Exchange, firstSeq int64) {
			t.Helper()
			for i, ex := range exs {
				if ex.Seq != firstSeq+int64(i) {
					t.Errorf("%s exchange %d: Seq = %d, want %d", rec, i, ex.Seq, firstSeq+int64(i))
				}
				if ex.RecordingID != rec {
					t.Errorf("%s exchange %d: RecordingID = %q", rec, i, ex.RecordingID)
				}
				if ex.ID <= 0 || seen[ex.ID] {
					t.Errorf("%s exchange %d: ID %d is not unique and positive", rec, i, ex.ID)
				}
				seen[ex.ID] = true
			}
		}
		check(a.ID, first, 1)
		check(a.ID, second, 4) // continues the recording's sequence
		check(b.ID, other, 1)  // each recording has its own sequence
		if first[2].ID >= second[0].ID {
			t.Errorf("IDs are not increasing: %d then %d", first[2].ID, second[0].ID)
		}
	})
}

func TestAppendExchangesRecordingCounters(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		clock := newClock(s)
		rec := newRecording(t, s, "counters")
		createdAt := clock.Now()

		clock.Advance(time.Minute)
		t10, t20, t05, t30 := baseTime.Add(10*time.Second), baseTime.Add(20*time.Second), baseTime.Add(5*time.Second), baseTime.Add(30*time.Second)
		batch1 := []*model.Exchange{
			mkEx(1, withStart(t20), withReqBody([]byte("abc")), withRespBody([]byte("defgh"))), // 8 captured bytes
			mkEx(2, withStart(t10), withStatus(500), withRespBody(nil)),                        // error status, 0 bytes
		}
		if err := s.AppendExchanges(ctx(), rec.ID, batch1); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetRecording(ctx(), rec.ID)
		if got.ExchangeCount != 2 || got.ErrorCount != 1 || got.Bytes != 8 {
			t.Errorf("after batch 1: count %d errors %d bytes %d, want 2/1/8", got.ExchangeCount, got.ErrorCount, got.Bytes)
		}
		if got.FirstAt == nil || !got.FirstAt.Equal(t10) || got.LastAt == nil || !got.LastAt.Equal(t20) {
			t.Errorf("after batch 1: first/last = %v / %v, want %v / %v", got.FirstAt, got.LastAt, t10, t20)
		}
		if !got.UpdatedAt.Equal(clock.Now()) || !got.CreatedAt.Equal(createdAt) {
			t.Errorf("timestamps: created %v updated %v", got.CreatedAt, got.UpdatedAt)
		}

		// A later batch containing an earlier and a later time widens the range.
		clock.Advance(time.Minute)
		batch2 := []*model.Exchange{
			mkEx(3, withStart(t05), withError("connection reset"), withStatus(502), withReqBody(bytes.Repeat([]byte("x"), 100))),
			mkEx(4, withStart(t30), withStatus(429), withRespBody([]byte("12"))),
			mkEx(5, withStart(time.Time{})), // unknown start time is ignored for the range
		}
		if err := s.AppendExchanges(ctx(), rec.ID, batch2); err != nil {
			t.Fatal(err)
		}
		got, _ = s.GetRecording(ctx(), rec.ID)
		// Default exchanges carry the 8-byte response body {"id":N}.
		const wantBytes = 8 + (100 + 8) + 2 + 8
		if got.ExchangeCount != 5 || got.ErrorCount != 3 || got.Bytes != wantBytes {
			t.Errorf("after batch 2: count %d errors %d bytes %d, want 5/3/%d", got.ExchangeCount, got.ErrorCount, got.Bytes, wantBytes)
		}
		if !got.FirstAt.Equal(t05) || !got.LastAt.Equal(t30) {
			t.Errorf("after batch 2: first/last = %v / %v, want %v / %v", got.FirstAt, got.LastAt, t05, t30)
		}
		if !got.UpdatedAt.Equal(clock.Now()) {
			t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, clock.Now())
		}
	})
}

func TestAppendExchangesWithoutTimestampsLeavesRangeEmpty(t *testing.T) {
	s := openMem(t)
	rec := newRecording(t, s, "r")
	appendN(t, s, rec.ID, 2, withStart(time.Time{}))
	got, _ := s.GetRecording(ctx(), rec.ID)
	if got.FirstAt != nil || got.LastAt != nil {
		t.Errorf("first/last = %v / %v, want nil", got.FirstAt, got.LastAt)
	}
	if got.ExchangeCount != 2 {
		t.Errorf("count = %d", got.ExchangeCount)
	}
	ex, err := s.GetExchange(ctx(), 1)
	if err != nil || !ex.StartedAt.IsZero() {
		t.Errorf("zero StartedAt round trip: %v, %v", ex.StartedAt, err)
	}
}

func TestAppendExchangesErrors(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		rec := newRecording(t, s, "r")

		t.Run("unknown recording", func(t *testing.T) {
			ex := mkEx(0)
			err := s.AppendExchanges(ctx(), "rec_nope", []*model.Exchange{ex})
			if !errors.Is(err, model.ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
			if ex.ID != 0 || ex.Seq != 0 || ex.RecordingID != "" {
				t.Errorf("failed append modified the exchange: %+v", ex)
			}
		})

		t.Run("nil exchange rejects the whole batch", func(t *testing.T) {
			good := mkEx(0)
			err := s.AppendExchanges(ctx(), rec.ID, []*model.Exchange{good, nil})
			if err == nil {
				t.Fatal("nil exchange accepted")
			}
			if good.ID != 0 {
				t.Errorf("exchange before the nil was assigned ID %d", good.ID)
			}
			if got, _ := s.GetRecording(ctx(), rec.ID); got.ExchangeCount != 0 {
				t.Errorf("partial batch written: %d exchanges", got.ExchangeCount)
			}
		})

		t.Run("empty batch is a no-op", func(t *testing.T) {
			if err := s.AppendExchanges(ctx(), rec.ID, nil); err != nil {
				t.Errorf("nil batch: %v", err)
			}
			if err := s.AppendExchanges(ctx(), rec.ID, []*model.Exchange{}); err != nil {
				t.Errorf("empty batch: %v", err)
			}
		})
	})
}

// TestAppendExchangesIsAtomic makes the database reject the last row of a
// batch and checks that none of the batch (nor its counters) is kept.
func TestAppendExchangesIsAtomic(t *testing.T) {
	s := openMem(t)
	rec := newRecording(t, s, "r")
	appendN(t, s, rec.ID, 2)

	// A trigger that rejects one specific path makes the third insert fail.
	if _, err := s.wdb.Exec(`CREATE TRIGGER reject_poison BEFORE INSERT ON exchanges
		WHEN NEW.path = '/poison' BEGIN SELECT RAISE(ABORT, 'poisoned'); END`); err != nil {
		t.Fatal(err)
	}
	batch := []*model.Exchange{mkEx(10), mkEx(11), mkEx(12, withPath("/poison", ""))}
	err := s.AppendExchanges(ctx(), rec.ID, batch)
	if err == nil || !strings.Contains(err.Error(), "poisoned") {
		t.Fatalf("err = %v, want the trigger's failure", err)
	}
	for i, ex := range batch {
		if ex.ID != 0 || ex.Seq != 0 {
			t.Errorf("exchange %d was assigned ID/Seq despite the failure: %d/%d", i, ex.ID, ex.Seq)
		}
	}
	got, _ := s.GetRecording(ctx(), rec.ID)
	if got.ExchangeCount != 2 {
		t.Errorf("counters moved: ExchangeCount = %d, want 2", got.ExchangeCount)
	}
	if n := countRows(t, s, "exchanges"); n != 2 {
		t.Errorf("%d exchange rows, want 2", n)
	}

	// The sequence continues without a gap after the rollback.
	if _, err := s.wdb.Exec(`DROP TRIGGER reject_poison`); err != nil {
		t.Fatal(err)
	}
	next := []*model.Exchange{mkEx(20)}
	if err := s.AppendExchanges(ctx(), rec.ID, next); err != nil || next[0].Seq != 3 {
		t.Errorf("append after rollback: seq %d, %v; want 3", next[0].Seq, err)
	}
}

func TestExchangeRoundTrip(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		rec := newRecording(t, s, "r")
		in := &model.Exchange{
			ServiceID: "svc_9",
			StartedAt: baseTime,
			Duration:  model.Duration(123456789 * time.Nanosecond),
			TTFB:      model.Duration(7 * time.Millisecond),
			Endpoint:  "POST /orders/{id}/items",
			Request: model.Request{
				Method: "POST", Scheme: "https", Host: "api.example.com:8443",
				Path: "/orders/42/items", RawQuery: "expand=lines&q=a%20b", Proto: "HTTP/2.0",
				Header: http.Header{
					"Content-Type": {"application/json"},
					"X-Multi":      {"one", "two"},
					"X-Unicode":    {"héllo ✓"},
				},
				Body: []byte(`{"sku":"A-1","qty":2}`), BodySize: 5000, BodyTruncated: true,
				ContentEncoding: "gzip",
			},
			Response: model.Response{
				Status: 201, Proto: "HTTP/2.0",
				Header: http.Header{
					"Content-Type": {"application/json; charset=utf-8"},
					"Set-Cookie":   {"a=1; Path=/", "b=2"},
				},
				Body: []byte(`{"ok":true}`), BodySize: 11, BodyTruncated: false,
				ContentEncoding: "br",
			},
			Error:      "",
			Anonymized: true,
		}
		if err := s.AppendExchanges(ctx(), rec.ID, []*model.Exchange{in}); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetExchange(ctx(), in.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := in.Clone()
		want.StartedAt = want.StartedAt.UTC()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
		}
		// The summary form derives from the same data.
		items, _, err := s.ListExchanges(ctx(), rec.ID, model.ExchangeQuery{})
		if err != nil || len(items) != 1 || !reflect.DeepEqual(items[0], want.Summary()) {
			t.Errorf("summary = %+v, %v; want %+v", items, err, want.Summary())
		}
		if items[0].ContentType != "application/json; charset=utf-8" {
			t.Errorf("content type = %q", items[0].ContentType)
		}
	})
}

func TestExchangeEdgeValues(t *testing.T) {
	s := openMem(t)
	rec := newRecording(t, s, "r")
	exs := []*model.Exchange{
		// Nothing but defaults.
		{},
		// Error exchange without a response.
		{Endpoint: "GET /x", Request: model.Request{Method: "GET", Path: "/x"}, Error: "dial tcp: connection refused",
			Response: model.Response{Status: 502}},
		// Negative and huge durations, far-future time.
		{StartedAt: time.Date(2200, 1, 1, 0, 0, 0, 1, time.UTC), Duration: model.Duration(1<<62 - 1), TTFB: -5,
			Request: model.Request{BodySize: 1 << 40}, Response: model.Response{BodySize: 1 << 41}},
		// Headers with nil and empty value lists.
		{Request: model.Request{Header: http.Header{"X-Empty": {}, "X-Nil": nil}}},
	}
	if err := s.AppendExchanges(ctx(), rec.ID, exs); err != nil {
		t.Fatal(err)
	}
	for i, in := range exs {
		got, err := s.GetExchange(ctx(), in.ID)
		if err != nil {
			t.Fatalf("exchange %d: %v", i, err)
		}
		want := in.Clone()
		if !want.StartedAt.IsZero() {
			want.StartedAt = want.StartedAt.UTC()
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("exchange %d mismatch:\n got %+v\nwant %+v", i, got, want)
		}
	}
}

func TestGetExchangeNotFound(t *testing.T) {
	s := openMem(t)
	for _, id := range []int64{0, 1, -1, 1 << 40} {
		if _, err := s.GetExchange(ctx(), id); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("GetExchange(%d) = %v, want ErrNotFound", id, err)
		}
	}
}

func TestExchangeIDsNeverReused(t *testing.T) {
	s := openMem(t)
	r1 := newRecording(t, s, "one")
	exs := appendN(t, s, r1.ID, 3)
	lastID := exs[2].ID
	if err := s.DeleteRecording(ctx(), r1.ID); err != nil {
		t.Fatal(err)
	}
	r2 := newRecording(t, s, "two")
	next := appendN(t, s, r2.ID, 1)
	if next[0].ID <= lastID {
		t.Errorf("ID %d reused or went backwards (previous max %d)", next[0].ID, lastID)
	}
}

// TestBodyRoundTripAndCompression covers boundary, large, binary and
// compressible bodies, and checks how they are physically stored.
func TestBodyRoundTripAndCompression(t *testing.T) {
	allBytes := make([]byte, 0, 256*8)
	for range 8 {
		for b := range 256 {
			allBytes = append(allBytes, byte(b))
		}
	}
	text := []byte(strings.Repeat(`{"name":"mirage","values":[1,2,3,4,5]}`, 100))

	cases := []struct {
		name     string
		body     []byte
		wantGzip bool
	}{
		{"nil", nil, false},
		{"empty", []byte{}, false},
		{"one byte", []byte{0}, false},
		{"just NULs", make([]byte, 100), false},
		{"exactly 1 KiB compressible", bytes.Repeat([]byte("a"), 1024), false}, // not above the threshold
		{"1 KiB + 1 compressible", bytes.Repeat([]byte("a"), 1025), true},
		{"1 KiB + 1 random", randomBytes(1, 1025), false}, // gzip would not shrink it
		{"all byte values", allBytes, true},               // 8x repeated -> compresses
		{"json text", text, true},
		{"3 MiB compressible", bytes.Repeat([]byte("mirage replay "), 3<<20/14), true},
		{"5 MiB random", randomBytes(2, 5<<20), false},
		{"64 KiB random", randomBytes(3, 64<<10), false},
		{"random then zeros", append(randomBytes(4, 20<<10), make([]byte, 200<<10)...), true}, // prefix sniff must not skip it
		{"invalid utf-8", []byte{0xff, 0xfe, 0xfd, 'a', 'b', 0x00, 0xc3, 0x28}, false},
	}
	backends(t, func(t *testing.T, s *SQLite) {
		rec := newRecording(t, s, "bodies")
		exs := make([]*model.Exchange, len(cases))
		for i, c := range cases {
			exs[i] = mkEx(i, withReqBody(c.body), withRespBody(c.body))
		}
		if err := s.AppendExchanges(ctx(), rec.ID, exs); err != nil {
			t.Fatal(err)
		}
		for i, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				got, err := s.GetExchange(ctx(), exs[i].ID)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got.Request.Body, c.body) || !bytes.Equal(got.Response.Body, c.body) {
					t.Fatalf("body mismatch: req %d bytes, resp %d bytes, want %d", len(got.Request.Body), len(got.Response.Body), len(c.body))
				}
				if len(c.body) == 0 && (got.Request.Body != nil || got.Response.Body != nil) {
					t.Errorf("empty body came back non-nil")
				}

				var flags, reqLen, respLen int
				err = s.rdb.QueryRow(`SELECT body_flags, COALESCE(length(req_body), 0), COALESCE(length(resp_body), 0) FROM exchanges WHERE id = ?`,
					exs[i].ID).Scan(&flags, &reqLen, &respLen)
				if err != nil {
					t.Fatal(err)
				}
				if gz := flags == flagReqGzip|flagRespGzip; gz != c.wantGzip || (!gz && flags != 0) {
					t.Errorf("body_flags = %d, want gzip=%v on both", flags, c.wantGzip)
				}
				switch {
				case c.wantGzip && reqLen >= len(c.body):
					t.Errorf("compressed body is %d bytes, not smaller than the original %d", reqLen, len(c.body))
				case !c.wantGzip && reqLen != len(c.body):
					t.Errorf("stored body is %d bytes, want the original %d", reqLen, len(c.body))
				}
			})
		}
	})
}

func TestPackBody(t *testing.T) {
	small := bytes.Repeat([]byte("a"), compressMinSize)
	if out, gz := packBody(small); gz || len(out) != len(small) {
		t.Errorf("body at the threshold was compressed")
	}
	big := bytes.Repeat([]byte("abcdefgh"), 1000)
	out, gz := packBody(big)
	if !gz || len(out) >= len(big)/10 {
		t.Fatalf("compressible body: gz=%v, %d -> %d bytes", gz, len(big), len(out))
	}
	back, err := unpackBody(out, true)
	if err != nil || !bytes.Equal(back, big) {
		t.Fatalf("unpack: %v, equal=%v", err, bytes.Equal(back, big))
	}
	if _, err := unpackBody([]byte("definitely not gzip"), true); err == nil {
		t.Error("unpacking garbage succeeded")
	}
	// The input slice is never modified.
	orig := bytes.Repeat([]byte("xyz"), 2000)
	cp := append([]byte(nil), orig...)
	packBody(orig)
	if !bytes.Equal(orig, cp) {
		t.Error("packBody modified its input")
	}
}

func TestPercentile(t *testing.T) {
	seq := func(n int) []int64 {
		s := make([]int64, n)
		for i := range s {
			s[i] = int64(i + 1)
		}
		return s
	}
	for _, c := range []struct {
		n, p int
		want int64
	}{
		{1, 50, 1}, {1, 99, 1},
		{2, 50, 1}, {2, 51, 2}, {2, 99, 2},
		{10, 50, 5}, {10, 95, 10}, {10, 99, 10},
		{100, 50, 50}, {100, 95, 95}, {100, 99, 99}, {100, 100, 100},
		{101, 50, 51}, {1000, 99, 990},
	} {
		if got := percentile(seq(c.n), c.p); got != c.want {
			t.Errorf("percentile(1..%d, p%d) = %d, want %d", c.n, c.p, got, c.want)
		}
	}
}

func TestEndpointStats(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		rec := newRecording(t, s, "stats")
		var exs []*model.Exchange
		// GET /a: 10 requests, 1..10 ms, two 500s and one 404.
		for i := 1; i <= 10; i++ {
			status := 200
			switch i {
			case 3, 7:
				status = 500
			case 5:
				status = 404
			}
			exs = append(exs, mkEx(i, withEndpoint("GET /a"), withDuration(time.Duration(i)*time.Millisecond), withStatus(status)))
		}
		// GET /b: 100 requests, 1..100 ms, mix of 200, 429 and a transport error with status 200 recorded.
		for i := 1; i <= 100; i++ {
			opts := []exOpt{withEndpoint("GET /b"), withDuration(time.Duration(i) * time.Millisecond)}
			switch {
			case i%25 == 0:
				opts = append(opts, withStatus(429))
			case i == 10:
				opts = append(opts, withError("reset")) // error with status 200 counts as an error
			}
			exs = append(exs, mkEx(i, opts...))
		}
		// POST /c: a single request.
		exs = append(exs, mkEx(1, withEndpoint("POST /c"), withMethod("POST"), withDuration(42*time.Millisecond), withStatus(0)))
		// Same count as POST /c, sorts after it alphabetically.
		exs = append(exs, mkEx(2, withEndpoint("POST /d"), withDuration(time.Millisecond)))
		if err := s.AppendExchanges(ctx(), rec.ID, exs); err != nil {
			t.Fatal(err)
		}

		stats, err := s.EndpointStats(ctx(), rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		ms := func(n float64) model.Duration { return model.Duration(n * float64(time.Millisecond)) }
		want := []model.EndpointStat{
			{Endpoint: "GET /b", Count: 100, Errors: 5, ErrorRate: 0.05, Avg: ms(50.5), P50: ms(50), P95: ms(95), P99: ms(99), Max: ms(100),
				StatusCounts: map[string]int64{"200": 96, "429": 4}},
			{Endpoint: "GET /a", Count: 10, Errors: 2, ErrorRate: 0.2, Avg: ms(5.5), P50: ms(5), P95: ms(10), P99: ms(10), Max: ms(10),
				StatusCounts: map[string]int64{"200": 7, "404": 1, "500": 2}},
			{Endpoint: "POST /c", Count: 1, Errors: 1, ErrorRate: 1, Avg: ms(42), P50: ms(42), P95: ms(42), P99: ms(42), Max: ms(42),
				StatusCounts: map[string]int64{"0": 1}},
			{Endpoint: "POST /d", Count: 1, Errors: 0, ErrorRate: 0, Avg: ms(1), P50: ms(1), P95: ms(1), P99: ms(1), Max: ms(1),
				StatusCounts: map[string]int64{"200": 1}},
		}
		if !reflect.DeepEqual(stats, want) {
			for i := range max(len(stats), len(want)) {
				if i >= len(stats) || i >= len(want) || !reflect.DeepEqual(stats[i], want[i]) {
					t.Errorf("stat %d:\n got %+v\nwant %+v", i, at(stats, i), at(want, i))
				}
			}
		}
	})
}

func at(s []model.EndpointStat, i int) any {
	if i < len(s) {
		return s[i]
	}
	return "<missing>"
}

func TestEndpointStatsEmptyAndUnknown(t *testing.T) {
	s := openMem(t)
	rec := newRecording(t, s, "empty")
	stats, err := s.EndpointStats(ctx(), rec.ID)
	if err != nil || stats == nil || len(stats) != 0 {
		t.Errorf("empty recording: %v, %v; want empty non-nil", stats, err)
	}
	if _, err := s.EndpointStats(ctx(), "rec_nope"); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown recording: %v", err)
	}
}

func TestEndpointStatsIsScopedToRecording(t *testing.T) {
	s := openMem(t)
	a, b := newRecording(t, s, "a"), newRecording(t, s, "b")
	appendN(t, s, a.ID, 5)
	appendN(t, s, b.ID, 2)
	stats, _ := s.EndpointStats(ctx(), a.ID)
	if len(stats) != 1 || stats[0].Count != 5 {
		t.Errorf("stats for a = %+v", stats)
	}
	if got := fmt.Sprint(stats[0].Endpoint); got != "GET /items/{id}" {
		t.Errorf("endpoint = %q", got)
	}
}

// TestCorruptRowsReturnErrors damages stored data behind the store's back and
// checks that reads fail with an error (and list views, which never decode
// headers or bodies, keep working).
func TestCorruptRowsReturnErrors(t *testing.T) {
	s := openMem(t)
	rec := newRecording(t, s, "corrupt")
	exs := appendN(t, s, rec.ID, 3, withReqBody(bytes.Repeat([]byte("z"), 4000)))

	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.wdb.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`UPDATE exchanges SET req_headers = 'not json' WHERE id = ?`, exs[0].ID)
	mustExec(`UPDATE exchanges SET body_flags = ?, req_body = X'0102030405' WHERE id = ?`, flagReqGzip, exs[1].ID)

	for _, id := range []int64{exs[0].ID, exs[1].ID} {
		if _, err := s.GetExchange(ctx(), id); err == nil || errors.Is(err, model.ErrNotFound) {
			t.Errorf("GetExchange(%d) on a corrupt row = %v, want a decoding error", id, err)
		}
	}
	if _, err := s.GetExchange(ctx(), exs[2].ID); err != nil {
		t.Errorf("healthy neighbour unreadable: %v", err)
	}
	err := s.IterateExchanges(ctx(), rec.ID, model.IterOptions{RequestBodies: true}, func(*model.Exchange) error { return nil })
	if err == nil {
		t.Error("IterateExchanges passed over a corrupt body")
	}
	// Corrupt headers are only noticed by readers that decode them.
	if items, total, err := s.ListExchanges(ctx(), rec.ID, model.ExchangeQuery{}); err != nil || total != 3 || len(items) != 3 {
		t.Errorf("ListExchanges with corrupt rows: %d items, total %d, %v", len(items), total, err)
	}

	svc, err := s.PutService(ctx(), model.Service{Name: "x", ListenAddr: ":1"})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(`UPDATE services SET data = 'garbage' WHERE id = ?`, svc.ID)
	if _, err := s.GetService(ctx(), svc.ID); err == nil {
		t.Error("GetService decoded garbage")
	}
	if _, err := s.ListServices(ctx()); err == nil {
		t.Error("ListServices decoded garbage")
	}
}
