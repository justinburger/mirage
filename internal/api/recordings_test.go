package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/har"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/ops"
)

func TestRecordingCRUD(t *testing.T) {
	e := newEnv(t)

	_, raw := e.do("GET", "/api/v1/recordings", nil)
	if strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("empty list = %s", raw)
	}

	var rec model.Recording
	e.call("POST", "/api/v1/recordings", map[string]any{"name": "  checkout  ", "description": "d", "labels": map[string]string{"env": "stg"}}, http.StatusCreated, &rec)
	if rec.ID == "" || rec.Name != "checkout" || rec.Description != "d" || rec.Labels["env"] != "stg" || rec.Source != "proxy" || rec.ExchangeCount != 0 {
		t.Fatalf("created = %+v", rec)
	}

	var got model.Recording
	e.call("GET", "/api/v1/recordings/"+rec.ID, nil, http.StatusOK, &got)
	if got.ID != rec.ID {
		t.Errorf("GET = %+v", got)
	}

	t.Run("patch changes only what is present", func(t *testing.T) {
		patch := func(body map[string]any) model.Recording {
			t.Helper()
			var out model.Recording // fresh: omitted JSON members must not keep old values
			e.call("PATCH", "/api/v1/recordings/"+rec.ID, body, http.StatusOK, &out)
			return out
		}
		if out := patch(map[string]any{"name": "renamed"}); out.Name != "renamed" || out.Description != "d" || out.Labels["env"] != "stg" {
			t.Errorf("after name patch: %+v", out)
		}
		if out := patch(map[string]any{"description": ""}); out.Description != "" || out.Name != "renamed" || out.Labels["env"] != "stg" {
			t.Errorf("after description patch: %+v", out)
		}
		if out := patch(map[string]any{"labels": map[string]string{}}); len(out.Labels) != 0 || out.Name != "renamed" {
			t.Errorf("labels not cleared: %+v", out)
		}
		if out := patch(map[string]any{"labels": map[string]string{"a": "b"}}); out.Labels["a"] != "b" || out.Name != "renamed" {
			t.Errorf("after labels patch: %+v", out)
		}
		if out := patch(map[string]any{"labels": nil}); out.Labels["a"] != "b" {
			t.Errorf("null labels must leave them alone: %+v", out)
		}
	})

	t.Run("echo fields are accepted, other fields are not", func(t *testing.T) {
		body := asMap(t, mustJSON(t, got))
		body["name"] = "echoed"
		var out model.Recording
		// A recording fetched from the API has read-only members (counters,
		// source): only the documented echo fields may come back.
		e.errorOf("PATCH", "/api/v1/recordings/"+rec.ID, body, http.StatusBadRequest)
		e.call("PATCH", "/api/v1/recordings/"+rec.ID, map[string]any{"name": "echoed", "created_at": "2001-01-01T00:00:00Z", "updated_at": "2001-01-01T00:00:00Z"}, http.StatusOK, &out)
		if out.Name != "echoed" || out.CreatedAt.Year() < 2020 {
			t.Errorf("out = %+v", out)
		}
	})

	t.Run("validation", func(t *testing.T) {
		long := strings.Repeat("n", maxNameLen+1)
		for _, tc := range []struct {
			method, path string
			body         any
		}{
			{"POST", "/api/v1/recordings", map[string]any{}},
			{"POST", "/api/v1/recordings", map[string]any{"name": "   "}},
			{"POST", "/api/v1/recordings", map[string]any{"name": long}},
			{"POST", "/api/v1/recordings", map[string]any{"name": "x", "bogus": 1}},
			{"POST", "/api/v1/recordings", map[string]any{"name": "x", "labels": map[string]string{"": "v"}}},
			{"PATCH", "/api/v1/recordings/" + rec.ID, map[string]any{"name": ""}},
			{"PATCH", "/api/v1/recordings/" + rec.ID, map[string]any{"name": long}},
			{"PATCH", "/api/v1/recordings/" + rec.ID, map[string]any{"description": strings.Repeat("d", 10_001)}},
		} {
			e.errorOf(tc.method, tc.path, tc.body, http.StatusBadRequest)
		}
	})

	t.Run("not found", func(t *testing.T) {
		e.errorOf("GET", "/api/v1/recordings/rec_nope", nil, http.StatusNotFound)
		e.errorOf("PATCH", "/api/v1/recordings/rec_nope", map[string]any{"name": "x"}, http.StatusNotFound)
		e.errorOf("DELETE", "/api/v1/recordings/rec_nope", nil, http.StatusNotFound)
		e.errorOf("GET", "/api/v1/recordings/rec_nope/endpoints", nil, http.StatusNotFound)
		e.errorOf("GET", "/api/v1/recordings/rec_nope/exchanges", nil, http.StatusNotFound)
		e.errorOf("POST", "/api/v1/recordings/rec_nope/anonymize", nil, http.StatusNotFound)
		e.errorOf("GET", "/api/v1/recordings/rec_nope/export.har", nil, http.StatusNotFound)
	})

	t.Run("delete", func(t *testing.T) {
		resp, body := e.do("DELETE", "/api/v1/recordings/"+rec.ID, nil)
		if resp.StatusCode != http.StatusNoContent || len(body) != 0 {
			t.Fatalf("DELETE = %d %q", resp.StatusCode, body)
		}
		e.errorOf("GET", "/api/v1/recordings/"+rec.ID, nil, http.StatusNotFound)
	})
}

