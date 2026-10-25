package store

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// fakeClock is a settable clock for the store's timestamps.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(s *SQLite) *fakeClock {
	c := &fakeClock{t: baseTime}
	s.now = c.Now
	return c
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fullService returns a service with every field set to a non-default value.
func fullService() model.Service {
	return model.Service{
		Name: "payments", ListenAddr: "127.0.0.1:9101", Upstream: "https://api.example.com",
		Mode: model.ModeHybrid, Enabled: true, RecordingID: "rec_abc",
		AnonymizeCapture: true, ProfileID: "prof_x",
		EndpointPatterns: []string{"/users/{id}", "/orders/{order}/lines"},
		PreserveHost:     true, InsecureSkipVerify: true,
		TLS:          &model.TLSConfig{SelfSigned: true, CertFile: "c.pem", KeyFile: "k.pem"},
		Timeout:      model.Duration(1500 * time.Millisecond),
		MaxBodyBytes: -1,
		Match: model.MatchConfig{
			Query: model.QuerySubset, IgnoreParams: []string{"ts", "nonce"}, Body: model.BodyExact,
			Headers: []string{"X-Tenant"}, PathTemplates: true, Selection: model.SelectRandom, MissStatus: 418,
		},
		Latency: model.LatencyConfig{
			Mode: model.LatencyFixed, Multiplier: 2.5, Fixed: model.Duration(250 * time.Millisecond),
			Jitter: model.Duration(20 * time.Millisecond),
		},
		ErrorAmplification: 10, ErrorFloor: 0.015,
	}
}

func TestServiceCRUD(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		clock := newClock(s)

		if got, err := s.ListServices(ctx()); err != nil || got == nil || len(got) != 0 {
			t.Fatalf("empty ListServices = %v, %v; want empty non-nil slice", got, err)
		}

		in := fullService()
		created, err := s.PutService(ctx(), in)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(created.ID, "svc_") {
			t.Errorf("assigned ID = %q, want svc_ prefix", created.ID)
		}
		if !created.CreatedAt.Equal(clock.Now()) || !created.UpdatedAt.Equal(clock.Now()) {
			t.Errorf("timestamps on insert = %v / %v, want %v", created.CreatedAt, created.UpdatedAt, clock.Now())
		}

		// Every field survives the round trip.
		want := in
		want.ID, want.CreatedAt, want.UpdatedAt = created.ID, created.CreatedAt, created.UpdatedAt
		got, err := s.GetService(ctx(), created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
		}
		if !reflect.DeepEqual(created, got) {
			t.Errorf("PutService result differs from GetService:\n put %+v\n get %+v", created, got)
		}

		// Update: CreatedAt is kept, UpdatedAt moves, fields change.
		clock.Advance(time.Hour)
		upd := got
		upd.Mode, upd.Enabled, upd.Latency.Mode = model.ModeVirtualize, false, model.LatencyNone
		upd.CreatedAt = time.Time{} // callers need not carry it
		saved, err := s.PutService(ctx(), upd)
		if err != nil {
			t.Fatal(err)
		}
		if !saved.CreatedAt.Equal(created.CreatedAt) {
			t.Errorf("CreatedAt changed on update: %v -> %v", created.CreatedAt, saved.CreatedAt)
		}
		if !saved.UpdatedAt.Equal(clock.Now()) {
			t.Errorf("UpdatedAt = %v, want %v", saved.UpdatedAt, clock.Now())
		}
		again, _ := s.GetService(ctx(), created.ID)
		if again.Mode != model.ModeVirtualize || again.Enabled || again.Latency.Mode != model.LatencyNone {
			t.Errorf("update not persisted: %+v", again)
		}
		if n := countRows(t, s, "services"); n != 1 {
			t.Errorf("update inserted a second row (%d rows)", n)
		}

		// Explicit ID inserts with that ID.
		explicit, err := s.PutService(ctx(), model.Service{ID: "svc_fixed", Name: "b", ListenAddr: ":9102"})
		if err != nil || explicit.ID != "svc_fixed" {
			t.Fatalf("explicit ID put = %+v, %v", explicit, err)
		}

		// Listing is by name.
		if _, err := s.PutService(ctx(), model.Service{Name: "Alpha", ListenAddr: ":9103"}); err != nil {
			t.Fatal(err)
		}
		list, err := s.ListServices(ctx())
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, svc := range list {
			names = append(names, svc.Name)
		}
		if !reflect.DeepEqual(names, []string{"Alpha", "b", "payments"}) {
			t.Errorf("service order = %v", names)
		}

		// Delete.
		if err := s.DeleteService(ctx(), created.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetService(ctx(), created.ID); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("GetService after delete: %v", err)
		}
		if err := s.DeleteService(ctx(), created.ID); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("second DeleteService: %v", err)
		}
	})
}

