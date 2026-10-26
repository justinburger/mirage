package replay

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

func tlCfg(mod func(*model.ReplayConfig)) model.ReplayConfig {
	cfg := model.ReplayConfig{Mode: model.ReplayTimeline}.WithDefaults()
	if mod != nil {
		mod(&cfg)
	}
	return cfg
}

func assertDues(t *testing.T, got, want []time.Duration) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d dues %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if d := got[i] - want[i]; d < -time.Microsecond || d > time.Microsecond {
			t.Fatalf("due[%d] = %v, want %v\n got  %v\n want %v", i, got[i], want[i], got, want)
		}
	}
}

func TestTimelineSpeed(t *testing.T) {
	offsets := []int{0, 100, 300, 1000}
	tests := []struct {
		name  string
		speed float64
		want  []time.Duration
	}{
		{"real time", 1, []time.Duration{0, ms(100), ms(300), ms(1000)}},
		{"ten times faster", 10, []time.Duration{0, ms(10), ms(30), ms(100)}},
		{"ten times slower", 0.1, []time.Duration{0, ms(1000), ms(3000), ms(10000)}},
		{"extreme", 100000, []time.Duration{0, ms(0.001), ms(0.003), ms(0.01)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tlCfg(func(c *model.ReplayConfig) { c.Speed = tt.speed })
			g := newTimelineGen(itemsAt(offsets), cfg)
			assertDues(t, dues(drain(g, cfg, 100)), tt.want)
		})
	}
}

func TestTimelineLoopPassSpacing(t *testing.T) {
	// Three requests at 0, 100, 300ms: the mean gap is 150ms, so a pass lasts
	// 450ms and the next pass starts there.
	cfg := tlCfg(func(c *model.ReplayConfig) { c.Loop = true; c.MaxRequests = 9 })
	g := newTimelineGen(itemsAt([]int{0, 100, 300}), cfg)
	got := dues(drain(g, cfg, 100))
	assertDues(t, got, []time.Duration{
		0, ms(100), ms(300),
		ms(450), ms(550), ms(750),
		ms(900), ms(1000), ms(1200),
	})
	// At 10x the whole pattern is compressed.
	cfg.Speed = 10
	g = newTimelineGen(itemsAt([]int{0, 100, 300}), cfg)
	got = dues(drain(g, cfg, 100))
	if d := got[3] - ms(45); d < -time.Microsecond || d > time.Microsecond {
		t.Errorf("second pass starts at %v, want 45ms", got[3])
	}
}

func TestTimelineLoopNeverStallsOnZeroSpan(t *testing.T) {
	// One request, or identical timestamps, have no recorded rate: a pass is
	// taken to last one second instead of looping at infinite speed, and the
	// requests of a pass are spread over that second.
	tests := []struct {
		offsets []int
		want    int // requests due in the first 3.5s
	}{
		{[]int{0}, 4},             // 0, 1, 2, 3s
		{[]int{0, 0, 0}, 3*3 + 2}, // 3 passes + the first two of the fourth (3.0, 3.33s; 3.67s is late)
	}
	for _, tt := range tests {
		cfg := tlCfg(func(c *model.ReplayConfig) { c.Loop = true; c.Duration = model.Duration(3500 * time.Millisecond) })
		g := newTimelineGen(itemsAt(tt.offsets), cfg)
		if got := len(drain(g, cfg, 1000)); got != tt.want {
			t.Errorf("offsets %v: %d requests in 3.5s, want %d", tt.offsets, got, tt.want)
		}
	}
}

func TestTimelineAmplifySpreadsCopiesEvenly(t *testing.T) {
	// Requests at 0 and 1s; the mean gap makes a pass 2s. Each is sent four
	// times, spread over the gap that follows it.
	cfg := tlCfg(func(c *model.ReplayConfig) { c.Amplify = 4 })
	g := newTimelineGen(itemsAt([]int{0, 1000}), cfg)
	js := drain(g, cfg, 100)
	assertDues(t, dues(js), []time.Duration{
		0, ms(250), ms(500), ms(750),
		ms(1000), ms(1250), ms(1500), ms(1750),
	})
	for i, j := range js {
		if j.copy != i%4 {
			t.Errorf("job %d: copy index %d, want %d", i, j.copy, i%4)
		}
	}
	// Never all at once: no two copies of an item share a due time.
	for i := 1; i < 4; i++ {
		if js[i].due == js[i-1].due {
			t.Errorf("copies %d and %d fire together", i-1, i)
		}
	}
}

