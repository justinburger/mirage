package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// listFixture appends 30 exchanges with varied methods, statuses, endpoints,
// paths and errors, and returns them in Seq order.
//
//	i      endpoint          path (query)
//	0-9    GET /items/{id}   /items/i
//	10-19  POST /orders      /orders (page=i&sort=asc)
//	20-29  GET /users/{id}   /users/i      (25 has the query name=a%25b_c)
//
// Methods cycle GET, POST, PUT by i. Statuses cycle 200 201 204 301 404 429 500
// 503 200 200, except 14 (504 with an error), 15 (200 with a transport error)
// and 16 (status 0, no response). That gives 10x200, 3x201, 3x204, 3x301,
// 2x404, 2x429, 2x500, 3x503, 1x504 and 1x status 0, and 10 error exchanges
// (5, 6, 7, 14, 15, 16, 17, 25, 26, 27).
func listFixture(t testing.TB, s *SQLite) (model.Recording, []*model.Exchange) {
	t.Helper()
	rec := newRecording(t, s, "list")
	statuses := []int{200, 201, 204, 301, 404, 429, 500, 503, 200, 200}
	exs := make([]*model.Exchange, 30)
	for i := range exs {
		opts := []exOpt{
			withMethod([]string{"GET", "POST", "PUT"}[i%3]),
			withStatus(statuses[i%10]),
		}
		switch {
		case i < 10:
			opts = append(opts, withEndpoint("GET /items/{id}"), withPath(fmt.Sprintf("/items/%d", i), ""))
		case i < 20:
			opts = append(opts, withEndpoint("POST /orders"), withPath("/orders", fmt.Sprintf("page=%d&sort=asc", i)))
		default:
			opts = append(opts, withEndpoint("GET /users/{id}"), withPath(fmt.Sprintf("/users/%d", i), ""))
		}
		switch i {
		case 14:
			opts = append(opts, withStatus(504), withError("upstream timeout"))
		case 15:
			opts = append(opts, withStatus(200), withError("reset by peer"))
		case 16:
			opts = append(opts, withStatus(0), withError("dial tcp: refused"))
		case 25:
			opts = append(opts, withPath("/users/25", "name=a%25b_c"))
		}
		exs[i] = mkEx(i, opts...)
	}
	if err := s.AppendExchanges(ctx(), rec.ID, exs); err != nil {
		t.Fatal(err)
	}
	return rec, exs
}

// oracle computes the expected list result in plain Go.
func oracle(all []*model.Exchange, q model.ExchangeQuery) (ids []int64, total int) {
	var match []*model.Exchange
	for _, e := range all {
		switch {
		case q.Method != "" && !strings.EqualFold(e.Request.Method, q.Method):
		case q.Status != 0 && e.Response.Status != q.Status:
		case q.StatusClass != 0 && e.Response.Status/100 != q.StatusClass:
		case q.PathContains != "" && !strings.Contains(e.Request.URI(), q.PathContains):
		case q.Endpoint != "" && e.Endpoint != q.Endpoint:
		case q.ErrorsOnly && !e.IsError():
		default:
			match = append(match, e)
		}
	}
	total = len(match)
	if q.Desc {
		for i, j := 0, len(match)-1; i < j; i, j = i+1, j-1 {
			match[i], match[j] = match[j], match[i]
		}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 1000)
	off := max(q.Offset, 0)
	for i := off; i < len(match) && i < off+limit; i++ {
		ids = append(ids, match[i].ID)
	}
	return ids, total
}