func TestServiceConflicts(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		a, err := s.PutService(ctx(), model.Service{Name: "a", ListenAddr: ":9001"})
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.PutService(ctx(), model.Service{Name: "b", ListenAddr: ":9002"})
		if err != nil {
			t.Fatal(err)
		}

		cases := []struct {
			name string
			svc  model.Service
			want string
		}{
			{"duplicate name on insert", model.Service{Name: "a", ListenAddr: ":9003"}, "name"},
			{"duplicate listen_addr on insert", model.Service{Name: "c", ListenAddr: ":9001"}, "listen address"},
			{"rename onto another service", model.Service{ID: b.ID, Name: "a", ListenAddr: ":9002"}, "name"},
			{"move onto another listen_addr", model.Service{ID: b.ID, Name: "b", ListenAddr: ":9001"}, "listen address"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				_, err := s.PutService(ctx(), c.svc)
				if !errors.Is(err, model.ErrConflict) {
					t.Fatalf("err = %v, want ErrConflict", err)
				}
				if !strings.Contains(err.Error(), c.want) {
					t.Errorf("error %q does not mention %q", err, c.want)
				}
			})
		}

		// A failed put changes nothing; re-putting an unchanged service is fine.
		if got, _ := s.GetService(ctx(), b.ID); got.Name != "b" || got.ListenAddr != ":9002" {
			t.Errorf("failed put modified the row: %+v", got)
		}
		if _, err := s.PutService(ctx(), a); err != nil {
			t.Errorf("re-putting the same service: %v", err)
		}
		if n := countRows(t, s, "services"); n != 2 {
			t.Errorf("%d services, want 2", n)
		}
	})
}

func TestProfileSeededAndProtected(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		p, err := s.GetProfile(ctx(), model.DefaultProfileID)
		if err != nil {
			t.Fatalf("default profile not seeded: %v", err)
		}
		want := model.DefaultAnonymizationProfile().WithDefaults()
		if p.Name != want.Name || !reflect.DeepEqual(p.Detectors, want.Detectors) ||
			p.DetectorStrategy != want.DetectorStrategy || !reflect.DeepEqual(p.RedactHeaders, want.RedactHeaders) {
			t.Errorf("seeded profile differs from model.DefaultAnonymizationProfile:\n got %+v\nwant %+v", p, want)
		}
		if len(p.Secret) != 64 || !p.HasSecret {
			t.Errorf("seeded profile secret = %q (HasSecret %v), want 64 hex chars", p.Secret, p.HasSecret)
		}
		if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
			t.Errorf("seeded profile has no timestamps: %+v", p)
		}

		err = s.DeleteProfile(ctx(), model.DefaultProfileID)
		if !errors.Is(err, model.ErrConflict) {
			t.Errorf("deleting the default profile: %v, want ErrConflict", err)
		}
		if _, err := s.GetProfile(ctx(), model.DefaultProfileID); err != nil {
			t.Errorf("default profile gone after refused delete: %v", err)
		}
	})
}

