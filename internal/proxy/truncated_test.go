package proxy

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// Responses larger than max_body_bytes are stored truncated, which virtualize
// cannot serve faithfully. The status counts them and warns while it is still
// cheap to fix (raise max_body_bytes, record again); raising the cap clears it.
func TestTruncatedCapturesAreCountedAndWarned(t *testing.T) {
	up := bodyUpstream(t, 5000)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.MaxBodyBytes = 1000 })
	base := e.start(svc)
	c := newClient(t)

	for i := 0; i < 3; i++ {
		if status, _, body := do(t, c, "GET", fmt.Sprintf("%s/big%d", base, i), nil); status != 200 || len(body) != 5000 {
			t.Fatalf("traffic itself must never be truncated: %d, %d bytes", status, len(body))
		}
	}
	stored := e.waitStored(svc.RecordingID, 3)
	for _, ex := range stored {
		if !ex.Response.BodyTruncated || len(ex.Response.Body) != 1000 || ex.Response.BodySize != 5000 {
			t.Fatalf("stored body: truncated=%v len=%d size=%d", ex.Response.BodyTruncated, len(ex.Response.Body), ex.Response.BodySize)
		}
	}
	waitFor(t, "three truncated captures", func() bool { return e.mgr.Status(svc.ID).CaptureTruncated == 3 })
	st := e.mgr.Status(svc.ID)
	if st.Error != "" || !st.Running || !strings.Contains(st.Warning, "max_body_bytes") || !strings.HasPrefix(st.Warning, "3 captured responses") {
		t.Errorf("status = %+v, want running with a max_body_bytes warning", st)
	}

	// Raising the cap is the fix the warning asks for: it clears the count.
	svc.MaxBodyBytes = 10000
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	if st := e.mgr.Status(svc.ID); st.CaptureTruncated != 0 || st.Warning != "" {
		t.Errorf("after raising the cap: truncated %d, warning %q", st.CaptureTruncated, st.Warning)
	}
	do(t, c, "GET", base+"/whole", nil)
	exs := e.waitStored(svc.RecordingID, 4)
	if last := exs[len(exs)-1]; last.Response.BodyTruncated || len(last.Response.Body) != 5000 {
		t.Errorf("after raising the cap the body is still cut: %d bytes", len(last.Response.Body))
	}
	if st := e.mgr.Status(svc.ID); st.CaptureTruncated != 0 {
		t.Errorf("a complete capture was counted as truncated")
	}
}

// max_body_bytes -1 drops bodies on purpose: that is not worth a warning.
func TestHeadersOnlyCaptureIsNotWarned(t *testing.T) {
	up := bodyUpstream(t, 5000)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.MaxBodyBytes = -1 })
	base := e.start(svc)
	do(t, newClient(t), "GET", base+"/x", nil)
	e.waitStored(svc.RecordingID, 1)
	if st := e.mgr.Status(svc.ID); st.CaptureTruncated != 0 || st.Warning != "" {
		t.Errorf("headers-only capture: truncated %d, warning %q", st.CaptureTruncated, st.Warning)
	}
}

// Captures still queued when the cap is raised were cut under the old cap:
// counting them would bring the warning straight back. Lowering the cap does
// not fix anything, so it does not clear the count.
func TestTruncatedCountFollowsTheCurrentCap(t *testing.T) {
	up := bodyUpstream(t, 5000)
	e := newEnv(t, func(d *Deps) { d.CaptureFlushInterval = 400 * time.Millisecond; d.CaptureBatchSize = 1000 })
	svc := e.service(up.URL, model.ModeRecord, func(s *model.Service) { s.MaxBodyBytes = 1000 })
	base := e.start(svc)
	c := newClient(t)
	for i := 0; i < 3; i++ {
		do(t, c, "GET", fmt.Sprintf("%s/q%d", base, i), nil)
	}
	svc.MaxBodyBytes = 100000 // raised while the three captures wait in the queue
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	e.waitStored(svc.RecordingID, 3)
	if st := e.mgr.Status(svc.ID); st.CaptureTruncated != 0 || st.Warning != "" {
		t.Errorf("queued old-cap captures counted after raising the cap: truncated %d, warning %q", st.CaptureTruncated, st.Warning)
	}

	svc.MaxBodyBytes = 1000
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	do(t, c, "GET", base+"/again", nil)
	waitFor(t, "one truncated capture", func() bool { return e.mgr.Status(svc.ID).CaptureTruncated == 1 })
	svc.MaxBodyBytes = 500 // lowering keeps the recording just as truncated
	if err := e.mgr.Apply(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	if st := e.mgr.Status(svc.ID); st.CaptureTruncated != 1 || !strings.HasPrefix(st.Warning, "1 captured response was larger") {
		t.Errorf("after lowering the cap: truncated %d, warning %q", st.CaptureTruncated, st.Warning)
	}
}

func TestTruncationWarningText(t *testing.T) {
	for _, tc := range []struct {
		served, captured int64
		want             string
	}{
		{0, 0, ""},
		{0, 1, "1 captured response was larger than max_body_bytes and was stored truncated; virtualize answers it 502"},
		{0, 2, "2 captured responses were larger than max_body_bytes and were stored truncated; virtualize answers them 502"},
		{1, 2, "1 request matched recordings that hold only part of their response body and was answered 502"},
		{5, 0, "5 requests matched recordings that hold only part of their response body and were answered 502"},
	} {
		got := truncationWarning(tc.served, tc.captured)
		if (tc.want == "") != (got == "") || !strings.HasPrefix(got, tc.want) {
			t.Errorf("truncationWarning(%d, %d) = %q, want prefix %q", tc.served, tc.captured, got, tc.want)
		}
	}
	for _, tc := range []struct {
		runtime, trunc string
		served         bool
		want           string
	}{
		{"", "", false, ""},
		{"capture limit reached", "", false, "capture limit reached"},
		{"", "T", false, "T"},
		{"capture limit reached", "T", false, "capture limit reached"},
		{"capture limit reached", "T", true, "T; capture limit reached"},
	} {
		if got := joinWarnings(tc.runtime, tc.trunc, tc.served); got != tc.want {
			t.Errorf("joinWarnings(%q, %q, %v) = %q, want %q", tc.runtime, tc.trunc, tc.served, got, tc.want)
		}
	}
}

// Hybrid answers truncated-only matches from the upstream, virtualize with a
// 502; the engine is built for one or the other, so switching rebuilds it.
func TestSwitchingInOrOutOfHybridRebuildsTheEngine(t *testing.T) {
	base := model.Service{RecordingID: "rec_x", Mode: model.ModeVirtualize}
	for _, tc := range []struct {
		from, to model.Mode
		same     bool
	}{
		{model.ModeVirtualize, model.ModeHybrid, false},
		{model.ModeHybrid, model.ModeVirtualize, false},
		{model.ModeHybrid, model.ModeHybrid, true},
		{model.ModeVirtualize, model.ModeVirtualize, true},
	} {
		a, b := base, base
		a.Mode, b.Mode = tc.from, tc.to
		if got := sameVirtualization(a, b); got != tc.same {
			t.Errorf("sameVirtualization(%s -> %s) = %v, want %v", tc.from, tc.to, got, tc.same)
		}
	}
}
