package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/model"
)

func strictProfile(name string) map[string]any {
	return map[string]any{
		"name":              name,
		"detectors":         []string{"email", "credit_card"},
		"detector_strategy": "fake",
		"redact_headers":    []string{"Authorization"},
		"opaque_bodies":     "keep",
		"rules": []map[string]any{
			{"name": "customer id", "enabled": true, "target": "json", "selector": "$..customer_id", "scope": "both", "strategy": "hash"},
			{"name": "card last4", "enabled": true, "target": "json", "selector": "$.card.last4", "scope": "response", "strategy": "mask", "keep_suffix": 2},
		},
	}
}

func TestProfileListNeverExposesSecrets(t *testing.T) {
	e := newEnv(t)
	var created model.AnonymizationProfile
	e.call("POST", "/api/v1/profiles", strictProfile("Strict"), http.StatusCreated, &created)

	resp, raw := e.do("GET", "/api/v1/profiles", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0]["id"] != "default" || list[1]["name"] != "Strict" {
		t.Fatalf("profiles = %s", raw)
	}
	for _, p := range list {
		if _, has := p["secret"]; has {
			t.Errorf("profile %v exposes its secret", p["id"])
		}
		if p["has_secret"] != true {
			t.Errorf("profile %v: has_secret = %v", p["id"], p["has_secret"])
		}
	}
	stored, _ := e.st.GetProfile(context.Background(), created.ID)
	if stored.Secret == "" || strings.Contains(string(raw), stored.Secret) {
		t.Errorf("secret %q leaked into the list or was never stored", stored.Secret)
	}
}

