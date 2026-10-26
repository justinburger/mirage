package metrics

import (
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// zoneRE matches an RFC 3339 timestamp that ends in "Z".
var zoneRE = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z$`)

// withLocalZone runs fn with time.Local set to a zone that is not UTC, the
// situation of a server on a developer laptop.
func withLocalZone(t *testing.T, fn func()) {
	t.Helper()
	old := time.Local
	time.Local = time.FixedZone("PDT", -7*3600)
	defer func() { time.Local = old }()
	fn()
}

// TestTimestampsAreUTC: every timestamp the metrics package hands out is in
// UTC, so the JSON the API writes uses "Z" like created_at and updated_at do,
// whatever the server's local zone is.
func TestTimestampsAreUTC(t *testing.T) {
	withLocalZone(t, func() {
		c := NewCollector("service:svc_1", model.KindService, "svc_1")
		s := c.Snapshot()
		if s.Since.Location() != time.UTC {
			t.Errorf("Snapshot.Since is in %v, want UTC", s.Since.Location())
		}
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if since, _ := m["since"].(string); !zoneRE.MatchString(since) {
			t.Errorf("since = %q, want a UTC (Z) timestamp", since)
		}

		r := NewRegistry()
		r.Collector("service:svc_1", model.KindService, "svc_1")
		// A tick handed a local time, as Run does with time.Now().Truncate.
		now := time.Now().In(time.Local).Truncate(time.Second)
		u := r.Tick(now)
		if u.Time.Location() != time.UTC || !u.Time.Equal(now) {
			t.Errorf("Update.Time = %v (%v), want %v in UTC", u.Time, u.Time.Location(), now)
		}
		b, err = json.Marshal(u)
		if err != nil {
			t.Fatal(err)
		}
		var um struct {
			Time      string `json:"time"`
			Snapshots []struct {
				Since string `json:"since"`
			} `json:"snapshots"`
		}
		if err := json.Unmarshal(b, &um); err != nil {
			t.Fatal(err)
		}
		if !zoneRE.MatchString(um.Time) {
			t.Errorf("update time = %q, want a UTC (Z) timestamp", um.Time)
		}
		if len(um.Snapshots) != 1 || !zoneRE.MatchString(um.Snapshots[0].Since) {
			t.Errorf("snapshot since = %+v, want a UTC (Z) timestamp", um.Snapshots)
		}
	})
}