func TestListExchanges(t *testing.T) {
	cases := []struct {
		name      string
		q         model.ExchangeQuery
		wantTotal int // hand-computed from the fixture, independent of the oracle
	}{
		{"no filter", model.ExchangeQuery{}, 30},
		{"desc", model.ExchangeQuery{Desc: true}, 30},
		{"method GET", model.ExchangeQuery{Method: "GET"}, 10},
		{"method is case-insensitive", model.ExchangeQuery{Method: "put"}, 10},
		{"method with no matches", model.ExchangeQuery{Method: "DELETE"}, 0},
		{"status 200", model.ExchangeQuery{Status: 200}, 10},
		{"status 500", model.ExchangeQuery{Status: 500}, 2},
		{"status 0 means any", model.ExchangeQuery{Status: 0}, 30},
		{"status class 2xx", model.ExchangeQuery{StatusClass: 2}, 16},
		{"status class 3xx", model.ExchangeQuery{StatusClass: 3}, 3},
		{"status class 4xx", model.ExchangeQuery{StatusClass: 4}, 4},
		{"status class 5xx", model.ExchangeQuery{StatusClass: 5}, 6},
		{"status class 1xx", model.ExchangeQuery{StatusClass: 1}, 0},
		{"status and class agree", model.ExchangeQuery{Status: 404, StatusClass: 4}, 2},
		{"status and class disagree", model.ExchangeQuery{Status: 404, StatusClass: 5}, 0},
		{"path substring", model.ExchangeQuery{PathContains: "items"}, 10},
		{"path matches the query string too", model.ExchangeQuery{PathContains: "sort=asc"}, 10},
		{"path across the ? separator", model.ExchangeQuery{PathContains: "/orders?page=12"}, 1},
		{"path with LIKE wildcards is literal", model.ExchangeQuery{PathContains: "a%25b_c"}, 1},
		{"lone percent is literal", model.ExchangeQuery{PathContains: "%"}, 1},
		{"lone underscore is literal", model.ExchangeQuery{PathContains: "_"}, 1},
		{"path is case-sensitive", model.ExchangeQuery{PathContains: "ITEMS"}, 0},
		{"path with no matches", model.ExchangeQuery{PathContains: "nothing"}, 0},
		{"endpoint", model.ExchangeQuery{Endpoint: "POST /orders"}, 10},
		{"endpoint is exact", model.ExchangeQuery{Endpoint: "POST /order"}, 0},
		{"errors only", model.ExchangeQuery{ErrorsOnly: true}, 10},
		{"errors only desc limit 2", model.ExchangeQuery{ErrorsOnly: true, Desc: true, Limit: 2}, 10},
		{"errors and 5xx", model.ExchangeQuery{ErrorsOnly: true, StatusClass: 5}, 6},
		{"method + class + endpoint", model.ExchangeQuery{Method: "GET", StatusClass: 2, Endpoint: "GET /items/{id}"}, 2},
		{"everything at once", model.ExchangeQuery{Method: "POST", StatusClass: 2, PathContains: "page", Endpoint: "POST /orders", Desc: true, Limit: 3}, 2},
		{"limit 5", model.ExchangeQuery{Limit: 5}, 30},
		{"limit 5 offset 5", model.ExchangeQuery{Limit: 5, Offset: 5}, 30},
		{"last partial page", model.ExchangeQuery{Limit: 10, Offset: 25}, 30},
		{"offset equal to total", model.ExchangeQuery{Offset: 30}, 30},
		{"offset past total", model.ExchangeQuery{Offset: 1000}, 30},
		{"negative offset is zero", model.ExchangeQuery{Offset: -4, Limit: 3}, 30},
		{"negative limit is the default", model.ExchangeQuery{Limit: -1}, 30},
		{"limit above the cap", model.ExchangeQuery{Limit: 1 << 20}, 30},
		{"filtered desc page", model.ExchangeQuery{Method: "GET", Desc: true, Limit: 4, Offset: 2}, 10},
	}
	backends(t, func(t *testing.T, s *SQLite) {
		rec, all := listFixture(t, s)
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				items, total, err := s.ListExchanges(ctx(), rec.ID, c.q)
				if err != nil {
					t.Fatal(err)
				}
				wantIDs, wantTotal := oracle(all, c.q)
				if int(total) != wantTotal {
					t.Errorf("total = %d, oracle says %d", total, wantTotal)
				}
				if wantTotal != c.wantTotal {
					t.Errorf("fixture/oracle disagree with the hand-computed total: oracle %d, hand %d", wantTotal, c.wantTotal)
				}
				if items == nil {
					t.Error("items is nil, want a non-nil slice")
				}
				gotIDs := make([]int64, len(items))
				for i, it := range items {
					gotIDs[i] = it.ID
				}
				if len(gotIDs) != len(wantIDs) || (len(gotIDs) > 0 && !reflect.DeepEqual(gotIDs, wantIDs)) {
					t.Errorf("ids = %v, want %v", gotIDs, wantIDs)
				}
			})
		}
	})
}

