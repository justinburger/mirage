package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
)

func TestCreateService(t *testing.T) {
	e := newEnv(t)
	body := validService("payments", ":9001")
	body["mode"] = "record"
	rec := e.seedRecording("payments rec", 0)
	body["recording_id"] = rec.ID

	resp, raw := e.do("POST", "/api/v1/services", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	m := asMap(t, raw)
	// Service fields are flattened next to "status".
	for _, key := range []string{"id", "name", "listen_addr", "upstream", "mode", "enabled", "recording_id", "match", "latency", "timeout", "created_at", "updated_at", "status"} {
		if _, ok := m[key]; !ok {
			t.Errorf("response lacks %q: %s", key, raw)
		}
	}
	if _, nested := m["Service"]; nested {
		t.Errorf("service must be flattened, got %s", raw)
	}
	id, _ := m["id"].(string)
	if !strings.HasPrefix(id, "svc_") {
		t.Errorf("id = %q, want a generated svc_ id", id)
	}
	// Defaults were applied before storing.
	if m["timeout"] != "1m0s" {
		t.Errorf("timeout = %v, want the 1m0s default", m["timeout"])
	}
	match := m["match"].(map[string]any)
	if match["query"] != "exact" || match["body"] != "json" || match["selection"] != "sequential" || match["miss_status"] != float64(404) {
		t.Errorf("match defaults = %v", match)
	}
	stored, err := e.st.GetService(context.Background(), id)
	if err != nil {
		t.Fatalf("service was not stored: %v", err)
	}
	if stored.Latency.Mode != model.LatencyRecorded || stored.Match.Query != model.QueryExact {
		t.Errorf("stored service lacks defaults: %+v", stored)
	}
	applied, ok := e.svcs.lastApplied(id)
	if !ok || applied.Name != "payments" {
		t.Errorf("Apply was not called with the stored service: %+v", applied)
	}
	st := m["status"].(map[string]any)
	if st["service_id"] != id || st["mode"] != "record" {
		t.Errorf("status = %v", st)
	}
}

func TestCreateServiceClientID(t *testing.T) {
	e := newEnv(t)
	body := validService("web", ":9002")
	body["id"] = "web-1"
	var out ServiceWithStatus
	e.call("POST", "/api/v1/services", body, http.StatusCreated, &out)
	if out.ID != "web-1" {
		t.Fatalf("ID = %q", out.ID)
	}

	dup := validService("web2", ":9003")
	dup["id"] = "web-1"
	if eb := e.errorOf("POST", "/api/v1/services", dup, http.StatusConflict); !strings.Contains(eb.Error, "web-1") {
		t.Errorf("error = %q", eb.Error)
	}
	// The existing service was not overwritten.
	got, _ := e.st.GetService(context.Background(), "web-1")
	if got.Name != "web" {
		t.Errorf("POST with an existing id overwrote the service: %+v", got)
	}

	for _, bad := range []string{"has space", "a/b", "../x", strings.Repeat("x", 65), "-lead"} {
		b := validService("n"+fmt.Sprint(len(bad)), ":0")
		b["id"] = bad
		eb := e.errorOf("POST", "/api/v1/services", b, http.StatusBadRequest)
		if len(eb.Problems) == 0 {
			t.Errorf("id %q: want problems, got %+v", bad, eb)
		}
	}
}

func TestCreateServiceValidation(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("r", 0)
	tests := []struct {
		name     string
		body     any
		status   int
		problems []string // each must appear in some problem
	}{
		{name: "bad name and address", body: map[string]any{"name": "bad name!", "listen_addr": "9001", "upstream": "http://u"}, status: 400,
			problems: []string{"name must be", "listen_addr"}},
		{name: "record without upstream or recording", body: map[string]any{"name": "x", "listen_addr": ":1", "mode": "record"}, status: 400,
			problems: []string{"upstream", "recording_id"}},
		{name: "unknown recording", body: map[string]any{"name": "x", "listen_addr": ":1", "upstream": "http://u", "mode": "virtualize", "recording_id": "rec_nope"}, status: 400,
			problems: []string{`recording_id "rec_nope" does not exist`}},
		{name: "unknown profile", body: map[string]any{"name": "x", "listen_addr": ":1", "upstream": "http://u", "profile_id": "prof_nope"}, status: 400,
			problems: []string{`profile_id "prof_nope" does not exist`}},
		{name: "validation and reference problems together", body: map[string]any{"name": "!!", "listen_addr": ":1", "upstream": "http://u", "profile_id": "prof_nope", "recording_id": rec.ID, "match": map[string]any{"query": "weird"}}, status: 400,
			problems: []string{"name must be", "match.query", "profile_id"}},
		{name: "unknown field", body: map[string]any{"name": "x", "listen_addr": ":1", "upstream": "http://u", "bogus": 1}, status: 400,
			problems: nil},
		{name: "wrong type", body: map[string]any{"name": "x", "listen_addr": ":1", "upstream": "http://u", "enabled": "yes"}, status: 400},
		{name: "not an object", body: "[1,2,3]", status: 400},
		{name: "malformed JSON", body: `{"name":`, status: 400},
		{name: "empty body", body: "", status: 400},
		{name: "bad duration", body: `{"name":"x","listen_addr":":1","upstream":"http://u","timeout":"soon"}`, status: 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eb := e.errorOf("POST", "/api/v1/services", tc.body, tc.status)
			joined := strings.Join(eb.Problems, "\n") + "\n" + eb.Error
			for _, want := range tc.problems {
				if !strings.Contains(joined, want) {
					t.Errorf("problems %q lack %q", joined, want)
				}
			}
		})
	}
	if n := e.svcs.applyCount(); n != 0 {
		t.Errorf("Apply was called %d times for invalid services", n)
	}
	if list, _ := e.st.ListServices(context.Background()); len(list) != 0 {
		t.Errorf("%d services stored from invalid requests", len(list))
	}
}

