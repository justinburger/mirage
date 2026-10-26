package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func replayRequest(recID string) map[string]any {
	return map[string]any{
		"name": "10x checkout",
		"config": map[string]any{
			"recording_id": recID, "target": "http://staging.internal:8080", "mode": "timeline", "speed": 10,
			"set_headers": map[string]string{"Authorization": "Bearer staging-token", "X-Env": "staging", "Cookie": "s=1", "x-api-key": "k"},
		},
	}
}

func TestCreateReplay(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("source", 5)

	t.Run("pending job; secrets masked in the response, kept in the store", func(t *testing.T) {
		var job model.ReplayJob
		resp, raw := e.do("POST", "/api/v1/replays", replayRequest(rec.ID))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		if err := json.Unmarshal(raw, &job); err != nil {
			t.Fatal(err)
		}
		if job.Name != "10x checkout" || job.Status != model.ReplayPending || !strings.HasPrefix(job.ID, "rpl_") {
			t.Errorf("job = %+v", job)
		}
		if strings.Contains(string(raw), "staging-token") || strings.Contains(string(raw), "s=1") {
			t.Errorf("credentials in the response: %s", raw)
		}
		h := job.Config.SetHeaders
		if h["Authorization"] != "***" || h["Cookie"] != "***" || h["x-api-key"] != "***" || h["X-Env"] != "staging" {
			t.Errorf("set_headers = %v", h)
		}
		if job.Config.Seed == 0 || job.Config.Concurrency != model.DefaultReplayConcurrency {
			t.Errorf("config was not defaulted by the manager: %+v", job.Config)
		}
		stored, _ := e.st.GetReplayJob(context.Background(), job.ID)
		if stored.Config.SetHeaders["Authorization"] != "Bearer staging-token" {
			t.Errorf("the stored header was masked: %v", stored.Config.SetHeaders)
		}
	})
	t.Run("start: true runs it", func(t *testing.T) {
		body := replayRequest(rec.ID)
		body["start"] = true
		var job model.ReplayJob
		e.call("POST", "/api/v1/replays", body, http.StatusCreated, &job)
		if job.Status != model.ReplayRunning || job.StartedAt == nil {
			t.Errorf("job = %+v", job)
		}
		if len(e.rpls.Running()) != 1 {
			t.Errorf("%d jobs running", len(e.rpls.Running()))
		}
		e.rpls.finish(job.ID, model.ReplayCompleted, nil)
	})
	t.Run("default name", func(t *testing.T) {
		body := replayRequest(rec.ID)
		delete(body, "name")
		var job model.ReplayJob
		e.call("POST", "/api/v1/replays", body, http.StatusCreated, &job)
		if !strings.HasPrefix(job.Name, "replay 20") {
			t.Errorf("name = %q", job.Name)
		}
	})
	t.Run("a job that cannot start is not left behind", func(t *testing.T) {
		before, _ := e.st.ListReplayJobs(context.Background())
		e.rpls.startErr = fmt.Errorf("replay: target unreachable: %w", model.ErrConflict)
		defer func() { e.rpls.startErr = nil }()
		body := replayRequest(rec.ID)
		body["start"] = true
		e.errorOf("POST", "/api/v1/replays", body, http.StatusConflict)
		after, _ := e.st.ListReplayJobs(context.Background())
		if len(after) != len(before) {
			t.Errorf("%d jobs left behind", len(after)-len(before))
		}
	})
	t.Run("errors", func(t *testing.T) {
		// A recording the request names that does not exist is a 400 with a
		// problem, like the other references in request bodies.
		missingRec := replayRequest("rec_nope")
		eb := e.errorOf("POST", "/api/v1/replays", missingRec, http.StatusBadRequest)
		if len(eb.Problems) != 1 || eb.Problems[0] != `recording_id "rec_nope" does not exist` {
			t.Errorf("problems = %q", eb.Problems)
		}
		if strings.Contains(eb.Error, "replay:") {
			t.Errorf("package prefix leaked: %q", eb.Error)
		}
		bad := replayRequest(rec.ID)
		bad["config"].(map[string]any)["mode"] = "teleport"
		if eb := e.errorOf("POST", "/api/v1/replays", bad, http.StatusBadRequest); len(eb.Problems) == 0 {
			t.Errorf("want problems, got %+v", eb)
		}
		noTarget := replayRequest(rec.ID)
		noTarget["config"].(map[string]any)["target"] = "ftp://x"
		e.errorOf("POST", "/api/v1/replays", noTarget, http.StatusBadRequest)
		unknownField := replayRequest(rec.ID)
		unknownField["config"].(map[string]any)["bogus"] = 1
		e.errorOf("POST", "/api/v1/replays", unknownField, http.StatusBadRequest)
		e.errorOf("POST", "/api/v1/replays", map[string]any{"name": "x", "config": map[string]any{}, "extra": 1}, http.StatusBadRequest)
		longName := replayRequest(rec.ID)
		longName["name"] = strings.Repeat("n", 300)
		e.errorOf("POST", "/api/v1/replays", longName, http.StatusBadRequest)
	})
}

