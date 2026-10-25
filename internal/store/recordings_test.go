package store

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

func TestRecordingCRUD(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		clock := newClock(s)

		in := model.Recording{
			Name: "checkout", Description: "prod capture", ServiceID: "svc_1", Source: "proxy",
			Anonymized: true, Labels: map[string]string{"env": "prod", "team": "pay"},
			// Counters supplied by the caller are ignored: AppendExchanges owns them.
			ExchangeCount: 99, ErrorCount: 9, Bytes: 999, FirstAt: ptr(baseTime), LastAt: ptr(baseTime),
		}
		created, err := s.CreateRecording(ctx(), in)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(created.ID, "rec_") {
			t.Errorf("assigned ID = %q", created.ID)
		}
		if created.ExchangeCount != 0 || created.ErrorCount != 0 || created.Bytes != 0 || created.FirstAt != nil || created.LastAt != nil {
			t.Errorf("counters not reset on create: %+v", created)
		}
		if !created.CreatedAt.Equal(clock.Now()) || !created.UpdatedAt.Equal(clock.Now()) {
			t.Errorf("timestamps = %v / %v", created.CreatedAt, created.UpdatedAt)
		}
		got, err := s.GetRecording(ctx(), created.ID)
		if err != nil || !reflect.DeepEqual(got, created) {
			t.Fatalf("round trip: %v\n got %+v\nwant %+v", err, got, created)
		}

		// The caller's label map is not aliased.
		in.Labels["env"] = "mutated"
		if g, _ := s.GetRecording(ctx(), created.ID); g.Labels["env"] != "prod" {
			t.Errorf("labels changed through the caller's map: %v", g.Labels)
		}

		// Duplicate ID conflicts.
		if _, err := s.CreateRecording(ctx(), model.Recording{ID: created.ID, Name: "dup"}); !errors.Is(err, model.ErrConflict) {
			t.Errorf("duplicate ID: %v, want ErrConflict", err)
		}
		// Explicit IDs are honoured.
		if r, err := s.CreateRecording(ctx(), model.Recording{ID: "rec_fixed", Name: "fixed"}); err != nil || r.ID != "rec_fixed" {
			t.Errorf("explicit ID: %+v, %v", r, err)
		}

		// Unknown recording.
		if _, err := s.GetRecording(ctx(), "rec_nope"); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("GetRecording(unknown) = %v", err)
		}
		if err := s.DeleteRecording(ctx(), "rec_nope"); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("DeleteRecording(unknown) = %v", err)
		}
		if _, err := s.UpdateRecording(ctx(), model.Recording{ID: "rec_nope", Name: "x"}); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("UpdateRecording(unknown) = %v", err)
		}
	})
}

func TestUpdateRecordingTouchesOnlyEditableFields(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		clock := newClock(s)
		rec, _ := s.CreateRecording(ctx(), model.Recording{
			Name: "orig", Description: "d", ServiceID: "svc_keep", Source: "proxy", Labels: map[string]string{"a": "b"},
		})
		appendN(t, s, rec.ID, 4, withStatus(500))
		before, _ := s.GetRecording(ctx(), rec.ID)

		clock.Advance(time.Minute)
		out, err := s.UpdateRecording(ctx(), model.Recording{
			ID: rec.ID, Name: "renamed", Description: "new desc", Source: "har", Anonymized: true,
			Labels: map[string]string{"x": "y"},
			// None of the following may take effect.
			ServiceID: "svc_other", ExchangeCount: 1000, ErrorCount: 1000, Bytes: 1000,
			CreatedAt: baseTime.AddDate(-5, 0, 0), FirstAt: ptr(baseTime.AddDate(-5, 0, 0)),
		})
		if err != nil {
			t.Fatal(err)
		}
		want := before
		want.Name, want.Description, want.Source, want.Anonymized = "renamed", "new desc", "har", true
		want.Labels = map[string]string{"x": "y"}
		want.UpdatedAt = clock.Now()
		if !reflect.DeepEqual(out, want) {
			t.Errorf("UpdateRecording result:\n got %+v\nwant %+v", out, want)
		}
		if got, _ := s.GetRecording(ctx(), rec.ID); !reflect.DeepEqual(got, want) {
			t.Errorf("stored recording:\n got %+v\nwant %+v", got, want)
		}

		// Clearing labels works.
		out, err = s.UpdateRecording(ctx(), model.Recording{ID: rec.ID, Name: "renamed"})
		if err != nil || out.Labels != nil {
			t.Errorf("clearing labels: %+v, %v", out.Labels, err)
		}
	})
}

