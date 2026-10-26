package api

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// documentedRoutes reads the route tables of docs/API.md: every table row that
// starts with an HTTP method and a path in backticks.
func documentedRoutes(t *testing.T) map[string]bool {
	t.Helper()
	data, err := os.ReadFile("../../docs/API.md")
	if err != nil {
		t.Fatalf("docs/API.md is the specification of this package: %v", err)
	}
	row := regexp.MustCompile("^\\|\\s*(GET|POST|PUT|PATCH|DELETE)\\s*\\|\\s*`([^`]+)`")
	routes := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		m := row.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		path, _, _ := strings.Cut(m[2], "?") // query strings are not part of the route
		routes[m[1]+" "+path] = true
	}
	if len(routes) < 40 {
		t.Fatalf("parsed only %d routes from docs/API.md; has its table format changed?", len(routes))
	}
	return routes
}

// routeCase is one request of the walk through the API.
type routeCase struct {
	method string
	doc    string // the route as documented, with {id} placeholders
	path   func(ids map[string]string) string
	body   func(ids map[string]string) any
	status int
	// save extracts values for later cases from the response body.
	save func(ids map[string]string, body []byte)
	// kind: "" JSON, "raw" not JSON (metrics, download), "empty" 204, "stream" SSE.
	kind string
}

const miniHAR = `{"log":{"version":"1.2","creator":{"name":"walk","version":"1"},"entries":[
 {"startedDateTime":"2026-01-01T00:00:00Z","time":12,"request":{"method":"GET","url":"https://api.example.com/users/42?expand=orders","headers":[{"name":"Authorization","value":"Bearer abc"}]},"response":{"status":200,"content":{"mimeType":"application/json","text":"{\"id\":42,\"email\":\"jane@acme.io\"}"}}},
 {"startedDateTime":"2026-01-01T00:00:01Z","time":30,"request":{"method":"POST","url":"https://api.example.com/orders","postData":{"mimeType":"application/json","text":"{\"qty\":2}"}},"response":{"status":503,"content":{"mimeType":"text/plain","text":"busy"}}}
]}}`

func static(p string) func(map[string]string) string {
	return func(map[string]string) string { return p }
}

func withID(format, key string) func(map[string]string) string {
	return func(ids map[string]string) string { return fmt.Sprintf(format, ids[key]) }
}

func saveID(key string) func(map[string]string, []byte) {
	return func(ids map[string]string, body []byte) {
		var m struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(body, &m)
		ids[key] = m.ID
	}
}

func jsonBody(v any) func(map[string]string) any { return func(map[string]string) any { return v } }

