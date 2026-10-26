package ops

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

func TestCopyAnonymized(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	// 1,234 exchanges: two full batches plus a partial one.
	src := seed(t, st, "payments", 1234)
	eng, err := LoadEngine(ctx, st, "")
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var progress []int64
	dst, err := CopyAnonymized(ctx, st, src.ID, eng, "payments (safe)", func(done int64) {
		mu.Lock()
		progress = append(progress, done)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("CopyAnonymized: %v", err)
	}

	t.Run("recording", func(t *testing.T) {
		if dst.ID == src.ID || dst.ID == "" {
			t.Errorf("destination ID = %q (source %q)", dst.ID, src.ID)
		}
		if dst.Name != "payments (safe)" {
			t.Errorf("Name = %q", dst.Name)
		}
		if dst.Source != SourceCopy || !dst.Anonymized {
			t.Errorf("Source=%q Anonymized=%v, want copy/true", dst.Source, dst.Anonymized)
		}
		for _, want := range []string{"payments", src.ID, "Default"} {
			if !strings.Contains(dst.Description, want) {
				t.Errorf("Description %q does not mention %q", dst.Description, want)
			}
		}
		if dst.Labels["env"] != "test" {
			t.Errorf("Labels = %v, want the source labels", dst.Labels)
		}
		if dst.ExchangeCount != src.ExchangeCount || dst.ExchangeCount != 1234 {
			t.Errorf("ExchangeCount = %d, source %d", dst.ExchangeCount, src.ExchangeCount)
		}
		if dst.ErrorCount != src.ErrorCount || dst.ErrorCount == 0 {
			t.Errorf("ErrorCount = %d, source %d", dst.ErrorCount, src.ErrorCount)
		}
		if dst.FirstAt == nil || src.FirstAt == nil || !dst.FirstAt.Equal(*src.FirstAt) {
			t.Errorf("FirstAt = %v, source %v", dst.FirstAt, src.FirstAt)
		}
	})

	t.Run("every exchange anonymized", func(t *testing.T) {
		srcEx, dstEx := loadAll(t, st, src.ID), loadAll(t, st, dst.ID)
		if len(srcEx) != len(dstEx) {
			t.Fatalf("exchange counts differ: %d vs %d", len(srcEx), len(dstEx))
		}
		for i, ex := range dstEx {
			if !ex.Anonymized {
				t.Fatalf("exchange %d not flagged anonymized", i)
			}
			d := dump(ex)
			for _, secret := range rawSecrets {
				if strings.Contains(d, secret) {
					t.Fatalf("exchange %d still contains %q:\n%s", i, secret, d)
				}
			}
			o := srcEx[i]
			if ex.Endpoint != o.Endpoint || ex.Request.Method != o.Request.Method || ex.Response.Status != o.Response.Status {
				t.Fatalf("exchange %d changed shape: %s %d vs %s %d", i, ex.Endpoint, ex.Response.Status, o.Endpoint, o.Response.Status)
			}
			if !ex.StartedAt.Equal(o.StartedAt) || ex.Duration != o.Duration {
				t.Fatalf("exchange %d lost its timing", i)
			}
			if ex.RecordingID != dst.ID || ex.ID == o.ID {
				t.Fatalf("exchange %d: RecordingID=%q ID=%d (source ID %d)", i, ex.RecordingID, ex.ID, o.ID)
			}
			if ex.Seq != int64(i+1) {
				t.Fatalf("exchange %d: Seq = %d", i, ex.Seq)
			}
		}
	})

	t.Run("source untouched", func(t *testing.T) {
		got, err := st.GetRecording(ctx, src.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ExchangeCount != 1234 || got.Anonymized {
			t.Errorf("source changed: count=%d anonymized=%v", got.ExchangeCount, got.Anonymized)
		}
		first := loadAll(t, st, src.ID)[0]
		if !strings.Contains(dump(first), "Pa$$w0rd!x") {
			t.Error("source exchange was anonymized in place")
		}
	})

	t.Run("progress", func(t *testing.T) {
		mu.Lock()
		defer mu.Unlock()
		want := []int64{500, 1000, 1234}
		if len(progress) != len(want) {
			t.Fatalf("progress = %v, want %v", progress, want)
		}
		for i := range want {
			if progress[i] != want[i] {
				t.Fatalf("progress = %v, want %v", progress, want)
			}
		}
	})

	t.Run("deterministic tokens", func(t *testing.T) {
		again, err := CopyAnonymized(ctx, st, src.ID, eng, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if again.Name != "payments (anon)" {
			t.Errorf("default name = %q", again.Name)
		}
		a, b := loadAll(t, st, dst.ID), loadAll(t, st, again.ID)
		for i := range a {
			if dump(a[i]) != dump(b[i]) {
				t.Fatalf("exchange %d differs between two copies:\n%s\n---\n%s", i, dump(a[i]), dump(b[i]))
			}
		}
	})
}

func TestCopyAnonymizedEdgeCases(t *testing.T) {
	ctx := context.Background()

	t.Run("empty source", func(t *testing.T) {
		st := newStore(t)
		src := seed(t, st, "empty", 0)
		eng, _ := LoadEngine(ctx, st, "")
		dst, err := CopyAnonymized(ctx, st, src.ID, eng, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if dst.ExchangeCount != 0 || !dst.Anonymized {
			t.Errorf("empty copy: count=%d anonymized=%v", dst.ExchangeCount, dst.Anonymized)
		}
	})

	t.Run("unknown source creates nothing", func(t *testing.T) {
		st := newStore(t)
		eng, _ := LoadEngine(ctx, st, "")
		_, err := CopyAnonymized(ctx, st, "rec_missing", eng, "x", nil)
		if !errors.Is(err, model.ErrNotFound) {
			t.Fatalf("err = %v, want not found", err)
		}
		if n := recordingCount(t, st); n != 0 {
			t.Errorf("%d recordings after a failed copy", n)
		}
	})

	t.Run("nil engine", func(t *testing.T) {
		st := newStore(t)
		src := seed(t, st, "src", 3)
		_, err := CopyAnonymized(ctx, st, src.ID, nil, "x", nil)
		if !errors.Is(err, model.ErrValidation) {
			t.Fatalf("err = %v, want validation error", err)
		}
		if n := recordingCount(t, st); n != 1 {
			t.Errorf("%d recordings, want only the source", n)
		}
	})

	t.Run("exchange without endpoint gets one", func(t *testing.T) {
		st := newStore(t)
		rec, _ := st.CreateRecording(ctx, model.Recording{Name: "legacy", Source: "proxy"})
		ex := mkExchange(1)
		ex.Endpoint = ""
		if err := st.AppendExchanges(ctx, rec.ID, []*model.Exchange{ex}); err != nil {
			t.Fatal(err)
		}
		eng, _ := LoadEngine(ctx, st, "")
		dst, err := CopyAnonymized(ctx, st, rec.ID, eng, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := loadAll(t, st, dst.ID)[0].Endpoint; got != "POST /users/{id}/orders" {
			t.Errorf("Endpoint = %q", got)
		}
	})
}

func TestCopyAnonymizedCleansUp(t *testing.T) {
	ctx := context.Background()

	t.Run("store error deletes the partial destination", func(t *testing.T) {
		real := newStore(t)
		src := seed(t, real, "src", 1200)
		eng, _ := LoadEngine(ctx, real, "")
		st := &flakyStore{Store: real, failAppendOn: 2}
		_, err := CopyAnonymized(ctx, st, src.ID, eng, "doomed", nil)
		if !errors.Is(err, errInjected) {
			t.Fatalf("err = %v, want the injected error", err)
		}
		recs, _ := real.ListRecordings(ctx)
		if len(recs) != 1 || recs[0].ID != src.ID {
			t.Fatalf("recordings after failure = %+v, want only the source", recs)
		}
		if got, _ := real.GetRecording(ctx, src.ID); got.ExchangeCount != 1200 {
			t.Errorf("source damaged: %d exchanges", got.ExchangeCount)
		}
	})

	t.Run("cancellation deletes the partial destination", func(t *testing.T) {
		st := newStore(t)
		src := seed(t, st, "src", 1500)
		eng, _ := LoadEngine(ctx, st, "")
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		_, err := CopyAnonymized(cctx, st, src.ID, eng, "doomed", func(done int64) {
			if done >= 500 {
				cancel()
			}
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		recs, _ := st.ListRecordings(ctx)
		if len(recs) != 1 || recs[0].ID != src.ID {
			t.Fatalf("recordings after cancel = %+v, want only the source", recs)
		}
	})

	t.Run("already cancelled context", func(t *testing.T) {
		st := newStore(t)
		src := seed(t, st, "src", 10)
		eng, _ := LoadEngine(ctx, st, "")
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := CopyAnonymized(cctx, st, src.ID, eng, "", nil); err == nil {
			t.Fatal("want an error")
		}
		if n := recordingCount(t, st); n != 1 {
			t.Errorf("%d recordings, want only the source", n)
		}
	})
}
