package ops

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/har"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
	"github.com/justinburger/mirage/internal/store"
)

// SourceHAR is the Recording.Source of recordings created by ImportHAR.
const SourceHAR = "har"

// exportBufferSize is the buffer between the HAR encoder (one Write call per
// entry) and the destination writer.
const exportBufferSize = 64 << 10

// InvalidHARError reports input that is not a usable HAR document. Problems
// say what is wrong, in words for the person who supplied the document (no
// package prefixes). It is a validation error: errors.Is(err,
// model.ErrValidation) holds.
type InvalidHARError struct {
	Problems []string
}

func (e *InvalidHARError) Error() string { return "invalid HAR: " + strings.Join(e.Problems, "; ") }

func (e *InvalidHARError) Unwrap() error { return model.ErrValidation }

// ImportHAR imports a HAR document read from r into a new recording called
// name (an empty name becomes "import <timestamp>"). Entries are streamed, so
// memory use does not depend on the size of the document. n classifies
// request paths into endpoints (nil behaves like pathnorm.Default); eng, if
// not nil, anonymizes every exchange before it is stored and marks the
// recording anonymized (see AnonymizeExchange: compressed bodies are decoded
// or dropped, never kept raw, and the endpoint is built from the anonymized
// path).
//
// An entry that cannot be anonymized is left out, never stored raw, and
// counted in the recording's description; the import fails only when no entry
// could be anonymized at all. An entry whose start time cannot be stored (a
// missing time, or a year before 1678 or after 2261, such as the 0001-01-01
// some tools write) is given the start time of the entry before it, or of
// the first entry that has a usable time when it comes first; a HAR without
// any usable time gets the import time. These are counted in the description
// too, so a timeline replay of the recording keeps a sane length.
//
// The import is all-or-nothing: on any error the partial recording is deleted.
// Unreadable HAR content (empty input included) returns an *InvalidHARError,
// which wraps model.ErrValidation; an error of r itself (for example a dropped
// upload) is returned wrapped but otherwise unchanged, so callers can
// recognise it with errors.Is/As.
func ImportHAR(ctx context.Context, st store.Store, r io.Reader, name string, eng *anonymize.Engine, n *pathnorm.Normalizer) (model.Recording, error) {
	rec, _, err := ImportHARStats(ctx, st, r, name, eng, n)
	return rec, err
}

// ImportHARStats is ImportHAR that also returns the Stats of the import:
// entries written, skipped as unsupported, left out because they could not be
// anonymized, and given a neighbour's start time.
func ImportHARStats(ctx context.Context, st store.Store, r io.Reader, name string, eng *anonymize.Engine, n *pathnorm.Normalizer) (model.Recording, Stats, error) {
	return importHAR(ctx, st, r, name, asAnonymizer(eng), n)
}

func importHAR(ctx context.Context, st store.Store, r io.Reader, name string, eng Anonymizer, n *pathnorm.Normalizer) (model.Recording, Stats, error) {
	var stats Stats
	if n == nil {
		n = pathnorm.Default
	}
	if strings.TrimSpace(name) == "" {
		name = "import " + time.Now().Format("2006-01-02 15:04:05")
	}
	rec, err := st.CreateRecording(ctx, model.Recording{
		Name:        name,
		Description: "Imported from a HAR file.",
		Source:      SourceHAR,
		Anonymized:  eng != nil,
	})
	if err != nil {
		return model.Recording{}, stats, fmt.Errorf("ops: import HAR: create recording: %w", err)
	}

	var (
		fnErr   error
		fnIndex int
		app     = newAppender(st, rec.ID, nil)
		im      = &importer{app: app, eng: eng, n: n, stats: &stats, now: time.Now().UTC()}
		src     = &errReader{r: r}
	)
	lg, err := har.DecodeEntries(src, func(index int, e *har.Entry) error {
		fnErr, fnIndex = im.entry(ctx, index, e), index
		return fnErr
	})
	switch {
	case err != nil && fnErr != nil && errors.Is(err, fnErr):
		// A failure of ours (store, cancelled context, bad entry). A bad
		// entry is reported as the input's fault, without the decoder's
		// wrapping.
		var ih *InvalidHARError
		if errors.As(fnErr, &ih) {
			err = &InvalidHARError{Problems: []string{fmt.Sprintf("entry %d: %s", fnIndex, strings.Join(ih.Problems, "; "))}}
		}
	case err != nil && src.err != nil:
		// The reader failed; that is not the HAR's fault.
		err = fmt.Errorf("read HAR: %w", src.err)
	case err != nil:
		err = &InvalidHARError{Problems: []string{harProblem(err, src.sawData)}}
	default:
		err = im.finish(ctx)
		stats.Written = app.done
		if err == nil && stats.allFailed() {
			err = stats.allFailedError("anonymize")
		}
	}
	if err != nil {
		return model.Recording{}, stats, discard(ctx, st, rec.ID, fmt.Errorf("ops: import HAR: %w", err))
	}

	rec.Description = importDescription(app.done, lg, eng, stats)
	if _, err := st.UpdateRecording(ctx, rec); err != nil {
		return model.Recording{}, stats, discard(ctx, st, rec.ID, fmt.Errorf("ops: import HAR: %w", err))
	}
	out, err := st.GetRecording(ctx, rec.ID)
	if err != nil {
		return model.Recording{}, stats, discard(ctx, st, rec.ID, fmt.Errorf("ops: import HAR: %w", err))
	}
	return out, stats, nil
}

