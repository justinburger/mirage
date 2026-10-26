// Package har converts between HTTP Archive (HAR 1.2) files and Mirage
// exchanges, so traffic captured elsewhere (browser devtools, Charles,
// mitmproxy, k6) can be imported and Mirage recordings can be exported.
//
// CONTRACT (implemented by the util agent):
//
//   - Decode reads a whole HAR document. Encoder streams one: it writes the
//     log header immediately and each entry as it is written, so exporting a
//     recording with millions of exchanges uses constant memory. Close
//     writes the closing brackets; the output must be a valid HAR 1.2
//     document that Chrome devtools can import.
//   - ToExchange maps one entry: startedDateTime → StartedAt; time →
//     Duration; timings.wait (+send) → TTFB when present; request URL →
//     Scheme/Host/Path/RawQuery; headers (skip HTTP/2 pseudo-headers such as
//     ":authority"); postData.text (or params re-encoded as a form) → Body;
//     response content.text → Body (decoding base64 when
//     content.encoding == "base64"); content.size → BodySize. Exchanges
//     whose response status is 0 get Error "no response in HAR".
//   - FromExchange maps back, including queryString and cookies arrays, and
//     writes non-UTF-8 bodies as base64 with encoding "base64". Bodies that
//     were truncated during capture are exported as captured, with a
//     "_mirageTruncated": true field on the content/postData object.
//   - Endpoint is left empty by ToExchange; callers fill it with pathnorm.
//
// # Behaviour details
//
// Decisions this implementation made where the contract is silent:
//
//   - Decoding is tolerant of real-world files: a UTF-8 byte order mark,
//     absent optional fields, -1 and fractional sizes and timings, numbers
//     given as strings, null objects, and startedDateTime in any common
//     ISO 8601 / RFC 3339 spelling (offset with or without a colon, any
//     fraction length, a space instead of "T", no zone at all meaning UTC).
//     Unknown fields, including Chrome's underscore fields, are ignored.
//   - Bodies in HAR files are stored decoded. When a HAR response carries a
//     Content-Encoding header, ToExchange moves it into
//     Response.ContentEncoding and removes the header, which is the shape
//     the proxy produces. A base64 body that is still gzip-compressed is
//     decompressed. FromExchange puts the header back so the HAR reads like
//     the original traffic.
//   - Request.Host is the Host header (or the HTTP/2 :authority pseudo
//     header) when present, otherwise the URL host. The Host header itself is
//     not kept in Request.Header, as in net/http.
//   - Header names are canonicalised (http.CanonicalHeaderKey); FromExchange
//     writes headers sorted by name so output is deterministic.
//   - Entries that are not HTTP(S) exchanges (data:, blob:, ws:,
//     chrome-extension: URLs) cannot be converted: ToExchange returns an
//     error wrapping ErrUnsupported, which importers should skip. See
//     ToExchanges.
//   - A params-only request body is re-encoded as
//     application/x-www-form-urlencoded, or as multipart/form-data when the
//     postData mimeType (or Content-Type header) names a boundary.
//   - Exchange.Error round-trips through the response "_error" field that
//     Chrome uses for failed requests.
package har

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// HAR is a decoded HAR document.
type HAR struct {
	Log Log `json:"log"`
}

// Log is the HAR log object.
type Log struct {
	Version string   `json:"version"`
	Creator Creator  `json:"creator"`
	Browser *Creator `json:"browser,omitempty"`
	Pages   []Page   `json:"pages,omitempty"`
	Entries []Entry  `json:"entries"`
	Comment string   `json:"comment,omitempty"`
}

// Creator identifies the producing application (also used for "browser").
type Creator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Comment string `json:"comment,omitempty"`
}

