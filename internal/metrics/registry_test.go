package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// recv returns the next update or fails the test after a timeout.
func recv(t *testing.T, ch <-chan Update) Update {
	t.Helper()
	select {
	case u, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return u
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an update")
		return Update{}
	}
}

// expectEmpty fails if an update is immediately available.
func expectEmpty(t *testing.T, ch <-chan Update) {
	t.Helper()
	select {
	case u, ok := <-ch:
		t.Fatalf("unexpected receive (ok=%v): %+v", ok, u)
	default:
	}
}

func TestRegistryCollectors(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Get("service:a"); ok {
		t.Fatal("Get on empty registry succeeded")
	}
	if got := r.List(); len(got) != 0 {
		t.Fatalf("List = %v", got)
	}

	b := r.Collector("service:b", model.KindService, "b")
	a := r.Collector("service:a", model.KindService, "a")
	z := r.Collector(ReplayCollectorName("z"), model.KindReplay, "z")
	if again := r.Collector("service:a", model.KindReplay, "ignored"); again != a {
		t.Error("Collector should return the existing collector")
	}
	if a.Kind() != model.KindService || a.ID() != "a" {
		t.Errorf("existing collector changed identity: %v %v", a.Kind(), a.ID())
	}
	if got, ok := r.Get("service:b"); !ok || got != b {
		t.Errorf("Get(service:b) = %v, %v", got, ok)
	}

	var names []string
	for _, c := range r.List() {
		names = append(names, c.Name())
	}
	if fmt.Sprint(names) != "[replay:z service:a service:b]" {
		t.Errorf("List order = %v, want sorted by name", names)
	}

	r.Remove("service:a")
	r.Remove("service:nope") // removing nothing is fine
	if _, ok := r.Get("service:a"); ok {
		t.Error("collector still present after Remove")
	}
	if got := r.List(); len(got) != 2 || got[0] != z || got[1] != b {
		t.Errorf("List after Remove = %v", got)
	}
	// A removed collector keeps working for whoever holds it, and a new one
	// under the same name is fresh.
	a.Observe(Observation{Endpoint: "x", Status: 200})
	if fresh := r.Collector("service:a", model.KindService, "a"); fresh == a || fresh.Snapshot().Requests != 0 {
		t.Error("re-created collector should be new")
	}
}

func TestRegistryConcurrentCollectorCreation(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	got := make([]*Collector, 32)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = r.Collector("service:same", model.KindService, "same")
			_ = r.List()
		}()
	}
	wg.Wait()
	for _, c := range got {
		if c != got[0] {
			t.Fatal("concurrent Collector calls returned different collectors")
		}
	}
	if len(r.List()) != 1 {
		t.Errorf("List = %d collectors", len(r.List()))
	}
}

func TestRegistryTickPublishes(t *testing.T) {
	r := NewRegistry()
	ch, cancel := r.Subscribe(4)
	defer cancel()

	b := r.Collector("service:b", model.KindService, "b")
	a := r.Collector("service:a", model.KindService, "a")
	observe(a, 3, "GET /x", 200, 5*time.Millisecond)
	observe(b, 2, "GET /y", 500, 7*time.Millisecond)

	now := base.Add(250 * time.Millisecond)
	ret := r.Tick(now)
	u := recv(t, ch)
	if !u.Time.Equal(now) || !ret.Time.Equal(now) {
		t.Errorf("Time = %v / %v, want %v", u.Time, ret.Time, now)
	}
	if len(u.Snapshots) != 2 || u.Snapshots[0].Name != "service:a" || u.Snapshots[1].Name != "service:b" {
		t.Fatalf("Snapshots = %+v", u.Snapshots)
	}
	if len(u.Points) != 2 {
		t.Fatalf("Points = %+v", u.Points)
	}
	wantT := (base.Unix() - 1) * 1000
	if p := u.Points["service:a"]; p.Requests != 3 || p.T != wantT {
		t.Errorf("a point = %+v", p)
	}
	if p := u.Points["service:b"]; p.Requests != 2 || p.Errors != 2 {
		t.Errorf("b point = %+v", p)
	}
	// Snapshots are taken after the tick, so RPS is the window just closed.
	if u.Snapshots[0].RPS != 3 || u.Snapshots[1].RPS != 2 || u.Snapshots[1].Requests != 2 {
		t.Errorf("snapshots = %+v", u.Snapshots)
	}

	// The point is also in each collector's series.
	if got := a.Series(0); len(got) != 1 || got[0] != u.Points["service:a"] {
		t.Errorf("series = %+v", got)
	}

	// A registry with no collectors still publishes, with non-nil members so
	// the JSON is [] and {} rather than null.
	r2 := NewRegistry()
	e := r2.Tick(now)
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["snapshots"]) != "[]" || string(m["points"]) != "{}" {
		t.Errorf("empty update JSON = %s", raw)
	}
}