func TestListRecordingsNewestFirst(t *testing.T) {
	s := openMem(t)
	clock := newClock(s)
	if got, err := s.ListRecordings(ctx()); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty list = %v, %v; want empty non-nil", got, err)
	}
	for _, name := range []string{"a", "b", "c"} {
		newRecording(t, s, name)
		clock.Advance(time.Second)
	}
	newRecording(t, s, "d") // same instant as nothing else, but after c
	newRecording(t, s, "e") // same instant as d: insertion order breaks the tie
	list, err := s.ListRecordings(ctx())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range list {
		names = append(names, r.Name)
	}
	if want := []string{"e", "d", "c", "b", "a"}; !reflect.DeepEqual(names, want) {
		t.Errorf("order = %v, want %v", names, want)
	}
}

func TestDeleteRecordingCascades(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		keep := newRecording(t, s, "keep")
		drop := newRecording(t, s, "drop")
		keepEx := appendN(t, s, keep.ID, 5)
		dropEx := appendN(t, s, drop.ID, 8)

		if err := s.DeleteRecording(ctx(), drop.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetRecording(ctx(), drop.ID); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("recording still there: %v", err)
		}
		for _, ex := range dropEx {
			if _, err := s.GetExchange(ctx(), ex.ID); !errors.Is(err, model.ErrNotFound) {
				t.Fatalf("exchange %d survived its recording: %v", ex.ID, err)
			}
		}
		var n int
		if err := s.rdb.QueryRow(`SELECT COUNT(*) FROM exchanges WHERE recording_id = ?`, drop.ID).Scan(&n); err != nil || n != 0 {
			t.Errorf("%d exchange rows left for the deleted recording (%v)", n, err)
		}
		// The other recording is untouched.
		if _, total, err := s.ListExchanges(ctx(), keep.ID, model.ExchangeQuery{}); err != nil || total != 5 {
			t.Errorf("surviving recording: total %d, %v", total, err)
		}
		for _, ex := range keepEx {
			if _, err := s.GetExchange(ctx(), ex.ID); err != nil {
				t.Errorf("exchange %d lost: %v", ex.ID, err)
			}
		}
		// Operations on the deleted recording fail cleanly.
		if err := s.AppendExchanges(ctx(), drop.ID, []*model.Exchange{mkEx(0)}); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("append to deleted recording: %v", err)
		}
	})
}

// TestDeleteRecordingFreesDiskSpace checks that a large recording's pages are
// returned to the filesystem, not just marked free.
func TestDeleteRecordingFreesDiskSpace(t *testing.T) {
	s, path := openFile(t)
	rec := newRecording(t, s, "big")
	exs := make([]*model.Exchange, 400)
	for i := range exs {
		exs[i] = mkEx(i, withRespBody(randomBytes(int64(i), 32<<10))) // incompressible, 32 KiB each
	}
	for i := 0; i < len(exs); i += 100 {
		if err := s.AppendExchanges(ctx(), rec.ID, exs[i:i+100]); err != nil {
			t.Fatal(err)
		}
	}
	checkpoint := func() int64 {
		t.Helper()
		if _, err := s.wdb.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	before := checkpoint()
	if before < 12<<20 {
		t.Fatalf("test data too small: database is %d bytes", before)
	}
	if err := s.DeleteRecording(ctx(), rec.ID); err != nil {
		t.Fatal(err)
	}
	after := checkpoint()
	if after > before/4 {
		t.Errorf("database is %d bytes after deleting the only recording (was %d); free pages were not released", after, before)
	}
	var free int
	if err := s.wdb.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil || free != 0 {
		t.Errorf("freelist_count = %d, %v", free, err)
	}
}