func TestListExchangesSummaryContent(t *testing.T) {
	s := openMem(t)
	rec, all := listFixture(t, s)
	items, _, err := s.ListExchanges(ctx(), rec.ID, model.ExchangeQuery{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for i, it := range items {
		want := all[i].Summary()
		want.StartedAt = want.StartedAt.UTC()
		if !reflect.DeepEqual(it, want) {
			t.Fatalf("summary %d:\n got %+v\nwant %+v", i, it, want)
		}
	}
	// Paging with every page size reassembles the whole list without
	// overlaps or gaps, ascending and descending.
	for _, desc := range []bool{false, true} {
		for _, size := range []int{1, 4, 7, 29, 30, 31} {
			var got []int64
			for off := 0; ; off += size {
				page, total, err := s.ListExchanges(ctx(), rec.ID, model.ExchangeQuery{Limit: size, Offset: off, Desc: desc})
				if err != nil {
					t.Fatal(err)
				}
				if total != 30 {
					t.Fatalf("total = %d", total)
				}
				if len(page) == 0 {
					break
				}
				for _, it := range page {
					got = append(got, it.ID)
				}
			}
			wantIDs, _ := oracle(all, model.ExchangeQuery{Desc: desc})
			if !reflect.DeepEqual(got, wantIDs) {
				t.Errorf("desc=%v size=%d: pages reassemble to %v, want %v", desc, size, got, wantIDs)
			}
		}
	}
}

func TestListExchangesIsScopedAndChecksRecording(t *testing.T) {
	s := openMem(t)
	a, b := newRecording(t, s, "a"), newRecording(t, s, "b")
	appendN(t, s, a.ID, 3)
	appendN(t, s, b.ID, 5)
	if items, total, err := s.ListExchanges(ctx(), a.ID, model.ExchangeQuery{}); err != nil || total != 3 || len(items) != 3 {
		t.Errorf("a: %d items, total %d, %v", len(items), total, err)
	}
	empty := newRecording(t, s, "empty")
	items, total, err := s.ListExchanges(ctx(), empty.ID, model.ExchangeQuery{ErrorsOnly: true})
	if err != nil || total != 0 || items == nil || len(items) != 0 {
		t.Errorf("empty recording: %v total %d %v", items, total, err)
	}
	if _, _, err := s.ListExchanges(ctx(), "rec_nope", model.ExchangeQuery{}); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown recording: %v", err)
	}
	if _, _, err := s.ListExchanges(ctx(), "rec_nope", model.ExchangeQuery{Status: 500}); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("unknown recording with a filter: %v", err)
	}
}

func TestListExchangesLimits(t *testing.T) {
	s := openMem(t)
	rec := newRecording(t, s, "many")
	exs := make([]*model.Exchange, 1100)
	for i := range exs {
		exs[i] = mkEx(i)
	}
	if err := s.AppendExchanges(ctx(), rec.ID, exs); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ limit, want int }{{0, 100}, {-5, 100}, {1, 1}, {1000, 1000}, {1001, 1000}, {99999, 1000}} {
		items, total, err := s.ListExchanges(ctx(), rec.ID, model.ExchangeQuery{Limit: c.limit})
		if err != nil || len(items) != c.want || total != 1100 {
			t.Errorf("limit %d: got %d items, total %d, %v; want %d items, total 1100", c.limit, len(items), total, err, c.want)
		}
	}
	// Deep pages work.
	items, _, err := s.ListExchanges(ctx(), rec.ID, model.ExchangeQuery{Offset: 1050, Limit: 100})
	if err != nil || len(items) != 50 || items[0].Seq != 1051 {
		t.Errorf("deep page: %d items (first seq %d), %v", len(items), firstSeq(items), err)
	}
}