// Decode reads a HAR document. The whole document is held in memory; use
// DecodeEntries to process a large file one entry at a time.
func Decode(r io.Reader) (*HAR, error) {
	var entries []Entry
	lg, err := DecodeEntries(r, func(_ int, e *Entry) error {
		entries = append(entries, *e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	lg.Entries = entries
	return &HAR{Log: *lg}, nil
}

// DecodeEntries reads a HAR document and calls fn for every entry in order,
// without holding more than one entry in memory. It returns the log object
// with Entries left empty. If fn returns an error, reading stops and that
// error is returned (wrapped with the entry index). The *Entry passed to fn
// is only valid during the call.
func DecodeEntries(r io.Reader, fn func(index int, e *Entry) error) (*Log, error) {
	lim := &entryLimiter{r: r, max: maxEntryBytes}
	br := bufio.NewReader(lim)
	if b, err := br.Peek(3); err == nil && bytes.Equal(b, []byte{0xEF, 0xBB, 0xBF}) {
		_, _ = br.Discard(3)
	}
	d := &docDecoder{dec: json.NewDecoder(br), fn: fn}
	lim.dec = d.dec
	lg, err := d.document()
	if err != nil {
		return nil, fmt.Errorf("har: %w", err)
	}
	return lg, nil
}

// maxEntryBytes bounds the size of one entry of the document (and of any other
// single JSON value in it), which has to be held in memory whole, several
// times over, while it is decoded. A variable so tests can lower it.
var maxEntryBytes int64 = 256 << 20

// ErrEntryTooLarge is wrapped by the error DecodeEntries returns for an entry
// larger than the decoder will buffer.
var ErrEntryTooLarge = errors.New("HAR entry is too large")

// entryLimiter fails a read once the JSON decoder is holding more than max
// bytes of one value: the bytes read from r minus the offset the decoder has
// consumed up to (the end of the last token) are the part of the value read
// so far.
type entryLimiter struct {
	r   io.Reader
	n   int64
	max int64
	dec *json.Decoder
	err error // set once the limit is hit; every later Read returns it
}

func (l *entryLimiter) Read(p []byte) (int, error) {
	if l.err != nil {
		return 0, l.err
	}
	n, err := l.r.Read(p)
	l.n += int64(n)
	if l.dec != nil && l.n-l.dec.InputOffset() > l.max {
		if l.max >= 1<<20 {
			l.err = fmt.Errorf("%w (more than %d MiB)", ErrEntryTooLarge, l.max>>20)
		} else {
			l.err = fmt.Errorf("%w (more than %d KiB)", ErrEntryTooLarge, l.max>>10)
		}
		return n, l.err
	}
	return n, err
}

// docDecoder walks the HAR document with json.Decoder tokens so that the
// (possibly huge) entries array is never materialised.
type docDecoder struct {
	dec *json.Decoder
	fn  func(int, *Entry) error
}

// ErrNotHAR is returned (wrapped) when the input is valid JSON but has no
// "log" object.
var ErrNotHAR = errors.New("not a HAR document: no \"log\" object")

func (d *docDecoder) document() (*Log, error) {
	if err := d.expect('{'); err != nil {
		return nil, err
	}
	var lg *Log
	for d.dec.More() {
		key, err := d.key()
		if err != nil {
			return nil, err
		}
		if key != "log" {
			if err := d.skip(); err != nil {
				return nil, err
			}
			continue
		}
		l, err := d.log()
		if err != nil {
			return nil, err
		}
		if l != nil {
			lg = l
		}
	}
	if err := d.expect('}'); err != nil {
		return nil, err
	}
	if lg == nil {
		return nil, ErrNotHAR
	}
	return lg, nil
}

// log decodes the log object; it returns nil for JSON null.
func (d *docDecoder) log() (*Log, error) {
	tok, err := d.dec.Token()
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, nil
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("\"log\" must be an object, got %v", tok)
	}
	lg := &Log{}
	for d.dec.More() {
		key, err := d.key()
		if err != nil {
			return nil, err
		}
		switch key {
		case "version":
			err = d.dec.Decode(&lg.Version)
		case "creator":
			err = d.dec.Decode(&lg.Creator)
		case "browser":
			err = d.dec.Decode(&lg.Browser)
		case "pages":
			err = d.dec.Decode(&lg.Pages)
		case "comment":
			err = d.dec.Decode(&lg.Comment)
		case "entries":
			err = d.entries()
		default:
			err = d.skip()
		}
		if err != nil {
			return nil, fmt.Errorf("log.%s: %w", key, err)
		}
	}
	if err := d.expect('}'); err != nil {
		return nil, err
	}
	return lg, nil
}

func (d *docDecoder) entries() error {
	tok, err := d.dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil // "entries": null
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return fmt.Errorf("expected an array, got %v", tok)
	}
	for i := 0; d.dec.More(); i++ {
		var e Entry
		if err := d.dec.Decode(&e); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		if err := d.fn(i, &e); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
	}
	return d.expect(']')
}

func (d *docDecoder) expect(want json.Delim) error {
	tok, err := d.dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	if got, ok := tok.(json.Delim); !ok || got != want {
		return fmt.Errorf("expected %q, got %v", want, tok)
	}
	return nil
}

func (d *docDecoder) key() (string, error) {
	tok, err := d.dec.Token()
	if err != nil {
		return "", err
	}
	s, ok := tok.(string)
	if !ok {
		return "", fmt.Errorf("expected an object key, got %v", tok)
	}
	return s, nil
}

func (d *docDecoder) skip() error {
	var raw json.RawMessage
	return d.dec.Decode(&raw)
}
