package ops

import (
	"context"
	"fmt"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/store"
)

const (
	// batchSize is the most exchanges written per AppendExchanges call.
	batchSize = 500
	// batchBytes flushes a batch early once its bodies add up to this many
	// bytes, so recordings with large bodies do not balloon memory.
	batchBytes = 8 << 20
	// cleanupTimeout bounds the deletion of a partial recording. The store
	// finishes a longer purge in the background.
	cleanupTimeout = 2 * time.Minute
)

// appender collects exchanges and writes them to one recording in batches.
type appender struct {
	st       store.Store
	recID    string
	progress Progress

	batch []*model.Exchange
	bytes int
	done  int64
}

func newAppender(st store.Store, recID string, progress Progress) *appender {
	return &appender{st: st, recID: recID, progress: progress, batch: make([]*model.Exchange, 0, batchSize)}
}

// add queues ex and flushes when the batch is full. The appender owns ex from
// here on.
func (a *appender) add(ctx context.Context, ex *model.Exchange) error {
	a.batch = append(a.batch, ex)
	a.bytes += len(ex.Request.Body) + len(ex.Response.Body)
	if len(a.batch) >= batchSize || a.bytes >= batchBytes {
		return a.flush(ctx)
	}
	return nil
}

// flush writes the queued exchanges and reports progress.
func (a *appender) flush(ctx context.Context) error {
	if len(a.batch) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.st.AppendExchanges(ctx, a.recID, a.batch); err != nil {
		return fmt.Errorf("ops: append exchanges: %w", err)
	}
	a.done += int64(len(a.batch))
	clear(a.batch) // drop references so the bodies can be collected
	a.batch = a.batch[:0]
	a.bytes = 0
	if a.progress != nil {
		a.progress(a.done)
	}
	return nil
}

// discard deletes a partially filled recording after cause and returns cause,
// annotated when the deletion itself failed. It runs on a context detached from
// ctx so that a cancelled operation still cleans up after itself.
func discard(ctx context.Context, st store.Store, recID string, cause error) error {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := st.DeleteRecording(dctx, recID); err != nil {
		return fmt.Errorf("%w (the partial recording %s could not be deleted: %v)", cause, recID, err)
	}
	return cause
}
