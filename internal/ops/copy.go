package ops

import (
	"context"
	"fmt"
	"maps"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
	"github.com/justinburger/mirage/internal/store"
)

// SourceCopy is the Recording.Source of anonymized copies.
const SourceCopy = "copy"

// CopyAnonymized copies recording srcID into a new recording named name,
// anonymizing every exchange with eng. An empty name becomes "<source name>
// (anon)". The source recording is not modified. progress, if not nil, is
// called after every batch with the number of exchanges written so far.
//
// The copy is all-or-nothing: if anything fails, including cancellation of
// ctx, the partially filled destination is deleted again. The returned
// recording carries the final counters.
func CopyAnonymized(ctx context.Context, st store.Store, srcID string, eng *anonymize.Engine, name string, progress Progress) (model.Recording, error) {
	return CopyAnonymizedReport(ctx, st, srcID, eng, name, progress, nil)
}

// CopyAnonymizedReport is CopyAnonymized that also hands the changes the
// engine made to every exchange to report (when not nil), one call per
// exchange, in recording order. The slice is only valid during the call.
func CopyAnonymizedReport(ctx context.Context, st store.Store, srcID string, eng *anonymize.Engine, name string, progress Progress, report func([]model.AnonChange)) (model.Recording, error) {
	if eng == nil {
		return model.Recording{}, fmt.Errorf("ops: copy recording %s: an anonymization engine is required: %w", srcID, model.ErrValidation)
	}
	src, err := st.GetRecording(ctx, srcID)
	if err != nil {
		return model.Recording{}, fmt.Errorf("ops: copy recording %s: %w", srcID, err)
	}
	if name == "" {
		name = src.Name + " (anon)"
	}
	prof := eng.Profile()
	dst, err := st.CreateRecording(ctx, model.Recording{
		Name:        name,
		Description: fmt.Sprintf("Anonymized copy of %q (%s) using profile %q (%s).", src.Name, src.ID, prof.Name, prof.ID),
		Source:      SourceCopy,
		Anonymized:  true,
		Labels:      maps.Clone(src.Labels),
	})
	if err != nil {
		return model.Recording{}, fmt.Errorf("ops: copy recording %s: create destination: %w", srcID, err)
	}

	app := newAppender(st, dst.ID, progress)
	err = st.IterateExchanges(ctx, srcID, model.IterOptions{RequestBodies: true, ResponseBodies: true}, func(src *model.Exchange) error {
		// Work on a shallow copy. AppendExchanges assigns ID and Seq in place,
		// and IterateExchanges reads the Seq of its last exchange after the
		// callback returned to find the next page: handing it the very object
		// it gave us would corrupt its pagination. The copy owns src's bodies
		// and headers from here on; src is not used again.
		ex := *src
		// The store assigns new IDs and sequence numbers in the copy.
		ex.ID, ex.Seq, ex.RecordingID = 0, 0, ""
		if ex.Endpoint == "" {
			ex.Endpoint = pathnorm.Default.Endpoint(ex.Request.Method, ex.Request.Path)
		}
		if report != nil {
			changes, err := eng.ExchangeWithReport(&ex)
			if err != nil {
				return fmt.Errorf("anonymize exchange %d: %w", src.ID, err)
			}
			report(changes)
		} else if err := eng.Exchange(&ex); err != nil {
			return fmt.Errorf("anonymize exchange %d: %w", src.ID, err)
		}
		return app.add(ctx, &ex)
	})
	if err == nil {
		err = app.flush(ctx)
	}
	if err != nil {
		return model.Recording{}, discard(ctx, st, dst.ID, fmt.Errorf("ops: copy recording %s: %w", srcID, err))
	}
	out, err := st.GetRecording(ctx, dst.ID)
	if err != nil {
		return model.Recording{}, discard(ctx, st, dst.ID, fmt.Errorf("ops: copy recording %s: %w", srcID, err))
	}
	return out, nil
}
