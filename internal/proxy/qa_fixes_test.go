package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// ---- Content-Type sniffing ---------------------------------------------

// noContentTypeUpstream answers /bare without a Content-Type (the nil entry
// keeps the upstream's own net/http from sniffing one), /typed with one, and
// /nobody (204) and a HEAD without one.
func noContentTypeUpstream(t testing.TB) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/typed":
			w.Header().Set("Content-Type", "application/x-custom")
		default:
			w.Header()["Content-Type"] = nil
		}
		if r.URL.Path == "/nobody" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, "<html><body>this would be sniffed as html</body></html>")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A response the upstream sent without a Content-Type reaches the client
// without one, in every proxied mode: net/http would otherwise sniff the body
// and add a header the upstream never sent.
func TestUpstreamWithoutContentTypeIsNotSniffed(t *testing.T) {
	up := noContentTypeUpstream(t)
	for _, tc := range []struct {
		name string
		mode model.Mode
		mods []func(*model.Service)
	}{
		{"passthrough", model.ModePassthrough, nil},
		{"record", model.ModeRecord, nil},
		{"hybrid miss", model.ModeHybrid, nil},
		{"record anonymized", model.ModeRecord, []func(*model.Service){func(s *model.Service) { s.AnonymizeCapture = true }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			svc := e.service(up.URL, tc.mode, tc.mods...)
			base := e.start(svc)
			c := newClient(t)

			for _, p := range []string{"/bare", "/nobody"} {
				_, hdr, _ := do(t, c, "GET", base+p, nil)
				if _, has := hdr["Content-Type"]; has {
					t.Errorf("GET %s: Content-Type = %q, the upstream sent none", p, hdr.Get("Content-Type"))
				}
			}
			_, hdr, body := do(t, c, "GET", base+"/typed", nil)
			if hdr.Get("Content-Type") != "application/x-custom" || !strings.Contains(string(body), "sniffed") {
				t.Errorf("typed: %v %s", hdr, body)
			}

			if tc.mode.UsesRecording() {
				// What is captured is what the upstream sent: no entry, not
				// even an empty one.
				for _, ex := range e.waitStored(svc.RecordingID, 3) {
					_, has := ex.Response.Header["Content-Type"]
					switch ex.Request.Path {
					case "/typed":
						if ex.Response.Header.Get("Content-Type") != "application/x-custom" {
							t.Errorf("captured %s headers = %v", ex.Request.Path, ex.Response.Header)
						}
					default:
						if has {
							t.Errorf("captured %s headers = %v, the upstream sent no Content-Type", ex.Request.Path, ex.Response.Header)
						}
					}
				}
			}
		})
	}
}

// The throttle wrapper of a fault shares the header map with the client's
// writer, so the suppression still applies.
func TestUpstreamWithoutContentTypeThroughThrottle(t *testing.T) {
	up := noContentTypeUpstream(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModePassthrough)
	base := e.start(svc)
	setRules(t, e, model.FaultRule{Type: model.FaultThrottle, BytesPerSecond: 10_000_000})
	_, hdr, body := do(t, newClient(t), "GET", base+"/bare", nil)
	if _, has := hdr["Content-Type"]; has || len(body) == 0 {
		t.Errorf("hdr = %v, body %q", hdr, body)
	}
}

// ---- virtualizer and the anonymization profile ---------------------------

