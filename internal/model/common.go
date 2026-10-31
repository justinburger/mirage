// Package model holds the domain types shared by every Mirage package.
//
// The types here are the contract between packages: the store persists them,
// the proxy produces them, the replay engine consumes them and the API
// serialises them to the web UI. JSON field names are part of the public API
// (see docs/API.md) and must stay stable.
package model

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewID returns a random identifier such as "svc_k3j2h4m5n6p7q8r9". The prefix
// identifies the kind of object ("svc", "rec", "prof", "rule", "flt", "rpl").
func NewID(prefix string) string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("model: crypto/rand failed: %v", err))
	}
	return prefix + "_" + idEncoding.EncodeToString(b[:])
}

// NewSecret returns a random 32-byte hex secret used to key deterministic
// anonymization (HMAC).
func NewSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("model: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// Duration is a time.Duration that marshals to JSON as a Go duration string
// ("250ms", "1.5s"). When unmarshalling it also accepts a JSON number, which is
// interpreted as milliseconds, so UIs and config files can use either form.
type Duration time.Duration

// D converts to a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Ms returns the duration in (fractional) milliseconds.
func (d Duration) Ms() float64 { return float64(d) / float64(time.Millisecond) }

// String formats like time.Duration.String but always in ASCII ("125us",
// never "125µs"), so JSON consumers can parse it simply.
func (d Duration) String() string {
	return strings.Replace(time.Duration(d).String(), "µs", "us", 1)
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` {
		*d = 0
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		v, err := ParseDuration(str)
		if err != nil {
			return err
		}
		*d = v
		return nil
	}
	ms, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("invalid duration %s", s)
	}
	*d = Duration(ms * float64(time.Millisecond))
	return nil
}

// ParseDuration parses a Go duration string ("1m30s"). A bare number is
// interpreted as milliseconds.
func ParseDuration(s string) (Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if ms, err := strconv.ParseFloat(s, 64); err == nil {
		return Duration(ms * float64(time.Millisecond)), nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return Duration(v), nil
}

// Sentinel errors shared across packages. Packages wrap these with context;
// the API maps them to HTTP status codes (404, 409, 400).
var (
	ErrNotFound   = errors.New("not found")
	ErrConflict   = errors.New("conflict")
	ErrValidation = errors.New("validation failed")
)

// ValidationError lists every problem found while validating an object.
// It wraps ErrValidation so errors.Is(err, ErrValidation) works.
type ValidationError struct {
	Problems []string `json:"problems"`
}

func (e *ValidationError) Error() string {
	return "validation failed: " + strings.Join(e.Problems, "; ")
}

func (e *ValidationError) Unwrap() error { return ErrValidation }

// problems accumulates validation messages.
type problems []string

func (p *problems) add(format string, args ...any) { *p = append(*p, fmt.Sprintf(format, args...)) }

func (p problems) err() error {
	if len(p) == 0 {
		return nil
	}
	return &ValidationError{Problems: p}
}

// finite reports whether every value is a real number (not NaN or ±Inf).
// Comparisons with NaN are always false, so range checks alone let it through.
func finite(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// validHeaderName reports whether s is an HTTP header field name (an RFC 9110
// token).
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// validHeaderValue reports whether s can be sent as a header value: no CR,
// LF, NUL or other control characters except tab.
func validHeaderValue(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}
