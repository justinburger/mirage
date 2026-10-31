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
// anonymizing every exchange with eng (see AnonymizeExchange: compressed
// bodies are decoded or dropped, never kept raw, and the endpoint is rebuilt
// from the anonymized path). An empty name becomes "<source name> (anon)". The
// source recording is not modified. progress, if not nil, is called after
// every batch with the number of exchanges written so far.
//
// An exchange that cannot be anonymized is left out of the copy (it is never
// written raw) and counted in the description of the new recording; the copy
// fails only when no exchange could be anonymized at all.
//
// The copy is all-or-nothing: if anything fails, including cancellation of
// ctx, the partially filled destination is deleted again. The returned
// recording carries the final counters.
func CopyAnonymized(ctx context.Context, st store.Store, srcID string, eng *anonymize.Engine, name string, progress Progress) (model.Recording, error) {
	rec, _, err := CopyAnonymizedStats(ctx, st, srcID, eng, name, progress, nil)
	return rec, err
}

// CopyAnonymizedReport is CopyAnonymized that also hands the changes the
// engine made to every exchange to report (when not nil), one call per
// anonymized exchange, in recording order. The slice is only valid during the
// call.
func CopyAnonymizedReport(ctx context.Context, st store.Store, srcID string, eng *anonymize.Engine, name string, progress Progress, report func([]model.AnonChange)) (model.Recording, error) {
	rec, _, err := CopyAnonymizedStats(ctx, st, srcID, eng, name, progress, report)
	return rec, err
}

// CopyAnonymizedStats is CopyAnonymizedReport that also returns the Stats of
// the copy: how many exchanges were written and how many could not be
// anonymized and were left out.
func CopyAnonymizedStats(ctx context.Context, st store.Store, srcID string, eng *anonymize.Engine, name string, progress Progress, report func([]model.AnonChange)) (model.Recording, Stats, error) {
	return copyAnonymized(ctx, st, srcID, asAnonymizer(eng), name, progress, report)
}

func copyAnonymized(ctx context.Context, st store.Store, srcID string, eng Anonymizer, name string, progress Progress, report func([]model.AnonChange)) (model.Recording, Stats, error) {
	var stats Stats
	if eng == nil {
		return model.Recording{}, stats, fmt.Errorf("ops: copy recording %s: an anonymization engine is required: %w", srcID, model.ErrValidation)
	}
	src, err := st.GetRecording(ctx, srcID)
	if err != nil {
		return model.Recording{}, stats, fmt.Errorf("ops: copy recording %s: %w", srcID, err)
	}
	if name == "" {
		name = src.Name + " (anon)"
	}
	prof := eng.Profile()
	desc := fmt.Sprintf("Anonymized copy of %q (%s) using profile %q (%s).", src.Name, src.ID, prof.Name, prof.ID)
	dst, err := st.CreateRecording(ctx, model.Recording{
		Name:        name,
		Description: desc,
		Source:      SourceCopy,
		Anonymized:  true,
		Labels:      maps.Clone(src.Labels),
	})
	if err != nil {
		return model.Recording{}, stats, fmt.Errorf("ops: copy recording %s: create destination: %w", srcID, err)
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
		stored, method, rawPath := ex.Endpoint, ex.Request.Method, ex.Request.Path
		changes, err := AnonymizeExchange(eng, &ex, pathnorm.Default, report != nil)
		if err != nil {
			// Never store what the anonymizer could not process; carry on
			// with the rest of the recording.
			stats.fail(fmt.Errorf("exchange %d: %w", src.ID, err))
			return nil
		}
		ex.Endpoint = keepStoredEndpoint(stored, ex.Endpoint, method, rawPath, ex.Request.Path)
		if report != nil {
			report(changes)
		}
		return app.add(ctx, &ex)
	})
	if err == nil {
		err = app.flush(ctx)
	}
	stats.Written = app.done
	if err == nil && stats.allFailed() {
		err = stats.allFailedError("anonymize")
	}
	if err == nil && stats.Failed > 0 {
		dst.Description = desc + failedNote(stats.Failed)
		_, err = st.UpdateRecording(ctx, dst)
	}
	if err != nil {
		return model.Recording{}, stats, discard(ctx, st, dst.ID, fmt.Errorf("ops: copy recording %s: %w", srcID, err))
	}
	out, err := st.GetRecording(ctx, dst.ID)
	if err != nil {
		return model.Recording{}, stats, discard(ctx, st, dst.ID, fmt.Errorf("ops: copy recording %s: %w", srcID, err))
	}
	return out, stats, nil
}