func TestCreateServiceConflicts(t *testing.T) {
	e := newEnv(t)
	e.call("POST", "/api/v1/services", validService("one", ":9101"), http.StatusCreated, nil)
	e.errorOf("POST", "/api/v1/services", validService("one", ":9102"), http.StatusConflict)
	e.errorOf("POST", "/api/v1/services", validService("two", ":9101"), http.StatusConflict)
	e.errorOf("POST", "/api/v1/services", validService("two", "0.0.0.0:9101"), http.StatusConflict)
	// Port 0 never conflicts.
	e.call("POST", "/api/v1/services", validService("three", ":0"), http.StatusCreated, nil)
	e.call("POST", "/api/v1/services", validService("four", ":0"), http.StatusCreated, nil)
}

func TestServiceEchoFieldsAreAccepted(t *testing.T) {
	e := newEnv(t)
	var created ServiceWithStatus
	e.call("POST", "/api/v1/services", validService("echo", ":9201"), http.StatusCreated, &created)

	// Fetch it and PUT exactly what came back: status, created_at and
	// updated_at are echo fields.
	resp, raw := e.do("GET", "/api/v1/services/"+created.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET: %d %s", resp.StatusCode, raw)
	}
	var out ServiceWithStatus
	e.call("PUT", "/api/v1/services/"+created.ID, string(raw), http.StatusOK, &out)
	if out.Name != "echo" {
		t.Errorf("name = %q", out.Name)
	}
	// A forged timestamp is ignored.
	body := asMap(t, raw)
	body["created_at"] = "2001-01-01T00:00:00Z"
	body["has_secret"] = true
	e.call("PUT", "/api/v1/services/"+created.ID, body, http.StatusOK, &out)
	if out.CreatedAt.Year() < 2020 {
		t.Errorf("created_at was taken from the request: %v", out.CreatedAt)
	}
}

func TestServiceRejectsOtherObjectsEchoFields(t *testing.T) {
	e := newEnv(t)
	body := validService("strict", ":0")
	body["fired"] = 3 // a fault-rule echo field, not a service one
	e.errorOf("POST", "/api/v1/services", body, http.StatusBadRequest)
}