func TestListRecordingsNewestFirst(t *testing.T) {
	e := newEnv(t)
	a := e.seedRecording("a", 1)
	time.Sleep(2 * time.Millisecond)
	b := e.seedRecording("b", 1)
	var recs []model.Recording
	e.call("GET", "/api/v1/recordings", nil, http.StatusOK, &recs)
	if len(recs) != 2 || recs[0].ID != b.ID || recs[1].ID != a.ID {
		t.Errorf("order = %v", recs)
	}
	if recs[0].ExchangeCount != 1 {
		t.Errorf("counters missing: %+v", recs[0])
	}
}

func TestDeleteRecordingReferentialIntegrity(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(e *env, rec model.Recording)
		wantErr string // empty = delete succeeds
	}{
		{name: "unused", setup: func(e *env, rec model.Recording) {}},
		{
			name: "enabled virtualize service",
			setup: func(e *env, rec model.Recording) {
				e.seedService(model.Service{Name: "v", Mode: model.ModeVirtualize, RecordingID: rec.ID, Enabled: true})
			},
			wantErr: `enabled service "v"`,
		},
		{
			name: "enabled record service",
			setup: func(e *env, rec model.Recording) {
				e.seedService(model.Service{Name: "r", Mode: model.ModeRecord, RecordingID: rec.ID, Enabled: true})
			},
			wantErr: `enabled service "r"`,
		},
		{
			name: "enabled hybrid service",
			setup: func(e *env, rec model.Recording) {
				e.seedService(model.Service{Name: "h", Mode: model.ModeHybrid, RecordingID: rec.ID, Enabled: true})
			},
			wantErr: `enabled service "h"`,
		},
		{
			name: "disabled service",
			setup: func(e *env, rec model.Recording) {
				e.seedService(model.Service{Name: "d", Mode: model.ModeRecord, RecordingID: rec.ID})
			},
		},
		{
			name: "enabled passthrough service keeps a recording id but does not use it",
			setup: func(e *env, rec model.Recording) {
				e.seedService(model.Service{Name: "p", Mode: model.ModePassthrough, RecordingID: rec.ID, Enabled: true})
			},
		},
		{
			name: "enabled service on another recording does not matter",
			setup: func(e *env, rec model.Recording) {
				e.seedService(model.Service{Name: "o", Mode: model.ModeRecord, RecordingID: "rec_other", Enabled: true})
			},
			wantErr: "",
		},
		{
			name:    "pending replay",
			setup:   func(e *env, rec model.Recording) { e.seedReplay("p", rec.ID, model.ReplayPending) },
			wantErr: "pending replay",
		},
		{
			name:    "running replay",
			setup:   func(e *env, rec model.Recording) { e.seedReplay("r", rec.ID, model.ReplayRunning) },
			wantErr: "running replay",
		},
		{name: "completed replay", setup: func(e *env, rec model.Recording) { e.seedReplay("c", rec.ID, model.ReplayCompleted) }},
		{name: "stopped replay", setup: func(e *env, rec model.Recording) { e.seedReplay("s", rec.ID, model.ReplayStopped) }},
		{name: "failed replay", setup: func(e *env, rec model.Recording) { e.seedReplay("f", rec.ID, model.ReplayFailed) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			rec := e.seedRecording("target", 3)
			tc.setup(e, rec)
			if tc.wantErr == "" {
				e.call("DELETE", "/api/v1/recordings/"+rec.ID, nil, http.StatusNoContent, nil)
				if _, err := e.st.GetRecording(context.Background(), rec.ID); !errors.Is(err, model.ErrNotFound) {
					t.Errorf("recording survived: %v", err)
				}
				return
			}
			eb := e.errorOf("DELETE", "/api/v1/recordings/"+rec.ID, nil, http.StatusConflict)
			if !strings.Contains(eb.Error, tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", eb.Error, tc.wantErr)
			}
			if _, err := e.st.GetRecording(context.Background(), rec.ID); err != nil {
				t.Errorf("recording was deleted despite the conflict: %v", err)
			}
		})
	}
}

