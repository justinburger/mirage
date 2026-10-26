package replay

import (
	"math"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

func stage(d time.Duration, rate float64, hold bool) model.ReplayStage {
	return model.ReplayStage{Duration: model.Duration(d), Rate: rate, Hold: hold}
}

// arrivalsBefore counts the arrivals of the curve with t < limit seconds.
func arrivalsBefore(c *rateCurve, limit float64) int64 {
	var n int64
	for {
		t, ok := c.arrival(n)
		if !ok || t >= limit {
			return n
		}
		n++
	}
}

func TestRateCurveConstant(t *testing.T) {
	c := newRateCurve(model.ReplayConfig{Rate: 250})
	for _, n := range []int64{0, 1, 2, 249, 250, 1_000_000} {
		got, ok := c.arrival(n)
		want := float64(n) / 250
		if !ok || math.Abs(got-want) > 1e-9 {
			t.Errorf("arrival(%d) = %v, %v; want %v", n, got, ok, want)
		}
	}
	if got := c.cumulative(2); got != 500 {
		t.Errorf("cumulative(2) = %v, want 500", got)
	}
	// Exactly rate × duration arrivals fall inside the duration.
	if got := arrivalsBefore(c, 4); got != 1000 {
		t.Errorf("arrivals in 4s = %d, want 1000", got)
	}
}

func TestRateCurveStages(t *testing.T) {
	sec := time.Second
	tests := []struct {
		name   string
		stages []model.ReplayStage
		// area is the integral of the rate over the whole staged part.
		area float64
		end  float64
	}{
		{"ramp from zero", []model.ReplayStage{stage(10*sec, 100, false)}, 500, 10},
		{"hold steps at once", []model.ReplayStage{stage(10*sec, 100, true)}, 1000, 10},
		{"ramp up then hold then ramp down", []model.ReplayStage{
			stage(10*sec, 100, false), stage(5*sec, 100, true), stage(5*sec, 0, false)}, 500 + 500 + 250, 20},
		{"ramp between two rates", []model.ReplayStage{
			stage(4*sec, 50, true), stage(4*sec, 150, false)}, 200 + 400, 8},
		{"quiet stage", []model.ReplayStage{
			stage(5*sec, 0, true), stage(5*sec, 40, true)}, 200, 10},
		{"fractional rates", []model.ReplayStage{stage(3*sec, 0.5, false)}, 0.75, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newRateCurve(model.ReplayConfig{Stages: tt.stages})
			if math.Abs(c.total-tt.area) > 1e-9 || math.Abs(c.finiteAt-tt.end) > 1e-9 {
				t.Fatalf("total=%v finiteAt=%v, want %v and %v", c.total, c.finiteAt, tt.area, tt.end)
			}
			if got := c.cumulative(tt.end); math.Abs(got-tt.area) > 1e-9 {
				t.Errorf("cumulative(end) = %v, want %v", got, tt.area)
			}
			// The number of arrivals inside the stages is the area, rounded
			// up (arrival n happens when N(t) reaches n, starting with n=0).
			want := int64(math.Ceil(tt.area - 1e-9))
			if got := arrivalsBefore(c, tt.end); got != want {
				t.Errorf("arrivals = %d, want ceil(area) = %d", got, want)
			}
			// Arrival times never go backwards, and N(arrival(n)) == n.
			prev := -1.0
			for n := int64(0); n < want; n++ {
				at, ok := c.arrival(n)
				if !ok || at < prev {
					t.Fatalf("arrival(%d) = %v after %v", n, at, prev)
				}
				prev = at
				if got := c.cumulative(at); math.Abs(got-float64(n)) > 1e-6 {
					t.Fatalf("cumulative(arrival(%d)) = %v", n, got)
				}
			}
		})
	}
}