func TestUpdateService(t *testing.T) {
	e := newEnv(t)
	var created ServiceWithStatus
	e.call("POST", "/api/v1/services", validService("api", ":9301"), http.StatusCreated, &created)

	t.Run("replaces the whole object", func(t *testing.T) {
		body := validService("api-renamed", ":9301")
		body["enabled"] = true
		body["error_floor"] = 0.25
		var out ServiceWithStatus
		e.call("PUT", "/api/v1/services/"+created.ID, body, http.StatusOK, &out)
		if out.ID != created.ID || out.Name != "api-renamed" || !out.Enabled || out.ErrorFloor != 0.25 {
			t.Errorf("out = %+v", out.Service)
		}
		if !out.CreatedAt.Equal(created.CreatedAt) {
			t.Errorf("created_at changed: %v -> %v", created.CreatedAt, out.CreatedAt)
		}
		if !out.Status.Running || out.Status.Mode != model.ModePassthrough {
			t.Errorf("status = %+v", out.Status)
		}
		applied, _ := e.svcs.lastApplied(created.ID)
		if applied.Name != "api-renamed" || !applied.Enabled {
			t.Errorf("controller has stale config: %+v", applied)
		}
	})

	t.Run("id in the URL wins over the body", func(t *testing.T) {
		body := validService("api-renamed", ":9301")
		body["id"] = "someone-else"
		var out ServiceWithStatus
		e.call("PUT", "/api/v1/services/"+created.ID, body, http.StatusOK, &out)
		if out.ID != created.ID {
			t.Errorf("ID = %q, want %q", out.ID, created.ID)
		}
		if _, err := e.st.GetService(context.Background(), "someone-else"); err == nil {
			t.Error("a service was created under the body id")
		}
	})

	t.Run("unknown service", func(t *testing.T) {
		e.errorOf("PUT", "/api/v1/services/svc_nope", validService("x", ":1"), http.StatusNotFound)
	})
	t.Run("invalid", func(t *testing.T) {
		eb := e.errorOf("PUT", "/api/v1/services/"+created.ID, map[string]any{"name": "!", "listen_addr": ":1", "upstream": "http://u"}, http.StatusBadRequest)
		if len(eb.Problems) == 0 {
			t.Error("want problems")
		}
	})
	t.Run("name conflict with another service", func(t *testing.T) {
		e.call("POST", "/api/v1/services", validService("other", ":9302"), http.StatusCreated, nil)
		e.errorOf("PUT", "/api/v1/services/"+created.ID, validService("other", ":9301"), http.StatusConflict)
	})
}

func TestServiceSavedWhenApplyFails(t *testing.T) {
	tests := []struct {
		name   string
		silent bool
	}{
		{name: "controller reports the error in its status"},
		{name: "controller leaves the status error empty", silent: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.svcs.applyErr = errors.New("listen tcp :9401: bind: address already in use")
			e.svcs.silentFail = tc.silent
			body := validService("busy", ":9401")
			body["enabled"] = true
			var out ServiceWithStatus
			e.call("POST", "/api/v1/services", body, http.StatusCreated, &out)
			if !strings.Contains(out.Status.Error, "address already in use") {
				t.Errorf("status.error = %q", out.Status.Error)
			}
			if out.Status.Running {
				t.Error("a service that failed to start must not report running")
			}
			if _, err := e.st.GetService(context.Background(), out.ID); err != nil {
				t.Errorf("service was not kept: %v", err)
			}

			// Updating it and failing again still saves.
			body["name"] = "busy2"
			e.call("PUT", "/api/v1/services/"+out.ID, body, http.StatusOK, &out)
			if out.Name != "busy2" || out.Status.Error == "" {
				t.Errorf("out = %+v", out)
			}
		})
	}
}