func TestTimelineAmplifyWithSimultaneousRequests(t *testing.T) {
	// Two requests recorded in the same instant: copies spread over the gap
	// to the next later request, not over a zero gap.
	cfg := tlCfg(func(c *model.ReplayConfig) { c.Amplify = 2 })
	g := newTimelineGen(itemsAt([]int{0, 0, 100}), cfg)
	got := dues(drain(g, cfg, 100))
	// The tied pair has four copies over the 100ms gap to the next request,
	// round by round: copy 0 of both, then copy 1 of both. Pass period: span
	// 100 + mean 50 = 150, so the last request has a 50ms gap.
	assertDues(t, got, []time.Duration{0, ms(25), ms(50), ms(75), ms(100), ms(125)})
}

func TestTimelineAmplifyAtSpeed(t *testing.T) {
	cfg := tlCfg(func(c *model.ReplayConfig) { c.Amplify = 5; c.Speed = 10 })
	g := newTimelineGen(itemsAt([]int{0, 1000}), cfg)
	js := drain(g, cfg, 100)
	if len(js) != 10 {
		t.Fatalf("got %d jobs, want 10", len(js))
	}
	if d := js[5].due - ms(100); d < -time.Microsecond || d > time.Microsecond {
		t.Errorf("second request at %v, want 100ms (1s / 10)", js[5].due)
	}
	if d := js[1].due - ms(20); d < -time.Microsecond || d > time.Microsecond {
		t.Errorf("first request's second copy at %v, want 20ms", js[1].due)
	}
}

func TestSchedulesAreOrdered(t *testing.T) {
	// Whatever the knobs, dues never go backwards.
	offsets := []int{0, 0, 3, 7, 7, 7, 40, 41, 900}
	for _, amp := range []int{1, 3} {
		for _, k := range []float64{1, 0.5, 4.5} {
			for _, loop := range []bool{false, true} {
				cfg := tlCfg(func(c *model.ReplayConfig) {
					c.Amplify, c.ErrorAmplification, c.Loop, c.Seed = amp, k, loop, 7
					c.MaxRequests = 500
				})
				g := newTimelineGen(itemsAt(offsets, 2, 5, 8), cfg)
				got := dues(drain(g, cfg, 1000))
				if !slices.IsSorted(got) {
					t.Fatalf("amp=%d k=%v loop=%v: dues not sorted: %v", amp, k, loop, got)
				}
				if !loop && len(got) == 0 {
					t.Fatalf("amp=%d k=%v: no jobs", amp, k)
				}
			}
		}
	}
}

func TestErrorAmplificationWhole(t *testing.T) {
	// 1000 requests, every 10th an error. k=10 sends each error 10 times.
	offsets := make([]int, 1000)
	var errs []int
	for i := range offsets {
		offsets[i] = i
		if i%10 == 0 {
			errs = append(errs, i)
		}
	}
	cfg := tlCfg(func(c *model.ReplayConfig) { c.ErrorAmplification = 10; c.Seed = 1 })
	js := drain(newTimelineGen(itemsAt(offsets, errs...), cfg), cfg, 100000)
	var nErr, nOK int
	for _, j := range js {
		if j.it.isErr() {
			nErr++
		} else {
			nOK++
		}
	}
	if nOK != 900 || nErr != 1000 {
		t.Errorf("ok=%d err=%d, want 900 and 1000 (100 errors x 10)", nOK, nErr)
	}
	// ~10x as frequent: the error share went from 10% to ~53%.
	if share := float64(nErr) / float64(len(js)); math.Abs(share-1000.0/1900) > 1e-9 {
		t.Errorf("error share = %v", share)
	}
}

