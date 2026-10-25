package fault

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/justinburger/mirage/internal/model"
)

// FaultHeader is the response header carrying the name of the error rule that
// produced an injected error response.
const FaultHeader = "X-Mirage-Fault"

// Content types used for injected error bodies when the rule does not set its
// own Content-Type response header.
const (
	contentTypeJSON = "application/json"
	contentTypeText = "text/plain; charset=utf-8"
)

type headerKV struct{ key, val string }

// errorResponse is a pre-rendered error fault response. Rendering once at
// compile time keeps the error path free of JSON validation and header
// canonicalisation work.
type errorResponse struct {
	rule        *model.FaultRule // the rule this was rendered from
	status      int
	headers     []headerKV // rule response headers, canonical keys, sorted
	contentType string     // default content type; "" if the rule sets one or no body is allowed
	length      string     // Content-Length value
	body        []byte
	hasBody     bool // the status allows a message body
	name        string
}

// newErrorResponse renders an error rule. It tolerates rules that were not
// defaulted or validated (a hand-built Plan) by falling back to the same
// defaults as model.FaultRule.WithDefaults.
func newErrorResponse(r *model.FaultRule) *errorResponse {
	status := r.Status
	if status == 0 {
		status = http.StatusServiceUnavailable
	}
	if status < 200 || status > 599 {
		status = http.StatusServiceUnavailable
	}
	e := &errorResponse{
		rule:    r,
		status:  status,
		body:    []byte(r.Body),
		hasBody: bodyAllowed(status),
		name:    sanitizeHeaderValue(r.Name),
	}
	ownType := false
	for k, v := range r.ResponseHeader {
		key := http.CanonicalHeaderKey(k)
		if key == "Content-Type" {
			ownType = true
		}
		e.headers = append(e.headers, headerKV{key: key, val: sanitizeHeaderValue(v)})
	}
	sort.Slice(e.headers, func(a, b int) bool { return e.headers[a].key < e.headers[b].key })

	if e.hasBody {
		e.length = strconv.Itoa(len(e.body))
		if !ownType {
			if looksLikeJSON(e.body) {
				e.contentType = contentTypeJSON
			} else {
				e.contentType = contentTypeText
			}
		}
	}
	return e
}

// write sends the response. Rule response headers are applied first, then the
// computed Content-Type / Content-Length, then X-Mirage-Fault, so the
// computed headers cannot be overridden by accident.
func (e *errorResponse) write(w http.ResponseWriter) {
	h := w.Header()
	for i := range e.headers {
		h.Set(e.headers[i].key, e.headers[i].val)
	}
	if e.hasBody {
		if e.contentType != "" {
			h.Set("Content-Type", e.contentType)
		}
		h.Set("Content-Length", e.length)
	}
	h.Set(FaultHeader, e.name)
	w.WriteHeader(e.status)
	if e.hasBody && len(e.body) > 0 {
		_, _ = w.Write(e.body)
	}
}

// bodyAllowed mirrors net/http's rule for which statuses may carry a body.
func bodyAllowed(status int) bool {
	switch {
	case status >= 100 && status <= 199:
		return false
	case status == http.StatusNoContent, status == http.StatusNotModified:
		return false
	}
	return true
}

// looksLikeJSON reports whether body is a JSON object or array. Bare scalars
// ("123", "true") are far more likely to be plain text in a fault rule.
func looksLikeJSON(body []byte) bool {
	for _, c := range body {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		case '{', '[':
			return json.Valid(body)
		}
		return false
	}
	return false
}

// sanitizeHeaderValue removes characters that cannot appear in a header value.
func sanitizeHeaderValue(s string) string {
	if !strings.ContainsAny(s, "\r\n\x00") {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 {
			return ' '
		}
		return r
	}, s)
}
