package model

import (
	"encoding/json"
	"github.com/justinburger/mirage/internal/pathnorm"
	"net"
	"net/url"
	"regexp"
	"time"
)

// Mode is how a virtual service handles traffic.
type Mode string

const (
	// ModePassthrough proxies every request to the upstream. Metrics and fault
	// injection still apply; nothing is recorded.
	ModePassthrough Mode = "passthrough"
	// ModeRecord proxies to the upstream and captures every exchange into the
	// service's recording.
	ModeRecord Mode = "record"
	// ModeVirtualize answers every request from the service's recording and
	// never contacts the upstream. Unmatched requests get Match.MissStatus.
	ModeVirtualize Mode = "virtualize"
	// ModeHybrid answers from the recording when it can and proxies misses to
	// the upstream, recording them. Successful GET/HEAD/OPTIONS misses are
	// also added to the live index, exactly as stored: with anonymize_capture
	// on they then match only requests carrying the same anonymized values
	// (for example replayed traffic) or via path_templates.
	ModeHybrid Mode = "hybrid"
)

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool {
	switch m {
	case ModePassthrough, ModeRecord, ModeVirtualize, ModeHybrid:
		return true
	}
	return false
}

// UsesUpstream reports whether the mode may contact the upstream.
func (m Mode) UsesUpstream() bool { return m != ModeVirtualize }

// UsesRecording reports whether the mode needs Service.RecordingID.
func (m Mode) UsesRecording() bool { return m != ModePassthrough }

// TLSConfig configures TLS on a service listener.
type TLSConfig struct {
	// SelfSigned generates an in-memory self-signed certificate at start-up.
	SelfSigned bool   `json:"self_signed,omitempty"`
	CertFile   string `json:"cert_file,omitempty"`
	KeyFile    string `json:"key_file,omitempty"`
}

// QueryMatch controls how query strings take part in virtualization matching.
type QueryMatch string

const (
	QueryExact  QueryMatch = "exact"  // all params (minus IgnoreParams) must match, order-insensitive
	QueryIgnore QueryMatch = "ignore" // query string is ignored
	QuerySubset QueryMatch = "subset" // every recorded param must be present in the request
)

// BodyMatch controls how request bodies take part in virtualization matching.
type BodyMatch string

const (
	BodyIgnore BodyMatch = "ignore"
	BodyExact  BodyMatch = "exact" // byte-for-byte
	BodyJSON   BodyMatch = "json"  // semantic JSON equality (key order and whitespace ignored); exact for non-JSON
)

// Selection picks one response when several recorded exchanges match.
type Selection string

const (
	SelectSequential Selection = "sequential" // cycle through candidates in recorded order (per match key)
	SelectRandom     Selection = "random"
	SelectFirst      Selection = "first"
)

// MatchConfig controls how the virtualizer maps an incoming request to a
// recorded exchange. Matching falls back through three levels and uses the
// first level that has candidates:
//
//  1. "exact": method + path + query (per Query) + body (per Body) + Headers
//  2. "path": method + path
//  3. "template": method + endpoint template (only when PathTemplates is set),
//     e.g. GET /users/7 matches a recording of GET /users/42.
type MatchConfig struct {
	Query         QueryMatch `json:"query"`
	IgnoreParams  []string   `json:"ignore_params,omitempty"`
	Body          BodyMatch  `json:"body"`
	Headers       []string   `json:"headers,omitempty"`
	PathTemplates bool       `json:"path_templates"`
	Selection     Selection  `json:"selection"`
	// MissStatus is the status returned for unmatched requests in virtualize
	// mode. Default 404. The body is a small JSON document explaining the miss.
	MissStatus int `json:"miss_status"`
}

// LatencyMode controls simulated latency when serving virtualized responses.
type LatencyMode string

const (
	LatencyNone     LatencyMode = "none"     // respond immediately
	LatencyRecorded LatencyMode = "recorded" // replay the recorded upstream latency (× Multiplier)
	LatencyFixed    LatencyMode = "fixed"    // always wait Fixed
)