func TestProfileSecretHandling(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		clock := newClock(s)

		created, err := s.PutProfile(ctx(), model.AnonymizationProfile{
			Name:      "strict",
			Detectors: []model.Detector{model.DetectEmail, model.DetectJWT},
			Rules: []model.AnonymizationRule{{
				ID: "r1", Name: "user email", Enabled: true, Target: model.TargetJSON, Selector: "$.user.email",
				Scope: model.ScopeResponse, Strategy: model.StrategyFake, Fake: model.FakeEmail,
			}, {
				Name: "card", Enabled: true, Target: model.TargetRegex, Selector: `\d{16}`,
				Strategy: model.StrategyMask, KeepPrefix: 2, KeepSuffix: 4,
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(created.ID, "prof_") {
			t.Errorf("assigned ID = %q", created.ID)
		}
		if len(created.Secret) != 64 {
			t.Fatalf("a profile saved without a secret did not get one: %q", created.Secret)
		}
		got, err := s.GetProfile(ctx(), created.ID)
		if err != nil || !reflect.DeepEqual(got, created) {
			t.Fatalf("round trip: %v\n got %+v\nwant %+v", err, got, created)
		}
		if len(got.Rules) != 2 || got.Rules[1].KeepSuffix != 4 || got.Rules[0].Fake != model.FakeEmail {
			t.Errorf("rules lost: %+v", got.Rules)
		}

		// Updating with an empty secret keeps the stored one.
		clock.Advance(time.Minute)
		upd := got
		upd.Secret, upd.HasSecret, upd.Name = "", false, "stricter"
		saved, err := s.PutProfile(ctx(), upd)
		if err != nil {
			t.Fatal(err)
		}
		if saved.Secret != created.Secret || saved.Name != "stricter" {
			t.Errorf("update with empty secret: %+v (stored secret was %q)", saved, created.Secret)
		}
		if !saved.CreatedAt.Equal(created.CreatedAt) || !saved.UpdatedAt.Equal(clock.Now()) {
			t.Errorf("timestamps after update: created %v updated %v", saved.CreatedAt, saved.UpdatedAt)
		}

		// A non-empty secret rotates it.
		rot := saved
		rot.Secret = strings.Repeat("ab", 32)
		rotated, err := s.PutProfile(ctx(), rot)
		if err != nil || rotated.Secret != rot.Secret {
			t.Fatalf("rotation: %+v, %v", rotated, err)
		}
		if g, _ := s.GetProfile(ctx(), created.ID); g.Secret != rot.Secret {
			t.Errorf("rotated secret not stored: %q", g.Secret)
		}

		// The built-in profile can be edited and keeps its secret.
		def, _ := s.GetProfile(ctx(), model.DefaultProfileID)
		edit := def
		edit.Secret, edit.Description = "", "edited"
		if _, err := s.PutProfile(ctx(), edit); err != nil {
			t.Fatal(err)
		}
		after, _ := s.GetProfile(ctx(), model.DefaultProfileID)
		if after.Secret != def.Secret || after.Description != "edited" {
			t.Errorf("editing the default profile: secret %q -> %q, description %q", def.Secret, after.Secret, after.Description)
		}
	})
}

func TestProfileListAndDelete(t *testing.T) {
	s := openMem(t)
	for _, name := range []string{"zeta", "Alpha", "beta"} {
		if _, err := s.PutProfile(ctx(), model.AnonymizationProfile{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListProfiles(ctx())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range list {
		names = append(names, p.Name)
	}
	if want := []string{"Default", "Alpha", "beta", "zeta"}; !reflect.DeepEqual(names, want) {
		t.Errorf("profile order = %v, want %v", names, want)
	}

	id := list[1].ID
	if err := s.DeleteProfile(ctx(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetProfile(ctx(), id); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("GetProfile after delete: %v", err)
	}
	if err := s.DeleteProfile(ctx(), id); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("second DeleteProfile: %v", err)
	}
}

func TestFaultRuleCRUDAndOrdering(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		clock := newClock(s)
		in := model.FaultRule{
			Name: "slow orders", Enabled: true, Order: 5, ServiceID: "svc_1",
			Methods: []string{"POST", "PUT"}, PathPattern: "/orders/**",
			Headers:     map[string]string{"X-Tenant": "acme"},
			Probability: 0.25, Type: model.FaultLatency,
			Delay: model.Duration(300 * time.Millisecond), Jitter: model.Duration(50 * time.Millisecond),
			Status: 503, Body: `{"e":1}`, ResponseHeader: map[string]string{"Retry-After": "1"},
			BytesPerSecond: 4096,
		}
		created, err := s.PutFaultRule(ctx(), in)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(created.ID, "flt_") {
			t.Errorf("assigned ID = %q", created.ID)
		}
		want := in
		want.ID, want.CreatedAt, want.UpdatedAt = created.ID, created.CreatedAt, created.UpdatedAt
		got, err := s.GetFaultRule(ctx(), created.ID)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("round trip: %v\n got %+v\nwant %+v", err, got, want)
		}

		clock.Advance(time.Second)
		upd := got
		upd.Probability = 1
		saved, err := s.PutFaultRule(ctx(), upd)
		if err != nil || !saved.CreatedAt.Equal(got.CreatedAt) || !saved.UpdatedAt.After(got.UpdatedAt) {
			t.Fatalf("update: %+v, %v", saved, err)
		}

		// Order, then name; ties on both fall back to ID for stability.
		for _, r := range []model.FaultRule{
			{Name: "b", Order: 1}, {Name: "a", Order: 1}, {Name: "z", Order: -3}, {Name: "m", Order: 5},
		} {
			if _, err := s.PutFaultRule(ctx(), r); err != nil {
				t.Fatal(err)
			}
		}
		list, err := s.ListFaultRules(ctx())
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, r := range list {
			names = append(names, r.Name)
		}
		if want := []string{"z", "a", "b", "m", "slow orders"}; !reflect.DeepEqual(names, want) {
			t.Errorf("fault order = %v, want %v", names, want)
		}

		if err := s.DeleteFaultRule(ctx(), created.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetFaultRule(ctx(), created.ID); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("GetFaultRule after delete: %v", err)
		}
		if err := s.DeleteFaultRule(ctx(), created.ID); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("second DeleteFaultRule: %v", err)
		}
	})
}

func sampleSeries(n int) []model.SeriesPoint {
	pts := make([]model.SeriesPoint, n)
	for i := range pts {
		pts[i] = model.SeriesPoint{
			T: baseTime.UnixMilli() + int64(i)*1000, Requests: int64(100 + i%7), Errors: int64(i % 3),
			P50: 12.5 + float64(i%5), P95: 40.25, P99: 90.125, Max: 150.5, InFlight: int64(i % 20),
			BytesIn: 1000 + int64(i), BytesOut: 50000 + int64(i)*3,
		}
	}
	return pts
}

func TestReplayJobCRUD(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		clock := newClock(s)
		started, ended := baseTime.Add(time.Second), baseTime.Add(61*time.Second)
		in := model.ReplayJob{
			Name: "10x checkout",
			Config: model.ReplayConfig{
				RecordingID: "rec_1", Target: "http://staging:8080", Mode: model.ReplayRate,
				Stages:  []model.ReplayStage{{Duration: model.Duration(30 * time.Second), Rate: 500}},
				Amplify: 10, ErrorAmplification: 2.5, Concurrency: 512, Loop: true,
				Include: []string{`^GET /orders`}, SetHeaders: map[string]string{"X-Env": "stg"},
				Duration: model.Duration(time.Minute), Timeout: model.Duration(5 * time.Second),
			},
			Status: model.ReplayCompleted, StartedAt: &started, EndedAt: &ended,
			Summary: &model.ReplaySummary{
				Planned: 1200, Sent: 1200, Completed: 1190, Failed: 10, Timeouts: 3, Errors: 25,
				StatusCounts: map[string]int64{"200": 1100, "500": 15, "429": 10},
				Latency:      model.LatencySummary{Count: 1190, Min: 1.5, Mean: 20.25, P50: 12, P99: 200.5, Max: 350},
				AchievedRPS:  19.83, ErrorRate: 0.0208,
				Elapsed:   model.Duration(60 * time.Second),
				Endpoints: []model.EndpointSummary{{Endpoint: "GET /orders/{id}", Count: 800, Errors: 12}},
			},
			Series: sampleSeries(60),
		}
		created, err := s.PutReplayJob(ctx(), in)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(created.ID, "rpl_") || !created.CreatedAt.Equal(clock.Now()) {
			t.Errorf("created = id %q createdAt %v", created.ID, created.CreatedAt)
		}

		got, err := s.GetReplayJob(ctx(), created.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := in
		want.ID, want.CreatedAt = created.ID, created.CreatedAt
		want.StartedAt, want.EndedAt = ptr(started.UTC()), ptr(ended.UTC())
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
		}

		// List omits Series but keeps everything else.
		list, err := s.ListReplayJobs(ctx())
		if err != nil || len(list) != 1 {
			t.Fatalf("list = %v, %v", list, err)
		}
		if list[0].Series != nil {
			t.Errorf("ListReplayJobs returned %d series points", len(list[0].Series))
		}
		noSeries := want
		noSeries.Series = nil
		if !reflect.DeepEqual(list[0], noSeries) {
			t.Errorf("list entry differs:\n got %+v\nwant %+v", list[0], noSeries)
		}

		// Update replaces the job, including a dropped series, and keeps CreatedAt.
		clock.Advance(time.Hour)
		upd := got
		upd.Status, upd.Series = model.ReplayStopped, nil
		saved, err := s.PutReplayJob(ctx(), upd)
		if err != nil || !saved.CreatedAt.Equal(created.CreatedAt) {
			t.Fatalf("update: %+v, %v", saved, err)
		}
		again, _ := s.GetReplayJob(ctx(), created.ID)
		if again.Status != model.ReplayStopped || len(again.Series) != 0 {
			t.Errorf("update not applied: status %s, %d series points", again.Status, len(again.Series))
		}

		if err := s.DeleteReplayJob(ctx(), created.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetReplayJob(ctx(), created.ID); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("GetReplayJob after delete: %v", err)
		}
		if err := s.DeleteReplayJob(ctx(), created.ID); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("second DeleteReplayJob: %v", err)
		}
	})
}