func TestErrorAmplificationFractional(t *testing.T) {
	const n, nErr = 2000, 1000
	offsets := make([]int, n)
	var errs []int
	for i := range offsets {
		offsets[i] = i
		if i%2 == 0 {
			errs = append(errs, i)
		}
	}
	count := func(k float64, seed uint64) (errCopies, okCopies int, per map[*item]int) {
		cfg := tlCfg(func(c *model.ReplayConfig) { c.ErrorAmplification = k; c.Seed = seed })
		per = map[*item]int{}
		for _, j := range drain(newTimelineGen(itemsAt(offsets, errs...), cfg), cfg, 1_000_000) {
			per[j.it]++
			if j.it.isErr() {
				errCopies++
			} else {
				okCopies++
			}
		}
		return
	}
	tests := []struct {
		k        float64
		min, max int // allowed copies per error item
		mean     float64
	}{
		{0.5, 0, 1, 0.5},
		{0.1, 0, 1, 0.1},
		{2.5, 2, 3, 2.5},
		{1, 1, 1, 1},
	}
	for _, tt := range tests {
		errCopies, okCopies, per := count(tt.k, 42)
		if okCopies != n-nErr {
			t.Errorf("k=%v: ok requests = %d, want %d (never thinned)", tt.k, okCopies, n-nErr)
		}
		for it, c := range per {
			if it.isErr() && (c < tt.min || c > tt.max) {
				t.Fatalf("k=%v: an error item was scheduled %d times", tt.k, c)
			}
		}
		// Binomial with n=1000: 5 sigma is well inside 5% of the mean here.
		mean := float64(errCopies) / nErr
		if math.Abs(mean-tt.mean) > 0.06 {
			t.Errorf("k=%v: mean copies per error = %.3f, want ~%v", tt.k, mean, tt.mean)
		}
	}
	// Same seed, same schedule; another seed samples differently.
	a1, _, _ := count(0.5, 9)
	a2, _, _ := count(0.5, 9)
	b, _, _ := count(0.5, 10)
	if a1 != a2 {
		t.Errorf("the same seed gave %d and %d", a1, a2)
	}
	if a1 == b {
		t.Logf("seeds 9 and 10 happen to agree (%d); not an error", a1)
	}
}

func TestSeededScheduleIsReproducible(t *testing.T) {
	cfg := tlCfg(func(c *model.ReplayConfig) { c.ErrorAmplification = 1.7; c.Seed = 123; c.Amplify = 2 })
	p := itemsAt([]int{0, 5, 9, 20, 50, 51, 52, 90}, 1, 3, 4, 7)
	run := func() []time.Duration { return dues(drain(newTimelineGen(p, cfg), cfg, 10000)) }
	a, b := run(), run()
	if !slices.Equal(a, b) {
		t.Fatal("the same config and recording gave different schedules")
	}
	cfg.Seed = 124
	if c := run(); slices.Equal(a, c) {
		t.Log("seed 123 and 124 gave the same schedule (possible but unlikely)")
	}
}

func TestPlannedIsExactForFiniteSchedules(t *testing.T) {
	offsets := []int{0, 10, 10, 35, 60, 61, 200, 450}
	p := itemsAt(offsets, 1, 4, 6)
	tests := []struct {
		name string
		mod  func(*model.ReplayConfig)
	}{
		{"plain", nil},
		{"amplify", func(c *model.ReplayConfig) { c.Amplify = 7 }},
		{"error amp whole", func(c *model.ReplayConfig) { c.ErrorAmplification = 3 }},
		{"error amp fractional", func(c *model.ReplayConfig) { c.ErrorAmplification = 0.4; c.Seed = 5 }},
		{"both", func(c *model.ReplayConfig) { c.ErrorAmplification = 2.5; c.Amplify = 3; c.Seed = 99 }},
		{"speed", func(c *model.ReplayConfig) { c.Speed = 4; c.Amplify = 2 }},
		{"duration cuts the pass", func(c *model.ReplayConfig) {
			c.Duration = model.Duration(100 * time.Millisecond)
			c.Amplify = 5
		}},
		{"duration cuts mid-copies", func(c *model.ReplayConfig) {
			c.Duration = model.Duration(37 * time.Millisecond)
			c.Amplify = 6
			c.ErrorAmplification = 2
		}},
		{"duration beyond the pass", func(c *model.ReplayConfig) { c.Duration = model.Duration(time.Hour) }},
		{"max requests caps", func(c *model.ReplayConfig) { c.Amplify = 5; c.MaxRequests = 17 }},
		{"max requests above", func(c *model.ReplayConfig) { c.MaxRequests = 1000 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tlCfg(tt.mod)
			g, planned, _ := newGenerator(p, cfg)
			got := int64(len(drain(g, cfg, 1_000_000)))
			if planned != got {
				t.Errorf("Planned = %d, the schedule has %d requests", planned, got)
			}
		})
	}
}