func TestCreateProfile(t *testing.T) {
	e := newEnv(t)

	t.Run("minimal profile gets defaults", func(t *testing.T) {
		resp, raw := e.do("POST", "/api/v1/profiles", map[string]any{"name": "Bare"})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		m := asMap(t, raw)
		if m["detector_strategy"] != "fake" || m["opaque_bodies"] != "keep" {
			t.Errorf("defaults missing: %s", raw)
		}
		for _, k := range []string{"detectors", "redact_headers", "rules"} {
			if arr, ok := m[k].([]any); !ok || len(arr) != 0 {
				t.Errorf("%s = %v, want []", k, m[k])
			}
		}
		if _, has := m["secret"]; has || m["has_secret"] != true {
			t.Errorf("secret handling: %s", raw)
		}
		if id, _ := m["id"].(string); !strings.HasPrefix(id, "prof_") {
			t.Errorf("id = %v", m["id"])
		}
	})
	t.Run("rules get ids", func(t *testing.T) {
		var p model.AnonymizationProfile
		e.call("POST", "/api/v1/profiles", strictProfile("With rules"), http.StatusCreated, &p)
		if len(p.Rules) != 2 || !strings.HasPrefix(p.Rules[0].ID, "rule_") {
			t.Errorf("rules = %+v", p.Rules)
		}
	})
	t.Run("a client-chosen secret is stored, never returned", func(t *testing.T) {
		body := strictProfile("Pinned")
		body["secret"] = "my-own-secret"
		resp, raw := e.do("POST", "/api/v1/profiles", body)
		if resp.StatusCode != http.StatusCreated || strings.Contains(string(raw), "my-own-secret") {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		id := asMap(t, raw)["id"].(string)
		if p, _ := e.st.GetProfile(context.Background(), id); p.Secret != "my-own-secret" {
			t.Errorf("stored secret = %q", p.Secret)
		}
	})
	t.Run("client id", func(t *testing.T) {
		body := strictProfile("Chosen id")
		body["id"] = "chosen"
		e.call("POST", "/api/v1/profiles", body, http.StatusCreated, nil)
		body["name"] = "Chosen id 2"
		e.errorOf("POST", "/api/v1/profiles", body, http.StatusConflict)
		body["id"] = "bad id"
		e.errorOf("POST", "/api/v1/profiles", body, http.StatusBadRequest)
	})
	t.Run("name conflict ignores case", func(t *testing.T) {
		e.errorOf("POST", "/api/v1/profiles", strictProfile("  WITH RULES "), http.StatusConflict)
	})
	t.Run("validation problems are listed", func(t *testing.T) {
		body := map[string]any{
			"name":      "",
			"detectors": []string{"telepathy"},
			"rules": []map[string]any{
				{"name": "bad path", "enabled": true, "target": "json", "selector": "$.[[", "strategy": "hash"},
				{"name": "bad regex", "enabled": true, "target": "regex", "selector": "(", "strategy": "redact"},
				{"name": "bad target", "enabled": true, "target": "telepathy", "selector": "x", "strategy": "hash"},
			},
		}
		eb := e.errorOf("POST", "/api/v1/profiles", body, http.StatusBadRequest)
		joined := strings.Join(eb.Problems, "\n")
		for _, want := range []string{"name is required", "telepathy", "regex"} {
			if !strings.Contains(joined, want) {
				t.Errorf("problems lack %q:\n%s", want, joined)
			}
		}
		if len(eb.Problems) < 4 {
			t.Errorf("want every problem listed, got %d: %v", len(eb.Problems), eb.Problems)
		}
	})
	t.Run("unknown fields", func(t *testing.T) {
		e.errorOf("POST", "/api/v1/profiles", map[string]any{"name": "x", "bogus": 1}, http.StatusBadRequest)
	})
}

func TestGetUpdateDeleteProfile(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var p model.AnonymizationProfile
	e.call("POST", "/api/v1/profiles", strictProfile("Editable"), http.StatusCreated, &p)
	before, _ := e.st.GetProfile(ctx, p.ID)

	var got model.AnonymizationProfile
	e.call("GET", "/api/v1/profiles/"+p.ID, nil, http.StatusOK, &got)
	if got.Name != "Editable" || got.Secret != "" || !got.HasSecret {
		t.Errorf("GET = %+v", got)
	}
	e.errorOf("GET", "/api/v1/profiles/nope", nil, http.StatusNotFound)

	t.Run("PUT with the fetched object keeps the secret", func(t *testing.T) {
		resp, raw := e.do("GET", "/api/v1/profiles/"+p.ID, nil)
		if resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode)
		}
		m := asMap(t, raw)
		m["description"] = "now documented"
		var out model.AnonymizationProfile
		// has_secret, created_at and updated_at come back unchanged: echo fields.
		e.call("PUT", "/api/v1/profiles/"+p.ID, m, http.StatusOK, &out)
		if out.Description != "now documented" || out.Secret != "" {
			t.Errorf("out = %+v", out)
		}
		after, _ := e.st.GetProfile(ctx, p.ID)
		if after.Secret != before.Secret {
			t.Error("an empty secret must keep the stored one")
		}
		if len(after.Rules) != 2 || after.Rules[0].ID != before.Rules[0].ID {
			t.Errorf("rule ids changed: %+v vs %+v", after.Rules, before.Rules)
		}
		if got := e.svcs.profiles; len(got) == 0 || got[len(got)-1] != p.ID {
			t.Errorf("ProfileChanged calls = %v", got)
		}
	})
	t.Run("regenerate_secret", func(t *testing.T) {
		body := strictProfile("Editable")
		body["regenerate_secret"] = true
		resp, raw := e.do("PUT", "/api/v1/profiles/"+p.ID, body)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		after, _ := e.st.GetProfile(ctx, p.ID)
		if after.Secret == "" || after.Secret == before.Secret {
			t.Errorf("secret not regenerated: %q -> %q", before.Secret, after.Secret)
		}
		if strings.Contains(string(raw), after.Secret) || asMap(t, raw)["has_secret"] != true {
			t.Errorf("response: %s", raw)
		}
		// Without the flag the new secret is kept.
		e.call("PUT", "/api/v1/profiles/"+p.ID, strictProfile("Editable"), http.StatusOK, nil)
		again, _ := e.st.GetProfile(ctx, p.ID)
		if again.Secret != after.Secret {
			t.Error("secret changed without regenerate_secret")
		}
	})
	t.Run("id in the URL wins", func(t *testing.T) {
		body := strictProfile("Editable")
		body["id"] = "other"
		var out model.AnonymizationProfile
		e.call("PUT", "/api/v1/profiles/"+p.ID, body, http.StatusOK, &out)
		if out.ID != p.ID {
			t.Errorf("ID = %q", out.ID)
		}
	})
	t.Run("PUT errors", func(t *testing.T) {
		e.errorOf("PUT", "/api/v1/profiles/nope", strictProfile("x"), http.StatusNotFound)
		var other model.AnonymizationProfile
		e.call("POST", "/api/v1/profiles", strictProfile("Other"), http.StatusCreated, &other)
		e.errorOf("PUT", "/api/v1/profiles/"+p.ID, strictProfile("other"), http.StatusConflict)
		bad := strictProfile("Editable")
		bad["rules"] = []map[string]any{{"name": "x", "enabled": true, "target": "json", "selector": "$.[[", "strategy": "hash"}}
		e.errorOf("PUT", "/api/v1/profiles/"+p.ID, bad, http.StatusBadRequest)
	})
	t.Run("a service that cannot adopt the change does not fail the save", func(t *testing.T) {
		e.svcs.profileErr = errBoom
		defer func() { e.svcs.profileErr = nil }()
		e.call("PUT", "/api/v1/profiles/"+p.ID, strictProfile("Editable"), http.StatusOK, nil)
	})
	t.Run("the default profile can be edited but not deleted", func(t *testing.T) {
		var def model.AnonymizationProfile
		e.call("GET", "/api/v1/profiles/default", nil, http.StatusOK, &def)
		def.Description = "edited"
		e.call("PUT", "/api/v1/profiles/default", def, http.StatusOK, nil)
		eb := e.errorOf("DELETE", "/api/v1/profiles/default", nil, http.StatusConflict)
		if !strings.Contains(eb.Error, "default") {
			t.Errorf("error = %q", eb.Error)
		}
	})
	t.Run("delete", func(t *testing.T) {
		resp, body := e.do("DELETE", "/api/v1/profiles/"+p.ID, nil)
		if resp.StatusCode != http.StatusNoContent || len(body) != 0 {
			t.Fatalf("DELETE = %d %q", resp.StatusCode, body)
		}
		e.errorOf("GET", "/api/v1/profiles/"+p.ID, nil, http.StatusNotFound)
		e.errorOf("DELETE", "/api/v1/profiles/"+p.ID, nil, http.StatusNotFound)
	})
}