func firstSeq(items []model.ExchangeSummary) int64 {
	if len(items) == 0 {
		return 0
	}
	return items[0].Seq
}

// explain returns the query plan of a statement as one string.
func explain(t *testing.T, s *SQLite, query string, args ...any) string {
	t.Helper()
	rows, err := s.rdb.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	return strings.Join(plan, " | ")
}

// TestQueriesUseIndexes guards against a schema change that turns the hot
// list and count queries into table scans.
func TestQueriesUseIndexes(t *testing.T) {
	s := openMem(t)
	rec, _ := listFixture(t, s)
	for _, c := range []struct {
		name  string
		query model.ExchangeQuery
		count bool
		index string
	}{
		{"unfiltered page", model.ExchangeQuery{}, false, "exchanges_rec_seq"},
		{"unfiltered desc page", model.ExchangeQuery{Desc: true}, false, "exchanges_rec_seq"},
		{"count by status", model.ExchangeQuery{Status: 500}, true, "exchanges_rec_status"},
		{"count by endpoint", model.ExchangeQuery{Endpoint: "POST /orders"}, true, "exchanges_rec_endpoint"},
		{"count errors", model.ExchangeQuery{ErrorsOnly: true}, true, "exchanges_rec_errors"},
		{"errors page", model.ExchangeQuery{ErrorsOnly: true}, false, "exchanges_rec_errors"},
	} {
		t.Run(c.name, func(t *testing.T) {
			where, args, _ := filterSQL(rec.ID, c.query)
			query := "SELECT " + summaryCols + " FROM exchanges WHERE " + where + " ORDER BY seq LIMIT 100 OFFSET 0"
			if c.count {
				query = "SELECT COUNT(*) FROM exchanges WHERE " + where
			}
			plan := explain(t, s, query, args...)
			if !strings.Contains(plan, c.index) {
				t.Errorf("plan does not use %s: %s", c.index, plan)
			}
			if strings.Contains(plan, "SCAN exchanges") && !strings.Contains(plan, "USING") {
				t.Errorf("full table scan: %s", plan)
			}
		})
	}
	if plan := explain(t, s, getExchangeSQL, 1); strings.Contains(plan, "SCAN") {
		t.Errorf("GetExchange scans the table: %s", plan)
	}
}

// ---- IterateExchanges ----

func collect(t *testing.T, s *SQLite, rec string, opts model.IterOptions) []*model.Exchange {
	t.Helper()
	var out []*model.Exchange
	err := s.IterateExchanges(ctx(), rec, opts, func(e *model.Exchange) error {
		out = append(out, e)
		return nil
	})
	if err != nil {
		t.Fatalf("IterateExchanges: %v", err)
	}
	return out
}

func TestIterateExchangesOrderAcrossChunks(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		rec := newRecording(t, s, "iter")
		const n = iterChunkRows*3 + 17 // several chunks plus a partial one
		exs := make([]*model.Exchange, n)
		for i := range exs {
			exs[i] = mkEx(i)
		}
		if err := s.AppendExchanges(ctx(), rec.ID, exs); err != nil {
			t.Fatal(err)
		}
		got := collect(t, s, rec.ID, model.IterOptions{})
		if len(got) != n {
			t.Fatalf("visited %d exchanges, want %d", len(got), n)
		}
		for i, e := range got {
			if e.Seq != int64(i+1) || e.ID != exs[i].ID || e.RecordingID != rec.ID {
				t.Fatalf("position %d: seq %d id %d (want seq %d id %d)", i, e.Seq, e.ID, i+1, exs[i].ID)
			}
			if e.Request.Body != nil || e.Response.Body != nil {
				t.Fatalf("position %d: bodies loaded although not requested", i)
			}
			if e.Response.BodySize != exs[i].Response.BodySize || e.Request.Path != exs[i].Request.Path {
				t.Fatalf("position %d: metadata not loaded: %+v", i, e)
			}
		}
	})
}