func TestPlannedForLoopsAndOtherModes(t *testing.T) {
	p := itemsAt([]int{0, 100, 200, 300}) // pass = 300 + 100 = 400ms, 4 requests
	tests := []struct {
		name       string
		cfg        model.ReplayConfig
		wantPlan   int64
		wantTarget float64
	}{
		{"loop with duration estimates from the pass rate",
			tlCfg(func(c *model.ReplayConfig) { c.Loop = true; c.Duration = model.Duration(4 * time.Second) }),
			40, 10},
		{"loop with duration at 2x",
			tlCfg(func(c *model.ReplayConfig) { c.Loop = true; c.Speed = 2; c.Duration = model.Duration(4 * time.Second) }),
			80, 20},
		{"loop with max requests",
			tlCfg(func(c *model.ReplayConfig) { c.Loop = true; c.MaxRequests = 123 }),
			123, 10},
		{"loop with both is capped",
			tlCfg(func(c *model.ReplayConfig) {
				c.Loop = true
				c.Duration = model.Duration(4 * time.Second)
				c.MaxRequests = 25
			}),
			25, 10},
		{"max mode single pass", model.ReplayConfig{Mode: model.ReplayMax}.WithDefaults(), 4, 0},
		{"max mode capped", model.ReplayConfig{Mode: model.ReplayMax, MaxRequests: 3}.WithDefaults(), 3, 0},
		{"max mode loop unknown", model.ReplayConfig{Mode: model.ReplayMax, Loop: true, Duration: model.Duration(time.Second)}.WithDefaults(), -1, 0},
		{"max mode loop with cap", model.ReplayConfig{Mode: model.ReplayMax, Loop: true, MaxRequests: 77}.WithDefaults(), 77, 0},
		{"rate constant", model.ReplayConfig{Mode: model.ReplayRate, Rate: 50, Duration: model.Duration(3 * time.Second)}.WithDefaults(), 150, 50},
		{"rate capped by max requests", model.ReplayConfig{Mode: model.ReplayRate, Rate: 50, Duration: model.Duration(3 * time.Second), MaxRequests: 20}.WithDefaults(), 20, 50},
		{"rate until max requests", model.ReplayConfig{Mode: model.ReplayRate, Rate: 50, MaxRequests: 500}.WithDefaults(), 500, 50},
		{"rate ramp", model.ReplayConfig{Mode: model.ReplayRate, Stages: []model.ReplayStage{stage(10*time.Second, 100, false)}}.WithDefaults(), 500, 50},
		{"rate stages then longer duration",
			model.ReplayConfig{Mode: model.ReplayRate, Duration: model.Duration(4 * time.Second),
				Stages: []model.ReplayStage{stage(2*time.Second, 100, true)}}.WithDefaults(), 400, 100},
		{"rate duration shorter than stages",
			model.ReplayConfig{Mode: model.ReplayRate, Duration: model.Duration(5 * time.Second),
				Stages: []model.ReplayStage{stage(10*time.Second, 100, false)}}.WithDefaults(), 125, 25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, planned, target := newGenerator(p, tt.cfg)
			if planned != tt.wantPlan {
				t.Errorf("Planned = %d, want %d", planned, tt.wantPlan)
			}
			if math.Abs(target-tt.wantTarget) > 1e-6 {
				t.Errorf("TargetRPS = %v, want %v", target, tt.wantTarget)
			}
		})
	}
}

func rateCfg(rate float64, d time.Duration, mod func(*model.ReplayConfig)) model.ReplayConfig {
	cfg := model.ReplayConfig{Mode: model.ReplayRate, Rate: rate, Duration: model.Duration(d)}.WithDefaults()
	if mod != nil {
		mod(&cfg)
	}
	return cfg
}