func TestReplayLifecycle(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("source", 5)
	var job model.ReplayJob
	e.call("POST", "/api/v1/replays", replayRequest(rec.ID), http.StatusCreated, &job)
	base := "/api/v1/replays/" + job.ID

	t.Run("start", func(t *testing.T) {
		var out model.ReplayJob
		e.call("POST", base+"/start", nil, http.StatusOK, &out)
		if out.Status != model.ReplayRunning || out.Summary == nil {
			t.Errorf("out = %+v", out)
		}
		if out.Config.SetHeaders["Authorization"] != "***" {
			t.Errorf("start response leaks credentials: %v", out.Config.SetHeaders)
		}
		e.errorOf("POST", base+"/start", nil, http.StatusConflict)
	})
	t.Run("list shows live state, without series", func(t *testing.T) {
		e.rpls.setSeries(job.ID, []model.SeriesPoint{{T: 1000, Requests: 5}})
		var list []model.ReplayJob
		e.call("GET", "/api/v1/replays", nil, http.StatusOK, &list)
		if len(list) != 1 || list[0].Status != model.ReplayRunning || len(list[0].Series) != 0 {
			t.Errorf("list = %+v", list)
		}
		if list[0].Config.SetHeaders["Authorization"] != "***" {
			t.Errorf("list leaks credentials")
		}
	})
	t.Run("series", func(t *testing.T) {
		e.rpls.setSeries(job.ID, []model.SeriesPoint{{T: 1000, Requests: 5}, {T: 2000, Requests: 7}, {T: 3000, Requests: 9}})
		var all, since []model.SeriesPoint
		e.call("GET", base+"/series", nil, http.StatusOK, &all)
		e.call("GET", base+"/series?since=2000", nil, http.StatusOK, &since)
		if len(all) != 3 || len(since) != 2 || since[0].T != 2000 {
			t.Errorf("all=%d since=%d", len(all), len(since))
		}
		_, raw := e.do("GET", base+"/series?since=9999", nil)
		if strings.TrimSpace(string(raw)) != "[]" {
			t.Errorf("series past the end = %s", raw)
		}
		e.errorOf("GET", base+"/series?since=x", nil, http.StatusBadRequest)
		e.errorOf("GET", "/api/v1/replays/rpl_nope/series", nil, http.StatusNotFound)
	})
	t.Run("stop", func(t *testing.T) {
		var out model.ReplayJob
		e.call("POST", base+"/stop", nil, http.StatusOK, &out)
		if out.Status != model.ReplayStopped || out.EndedAt == nil {
			t.Errorf("out = %+v", out)
		}
		if len(e.rpls.stopped) != 1 {
			t.Errorf("Stop calls = %v", e.rpls.stopped)
		}
	})
	t.Run("get includes the series of a finished job", func(t *testing.T) {
		var out model.ReplayJob
		e.call("GET", base, nil, http.StatusOK, &out)
		if len(out.Series) != 3 || out.Status != model.ReplayStopped {
			t.Errorf("out = %+v", out)
		}
	})
	t.Run("re-run a finished job", func(t *testing.T) {
		var out model.ReplayJob
		e.call("POST", base+"/start", nil, http.StatusOK, &out)
		if out.Status != model.ReplayRunning {
			t.Errorf("status = %q", out.Status)
		}
		e.call("POST", base+"/stop", nil, http.StatusOK, &out)
	})
	t.Run("delete", func(t *testing.T) {
		resp, body := e.do("DELETE", base, nil)
		if resp.StatusCode != http.StatusNoContent || len(body) != 0 {
			t.Fatalf("DELETE = %d %q", resp.StatusCode, body)
		}
		e.errorOf("GET", base, nil, http.StatusNotFound)
		e.errorOf("DELETE", base, nil, http.StatusNotFound)
	})
	t.Run("unknown job", func(t *testing.T) {
		for _, c := range [][2]string{{"POST", "/start"}, {"POST", "/stop"}, {"POST", "/clone"}} {
			e.errorOf(c[0], "/api/v1/replays/rpl_nope"+c[1], nil, http.StatusNotFound)
		}
	})
}

