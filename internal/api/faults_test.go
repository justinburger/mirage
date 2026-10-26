package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func faultBody(name string) map[string]any {
	return map[string]any{
		"name": name, "enabled": true, "order": 0, "methods": []string{"POST"}, "path_pattern": "/checkout/**",
		"probability": 1, "type": "error", "status": 503, "body": `{"error":"overloaded"}`,
		"response_headers": map[string]string{"Retry-After": "1"},
	}
}

// fire sends a request through the injector and reports whether a fault fired.
func fire(e *env, method, path string) bool {
	return e.inj.Decide("svc_x", httptest.NewRequest(method, path, nil)) != nil
}

func TestFaultCRUD(t *testing.T) {
	e := newEnv(t)

	_, raw := e.do("GET", "/api/v1/faults", nil)
	if strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("empty list = %s", raw)
	}

	var created FaultRuleWithCount
	resp, body := e.do("POST", "/api/v1/faults", faultBody("503s on checkout"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	m := asMap(t, body)
	for _, k := range []string{"id", "name", "enabled", "order", "methods", "path_pattern", "probability", "type", "status", "body", "response_headers", "created_at", "updated_at", "fired"} {
		if _, ok := m[k]; !ok {
			t.Errorf("response lacks %q: %s", k, body)
		}
	}
	if _, nested := m["FaultRule"]; nested {
		t.Errorf("rule must be flattened: %s", body)
	}
	e.call("GET", "/api/v1/faults/"+m["id"].(string), nil, http.StatusOK, &created)
	if created.Name != "503s on checkout" || created.Fired != 0 || !strings.HasPrefix(created.ID, "flt_") {
		t.Fatalf("created = %+v", created)
	}

	t.Run("the injector follows the store", func(t *testing.T) {
		if !fire(e, "POST", "/checkout/pay") {
			t.Fatal("the new rule is not active")
		}
		if fire(e, "GET", "/checkout/pay") || fire(e, "POST", "/other") {
			t.Error("the rule fires outside its scope")
		}
	})
	t.Run("fire counts", func(t *testing.T) {
		before := e.inj.Counts()[created.ID]
		for i := 0; i < 3; i++ {
			fire(e, "POST", "/checkout/pay")
		}
		var list []FaultRuleWithCount
		e.call("GET", "/api/v1/faults", nil, http.StatusOK, &list)
		if len(list) != 1 || list[0].Fired != before+3 {
			t.Errorf("fired = %d, want %d", list[0].Fired, before+3)
		}
		var one FaultRuleWithCount
		e.call("GET", "/api/v1/faults/"+created.ID, nil, http.StatusOK, &one)
		if one.Fired != before+3 {
			t.Errorf("single fired = %d", one.Fired)
		}
	})
	t.Run("update", func(t *testing.T) {
		counts := e.inj.Counts()[created.ID]
		body := faultBody("renamed")
		body["probability"] = 0
		body["fired"] = 99 // echo field
		var out FaultRuleWithCount
		e.call("PUT", "/api/v1/faults/"+created.ID, body, http.StatusOK, &out)
		if out.Name != "renamed" || out.Probability != 0 || out.ID != created.ID {
			t.Errorf("out = %+v", out)
		}
		if out.Fired != counts {
			t.Errorf("fired = %d, want the real count %d (not the echoed one)", out.Fired, counts)
		}
		if fire(e, "POST", "/checkout/pay") {
			t.Error("the update did not reach the injector")
		}
		body["id"] = "other"
		e.call("PUT", "/api/v1/faults/"+created.ID, body, http.StatusOK, &out)
		if out.ID != created.ID {
			t.Errorf("ID = %q, the URL must win", out.ID)
		}
	})
	t.Run("delete", func(t *testing.T) {
		resp, b := e.do("DELETE", "/api/v1/faults/"+created.ID, nil)
		if resp.StatusCode != http.StatusNoContent || len(b) != 0 {
			t.Fatalf("DELETE = %d %q", resp.StatusCode, b)
		}
		if n := len(e.inj.Rules()); n != 0 {
			t.Errorf("injector still has %d rules", n)
		}
		e.errorOf("GET", "/api/v1/faults/"+created.ID, nil, http.StatusNotFound)
		e.errorOf("DELETE", "/api/v1/faults/"+created.ID, nil, http.StatusNotFound)
		e.errorOf("PUT", "/api/v1/faults/"+created.ID, faultBody("x"), http.StatusNotFound)
	})
}

// "status" is an echo field of services, but for a fault rule it is the HTTP
// status to answer with, and must not be dropped.
func TestFaultStatusIsARealField(t *testing.T) {
	e := newEnv(t)
	body := faultBody("teapot")
	body["status"] = 418
	var out FaultRuleWithCount
	e.call("POST", "/api/v1/faults", body, http.StatusCreated, &out)
	if out.Status != 418 {
		t.Errorf("status = %d, want 418", out.Status)
	}
	body["status"] = 599
	e.call("PUT", "/api/v1/faults/"+out.ID, body, http.StatusOK, &out)
	if out.Status != 599 {
		t.Errorf("status = %d after update", out.Status)
	}
	// "fired" is the echo field; a service's "status" is not accepted here...
	body["fired"] = 12
	e.call("PUT", "/api/v1/faults/"+out.ID, body, http.StatusOK, &out)
	// ...and unknown members still are not.
	body["bogus"] = 1
	e.errorOf("PUT", "/api/v1/faults/"+out.ID, body, http.StatusBadRequest)
}

func TestFaultOrderingAndDefaults(t *testing.T) {
	e := newEnv(t)
	for i, name := range []string{"c", "a", "b"} {
		body := faultBody(name)
		body["order"] = []int{2, 0, 1}[i]
		e.call("POST", "/api/v1/faults", body, http.StatusCreated, nil)
	}
	var list []FaultRuleWithCount
	e.call("GET", "/api/v1/faults", nil, http.StatusOK, &list)
	var names []string
	for _, r := range list {
		names = append(names, r.Name)
	}
	if got := strings.Join(names, ""); got != "abc" {
		t.Errorf("order = %s, want abc (by order, then name)", got)
	}

	// Defaults of the model apply: an error rule without status is a 503.
	var r FaultRuleWithCount
	e.call("POST", "/api/v1/faults", map[string]any{"name": "default status", "probability": 0.5, "type": "error"}, http.StatusCreated, &r)
	if r.Status != 503 {
		t.Errorf("status = %d, want 503", r.Status)
	}
}

func TestFaultValidation(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name   string
		body   any
		status int
		want   string
	}{
		{"missing name", map[string]any{"probability": 1, "type": "abort"}, 400, "name is required"},
		{"probability out of range", map[string]any{"name": "x", "probability": 1.5, "type": "abort"}, 400, "probability"},
		{"unknown type", map[string]any{"name": "x", "probability": 1, "type": "gremlins"}, 400, "type"},
		{"latency without delay", map[string]any{"name": "x", "probability": 1, "type": "latency"}, 400, "delay"},
		{"error with a bad status", map[string]any{"name": "x", "probability": 1, "type": "error", "status": 42}, 400, "status"},
		{"throttle without rate", map[string]any{"name": "x", "probability": 1, "type": "throttle"}, 400, "bytes_per_second"},
		{"bad path pattern", map[string]any{"name": "x", "probability": 1, "type": "abort", "path_pattern": "no-slash"}, 400, "path_pattern"},
		{"bad regex", map[string]any{"name": "x", "probability": 1, "type": "abort", "path_pattern": "re:("}, 400, "regex"},
		{"header names the injector refuses", map[string]any{"name": "x", "probability": 1, "type": "abort", "headers": map[string]string{"bad header": "v"}}, 400, "header"},
		{"unknown field", map[string]any{"name": "x", "probability": 1, "type": "abort", "bogus": 1}, 400, "bogus"},
		{"bad duration", `{"name":"x","probability":1,"type":"latency","delay":"forever"}`, 400, "duration"},
		{"bad id", map[string]any{"id": "no spaces", "name": "x", "probability": 1, "type": "abort"}, 400, "id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eb := e.errorOf("POST", "/api/v1/faults", tc.body, tc.status)
			joined := strings.ToLower(eb.Error + " " + strings.Join(eb.Problems, " "))
			if !strings.Contains(joined, strings.ToLower(tc.want)) {
				t.Errorf("error %q lacks %q", joined, tc.want)
			}
		})
	}
	if list, _ := e.st.ListFaultRules(context.Background()); len(list) != 0 {
		t.Errorf("%d invalid rules were stored", len(list))
	}
	if n := len(e.inj.Rules()); n != 0 {
		t.Errorf("injector has %d rules", n)
	}
}

