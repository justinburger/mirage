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
package har

import (
	"io"

	"github.com/justinburger/mirage/internal/model"
)

// HAR is a decoded HAR document.
type HAR struct {
	Log Log `json:"log"`
}

// Log is the HAR log object.
type Log struct {
	Version string  `json:"version"`
	Creator Creator `json:"creator"`
	Entries []Entry `json:"entries"`
}

// Creator identifies the producing application.
type Creator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Entry is one HAR entry. The full HAR 1.2 shape (request, response, timings,
// cache, serverIPAddress…) is defined by the implementation.
type Entry struct {
	// fields defined by the implementation
}

// Decode reads a HAR document.
func Decode(r io.Reader) (*HAR, error) { panic("har: not implemented") }

// ToExchange converts a HAR entry to an exchange.
func ToExchange(e *Entry) (*model.Exchange, error) { panic("har: not implemented") }

// FromExchange converts an exchange to a HAR entry.
func FromExchange(ex *model.Exchange) Entry { panic("har: not implemented") }

// Encoder streams a HAR document.
type Encoder struct{}

// NewEncoder starts a HAR document on w with the given creator version.
func NewEncoder(w io.Writer, creatorVersion string) (*Encoder, error) {
	panic("har: not implemented")
}

// Write appends one exchange as an entry.
func (e *Encoder) Write(ex *model.Exchange) error { panic("har: not implemented") }

// Close finishes the document. It does not close the underlying writer.
func (e *Encoder) Close() error { panic("har: not implemented") }