// Limits of the exchanges an importer holds back while it waits for the first
// entry with a usable start time (see importer.add).
const (
	maxLeadEntries = 4096
	maxLeadBytes   = 32 << 20
)

// importer converts HAR entries to exchanges and queues them.
type importer struct {
	app   *appender
	eng   Anonymizer
	n     *pathnorm.Normalizer
	stats *Stats
	// now is the import time, the start time of last resort.
	now time.Time

	// prev is the start time of the last exchange queued (valid once have).
	prev time.Time
	have bool
	// lead holds the exchanges before the first one with a usable start time,
	// which will take that start time.
	lead      []*model.Exchange
	leadBytes int
}

// entry converts one HAR entry and queues it.
func (im *importer) entry(ctx context.Context, index int, e *har.Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ex, err := har.ToExchange(e)
	if err != nil {
		if errors.Is(err, har.ErrUnsupported) {
			im.stats.Skipped++
			return nil
		}
		return &InvalidHARError{Problems: []string{strings.TrimPrefix(err.Error(), "har: ")}}
	}
	if im.eng != nil {
		// The endpoint is built from the raw and the anonymized path.
		if _, err := AnonymizeExchange(im.eng, ex, im.n, false); err != nil {
			// Never store what the anonymizer could not process.
			im.stats.fail(fmt.Errorf("entry %d: %w", index, err))
			return nil
		}
	} else {
		// The endpoint comes from the raw escaped path.
		ex.Endpoint = im.n.Endpoint(ex.Request.Method, ex.Request.Path)
	}
	return im.add(ctx, ex)
}

// add queues ex, repairing a start time that cannot be stored.
func (im *importer) add(ctx context.Context, ex *model.Exchange) error {
	if har.StartTimeStorable(ex.StartedAt) {
		if !im.have {
			if err := im.release(ctx, ex.StartedAt); err != nil {
				return err
			}
		}
		im.prev = ex.StartedAt
		return im.app.add(ctx, ex)
	}
	im.stats.BadTimes++
	if im.have {
		ex.StartedAt = im.prev
		return im.app.add(ctx, ex)
	}
	im.lead = append(im.lead, ex)
	im.leadBytes += len(ex.Request.Body) + len(ex.Response.Body)
	if len(im.lead) >= maxLeadEntries || im.leadBytes >= maxLeadBytes {
		return im.release(ctx, im.now)
	}
	return nil
}

// release gives the held back exchanges start time t and queues them.
func (im *importer) release(ctx context.Context, t time.Time) error {
	im.prev, im.have = t, true
	lead := im.lead
	im.lead, im.leadBytes = nil, 0
	for _, ex := range lead {
		ex.StartedAt = t
		if err := im.app.add(ctx, ex); err != nil {
			return err
		}
	}
	return nil
}

// finish queues what is still held back and writes the last batch.
func (im *importer) finish(ctx context.Context) error {
	if len(im.lead) > 0 {
		if err := im.release(ctx, im.now); err != nil {
			return err
		}
	}
	return im.app.flush(ctx)
}

// harProblem words a failure to read the HAR document. sawData says whether
// the input held anything besides white space.
func harProblem(err error, sawData bool) string {
	var syn *json.SyntaxError
	switch {
	case !sawData && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)):
		return "the input is empty: send a HAR document"
	case errors.As(err, &syn):
		return fmt.Sprintf("malformed JSON at byte %d: %v", syn.Offset, syn)
	}
	msg := strings.TrimPrefix(err.Error(), "har: ")
	return strings.ReplaceAll(msg, "unexpected EOF", "unexpected end of input (is the document truncated?)")
}

