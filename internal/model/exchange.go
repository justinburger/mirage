package model

import (
	"encoding/base64"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Request is a captured HTTP request.
type Request struct {
	Method   string      `json:"method"`
	Scheme   string      `json:"scheme,omitempty"` // scheme the client used to reach Mirage
	Host     string      `json:"host,omitempty"`   // Host header as received
	Path     string      `json:"path"`             // always the escaped path (r.URL.EscapedPath()), e.g. "/users/42"
	RawQuery string      `json:"raw_query,omitempty"`
	Proto    string      `json:"proto,omitempty"`
	Header   http.Header `json:"header,omitempty"`
	// Body is the captured body, decoded (gzip/deflate removed) when possible.
	Body []byte `json:"body,omitempty"`
	// BodySize is the full size of the body as seen on the wire (bytes),
	// even if Body was truncated.
	BodySize      int64 `json:"body_size"`
	BodyTruncated bool  `json:"body_truncated,omitempty"`
	// ContentEncoding is informational: non-empty means Body was already
	// decoded from this encoding and Header no longer carries
	// Content-Encoding. A body is still encoded only if Header still has a
	// Content-Encoding other than identity.
	ContentEncoding string `json:"content_encoding,omitempty"`
}

// URI returns path plus query ("/users/42?x=1").
func (r *Request) URI() string {
	if r.RawQuery == "" {
		return r.Path
	}
	return r.Path + "?" + r.RawQuery
}

// Response is a captured HTTP response.
type Response struct {
	Status          int         `json:"status"`
	Proto           string      `json:"proto,omitempty"`
	Header          http.Header `json:"header,omitempty"`
	Body            []byte      `json:"body,omitempty"`
	BodySize        int64       `json:"body_size"`
	BodyTruncated   bool        `json:"body_truncated,omitempty"`
	ContentEncoding string      `json:"content_encoding,omitempty"`
}

// Exchange is one recorded request/response pair.
type Exchange struct {
	ID          int64  `json:"id"` // assigned by the store, unique across recordings
	RecordingID string `json:"recording_id"`
	// Seq is the 1-based order in which the store persisted the exchange
	// (roughly completion order). It is NOT chronological: use StartedAt for
	// time order.
	Seq       int64  `json:"seq"`
	ServiceID string `json:"service_id,omitempty"`
	// StartedAt is when the request arrived at Mirage. Replay uses the gaps
	// between StartedAt values to reproduce the original traffic shape.
	StartedAt time.Time `json:"started_at"`
	// TTFB and Duration are measured from the moment the request is handed
	// to the upstream transport (after any injected fault delay) until the
	// response headers arrived / the last upstream body byte was read.
	Duration Duration `json:"duration"`
	TTFB     Duration `json:"ttfb"`
	// Endpoint is the normalised endpoint, e.g. "GET /users/{id}". It is
	// always computed from the raw path before anonymization and is never
	// recomputed from an anonymized path.
	Endpoint string   `json:"endpoint"`
	Request  Request  `json:"request"`
	Response Response `json:"response"`
	// Error is set only for upstream failures (no usable upstream response);
	// Response.Status is then the status Mirage synthesised (502/504).
	Error      string `json:"error,omitempty"`
	Anonymized bool   `json:"anonymized"`
}

// IsErrorStatus reports whether an HTTP status counts as an error for error
// rates and error amplification: 5xx and 429 (rate limited). 0 means no
// response at all and is also an error.
func IsErrorStatus(status int) bool {
	return status == 0 || status >= 500 || status == http.StatusTooManyRequests
}

// IsError reports whether the exchange counts as an error (see IsErrorStatus).
func (e *Exchange) IsError() bool { return e.Error != "" || IsErrorStatus(e.Response.Status) }

// Summary returns the list-view form of the exchange.
func (e *Exchange) Summary() ExchangeSummary {
	return ExchangeSummary{
		ID:          e.ID,
		Seq:         e.Seq,
		StartedAt:   e.StartedAt,
		Duration:    e.Duration,
		Method:      e.Request.Method,
		Path:        e.Request.Path,
		RawQuery:    e.Request.RawQuery,
		Status:      e.Response.Status,
		Endpoint:    e.Endpoint,
		ReqBytes:    e.Request.BodySize,
		RespBytes:   e.Response.BodySize,
		ContentType: e.Response.Header.Get("Content-Type"),
		Error:       e.Error,
	}
}

// ExchangeSummary is the compact form used in exchange lists.
type ExchangeSummary struct {
	ID          int64     `json:"id"`
	Seq         int64     `json:"seq"`
	StartedAt   time.Time `json:"started_at"`
	Duration    Duration  `json:"duration"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	RawQuery    string    `json:"raw_query,omitempty"`
	Status      int       `json:"status"`
	Endpoint    string    `json:"endpoint"`
	ReqBytes    int64     `json:"req_bytes"`
	RespBytes   int64     `json:"resp_bytes"`
	ContentType string    `json:"content_type,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// ExchangeQuery filters and pages an exchange list. Zero values mean "any".
type ExchangeQuery struct {
	Method       string `json:"method,omitempty"`
	Status       int    `json:"status,omitempty"`       // exact status
	StatusClass  int    `json:"status_class,omitempty"` // 2 = 2xx … 5 = 5xx
	PathContains string `json:"path,omitempty"`         // substring of path+query
	Endpoint     string `json:"endpoint,omitempty"`     // exact endpoint
	ErrorsOnly   bool   `json:"errors,omitempty"`       // IsError() exchanges only
	Offset       int    `json:"offset,omitempty"`
	Limit        int    `json:"limit,omitempty"` // 0 = 100, max 1000
	Desc         bool   `json:"desc,omitempty"`  // newest first
}

// IterOptions controls what IterateExchanges loads. Bodies are the expensive
// part, so callers ask only for what they need.
type IterOptions struct {
	RequestBodies  bool
	ResponseBodies bool
}

// Recording is a named collection of exchanges.
type Recording struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	ServiceID   string            `json:"service_id,omitempty"` // service that captured it, if any
	Source      string            `json:"source"`               // "proxy", "har", "copy"
	Anonymized  bool              `json:"anonymized"`           // every exchange is anonymized
	Labels      map[string]string `json:"labels,omitempty"`
	// Counters maintained by the store as exchanges are appended.
	ExchangeCount int64 `json:"exchange_count"`
	ErrorCount    int64 `json:"error_count"`
	Bytes         int64 `json:"bytes"` // captured body bytes (request + response)
	// FirstAt/LastAt are the StartedAt of the first and last exchange.
	FirstAt   *time.Time `json:"first_at,omitempty"`
	LastAt    *time.Time `json:"last_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// EndpointStat summarises one endpoint within a recording.
type EndpointStat struct {
	Endpoint  string   `json:"endpoint"`
	Count     int64    `json:"count"`
	Errors    int64    `json:"errors"`
	ErrorRate float64  `json:"error_rate"` // Errors / Count
	Avg       Duration `json:"avg"`
	P50       Duration `json:"p50"`
	P95       Duration `json:"p95"`
	P99       Duration `json:"p99"`
	Max       Duration `json:"max"`
	// StatusCounts maps status code (as a string) to count.
	StatusCounts map[string]int64 `json:"status_counts"`
}

// BodyView is a body rendered for JSON transport: UTF-8 text when possible,
// otherwise base64.
type BodyView struct {
	Encoding    string `json:"encoding"`       // "text", "base64" or "none"
	Data        string `json:"data,omitempty"` // text or base64 payload
	Size        int64  `json:"size"`
	Truncated   bool   `json:"truncated,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

// NewBodyView renders a body for transport.
func NewBodyView(body []byte, size int64, truncated bool, contentType string) BodyView {
	v := BodyView{Size: size, Truncated: truncated, ContentType: contentType}
	switch {
	case len(body) == 0:
		v.Encoding = "none"
	case utf8.Valid(body) && isTextual(contentType, body):
		v.Encoding, v.Data = "text", string(body)
	default:
		v.Encoding, v.Data = "base64", base64.StdEncoding.EncodeToString(body)
	}
	return v
}

// Bytes decodes the view back to raw bytes.
func (v BodyView) Bytes() ([]byte, error) {
	switch v.Encoding {
	case "base64":
		return base64.StdEncoding.DecodeString(v.Data)
	case "none", "":
		if v.Data == "" {
			return nil, nil
		}
		return []byte(v.Data), nil
	default:
		return []byte(v.Data), nil
	}
}

func isTextual(contentType string, body []byte) bool {
	mt, _, _ := mime.ParseMediaType(contentType)
	switch {
	case mt == "":
		// Unknown type: treat as text if it has no NUL bytes.
		return !strings.ContainsRune(string(body[:min(len(body), 512)]), 0)
	case strings.HasPrefix(mt, "text/"),
		strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "+xml"),
		mt == "application/json", mt == "application/xml", mt == "application/javascript",
		mt == "application/x-www-form-urlencoded", mt == "application/graphql",
		mt == "application/x-ndjson":
		return true
	}
	return false
}

// RequestView / ResponseView / ExchangeView are the API forms of the captured
// types, with bodies rendered as BodyView.
type RequestView struct {
	Method          string      `json:"method"`
	Scheme          string      `json:"scheme,omitempty"`
	Host            string      `json:"host,omitempty"`
	Path            string      `json:"path"`
	RawQuery        string      `json:"raw_query,omitempty"`
	Proto           string      `json:"proto,omitempty"`
	Header          http.Header `json:"header"`
	Body            BodyView    `json:"body"`
	ContentEncoding string      `json:"content_encoding,omitempty"`
}

type ResponseView struct {
	Status          int         `json:"status"`
	Proto           string      `json:"proto,omitempty"`
	Header          http.Header `json:"header"`
	Body            BodyView    `json:"body"`
	ContentEncoding string      `json:"content_encoding,omitempty"`
}

type ExchangeView struct {
	ID          int64        `json:"id"`
	RecordingID string       `json:"recording_id"`
	Seq         int64        `json:"seq"`
	ServiceID   string       `json:"service_id,omitempty"`
	StartedAt   time.Time    `json:"started_at"`
	Duration    Duration     `json:"duration"`
	TTFB        Duration     `json:"ttfb"`
	Endpoint    string       `json:"endpoint"`
	Request     RequestView  `json:"request"`
	Response    ResponseView `json:"response"`
	Error       string       `json:"error,omitempty"`
	Anonymized  bool         `json:"anonymized"`
}

// View converts an exchange to its API form.
func (e *Exchange) View() ExchangeView {
	h := func(x http.Header) http.Header {
		if x == nil {
			return http.Header{}
		}
		return x
	}
	return ExchangeView{
		ID: e.ID, RecordingID: e.RecordingID, Seq: e.Seq, ServiceID: e.ServiceID,
		StartedAt: e.StartedAt, Duration: e.Duration, TTFB: e.TTFB, Endpoint: e.Endpoint,
		Error: e.Error, Anonymized: e.Anonymized,
		Request: RequestView{
			Method: e.Request.Method, Scheme: e.Request.Scheme, Host: e.Request.Host,
			Path: e.Request.Path, RawQuery: e.Request.RawQuery, Proto: e.Request.Proto,
			Header:          h(e.Request.Header),
			Body:            NewBodyView(e.Request.Body, e.Request.BodySize, e.Request.BodyTruncated, e.Request.Header.Get("Content-Type")),
			ContentEncoding: e.Request.ContentEncoding,
		},
		Response: ResponseView{
			Status: e.Response.Status, Proto: e.Response.Proto,
			Header:          h(e.Response.Header),
			Body:            NewBodyView(e.Response.Body, e.Response.BodySize, e.Response.BodyTruncated, e.Response.Header.Get("Content-Type")),
			ContentEncoding: e.Response.ContentEncoding,
		},
	}
}

// Exchange converts an API view back to an exchange (used by anonymization
// previews of edited samples).
func (v ExchangeView) Exchange() (*Exchange, error) {
	reqBody, err := v.Request.Body.Bytes()
	if err != nil {
		return nil, err
	}
	respBody, err := v.Response.Body.Bytes()
	if err != nil {
		return nil, err
	}
	reqSize, respSize := v.Request.Body.Size, v.Response.Body.Size
	if reqSize < int64(len(reqBody)) {
		reqSize = int64(len(reqBody))
	}
	if respSize < int64(len(respBody)) {
		respSize = int64(len(respBody))
	}
	return &Exchange{
		ID: v.ID, RecordingID: v.RecordingID, Seq: v.Seq, ServiceID: v.ServiceID,
		StartedAt: v.StartedAt, Duration: v.Duration, TTFB: v.TTFB, Endpoint: v.Endpoint,
		Error: v.Error, Anonymized: v.Anonymized,
		Request: Request{
			Method: v.Request.Method, Scheme: v.Request.Scheme, Host: v.Request.Host,
			Path: v.Request.Path, RawQuery: v.Request.RawQuery, Proto: v.Request.Proto,
			Header: v.Request.Header.Clone(), Body: reqBody, BodySize: reqSize,
			BodyTruncated: v.Request.Body.Truncated, ContentEncoding: v.Request.ContentEncoding,
		},
		Response: Response{
			Status: v.Response.Status, Proto: v.Response.Proto,
			Header: v.Response.Header.Clone(), Body: respBody, BodySize: respSize,
			BodyTruncated: v.Response.Body.Truncated, ContentEncoding: v.Response.ContentEncoding,
		},
	}, nil
}

// Clone returns a deep copy of the exchange.
func (e *Exchange) Clone() *Exchange {
	c := *e
	c.Request.Header = e.Request.Header.Clone()
	c.Response.Header = e.Response.Header.Clone()
	c.Request.Body = append([]byte(nil), e.Request.Body...)
	c.Response.Body = append([]byte(nil), e.Response.Body...)
	return &c
}
