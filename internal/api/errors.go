package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/justinburger/mirage/internal/model"
)

// errorBody is the JSON form of every error response.
type errorBody struct {
	Error    string   `json:"error"`
	Problems []string `json:"problems,omitempty"`
}

// httpError is an error that carries its HTTP status.
type httpError struct {
	status   int
	msg      string
	problems []string
}

func (e *httpError) Error() string { return e.msg }

// statusClientClosed is the (nginx) status logged for requests the client
// abandoned. Nobody is left to read it.
const statusClientClosed = 499

func newHTTPError(status int, format string, args ...any) error {
	return &httpError{status: status, msg: fmt.Sprintf(format, args...)}
}

func badRequest(format string, args ...any) error {
	return newHTTPError(http.StatusBadRequest, format, args...)
}

// invalid returns a validation error listing problems (HTTP 400).
func invalid(problems ...string) error {
	return &model.ValidationError{Problems: problems}
}

// validationProblems extracts the problems of a validation error; any other
// error yields its message as the single problem.
func validationProblems(err error) []string {
	if err == nil {
		return nil
	}
	var ve *model.ValidationError
	if errors.As(err, &ve) {
		return ve.Problems
	}
	return []string{err.Error()}
}

// internalPrefixes are the package prefixes of wrapped errors. They help in
// logs and mean nothing to API clients.
var internalPrefixes = regexp.MustCompile(`\b(?:store|ops|replay|proxy|virtual|api|har|anonymize|fault): `)

// publicMessage turns an error chain into a message fit for API clients.
func publicMessage(err error) string { return cleanMessage(err.Error()) }

// cleanMessage is publicMessage for text that is already a string, such as the
// error a replay job recorded.
func cleanMessage(msg string) string {
	msg = collapseRepeats(internalPrefixes.ReplaceAllString(msg, ""))
	for _, suffix := range []string{": conflict", ": validation failed"} {
		msg = strings.TrimSuffix(msg, suffix)
	}
	return msg
}

// collapseRepeats drops a context segment of a wrapped error message that only
// restates the one before it. Each layer names the object it works on, so a
// chain reads 'recording "x": recording "x": not found' or 'failed recording
// "x": recording "x": not found'; the second mention adds nothing.
func collapseRepeats(msg string) string {
	parts := strings.Split(msg, ": ")
	if len(parts) < 2 {
		return msg
	}
	out := parts[:1]
	for _, p := range parts[1:] {
		prev := out[len(out)-1]
		if p == prev || strings.HasSuffix(prev, " "+p) {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, ": ")
}

// refMissing is the error for a request that names an object that does not
// exist (a recording, profile, service or exchange id in its body or query):
// 400 with a problem, whichever endpoint it is. A missing object that is the
// target of the URL itself is a 404 instead.
func refMissing(field, id string) error {
	return invalid(field + " " + quote(id) + " does not exist")
}

// classify maps an error to the status and body of its response.
func classify(err error) (int, errorBody) {
	var (
		he  *httpError
		ve  *model.ValidationError
		mbe *http.MaxBytesError
	)
	switch {
	case errors.As(err, &he):
		return he.status, errorBody{Error: he.msg, Problems: he.problems}
	case errors.As(err, &ve):
		return http.StatusBadRequest, errorBody{Error: "validation failed", Problems: ve.Problems}
	case errors.Is(err, model.ErrValidation):
		return http.StatusBadRequest, errorBody{Error: publicMessage(err)}
	case errors.Is(err, model.ErrNotFound):
		return http.StatusNotFound, errorBody{Error: publicMessage(err)}
	case errors.Is(err, model.ErrConflict):
		return http.StatusConflict, errorBody{Error: publicMessage(err)}
	case errors.As(err, &mbe):
		return http.StatusRequestEntityTooLarge, errorBody{Error: fmt.Sprintf("request body is larger than %s", byteSize(mbe.Limit))}
	case errors.Is(err, errUnavailable):
		return http.StatusServiceUnavailable, errorBody{Error: errUnavailable.Error()}
	case errors.Is(err, context.Canceled):
		return statusClientClosed, errorBody{Error: "request cancelled"}
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, errorBody{Error: "operation timed out"}
	}
	return http.StatusInternalServerError, errorBody{Error: "internal server error"}
}

func byteSize(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GiB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// fail writes the response for err. Unexpected errors are logged with their
// detail and answered generically. If the response is already under way the
// only honest signal left is to abort the connection.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, body := classify(err)
	switch status {
	case http.StatusInternalServerError, http.StatusGatewayTimeout:
		s.log.Error("api: request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	case statusClientClosed:
		s.log.Debug("api: client went away", "method", r.Method, "path", r.URL.Path)
	}
	if started(w) {
		panic(http.ErrAbortHandler)
	}
	writeJSON(w, status, body)
}

// started reports whether the response header has been sent.
func started(w http.ResponseWriter) bool {
	if rec, ok := w.(interface{ Started() bool }); ok {
		return rec.Started()
	}
	return false
}
