package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// fillRecording appends n small exchanges to rec in a few big batches.
func fillRecording(t testing.TB, s *SQLite, rec string, n int) {
	t.Helper()
	const batch = 5000
	for done := 0; done < n; done += batch {
		exs := make([]*model.Exchange, min(batch, n-done))
		for i := range exs {
			exs[i] = mkEx(done + i)
		}
		if err := s.AppendExchanges(ctx(), rec, exs); err != nil {
			t.Fatal(err)
		}
	}
}

// exchangeRows counts the exchange rows of a recording, deleted or not.
func exchangeRows(t testing.TB, s *SQLite, rec string) int {
	t.Helper()
	var n int
	if err := s.rdb.QueryRow(`SELECT COUNT(*) FROM exchanges WHERE recording_id = ?`, rec).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// recordingRows counts the rows of the recordings table with this ID, including tombstoned ones.
func recordingRows(t testing.TB, s *SQLite, rec string) int {
	t.Helper()
	var n int
	if err := s.rdb.QueryRow(`SELECT COUNT(*) FROM recordings WHERE id = ?`, rec).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// expectGone fails unless every operation on rec reports it as not found
// (and, for CreateRecording, as taken), which is what a client must see from
// the moment DeleteRecording starts.
func expectGone(t testing.TB, s *SQLite, rec string) {
	t.Helper()
	calls := map[string]func() error{
		"GetRecording":    func() error { _, err := s.GetRecording(ctx(), rec); return err },
		"UpdateRecording": func() error { _, err := s.UpdateRecording(ctx(), model.Recording{ID: rec, Name: "x"}); return err },
		"DeleteRecording": func() error { return s.DeleteRecording(ctx(), rec) },
		"AppendExchanges": func() error { return s.AppendExchanges(ctx(), rec, []*model.Exchange{mkEx(0)}) },
		"ListExchanges":   func() error { _, _, err := s.ListExchanges(ctx(), rec, model.ExchangeQuery{}); return err },
		"IterateExchanges": func() error {
			return s.IterateExchanges(ctx(), rec, model.IterOptions{}, func(*model.Exchange) error { return nil })
		},
		"EndpointStats": func() error { _, err := s.EndpointStats(ctx(), rec); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("%s on a recording being deleted: %v, want ErrNotFound", name, err)
		}
	}
	list, err := s.ListRecordings(ctx())
	if err != nil {
		t.Errorf("ListRecordings: %v", err)
	}
	for _, r := range list {
		if r.ID == rec {
			t.Errorf("ListRecordings still shows the recording being deleted")
		}
	}
	// Its ID stays reserved until the last exchange is gone.
	if _, err := s.CreateRecording(ctx(), model.Recording{ID: rec, Name: "again"}); !errors.Is(err, model.ErrConflict) {
		t.Errorf("CreateRecording with the ID being purged: %v, want ErrConflict", err)
	}
}

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}

// expectedChunks returns the sizes of the DELETE chunks for a recording of
// rows rows: full chunks, then the remainder (or an empty final chunk, which
// is how the loop learns that nothing is left).
func expectedChunks(rows, chunk int) []int {
	var out []int
	for ; rows >= chunk; rows -= chunk {
		out = append(out, chunk)
	}
	return append(out, rows)
}

// testLargeDelete deletes a big recording in chunks of s.deleteChunk while a
// goroutine keeps capturing batches into another recording, and checks that
//
//   - the big recording is gone for every client from the first chunk on, though
//     its rows are not, and its ID stays reserved until the end;
//   - no chunk exceeds the chunk size; and
//   - the writer is released between chunks: after each one the delete waits
//     for a capture batch to commit before it may continue, so a delete that held
//     the writer across chunks would time out here.
func testLargeDelete(t *testing.T, s *SQLite, rows int) {
	big := newRecording(t, s, "big")
	other := newRecording(t, s, "other")
	keep := newRecording(t, s, "keep")
	fillRecording(t, s, big.ID, rows)
	appendN(t, s, keep.ID, 7)
	chunk := s.deleteChunk

	var (
		appended atomic.Int64 // exchanges committed to other
		stop     = make(chan struct{})
		wg       sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i += 2 {
			select {
			case <-stop:
				return
			default:
			}
			if err := s.AppendExchanges(ctx(), other.ID, []*model.Exchange{mkEx(i), mkEx(i + 1)}); err != nil {
				t.Errorf("capture append during delete: %v", err)
				return
			}
			appended.Add(2)
		}
	}()

	var (
		chunks   []int
		progress []int64 // captured exchanges at each chunk boundary
	)
	s.afterPurgeChunk = func(id string, n int) {
		if id != big.ID {
			t.Errorf("chunk reported for %q", id)
		}
		chunks = append(chunks, n)
		if n > chunk {
			t.Errorf("chunk %d deleted %d rows, more than %d", len(chunks), n, chunk)
		}

		// Mid-purge: hidden from clients, rows still there, neighbours untouched.
		expectGone(t, s, big.ID)
		if left := exchangeRows(t, s, big.ID); left != rows-sum(chunks) {
			t.Errorf("after chunk %d: %d rows left, want %d", len(chunks), left, rows-sum(chunks))
		}
		if got, err := s.GetRecording(ctx(), keep.ID); err != nil || got.ExchangeCount != 7 {
			t.Errorf("unrelated recording during the purge: %+v, %v", got, err)
		}

		// The writer is free: a capture batch must get through before we go on.
		want := appended.Load() + 2
		deadline := time.After(20 * time.Second)
		for appended.Load() < want {
			select {
			case <-deadline:
				t.Errorf("no capture batch committed after delete chunk %d: the writer was not released", len(chunks))
				return
			case <-time.After(time.Millisecond):
			}
		}
		progress = append(progress, appended.Load())
	}

	start := time.Now()
	err := s.DeleteRecording(ctx(), big.ID)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("deleted %d rows in %d chunks of up to %d in %v; %d exchanges captured meanwhile",
		rows, len(chunks), chunk, time.Since(start), appended.Load())

	if want := expectedChunks(rows, chunk); !reflect.DeepEqual(chunks, want) {
		t.Errorf("chunk sizes = %v, want %v", chunks, want)
	}
	for i := 1; i < len(progress); i++ {
		if progress[i] <= progress[i-1] {
			t.Errorf("no capture progress between chunks %d and %d: %v", i, i+1, progress)
		}
	}

	// Finished: nothing left, the ID is free, and the captured exchanges are all
	// there, numbered without gaps.
	if exchangeRows(t, s, big.ID) != 0 || recordingRows(t, s, big.ID) != 0 {
		t.Error("big recording not fully deleted")
	}
	if err := s.DeleteRecording(ctx(), big.ID); !errors.Is(err, model.ErrNotFound) {
		t.Errorf("second DeleteRecording: %v", err)
	}
	if _, err := s.CreateRecording(ctx(), model.Recording{ID: big.ID, Name: "reborn"}); err != nil {
		t.Errorf("CreateRecording after the purge: %v", err)
	}
	got, err := s.GetRecording(ctx(), other.ID)
	if err != nil || got.ExchangeCount != appended.Load() {
		t.Fatalf("capture recording: %+v, %v; want %d exchanges", got, err, appended.Load())
	}
	var maxSeq, n int64
	if err := s.rdb.QueryRow(`SELECT COALESCE(MAX(seq), 0), COUNT(*) FROM exchanges WHERE recording_id = ?`, other.ID).Scan(&maxSeq, &n); err != nil {
		t.Fatal(err)
	}
	if maxSeq != n || n != appended.Load() {
		t.Errorf("capture recording has %d rows with max seq %d, want %d", n, maxSeq, appended.Load())
	}
	if exchangeRows(t, s, keep.ID) != 7 {
		t.Errorf("unrelated recording lost exchanges")
	}
}

// TestDeleteLargeRecording is the real-size case: more rows than one chunk
// (deleteChunkRows) on a file database. The in-memory variant uses small chunks
// to keep the race detector's runtime down.
func TestDeleteLargeRecording(t *testing.T) {
	t.Run("file, default chunk size", func(t *testing.T) {
		s, _ := openFile(t)
		if s.deleteChunk != deleteChunkRows || deleteChunkRows != 10_000 {
			t.Fatalf("chunk size = %d (const %d), want 10000", s.deleteChunk, deleteChunkRows)
		}
		testLargeDelete(t, s, deleteChunkRows+2_500)
	})
	t.Run("memory, small chunks", func(t *testing.T) {
		s := openMem(t)
		s.deleteChunk = 500
		testLargeDelete(t, s, 2_600)
	})
	t.Run("file, small chunks", func(t *testing.T) {
		s, _ := openFile(t)
		s.deleteChunk = 500
		testLargeDelete(t, s, 2_600)
	})
}

// TestDeleteRecordingChunkBoundaries uses a small chunk size to check the loop
// around exact multiples.
func TestDeleteRecordingChunkBoundaries(t *testing.T) {
	const chunk = 10
	cases := []struct {
		rows int
		want []int
	}{
		{0, []int{0}},
		{1, []int{1}},
		{9, []int{9}},
		{10, []int{10, 0}},
		{11, []int{10, 1}},
		{20, []int{10, 10, 0}},
		{25, []int{10, 10, 5}},
	}
	backends(t, func(t *testing.T, s *SQLite) {
		s.deleteChunk = chunk
		for _, c := range cases {
			t.Run(fmt.Sprint(c.rows), func(t *testing.T) {
				rec := newRecording(t, s, "r")
				fillRecording(t, s, rec.ID, c.rows)
				var chunks []int
				s.afterPurgeChunk = func(_ string, n int) { chunks = append(chunks, n) }
				if err := s.DeleteRecording(ctx(), rec.ID); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(chunks, c.want) {
					t.Errorf("chunks = %v, want %v", chunks, c.want)
				}
				if exchangeRows(t, s, rec.ID) != 0 || recordingRows(t, s, rec.ID) != 0 {
					t.Errorf("rows left behind")
				}
			})
		}
	})
}

// TestDeleteRecordingContext: a cancelled context before the delete starts
// changes nothing; one that ends mid-purge leaves the recording gone and hands
// the rest of the work to the background.
func TestDeleteRecordingContext(t *testing.T) {
	t.Run("cancelled before the call", func(t *testing.T) {
		s := openMem(t)
		rec := newRecording(t, s, "r")
		appendN(t, s, rec.ID, 5)
		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := s.DeleteRecording(cctx, rec.ID); err == nil {
			t.Fatal("DeleteRecording with a cancelled context succeeded")
		}
		if got, err := s.GetRecording(ctx(), rec.ID); err != nil || got.ExchangeCount != 5 {
			t.Errorf("recording after a refused delete: %+v, %v", got, err)
		}
	})

	backends(t, func(t *testing.T, s *SQLite) {
		s.deleteChunk = 10
		rec := newRecording(t, s, "r")
		fillRecording(t, s, rec.ID, 55)

		cctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var once sync.Once
		s.afterPurgeChunk = func(string, int) { once.Do(cancel) }

		err := s.DeleteRecording(cctx, rec.ID)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DeleteRecording = %v, want context.Canceled", err)
		}
		// Gone for clients at once; the background finishes the purge.
		if _, err := s.GetRecording(ctx(), rec.ID); !errors.Is(err, model.ErrNotFound) {
			t.Errorf("GetRecording after an interrupted delete: %v", err)
		}
		s.bgWG.Wait()
		if exchangeRows(t, s, rec.ID) != 0 || recordingRows(t, s, rec.ID) != 0 {
			t.Errorf("background purge left %d exchanges, %d recording rows", exchangeRows(t, s, rec.ID), recordingRows(t, s, rec.ID))
		}
	})
}