func walkCases() []routeCase {
	const v1 = "/api/v1"
	profileBody := map[string]any{"name": "walk profile", "detectors": []string{"email"}, "detector_strategy": "fake"}
	faultBody := map[string]any{"name": "walk fault", "enabled": true, "probability": 0.5, "type": "error", "status": 503}
	return []routeCase{
		// System.
		{method: "GET", doc: v1 + "/health", path: static(v1 + "/health"), status: 200},
		{method: "GET", doc: v1 + "/info", path: static(v1 + "/info"), status: 200},
		{method: "GET", doc: "/metrics", path: static("/metrics"), status: 200, kind: "raw"},

		// Recordings: create empty, import a HAR, inspect.
		{method: "POST", doc: v1 + "/recordings", path: static(v1 + "/recordings"), body: jsonBody(map[string]any{"name": "walk empty"}), status: 201, save: saveID("rec_empty")},
		{method: "POST", doc: v1 + "/recordings/import", path: static(v1 + "/recordings/import?name=walk%20import"), body: jsonBody(miniHAR), status: 201, save: saveID("rec")},
		{method: "GET", doc: v1 + "/recordings", path: static(v1 + "/recordings"), status: 200},
		{method: "GET", doc: v1 + "/recordings/{id}", path: withID(v1+"/recordings/%s", "rec"), status: 200},
		{method: "PATCH", doc: v1 + "/recordings/{id}", path: withID(v1+"/recordings/%s", "rec"), body: jsonBody(map[string]any{"description": "walked"}), status: 200},
		{method: "GET", doc: v1 + "/recordings/{id}/endpoints", path: withID(v1+"/recordings/%s/endpoints", "rec"), status: 200},
		{method: "GET", doc: v1 + "/recordings/{id}/exchanges", path: withID(v1+"/recordings/%s/exchanges?limit=1&desc=1", "rec"), status: 200,
			save: func(ids map[string]string, body []byte) {
				var page struct{ Items []struct{ ID int64 } }
				_ = json.Unmarshal(body, &page)
				ids["ex"] = fmt.Sprint(page.Items[0].ID)
			}},
		{method: "GET", doc: v1 + "/exchanges/{id}", path: withID(v1+"/exchanges/%s", "ex"), status: 200},
		{method: "GET", doc: v1 + "/recordings/{id}/export.har", path: withID(v1+"/recordings/%s/export.har", "rec"), status: 200, kind: "raw"},

		// Anonymization.
		{method: "POST", doc: v1 + "/recordings/{id}/anonymize", path: withID(v1+"/recordings/%s/anonymize", "rec"), body: jsonBody(map[string]any{"name": "walk (anon)"}), status: 201, save: saveID("rec_anon")},
		{method: "POST", doc: v1 + "/profiles", path: static(v1 + "/profiles"), body: jsonBody(profileBody), status: 201, save: saveID("prof")},
		{method: "GET", doc: v1 + "/profiles", path: static(v1 + "/profiles"), status: 200},
		{method: "GET", doc: v1 + "/profiles/{id}", path: withID(v1+"/profiles/%s", "prof"), status: 200},
		{method: "PUT", doc: v1 + "/profiles/{id}", path: withID(v1+"/profiles/%s", "prof"), body: jsonBody(map[string]any{"name": "walk profile", "description": "edited", "regenerate_secret": true}), status: 200},
		{method: "GET", doc: v1 + "/anonymize/detectors", path: static(v1 + "/anonymize/detectors"), status: 200},
		{method: "POST", doc: v1 + "/anonymize/preview", path: static(v1 + "/anonymize/preview"),
			body: func(ids map[string]string) any {
				return map[string]any{"profile_id": ids["prof"], "exchange_id": mustAtoi(ids["ex"])}
			}, status: 200},

		// Services.
		{method: "POST", doc: v1 + "/services", path: static(v1 + "/services"), body: jsonBody(validService("walk", ":0")), status: 201, save: saveID("svc")},
		{method: "GET", doc: v1 + "/services", path: static(v1 + "/services"), status: 200},
		{method: "GET", doc: v1 + "/services/{id}", path: withID(v1+"/services/%s", "svc"), status: 200},
		{method: "PUT", doc: v1 + "/services/{id}", path: withID(v1+"/services/%s", "svc"), body: jsonBody(validService("walk", ":0")), status: 200},
		{method: "POST", doc: v1 + "/services/{id}/mode", path: withID(v1+"/services/%s/mode", "svc"),
			body: func(ids map[string]string) any {
				return map[string]any{"mode": "virtualize", "recording_id": ids["rec"]}
			}, status: 200},
		{method: "POST", doc: v1 + "/services/{id}/enable", path: withID(v1+"/services/%s/enable", "svc"), status: 200},
		{method: "POST", doc: v1 + "/services/{id}/reload", path: withID(v1+"/services/%s/reload", "svc"), status: 200},
		{method: "GET", doc: v1 + "/services/{id}/stats", path: withID(v1+"/services/%s/stats", "svc"), status: 200},
		{method: "GET", doc: v1 + "/services/{id}/series", path: withID(v1+"/services/%s/series?since=0", "svc"), status: 200},
		{method: "POST", doc: v1 + "/services/{id}/disable", path: withID(v1+"/services/%s/disable", "svc"), status: 200},

		// Faults.
		{method: "POST", doc: v1 + "/faults", path: static(v1 + "/faults"), body: jsonBody(faultBody), status: 201, save: saveID("flt")},
		{method: "GET", doc: v1 + "/faults", path: static(v1 + "/faults"), status: 200},
		{method: "GET", doc: v1 + "/faults/{id}", path: withID(v1+"/faults/%s", "flt"), status: 200},
		{method: "PUT", doc: v1 + "/faults/{id}", path: withID(v1+"/faults/%s", "flt"), body: jsonBody(faultBody), status: 200},

		// Replays.
		{method: "POST", doc: v1 + "/replays", path: static(v1 + "/replays"),
			body: func(ids map[string]string) any {
				return map[string]any{"name": "walk replay", "config": map[string]any{"recording_id": ids["rec"], "target": "http://staging.example.com", "mode": "timeline"}}
			}, status: 201, save: saveID("rpl")},
		{method: "GET", doc: v1 + "/replays", path: static(v1 + "/replays"), status: 200},
		{method: "GET", doc: v1 + "/replays/{id}", path: withID(v1+"/replays/%s", "rpl"), status: 200},
		{method: "POST", doc: v1 + "/replays/{id}/start", path: withID(v1+"/replays/%s/start", "rpl"), status: 200},
		{method: "GET", doc: v1 + "/replays/{id}/series", path: withID(v1+"/replays/%s/series?since=0", "rpl"), status: 200},
		{method: "POST", doc: v1 + "/replays/{id}/stop", path: withID(v1+"/replays/%s/stop", "rpl"), status: 200},
		{method: "POST", doc: v1 + "/replays/{id}/clone", path: withID(v1+"/replays/%s/clone", "rpl"), body: jsonBody(map[string]any{"name": "walk clone"}), status: 201, save: saveID("rpl_clone")},

		// Live metrics.
		{method: "GET", doc: v1 + "/stats", path: static(v1 + "/stats"), status: 200},
		{method: "GET", doc: v1 + "/stream", path: static(v1 + "/stream"), status: 200, kind: "stream"},

		// Clean-up, dependants first.
		{method: "DELETE", doc: v1 + "/replays/{id}", path: withID(v1+"/replays/%s", "rpl_clone"), status: 204, kind: "empty"},
		{method: "DELETE", doc: v1 + "/replays/{id}", path: withID(v1+"/replays/%s", "rpl"), status: 204, kind: "empty"},
		{method: "DELETE", doc: v1 + "/faults/{id}", path: withID(v1+"/faults/%s", "flt"), status: 204, kind: "empty"},
		{method: "DELETE", doc: v1 + "/profiles/{id}", path: withID(v1+"/profiles/%s", "prof"), status: 204, kind: "empty"},
		{method: "DELETE", doc: v1 + "/services/{id}", path: withID(v1+"/services/%s", "svc"), status: 204, kind: "empty"},
		{method: "DELETE", doc: v1 + "/recordings/{id}", path: withID(v1+"/recordings/%s", "rec_anon"), status: 204, kind: "empty"},
		{method: "DELETE", doc: v1 + "/recordings/{id}", path: withID(v1+"/recordings/%s", "rec"), status: 204, kind: "empty"},
	}
}

