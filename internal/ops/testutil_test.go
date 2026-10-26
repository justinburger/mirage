package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/har"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
	"github.com/justinburger/mirage/internal/store"
)

var baseTime = time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)

// rawSecrets are values that must not survive anonymization with the default
// profile. mkExchange puts every one of them into each exchange.
var rawSecrets = []string{
	"acme.io", "Pa$$w0rd!x", "4111111111111111", "secret-token-", "session=abc",
}

func newStore(t testing.TB) *store.SQLite {
	t.Helper()
	st, err := store.Open(store.MemoryPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// mkExchange builds exchange i of a test recording: a POST with credentials,
// an email, a card number and a name in the request, and the same PII in the
// response. Every tenth exchange is a 503.
func mkExchange(i int) *model.Exchange {
	reqBody := fmt.Sprintf(`{"customer_id":"cust-%d","email":"jane%d@acme.io","password":"Pa$$w0rd!x","card":"4111111111111111"}`, i, i)
	status := 200
	if i%10 == 0 {
		status = 503
	}
	respBody := fmt.Sprintf(`{"id":%d,"email":"jane%d@acme.io","first_name":"Jane"}`, i, i)
	path := fmt.Sprintf("/users/%d/orders", 1000+i)
	return &model.Exchange{
		ServiceID: "svc_test",
		StartedAt: baseTime.Add(time.Duration(i) * 100 * time.Millisecond),
		Duration:  model.Duration(time.Duration(i%50+1) * time.Millisecond),
		TTFB:      model.Duration(time.Duration(i%50+1) * time.Millisecond / 2),
		Endpoint:  pathnorm.Default.Endpoint("POST", path),
		Request: model.Request{
			Method:   "POST",
			Scheme:   "http",
			Host:     "localhost:9001",
			Path:     path,
			RawQuery: "expand=items",
			Proto:    "HTTP/1.1",
			Header: http.Header{
				"Content-Type":  {"application/json"},
				"Authorization": {fmt.Sprintf("Bearer secret-token-%d", i)},
				"Cookie":        {"session=abc" + fmt.Sprint(i)},
			},
			Body:     []byte(reqBody),
			BodySize: int64(len(reqBody)),
		},
		Response: model.Response{
			Status: status,
			Proto:  "HTTP/1.1",
			Header: http.Header{
				"Content-Type": {"application/json"},
				"Set-Cookie":   {"session=abc" + fmt.Sprint(i) + "; Path=/"},
			},
			Body:     []byte(respBody),
			BodySize: int64(len(respBody)),
		},
	}
}

// seed creates a recording holding n exchanges built by mkExchange.
func seed(t testing.TB, st store.Store, name string, n int) model.Recording {
	t.Helper()
	ctx := context.Background()
	rec, err := st.CreateRecording(ctx, model.Recording{Name: name, Source: "proxy", Labels: map[string]string{"env": "test"}})
	if err != nil {
		t.Fatalf("CreateRecording: %v", err)
	}
	for start := 0; start < n; start += 400 {
		end := min(start+400, n)
		batch := make([]*model.Exchange, 0, end-start)
		for i := start; i < end; i++ {
			batch = append(batch, mkExchange(i))
		}
		if err := st.AppendExchanges(ctx, rec.ID, batch); err != nil {
			t.Fatalf("AppendExchanges: %v", err)
		}
	}
	rec, err = st.GetRecording(ctx, rec.ID)
	if err != nil {
		t.Fatalf("GetRecording: %v", err)
	}
	return rec
}

// loadAll reads every exchange of a recording, with bodies.
func loadAll(t testing.TB, st store.Store, recID string) []*model.Exchange {
	t.Helper()
	var out []*model.Exchange
	err := st.IterateExchanges(context.Background(), recID, model.IterOptions{RequestBodies: true, ResponseBodies: true}, func(ex *model.Exchange) error {
		out = append(out, ex)
		return nil
	})
	if err != nil {
		t.Fatalf("IterateExchanges(%s): %v", recID, err)
	}
	return out
}

// dump renders everything of an exchange that anonymization may touch.
func dump(ex *model.Exchange) string {
	var b strings.Builder
	b.WriteString(ex.Request.Path + "?" + ex.Request.RawQuery + "\n")
	for _, h := range []http.Header{ex.Request.Header, ex.Response.Header} {
		for _, k := range slices.Sorted(maps.Keys(h)) {
			b.WriteString(k + ": " + strings.Join(h[k], ",") + "\n")
		}
		b.WriteString("--\n")
	}
	b.Write(ex.Request.Body)
	b.WriteByte('\n')
	b.Write(ex.Response.Body)
	return b.String()
}

func recordingCount(t testing.TB, st store.Store) int {
	t.Helper()
	recs, err := st.ListRecordings(context.Background())
	if err != nil {
		t.Fatalf("ListRecordings: %v", err)
	}
	return len(recs)
}

// harDoc marshals a HAR document with the given entries.
func harDoc(t testing.TB, entries ...har.Entry) []byte {
	t.Helper()
	doc := har.HAR{Log: har.Log{Version: "1.2", Creator: har.Creator{Name: "ops-test", Version: "9.9"}, Entries: entries}}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal HAR: %v", err)
	}
	return b
}

// entry builds a HAR entry for a plain GET of url answered with status.
func entry(url string, status int) har.Entry {
	return har.Entry{
		StartedDateTime: har.Timestamp{Time: baseTime},
		Time:            12,
		Request:         har.Request{Method: "GET", URL: url, HTTPVersion: "HTTP/1.1", HeadersSize: -1, BodySize: -1},
		Response:        har.Response{Status: status, StatusText: "x", HTTPVersion: "HTTP/1.1", HeadersSize: -1, BodySize: -1, Content: har.Content{MimeType: "text/plain"}},
		Timings:         har.Timings{Send: 0, Wait: 10, Receive: 2},
	}
}

var errInjected = errors.New("injected failure")

// flakyStore wraps a store and fails selected operations.
type flakyStore struct {
	store.Store
	appends      atomic.Int32
	failAppendOn int32 // fail the nth AppendExchanges call (1-based); 0 = never
	failUpdate   bool
	failDelete   bool
	failList     bool
	failGetProf  func(id string) (model.AnonymizationProfile, bool)
}

func (f *flakyStore) AppendExchanges(ctx context.Context, id string, exs []*model.Exchange) error {
	if n := f.appends.Add(1); f.failAppendOn != 0 && n == f.failAppendOn {
		return errInjected
	}
	return f.Store.AppendExchanges(ctx, id, exs)
}

func (f *flakyStore) UpdateRecording(ctx context.Context, r model.Recording) (model.Recording, error) {
	if f.failUpdate {
		return model.Recording{}, errInjected
	}
	return f.Store.UpdateRecording(ctx, r)
}

func (f *flakyStore) DeleteRecording(ctx context.Context, id string) error {
	if f.failDelete {
		return errInjected
	}
	return f.Store.DeleteRecording(ctx, id)
}

func (f *flakyStore) ListFaultRules(ctx context.Context) ([]model.FaultRule, error) {
	if f.failList {
		return nil, errInjected
	}
	return f.Store.ListFaultRules(ctx)
}

func (f *flakyStore) GetProfile(ctx context.Context, id string) (model.AnonymizationProfile, error) {
	if f.failGetProf != nil {
		if p, ok := f.failGetProf(id); ok {
			return p, nil
		}
	}
	return f.Store.GetProfile(ctx, id)
}

// failWriter fails after n bytes.
type failWriter struct {
	n   int
	buf bytes.Buffer
}

func (w *failWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.n {
		return 0, errInjected
	}
	return w.buf.Write(p)
}