func TestIterateExchangesBodyOptions(t *testing.T) {
	s := openMem(t)
	rec := newRecording(t, s, "bodies")
	big := bytes.Repeat([]byte("compressible "), 400) // gzipped on disk
	exs := []*model.Exchange{
		mkEx(0, withReqBody([]byte("req-0")), withRespBody([]byte("resp-0"))),
		mkEx(1, withReqBody(big), withRespBody(randomBytes(1, 3000))),
		mkEx(2, withReqBody(nil), withRespBody(nil)),
	}
	if err := s.AppendExchanges(ctx(), rec.ID, exs); err != nil {
		t.Fatal(err)
	}
	for _, o := range []model.IterOptions{{}, {RequestBodies: true}, {ResponseBodies: true}, {RequestBodies: true, ResponseBodies: true}} {
		t.Run(fmt.Sprintf("req=%v resp=%v", o.RequestBodies, o.ResponseBodies), func(t *testing.T) {
			got := collect(t, s, rec.ID, o)
			if len(got) != 3 {
				t.Fatalf("visited %d", len(got))
			}
			for i, e := range got {
				wantReq, wantResp := exs[i].Request.Body, exs[i].Response.Body
				if !o.RequestBodies {
					wantReq = nil
				}
				if !o.ResponseBodies {
					wantResp = nil
				}
				if !bytes.Equal(e.Request.Body, wantReq) || (len(wantReq) == 0) != (len(e.Request.Body) == 0) {
					t.Errorf("exchange %d request body = %d bytes, want %d", i, len(e.Request.Body), len(wantReq))
				}
				if !bytes.Equal(e.Response.Body, wantResp) || (len(wantResp) == 0) != (len(e.Response.Body) == 0) {
					t.Errorf("exchange %d response body = %d bytes, want %d", i, len(e.Response.Body), len(wantResp))
				}
			}
		})
	}
	// With both bodies requested the result equals GetExchange.
	got := collect(t, s, rec.ID, model.IterOptions{RequestBodies: true, ResponseBodies: true})
	for i, e := range got {
		want, _ := s.GetExchange(ctx(), exs[i].ID)
		if !reflect.DeepEqual(e, want) {
			t.Errorf("exchange %d differs from GetExchange:\n got %+v\nwant %+v", i, e, want)
		}
	}
}

// TestIterateExchangesBoundsMemory uses bodies large enough that the byte cap,
// not the row cap, ends each chunk.
func TestIterateExchangesBoundsMemory(t *testing.T) {
	s, _ := openFile(t)
	rec := newRecording(t, s, "heavy")
	const n = 14
	bodies := make([][]byte, n)
	exs := make([]*model.Exchange, n)
	for i := range exs {
		bodies[i] = randomBytes(int64(i), 1<<20) // 1 MiB each, incompressible
		exs[i] = mkEx(i, withRespBody(bodies[i]))
	}
	if err := s.AppendExchanges(ctx(), rec.ID, exs); err != nil {
		t.Fatal(err)
	}
	var seq int64
	chunks := 0
	err := s.IterateExchanges(ctx(), rec.ID, model.IterOptions{ResponseBodies: true}, func(e *model.Exchange) error {
		seq++
		if e.Seq != seq || !bytes.Equal(e.Response.Body, bodies[seq-1]) {
			t.Fatalf("exchange %d came back wrong (seq %d)", seq, e.Seq)
		}
		return nil
	})
	if err != nil || seq != n {
		t.Fatalf("visited %d, %v", seq, err)
	}
	// Count chunks directly: the cap is 8 MiB, so 14 MiB needs at least two.
	rows := 0
	for after := int64(0); after < n; {
		chunk, err := s.readChunk(ctx(), iterSQL(model.IterOptions{ResponseBodies: true}), rec.ID, after, n)
		if err != nil {
			t.Fatal(err)
		}
		chunks++
		rows += len(chunk)
		if len(chunk) > 9 {
			t.Errorf("a chunk held %d MiB-sized bodies, want at most 9", len(chunk))
		}
		after = chunk[len(chunk)-1].Seq
	}
	if chunks < 2 || rows != n {
		t.Errorf("%d chunks covering %d rows", chunks, rows)
	}
}

