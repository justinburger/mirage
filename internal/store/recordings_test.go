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

// TestUpdateRecordingTouchesOnlyEditableFields: only Name, Description and
// Labels are editable. Source and Anonymized belong to CreateRecording and
// AppendExchanges, the counters and timestamps to AppendExchanges.
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
			ID: rec.ID, Name: "renamed", Description: "new desc",
			Labels: map[string]string{"x": "y"},
			// None of the following may take effect.
			Source: "har", Anonymized: true,
			ServiceID: "svc_other", ExchangeCount: 1000, ErrorCount: 1000, Bytes: 1000,
			CreatedAt: baseTime.AddDate(-5, 0, 0), FirstAt: ptr(baseTime.AddDate(-5, 0, 0)),
		})
		if err != nil {
			t.Fatal(err)
		}
		want := before
		want.Name, want.Description = "renamed", "new desc"
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

		// The flags cannot be cleared through UpdateRecording either.
		anon, _ := s.CreateRecording(ctx(), model.Recording{Name: "anon", Source: "har", Anonymized: true})
		out, err = s.UpdateRecording(ctx(), model.Recording{ID: anon.ID, Name: "anon2", Source: "proxy", Anonymized: false})
		if err != nil || !out.Anonymized || out.Source != "har" || out.Name != "anon2" {
			t.Errorf("update of Source/Anonymized took effect: %+v, %v", out, err)
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

// TestAppendMaintainsAnonymizedFlag: Recording.Anonymized is owned by
// CreateRecording (initial value) and AppendExchanges. Appending to an empty
// recording sets it to whether every appended exchange is anonymized; on a
// non-empty one it can only be cleared, never set.
func TestAppendMaintainsAnonymizedFlag(t *testing.T) {
	type step struct {
		anon []bool // Anonymized of each appended exchange; nil = append an empty batch
		want bool   // the recording's flag afterwards
	}
	cases := []struct {
		name    string
		created bool
		steps   []step
	}{
		{"empty, created plain, all anonymized -> true", false, []step{{[]bool{true, true}, true}}},
		{"empty, created anonymized, all anonymized -> true", true, []step{{[]bool{true}, true}}},
		{"empty, created anonymized, one plain -> false", true, []step{{[]bool{false}, false}}},
		{"empty, created anonymized, mixed batch -> false", true, []step{{[]bool{true, false, true}, false}}},
		{"empty, created plain, mixed batch -> false", false, []step{{[]bool{true, false}, false}}},
		{"stays true over anonymized batches", true, []step{{[]bool{true}, true}, {[]bool{true, true}, true}, {[]bool{true}, true}}},
		{"one plain exchange clears it for good", true, []step{{[]bool{true}, true}, {[]bool{true, false}, false}, {[]bool{true, true}, false}}},
		{"plain first, anonymized later: not restored", false, []step{{[]bool{false}, false}, {[]bool{true}, false}, {[]bool{true, true}, false}}},
		{"anonymized fills a plain-created recording, plain clears it", false, []step{{[]bool{true}, true}, {[]bool{true}, true}, {[]bool{false}, false}}},
		{"empty batch changes nothing (anonymized)", true, []step{{nil, true}, {[]bool{true}, true}, {nil, true}}},
		{"empty batch changes nothing (plain)", false, []step{{nil, false}, {[]bool{true}, true}, {nil, true}}},
	}
	backends(t, func(t *testing.T, s *SQLite) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				rec, err := s.CreateRecording(ctx(), model.Recording{Name: c.name, Source: "proxy", Anonymized: c.created})
				if err != nil {
					t.Fatal(err)
				}
				if rec.Anonymized != c.created {
					t.Fatalf("created recording has Anonymized %v, want %v", rec.Anonymized, c.created)
				}
				next := 0
				for i, st := range c.steps {
					exs := make([]*model.Exchange, len(st.anon))
					for j, a := range st.anon {
						exs[j] = mkEx(next, withAnonymized(a))
						next++
					}
					if err := s.AppendExchanges(ctx(), rec.ID, exs); err != nil {
						t.Fatal(err)
					}
					got, err := s.GetRecording(ctx(), rec.ID)
					if err != nil {
						t.Fatal(err)
					}
					if got.Anonymized != st.want {
						t.Fatalf("step %d (append %v): Anonymized = %v, want %v", i, st.anon, got.Anonymized, st.want)
					}
				}
				// Listing reads the same stored flag, and each exchange keeps its own.
				list, _ := s.ListRecordings(ctx())
				for _, r := range list {
					if r.ID == rec.ID && r.Anonymized != c.steps[len(c.steps)-1].want {
						t.Errorf("ListRecordings shows Anonymized = %v", r.Anonymized)
					}
				}
				var plain int
				if err := s.rdb.QueryRow(`SELECT COUNT(*) FROM exchanges WHERE recording_id = ? AND anonymized = 0`, rec.ID).Scan(&plain); err != nil {
					t.Fatal(err)
				}
				got, _ := s.GetRecording(ctx(), rec.ID)
				if got.ExchangeCount > 0 && got.Anonymized && plain != 0 {
					t.Errorf("recording is flagged anonymized but holds %d plain exchanges", plain)
				}
			})
		}
	})
}
