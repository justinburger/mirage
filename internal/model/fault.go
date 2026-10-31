package model

import (
	"regexp"
	"strings"
	"time"
)

// FaultType is the kind of fault a rule injects.
type FaultType string

const (
	// FaultLatency delays the request by Delay ± Jitter before it is handled.
	FaultLatency FaultType = "latency"
	// FaultError answers immediately with Status/Body/Headers.
	FaultError FaultType = "error"
	// FaultAbort drops the connection without a response (connection reset
	// for HTTP/1.1, stream reset for HTTP/2).
	FaultAbort FaultType = "abort"
	// FaultTimeout holds the request open for Delay (default 60s) or until the
	// client gives up, then aborts. Simulates a hung upstream.
	FaultTimeout FaultType = "timeout"
	// FaultThrottle limits the response body to BytesPerSecond.
	FaultThrottle FaultType = "throttle"
)

// FaultRule injects a fault into a fraction of matching requests on a
// service listener. It applies in every mode, before the request is proxied or
// virtualized.
//
// Evaluation: every enabled rule that matches fires independently with its
// Probability. Latency delays add up, throttles take the lowest rate, and the
// first terminal fault (error, abort, timeout) in rule order wins.
type FaultRule struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// Order sorts rules (ascending); ties sort by name.
	Order int `json:"order"`
	// ServiceID limits the rule to one service. Empty = every service.
	ServiceID string `json:"service_id,omitempty"`
	// Methods limits the rule to these HTTP methods. Empty = all.
	Methods []string `json:"methods,omitempty"`
	// PathPattern limits the rule by path. Glob syntax: "*" matches one
	// segment, "**" any number of segments ("/api/**", "/users/*/orders").
	// Prefix with "re:" for an RE2 regex. Empty = all paths.
	PathPattern string `json:"path_pattern,omitempty"`
	// Headers that must be present with these exact values.
	Headers map[string]string `json:"headers,omitempty"`
	// Probability (0..1) that a matching request gets the fault.
	Probability float64   `json:"probability"`
	Type        FaultType `json:"type"`

	Delay          Duration          `json:"delay,omitempty"`  // latency, timeout
	Jitter         Duration          `json:"jitter,omitempty"` // latency
	Status         int               `json:"status,omitempty"` // error (default 503)
	Body           string            `json:"body,omitempty"`   // error
	ResponseHeader map[string]string `json:"response_headers,omitempty"`
	BytesPerSecond int64             `json:"bytes_per_second,omitempty"` // throttle

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// WithDefaults returns a copy with zero-valued fields defaulted.
func (r FaultRule) WithDefaults() FaultRule {
	if r.Type == FaultError && r.Status == 0 {
		r.Status = 503
	}
	if r.Type == FaultTimeout && r.Delay == 0 {
		r.Delay = Duration(60 * time.Second)
	}
	return r
}

// Validate checks the rule (after WithDefaults).
func (r FaultRule) Validate() error {
	var p problems
	if strings.TrimSpace(r.Name) == "" {
		p.add("name is required")
	}
	if !finite(r.Probability) || r.Probability < 0 || r.Probability > 1 {
		p.add("probability must be between 0 and 1")
	}
	switch r.Type {
	case FaultLatency:
		if r.Delay <= 0 && r.Jitter <= 0 {
			p.add("latency fault needs delay or jitter")
		}
	case FaultError:
		if r.Status < 100 || r.Status > 599 {
			p.add("status must be a valid HTTP status")
		}
	case FaultAbort:
	case FaultTimeout:
		if r.Delay <= 0 {
			p.add("timeout fault needs a positive delay")
		}
	case FaultThrottle:
		if r.BytesPerSecond <= 0 {
			p.add("throttle fault needs bytes_per_second > 0")
		}
	default:
		p.add("type %q must be latency, error, abort, timeout or throttle", r.Type)
	}
	if r.Delay < 0 || r.Jitter < 0 {
		p.add("delay and jitter must not be negative")
	}
	if strings.HasPrefix(r.PathPattern, "re:") {
		if _, err := regexp.Compile(strings.TrimPrefix(r.PathPattern, "re:")); err != nil {
			p.add("path_pattern regex: %v", err)
		}
	} else if r.PathPattern != "" && !strings.HasPrefix(r.PathPattern, "/") {
		p.add("path_pattern must start with / (or re:)")
	}
	return p.err()
}
