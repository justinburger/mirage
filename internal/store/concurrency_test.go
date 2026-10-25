package store

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// TestConcurrentAppendAndRead runs writers and readers of every kind at once
// and then checks the totals, the per-recording sequences and ID uniqueness.
// It is most useful under -race.
func TestConcurrentAppendAndRead(t *testing.T) {
	const (
		writers   = 6
		batches   = 10
		batchSize = 25
	)
	backends(t, func(t *testing.T, s *SQLite) {
		shared := newRecording(t, s, "shared")
		own := make([]model.Recording, writers)
		for i := range own {
			own[i] = newRecording(t, s, fmt.Sprintf("own-%d", i))
		}

		var (
			errMu    sync.Mutex
			firstErr error
			stop     atomic.Bool
		)
		fail := func(format string, args ...any) {
			errMu.Lock()
			if firstErr == nil {
				firstErr = fmt.Errorf(format, args...)
			}
			errMu.Unlock()
		}

		// Readers: every read API against recordings that are growing.
		var readers sync.WaitGroup
		for r := range 5 {
			readers.Add(1)
			go func() {
				defer readers.Done()
				for i := 0; !stop.Load(); i++ {
					target := shared
					if r%2 == 1 {
						target = own[i%writers]
					}
					switch r {
					case 0, 1:
						items, total, err := s.ListExchanges(ctx(), target.ID, model.ExchangeQuery{Desc: true, Limit: 50})
						if err != nil {
							fail("ListExchanges: %v", err)
							return
						}
						if int64(len(items)) > total {
							fail("ListExchanges returned %d items for a total of %d", len(items), total)
							return
						}
					case 2:
						var last int64
						err := s.IterateExchanges(ctx(), target.ID, model.IterOptions{ResponseBodies: true}, func(e *model.Exchange) error {
							if e.Seq <= last {
								return fmt.Errorf("sequence went from %d to %d", last, e.Seq)
							}
							last = e.Seq
							return nil
						})
						if err != nil {
							fail("IterateExchanges: %v", err)
							return
						}
					case 3:
						if _, err := s.EndpointStats(ctx(), target.ID); err != nil {
							fail("EndpointStats: %v", err)
							return
						}
						if _, err := s.ListRecordings(ctx()); err != nil {
							fail("ListRecordings: %v", err)
							return
						}
					case 4:
						if _, err := s.GetRecording(ctx(), target.ID); err != nil {
							fail("GetRecording: %v", err)
							return
						}
						if _, err := s.GetExchange(ctx(), 1+int64(i%20)); err != nil && !isNotFound(err) {
							fail("GetExchange: %v", err)
							return
						}
						if _, err := s.ListServices(ctx()); err != nil {
							fail("ListServices: %v", err)
							return
						}
					}
				}
			}()
		}

		// Writers: batches to a private and to the shared recording, mixed
		// with configuration writes that compete for the single writer.
		var writersWG sync.WaitGroup
		for w := range writers {
			writersWG.Add(1)
			go func() {
				defer writersWG.Done()
				for b := range batches {
					for _, rec := range []string{own[w].ID, shared.ID} {
						exs := make([]*model.Exchange, batchSize)
						for i := range exs {
							exs[i] = mkEx(b*batchSize+i, withRespBody([]byte(fmt.Sprintf("w%d b%d i%d", w, b, i))))
						}
						if err := s.AppendExchanges(ctx(), rec, exs); err != nil {
							fail("AppendExchanges: %v", err)
							return
						}
					}
					if err := s.PutSetting(ctx(), fmt.Sprintf("writer-%d", w), fmt.Sprint(b)); err != nil {
						fail("PutSetting: %v", err)
						return
					}
					if _, err := s.PutService(ctx(), model.Service{ID: fmt.Sprintf("svc_%d", w), Name: fmt.Sprintf("s%d", w), ListenAddr: fmt.Sprintf(":%d", 9000+w)}); err != nil {
						fail("PutService: %v", err)
						return
					}
				}
			}()
		}
		writersWG.Wait()
		stop.Store(true)
		readers.Wait()
		if firstErr != nil {
			t.Fatal(firstErr)
		}

		// Totals.
		check := func(rec model.Recording, want int64) {
			t.Helper()
			got, err := s.GetRecording(ctx(), rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.ExchangeCount != want {
				t.Errorf("%s: ExchangeCount = %d, want %d", rec.Name, got.ExchangeCount, want)
			}
			// The counter must match reality, and Seq must be 1..N without gaps.
			var seq int64
			err = s.IterateExchanges(ctx(), rec.ID, model.IterOptions{}, func(e *model.Exchange) error {
				seq++
				if e.Seq != seq {
					return fmt.Errorf("gap: position %d has seq %d", seq, e.Seq)
				}
				return nil
			})
			if err != nil || seq != want {
				t.Errorf("%s: iterated %d exchanges (%v), want %d", rec.Name, seq, err, want)
			}
		}
		for _, r := range own {
			check(r, batches*batchSize)
		}
		check(shared, writers*batches*batchSize)

		// IDs are unique across recordings.
		var distinct, total int
		if err := s.rdb.QueryRow(`SELECT COUNT(DISTINCT id), COUNT(*) FROM exchanges`).Scan(&distinct, &total); err != nil {
			t.Fatal(err)
		}
		if distinct != total || total != 2*writers*batches*batchSize {
			t.Errorf("exchanges: %d distinct IDs of %d rows, want %d rows", distinct, total, 2*writers*batches*batchSize)
		}
	})
}

func isNotFound(err error) bool {
	return err != nil && errors.Is(err, model.ErrNotFound)
}

// TestConcurrentConfigWrites hammers upserts of the same keys.
func TestConcurrentConfigWrites(t *testing.T) {
	backends(t, func(t *testing.T, s *SQLite) {
		const goroutines, rounds = 8, 40
		var wg sync.WaitGroup
		errs := make(chan error, goroutines)
		for g := range goroutines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for r := range rounds {
					v := fmt.Sprintf("g%d-r%d", g, r)
					if err := s.PutSetting(ctx(), "shared", v); err != nil {
						errs <- err
						return
					}
					if _, err := s.PutFaultRule(ctx(), model.FaultRule{ID: "flt_one", Name: v, Type: model.FaultAbort}); err != nil {
						errs <- err
						return
					}
					if _, err := s.PutProfile(ctx(), model.AnonymizationProfile{ID: "prof_one", Name: v}); err != nil {
						errs <- err
						return
					}
					if _, err := s.PutReplayJob(ctx(), model.ReplayJob{ID: "rpl_one", Name: v, Series: sampleSeries(5)}); err != nil {
						errs <- err
						return
					}
					if _, err := s.GetSetting(ctx(), "shared"); err != nil {
						errs <- err
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		for table, want := range map[string]int{"fault_rules": 1, "replay_jobs": 1, "settings": 1} {
			if n := countRows(t, s, table); n != want {
				t.Errorf("%s has %d rows, want %d", table, n, want)
			}
		}
		// Every upsert of one profile kept the same secret.
		p, err := s.GetProfile(ctx(), "prof_one")
		if err != nil || len(p.Secret) != 64 {
			t.Errorf("profile after concurrent upserts: %q, %v", p.Secret, err)
		}
	})
}

// TestReadersDoNotWaitForWriter checks that reads keep flowing while a write
// transaction is open, which is what the WAL reader pool is for.
func TestReadersDoNotWaitForWriter(t *testing.T) {
	s, _ := openFile(t)
	rec := newRecording(t, s, "r")
	appendN(t, s, rec.ID, 10)

	tx, err := s.wdb.BeginTx(ctx(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE recordings SET name = 'locked' WHERE id = ?`, rec.ID); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		got, err := s.GetRecording(ctx(), rec.ID)
		if err == nil && got.Name != "r" {
			err = fmt.Errorf("reader saw uncommitted data: %q", got.Name)
		}
		if err == nil {
			_, _, err = s.ListExchanges(ctx(), rec.ID, model.ExchangeQuery{})
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reader blocked behind an open write transaction")
	}
}