func TestGetAndListServices(t *testing.T) {
	e := newEnv(t)
	for _, n := range []string{"charlie", "alpha", "bravo"} {
		e.call("POST", "/api/v1/services", validService(n, ":0"), http.StatusCreated, nil)
	}
	// A service the controller has never heard of still lists, with a status
	// derived from the stored service.
	orphan := e.seedService(model.Service{Name: "delta", Mode: model.ModeRecord, RecordingID: "rec_x"})

	var list []ServiceWithStatus
	e.call("GET", "/api/v1/services", nil, http.StatusOK, &list)
	var names []string
	for _, s := range list {
		names = append(names, s.Name)
		if s.Status.ServiceID != s.ID {
			t.Errorf("%s: status.service_id = %q", s.Name, s.Status.ServiceID)
		}
	}
	if got := strings.Join(names, ","); got != "alpha,bravo,charlie,delta" {
		t.Errorf("order = %s, want sorted by name", got)
	}
	last := list[len(list)-1]
	if last.ID != orphan.ID || last.Status.Mode != model.ModeRecord || last.Status.Running {
		t.Errorf("orphan status = %+v", last.Status)
	}

	var one ServiceWithStatus
	e.call("GET", "/api/v1/services/"+orphan.ID, nil, http.StatusOK, &one)
	if one.Name != "delta" {
		t.Errorf("GET = %+v", one.Service)
	}
	e.errorOf("GET", "/api/v1/services/svc_nope", nil, http.StatusNotFound)

	// An empty list is [], not null.
	e2 := newEnv(t)
	_, raw := e2.do("GET", "/api/v1/services", nil)
	if strings.TrimSpace(string(raw)) != "[]" {
		t.Errorf("empty list = %s", raw)
	}
}

func TestServiceStatusComesFromController(t *testing.T) {
	e := newEnv(t)
	var s ServiceWithStatus
	e.call("POST", "/api/v1/services", validService("live", ":0"), http.StatusCreated, &s)
	e.svcs.setStatus(model.ServiceStatus{ServiceID: s.ID, Running: true, Addr: "[::]:9999", Mode: model.ModeRecord, Captured: 1520, CaptureDropped: 3})
	var got ServiceWithStatus
	e.call("GET", "/api/v1/services/"+s.ID, nil, http.StatusOK, &got)
	if !got.Status.Running || got.Status.Addr != "[::]:9999" || got.Status.Captured != 1520 || got.Status.CaptureDropped != 3 {
		t.Errorf("status = %+v", got.Status)
	}
}

func TestDeleteService(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("keepme", 3)
	body := validService("doomed", ":0")
	body["mode"] = "record"
	body["recording_id"] = rec.ID
	var s ServiceWithStatus
	e.call("POST", "/api/v1/services", body, http.StatusCreated, &s)
	name := metrics.ServiceCollectorName(s.ID)
	e.reg.Collector(name, model.KindService, s.ID)

	resp, raw := e.do("DELETE", "/api/v1/services/"+s.ID, nil)
	if resp.StatusCode != http.StatusNoContent || len(raw) != 0 {
		t.Fatalf("DELETE = %d %q", resp.StatusCode, raw)
	}
	if len(e.svcs.removed) != 1 || e.svcs.removed[0] != s.ID {
		t.Errorf("Remove calls = %v", e.svcs.removed)
	}
	if _, ok := e.reg.Get(name); ok {
		t.Error("the service's collector survived the delete")
	}
	if _, err := e.st.GetService(context.Background(), s.ID); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("service still stored: %v", err)
	}
	if _, err := e.st.GetRecording(context.Background(), rec.ID); err != nil {
		t.Errorf("the recording must be kept: %v", err)
	}
	e.errorOf("DELETE", "/api/v1/services/"+s.ID, nil, http.StatusNotFound)

	t.Run("controller error leaves the service stored", func(t *testing.T) {
		var s2 ServiceWithStatus
		e.call("POST", "/api/v1/services", validService("stuck", ":0"), http.StatusCreated, &s2)
		e.svcs.removeErr = errBoom
		eb := e.errorOf("DELETE", "/api/v1/services/"+s2.ID, nil, http.StatusInternalServerError)
		if strings.Contains(eb.Error, "boom") {
			t.Errorf("internal detail leaked: %q", eb.Error)
		}
		if _, err := e.st.GetService(context.Background(), s2.ID); err != nil {
			t.Errorf("service was deleted anyway: %v", err)
		}
		e.svcs.removeErr = nil
	})
	t.Run("controller that does not know the service", func(t *testing.T) {
		s3 := e.seedService(model.Service{Name: "unknown-to-controller"})
		e.svcs.removeErr = fmt.Errorf("proxy: service %s: %w", s3.ID, model.ErrNotFound)
		e.call("DELETE", "/api/v1/services/"+s3.ID, nil, http.StatusNoContent, nil)
		e.svcs.removeErr = nil
	})
}

