package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const (
	// maxJSONBody is the largest JSON request body accepted.
	maxJSONBody = 10 << 20
	// maxImportBody is the largest HAR upload accepted. It is streamed.
	maxImportBody = 1 << 30
)

// echoFields are the read-only members every API object may carry. Clients
// may send them back unchanged (the UI PUTs what it fetched); the server
// ignores them. Some endpoints add their own (a service's "status", a fault
// rule's "fired"): they cannot be global, because "status" is a real member of
// fault rules.
var echoFields = []string{"has_secret", "created_at", "updated_at"}

// Extra echo fields of particular endpoints.
const (
	echoServiceStatus = "status"
	echoFaultFired    = "fired"
)

// idPattern is what client-supplied object IDs must look like. IDs end up in
// URL paths, so anything that needs escaping is refused.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func checkID(id string) error {
	if !idPattern.MatchString(id) {
		return invalid("id must be 1-64 characters of letters, digits, '.', '_' or '-'")
	}
	return nil
}

// writeJSON writes v as the response body. Marshalling happens first, so an
// encoding failure still produces a clean 500.
func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		buf.Reset()
		status = http.StatusInternalServerError
		_ = enc.Encode(errorBody{Error: "internal server error"})
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func writeNoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// decodeJSON reads the request body into dst, which must be a pointer to a
// struct. Unknown members are rejected, apart from the echo fields (the common
// ones plus any extra names given), which are dropped before decoding, so they
// never reach dst and a client cannot smuggle timestamps into the store. With
// optional set, an empty body is accepted and leaves dst untouched.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, optional bool, extraEcho ...string) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return decodeJSONBytes(body, dst, optional, extraEcho...)
}

func decodeJSONBytes(body []byte, dst any, optional bool, extraEcho ...string) error {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		if optional {
			return nil
		}
		return badRequest("request body is empty; send a JSON object")
	}
	if body[0] != '{' {
		return badRequest("request body must be a JSON object")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return jsonProblem(err)
	}
	stripped := false
	for key := range top {
		if isEcho(key, extraEcho) {
			delete(top, key)
			stripped = true
		}
	}
	if stripped {
		var err error
		if body, err = json.Marshal(top); err != nil {
			return err
		}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return jsonProblem(err)
	}
	return nil
}

func isEcho(key string, extra []string) bool {
	for _, set := range [][]string{echoFields, extra} {
		for _, echo := range set {
			if strings.EqualFold(key, echo) {
				return true
			}
		}
	}
	return false
}

// jsonProblem turns an encoding/json error into a 400 with a readable message.
func jsonProblem(err error) error {
	var (
		syn *json.SyntaxError
		typ *json.UnmarshalTypeError
	)
	switch {
	case errors.As(err, &syn):
		return badRequest("malformed JSON at byte %d: %v", syn.Offset, err)
	case errors.As(err, &typ):
		field := typ.Field
		if field == "" {
			return badRequest("invalid JSON: expected %s, got %s", typ.Type, typ.Value)
		}
		return badRequest("invalid value for %q: expected %s, got %s", field, typ.Type, typ.Value)
	case errors.Is(err, io.ErrUnexpectedEOF):
		return badRequest("malformed JSON: unexpected end of input")
	}
	if rest, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return badRequest("unknown field %s", rest)
	}
	return badRequest("invalid JSON: %v", err)
}

// queryInt parses an optional integer query parameter.
func queryInt(r *http.Request, name string, def int64) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, badRequest("query parameter %s must be an integer, got %q", name, raw)
	}
	return n, nil
}

// queryFlag parses an optional boolean query parameter ("1" and "true").
func queryFlag(r *http.Request, name string) bool {
	switch strings.ToLower(r.URL.Query().Get(name)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// nonNil returns s, or an empty slice when s is nil, so lists marshal as [].
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