func TestFaultClientIDConflict(t *testing.T) {
	e := newEnv(t)
	body := faultBody("one")
	body["id"] = "my-rule"
	e.call("POST", "/api/v1/faults", body, http.StatusCreated, nil)
	body["name"] = "two"
	e.errorOf("POST", "/api/v1/faults", body, http.StatusConflict)
	if got, _ := e.st.GetFaultRule(context.Background(), "my-rule"); got.Name != "one" {
		t.Errorf("POST with an existing id overwrote the rule: %+v", got)
	}
}

func TestFaultCountsSurviveReloads(t *testing.T) {
	e := newEnv(t)
	var a, b FaultRuleWithCount
	e.call("POST", "/api/v1/faults", faultBody("a"), http.StatusCreated, &a)
	fire(e, "POST", "/checkout/x")
	fire(e, "POST", "/checkout/x")
	e.call("POST", "/api/v1/faults", faultBody("b"), http.StatusCreated, &b)
	var list []FaultRuleWithCount
	e.call("GET", "/api/v1/faults", nil, http.StatusOK, &list)
	if list[0].ID != a.ID || list[0].Fired < 2 {
		t.Errorf("rule a lost its count when another rule was added: %+v", list[0])
	}
}

func TestConcurrentFaultChangesKeepInjectorInSync(t *testing.T) {
	e := newEnv(t)
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := faultBody(strings.Repeat("r", i+1))
			resp, raw := e.do("POST", "/api/v1/faults", body)
			if resp.StatusCode != http.StatusCreated {
				t.Errorf("POST %d: %d %s", i, resp.StatusCode, raw)
			}
		}(i)
	}
	wg.Wait()
	stored, _ := e.st.ListFaultRules(context.Background())
	if len(stored) != n || len(e.inj.Rules()) != n {
		t.Fatalf("store has %d rules, injector %d, want %d", len(stored), len(e.inj.Rules()), n)
	}
	for i, r := range e.inj.Rules() {
		if r.ID != stored[i].ID {
			t.Fatalf("rule %d differs between store and injector", i)
		}
	}
	// Deleting concurrently keeps them in step too.
	for _, r := range stored[:10] {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			e.do("DELETE", "/api/v1/faults/"+id, nil)
		}(r.ID)
	}
	wg.Wait()
	stored, _ = e.st.ListFaultRules(context.Background())
	if len(stored) != 10 || len(e.inj.Rules()) != 10 {
		t.Errorf("after deletes: store %d, injector %d, want 10", len(stored), len(e.inj.Rules()))
	}
}

