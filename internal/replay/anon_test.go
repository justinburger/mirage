package replay

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/store"
)

var emailRE = regexp.MustCompile(`^[A-Za-z0-9._-]+@(example\.(com|net|org))$`)

// emailsIn extracts the "email" member of every JSON body the target saw.
func emailsIn(t *testing.T, reqs []seen) []string {
	t.Helper()
	var out []string
	for _, s := range reqs {
		var v struct {
			Email string `json:"email"`
		}
		if err := json.Unmarshal(s.body, &v); err != nil {
			t.Fatalf("body %q: %v", s.body, err)
		}
		out = append(out, v.Email)
	}
	return out
}

func userRecording(t *testing.T, st store.Store) string {
	t.Helper()
	body := `{"email":"jane.doe@acme.io","name":"Jane","password":"hunter2"}`
	return seedRecording(t, st,
		rx{at: 0, method: "POST", path: "/signup", body: body, query: "token=abc123&x=1",
			header: map[string]string{"Content-Type": "application/json", "Authorization": "Bearer recorded-secret"}},
		rx{at: 10 * time.Millisecond, method: "POST", path: "/login", body: body,
			header: map[string]string{"Content-Type": "application/json"}},
	)
}

func TestRunAnonymizesOnTheWire(t *testing.T) {
	st := openStore(t)
	rec := userRecording(t, st)
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, target.URL)
	cfg.ProfileID = model.DefaultProfileID
	sum, _ := mustRun(t, st, cfg)
	if sum.Completed != 2 {
		t.Fatalf("completed %d failed %d", sum.Completed, sum.Failed)
	}
	reqs := target.requests()
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].uri < reqs[j].uri })
	emails := emailsIn(t, reqs)
	for _, e := range emails {
		if e == "jane.doe@acme.io" || !emailRE.MatchString(e) {
			t.Errorf("target saw email %q, want a fake one", e)
		}
	}
	if emails[0] != emails[1] {
		t.Errorf("the same email must map to the same fake in every request: %v", emails)
	}
	for _, s := range reqs {
		if bytes.Contains(s.body, []byte("hunter2")) {
			t.Errorf("password reached the target: %s", s.body)
		}
	}
	signup := reqs[1]
	if !strings.HasPrefix(signup.uri, "/signup?") || strings.Contains(signup.uri, "abc123") {
		t.Errorf("query token should be hashed: %s", signup.uri)
	}
	if got := signup.header.Get("Authorization"); !strings.HasPrefix(got, "Bearer h_") || strings.Contains(got, "recorded-secret") {
		t.Errorf("Authorization = %q, want a hashed bearer token", got)
	}
	if signup.header.Get("X-Mirage-Replay") != "cli" {
		t.Error("the replay marker must survive anonymization")
	}
	if int64(len(reqs[0].body)+len(reqs[1].body)) != sum.BytesSent {
		t.Errorf("BytesSent %d does not match the anonymized bodies", sum.BytesSent)
	}
	// The stored recording is untouched.
	var stored int
	_ = st.IterateExchanges(context.Background(), rec, model.IterOptions{RequestBodies: true}, func(e *model.Exchange) error {
		if bytes.Contains(e.Request.Body, []byte("jane.doe@acme.io")) {
			stored++
		}
		return nil
	})
	if stored != 2 {
		t.Errorf("recording changed: %d of 2 bodies keep the original email", stored)
	}
}

func TestRunSetHeadersAreNotAnonymized(t *testing.T) {
	// The profile hashes Authorization, but an Authorization the user sets
	// for the target must reach it as given.
	st := openStore(t)
	rec := userRecording(t, st)
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, target.URL)
	cfg.ProfileID = model.DefaultProfileID
	cfg.SetHeaders = map[string]string{"Authorization": "Bearer staging-token", "X-Env": "stg"}
	_, _ = mustRun(t, st, cfg)
	for _, s := range target.requests() {
		if s.header.Get("Authorization") != "Bearer staging-token" || s.header.Get("X-Env") != "stg" {
			t.Errorf("headers = %v", s.header)
		}
	}
}

func TestRunWithoutProfileSendsRequestsAsStored(t *testing.T) {
	st := openStore(t)
	rec := userRecording(t, st)
	target := newRecorder(t, nil)
	_, _ = mustRun(t, st, baseCfg(rec, target.URL))
	for _, e := range emailsIn(t, target.requests()) {
		if e != "jane.doe@acme.io" {
			t.Errorf("email = %q", e)
		}
	}
}