func importDescription(imported int64, lg *har.Log, eng Anonymizer, stats Stats) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Imported %d exchange", imported)
	if imported != 1 {
		b.WriteByte('s')
	}
	b.WriteString(" from a HAR file")
	if lg != nil && lg.Creator.Name != "" {
		fmt.Fprintf(&b, " written by %s", lg.Creator.Name)
		if lg.Creator.Version != "" {
			fmt.Fprintf(&b, " %s", lg.Creator.Version)
		}
	}
	b.WriteByte('.')
	if skipped := stats.Skipped; skipped > 0 {
		fmt.Fprintf(&b, " Skipped %d unsupported entr", skipped)
		if skipped == 1 {
			b.WriteString("y")
		} else {
			b.WriteString("ies")
		}
		b.WriteString(" (not HTTP or HTTPS requests, such as data: URLs).")
	}
	if eng != nil {
		p := eng.Profile()
		fmt.Fprintf(&b, " Anonymized with profile %q.", p.Name)
		if stats.Failed > 0 {
			fmt.Fprintf(&b, " Left out %s that could not be anonymized.", plural(stats.Failed, "entry", "entries"))
		}
	}
	if stats.BadTimes > 0 {
		fmt.Fprintf(&b, " %s had a missing start time or one outside the years 1678 to 2261 and took the start time of a neighbouring entry.", plural(stats.BadTimes, "entry", "entries"))
	}
	return b.String()
}

// ExportHAR writes recording recordingID to w as a HAR 1.2 document, labelled
// with version as the creator version. When eng is not nil every exchange is
// anonymized on its way out (the stored recording is untouched; see
// AnonymizeExchange: compressed bodies are decoded or dropped, never written
// raw). An exchange that cannot be anonymized is left out of the document; the
// export fails only when none could be anonymized.
//
// The recording is looked up before anything is written, so an unknown
// recording (an error wrapping model.ErrNotFound) leaves w untouched. If the
// export fails midway the document is left unfinished on purpose, so the
// output is visibly truncated, and the buffered part is still flushed to w.
func ExportHAR(ctx context.Context, st store.Store, recordingID string, w io.Writer, eng *anonymize.Engine, version string) error {
	_, err := ExportHARStats(ctx, st, recordingID, w, eng, version)
	return err
}

// ExportHARStats is ExportHAR that also returns the Stats of the export:
// how many exchanges were written and how many could not be anonymized and
// were left out.
func ExportHARStats(ctx context.Context, st store.Store, recordingID string, w io.Writer, eng *anonymize.Engine, version string) (Stats, error) {
	return exportHAR(ctx, st, recordingID, w, asAnonymizer(eng), version)
}

func exportHAR(ctx context.Context, st store.Store, recordingID string, w io.Writer, eng Anonymizer, version string) (Stats, error) {
	var stats Stats
	if _, err := st.GetRecording(ctx, recordingID); err != nil {
		return stats, fmt.Errorf("ops: export recording %s: %w", recordingID, err)
	}
	bw := bufio.NewWriterSize(w, exportBufferSize)
	enc, err := har.NewEncoder(bw, version)
	if err != nil {
		return stats, fmt.Errorf("ops: export recording %s: %w", recordingID, err)
	}
	err = st.IterateExchanges(ctx, recordingID, model.IterOptions{RequestBodies: true, ResponseBodies: true}, func(ex *model.Exchange) error {
		if eng != nil {
			id := ex.ID
			if _, err := AnonymizeExchange(eng, ex, pathnorm.Default, false); err != nil {
				// Never write what the anonymizer could not process.
				stats.fail(fmt.Errorf("exchange %d: %w", id, err))
				return nil
			}
		}
		if err := enc.Write(ex); err != nil {
			return err
		}
		stats.Written++
		return nil
	})
	if err == nil && stats.allFailed() {
		// The document stays unfinished: an export with nothing in it must
		// not pass for a (short) valid HAR.
		err = stats.allFailedError("anonymize")
	}
	if err == nil {
		err = enc.Close()
	}
	if ferr := bw.Flush(); err == nil {
		err = ferr
	}
	if err != nil {
		return stats, fmt.Errorf("ops: export recording %s: %w", recordingID, err)
	}
	return stats, nil
}

// errReader remembers the first non-EOF error of the wrapped reader, which
// lets ImportHAR tell a broken input stream from a broken document.
type errReader struct {
	r   io.Reader
	err error
	// sawData is set once anything but JSON white space has been read.
	sawData bool
}

func (e *errReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if !e.sawData && len(bytes.TrimSpace(p[:n])) > 0 {
		e.sawData = true
	}
	if err != nil && err != io.EOF && e.err == nil {
		e.err = err
	}
	return n, err
}