func ptr[T any](v T) *T { return &v }

func TestReplayJobListNewestFirst(t *testing.T) {
	s := openMem(t)
	clock := newClock(s)
	for _, name := range []string{"first", "second", "third"} {
		if _, err := s.PutReplayJob(ctx(), model.ReplayJob{Name: name, Status: model.ReplayPending}); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Second)
	}
	// Same timestamp: the later insert still sorts first.
	for _, name := range []string{"fourth", "fifth"} {
		if _, err := s.PutReplayJob(ctx(), model.ReplayJob{Name: name, Status: model.ReplayPending}); err != nil {
			t.Fatal(err)
		}
	}

	list, err := s.ListReplayJobs(ctx())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, j := range list {
		names = append(names, j.Name)
	}
	if want := []string{"fifth", "fourth", "third", "second", "first"}; !reflect.DeepEqual(names, want) {
		t.Errorf("order = %v, want %v", names, want)
	}
}

func TestReplayJobLargeSeriesIsCompressed(t *testing.T) {
	s, _ := openFile(t)
	series := sampleSeries(3600) // an hour at one point per second
	j, err := s.PutReplayJob(ctx(), model.ReplayJob{Name: "long", Status: model.ReplayCompleted, Series: series})
	if err != nil {
		t.Fatal(err)
	}
	var stored int
	var gz bool
	if err := s.rdb.QueryRow(`SELECT length(series), series_gz FROM replay_jobs WHERE id = ?`, j.ID).Scan(&stored, &gz); err != nil {
		t.Fatal(err)
	}
	if !gz || stored > 3600*40 {
		t.Errorf("series stored as %d bytes (gz=%v); expected well under 40 B/point when compressed", stored, gz)
	}
	got, err := s.GetReplayJob(ctx(), j.ID)
	if err != nil || !reflect.DeepEqual(got.Series, series) {
		t.Fatalf("series round trip failed: %v (len %d)", err, len(got.Series))
	}
}