func TestSetServiceMode(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("existing", 2)
	body := validService("modes", ":0")
	body["anonymize_capture"] = true
	var s ServiceWithStatus
	e.call("POST", "/api/v1/services", body, http.StatusCreated, &s)
	path := "/api/v1/services/" + s.ID + "/mode"

	t.Run("existing recording", func(t *testing.T) {
		var out ServiceWithStatus
		e.call("POST", path, map[string]any{"mode": "virtualize", "recording_id": rec.ID}, http.StatusOK, &out)
		if out.Mode != model.ModeVirtualize || out.RecordingID != rec.ID {
			t.Errorf("out = %+v", out.Service)
		}
		applied, _ := e.svcs.lastApplied(s.ID)
		if applied.Mode != model.ModeVirtualize {
			t.Errorf("controller mode = %q", applied.Mode)
		}
		if out.Status.Mode != model.ModeVirtualize {
			t.Errorf("status mode = %q", out.Status.Mode)
		}
	})

	t.Run("keeps the recording when only the mode changes", func(t *testing.T) {
		var out ServiceWithStatus
		e.call("POST", path, map[string]any{"mode": "hybrid"}, http.StatusOK, &out)
		if out.Mode != model.ModeHybrid || out.RecordingID != rec.ID {
			t.Errorf("out = %+v", out.Service)
		}
		e.call("POST", path, map[string]any{"mode": "passthrough"}, http.StatusOK, &out)
		if out.Mode != model.ModePassthrough || out.RecordingID != rec.ID {
			t.Errorf("passthrough dropped the recording: %+v", out.Service)
		}
	})

	t.Run("new recording", func(t *testing.T) {
		before, _ := e.st.ListRecordings(context.Background())
		var out ServiceWithStatus
		e.call("POST", path, map[string]any{"mode": "record", "new_recording_name": "  checkout run 3  "}, http.StatusOK, &out)
		after, _ := e.st.ListRecordings(context.Background())
		if len(after) != len(before)+1 {
			t.Fatalf("recordings %d -> %d, want one more", len(before), len(after))
		}
		created, err := e.st.GetRecording(context.Background(), out.RecordingID)
		if err != nil {
			t.Fatal(err)
		}
		if created.Name != "checkout run 3" || created.ServiceID != s.ID || created.Source != "proxy" || !created.Anonymized {
			t.Errorf("new recording = %+v", created)
		}
		if out.Mode != model.ModeRecord {
			t.Errorf("mode = %q", out.Mode)
		}
	})

	t.Run("refused requests", func(t *testing.T) {
		before, _ := e.st.ListRecordings(context.Background())
		tests := []struct {
			name   string
			body   any
			status int
			want   string
		}{
			{"mode missing", map[string]any{}, 400, "mode is required"},
			{"bad mode", map[string]any{"mode": "chaos"}, 400, "must be passthrough"},
			{"both recordings", map[string]any{"mode": "record", "recording_id": rec.ID, "new_recording_name": "x"}, 400, "mutually exclusive"},
			{"blank name", map[string]any{"mode": "record", "new_recording_name": "   "}, 400, "must not be blank"},
			{"passthrough with new recording", map[string]any{"mode": "passthrough", "new_recording_name": "x"}, 400, "does not use a recording"},
			{"unknown recording", map[string]any{"mode": "virtualize", "recording_id": "rec_nope"}, 400, "does not exist"},
			{"unknown field", map[string]any{"mode": "record", "bogus": true}, 400, "bogus"},
		}
		for _, tc := range tests {
			eb := e.errorOf("POST", path, tc.body, tc.status)
			if joined := eb.Error + strings.Join(eb.Problems, ";"); !strings.Contains(joined, tc.want) {
				t.Errorf("%s: error %q lacks %q", tc.name, joined, tc.want)
			}
		}
		after, _ := e.st.ListRecordings(context.Background())
		if len(after) != len(before) {
			t.Errorf("refused mode changes created %d recordings", len(after)-len(before))
		}
		e.errorOf("POST", "/api/v1/services/svc_nope/mode", map[string]any{"mode": "record"}, http.StatusNotFound)
	})

	t.Run("recording created for a change that then fails is removed", func(t *testing.T) {
		// A service whose profile disappeared fails the final check, after the
		// recording was created.
		s2 := e.seedService(model.Service{Name: "orphan-profile", ProfileID: "prof_gone"})
		before, _ := e.st.ListRecordings(context.Background())
		e.errorOf("POST", "/api/v1/services/"+s2.ID+"/mode", map[string]any{"mode": "record", "new_recording_name": "ghost"}, http.StatusBadRequest)
		after, _ := e.st.ListRecordings(context.Background())
		if len(after) != len(before) {
			t.Errorf("an orphan recording was left behind (%d -> %d)", len(before), len(after))
		}
	})
}

