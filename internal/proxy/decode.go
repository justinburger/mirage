package proxy

import (
	"net/http"

	"github.com/justinburger/mirage/internal/bodycodec"
	"github.com/justinburger/mirage/internal/model"
)

// The body decoding of captures lives in internal/bodycodec, shared with the
// anonymized copies, exports and imports; these are the names the proxy uses.

// decodeExchange decodes the request and response bodies of a captured
// exchange in place (see bodycodec.DecodeExchange).
func decodeExchange(ex *model.Exchange, limit int64) { bodycodec.DecodeExchange(ex, limit) }

// decodeExchangeForAnon prepares a capture for the anonymizer: whatever is
// still under a content coding after decoding is dropped, never stored raw
// (see bodycodec.PrepareForAnonymization).
func decodeExchangeForAnon(ex *model.Exchange, limit int64) {
	if limit <= 0 {
		// Bodies are not captured (max_body_bytes -1); nothing to inspect.
		return
	}
	bodycodec.PrepareForAnonymization(ex, limit)
}

// decodeBody and decodeBodyLenient are bodycodec.Decode and DecodeLenient.
func decodeBody(h http.Header, body []byte, size int64, truncated bool, limit int64) ([]byte, int64, bool, string, bool) {
	return bodycodec.Decode(h, body, size, truncated, limit)
}

func decodeBodyLenient(h http.Header, body []byte, size int64, truncated bool, limit int64) ([]byte, int64, bool, string, bool) {
	return bodycodec.DecodeLenient(h, body, size, truncated, limit)
}