func TestCloneReplay(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("source", 5)
	var job model.ReplayJob
	e.call("POST", "/api/v1/replays", replayRequest(rec.ID), http.StatusCreated, &job)
	e.call("POST", "/api/v1/replays/"+job.ID+"/start", nil, http.StatusOK, nil)
	e.rpls.finish(job.ID, model.ReplayCompleted, &model.ReplaySummary{Planned: 10, Sent: 10, Completed: 10})

	check := func(name string, body any, wantName string) {
		t.Helper()
		var clone model.ReplayJob
		e.call("POST", "/api/v1/replays/"+job.ID+"/clone", body, http.StatusCreated, &clone)
		if clone.ID == job.ID || clone.Status != model.ReplayPending || clone.Name != wantName {
			t.Errorf("%s: clone = %+v", name, clone)
		}
		if clone.Summary != nil || clone.StartedAt != nil {
			t.Errorf("%s: a clone starts without results: %+v", name, clone)
		}
		if clone.Config.SetHeaders["Authorization"] != "***" {
			t.Errorf("%s: response leaks credentials: %v", name, clone.Config.SetHeaders)
		}
		// The manager received the real config, credentials and seed included.
		e.rpls.mu.Lock()
		got := e.rpls.created[len(e.rpls.created)-1]
		e.rpls.mu.Unlock()
		if got.SetHeaders["Authorization"] != "Bearer staging-token" {
			t.Errorf("%s: clone was created from the masked config: %v", name, got.SetHeaders)
		}
		if got.Seed != job.Config.Seed || got.Seed == 0 || got.Speed != 10 || got.RecordingID != rec.ID {
			t.Errorf("%s: clone config differs: %+v vs %+v", name, got, job.Config)
		}
	}
	check("default name", nil, "10x checkout (copy)")
	check("empty object", map[string]any{}, "10x checkout (copy)")
	check("named", map[string]any{"name": "again"}, "again")
	e.errorOf("POST", "/api/v1/replays/"+job.ID+"/clone", map[string]any{"bogus": 1}, http.StatusBadRequest)
	e.errorOf("POST", "/api/v1/replays/"+job.ID+"/clone", map[string]any{"name": strings.Repeat("n", 300)}, http.StatusBadRequest)
}