func TestEnableDisableReload(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("r", 1)
	body := validService("toggle", ":0")
	body["mode"] = "virtualize"
	body["recording_id"] = rec.ID
	var s ServiceWithStatus
	e.call("POST", "/api/v1/services", body, http.StatusCreated, &s)
	if s.Enabled {
		t.Fatal("services start disabled")
	}

	var out ServiceWithStatus
	e.call("POST", "/api/v1/services/"+s.ID+"/enable", nil, http.StatusOK, &out)
	if !out.Enabled || !out.Status.Running {
		t.Errorf("after enable: %+v / %+v", out.Service, out.Status)
	}
	if got, _ := e.st.GetService(context.Background(), s.ID); !got.Enabled {
		t.Error("enabled flag not stored")
	}
	if applied, _ := e.svcs.lastApplied(s.ID); !applied.Enabled {
		t.Error("controller not told")
	}

	t.Run("reload", func(t *testing.T) {
		e.call("POST", "/api/v1/services/"+s.ID+"/reload", nil, http.StatusOK, &out)
		if len(e.svcs.reloaded) != 1 || e.svcs.reloaded[0] != s.ID {
			t.Errorf("Reload calls = %v", e.svcs.reloaded)
		}
		e.errorOf("POST", "/api/v1/services/svc_nope/reload", nil, http.StatusNotFound)
		e.svcs.reloadErr = fmt.Errorf("proxy: service is not running: %w", model.ErrConflict)
		e.errorOf("POST", "/api/v1/services/"+s.ID+"/reload", nil, http.StatusConflict)
		e.svcs.reloadErr = errBoom
		if eb := e.errorOf("POST", "/api/v1/services/"+s.ID+"/reload", nil, http.StatusInternalServerError); strings.Contains(eb.Error, "boom") {
			t.Errorf("internal detail leaked: %q", eb.Error)
		}
		e.svcs.reloadErr = nil
	})

	t.Run("disable works even when the recording is gone", func(t *testing.T) {
		if err := e.st.DeleteRecording(context.Background(), rec.ID); err != nil {
			t.Fatal(err)
		}
		e.call("POST", "/api/v1/services/"+s.ID+"/disable", nil, http.StatusOK, &out)
		if out.Enabled || out.Status.Running {
			t.Errorf("after disable: %+v / %+v", out.Service, out.Status)
		}
	})
	t.Run("enable refuses a service whose recording is gone", func(t *testing.T) {
		before := e.svcs.applyCount()
		eb := e.errorOf("POST", "/api/v1/services/"+s.ID+"/enable", nil, http.StatusBadRequest)
		if !strings.Contains(strings.Join(eb.Problems, ";"), "does not exist") {
			t.Errorf("problems = %v", eb.Problems)
		}
		if e.svcs.applyCount() != before {
			t.Error("Apply was called for a service that cannot run")
		}
	})
	t.Run("unknown service", func(t *testing.T) {
		e.errorOf("POST", "/api/v1/services/svc_nope/enable", nil, http.StatusNotFound)
		e.errorOf("POST", "/api/v1/services/svc_nope/disable", nil, http.StatusNotFound)
	})
}

