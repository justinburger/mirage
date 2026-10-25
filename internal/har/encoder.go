package har

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/justinburger/mirage/internal/model"
)

// ErrClosed is returned by Encoder.Write after Close.
var ErrClosed = errors.New("har: encoder is closed")

// maxRetainedBuffer is the largest scratch buffer an Encoder keeps between
// writes. One huge body must not pin its memory for the rest of an export.
const maxRetainedBuffer = 1 << 20

// Encoder streams a HAR document.
//
// The log header is written when the Encoder is created, every entry as soon
// as it is written, and the closing brackets by Close. Memory use does not
// depend on the number of entries. The output has one entry per line and is
// not indented. Each Write issues a single call to the underlying writer, so
// wrap files and sockets in a bufio.Writer for throughput and flush it after
// Close.
//
// An Encoder is safe for concurrent use; entries appear in the order the
// calls acquire it.
type Encoder struct {
	mu     sync.Mutex
	w      io.Writer
	buf    bytes.Buffer
	enc    *json.Encoder
	n      int   // entries written
	err    error // sticky error from the underlying writer
	closed bool
	closeE error // result of the first Close
}

// NewEncoder starts a HAR document on w with the given creator version.
func NewEncoder(w io.Writer, creatorVersion string) (*Encoder, error) {
	e := &Encoder{w: w}
	e.enc = json.NewEncoder(&e.buf)
	e.enc.SetEscapeHTML(false)

	e.buf.WriteString(`{"log":{"version":"1.2","creator":{"name":"Mirage","version":`)
	if err := e.enc.Encode(creatorVersion); err != nil {
		return nil, fmt.Errorf("har: creator version: %w", err)
	}
	e.buf.Truncate(e.buf.Len() - 1) // Encode's trailing newline
	e.buf.WriteString(`},"entries":[` + "\n")
	if err := e.flush(); err != nil {
		return nil, err
	}
	return e, nil
}

// Write appends one exchange as an entry.
func (e *Encoder) Write(ex *model.Exchange) error {
	if ex == nil {
		return errors.New("har: nil exchange")
	}
	entry := FromExchange(ex)

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	if e.err != nil {
		return e.err
	}
	e.buf.Reset()
	if e.n > 0 {
		e.buf.WriteByte(',')
	}
	if err := e.enc.Encode(&entry); err != nil { // appends the newline
		e.buf.Reset()
		return fmt.Errorf("har: encoding entry %d: %w", e.n, err)
	}
	if err := e.flush(); err != nil {
		return err
	}
	e.n++
	return nil
}

// Count returns the number of entries written so far.
func (e *Encoder) Count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.n
}

// Close finishes the document. It does not close the underlying writer. It is
// safe to call more than once; later calls return the first call's result and
// write nothing.
func (e *Encoder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return e.closeE
	}
	e.closed = true
	if e.err != nil {
		e.closeE = e.err
		return e.closeE
	}
	e.buf.Reset()
	e.buf.WriteString("]}}\n")
	e.closeE = e.flush()
	e.buf = bytes.Buffer{}
	return e.closeE
}

// flush writes the buffer to the underlying writer in one call. e.mu must be
// held (or the Encoder not yet shared).
func (e *Encoder) flush() error {
	if e.err != nil {
		return e.err
	}
	_, err := e.w.Write(e.buf.Bytes())
	if err != nil {
		e.err = fmt.Errorf("har: write: %w", err)
		return e.err
	}
	if e.buf.Cap() > maxRetainedBuffer {
		e.buf = bytes.Buffer{} // e.enc keeps pointing at &e.buf
	}
	return nil
}