// Which set_headers are masked is a fixed rule, not a property of the default
// anonymization profile that anyone may edit.
func TestReplayMasksCredentialHeaders(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("source", 1)
	// Strip the default profile's redact list down to nothing: masking must not care.
	var def model.AnonymizationProfile
	e.call("GET", "/api/v1/profiles/default", nil, http.StatusOK, &def)
	def.RedactHeaders = []string{"X-Only-This"}
	e.call("PUT", "/api/v1/profiles/default", def, http.StatusOK, nil)

	headers := map[string]string{
		"Authorization": "Bearer a", "Proxy-Authorization": "Basic b", "cookie": "s=1", "Set-Cookie": "s=2",
		"X-Api-Key": "k", "X-INTERNAL-token": "t", "X-Amz-Security-Token": "aws", "X-Api-Secret": "sh",
		"X-Auth-Token": "t2", "Ocp-Apim-Subscription-Key": "sub", "X-Session-Id": "sid", "X-Db-Password": "pw",
		// Not credentials: shown as sent.
		"X-Env": "stg", "X-Forwarded-For": "10.0.0.1", "X-Real-Ip": "10.0.0.2", "Forwarded": "for=10.0.0.3",
		"Content-Type": "application/json", "User-Agent": "mirage", "Accept-Language": "en",
	}
	body := replayRequest(rec.ID)
	body["config"].(map[string]any)["set_headers"] = headers
	var job model.ReplayJob
	e.call("POST", "/api/v1/replays", body, http.StatusCreated, &job)

	var listed []model.ReplayJob
	e.call("GET", "/api/v1/replays", nil, http.StatusOK, &listed)
	var fetched model.ReplayJob
	e.call("GET", "/api/v1/replays/"+job.ID, nil, http.StatusOK, &fetched)
	for where, got := range map[string]map[string]string{"create": job.Config.SetHeaders, "list": listed[0].Config.SetHeaders, "get": fetched.Config.SetHeaders} {
		for name, value := range headers {
			want := value
			if isSensitiveHeader(name) {
				want = maskedValue
			}
			if got[name] != want {
				t.Errorf("%s: %s = %q, want %q", where, name, got[name], want)
			}
		}
	}
	for _, name := range []string{"Authorization", "X-Api-Key", "X-Amz-Security-Token", "X-Api-Secret", "X-Auth-Token"} {
		if fetched.Config.SetHeaders[name] != maskedValue {
			t.Errorf("%s is not masked: %q", name, fetched.Config.SetHeaders[name])
		}
	}
	for _, name := range []string{"X-Env", "X-Forwarded-For", "X-Real-Ip", "Forwarded", "Content-Type"} {
		if fetched.Config.SetHeaders[name] != headers[name] {
			t.Errorf("%s was masked: %q", name, fetched.Config.SetHeaders[name])
		}
	}
	// The stored job keeps the real values (a re-run needs them).
	stored, _ := e.st.GetReplayJob(context.Background(), job.ID)
	if stored.Config.SetHeaders["X-Api-Secret"] != "sh" {
		t.Errorf("stored header = %q", stored.Config.SetHeaders["X-Api-Secret"])
	}
}

// A client that GETs a job, edits it and POSTs it back would send the mask as
// the credential. That is a 400 naming the header, and nothing is stored.
func TestCreateReplayRejectsMaskedSecretHeaders(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("source", 1)
	body := replayRequest(rec.ID)
	body["config"].(map[string]any)["set_headers"] = map[string]string{
		"Authorization": maskedValue, "X-Api-Key": maskedValue, "X-Env": maskedValue, "Cookie": "s=1",
	}
	eb := e.errorOf("POST", "/api/v1/replays", body, http.StatusBadRequest)
	if len(eb.Problems) != 2 || !strings.Contains(eb.Problems[0], "Authorization") || !strings.Contains(eb.Problems[1], "X-Api-Key") {
		t.Errorf("problems = %q, want one each for Authorization and X-Api-Key", eb.Problems)
	}
	var jobs []model.ReplayJob
	e.call("GET", "/api/v1/replays", nil, http.StatusOK, &jobs)
	if len(jobs) != 0 {
		t.Errorf("a rejected request left %d jobs", len(jobs))
	}

	// "***" on a header that is not a credential is just a value, and a real
	// secret is stored.
	body["config"].(map[string]any)["set_headers"] = map[string]string{"X-Env": maskedValue, "Authorization": "Bearer real"}
	var job model.ReplayJob
	e.call("POST", "/api/v1/replays", body, http.StatusCreated, &job)
	stored, _ := e.st.GetReplayJob(context.Background(), job.ID)
	if stored.Config.SetHeaders["Authorization"] != "Bearer real" || stored.Config.SetHeaders["X-Env"] != maskedValue {
		t.Errorf("stored headers = %v", stored.Config.SetHeaders)
	}
}