func TestServiceStatsAndSeries(t *testing.T) {
	e := newEnv(t)
	var s ServiceWithStatus
	e.call("POST", "/api/v1/services", validService("traffic", ":0"), http.StatusCreated, &s)

	t.Run("no collector yet", func(t *testing.T) {
		var snap model.StatsSnapshot
		e.call("GET", "/api/v1/services/"+s.ID+"/stats", nil, http.StatusOK, &snap)
		if snap.Name != "service:"+s.ID || snap.Kind != model.KindService || snap.ID != s.ID || snap.Requests != 0 {
			t.Errorf("snapshot = %+v", snap)
		}
		for _, k := range []string{"1xx", "2xx", "3xx", "4xx", "5xx", "err"} {
			if v, ok := snap.StatusClasses[k]; !ok || v != 0 {
				t.Errorf("status class %s = %v (present %v)", k, v, ok)
			}
		}
		_, raw := e.do("GET", "/api/v1/services/"+s.ID+"/series", nil)
		if strings.TrimSpace(string(raw)) != "[]" {
			t.Errorf("series = %s, want []", raw)
		}
	})

	t.Run("with traffic", func(t *testing.T) {
		col := e.reg.Collector(metrics.ServiceCollectorName(s.ID), model.KindService, s.ID)
		for i := 0; i < 5; i++ {
			col.Observe(metrics.Observation{Endpoint: "GET /a", Status: 200, Duration: 10 * time.Millisecond, BytesIn: 10, BytesOut: 100})
		}
		col.Observe(metrics.Observation{Endpoint: "GET /a", Status: 503, Duration: 90 * time.Millisecond, Error: true})
		now := time.Now()
		e.reg.Tick(now.Add(-2 * time.Second))
		e.reg.Tick(now.Add(-time.Second))
		e.reg.Tick(now)

		var snap model.StatsSnapshot
		e.call("GET", "/api/v1/services/"+s.ID+"/stats", nil, http.StatusOK, &snap)
		if snap.Requests != 6 || snap.Errors != 1 || snap.StatusClasses["2xx"] != 5 || snap.StatusClasses["5xx"] != 1 {
			t.Errorf("snapshot = %+v", snap)
		}
		if len(snap.Endpoints) != 1 || snap.Endpoints[0].Endpoint != "GET /a" {
			t.Errorf("endpoints = %+v", snap.Endpoints)
		}

		var all []model.SeriesPoint
		e.call("GET", "/api/v1/services/"+s.ID+"/series", nil, http.StatusOK, &all)
		if len(all) != 3 {
			t.Fatalf("default series has %d points, want 3", len(all))
		}
		var total int64
		for _, p := range all {
			total += p.Requests
		}
		if total != 6 {
			t.Errorf("series requests add up to %d, want 6", total)
		}
		var recent []model.SeriesPoint
		e.call("GET", fmt.Sprintf("/api/v1/services/%s/series?since=%d", s.ID, all[2].T), nil, http.StatusOK, &recent)
		if len(recent) != 1 || recent[0].T != all[2].T {
			t.Errorf("since filter: %+v", recent)
		}
		var none []model.SeriesPoint
		e.call("GET", fmt.Sprintf("/api/v1/services/%s/series?since=%d", s.ID, all[2].T+1), nil, http.StatusOK, &none)
		if none == nil || len(none) != 0 {
			t.Errorf("series past the end = %#v", none)
		}
	})

	t.Run("default window is the last 15 minutes", func(t *testing.T) {
		var s2 ServiceWithStatus
		e.call("POST", "/api/v1/services", validService("history", ":0"), http.StatusCreated, &s2)
		col := e.reg.Collector(metrics.ServiceCollectorName(s2.ID), model.KindService, s2.ID)
		old := time.Now().Add(-time.Hour)
		col.Observe(metrics.Observation{Endpoint: "GET /a", Status: 200, Duration: time.Millisecond})
		col.Tick(old)
		col.Observe(metrics.Observation{Endpoint: "GET /a", Status: 200, Duration: time.Millisecond})
		col.Tick(time.Now())
		var pts []model.SeriesPoint
		e.call("GET", "/api/v1/services/"+s2.ID+"/series", nil, http.StatusOK, &pts)
		for _, p := range pts {
			if p.T < time.Now().Add(-16*time.Minute).UnixMilli() {
				t.Errorf("point %d is older than the default window", p.T)
			}
		}
		var everything []model.SeriesPoint
		e.call("GET", "/api/v1/services/"+s2.ID+"/series?since=0", nil, http.StatusOK, &everything)
		if len(everything) <= len(pts) {
			t.Errorf("since=0 returned %d points, default %d", len(everything), len(pts))
		}
	})

	t.Run("errors", func(t *testing.T) {
		e.errorOf("GET", "/api/v1/services/svc_nope/stats", nil, http.StatusNotFound)
		e.errorOf("GET", "/api/v1/services/svc_nope/series", nil, http.StatusNotFound)
		e.errorOf("GET", "/api/v1/services/"+s.ID+"/series?since=yesterday", nil, http.StatusBadRequest)
	})
}