func TestSettings(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		if _, err := s.GetSetting(ctx(), "missing"); !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("GetSetting(missing) = %v, want ErrNotFound", err)
		}
		if err := s.PutSetting(ctx(), "k", "v1"); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetSetting(ctx(), "k"); err != nil || v != "v1" {
			t.Fatalf("GetSetting = %q, %v", v, err)
		}
		if err := s.PutSetting(ctx(), "k", "v2"); err != nil {
			t.Fatal(err)
		}
		if v, _ := s.GetSetting(ctx(), "k"); v != "v2" {
			t.Errorf("overwrite not applied: %q", v)
		}
		// Empty values and odd keys are legal.
		if err := s.PutSetting(ctx(), "", ""); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetSetting(ctx(), ""); err != nil || v != "" {
			t.Errorf("empty key: %q, %v", v, err)
		}
		if err := s.PutSetting(ctx(), "multi\nline", "päyload ✓"); err != nil {
			t.Fatal(err)
		}
		if v, _ := s.GetSetting(ctx(), "multi\nline"); v != "päyload ✓" {
			t.Errorf("unicode value = %q", v)
		}
	})
}

// countRows counts the rows of a table (table is a test constant).
func countRows(t testing.TB, s *SQLite, table string) int {
	t.Helper()
	var n int
	if err := s.rdb.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestMissingObjectsWrapErrNotFound checks every lookup, update and delete of
// an unknown ID against both backends.
func TestMissingObjectsWrapErrNotFound(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		calls := map[string]func() error{
			"GetService":       func() error { _, err := s.GetService(ctx(), "svc_nope"); return err },
			"DeleteService":    func() error { return s.DeleteService(ctx(), "svc_nope") },
			"GetRecording":     func() error { _, err := s.GetRecording(ctx(), "rec_nope"); return err },
			"UpdateRecording":  func() error { _, err := s.UpdateRecording(ctx(), model.Recording{ID: "rec_nope"}); return err },
			"DeleteRecording":  func() error { return s.DeleteRecording(ctx(), "rec_nope") },
			"AppendExchanges":  func() error { return s.AppendExchanges(ctx(), "rec_nope", []*model.Exchange{mkEx(0)}) },
			"ListExchanges":    func() error { _, _, err := s.ListExchanges(ctx(), "rec_nope", model.ExchangeQuery{}); return err },
			"GetExchange":      func() error { _, err := s.GetExchange(ctx(), 4242); return err },
			"IterateExchanges": func() error { return s.IterateExchanges(ctx(), "rec_nope", model.IterOptions{}, nil) },
			"EndpointStats":    func() error { _, err := s.EndpointStats(ctx(), "rec_nope"); return err },
			"GetProfile":       func() error { _, err := s.GetProfile(ctx(), "prof_nope"); return err },
			"DeleteProfile":    func() error { return s.DeleteProfile(ctx(), "prof_nope") },
			"GetFaultRule":     func() error { _, err := s.GetFaultRule(ctx(), "flt_nope"); return err },
			"DeleteFaultRule":  func() error { return s.DeleteFaultRule(ctx(), "flt_nope") },
			"GetReplayJob":     func() error { _, err := s.GetReplayJob(ctx(), "rpl_nope"); return err },
			"DeleteReplayJob":  func() error { return s.DeleteReplayJob(ctx(), "rpl_nope") },
			"GetSetting":       func() error { _, err := s.GetSetting(ctx(), "nope"); return err },
		}
		for name, call := range calls {
			err := call()
			if !errors.Is(err, model.ErrNotFound) {
				t.Errorf("%s: err = %v, want one wrapping ErrNotFound", name, err)
			}
			if errors.Is(err, model.ErrConflict) {
				t.Errorf("%s: err also matches ErrConflict", name)
			}
		}
	})
}

func TestSchemaVersion(t *testing.T) {
	s := openMem(t)
	v, err := s.SchemaVersion(ctx())
	if err != nil || v != latestVersion(migrations) || v < 1 {
		t.Errorf("SchemaVersion = %d, %v", v, err)
	}
}