// TestConcurrentDeleteRecording: of several simultaneous deletes exactly one
// succeeds and the others see the recording as gone.
func TestConcurrentDeleteRecording(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		s.deleteChunk = 50
		rec := newRecording(t, s, "r")
		fillRecording(t, s, rec.ID, 500)
		const n = 6
		var (
			wg   sync.WaitGroup
			errs = make([]error, n)
		)
		for i := range n {
			wg.Add(1)
			go func() { defer wg.Done(); errs[i] = s.DeleteRecording(ctx(), rec.ID) }()
		}
		wg.Wait()
		ok := 0
		for i, err := range errs {
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, model.ErrNotFound):
				t.Errorf("delete %d: %v", i, err)
			}
		}
		if ok != 1 {
			t.Errorf("%d deletes succeeded, want 1", ok)
		}
		if exchangeRows(t, s, rec.ID) != 0 || recordingRows(t, s, rec.ID) != 0 {
			t.Error("rows left behind")
		}
	})
}

// TestHalfDeletedRecordingIsResumedOnOpen: a purge that never finished (a
// crash, or Close) is invisible to clients and completed by the next Open.
func TestHalfDeletedRecordingIsResumedOnOpen(t *testing.T) {
	s, path := openFile(t)
	gone := newRecording(t, s, "gone")
	keep := newRecording(t, s, "keep")
	fillRecording(t, s, gone.ID, 300)
	appendN(t, s, keep.ID, 4)

	// What DeleteRecording leaves behind if the process dies after its first step.
	if _, err := s.wdb.Exec(`UPDATE recordings SET deleted_at = ? WHERE id = ?`, nanos(baseTime), gone.ID); err != nil {
		t.Fatal(err)
	}
	expectGone(t, s, gone.ID)
	if n := exchangeRows(t, s, gone.ID); n != 300 {
		t.Fatalf("setup: %d exchanges, want 300", n)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.bgWG.Wait() // the purge started by Open
	if exchangeRows(t, s2, gone.ID) != 0 || recordingRows(t, s2, gone.ID) != 0 {
		t.Errorf("Open did not finish the purge: %d exchanges, %d recording rows",
			exchangeRows(t, s2, gone.ID), recordingRows(t, s2, gone.ID))
	}
	if got, err := s2.GetRecording(ctx(), keep.ID); err != nil || got.ExchangeCount != 4 || exchangeRows(t, s2, keep.ID) != 4 {
		t.Errorf("unrelated recording after resume: %+v, %v", got, err)
	}
	list, _ := s2.ListRecordings(ctx())
	if len(list) != 1 || list[0].ID != keep.ID {
		t.Errorf("ListRecordings = %v", list)
	}
}

// TestCloseStopsBackgroundPurge: Close does not hang on or race with a purge
// that is still running, and what it left is picked up by the next Open.
func TestCloseStopsBackgroundPurge(t *testing.T) {
	s, path := openFile(t)
	rec := newRecording(t, s, "r")
	fillRecording(t, s, rec.ID, 400)
	if _, err := s.wdb.Exec(`UPDATE recordings SET deleted_at = 1 WHERE id = ?`, rec.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	for i := range 3 { // open and close at once, racing the purge Open starts
		s2, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := s2.Close(); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}
	s3, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	s3.bgWG.Wait()
	if exchangeRows(t, s3, rec.ID) != 0 || recordingRows(t, s3, rec.ID) != 0 {
		t.Error("purge never completed")
	}
	// Starting a background purge on a closed store is a no-op, not a panic.
	s3.bgMu.Lock()
	s3.bgClosed = true
	s3.bgMu.Unlock()
	s3.purgeInBackground("rec_x")
}