func TestStatsAll(t *testing.T) {
	e := newEnv(t)
	_, raw := e.do("GET", "/api/v1/stats", nil)
	if strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("no collectors = %s, want []", raw)
	}
	e.reg.Collector("service:b", model.KindService, "b").Observe(metrics.Observation{Endpoint: "GET /", Status: 200, Duration: time.Millisecond})
	e.reg.Collector("replay:a", model.KindReplay, "a")
	var snaps []model.StatsSnapshot
	e.call("GET", "/api/v1/stats", nil, http.StatusOK, &snaps)
	if len(snaps) != 2 || snaps[0].Name != "replay:a" || snaps[1].Name != "service:b" || snaps[1].Requests != 1 {
		t.Errorf("snapshots = %+v", snaps)
	}
}

// Concurrent changes to one service must leave the controller with the same
// configuration as the store.
func TestConcurrentServiceUpdatesStayConsistent(t *testing.T) {
	e := newEnv(t)
	var s ServiceWithStatus
	e.call("POST", "/api/v1/services", validService("race", ":0"), http.StatusCreated, &s)

	const workers = 24
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := validService(fmt.Sprintf("race-%d", i), ":0")
			body["max_capture_bytes"] = i + 1
			resp, raw := e.do("PUT", "/api/v1/services/"+s.ID, body)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("PUT %d: %d %s", i, resp.StatusCode, raw)
			}
		}(i)
	}
	wg.Wait()

	stored, err := e.st.GetService(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	applied, _ := e.svcs.lastApplied(s.ID)
	if stored.Name != applied.Name || stored.MaxCaptureBytes != applied.MaxCaptureBytes {
		t.Errorf("store has %s/%d but the controller has %s/%d", stored.Name, stored.MaxCaptureBytes, applied.Name, applied.MaxCaptureBytes)
	}
	if n := e.srv.svcLocks.size(); n != 0 {
		t.Errorf("%d service locks leaked", n)
	}
}