func TestDeleteProfileReferentialIntegrity(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("r", 1)
	mk := func(name string) model.AnonymizationProfile {
		var p model.AnonymizationProfile
		e.call("POST", "/api/v1/profiles", strictProfile(name), http.StatusCreated, &p)
		return p
	}

	usedBySvc, usedByReplay, usedByDoneReplay, free := mk("by service"), mk("by replay"), mk("by finished replay"), mk("free")
	e.seedService(model.Service{Name: "uses-profile", ProfileID: usedBySvc.ID})
	e.seedReplay("uses-profile-replay", rec.ID, model.ReplayPending, func(c *model.ReplayConfig) { c.ProfileID = usedByReplay.ID })
	e.seedReplay("finished-replay", rec.ID, model.ReplayCompleted, func(c *model.ReplayConfig) { c.ProfileID = usedByDoneReplay.ID })

	for _, tc := range []struct {
		p    model.AnonymizationProfile
		want string
	}{
		{usedBySvc, `service "uses-profile"`},
		{usedByReplay, `replay "uses-profile-replay"`},
		{usedByDoneReplay, `replay "finished-replay"`},
	} {
		eb := e.errorOf("DELETE", "/api/v1/profiles/"+tc.p.ID, nil, http.StatusConflict)
		if !strings.Contains(eb.Error, tc.want) {
			t.Errorf("error = %q, want %q", eb.Error, tc.want)
		}
		if _, err := e.st.GetProfile(context.Background(), tc.p.ID); err != nil {
			t.Errorf("profile %s was deleted despite the conflict", tc.p.Name)
		}
	}
	e.call("DELETE", "/api/v1/profiles/"+free.ID, nil, http.StatusNoContent, nil)
}

