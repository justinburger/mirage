package har

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Timestamp is a time that reads every common ISO 8601 spelling found in HAR
// files and writes RFC 3339 in UTC with nanosecond precision (trailing zeros
// trimmed), for example "2024-01-31T10:00:00.123Z". An empty string or null
// reads as the zero time.
type Timestamp struct {
	time.Time
}

// timestampLayouts are tried in order. Go accepts a fractional second after
// the seconds field even when the layout has none.
var timestampLayouts = [...]string{
	time.RFC3339,                // 2024-01-31T10:00:00Z, ...+02:00
	"2006-01-02T15:04:05Z0700",  // ...+0200
	"2006-01-02T15:04:05Z07",    // ...+02
	"2006-01-02 15:04:05Z07:00", // space instead of T
	"2006-01-02 15:04:05Z0700",
	"2006-01-02 15:04:05Z07",
	"2006-01-02T15:04:05", // no zone: UTC
	"2006-01-02 15:04:05",
}

// parseTimestamp parses s with the layouts above, then as an HTTP date
// (cookie expiry style).
func parseTimestamp(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range timestampLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	if t, err := http.ParseTime(s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid timestamp %q", s)
}

// MarshalJSON implements json.Marshaler.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 40)
	b = append(b, '"')
	b = t.Time.UTC().AppendFormat(b, time.RFC3339Nano)
	return append(b, '"'), nil
}

// UnmarshalJSON implements json.Unmarshaler.
func (t *Timestamp) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] != '"' {
		return fmt.Errorf("invalid timestamp %s: expected a string", b)
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := parseTimestamp(s)
	if err != nil {
		return err
	}
	t.Time = v
	return nil
}

// Size is a byte count. HAR says integer, but files in the wild contain 12.0,
// "12" and null; Size reads all of them. Write is a plain JSON integer.
type Size int64

// UnmarshalJSON implements json.Unmarshaler.
func (s *Size) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	str := string(b)
	if b[0] == '"' {
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		if str = strings.TrimSpace(str); str == "" {
			return nil
		}
	}
	if n, err := strconv.ParseInt(str, 10, 64); err == nil {
		*s = Size(n)
		return nil
	}
	f, err := strconv.ParseFloat(str, 64)
	if err != nil || math.IsNaN(f) || f >= math.MaxInt64 || f <= math.MinInt64 {
		return fmt.Errorf("invalid size %s", b)
	}
	*s = Size(math.Round(f))
	return nil
}

// UnmarshalJSON reads a cookie, tolerating the non-string "expires" values
// some tools write (null, -1).
func (c *Cookie) UnmarshalJSON(b []byte) error {
	type plain Cookie
	aux := struct {
		*plain
		Expires json.RawMessage `json:"expires"`
	}{plain: (*plain)(c)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	c.Expires = ""
	if raw := bytes.TrimSpace(aux.Expires); len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &c.Expires); err != nil {
			return err
		}
	}
	return nil
}
