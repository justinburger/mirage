package ops

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/har"
	"github.com/justinburger/mirage/internal/model"
)

// timedEntry is a GET of /e<i> that started at t.
func timedEntry(i int, t time.Time) har.Entry {
	e := entry(fmt.Sprintf("http://localhost:9001/e%d", i), 200)
	e.StartedDateTime = har.Timestamp{Time: t}
	return e
}

// checkStartTimes fails unless every exchange of the recording has a storable
// start time, in the order of the recording, and the recording spans less than
// maxSpan: a timeline replay waits for the span, so a time that wrapped (or
// stayed at year 1 or 9999) would stall it for years.
func checkStartTimes(t *testing.T, exs []*model.Exchange, maxSpan time.Duration) {
	t.Helper()
	for i, ex := range exs {
		if !har.StartTimeStorable(ex.StartedAt) {
			t.Errorf("exchange %d: StartedAt = %v", i, ex.StartedAt)
		}
		if i > 0 && ex.StartedAt.Before(exs[i-1].StartedAt) {
			t.Errorf("exchange %d starts at %v, before exchange %d at %v", i, ex.StartedAt, i-1, exs[i-1].StartedAt)
		}
	}
	if span := exs[len(exs)-1].StartedAt.Sub(exs[0].StartedAt); span > maxSpan || span < 0 {
		t.Errorf("the recording spans %v", span)
	}
}

func TestImportHARRepairsStartTimes(t *testing.T) {
	ctx := context.Background()
	zero := time.Time{}
	y9999 := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	y1500 := time.Date(1500, 6, 1, 0, 0, 0, 0, time.UTC)
	t1, t3 := baseTime.Add(time.Second), baseTime.Add(3*time.Second)

	for _, anonymized := range []bool{false, true} {
		t.Run(fmt.Sprintf("anonymized=%v", anonymized), func(t *testing.T) {
			st := newStore(t)
			var eng = defaultEngine(t, st)
			if !anonymized {
				eng = nil
			}
			doc := harDoc(t,
				timedEntry(0, zero),  // leading: takes the first usable time
				timedEntry(1, y9999), // leading
				timedEntry(2, t1),
				timedEntry(3, y1500), // takes the time of the entry before
				timedEntry(4, t3),
				timedEntry(5, zero),
			)
			rec, stats, err := ImportHARStats(ctx, st, bytes.NewReader(doc), "times", eng, nil)
			if err != nil {
				t.Fatalf("ImportHAR: %v", err)
			}
			if stats.BadTimes != 4 || stats.Written != 6 {
				t.Errorf("stats = %+v", stats)
			}
			if !strings.Contains(rec.Description, "4 entries had a missing start time or one outside the years 1678 to 2261") {
				t.Errorf("Description = %q", rec.Description)
			}
			exs := loadAll(t, st, rec.ID)
			want := []time.Time{t1, t1, t1, t1, t3, t3}
			if len(exs) != len(want) {
				t.Fatalf("%d exchanges", len(exs))
			}
			for i, ex := range exs {
				if !ex.StartedAt.Equal(want[i]) {
					t.Errorf("exchange %d: StartedAt = %v, want %v", i, ex.StartedAt, want[i])
				}
			}
			checkStartTimes(t, exs, time.Minute)
			if rec.FirstAt == nil || !rec.FirstAt.Equal(t1) || rec.LastAt == nil || !rec.LastAt.Equal(t3) {
				t.Errorf("recording spans %v to %v", rec.FirstAt, rec.LastAt)
			}
		})
	}

	t.Run("no usable time at all takes the import time", func(t *testing.T) {
		st := newStore(t)
		before := time.Now().Add(-time.Second)
		doc := harDoc(t, timedEntry(0, zero), timedEntry(1, y9999), timedEntry(2, y1500))
		rec, stats, err := ImportHARStats(ctx, st, bytes.NewReader(doc), "all bad", nil, nil)
		if err != nil || stats.BadTimes != 3 {
			t.Fatalf("ImportHAR: %v, %+v", err, stats)
		}
		exs := loadAll(t, st, rec.ID)
		for i, ex := range exs {
			if ex.StartedAt.Before(before) || ex.StartedAt.After(time.Now().Add(time.Second)) || !ex.StartedAt.Equal(exs[0].StartedAt) {
				t.Errorf("exchange %d: StartedAt = %v", i, ex.StartedAt)
			}
		}
		checkStartTimes(t, exs, time.Second)
	})

	t.Run("a long run of leading bad times does not pile up in memory", func(t *testing.T) {
		st := newStore(t)
		var entries []har.Entry
		for i := 0; i < maxLeadEntries+10; i++ {
			entries = append(entries, timedEntry(i, y9999))
		}
		entries = append(entries, timedEntry(maxLeadEntries+10, baseTime))
		rec, stats, err := ImportHARStats(ctx, st, bytes.NewReader(harDoc(t, entries...)), "long", nil, nil)
		if err != nil || stats.BadTimes != int64(maxLeadEntries+10) {
			t.Fatalf("ImportHAR: %v, %+v", err, stats)
		}
		if rec.ExchangeCount != int64(len(entries)) {
			t.Errorf("ExchangeCount = %d, want %d", rec.ExchangeCount, len(entries))
		}
		for i, ex := range loadAll(t, st, rec.ID) {
			if !har.StartTimeStorable(ex.StartedAt) {
				t.Fatalf("exchange %d: StartedAt = %v", i, ex.StartedAt)
			}
		}
	})

	t.Run("good times are untouched and not counted", func(t *testing.T) {
		st := newStore(t)
		rec, stats, err := ImportHARStats(ctx, st, bytes.NewReader(harDoc(t, timedEntry(0, t1), timedEntry(1, t3))), "good", nil, nil)
		if err != nil || stats.BadTimes != 0 {
			t.Fatalf("ImportHAR: %v, %+v", err, stats)
		}
		if strings.Contains(rec.Description, "start time") {
			t.Errorf("Description = %q", rec.Description)
		}
	})
}