func TestRateCurveRampShape(t *testing.T) {
	// A linear ramp 0 -> 100 req/s over 10s: half the arrivals come in the
	// last 29.3% of the time (N(t) = 5t²), and the first second holds 5.
	c := newRateCurve(model.ReplayConfig{Stages: []model.ReplayStage{stage(10*time.Second, 100, false)}})
	if got := arrivalsBefore(c, 1); got != 5 {
		t.Errorf("arrivals in first second = %d, want 5", got)
	}
	half, _ := c.arrival(250)
	if want := math.Sqrt(50); math.Abs(half-want) > 1e-9 {
		t.Errorf("arrival(250) = %v, want %v", half, want)
	}
}

func TestRateCurveTailHoldsLastRate(t *testing.T) {
	c := newRateCurve(model.ReplayConfig{Stages: []model.ReplayStage{stage(2*time.Second, 100, true)}})
	// 200 arrivals in the stage, then one every 10ms at the held rate.
	at, ok := c.arrival(200)
	if !ok || math.Abs(at-2) > 1e-9 {
		t.Errorf("arrival(200) = %v, %v; want 2", at, ok)
	}
	at, _ = c.arrival(300)
	if math.Abs(at-3) > 1e-9 {
		t.Errorf("arrival(300) = %v, want 3", at)
	}
	if got := c.cumulative(5); math.Abs(got-500) > 1e-9 {
		t.Errorf("cumulative(5) = %v, want 500", got)
	}
}

func TestRateCurveZeroTail(t *testing.T) {
	c := newRateCurve(model.ReplayConfig{Stages: []model.ReplayStage{stage(time.Second, 10, true), stage(time.Second, 0, true)}})
	if _, ok := c.arrival(10); ok {
		t.Error("a zero tail rate must produce no further arrivals")
	}
	if _, ok := c.arrival(9); !ok {
		t.Error("arrival 9 is inside the first stage")
	}
}

func TestRateCurveRewind(t *testing.T) {
	c := newRateCurve(model.ReplayConfig{Stages: []model.ReplayStage{stage(time.Second, 10, true), stage(time.Second, 20, true)}})
	late, _ := c.arrival(25)
	early, _ := c.arrival(2)
	again, _ := c.arrival(25)
	if late != again || early >= late {
		t.Errorf("cursor lookup is not stable: %v %v %v", early, late, again)
	}
}

func TestSolveSegmentStability(t *testing.T) {
	// Starting at zero, descending to zero and tiny ramps must not produce
	// NaN or Inf.
	segs := []segment{
		{dur: 10, r0: 0, r1: 100, area: 500},
		{dur: 10, r0: 100, r1: 0, area: 500},
		{dur: 1e-3, r0: 0, r1: 1e-9, area: 5e-13},
		{dur: 3600, r0: 1e6, r1: 1e6, area: 3.6e9},
	}
	for i, s := range segs {
		for _, f := range []float64{0, 1e-12, 0.25, 0.5, 0.999999, 1} {
			x := s.area * f
			got := solveSegment(&s, x)
			if math.IsNaN(got) || math.IsInf(got, 0) || got < 0 || got > s.dur+1e-9 {
				t.Errorf("seg %d x=%v: solveSegment = %v", i, x, got)
			}
		}
	}
}

func TestSecondsToDuration(t *testing.T) {
	if got := secondsToDuration(1.5); got != 1500*time.Millisecond {
		t.Errorf("secondsToDuration(1.5) = %v", got)
	}
	if got := secondsToDuration(1e30); got != time.Duration(math.MaxInt64) {
		t.Errorf("huge value must clamp, got %v", got)
	}
	if got := secondsToDuration(-3); got != 0 {
		t.Errorf("negative must clamp to 0, got %v", got)
	}
}

func BenchmarkRateCurveArrival(b *testing.B) {
	c := newRateCurve(model.ReplayConfig{Stages: []model.ReplayStage{
		stage(time.Minute, 5000, false), stage(time.Minute, 5000, true), stage(time.Minute, 0, false)}})
	b.ReportAllocs()
	b.ResetTimer()
	var n int64
	for i := 0; i < b.N; i++ {
		if _, ok := c.arrival(n); !ok {
			n = 0
		}
		n++
	}
}