func TestEndpointsAndExchanges(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("traffic", 30)

	var stats []model.EndpointStat
	e.call("GET", "/api/v1/recordings/"+rec.ID+"/endpoints", nil, http.StatusOK, &stats)
	if len(stats) != 1 || stats[0].Endpoint != "POST /users/{id}/orders" || stats[0].Count != 30 || stats[0].Errors != 3 {
		t.Errorf("endpoints = %+v", stats)
	}
	empty := e.seedRecording("empty", 0)
	_, raw := e.do("GET", "/api/v1/recordings/"+empty.ID+"/endpoints", nil)
	if strings.TrimSpace(string(raw)) != "[]" {
		t.Errorf("no endpoints = %s", raw)
	}

	list := func(query string) exchangePage {
		t.Helper()
		var page exchangePage
		e.call("GET", "/api/v1/recordings/"+rec.ID+"/exchanges"+query, nil, http.StatusOK, &page)
		return page
	}
	t.Run("pagination", func(t *testing.T) {
		p := list("?limit=10&offset=5")
		if p.Total != 30 || len(p.Items) != 10 || p.Items[0].Seq != 6 {
			t.Errorf("page = total %d, %d items, first seq %d", p.Total, len(p.Items), p.Items[0].Seq)
		}
		if p := list(""); len(p.Items) != 30 {
			t.Errorf("default limit returned %d items", len(p.Items))
		}
		if p := list("?offset=100"); p.Items == nil || len(p.Items) != 0 || p.Total != 30 {
			t.Errorf("past the end = %+v", p)
		}
	})
	t.Run("desc", func(t *testing.T) {
		p := list("?desc=1&limit=3")
		if p.Items[0].Seq != 30 || p.Items[2].Seq != 28 {
			t.Errorf("desc seqs = %d..%d", p.Items[0].Seq, p.Items[2].Seq)
		}
		if p := list("?desc=true&limit=1"); p.Items[0].Seq != 30 {
			t.Errorf("desc=true seq = %d", p.Items[0].Seq)
		}
	})
	t.Run("filters", func(t *testing.T) {
		tests := []struct {
			query string
			total int64
		}{
			{"?errors=1", 3},
			{"?errors=true", 3},
			{"?status=503", 3},
			{"?status=200", 27},
			{"?status_class=5", 3},
			{"?status_class=2", 27},
			{"?status_class=4", 0},
			{"?method=post", 30},
			{"?method=GET", 0},
			{"?path=/users/1005/", 1},
			{"?path=expand%3Ditems", 30},
			{"?endpoint=" + "POST%20%2Fusers%2F%7Bid%7D%2Forders", 30},
			{"?endpoint=nope", 0},
			{"?method=POST&errors=1&status=503", 3},
		}
		for _, tc := range tests {
			if p := list(tc.query); p.Total != tc.total {
				t.Errorf("%s: total = %d, want %d", tc.query, p.Total, tc.total)
			}
		}
	})
	t.Run("summary shape", func(t *testing.T) {
		_, raw := e.do("GET", "/api/v1/recordings/"+rec.ID+"/exchanges?limit=1", nil)
		var page struct {
			Items []map[string]any `json:"items"`
			Total int              `json:"total"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"id", "seq", "started_at", "duration", "method", "path", "raw_query", "status", "endpoint", "req_bytes", "resp_bytes", "content_type"} {
			if _, ok := page.Items[0][k]; !ok {
				t.Errorf("summary lacks %q: %s", k, raw)
			}
		}
	})
	t.Run("bad parameters", func(t *testing.T) {
		for _, q := range []string{"?limit=abc", "?offset=-1", "?limit=-5", "?status=700", "?status_class=9", "?status=x", "?offset=1.5"} {
			e.errorOf("GET", "/api/v1/recordings/"+rec.ID+"/exchanges"+q, nil, http.StatusBadRequest)
		}
		eb := e.errorOf("GET", "/api/v1/recordings/"+rec.ID+"/exchanges?limit=x&offset=-1", nil, http.StatusBadRequest)
		if len(eb.Problems) != 2 {
			t.Errorf("problems = %v, want both parameters reported", eb.Problems)
		}
	})
}

func TestGetExchange(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("one", 2)
	var page exchangePage
	e.call("GET", "/api/v1/recordings/"+rec.ID+"/exchanges", nil, http.StatusOK, &page)
	id := page.Items[1].ID

	var view model.ExchangeView
	e.call("GET", fmt.Sprintf("/api/v1/exchanges/%d", id), nil, http.StatusOK, &view)
	if view.ID != id || view.RecordingID != rec.ID || view.Request.Method != "POST" || view.Request.Path != "/users/1001/orders" {
		t.Errorf("view = %+v", view)
	}
	if view.Request.Body.Encoding != "text" || !strings.Contains(view.Request.Body.Data, "jane1@acme.io") {
		t.Errorf("request body = %+v", view.Request.Body)
	}
	if view.Response.Status != 200 || view.Response.Body.Encoding != "text" {
		t.Errorf("response = %+v", view.Response)
	}
	e.errorOf("GET", "/api/v1/exchanges/999999", nil, http.StatusNotFound)
	e.errorOf("GET", "/api/v1/exchanges/abc", nil, http.StatusNotFound)
}

func TestAnonymizeRecording(t *testing.T) {
	e := newEnv(t)
	src := e.seedRecording("payments", 700)

	var prof model.AnonymizationProfile
	e.call("POST", "/api/v1/profiles", map[string]any{"name": "emails only", "detectors": []string{"email"}, "detector_strategy": "hash"}, http.StatusCreated, &prof)

	t.Run("default profile when profile_id is absent", func(t *testing.T) {
		var out model.Recording
		e.call("POST", "/api/v1/recordings/"+src.ID+"/anonymize", map[string]any{"name": "payments (anon)"}, http.StatusCreated, &out)
		if out.Name != "payments (anon)" || out.Source != "copy" || !out.Anonymized || out.ExchangeCount != 700 || out.ErrorCount != src.ErrorCount {
			t.Errorf("copy = %+v", out)
		}
		first := firstExchange(t, e, out.ID)
		if strings.Contains(first, "acme.io") || strings.Contains(first, "Pa$$w0rd!x") || strings.Contains(first, "secret-token") {
			t.Errorf("copy still holds raw values: %s", first)
		}
	})
	t.Run("no body at all", func(t *testing.T) {
		var out model.Recording
		e.call("POST", "/api/v1/recordings/"+src.ID+"/anonymize", nil, http.StatusCreated, &out)
		if out.Name != "payments (anon)" {
			t.Errorf("default name = %q", out.Name)
		}
	})
	t.Run("explicit profile", func(t *testing.T) {
		var out model.Recording
		e.call("POST", "/api/v1/recordings/"+src.ID+"/anonymize", map[string]any{"profile_id": prof.ID}, http.StatusCreated, &out)
		first := firstExchange(t, e, out.ID)
		if strings.Contains(first, "acme.io") {
			t.Errorf("email survived: %s", first)
		}
		if !strings.Contains(first, "Pa$$w0rd!x") {
			t.Errorf("the emails-only profile must not touch passwords: %s", first)
		}
		if !strings.Contains(out.Description, "emails only") {
			t.Errorf("description = %q", out.Description)
		}
	})
	t.Run("errors", func(t *testing.T) {
		before, _ := e.st.ListRecordings(context.Background())
		e.errorOf("POST", "/api/v1/recordings/"+src.ID+"/anonymize", map[string]any{"profile_id": "prof_nope"}, http.StatusNotFound)
		e.errorOf("POST", "/api/v1/recordings/"+src.ID+"/anonymize", map[string]any{"bogus": 1}, http.StatusBadRequest)
		e.errorOf("POST", "/api/v1/recordings/"+src.ID+"/anonymize", map[string]any{"name": strings.Repeat("n", 300)}, http.StatusBadRequest)
		after, _ := e.st.ListRecordings(context.Background())
		if len(after) != len(before) {
			t.Errorf("failed requests left %d recordings", len(after)-len(before))
		}
	})
}

func firstExchange(t *testing.T, e *env, recID string) string {
	t.Helper()
	var page exchangePage
	e.call("GET", "/api/v1/recordings/"+recID+"/exchanges?limit=1", nil, http.StatusOK, &page)
	if len(page.Items) == 0 {
		t.Fatalf("recording %s is empty", recID)
	}
	_, raw := e.do("GET", fmt.Sprintf("/api/v1/exchanges/%d", page.Items[0].ID), nil)
	return string(raw)
}

func TestExportHAR(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("My Orders: Q4/2026", 120)

	resp, raw := e.do("GET", "/api/v1/recordings/"+rec.ID+"/export.har", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	cd := resp.Header.Get("Content-Disposition")
	typ, params, err := mime.ParseMediaType(cd)
	if err != nil || typ != "attachment" || params["filename"] != "My_Orders_Q4_2026.har" {
		t.Errorf("Content-Disposition = %q (%v)", cd, err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	doc, err := har.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not a HAR document: %v", err)
	}
	if len(doc.Log.Entries) != 120 || doc.Log.Creator.Version != "v0.0.0-test" {
		t.Errorf("%d entries, creator %+v", len(doc.Log.Entries), doc.Log.Creator)
	}
	if !strings.Contains(string(raw), "acme.io") {
		t.Error("an export without profile_id must not anonymize")
	}

	t.Run("anonymized on export", func(t *testing.T) {
		_, raw := e.do("GET", "/api/v1/recordings/"+rec.ID+"/export.har?profile_id=default", nil)
		if _, err := har.Decode(bytes.NewReader(raw)); err != nil {
			t.Fatalf("not a HAR document: %v", err)
		}
		for _, secret := range []string{"acme.io", "Pa$$w0rd!x", "secret-token"} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("export contains %q", secret)
			}
		}
		// The stored recording is untouched.
		if !strings.Contains(firstExchange(t, e, rec.ID), "Pa$$w0rd!x") {
			t.Error("the stored recording was changed")
		}
	})
	t.Run("unknown profile fails cleanly, without a download header", func(t *testing.T) {
		resp, body := e.do("GET", "/api/v1/recordings/"+rec.ID+"/export.har?profile_id=prof_nope", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status %d: %s", resp.StatusCode, body)
		}
		if cd := resp.Header.Get("Content-Disposition"); cd != "" {
			t.Errorf("Content-Disposition = %q on an error", cd)
		}
	})
	t.Run("name that sanitizes to nothing falls back to the id", func(t *testing.T) {
		r2 := e.seedRecording("???", 1)
		resp, _ := e.do("GET", "/api/v1/recordings/"+r2.ID+"/export.har", nil)
		_, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
		if params["filename"] != r2.ID+".har" {
			t.Errorf("filename = %q", params["filename"])
		}
	})
}

// --------------------------------------------------------------- import

// harBytes exports a recording of n exchanges to HAR.
func harBytes(t testing.TB, e *env, n int) []byte {
	t.Helper()
	rec := e.seedRecording(fmt.Sprintf("source-%d", n), n)
	var buf bytes.Buffer
	if err := ops.ExportHAR(context.Background(), e.st, rec.ID, &buf, nil, "test"); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func multipartBody(t testing.TB, fields [][2]string, fileName string, file []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	if file != nil {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, fileName))
		h.Set("Content-Type", "application/json")
		pw, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		pw.Write(file)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func TestImportRecordingRawBody(t *testing.T) {
	e := newEnv(t)
	doc := harBytes(t, e, 40)

	var rec model.Recording
	resp, raw := e.send(request{method: "POST", path: "/api/v1/recordings/import?name=" + "from%20devtools", body: doc, header: map[string]string{"Content-Type": "application/json"}})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Name != "from devtools" || rec.Source != "har" || rec.Anonymized || rec.ExchangeCount != 40 {
		t.Errorf("rec = %+v", rec)
	}

	t.Run("default name is import <timestamp>", func(t *testing.T) {
		resp, raw := e.send(request{method: "POST", path: "/api/v1/recordings/import", body: doc})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		var r model.Recording
		_ = json.Unmarshal(raw, &r)
		if !strings.HasPrefix(r.Name, "import 20") {
			t.Errorf("name = %q", r.Name)
		}
	})
	t.Run("anonymize while importing", func(t *testing.T) {
		resp, raw := e.send(request{method: "POST", path: "/api/v1/recordings/import?profile_id=default&name=safe", body: doc})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		var r model.Recording
		_ = json.Unmarshal(raw, &r)
		if !r.Anonymized {
			t.Error("recording not flagged anonymized")
		}
		if first := firstExchange(t, e, r.ID); strings.Contains(first, "acme.io") || strings.Contains(first, "Pa$$w0rd!x") {
			t.Errorf("raw values stored: %s", first)
		}
	})
	t.Run("unknown profile", func(t *testing.T) {
		before, _ := e.st.ListRecordings(context.Background())
		resp, _ := e.send(request{method: "POST", path: "/api/v1/recordings/import?profile_id=prof_nope", body: doc})
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status %d, want 404", resp.StatusCode)
		}
		after, _ := e.st.ListRecordings(context.Background())
		if len(after) != len(before) {
			t.Error("a recording was created")
		}
	})
}

func TestImportRecordingMultipart(t *testing.T) {
	e := newEnv(t)
	doc := harBytes(t, e, 25)

	post := func(fields [][2]string, fileName string, file []byte, query string) (*http.Response, []byte) {
		body, ctype := multipartBody(t, fields, fileName, file)
		return e.send(request{method: "POST", path: "/api/v1/recordings/import" + query, body: body, header: map[string]string{"Content-Type": ctype}})
	}
	decode := func(raw []byte) model.Recording {
		var r model.Recording
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return r
	}

	t.Run("name defaults to the uploaded file name", func(t *testing.T) {
		resp, raw := post(nil, "checkout-flow.har", doc, "")
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		if r := decode(raw); r.Name != "checkout-flow" || r.ExchangeCount != 25 || r.Source != "har" {
			t.Errorf("rec = %+v", r)
		}
	})
	t.Run("file name with a path and upper-case extension", func(t *testing.T) {
		resp, raw := post(nil, `C:\Users\me\Capture 1.HAR`, doc, "")
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		if r := decode(raw); r.Name != "Capture 1" {
			t.Errorf("name = %q", r.Name)
		}
	})
	t.Run("query name wins over the file name", func(t *testing.T) {
		resp, raw := post(nil, "x.har", doc, "?name=explicit")
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		if r := decode(raw); r.Name != "explicit" {
			t.Errorf("name = %q", r.Name)
		}
	})
	t.Run("form fields before the file", func(t *testing.T) {
		resp, raw := post([][2]string{{"name", "from form"}, {"profile_id", "default"}}, "x.har", doc, "")
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		if r := decode(raw); r.Name != "from form" || !r.Anonymized {
			t.Errorf("rec = %+v", r)
		}
	})
	t.Run("missing file part", func(t *testing.T) {
		before, _ := e.st.ListRecordings(context.Background())
		resp, raw := post([][2]string{{"name", "n"}}, "", nil, "")
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "file") {
			t.Errorf("status %d: %s", resp.StatusCode, raw)
		}
		after, _ := e.st.ListRecordings(context.Background())
		if len(after) != len(before) {
			t.Error("a recording was created")
		}
	})
	t.Run("multipart without a boundary", func(t *testing.T) {
		resp, _ := e.send(request{method: "POST", path: "/api/v1/recordings/import", body: doc, header: map[string]string{"Content-Type": "multipart/form-data"}})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status %d, want 400", resp.StatusCode)
		}
	})
}

func TestImportRecordingErrors(t *testing.T) {
	e := newEnv(t)
	doc := harBytes(t, e, 5)
	before, _ := e.st.ListRecordings(context.Background())

	tests := []struct {
		name   string
		body   any
		status int
	}{
		{"empty body", "", 400},
		{"not JSON", "this is not json", 400},
		{"JSON that is not HAR", `{"hello":"world"}`, 400},
		{"truncated HAR", doc[:len(doc)/2], 400},
		{"entry with an unusable URL", `{"log":{"entries":[{"request":{"method":"GET","url":"nonsense"},"response":{"status":200}}]}}`, 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eb := e.errorOf("POST", "/api/v1/recordings/import", tc.body, tc.status)
			if strings.Contains(eb.Error, "ops:") {
				t.Errorf("package prefix leaked: %q", eb.Error)
			}
		})
	}
	after, _ := e.st.ListRecordings(context.Background())
	if len(after) != len(before) {
		t.Errorf("failed imports left %d recordings", len(after)-len(before))
	}
}

func TestImportRecordingSizeLimit(t *testing.T) {
	e := newEnv(t)
	e.srv.importLimit = 2048
	doc := harBytes(t, e, 30)
	before, _ := e.st.ListRecordings(context.Background())
	if len(doc) <= 2048 {
		t.Fatalf("fixture too small: %d bytes", len(doc))
	}
	eb := e.errorOf("POST", "/api/v1/recordings/import", doc, http.StatusRequestEntityTooLarge)
	if !strings.Contains(eb.Error, "2 KiB") {
		t.Errorf("error = %q", eb.Error)
	}
	body, ctype := multipartBody(t, nil, "big.har", doc)
	resp, raw := e.send(request{method: "POST", path: "/api/v1/recordings/import", body: body, header: map[string]string{"Content-Type": ctype}})
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("multipart: status %d: %s", resp.StatusCode, raw)
	}
	after, _ := e.st.ListRecordings(context.Background())
	if len(after) != len(before) {
		t.Errorf("oversized imports left %d recordings", len(after)-len(before))
	}
}

// The import must process the upload as it arrives instead of buffering it:
// half-way through an upload that is still open, the recording already holds
// the entries that were sent.
func TestImportRecordingStreams(t *testing.T) {
	e := newEnv(t)
	src := e.seedRecording("big", 1200)
	var full bytes.Buffer
	if err := ops.ExportHAR(context.Background(), e.st, src.ID, &full, nil, "test"); err != nil {
		t.Fatal(err)
	}
	doc := full.Bytes()
	cut := len(doc) * 3 / 4 // well past the first 500-exchange batch

	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="stream.har"`)
	h.Set("Content-Type", "application/json")
	part, _ := mw.CreatePart(h)
	prefix := append([]byte(nil), form.Bytes()...)
	form.Reset()

	pr, pw := io.Pipe()
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/recordings/import", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := e.ts.Client().Do(req)
		done <- result{resp, err}
	}()
	_ = part

	write := func(b []byte) {
		if _, err := pw.Write(b); err != nil {
			t.Errorf("pipe write: %v", err)
		}
	}
	write(prefix)
	write(doc[:cut])

	// The server has read a good part of the document; its recording should
	// already contain the first batch(es) while the request is still open.
	deadline := time.Now().Add(10 * time.Second)
	var partial int64
	for time.Now().Before(deadline) {
		recs, _ := e.st.ListRecordings(context.Background())
		for _, r := range recs {
			if r.Source == "har" && r.ExchangeCount > partial {
				partial = r.ExchangeCount
			}
		}
		if partial >= 500 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if partial < 500 {
		t.Fatalf("only %d exchanges stored while the upload was still open: the body is being buffered", partial)
	}

	write(doc[cut:])
	write([]byte("\r\n--" + mw.Boundary() + "--\r\n"))
	pw.Close()
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	defer res.resp.Body.Close()
	raw, _ := io.ReadAll(res.resp.Body)
	if res.resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %s", res.resp.StatusCode, raw)
	}
	var rec model.Recording
	_ = json.Unmarshal(raw, &rec)
	if rec.ExchangeCount != 1200 || rec.Name != "stream" {
		t.Errorf("rec = %+v", rec)
	}
}

func TestImportSkipsUnsupportedEntries(t *testing.T) {
	e := newEnv(t)
	doc := `{"log":{"version":"1.2","creator":{"name":"t","version":"1"},"entries":[
	 {"startedDateTime":"2026-01-01T00:00:00Z","time":5,"request":{"method":"GET","url":"https://a.example.com/x"},"response":{"status":200,"content":{"mimeType":"text/plain"}}},
	 {"startedDateTime":"2026-01-01T00:00:01Z","time":5,"request":{"method":"GET","url":"data:text/plain,hi"},"response":{"status":200,"content":{"mimeType":"text/plain"}}}
	]}}`
	var rec model.Recording
	e.call("POST", "/api/v1/recordings/import?name=mixed", doc, http.StatusCreated, &rec)
	if rec.ExchangeCount != 1 || !strings.Contains(rec.Description, "Skipped 1 unsupported entry") {
		t.Errorf("rec = %+v", rec)
	}
}

func TestDownloadAndUploadNames(t *testing.T) {
	for in, want := range map[string]string{
		"payments":               "payments",
		"My Orders: Q4/2026":     "My_Orders_Q4_2026",
		"  spaced  out  ":        "spaced_out",
		"../../etc/passwd":       "etc_passwd",
		"???":                    "FALLBACK",
		"":                       "FALLBACK",
		"ünïcödé-name":           "n_c_d_-name",
		strings.Repeat("a", 200): strings.Repeat("a", 80),
		"trailing...":            "trailing",
		"a.b-c_d":                "a.b-c_d",
	} {
		if got := downloadName(in, "FALLBACK"); got != want {
			t.Errorf("downloadName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"capture.har":        "capture",
		"capture.HAR":        "capture",
		"capture.json":       "capture",
		"capture.txt":        "capture.txt",
		".har":               ".har",
		`C:\dir\x y.har`:     "x y",
		"/tmp/dir/trace.har": "trace",
		"":                   "",
		".":                  "",
	} {
		if got := nameFromUpload(in); got != want {
			t.Errorf("nameFromUpload(%q) = %q, want %q", in, got, want)
		}
	}
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