func TestFaultTypesRoundTrip(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []map[string]any{
		{"name": "latency", "probability": 0.3, "type": "latency", "delay": "250ms", "jitter": "50ms"},
		{"name": "timeout", "probability": 0.01, "type": "timeout", "delay": "30s"},
		{"name": "abort", "probability": 0.02, "type": "abort"},
		{"name": "throttle", "probability": 1, "type": "throttle", "bytes_per_second": 1024},
		{"name": "ms number", "probability": 1, "type": "latency", "delay": 1500},
	} {
		var out model.FaultRule
		e.call("POST", "/api/v1/faults", tc, http.StatusCreated, &out)
		if out.Type != model.FaultType(tc["type"].(string)) {
			t.Errorf("%v: type = %q", tc["name"], out.Type)
		}
	}
	var list []model.FaultRule
	e.call("GET", "/api/v1/faults", nil, http.StatusOK, &list)
	byName := map[string]model.FaultRule{}
	for _, r := range list {
		byName[r.Name] = r
	}
	if byName["latency"].Delay.D().Milliseconds() != 250 || byName["latency"].Jitter.D().Milliseconds() != 50 {
		t.Errorf("latency = %+v", byName["latency"])
	}
	if byName["ms number"].Delay.D().Milliseconds() != 1500 {
		t.Errorf("a JSON number must mean milliseconds: %+v", byName["ms number"])
	}
}