var errStop = errors.New("stop here")

func TestIterateExchangesEarlyStop(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		rec := newRecording(t, s, "stop")
		appendN(t, s, rec.ID, 600)
		for _, stopAt := range []int{1, 3, 256, 257, 600} {
			calls := 0
			err := s.IterateExchanges(ctx(), rec.ID, model.IterOptions{}, func(e *model.Exchange) error {
				calls++
				if calls == stopAt {
					return errStop
				}
				return nil
			})
			if err != errStop { //nolint:errorlint // the contract is that the exact error comes back
				t.Errorf("stopAt %d: err = %v, want exactly errStop", stopAt, err)
			}
			if calls != stopAt {
				t.Errorf("stopAt %d: fn called %d times", stopAt, calls)
			}
		}
		// A wrapped error is returned as-is too.
		wrapped := fmt.Errorf("replay aborted: %w", errStop)
		err := s.IterateExchanges(ctx(), rec.ID, model.IterOptions{}, func(*model.Exchange) error { return wrapped })
		if !errors.Is(err, errStop) {
			t.Errorf("wrapped error lost: %v", err)
		}
	})
}

func TestIterateExchangesContextCancel(t *testing.T) {
	s := openMem(t)
	rec := newRecording(t, s, "cancel")
	appendN(t, s, rec.ID, 600)
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := s.IterateExchanges(cctx, rec.ID, model.IterOptions{}, func(*model.Exchange) error {
		calls++
		if calls == 5 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls != 5 {
		t.Errorf("fn called %d times, want 5", calls)
	}
}

func TestIterateExchangesEmptyAndUnknown(t *testing.T) {
	s := openMem(t)
	rec := newRecording(t, s, "empty")
	called := false
	fn := func(*model.Exchange) error { called = true; return nil }
	if err := s.IterateExchanges(ctx(), rec.ID, model.IterOptions{}, fn); err != nil || called {
		t.Errorf("empty recording: err %v, called %v", err, called)
	}
	if err := s.IterateExchanges(ctx(), "rec_nope", model.IterOptions{}, fn); !errors.Is(err, model.ErrNotFound) || called {
		t.Errorf("unknown recording: err %v, called %v", err, called)
	}
}

// TestIterateExchangesCallbackMayUseStore proves fn can call back into the
// store, including writes, without deadlocking, and that appends made during
// the iteration are not visited.
func TestIterateExchangesCallbackMayUseStore(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		rec := newRecording(t, s, "reentrant")
		other := newRecording(t, s, "other")
		const n = 300
		appendN(t, s, rec.ID, n)

		done := make(chan error, 1)
		visited := 0
		go func() {
			done <- s.IterateExchanges(ctx(), rec.ID, model.IterOptions{}, func(e *model.Exchange) error {
				visited++
				if _, err := s.GetExchange(ctx(), e.ID); err != nil {
					return err
				}
				if _, _, err := s.ListExchanges(ctx(), rec.ID, model.ExchangeQuery{Limit: 1}); err != nil {
					return err
				}
				if visited%100 == 1 {
					// Writes to the same and another recording while iterating.
					if err := s.AppendExchanges(ctx(), rec.ID, []*model.Exchange{mkEx(1000 + visited)}); err != nil {
						return err
					}
					if err := s.AppendExchanges(ctx(), other.ID, []*model.Exchange{mkEx(visited)}); err != nil {
						return err
					}
					return s.IterateExchanges(ctx(), other.ID, model.IterOptions{}, func(*model.Exchange) error { return nil })
				}
				return nil
			})
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("IterateExchanges deadlocked when its callback used the store")
		}
		if visited != n {
			t.Errorf("visited %d exchanges, want %d (appends during iteration must not be visited)", visited, n)
		}
		if got, _ := s.GetRecording(ctx(), rec.ID); got.ExchangeCount != n+3 {
			t.Errorf("recording has %d exchanges, want %d", got.ExchangeCount, n+3)
		}
	})
}