func TestSameVirtualizationComparesAnonymization(t *testing.T) {
	base := model.Service{RecordingID: "rec_1", Mode: model.ModeVirtualize}.WithDefaults()
	with := func(mods ...func(*model.Service)) model.Service {
		s := base
		for _, m := range mods {
			m(&s)
		}
		return s
	}
	on := func(s *model.Service) { s.AnonymizeCapture = true }
	profile := func(id string) func(*model.Service) { return func(s *model.Service) { s.ProfileID = id } }

	for _, tc := range []struct {
		name string
		a, b model.Service
		same bool
	}{
		{"off to on", base, with(on), false},
		{"on to off", with(on), base, false},
		{"on, same profile", with(on, profile("p1")), with(on, profile("p1")), true},
		{"on, other profile", with(on, profile("p1")), with(on, profile("p2")), false},
		{"on, default spelled out", with(on), with(on, profile(model.DefaultProfileID)), true},
		{"on, default to other", with(on), with(on, profile("p2")), false},
		{"off, profile is irrelevant", with(profile("p1")), with(profile("p2")), true},
	} {
		if got := sameVirtualization(tc.a, tc.b); got != tc.same {
			t.Errorf("%s: sameVirtualization = %v, want %v", tc.name, got, tc.same)
		}
	}
}

