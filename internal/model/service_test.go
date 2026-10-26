package model

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func validService() Service {
	return Service{
		Name: "svc", ListenAddr: ":9001", Mode: ModePassthrough,
		Upstream: "http://127.0.0.1:8080",
	}.WithDefaults()
}

// TestServiceListenAddrPort checks that the port of listen_addr must be a
// number in 0-65535, not any text after a colon.
func TestServiceListenAddrPort(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
	}{
		{":9001", true},
		{":0", true},
		{":65535", true},
		{"127.0.0.1:8080", true},
		{"[::1]:9000", true},
		{"localhost:1", true},
		{":abc", false},
		{":99999", false},
		{":65536", false},
		{":-1", false},
		{":+80", false},
		{":8 0", false},
		{":", false},
		{"127.0.0.1:", false},
		{"127.0.0.1", false},
		{"", false},
		{":0x50", false},
	}
	for _, c := range cases {
		t.Run(c.addr, func(t *testing.T) {
			s := validService()
			s.ListenAddr = c.addr
			err := s.Validate()
			if c.ok && err != nil {
				t.Fatalf("Validate(%q) = %v, want ok", c.addr, err)
			}
			if !c.ok {
				if err == nil {
					t.Fatalf("Validate(%q) accepted an invalid listen_addr", c.addr)
				}
				if !errors.Is(err, ErrValidation) {
					t.Errorf("error %v does not wrap ErrValidation", err)
				}
				if !strings.Contains(err.Error(), "listen_addr") {
					t.Errorf("error %q does not name listen_addr", err)
				}
			}
		})
	}
}

// TestServiceStatusStartedAtUTC checks that the JSON form of a service status
// writes started_at in UTC ("Z"), like every other timestamp of the API,
// whatever zone the producer's clock was in.
func TestServiceStatusStartedAtUTC(t *testing.T) {
	zone := time.FixedZone("PDT", -7*3600)
	at := time.Date(2026, 10, 25, 19, 29, 26, 181680000, zone)
	st := ServiceStatus{ServiceID: "svc_x", Running: true, Mode: ModeRecord, StartedAt: &at, Captured: 3}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	got, _ := raw["started_at"].(string)
	if want := "2026-10-26T02:29:26.18168Z"; got != want {
		t.Errorf("started_at = %q, want %q", got, want)
	}
	if raw["captured"] != float64(3) || raw["service_id"] != "svc_x" || raw["running"] != true {
		t.Errorf("other fields changed: %s", b)
	}
	if !at.Equal(*st.StartedAt) || at.Location() != zone {
		t.Errorf("Marshal modified the caller's time: %v", st.StartedAt)
	}
	// Absent stays absent.
	b, _ = json.Marshal(ServiceStatus{ServiceID: "svc_y"})
	if strings.Contains(string(b), "started_at") {
		t.Errorf("zero status has started_at: %s", b)
	}
	// Round trip.
	var back ServiceStatus
	if err := json.Unmarshal([]byte(`{"service_id":"svc_x","started_at":"2026-10-26T02:29:26Z"}`), &back); err != nil || back.StartedAt == nil {
		t.Fatalf("unmarshal: %v %+v", err, back)
	}
}