// LatencyConfig controls simulated latency in virtualize and hybrid modes.
type LatencyConfig struct {
	Mode LatencyMode `json:"mode"`
	// Multiplier scales recorded latency (2 = twice as slow). 0 means 1.
	Multiplier float64 `json:"multiplier"`
	// Fixed is the delay for LatencyFixed.
	Fixed Duration `json:"fixed"`
	// Jitter adds a uniform random offset in [-Jitter, +Jitter] (never below 0).
	Jitter Duration `json:"jitter"`
}

// Service is one virtual service: a listener that sits in front of an upstream
// API.
type Service struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ListenAddr string `json:"listen_addr"` // e.g. ":9001" or "127.0.0.1:9001"
	Upstream   string `json:"upstream"`    // base URL, e.g. "https://api.example.com"
	Mode       Mode   `json:"mode"`
	Enabled    bool   `json:"enabled"`

	// RecordingID is the recording written in record/hybrid mode and served
	// in virtualize/hybrid mode.
	RecordingID string `json:"recording_id,omitempty"`

	// AnonymizeCapture anonymizes exchanges before they are written to disk,
	// using ProfileID ("" = the built-in "default" profile). Raw values then
	// never touch the database.
	AnonymizeCapture bool   `json:"anonymize_capture"`
	ProfileID        string `json:"profile_id,omitempty"`

	// EndpointPatterns are optional path templates ("/users/{id}") used to
	// group requests into endpoints for metrics and template matching. They
	// take precedence over automatic detection of ID-like path segments.
	EndpointPatterns []string `json:"endpoint_patterns,omitempty"`

	PreserveHost       bool       `json:"preserve_host"`
	InsecureSkipVerify bool       `json:"insecure_skip_verify"`
	TLS                *TLSConfig `json:"tls,omitempty"`
	// Timeout bounds the wait for the upstream's response headers
	// (ResponseHeaderTimeout); bodies then stream until EOF or client
	// disconnect. 0 = 60s.
	Timeout Duration `json:"timeout"`
	// MaxBodyBytes caps how much of each request/response body is captured.
	// 0 = 1 MiB; -1 = do not capture bodies. Traffic is never truncated, only
	// the captured copy.
	MaxBodyBytes int64 `json:"max_body_bytes"`
	// CaptureSample is the fraction (0..1] of requests captured in
	// record/hybrid mode; 0 means 1 (everything). Decided before any
	// buffering, so unsampled requests cost nothing extra.
	CaptureSample float64 `json:"capture_sample,omitempty"`
	// MaxCaptureBytes stops capturing (status error "capture limit
	// reached") once the recording holds this many body bytes. 0 = no limit.
	MaxCaptureBytes int64 `json:"max_capture_bytes,omitempty"`

	Match   MatchConfig   `json:"match"`
	Latency LatencyConfig `json:"latency"`

	// ErrorAmplification multiplies the recorded per-endpoint error rate when
	// serving virtualized responses (virtualize/hybrid). 1 (or 0) = as
	// recorded, 10 = ten times as many errors (capped at 100%), 0.5 = half.
	// Injected errors reuse real recorded error responses for the endpoint
	// when there are any, otherwise a synthetic 503.
	ErrorAmplification float64 `json:"error_amplification"`
	// ErrorFloor is a minimum error rate (0..1) applied after amplification,
	// so endpoints that never failed during recording can still fail.
	ErrorFloor float64 `json:"error_floor"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Defaults for zero-valued service fields.
const (
	DefaultMaxBodyBytes   int64 = 1 << 20
	DefaultServiceTimeout       = 60 * time.Second
	DefaultMissStatus           = 404
)

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// WithDefaults returns a copy of s with zero-valued fields set to defaults.
func (s Service) WithDefaults() Service {
	if s.Mode == "" {
		s.Mode = ModePassthrough
	}
	if s.Timeout == 0 {
		s.Timeout = Duration(DefaultServiceTimeout)
	}
	if s.MaxBodyBytes == 0 {
		s.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if s.Match.Query == "" {
		s.Match.Query = QueryExact
	}
	if s.Match.Body == "" {
		s.Match.Body = BodyJSON
	}
	if s.Match.Selection == "" {
		s.Match.Selection = SelectSequential
	}
	if s.Match.MissStatus == 0 {
		s.Match.MissStatus = DefaultMissStatus
	}
	if s.Latency.Mode == "" {
		s.Latency.Mode = LatencyRecorded
	}
	if s.Latency.Multiplier == 0 {
		s.Latency.Multiplier = 1
	}
	if s.ErrorAmplification == 0 {
		s.ErrorAmplification = 1
	}
	return s
}

// Validate checks a service (after WithDefaults). It does not check that the
// recording or profile exist; the API does that.
func (s Service) Validate() error {
	var p problems
	if !nameRE.MatchString(s.Name) {
		p.add("name must be 1-64 chars of letters, digits, '.', '_' or '-'")
	}
	if _, port, err := net.SplitHostPort(s.ListenAddr); err != nil {
		p.add("listen_addr %q must be host:port or :port", s.ListenAddr)
	} else if !validPort(port) {
		p.add("listen_addr %q has an invalid port %q (use a number from 0 to 65535)", s.ListenAddr, port)
	}
	if !s.Mode.Valid() {
		p.add("mode %q must be passthrough, record, virtualize or hybrid", s.Mode)
	}
	if s.Mode.UsesUpstream() || s.Upstream != "" {
		u, err := url.Parse(s.Upstream)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			p.add("upstream %q must be an absolute http(s) URL", s.Upstream)
		}
	}
	if s.Mode.UsesRecording() && s.RecordingID == "" {
		p.add("mode %s needs a recording_id", s.Mode)
	}
	if s.TLS != nil && !s.TLS.SelfSigned && (s.TLS.CertFile == "" || s.TLS.KeyFile == "") {
		p.add("tls needs self_signed or both cert_file and key_file")
	}
	if s.MaxBodyBytes < -1 {
		p.add("max_body_bytes must be -1, 0 or positive")
	}
	if s.Mode == ModeHybrid && s.MaxBodyBytes == -1 {
		p.add("hybrid mode needs captured bodies (max_body_bytes must not be -1)")
	}
	if s.CaptureSample < 0 || s.CaptureSample > 1 {
		p.add("capture_sample must be between 0 and 1")
	}
	if s.MaxCaptureBytes < 0 {
		p.add("max_capture_bytes must not be negative")
	}
	if s.Timeout < 0 {
		p.add("timeout must not be negative")
	}
	switch s.Match.Query {
	case QueryExact, QueryIgnore, QuerySubset:
	default:
		p.add("match.query %q must be exact, ignore or subset", s.Match.Query)
	}
	switch s.Match.Body {
	case BodyIgnore, BodyExact, BodyJSON:
	default:
		p.add("match.body %q must be ignore, exact or json", s.Match.Body)
	}
	switch s.Match.Selection {
	case SelectSequential, SelectRandom, SelectFirst:
	default:
		p.add("match.selection %q must be sequential, random or first", s.Match.Selection)
	}
	if s.Match.MissStatus < 100 || s.Match.MissStatus > 599 {
		p.add("match.miss_status must be a valid HTTP status")
	}
	switch s.Latency.Mode {
	case LatencyNone, LatencyRecorded, LatencyFixed:
	default:
		p.add("latency.mode %q must be none, recorded or fixed", s.Latency.Mode)
	}
	if s.Latency.Multiplier < 0 || s.Latency.Fixed < 0 || s.Latency.Jitter < 0 {
		p.add("latency values must not be negative")
	}
	if s.ErrorAmplification < 0 || s.ErrorAmplification > 1000 {
		p.add("error_amplification must be between 0 and 1000")
	}
	if s.ErrorFloor < 0 || s.ErrorFloor > 1 {
		p.add("error_floor must be between 0 and 1")
	}
	for _, pat := range s.EndpointPatterns {
		if err := pathnorm.ValidatePattern(pat); err != nil {
			p.add("endpoint pattern %q: %v", pat, err)
		}
	}
	if !finite(s.CaptureSample, s.ErrorAmplification, s.ErrorFloor, s.Latency.Multiplier) {
		p.add("capture_sample, error_amplification, error_floor and latency.multiplier must be numbers")
	}
	return p.err()
}

// validPort reports whether s is a decimal port number from 0 to 65535. Port
// names ("http") are not accepted: resolving them depends on the machine's
// services database, and a listen address should mean the same everywhere.
func validPort(s string) bool {
	if s == "" || len(s) > 5 {
		return false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n <= 65535
}

// ServiceStatus is the live runtime state of a service listener.
type ServiceStatus struct {
	ServiceID string `json:"service_id"`
	Running   bool   `json:"running"`
	Addr      string `json:"addr,omitempty"` // resolved listen address
	Mode      Mode   `json:"mode"`
	Error     string `json:"error,omitempty"` // last start/runtime error
	// Warning is a note about something that leaves the service working:
	// "capture limit reached" (traffic keeps flowing, only capturing stopped;
	// cleared when the service is applied again) and/or a note about truncated
	// recordings naming max_body_bytes (derived from CaptureTruncated and
	// VirtualTruncated; shown while they are non-zero). Error stays for
	// failures.
	Warning   string     `json:"warning,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	// Captured counts exchanges written to the recording since start.
	Captured int64 `json:"captured"`
	// CaptureDropped counts exchanges dropped because the capture queue was
	// full (by count or bytes); the request itself was still served.
	CaptureDropped int64 `json:"capture_dropped"`
	// CaptureSkipped counts exchanges not captured because the client went
	// away before the upstream response finished, or sampling skipped them.
	CaptureSkipped int64 `json:"capture_skipped"`
	// CaptureTruncated counts stored exchanges whose response body was larger
	// than max_body_bytes and was cut (not counted with max_body_bytes -1;
	// reset when the cap is raised or the recording changes). Virtualize
	// answers such recordings with a 502 (see VirtualTruncated).
	CaptureTruncated int64 `json:"capture_truncated"`
	// VirtualHits/VirtualMisses count virtualization lookups since start.
	VirtualHits   int64 `json:"virtual_hits"`
	VirtualMisses int64 `json:"virtual_misses"`
	// VirtualInjected counts responses swapped by error amplification.
	VirtualInjected int64 `json:"virtual_injected"`
	// VirtualTruncated counts requests answered 502 because every matching
	// recording held only part of its response body (raise max_body_bytes and
	// record them again).
	VirtualTruncated int64 `json:"virtual_truncated"`
	// IndexedExchanges is the size of the virtualization index (0 when not
	// virtualizing).
	IndexedExchanges int64 `json:"indexed_exchanges"`
}

// MarshalJSON writes started_at in UTC, like every other timestamp of the API,
// whatever zone the clock that produced it was in. The receiver is not
// modified.
func (s ServiceStatus) MarshalJSON() ([]byte, error) {
	type plain ServiceStatus // no methods: avoids recursing into MarshalJSON
	if s.StartedAt != nil {
		t := s.StartedAt.UTC()
		s.StartedAt = &t
	}
	return json.Marshal(plain(s))
}

// VirtualStats counts virtualization activity of one service since its
// virtual index was built.
type VirtualStats struct {
	Indexed  int64 `json:"indexed"`
	Hits     int64 `json:"hits"`
	Misses   int64 `json:"misses"`
	Injected int64 `json:"injected"`
	// Truncated counts requests answered 502 because every matching
	// recording held only part of its response body (see docs/API.md).
	Truncated int64            `json:"truncated"`
	ByLevel   map[string]int64 `json:"by_level"`
}