// Turning anonymize_capture on, or moving to another profile, rebuilds the
// virtualizer of a running service (its engine embeds the profile's
// anonymizer); an unrelated change keeps it.
func TestApplyRebuildsVirtualizerWhenAnonymizationChanges(t *testing.T) {
	up := failIfHit(t)
	e := newEnv(t)
	if _, err := e.st.PutProfile(context.Background(), model.AnonymizationProfile{ID: "prof_other", Name: "other", Secret: "s"}); err != nil {
		t.Fatal(err)
	}
	svc := e.service(up.URL, model.ModeVirtualize)
	e.start(svc)
	if n := e.virts.count(); n != 1 {
		t.Fatalf("builds = %d", n)
	}
	apply := func(s model.Service) {
		t.Helper()
		if err := e.mgr.Apply(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}

	renamed := svc
	renamed.Name = "renamed"
	apply(renamed)
	if n := e.virts.count(); n != 1 {
		t.Errorf("an unrelated change rebuilt the virtualizer (builds = %d)", n)
	}
	profileOnly := renamed
	profileOnly.ProfileID = "prof_other" // anonymize_capture is off: irrelevant
	apply(profileOnly)
	if n := e.virts.count(); n != 1 {
		t.Errorf("a profile change with anonymize_capture off rebuilt the virtualizer (builds = %d)", n)
	}

	anon := profileOnly
	anon.AnonymizeCapture = true
	apply(anon)
	if n := e.virts.count(); n != 2 {
		t.Errorf("anonymize_capture on: builds = %d, want 2", n)
	}
	anon.ProfileID = model.DefaultProfileID
	apply(anon)
	if n := e.virts.count(); n != 3 {
		t.Errorf("other profile: builds = %d, want 3", n)
	}
	apply(anon)
	if n := e.virts.count(); n != 3 {
		t.Errorf("same configuration: builds = %d, want 3", n)
	}
	anon.AnonymizeCapture = false
	apply(anon)
	if n := e.virts.count(); n != 4 {
		t.Errorf("anonymize_capture off: builds = %d, want 4", n)
	}
}

// ProfileChanged rebuilds the virtualizer of every running virtualize and
// hybrid service that uses the profile with anonymize_capture on, and nothing
// else.
func TestProfileChangedRebuildsVirtualizers(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.st.PutProfile(ctx, model.AnonymizationProfile{ID: "prof_other", Name: "other", Secret: "s"}); err != nil {
		t.Fatal(err)
	}
	anon := func(s *model.Service) { s.AnonymizeCapture = true }
	other := func(s *model.Service) { s.AnonymizeCapture = true; s.ProfileID = "prof_other" }

	virt := e.service(up.URL, model.ModeVirtualize, anon)
	hyb := e.service(up.URL, model.ModeHybrid, anon)
	rec := e.service(up.URL, model.ModeRecord, anon)
	plain := e.service(up.URL, model.ModeVirtualize)
	elsewhere := e.service(up.URL, model.ModeVirtualize, other)
	stopped := e.service(up.URL, model.ModeVirtualize, anon, func(s *model.Service) { s.Enabled = false })
	for _, s := range []model.Service{virt, hyb, rec, plain, elsewhere, stopped} {
		if s.Enabled {
			e.start(s)
		} else if err := e.mgr.Apply(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	before := e.virts.count() // virt, hyb, plain, elsewhere
	if before != 4 {
		t.Fatalf("builds before = %d, want 4", before)
	}
	old := func(id string) Virtualizer {
		t.Helper()
		s := e.mgr.get(id)
		return s.state.Load().virt
	}
	oldVirt, oldHyb, oldPlain, oldElse := old(virt.ID), old(hyb.ID), old(plain.ID), old(elsewhere.ID)
	recAnon := e.mgr.get(rec.ID).state.Load().anon

	if err := e.mgr.ProfileChanged(ctx, ""); err != nil { // "" is the default profile
		t.Fatal(err)
	}
	if got := e.virts.count(); got != before+2 {
		t.Errorf("builds after = %d, want %d (virtualize + hybrid only)", got, before+2)
	}
	if old(virt.ID) == oldVirt || old(hyb.ID) == oldHyb {
		t.Error("the virtualizers of the services using the profile were kept")
	}
	if old(plain.ID) != oldPlain || old(elsewhere.ID) != oldElse {
		t.Error("a service that does not use the profile with anonymize_capture was rebuilt")
	}
	if e.mgr.get(rec.ID).state.Load().anon == recAnon {
		t.Error("the capture anonymizer of the record service was not reloaded")
	}
	if e.mgr.get(stopped.ID).state.Load() != nil {
		t.Error("a stopped service was started")
	}
	// The replaced virtualizers are closed once their requests drain.
	waitFor(t, "the old virtualizers to be closed", func() bool {
		return oldVirt.(*fakeVirt).closed.Load() && oldHyb.(*fakeVirt).closed.Load()
	})
	if oldPlain.(*fakeVirt).closed.Load() || oldElse.(*fakeVirt).closed.Load() {
		t.Error("the virtualizer of an unrelated service was closed")
	}

	// The services keep serving.
	base := e.url(virt.ID)
	if status, hdr, _ := do(t, newClient(t), "GET", base+"/x", nil); status != 404 || hdr.Get("X-Mirage-Match") != "miss" {
		t.Errorf("rebuilt virtual service: %d %v", status, hdr)
	}

	// The other profile reaches only its own service.
	if err := e.mgr.ProfileChanged(ctx, "prof_other"); err != nil {
		t.Fatal(err)
	}
	if got := e.virts.count(); got != before+3 {
		t.Errorf("builds after the other profile = %d, want %d", got, before+3)
	}
}

// A virtualizer that cannot be rebuilt keeps serving with the previous one and
// the error is reported; the next successful rebuild clears it.
func TestProfileChangedKeepsVirtualizerWhenRebuildFails(t *testing.T) {
	up := newEcho(t)
	e := newEnv(t)
	svc := e.service(up.URL, model.ModeHybrid, func(s *model.Service) { s.AnonymizeCapture = true })
	seedExchange(t, e, svc.RecordingID, "GET", "/known", 200, `{"known":true}`)
	base := e.start(svc)
	c := newClient(t)
	oldVirt := e.mgr.get(svc.ID).state.Load().virt

	e.virts.setFail(errors.New("profile broke"))
	err := e.mgr.ProfileChanged(context.Background(), model.DefaultProfileID)
	if err == nil || !strings.Contains(err.Error(), "profile broke") || !strings.Contains(err.Error(), svc.ID) {
		t.Fatalf("ProfileChanged = %v", err)
	}
	if got := e.mgr.get(svc.ID).state.Load().virt; got != oldVirt {
		t.Error("a failed rebuild replaced the virtualizer")
	}
	if st := e.mgr.Status(svc.ID); !strings.Contains(st.Error, "profile broke") || !st.Running {
		t.Errorf("status = %+v", st)
	}
	if status, _, body := do(t, c, "GET", base+"/known", nil); status != 200 || !strings.Contains(string(body), "known") {
		t.Errorf("after a failed rebuild: %d %s", status, body)
	}

	e.virts.setFail(nil)
	if err := e.mgr.ProfileChanged(context.Background(), model.DefaultProfileID); err != nil {
		t.Fatal(err)
	}
	if st := e.mgr.Status(svc.ID); st.Error != "" {
		t.Errorf("error after a successful rebuild: %q", st.Error)
	}
	if got := e.mgr.get(svc.ID).state.Load().virt; got == oldVirt {
		t.Error("the virtualizer was not rebuilt")
	}
}

// ---- config_updated_at -------------------------------------------------

// The status carries the UpdatedAt of the configuration last applied, for
// stopped services too, so a stream client sees a rename that changes
// nothing else.
func TestStatusCarriesConfigUpdatedAt(t *testing.T) {
	e := newEnv(t)
	svc := e.service(failIfHit(t).URL, model.ModePassthrough)
	svc.Enabled = false
	for i, at := range []time.Time{time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC), time.Date(2026, 10, 30, 2, 0, 0, 0, time.UTC)} {
		svc.Name = fmt.Sprintf("name-%d", i)
		svc.UpdatedAt = at
		if err := e.mgr.Apply(context.Background(), svc); err != nil {
			t.Fatal(err)
		}
		st := e.mgr.Status(svc.ID)
		if st.Running || st.ConfigUpdatedAt == nil || !st.ConfigUpdatedAt.Equal(at) {
			t.Fatalf("apply %d: running=%v config_updated_at=%v, want %v", i, st.Running, st.ConfigUpdatedAt, at)
		}
	}
}

// ---- early answers to large uploads ------------------------------------

// An upstream that answers before reading a large upload gets the client a
// "Connection: close", as net/http announces it for a handler that is not in
// full duplex mode: the proxy will close the connection rather than drain
// the rest of the body, and a keep-alive client must not reuse it. A small
// remainder, or a body read to its end, keeps the connection.
//
// The large uploads are 64 MiB, far more than the socket buffers between the
// client, the proxy and the upstream can hold, so most of the body is still
// unread when the answer comes on any machine.
func TestEarlyAnswerToALargeUploadAnnouncesTheClose(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/read" {
			_, _ = io.Copy(io.Discard, r.Body)
		} else {
			// Answer at once: without full duplex net/http would first try
			// to drain part of a chunked body.
			_ = http.NewResponseController(w).EnableFullDuplex()
		}
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	t.Cleanup(up.Close)
	big := make([]byte, 64<<20)
	for _, mode := range []model.Mode{model.ModePassthrough, model.ModeRecord} {
		e := newEnv(t)
		base := e.start(e.service(up.URL, mode))
		for _, tc := range []struct {
			name      string
			path      string
			body      []byte
			chunked   bool
			wantClose bool
		}{
			{"64 MiB, answered early", "/early", big, false, true},
			{"64 MiB chunked, answered early", "/early", big, true, true},
			{"10 KiB, answered early", "/early", big[:10<<10], false, false},
			{"2 MiB, read to the end", "/read", big[:2<<20], false, false},
		} {
			for range 3 {
				req, _ := http.NewRequest("POST", base+tc.path, bytes.NewReader(tc.body))
				if tc.chunked {
					req.ContentLength = -1
				}
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("%s, %s: %v", mode, tc.name, err)
				}
				_, _ = io.Copy(io.Discard, res.Body)
				res.Body.Close()
				if res.StatusCode != http.StatusUnauthorized {
					t.Fatalf("%s, %s: status %d", mode, tc.name, res.StatusCode)
				}
				if res.Close != tc.wantClose {
					t.Errorf("%s, %s: Connection: close announced = %v, want %v", mode, tc.name, res.Close, tc.wantClose)
					break
				}
			}
		}
	}
}