func mustAtoi(s string) int64 {
	var n int64
	_, _ = fmt.Sscan(s, &n)
	return n
}

// TestEveryDocumentedRouteExists walks every route of docs/API.md with a real
// request and checks that the documented ones and the walked ones are the same
// set, so neither a missing handler nor a route missing from the walk goes
// unnoticed.
func TestEveryDocumentedRouteExists(t *testing.T) {
	cases := walkCases()
	documented := documentedRoutes(t)

	walked := map[string]bool{}
	for _, c := range cases {
		key := c.method + " " + c.doc
		walked[key] = true
		if !documented[key] {
			t.Errorf("walk case %q is not in docs/API.md", key)
		}
	}
	var missing []string
	for key := range documented {
		if !walked[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("documented routes the walk does not cover:\n  %s", strings.Join(missing, "\n  "))
	}

	e := newEnv(t)
	e.run()
	ids := map[string]string{}
	for i, c := range cases {
		path := c.path(ids)
		name := fmt.Sprintf("%02d %s %s", i, c.method, c.doc)
		t.Run(name, func(t *testing.T) {
			if strings.Contains(path, "//") || strings.Contains(path, "/?") || strings.HasSuffix(path, "/") {
				t.Fatalf("a placeholder was not filled: %s", path)
			}
			if c.kind == "stream" {
				s, resp := e.openStream(path, nil)
				if resp.StatusCode != c.status {
					t.Fatalf("status %d, want %d", resp.StatusCode, c.status)
				}
				s.expect("services")
				return
			}
			var body any
			if c.body != nil {
				body = c.body(ids)
			}
			resp, raw := e.do(c.method, path, body)
			if resp.StatusCode != c.status {
				t.Fatalf("%s %s = %d, want %d; body: %s", c.method, path, resp.StatusCode, c.status, raw)
			}
			if bytesContainRouterMiss(raw) {
				t.Fatalf("answered by the router fallback, not a handler: %s", raw)
			}
			switch c.kind {
			case "empty":
				if len(raw) != 0 {
					t.Errorf("204 with a body: %q", raw)
				}
			case "raw":
			default:
				if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
					t.Errorf("Content-Type = %q", ct)
				}
				if !json.Valid(raw) {
					t.Errorf("not valid JSON: %s", raw)
				}
			}
			if c.save != nil {
				c.save(ids, raw)
			}
		})
	}
	for _, key := range []string{"rec", "rec_empty", "ex", "rec_anon", "prof", "svc", "flt", "rpl", "rpl_clone"} {
		if ids[key] == "" || ids[key] == "0" {
			t.Errorf("the walk never captured %q", key)
		}
	}
}

func bytesContainRouterMiss(body []byte) bool {
	s := string(body)
	return strings.Contains(s, "no such endpoint") || strings.Contains(s, "is not allowed for")
}