func TestRunVaryClones(t *testing.T) {
	st := openStore(t)
	rec := userRecording(t, st)

	run := func(vary bool) []string {
		target := newRecorder(t, nil)
		cfg := baseCfg(rec, target.URL)
		cfg.ProfileID = model.DefaultProfileID
		cfg.Amplify = 3
		cfg.VaryClones = vary
		sum, _ := mustRun(t, st, cfg)
		if sum.Sent != 6 || sum.Completed != 6 {
			t.Fatalf("vary=%v: sent %d completed %d", vary, sum.Sent, sum.Completed)
		}
		return emailsIn(t, target.requests())
	}

	same := run(false)
	if distinct(same) != 1 {
		t.Errorf("without VaryClones all six requests carry one identity, got %v", same)
	}

	varied := run(true)
	counts := map[string]int{}
	for _, e := range varied {
		if !emailRE.MatchString(e) {
			t.Errorf("not a fake email: %q", e)
		}
		counts[e]++
	}
	// Three copies, three identities; within a copy the same email is the
	// same identity in both recorded requests, so each shows up exactly twice.
	if len(counts) != 3 {
		t.Fatalf("%d distinct identities, want 3: %v", len(counts), counts)
	}
	for e, n := range counts {
		if n != 2 {
			t.Errorf("identity %q appears %d times, want 2 (once per recorded request)", e, n)
		}
	}
	// Copy 0 keeps the base identity, the one a plain run produces.
	if counts[same[0]] != 2 {
		t.Errorf("the base identity %q is missing from the clones %v", same[0], counts)
	}
}

func distinct(ss []string) int {
	m := map[string]bool{}
	for _, s := range ss {
		m[s] = true
	}
	return len(m)
}

func TestRunAnonymizedGzipBodies(t *testing.T) {
	// A body recorded as gzip is decoded by the recorder; replay anonymizes
	// the plain body and compresses the result again.
	st := openStore(t)
	rec := seedRecording(t, st, rx{method: "POST", path: "/events", encoding: "gzip",
		body:   `{"email":"jane.doe@acme.io","n":1}`,
		header: map[string]string{"Content-Type": "application/json"}})
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, target.URL)
	cfg.ProfileID = model.DefaultProfileID
	sum, _ := mustRun(t, st, cfg)
	if sum.Completed != 1 {
		t.Fatalf("completed %d", sum.Completed)
	}
	s := target.requests()[0]
	if s.header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", s.header.Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(bytes.NewReader(s.body))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	var v struct{ Email string }
	if err := json.Unmarshal(plain, &v); err != nil || v.Email == "jane.doe@acme.io" || !emailRE.MatchString(v.Email) {
		t.Errorf("decompressed body = %s (%v)", plain, err)
	}
	if sum.BytesSent != int64(len(s.body)) {
		t.Errorf("BytesSent %d, wire %d", sum.BytesSent, len(s.body))
	}
}

func TestRunAnonymizedPathWithTargetPrefix(t *testing.T) {
	// A path rule works on the recorded path; the target prefix is added
	// afterwards and is not part of what the rules see.
	st := openStore(t)
	prof, err := st.PutProfile(context.Background(), model.AnonymizationProfile{
		Name: "paths", Secret: "s3",
		Rules: []model.AnonymizationRule{{Name: "user id", Enabled: true, Target: model.TargetPath,
			Selector: "/users/{id}", Scope: model.ScopeRequest, Strategy: model.StrategyHash}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := seedRecording(t, st, rx{path: "/users/12345", query: "a=b"})
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, target.URL+"/api")
	cfg.ProfileID = prof.ID
	sum, _ := mustRun(t, st, cfg)
	if sum.Completed != 1 {
		t.Fatalf("completed %d failed %d", sum.Completed, sum.Failed)
	}
	u, _ := url.Parse("http://x" + target.uris()[0])
	if !strings.HasPrefix(u.Path, "/api/users/h_") || strings.Contains(u.Path, "12345") || u.RawQuery != "a=b" {
		t.Errorf("target saw %s", target.uris()[0])
	}
}

func TestRunProfileDeletedBeforeTheRunFailsAtLoad(t *testing.T) {
	// Create checks the profile, but it can be deleted before the job starts.
	st := openStore(t)
	rec := userRecording(t, st)
	target := newRecorder(t, nil)
	cfg := baseCfg(rec, target.URL)
	cfg.ProfileID = "prof_gone"
	_, _, err := runCfg(t, st, cfg)
	if err == nil || !strings.Contains(err.Error(), "prof_gone") {
		t.Errorf("err = %v", err)
	}
	if target.count() != 0 {
		t.Error("nothing may be sent when the profile cannot be loaded")
	}
}

func BenchmarkBuildRequest(b *testing.B) {
	st := openStore(b)
	rec := seedRecording(b, st, rx{method: "POST", path: "/users/1234/orders", query: "a=1&b=2",
		body:   `{"email":"jane.doe@acme.io","items":[{"sku":"A1","qty":2},{"sku":"B2","qty":1}],"note":"please leave at the door"}`,
		header: map[string]string{"Content-Type": "application/json", "Accept": "*/*", "User-Agent": "bench/1.0", "Authorization": "Bearer t"}})
	for _, profile := range []string{"", model.DefaultProfileID} {
		name := "plain"
		if profile != "" {
			name = "anonymized"
		}
		b.Run(name, func(b *testing.B) {
			cfg := baseCfg(rec, "http://target:8080/api")
			cfg.ProfileID = profile
			r, err := newRunner(st, cfg, RunOptions{Collector: newTestCollector()})
			if err != nil {
				b.Fatal(err)
			}
			if err := r.buildEngines(context.Background()); err != nil {
				b.Fatal(err)
			}
			prep, err := r.load(context.Background())
			if err != nil {
				b.Fatal(err)
			}
			j := job{it: &prep.items[0]}
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := r.build(ctx, j); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
