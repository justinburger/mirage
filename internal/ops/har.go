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
// recording anonymized.
//
// The import is all-or-nothing: on any error the partial recording is deleted.
// Unreadable HAR content (empty input included) returns an *InvalidHARError,
// which wraps model.ErrValidation; an error of r itself (for example a dropped
// upload) is returned wrapped but otherwise unchanged, so callers can
// recognise it with errors.Is/As.
func ImportHAR(ctx context.Context, st store.Store, r io.Reader, name string, eng *anonymize.Engine, n *pathnorm.Normalizer) (model.Recording, error) {
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
		return model.Recording{}, fmt.Errorf("ops: import HAR: create recording: %w", err)
	}

	var (
		skipped int64
		fnErr   error
		fnIndex int
		app     = newAppender(st, rec.ID, nil)
		src     = &errReader{r: r}
	)
	lg, err := har.DecodeEntries(src, func(index int, e *har.Entry) error {
		fnErr, fnIndex = importEntry(ctx, app, e, eng, n, &skipped), index
		return fnErr
	})
	switch {
	case err != nil && fnErr != nil && errors.Is(err, fnErr):
		// A failure of ours (store, anonymizer, cancelled context, bad entry).
		// A bad entry is reported as the input's fault, without the decoder's
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
		err = app.flush(ctx)
	}
	if err != nil {
		return model.Recording{}, discard(ctx, st, rec.ID, fmt.Errorf("ops: import HAR: %w", err))
	}

	rec.Description = importDescription(app.done, skipped, lg, eng)
	if _, err := st.UpdateRecording(ctx, rec); err != nil {
		return model.Recording{}, discard(ctx, st, rec.ID, fmt.Errorf("ops: import HAR: %w", err))
	}
	out, err := st.GetRecording(ctx, rec.ID)
	if err != nil {
		return model.Recording{}, discard(ctx, st, rec.ID, fmt.Errorf("ops: import HAR: %w", err))
	}
	return out, nil
}

// importEntry converts one HAR entry and queues it.
func importEntry(ctx context.Context, app *appender, e *har.Entry, eng *anonymize.Engine, n *pathnorm.Normalizer, skipped *int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ex, err := har.ToExchange(e)
	if err != nil {
		if errors.Is(err, har.ErrUnsupported) {
			*skipped++
			return nil
		}
		return &InvalidHARError{Problems: []string{strings.TrimPrefix(err.Error(), "har: ")}}
	}
	// The endpoint comes from the raw escaped path, before anonymization can
	// replace segments of it.
	ex.Endpoint = n.Endpoint(ex.Request.Method, ex.Request.Path)
	if eng != nil {
		if err := eng.Exchange(ex); err != nil {
			return fmt.Errorf("anonymize %s %s: %w", ex.Request.Method, ex.Request.Path, err)
		}
	}
	return app.add(ctx, ex)
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

func importDescription(imported, skipped int64, lg *har.Log, eng *anonymize.Engine) string {
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
	if skipped > 0 {
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
	}
	return b.String()
}

// ExportHAR writes recording recordingID to w as a HAR 1.2 document, labelled
// with version as the creator version. When eng is not nil every exchange is
// anonymized on its way out (the stored recording is untouched).
//
// The recording is looked up before anything is written, so an unknown
// recording (an error wrapping model.ErrNotFound) leaves w untouched. If the
// export fails midway the document is left unfinished on purpose, so the
// output is visibly truncated, and the buffered part is still flushed to w.
func ExportHAR(ctx context.Context, st store.Store, recordingID string, w io.Writer, eng *anonymize.Engine, version string) error {
	if _, err := st.GetRecording(ctx, recordingID); err != nil {
		return fmt.Errorf("ops: export recording %s: %w", recordingID, err)
	}
	bw := bufio.NewWriterSize(w, exportBufferSize)
	enc, err := har.NewEncoder(bw, version)
	if err != nil {
		return fmt.Errorf("ops: export recording %s: %w", recordingID, err)
	}
	err = st.IterateExchanges(ctx, recordingID, model.IterOptions{RequestBodies: true, ResponseBodies: true}, func(ex *model.Exchange) error {
		if eng != nil {
			id := ex.ID
			if err := eng.Exchange(ex); err != nil {
				return fmt.Errorf("anonymize exchange %d: %w", id, err)
			}
		}
		return enc.Write(ex)
	})
	if err == nil {
		err = enc.Close()
	}
	if ferr := bw.Flush(); err == nil {
		err = ferr
	}
	if err != nil {
		return fmt.Errorf("ops: export recording %s: %w", recordingID, err)
	}
	return nil
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