func TestUpdateJSONShape(t *testing.T) {
	r := NewRegistry()
	c := r.Collector("service:x", model.KindService, "x")
	observe(c, 1, "GET /", 200, time.Millisecond)
	raw, err := json.Marshal(r.Tick(base))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Time      time.Time `json:"time"`
		Snapshots []struct {
			Name string `json:"name"`
		} `json:"snapshots"`
		Points map[string]struct {
			T        int64 `json:"t"`
			Requests int64 `json:"requests"`
		} `json:"points"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if got.Snapshots[0].Name != "service:x" || got.Points["service:x"].Requests != 1 {
		t.Errorf("decoded = %+v", got)
	}
}

func TestSubscribeDropsWhenFull(t *testing.T) {
	r := NewRegistry()
	slow, cancelSlow := r.Subscribe(1)
	defer cancelSlow()
	fast, cancelFast := r.Subscribe(16)
	defer cancelFast()
	r.Collector("service:a", model.KindService, "a")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 5 {
			r.Tick(base.Add(time.Duration(i) * time.Second))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Tick blocked on a subscriber that is not reading")
	}

	// The slow subscriber kept only the first update; the rest were dropped.
	u := recv(t, slow)
	if want := (base.Unix() - 1) * 1000; u.Points["service:a"].T != want {
		t.Errorf("slow subscriber got T=%d, want the first update (%d)", u.Points["service:a"].T, want)
	}
	expectEmpty(t, slow)
	// A subscriber with room got everything, in order.
	for i := range 5 {
		got := recv(t, fast)
		if want := (base.Unix() - 1 + int64(i)) * 1000; got.Points["service:a"].T != want {
			t.Errorf("fast update %d T = %d, want %d", i, got.Points["service:a"].T, want)
		}
	}
	expectEmpty(t, fast)

	// Once it drains, the slow subscriber receives again.
	r.Tick(base.Add(10 * time.Second))
	recv(t, slow)
}

func TestSubscribeBufferClamped(t *testing.T) {
	r := NewRegistry()
	for _, buf := range []int{0, -3} {
		ch, cancel := r.Subscribe(buf)
		if cap(ch) != 1 {
			t.Errorf("Subscribe(%d) capacity = %d, want 1", buf, cap(ch))
		}
		r.Tick(base)
		recv(t, ch) // an unbuffered channel would have dropped this
		cancel()
	}
}

func TestSubscribeCancel(t *testing.T) {
	r := NewRegistry()
	r.Collector("service:a", model.KindService, "a")
	ch1, cancel1 := r.Subscribe(2)
	ch2, cancel2 := r.Subscribe(2)
	defer cancel2()
	if r.Subscribers() != 2 {
		t.Fatalf("Subscribers = %d", r.Subscribers())
	}

	r.Tick(base)
	cancel1()
	if r.Subscribers() != 1 {
		t.Errorf("Subscribers after cancel = %d", r.Subscribers())
	}
	// Updates queued before cancel are still readable, then the channel
	// reports closed.
	if _, ok := <-ch1; !ok {
		t.Error("buffered update lost on cancel")
	}
	if _, ok := <-ch1; ok {
		t.Error("channel still open after cancel")
	}
	cancel1() // idempotent
	cancel1()

	r.Tick(base.Add(time.Second)) // must not panic sending to the closed channel
	recv(t, ch2)
	recv(t, ch2)
}

func TestSubscribeCancelClosesExactlyOnceConcurrently(t *testing.T) {
	r := NewRegistry()
	_, cancel := r.Subscribe(1)
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cancel() // a double close would panic
		}()
	}
	wg.Wait()
	if r.Subscribers() != 0 {
		t.Errorf("Subscribers = %d", r.Subscribers())
	}
}

func TestSubscribeChurnWhileTicking(t *testing.T) {
	r := NewRegistry()
	r.Collector("service:a", model.KindService, "a")
	var stop atomic.Bool
	var ticker sync.WaitGroup
	ticker.Add(1)
	go func() {
		defer ticker.Done()
		for i := 0; !stop.Load(); i++ {
			r.Tick(base.Add(time.Duration(i) * time.Second))
		}
	}()

	var churners sync.WaitGroup
	for range 8 {
		churners.Add(1)
		go func() {
			defer churners.Done()
			for i := range 300 {
				ch, cancel := r.Subscribe(1 + i%3)
				// Sometimes read an update, then cancel while Tick keeps
				// publishing; a send on the closed channel would panic.
				if i%2 == 0 {
					select {
					case <-ch:
					case <-time.After(time.Millisecond):
					}
				}
				cancel()
				for range ch { // drained to close: the channel was closed once
				}
			}
		}()
	}
	churners.Wait()
	stop.Store(true)
	ticker.Wait()
	if n := r.Subscribers(); n != 0 {
		t.Errorf("Subscribers = %d after every subscription was cancelled", n)
	}
}

func TestRunTicksAndStops(t *testing.T) {
	r := NewRegistry()
	r.interval = 10 * time.Millisecond
	c := r.Collector("service:a", model.KindService, "a")
	ch, cancel := r.Subscribe(64)
	defer cancel()

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()

	observe(c, 5, "GET /x", 200, time.Millisecond)
	var total int64
	var last time.Time
	for range 4 {
		u := recv(t, ch)
		if !u.Time.After(last) {
			t.Errorf("update time %v not after %v", u.Time, last)
		}
		last = u.Time
		total += u.Points["service:a"].Requests
		if len(u.Snapshots) != 1 {
			t.Errorf("snapshots = %d", len(u.Snapshots))
		}
		if got := u.Time.Truncate(10 * time.Millisecond); !got.Equal(u.Time) {
			t.Errorf("update time %v is not aligned to the tick interval", u.Time)
		}
	}
	if total != 5 {
		t.Errorf("requests seen across updates = %d, want 5 (counted once)", total)
	}

	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunReturnsWhenAlreadyCancelled(t *testing.T) {
	r := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run ignored a cancelled context")
	}
}

func TestNextBoundary(t *testing.T) {
	tests := []struct {
		name     string
		now      time.Time
		interval time.Duration
		want     time.Time
	}{
		{"mid second", time.Unix(100, 250e6), time.Second, time.Unix(101, 0)},
		{"just before", time.Unix(100, 999_999_999), time.Second, time.Unix(101, 0)},
		{"exactly on the boundary moves to the next", time.Unix(100, 0), time.Second, time.Unix(101, 0)},
		{"just after", time.Unix(100, 1), time.Second, time.Unix(101, 0)},
		{"sub-second interval", time.Unix(100, 35e6), 10 * time.Millisecond, time.Unix(100, 40e6)},
	}
	for _, tt := range tests {
		if got := nextBoundary(tt.now, tt.interval); !got.Equal(tt.want) {
			t.Errorf("%s: nextBoundary = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestRunAlignsToWallClockSeconds runs the real one-second schedule: the
// first update must land on a whole second shortly after it begins.
func TestRunAlignsToWallClockSeconds(t *testing.T) {
	if testing.Short() {
		t.Skip("waits up to one second")
	}
	r := NewRegistry()
	r.Collector("service:a", model.KindService, "a")
	ch, cancel := r.Subscribe(4)
	defer cancel()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { stop(); <-done }()

	started := time.Now()
	u := recv(t, ch)
	if u.Time.Nanosecond() != 0 {
		t.Errorf("update time %v is not a whole second", u.Time)
	}
	if wait := time.Since(started); wait > 1500*time.Millisecond {
		t.Errorf("first update took %v", wait)
	}
	// The tick closed the second that just ended.
	if want := (u.Time.Unix() - 1) * 1000; u.Points["service:a"].T != want {
		t.Errorf("T = %d, want %d", u.Points["service:a"].T, want)
	}
}