func TestRateModeConstantCount(t *testing.T) {
	p := itemsAt([]int{0, 1, 2, 3, 4})
	tests := []struct {
		rate float64
		d    time.Duration
		want int
	}{
		{100, 2 * time.Second, 200},
		{1, 10 * time.Second, 10},
		{0.5, 10 * time.Second, 5},
		{2500, 400 * time.Millisecond, 1000},
		{10000, time.Second, 10000},
	}
	for _, tt := range tests {
		cfg := rateCfg(tt.rate, tt.d, nil)
		g, planned, _ := newGenerator(p, cfg)
		js := drain(g, cfg, 100000)
		if len(js) != tt.want || planned != int64(tt.want) {
			t.Errorf("rate %v for %v: %d requests (planned %d), want %d", tt.rate, tt.d, len(js), planned, tt.want)
		}
		if last := js[len(js)-1].due; last >= tt.d {
			t.Errorf("rate %v: last request due at %v, not before %v", tt.rate, last, tt.d)
		}
		if g.end() != tt.d {
			t.Errorf("end() = %v, want %v", g.end(), tt.d)
		}
	}
}

func TestRateModeCyclesItemsInTimeOrder(t *testing.T) {
	p := itemsAt([]int{0, 10, 20})
	for i := range p.items {
		p.items[i].uri = "/" + string(rune('a'+i))
	}
	cfg := rateCfg(100, time.Second, nil)
	var got []string
	for _, j := range drain(newRateGen(p, cfg), cfg, 8) {
		got = append(got, j.it.uri)
	}
	want := []string{"/a", "/b", "/c", "/a", "/b", "/c", "/a", "/b"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRateModeStagesIntegrate(t *testing.T) {
	p := itemsAt([]int{0, 1})
	tests := []struct {
		name   string
		stages []model.ReplayStage
		dur    time.Duration
		want   int
	}{
		{"ramp to 200 over 10s", []model.ReplayStage{stage(10*time.Second, 200, false)}, 0, 1000},
		{"ramp then hold", []model.ReplayStage{stage(5*time.Second, 100, false), stage(5*time.Second, 100, true)}, 0, 250 + 500},
		{"hold step", []model.ReplayStage{stage(2*time.Second, 300, true)}, 0, 600},
		{"duration longer holds the last rate", []model.ReplayStage{stage(2*time.Second, 100, true)}, 5 * time.Second, 500},
		{"duration shorter cuts the ramp", []model.ReplayStage{stage(10*time.Second, 100, false)}, 4 * time.Second, 80},
		{"up and down", []model.ReplayStage{stage(4*time.Second, 100, false), stage(4*time.Second, 0, false)}, 0, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := model.ReplayConfig{Mode: model.ReplayRate, Stages: tt.stages, Duration: model.Duration(tt.dur)}.WithDefaults()
			g, planned, _ := newGenerator(p, cfg)
			js := drain(g, cfg, 1_000_000)
			if abs(len(js)-tt.want) > 1 {
				t.Errorf("%d requests, want %d (area under the curve)", len(js), tt.want)
			}
			if planned != int64(len(js)) {
				t.Errorf("Planned = %d, schedule has %d", planned, len(js))
			}
			if !slices.IsSorted(dues(js)) {
				t.Error("dues not sorted")
			}
		})
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func TestRateModeQuietTailStillEndsAtStageEnd(t *testing.T) {
	cfg := model.ReplayConfig{Mode: model.ReplayRate,
		Stages: []model.ReplayStage{stage(time.Second, 10, true), stage(2*time.Second, 0, true)}}.WithDefaults()
	g := newRateGen(itemsAt([]int{0}), cfg)
	js := drain(g, cfg, 1000)
	if len(js) != 10 {
		t.Errorf("%d requests, want 10", len(js))
	}
	if g.end() != 3*time.Second {
		t.Errorf("end() = %v, want 3s so the job lasts through the quiet stage", g.end())
	}
}

func TestRateModeErrorAmplificationReshapesTheMix(t *testing.T) {
	// Rate mode consumes the sequence at the same rate: the request count is
	// unchanged, but errors take a larger share.
	p := itemsAt([]int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, 0)
	base := rateCfg(100, time.Second, nil)
	amp := rateCfg(100, time.Second, func(c *model.ReplayConfig) { c.ErrorAmplification = 10 })
	count := func(cfg model.ReplayConfig) (total, errs int) {
		for _, j := range drain(newRateGen(p, cfg), cfg, 10000) {
			total++
			if j.it.isErr() {
				errs++
			}
		}
		return
	}
	t0, e0 := count(base)
	t1, e1 := count(amp)
	if t0 != 100 || t1 != 100 {
		t.Fatalf("request counts %d and %d, want 100 (rate is the total)", t0, t1)
	}
	if e0 != 10 || e1 < 50 {
		t.Errorf("errors: baseline %d (want 10), amplified %d (want a majority)", e0, e1)
	}
}

func TestMaxModeSinglePassAndLoop(t *testing.T) {
	p := itemsAt([]int{0, 1, 2, 3, 4}, 2)
	cfg := model.ReplayConfig{Mode: model.ReplayMax}.WithDefaults()
	js := drain(newMaxGen(p, cfg), cfg, 100)
	if len(js) != 5 {
		t.Errorf("single pass: %d requests, want 5", len(js))
	}
	for _, j := range js {
		if j.due >= 0 {
			t.Fatalf("closed-model job has due %v, want < 0", j.due)
		}
	}
	cfg = model.ReplayConfig{Mode: model.ReplayMax, Loop: true, MaxRequests: 23}.WithDefaults()
	if got := len(drain(newMaxGen(p, cfg), cfg, 1000)); got != 23 {
		t.Errorf("loop with MaxRequests: %d requests, want 23", got)
	}
	// Error amplification widens a pass.
	cfg = model.ReplayConfig{Mode: model.ReplayMax, ErrorAmplification: 4}.WithDefaults()
	if got := len(drain(newMaxGen(p, cfg), cfg, 1000)); got != 8 {
		t.Errorf("error amplification 4: %d requests, want 4 + 4 = 8", got)
	}
}

func TestCyclerForcesProgressWhenEverythingIsSampledOut(t *testing.T) {
	p := itemsAt([]int{0, 1}, 0, 1)
	c := newCycler(p.items, 1e-12, 1, true)
	for i := 0; i < 3; i++ {
		if _, ok := c.next(); !ok {
			t.Fatal("cycler stopped")
		}
	}
}

func TestMaxGapShortensIdleTimeBeforeSpeed(t *testing.T) {
	// An overnight gap in a capture: 0, 10ms, 3h, 3h+20ms.
	l := mustLoader(t, model.ReplayConfig{Mode: model.ReplayTimeline}, "http://t", true)
	for _, at := range []time.Duration{0, 10 * time.Millisecond, 3 * time.Hour, 3*time.Hour + 20*time.Millisecond} {
		if err := l.add(rx{at: at}.exchange()); err != nil {
			t.Fatal(err)
		}
	}
	p, err := l.finish(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var got []time.Duration
	for _, it := range p.items {
		got = append(got, it.off)
	}
	want := []time.Duration{0, ms(10), ms(10) + 5*time.Second, ms(30) + 5*time.Second}
	if !slices.Equal(got, want) {
		t.Errorf("offsets = %v, want %v", got, want)
	}
	cfg := tlCfg(func(c *model.ReplayConfig) { c.Speed = 5 })
	js := drain(newTimelineGen(p, cfg), cfg, 10)
	if d := js[3].due - (ms(30)+5*time.Second)/5; d < -time.Microsecond || d > time.Microsecond {
		t.Errorf("last request due %v, want (30ms+5s)/5", js[3].due)
	}
}

func BenchmarkTimelineGen(b *testing.B) {
	offsets := make([]int, 10000)
	for i := range offsets {
		offsets[i] = i * 3
	}
	p := itemsAt(offsets, 5, 50, 500)
	cfg := tlCfg(func(c *model.ReplayConfig) { c.Loop = true; c.Amplify = 3; c.ErrorAmplification = 2.5; c.Seed = 1 })
	g := newTimelineGen(p, cfg)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := g.next(); !ok {
			b.Fatal("ended")
		}
	}
}

func BenchmarkRateGen(b *testing.B) {
	p := itemsAt([]int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, 3)
	cfg := model.ReplayConfig{Mode: model.ReplayRate, ErrorAmplification: 2.5, Seed: 1,
		Stages: []model.ReplayStage{stage(time.Hour, 20000, false)}, Duration: model.Duration(24 * time.Hour)}.WithDefaults()
	g := newRateGen(p, cfg)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := g.next(); !ok {
			b.Fatal("ended")
		}
	}
}

func BenchmarkMaxGen(b *testing.B) {
	p := itemsAt([]int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, 3)
	cfg := model.ReplayConfig{Mode: model.ReplayMax, Loop: true, MaxRequests: 1}.WithDefaults()
	g := newMaxGen(p, cfg)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := g.next(); !ok {
			b.Fatal("ended")
		}
	}
}