// TestIterateExchangesCallbackMayMutateSeq proves the paging cursor does not
// depend on what fn does to the exchange: passing the visited exchange to
// AppendExchanges rewrites its Seq in place (a copy of a recording into
// another one), and the iteration must still visit each row exactly once.
func TestIterateExchangesCallbackMayMutateSeq(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		src := newRecording(t, s, "src")
		dst := newRecording(t, s, "dst")
		const n = iterChunkRows*2 + 5
		appendN(t, s, src.ID, n)
		visited := 0
		err := s.IterateExchanges(ctx(), src.ID, model.IterOptions{}, func(e *model.Exchange) error {
			visited++
			if visited > 3*n {
				return fmt.Errorf("runaway iteration after %d visits", visited)
			}
			return s.AppendExchanges(ctx(), dst.ID, []*model.Exchange{e}) // rewrites e.Seq, e.ID and e.RecordingID
		})
		if err != nil {
			t.Fatal(err)
		}
		if visited != n {
			t.Fatalf("visited %d exchanges, want %d", visited, n)
		}
		if got, _ := s.GetRecording(ctx(), dst.ID); got.ExchangeCount != n {
			t.Fatalf("destination has %d exchanges, want %d", got.ExchangeCount, n)
		}
	})
}

func TestIterateExchangesRecordingDeletedMidway(t *testing.T) {
	s := openMem(t)
	rec := newRecording(t, s, "doomed")
	appendN(t, s, rec.ID, iterChunkRows*3)
	calls := 0
	err := s.IterateExchanges(ctx(), rec.ID, model.IterOptions{}, func(*model.Exchange) error {
		calls++
		if calls == 1 {
			return s.DeleteRecording(ctx(), rec.ID)
		}
		return nil
	})
	if !errors.Is(err, model.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if calls != iterChunkRows {
		t.Errorf("fn called %d times, want %d (the chunk already read)", calls, iterChunkRows)
	}
}

// TestIterateDoesNotBlockWriters holds an iteration open (callback parked)
// and shows that appends complete meanwhile, which a long-lived read
// transaction could prevent when the WAL has to be checkpointed.
func TestIterateDoesNotBlockWriters(t *testing.T) {
	s, _ := openFile(t)
	rec := newRecording(t, s, "slow")
	other := newRecording(t, s, "live")
	appendN(t, s, rec.ID, 50)

	parked, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	var once atomic.Bool
	go func() {
		finished <- s.IterateExchanges(ctx(), rec.ID, model.IterOptions{}, func(*model.Exchange) error {
			if once.CompareAndSwap(false, true) {
				close(parked)
				<-release
			}
			return nil
		})
	}()
	<-parked
	for range 20 {
		exs := make([]*model.Exchange, 100)
		for i := range exs {
			exs[i] = mkEx(i)
		}
		if err := s.AppendExchanges(ctx(), other.ID, exs); err != nil {
			t.Fatal(err)
		}
	}
	// Force a full checkpoint attempt while the iteration is parked.
	if _, err := s.wdb.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}