func TestIsSensitiveHeader(t *testing.T) {
	for name, want := range map[string]bool{
		"Authorization": true, "AUTHORIZATION": true, "Proxy-Authorization": true, "Cookie": true, "Set-Cookie": true,
		"X-Api-Key": true, "X-API-KEY": true, "Api-Key": true, "ApiKey": true, "Key": true, "X-Functions-Key": true,
		"X-Auth-Token": true, "X-Amz-Security-Token": true, "X-Api-Secret": true, "X-Client-Secret": true,
		"X-Password": true, "X-Credentials": true, "X-Session-Token": true, " x-csrf-token ": true,
		"X-Env": false, "Content-Type": false, "Accept": false, "User-Agent": false, "Host": false,
		"X-Forwarded-For": false, "X-Real-Ip": false, "Forwarded": false, "Keep-Alive": false, "X-Request-Id": false,
	} {
		if got := isSensitiveHeader(name); got != want {
			t.Errorf("isSensitiveHeader(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestPublicJobDoesNotModifyItsInput(t *testing.T) {
	orig := map[string]string{"Authorization": "Bearer x", "X-Env": "stg"}
	job := model.ReplayJob{Config: model.ReplayConfig{SetHeaders: orig}}
	masked := publicJob(job)
	if orig["Authorization"] != "Bearer x" {
		t.Error("publicJob wrote into the manager's map")
	}
	if masked.Config.SetHeaders["Authorization"] != "***" || masked.Config.SetHeaders["X-Env"] != "stg" {
		t.Errorf("masked = %v", masked.Config.SetHeaders)
	}
	if got := publicJob(model.ReplayJob{}); got.Config.SetHeaders != nil {
		t.Errorf("job without headers = %+v", got)
	}
}

// A replay that points at a recording or profile that does not exist is a bad
// request, answered the way POST /services answers it.
func TestCreateReplayUnknownReferencesAre400(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("source", 3)

	body := replayRequest("rec_nope")
	eb := e.errorOf("POST", "/api/v1/replays", body, http.StatusBadRequest)
	if len(eb.Problems) != 1 || !strings.Contains(eb.Problems[0], `recording_id "rec_nope" does not exist`) {
		t.Errorf("unknown recording: %+v", eb)
	}

	body = replayRequest(rec.ID)
	body["config"].(map[string]any)["profile_id"] = "prof_nope"
	eb = e.errorOf("POST", "/api/v1/replays", body, http.StatusBadRequest)
	if len(eb.Problems) != 1 || !strings.Contains(eb.Problems[0], `profile_id "prof_nope" does not exist`) {
		t.Errorf("unknown profile: %+v", eb)
	}

	body = replayRequest("rec_nope")
	body["config"].(map[string]any)["profile_id"] = "prof_nope"
	eb = e.errorOf("POST", "/api/v1/replays", body, http.StatusBadRequest)
	if len(eb.Problems) != 2 {
		t.Errorf("both references: %+v", eb)
	}

	// Config problems still win: they are reported before any lookup.
	body = replayRequest("rec_nope")
	body["config"].(map[string]any)["mode"] = "teleport"
	eb = e.errorOf("POST", "/api/v1/replays", body, http.StatusBadRequest)
	for _, p := range eb.Problems {
		if strings.Contains(p, "does not exist") {
			t.Errorf("a reference problem next to a config problem: %q", p)
		}
	}
	if jobs, _ := e.st.ListReplayJobs(context.Background()); len(jobs) != 0 {
		t.Errorf("%d jobs created by refused requests", len(jobs))
	}
}

// Starting a replay that cannot have anything to send fails at once, with a
// 409, instead of answering "running" and failing a moment later.
func TestReplayStartFailsAtOnceWithoutTraffic(t *testing.T) {
	e := newEnv(t)
	empty := e.seedRecording("empty", 0)
	full := e.seedRecording("full", 4)

	t.Run("create with start on an empty recording", func(t *testing.T) {
		body := replayRequest(empty.ID)
		body["start"] = true
		eb := e.errorOf("POST", "/api/v1/replays", body, http.StatusConflict)
		if !strings.Contains(eb.Error, "empty") || !strings.Contains(eb.Error, "nothing to replay") {
			t.Errorf("error = %q", eb.Error)
		}
		if jobs, _ := e.st.ListReplayJobs(context.Background()); len(jobs) != 0 {
			t.Errorf("%d jobs left behind", len(jobs))
		}
		if len(e.rpls.Running()) != 0 {
			t.Error("a job is running")
		}
	})

	t.Run("a pending job on an empty recording can exist but not start", func(t *testing.T) {
		var job model.ReplayJob
		e.call("POST", "/api/v1/replays", replayRequest(empty.ID), http.StatusCreated, &job)
		eb := e.errorOf("POST", "/api/v1/replays/"+job.ID+"/start", nil, http.StatusConflict)
		if !strings.Contains(eb.Error, "empty") {
			t.Errorf("error = %q", eb.Error)
		}
		var again model.ReplayJob
		e.call("GET", "/api/v1/replays/"+job.ID, nil, http.StatusOK, &again)
		if again.Status != model.ReplayPending {
			t.Errorf("status = %q after a refused start", again.Status)
		}
	})

	t.Run("the recording of a finished job was deleted", func(t *testing.T) {
		gone := e.seedRecording("doomed", 2)
		var job model.ReplayJob
		e.call("POST", "/api/v1/replays", replayRequest(gone.ID), http.StatusCreated, &job)
		e.call("POST", "/api/v1/replays/"+job.ID+"/start", nil, http.StatusOK, nil)
		e.rpls.finish(job.ID, model.ReplayCompleted, nil)
		e.call("DELETE", "/api/v1/recordings/"+gone.ID, nil, http.StatusNoContent, nil)

		for _, step := range []string{"/start", "/clone"} {
			eb := e.errorOf("POST", "/api/v1/replays/"+job.ID+step, nil, http.StatusConflict)
			if !strings.Contains(eb.Error, "no longer exists") {
				t.Errorf("%s: error = %q", step, eb.Error)
			}
			if n := strings.Count(eb.Error, gone.ID); n != 1 {
				t.Errorf("%s: error names the recording %d times: %q", step, n, eb.Error)
			}
			if strings.Contains(eb.Error, "store:") {
				t.Errorf("%s: package prefix leaked: %q", step, eb.Error)
			}
		}
		var again model.ReplayJob
		e.call("GET", "/api/v1/replays/"+job.ID, nil, http.StatusOK, &again)
		if again.Status != model.ReplayCompleted {
			t.Errorf("status = %q: a refused start must not touch the job", again.Status)
		}
	})

	t.Run("a recording with traffic still starts", func(t *testing.T) {
		var job model.ReplayJob
		e.call("POST", "/api/v1/replays", replayRequest(full.ID), http.StatusCreated, &job)
		e.call("POST", "/api/v1/replays/"+job.ID+"/start", nil, http.StatusOK, &job)
		if job.Status != model.ReplayRunning {
			t.Errorf("status = %q", job.Status)
		}
		// Starting a running job is still the manager's conflict.
		e.errorOf("POST", "/api/v1/replays/"+job.ID+"/start", nil, http.StatusConflict)
	})
}

// What a failed job recorded is shown without the internal package prefixes
// and without the repeated object names of the error chain.
func TestReplayJobErrorIsSanitized(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("source", 1)
	job := e.seedReplay("broken", rec.ID, model.ReplayFailed)
	job.Error = `failed recording "rec_x": store: recording "rec_x": not found`
	if _, err := e.st.PutReplayJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var got model.ReplayJob
	e.call("GET", "/api/v1/replays/"+job.ID, nil, http.StatusOK, &got)
	if got.Error != `failed recording "rec_x": not found` {
		t.Errorf("error = %q", got.Error)
	}
	var list []model.ReplayJob
	e.call("GET", "/api/v1/replays", nil, http.StatusOK, &list)
	if len(list) != 1 || list[0].Error != got.Error {
		t.Errorf("list error = %+v", list)
	}
}

func TestReplayDeleteConflictFromManager(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("source", 1)
	var job model.ReplayJob
	e.call("POST", "/api/v1/replays", replayRequest(rec.ID), http.StatusCreated, &job)
	// A manager may refuse to delete a running job.
	failing := &deleteRefusingReplays{fakeReplays: e.rpls}
	e.srv.rpls = failing
	eb := e.errorOf("DELETE", "/api/v1/replays/"+job.ID, nil, http.StatusConflict)
	if !strings.Contains(eb.Error, "running") {
		t.Errorf("error = %q", eb.Error)
	}
}

type deleteRefusingReplays struct{ *fakeReplays }

func (d *deleteRefusingReplays) Delete(ctx context.Context, id string) error {
	return fmt.Errorf("replay: job %s is running: %w", id, model.ErrConflict)
}

func TestNilControllersAnswer503(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Services, d.Replays = nil, nil })
	rec := e.seedRecording("r", 1)
	e.call("GET", "/api/v1/replays", nil, http.StatusOK, nil)
	e.errorOf("POST", "/api/v1/replays", replayRequest(rec.ID), http.StatusServiceUnavailable)
	// Services can still be stored; they cannot be applied.
	var out ServiceWithStatus
	e.call("POST", "/api/v1/services", validService("nocontrol", ":0"), http.StatusCreated, &out)
	if !strings.Contains(out.Status.Error, "not available") {
		t.Errorf("status = %+v", out.Status)
	}
	e.call("DELETE", "/api/v1/services/"+out.ID, nil, http.StatusNoContent, nil)
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		msg    string
	}{
		{"validation error", &model.ValidationError{Problems: []string{"a", "b"}}, 400, "validation failed"},
		{"wrapped validation error", fmt.Errorf("ctx: %w", &model.ValidationError{Problems: []string{"a"}}), 400, "validation failed"},
		{"bare validation sentinel", fmt.Errorf("ops: bad input: %w", model.ErrValidation), 400, "bad input"},
		{"not found", fmt.Errorf("store: recording %q: %w", "rec_x", model.ErrNotFound), 404, `recording "rec_x": not found`},
		{"conflict", fmt.Errorf("store: service name %q already in use: %w", "x", model.ErrConflict), 409, `service name "x" already in use`},
		{"unavailable", errUnavailable, 503, "not available"},
		{"canceled", fmt.Errorf("x: %w", context.Canceled), statusClientClosed, "cancelled"},
		{"deadline", context.DeadlineExceeded, 504, "timed out"},
		{"http error", newHTTPError(418, "teapot"), 418, "teapot"},
		{"unexpected", errors.New("sqlite: database is locked at /var/lib/x.db"), 500, "internal server error"},
		{"max bytes", &http.MaxBytesError{Limit: 10 << 20}, 413, "10 MiB"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, body := classify(tc.err)
			if status != tc.status || !strings.Contains(body.Error, tc.msg) {
				t.Errorf("classify = %d %q, want %d containing %q", status, body.Error, tc.status, tc.msg)
			}
			if status == 500 && strings.Contains(body.Error, "sqlite") {
				t.Error("internal detail leaked")
			}
		})
	}
	if _, body := classify(&model.ValidationError{Problems: []string{"a", "b"}}); len(body.Problems) != 2 {
		t.Errorf("problems = %v", body.Problems)
	}
}