func TestListDetectors(t *testing.T) {
	e := newEnv(t)
	var list []anonymize.DetectorInfo
	e.call("GET", "/api/v1/anonymize/detectors", nil, http.StatusOK, &list)
	if len(list) != len(model.AllDetectors) {
		t.Fatalf("%d detectors, want %d", len(list), len(model.AllDetectors))
	}
	for i, d := range list {
		if d.Name != model.AllDetectors[i] || d.Description == "" || d.Example == "" {
			t.Errorf("detector %d = %+v", i, d)
		}
	}
	// /info carries the same list.
	var info struct {
		Detectors []anonymize.DetectorInfo `json:"detectors"`
	}
	e.call("GET", "/api/v1/info", nil, http.StatusOK, &info)
	if len(info.Detectors) != len(list) {
		t.Errorf("info has %d detectors", len(info.Detectors))
	}
}

func TestAnonymizePreview(t *testing.T) {
	e := newEnv(t)
	rec := e.seedRecording("r", 3)
	var page exchangePage
	e.call("GET", "/api/v1/recordings/"+rec.ID+"/exchanges", nil, http.StatusOK, &page)
	exID := page.Items[1].ID

	type result struct {
		Exchange model.ExchangeView `json:"exchange"`
		Changes  []model.AnonChange `json:"changes"`
	}
	preview := func(body map[string]any) result {
		t.Helper()
		var out result
		e.call("POST", "/api/v1/anonymize/preview", body, http.StatusOK, &out)
		return out
	}

	t.Run("stored exchange, default profile", func(t *testing.T) {
		out := preview(map[string]any{"exchange_id": exID})
		if len(out.Changes) == 0 {
			t.Fatal("no changes reported")
		}
		if !out.Exchange.Anonymized || strings.Contains(out.Exchange.Request.Body.Data, "Pa$$w0rd!x") || strings.Contains(out.Exchange.Request.Body.Data, "acme.io") {
			t.Errorf("exchange = %+v", out.Exchange.Request.Body)
		}
		// Changes describe what happened.
		var sawPassword bool
		for _, c := range out.Changes {
			if c.Location == "" || c.Source == "" || c.Before == c.After {
				t.Errorf("odd change %+v", c)
			}
			sawPassword = sawPassword || strings.Contains(c.Before, "Pa$$w0rd!x")
		}
		if !sawPassword {
			t.Errorf("the password change is missing: %+v", out.Changes)
		}
		// The stored exchange is untouched.
		var view model.ExchangeView
		e.call("GET", fmt.Sprintf("/api/v1/exchanges/%d", exID), nil, http.StatusOK, &view)
		if !strings.Contains(view.Request.Body.Data, "Pa$$w0rd!x") || view.Anonymized {
			t.Error("preview modified the stored exchange")
		}
	})
	t.Run("profile_id", func(t *testing.T) {
		var p model.AnonymizationProfile
		e.call("POST", "/api/v1/profiles", map[string]any{"name": "emails only", "detectors": []string{"email"}, "detector_strategy": "hash"}, http.StatusCreated, &p)
		out := preview(map[string]any{"profile_id": p.ID, "exchange_id": exID})
		if strings.Contains(out.Exchange.Request.Body.Data, "acme.io") || !strings.Contains(out.Exchange.Request.Body.Data, "Pa$$w0rd!x") {
			t.Errorf("body = %s", out.Exchange.Request.Body.Data)
		}
	})
	t.Run("inline profile and sample exchange", func(t *testing.T) {
		sample := model.ExchangeView{
			Endpoint: "POST /login",
			Request: model.RequestView{Method: "POST", Host: "sample.local", Path: "/login", Header: http.Header{"Content-Type": {"application/json"}},
				Body: model.BodyView{Encoding: "text", Data: `{"user":"jane@acme.io","pin":"1234"}`, Size: 35, ContentType: "application/json"}},
			Response: model.ResponseView{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: model.BodyView{Encoding: "none"}},
		}
		body := map[string]any{
			"profile": map[string]any{
				"name": "preview", "detectors": []string{"email"}, "detector_strategy": "redact",
				"rules": []map[string]any{{"name": "pin", "enabled": true, "target": "json", "selector": "$.pin", "strategy": "mask", "keep_suffix": 1}},
			},
			"exchange": sample,
		}
		out := preview(body)
		got := out.Exchange.Request.Body.Data
		if got != `{"user":"[REDACTED]","pin":"***4"}` {
			t.Errorf("body = %s", got)
		}
		if len(out.Changes) != 2 {
			t.Errorf("changes = %+v", out.Changes)
		}
	})
	t.Run("tokens are stable for an unsaved profile and follow the stored secret for a saved one", func(t *testing.T) {
		hashRule := []map[string]any{{"name": "cust", "enabled": true, "target": "json", "selector": "$..customer_id", "strategy": "hash"}}
		token := func(profile map[string]any) string {
			t.Helper()
			out := preview(map[string]any{"profile": profile, "exchange_id": exID})
			var m map[string]any
			if err := json.Unmarshal([]byte(out.Exchange.Request.Body.Data), &m); err != nil {
				t.Fatalf("body %q: %v", out.Exchange.Request.Body.Data, err)
			}
			return m["customer_id"].(string)
		}
		unsaved := map[string]any{"name": "scratch", "rules": hashRule}
		a, b := token(unsaved), token(unsaved)
		if a != b || !strings.HasPrefix(a, "h_") {
			t.Errorf("unsaved profile tokens: %q vs %q", a, b)
		}

		var saved model.AnonymizationProfile
		e.call("POST", "/api/v1/profiles", map[string]any{"name": "saved hash", "rules": hashRule}, http.StatusCreated, &saved)
		viaID := preview(map[string]any{"profile_id": saved.ID, "exchange_id": exID})
		var vm map[string]any
		_ = json.Unmarshal([]byte(viaID.Exchange.Request.Body.Data), &vm)
		inlineSameID := token(map[string]any{"id": saved.ID, "name": "saved hash", "rules": hashRule})
		if inlineSameID != vm["customer_id"] {
			t.Errorf("an inline profile with the stored id must reuse the stored secret: %q vs %q", inlineSameID, vm["customer_id"])
		}
		if inlineSameID == a {
			t.Error("two different secrets produced the same token")
		}
	})
	t.Run("errors", func(t *testing.T) {
		sample := map[string]any{"request": map[string]any{"method": "GET", "path": "/", "body": map[string]any{"encoding": "none"}}, "response": map[string]any{"status": 200, "body": map[string]any{"encoding": "none"}}}
		badB64 := map[string]any{"request": map[string]any{"method": "GET", "path": "/", "body": map[string]any{"encoding": "base64", "data": "!!!"}}, "response": map[string]any{"status": 200, "body": map[string]any{"encoding": "none"}}}
		tests := []struct {
			name   string
			body   map[string]any
			status int
		}{
			{"neither exchange nor id", map[string]any{}, 400},
			{"both", map[string]any{"exchange_id": exID, "exchange": sample}, 400},
			// References inside the request are 400s, like everywhere else.
			{"unknown exchange", map[string]any{"exchange_id": 987654}, 400},
			{"unknown profile", map[string]any{"profile_id": "nope", "exchange_id": exID}, 400},
			{"bad base64", map[string]any{"exchange": badB64}, 400},
			{"invalid inline profile", map[string]any{"profile": map[string]any{"name": "x", "rules": []map[string]any{{"name": "bad", "enabled": true, "target": "json", "selector": "$.[[", "strategy": "hash"}}}, "exchange_id": exID}, 400},
			{"unknown field", map[string]any{"exchange_id": exID, "bogus": true}, 400},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) { e.errorOf("POST", "/api/v1/anonymize/preview", tc.body, tc.status) })
		}
	})
}
