package ops

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/bodycodec"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
)

// AnonymizeBodyLimit is the most decoded bytes per body that anonymizing
// operations inspect: the per-entry cap of the HAR reader. A body that
// inflates past it is cut (and marked truncated) at the limit, which is also
// the most memory one compressed body can claim.
const AnonymizeBodyLimit = 256 << 20

// Anonymizer is what the anonymizing operations need of an engine.
// *anonymize.Engine implements it; tests substitute failing engines.
type Anonymizer interface {
	// Profile describes the anonymization (for recording descriptions).
	Profile() model.AnonymizationProfile
	// Exchange anonymizes an exchange in place; on error the exchange may be
	// partly anonymized and must not be stored.
	Exchange(*model.Exchange) error
	// ExchangeWithReport is Exchange that also lists what changed.
	ExchangeWithReport(*model.Exchange) ([]model.AnonChange, error)
}

// asAnonymizer converts an engine to the interface, keeping a nil engine nil
// (a nil *anonymize.Engine in an interface would not compare equal to nil).
func asAnonymizer(eng *anonymize.Engine) Anonymizer {
	if eng == nil {
		return nil
	}
	return eng
}

// AnonymizeExchange anonymizes ex in place with eng, the one way every
// anonymizing operation (copies, exports, imports, the CLI) does it:
//
//  1. bodycodec.PrepareForAnonymization decodes compressed bodies, up to
//     AnonymizeBodyLimit, and drops (marking them truncated) those that are
//     still compressed afterwards (br, a stream that yields nothing), because
//     the engine can only read plain bodies and would keep an opaque one as it
//     is, raw personal data included;
//  2. the engine runs;
//  3. ex.Endpoint is recomputed from the raw and the anonymized path with
//     n.AnonymizedEndpoint (nil n: pathnorm.Default), so that no raw path
//     segment survives in the endpoint.
//
// ex is modified in place, headers included, so the caller must own them. With
// withReport the changes the engine made are returned. On error ex must not
// be stored, in any form: it may be partly anonymized.
func AnonymizeExchange(eng Anonymizer, ex *model.Exchange, n *pathnorm.Normalizer, withReport bool) ([]model.AnonChange, error) {
	if eng == nil {
		return nil, fmt.Errorf("ops: anonymize exchange: an anonymization engine is required: %w", model.ErrValidation)
	}
	if ex == nil {
		return nil, fmt.Errorf("ops: anonymize exchange: nil exchange: %w", model.ErrValidation)
	}
	if n == nil {
		n = pathnorm.Default
	}
	method, rawPath := ex.Request.Method, ex.Request.Path
	reinstateEncoding(&ex.Request.Header, ex.Request.Body, &ex.Request.ContentEncoding)
	reinstateEncoding(&ex.Response.Header, ex.Response.Body, &ex.Response.ContentEncoding)
	bodycodec.PrepareForAnonymization(ex, AnonymizeBodyLimit)

	var (
		changes []model.AnonChange
		err     error
	)
	if withReport {
		changes, err = eng.ExchangeWithReport(ex)
	} else {
		err = eng.Exchange(ex)
	}
	if err != nil {
		return nil, err
	}
	ex.Endpoint = n.AnonymizedEndpoint(method, rawPath, ex.Request.Path)
	return changes, nil
}

// reinstateEncoding undoes the bookkeeping of a HAR import for a body that
// the import could not decode. A HAR entry's Content-Encoding header moves
// into ContentEncoding on import and the body is expected to be decoded
// already; but a stream that was cut, or a coding the decoder does not know
// (br, zstd), stays as it came, compressed, with no header to say so. If the
// bytes may be that (bodycodec.MayBeCompressed, which errs on the side of
// compressed), the header is put back, so that PrepareForAnonymization decodes
// or drops the body instead of storing it as readable.
func reinstateEncoding(h *http.Header, body []byte, contentEncoding *string) {
	ce := *contentEncoding
	if ce == "" || len(body) == 0 || len(h.Values("Content-Encoding")) > 0 {
		return
	}
	if !bodycodec.MayBeCompressed(ce, body) {
		return
	}
	if *h == nil {
		*h = make(http.Header, 1)
	}
	h.Set("Content-Encoding", ce)
	*contentEncoding = ""
}

// Stats says what an anonymizing operation did besides writing the exchanges.
type Stats struct {
	// Written is the number of exchanges written.
	Written int64
	// Failed is the number of exchanges that could not be anonymized. They
	// are left out, never written in any form.
	Failed int64
	// FirstError is the first such failure (nil when Failed is 0).
	FirstError error
	// Skipped (imports) is the number of HAR entries that are not HTTP or
	// HTTPS exchanges.
	Skipped int64
	// BadTimes (imports) is the number of entries whose start time could not
	// be stored (years before 1678 or after 2261, or missing); they were
	// given the time of a neighbouring entry.
	BadTimes int64
}

// fail counts one exchange that could not be anonymized.
func (s *Stats) fail(err error) {
	s.Failed++
	if s.FirstError == nil {
		s.FirstError = err
	}
}

// allFailed reports whether anonymization failed for every exchange there was.
func (s Stats) allFailed() bool { return s.Failed > 0 && s.Written == 0 }

// allFailedError is the error of an operation in which nothing could be
// anonymized.
func (s Stats) allFailedError(what string) error {
	err := s.FirstError
	if err == nil {
		err = errors.New("anonymization failed")
	}
	return fmt.Errorf("%s: none of the %d exchanges could be anonymized, so nothing was written (first failure: %w)", what, s.Failed, err)
}

// plural writes "1 exchange" or "3 exchanges".
func plural(n int64, singular, pl string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, pl)
}

// failedNote is the sentence for recording descriptions.
func failedNote(failed int64) string {
	if failed == 0 {
		return ""
	}
	return fmt.Sprintf(" Left out %s that could not be anonymized.", plural(failed, "exchange", "exchanges"))
}

// keepStoredEndpoint decides the endpoint of a copied exchange. AnonymizedEndpoint
// computes it from the paths with the default normalizer; but the stored
// endpoint may come from a service's own patterns ("GET /orders/{ref}"),
// which the copy cannot rebuild. Such an endpoint is kept when it cannot carry
// anything the anonymizer removed: every literal segment must be one that the
// anonymized path has at the same position.
func keepStoredEndpoint(stored, computed, method, rawPath, anonPath string) string {
	if stored == "" || stored == computed || stored == pathnorm.Default.Endpoint(method, rawPath) {
		return computed
	}
	_, tmpl, ok := strings.Cut(stored, " ")
	if !ok || !strings.HasPrefix(stored, method+" ") {
		return computed
	}
	segs, anon := strings.Split(tmpl, "/"), strings.Split(anonPath, "/")
	if len(segs) != len(anon) {
		return computed
	}
	for i, s := range segs {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			continue
		}
		if s != anon[i] {
			return computed
		}
	}
	return stored
}
