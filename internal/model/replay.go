package model

import (
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ReplayMode is how a replay job schedules requests.
type ReplayMode string

const (
	// ReplayTimeline reproduces the recorded inter-arrival times, compressed
	// by Speed (Speed 10 = the same traffic shape at 10x the rate).
	ReplayTimeline ReplayMode = "timeline"
	// ReplayRate sends at a fixed arrival rate (Rate req/s) or along Stages,
	// cycling through the recording in order. Open model: arrivals do not
	// wait for responses.
	ReplayRate ReplayMode = "rate"
	// ReplayMax sends as fast as Concurrency workers allow (closed model).
	ReplayMax ReplayMode = "max"
)

// ReplayStage ramps the arrival rate linearly from the previous stage's
// rate (or 0) to Rate over Duration, or with Hold steps to Rate at once and
// holds it for Duration.
type ReplayStage struct {
	Duration Duration `json:"duration"`
	Rate     float64  `json:"rate"`
	Hold     bool     `json:"hold,omitempty"`
}

// ReplayConfig configures a replay (load test) job.
type ReplayConfig struct {
	RecordingID string `json:"recording_id"`
	// Target is the base URL requests are sent to; each recorded request's
	// path and query are appended ("http://staging:8080" + "/users/42?x=1").
	// A path on the target is used as a prefix.
	Target string     `json:"target"`
	Mode   ReplayMode `json:"mode"`

	// Speed compresses time in timeline mode (default 1).
	Speed float64 `json:"speed,omitempty"`
	// MaxGap (timeline) shortens idle gaps in the recording longer than
	// MaxGap to MaxGap before Speed is applied, so overnight lulls in a
	// capture do not stall a replay. 0 = keep gaps.
	MaxGap Duration `json:"max_gap,omitempty"`
	// Rate is the constant arrival rate in rate mode when Stages is empty.
	Rate   float64       `json:"rate,omitempty"`
	Stages []ReplayStage `json:"stages,omitempty"`

	// Amplify (timeline mode only) sends each scheduled request this many
	// times (default 1), so Amplify 10 is ten times the recorded traffic.
	// Copies are spread evenly across the gap to the next request instead of
	// firing in a burst. In rate mode the rate already is the total send
	// rate, so Validate rejects Amplify > 1 there and in max mode.
	Amplify int `json:"amplify,omitempty"`
	// ErrorAmplification oversamples requests whose recorded response was an
	// error (see IsErrorStatus): each is scheduled floor(k) times plus once
	// more with probability frac(k), so 10 makes them ~10x as frequent and
	// 0.5 halves them. In timeline mode the extra copies add load; in rate
	// and max modes the sequence is consumed at the same rate/concurrency,
	// so only the share of error-inducing traffic changes. Default 1.
	ErrorAmplification float64 `json:"error_amplification,omitempty"`

	// Concurrency caps in-flight requests (default 2048 for timeline and
	// rate, so the open model is not silently throttled; 64 for max). In max
	// mode it is the number of workers.
	Concurrency int `json:"concurrency,omitempty"`
	// Duration stops the job after this long. 0 = one pass through the
	// recording (timeline, max). Required for rate mode unless Loop is false
	// and MaxRequests is set.
	Duration Duration `json:"duration,omitempty"`
	// Loop (timeline and max) repeats the recording until Duration or
	// MaxRequests is reached. Rate mode always cycles the recording until its
	// stages, Duration or MaxRequests end.
	Loop bool `json:"loop,omitempty"`
	// MaxRequests stops after this many requests (0 = no cap).
	MaxRequests int64 `json:"max_requests,omitempty"`
	// Timeout per request (default 30s).
	Timeout Duration `json:"timeout,omitempty"`

	// Include/Exclude filter requests by RE2 regex on "METHOD /path?query".
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`

	// SetHeaders are set on every request (e.g. an Authorization header that
	// is valid for the target environment). RemoveHeaders are deleted.
	SetHeaders    map[string]string `json:"set_headers,omitempty"`
	RemoveHeaders []string          `json:"remove_headers,omitempty"`
	// HostHeader overrides the Host header (default: the target's host).
	HostHeader         string `json:"host_header,omitempty"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`

	// ProfileID anonymizes every request on the fly with this profile before
	// sending ("" = send as stored, which is already anonymized if the
	// recording was captured with anonymization on). Leave it empty when the
	// target is a Mirage virtual service built from the same anonymized
	// recording, or requests will no longer match.
	ProfileID string `json:"profile_id,omitempty"`
	// VaryClones (with Amplify > 1 and a ProfileID) re-keys anonymization per
	// copy so each copy carries different but consistent identities, instead
	// of N identical requests hammering the same keys.
	VaryClones bool `json:"vary_clones,omitempty"`
	// CompareStatus counts responses whose status differs from the recording.
	CompareStatus bool `json:"compare_status,omitempty"`
	// Seed makes the run reproducible (error amplification sampling, any
	// randomness). 0 = Create picks a random seed and stores it, so a
	// re-run or clone reproduces the same schedule.
	Seed uint64 `json:"seed,omitempty"`
}

// Defaults for zero-valued replay fields.
const (
	DefaultReplayConcurrency    = 2048 // timeline and rate
	DefaultReplayMaxConcurrency = 64   // max mode workers
	DefaultReplayTimeout        = 30 * time.Second
	MaxReplayConcurrency        = 100000
	// MaxReplaySeries is the most per-second points a job keeps; longer runs
	// merge adjacent points (halving resolution) to stay under it.
	MaxReplaySeries = 7200
)

// WithDefaults returns a copy with zero-valued fields defaulted.
func (c ReplayConfig) WithDefaults() ReplayConfig {
	if c.Mode == "" {
		c.Mode = ReplayTimeline
	}
	if c.Speed == 0 {
		c.Speed = 1
	}
	if c.Amplify == 0 {
		c.Amplify = 1
	}
	if c.ErrorAmplification == 0 {
		c.ErrorAmplification = 1
	}
	if c.Concurrency == 0 {
		c.Concurrency = DefaultReplayConcurrency
		if c.Mode == ReplayMax {
			c.Concurrency = DefaultReplayMaxConcurrency
		}
	}
	if c.Timeout == 0 {
		c.Timeout = Duration(DefaultReplayTimeout)
	}
	return c
}

// Validate checks the config (after WithDefaults).
func (c ReplayConfig) Validate() error {
	var p problems
	if c.RecordingID == "" {
		p.add("recording_id is required")
	}
	u, err := url.Parse(c.Target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		p.add("target %q must be an absolute http(s) URL", c.Target)
	}
	switch c.Mode {
	case ReplayTimeline:
		if c.Speed <= 0 || c.Speed > 100000 {
			p.add("speed must be > 0 and <= 100000")
		}
	case ReplayRate:
		if len(c.Stages) == 0 && c.Rate <= 0 {
			p.add("rate mode needs rate > 0 or stages")
		}
		for i, s := range c.Stages {
			if s.Duration <= 0 || s.Rate < 0 {
				p.add("stage %d needs duration > 0 and rate >= 0", i+1)
			}
		}
		if c.Duration == 0 && len(c.Stages) == 0 && c.MaxRequests == 0 {
			p.add("rate mode needs duration, stages or max_requests")
		}
	case ReplayMax:
	default:
		p.add("mode %q must be timeline, rate or max", c.Mode)
	}
	if c.Amplify < 1 || c.Amplify > 1000 {
		p.add("amplify must be between 1 and 1000")
	}
	if c.Amplify > 1 && c.Mode != ReplayTimeline {
		p.add("amplify applies to timeline mode only (in rate mode raise the rate instead)")
	}
	if c.MaxGap < 0 {
		p.add("max_gap must not be negative")
	}
	if c.ErrorAmplification <= 0 || c.ErrorAmplification > 1000 {
		p.add("error_amplification must be > 0 and <= 1000")
	}
	if c.Concurrency < 1 || c.Concurrency > MaxReplayConcurrency {
		p.add("concurrency must be between 1 and %d", MaxReplayConcurrency)
	}
	if c.Duration < 0 || c.Timeout < 0 || c.MaxRequests < 0 {
		p.add("duration, timeout and max_requests must not be negative")
	}
	if c.Loop && c.Duration == 0 && c.MaxRequests == 0 {
		p.add("loop needs duration or max_requests")
	}
	if c.VaryClones && c.ProfileID == "" {
		p.add("vary_clones needs profile_id")
	}
	for _, re := range append(append([]string{}, c.Include...), c.Exclude...) {
		if _, err := regexp.Compile(re); err != nil {
			p.add("filter %q: %v", re, err)
		}
	}
	for k := range c.SetHeaders {
		if strings.TrimSpace(k) == "" {
			p.add("set_headers has an empty header name")
		}
	}
	return p.err()
}

// ReplayStatus is the lifecycle state of a replay job.
type ReplayStatus string

const (
	ReplayPending   ReplayStatus = "pending"
	ReplayRunning   ReplayStatus = "running"
	ReplayCompleted ReplayStatus = "completed"
	ReplayStopped   ReplayStatus = "stopped"
	ReplayFailed    ReplayStatus = "failed"
)

// Done reports whether the status is terminal.
func (s ReplayStatus) Done() bool {
	return s == ReplayCompleted || s == ReplayStopped || s == ReplayFailed
}

// ReplayJob is a replay run and, once finished, its results.
type ReplayJob struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Config    ReplayConfig `json:"config"`
	Status    ReplayStatus `json:"status"`
	Error     string       `json:"error,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
	StartedAt *time.Time   `json:"started_at,omitempty"`
	EndedAt   *time.Time   `json:"ended_at,omitempty"`
	// Summary is live while running and final once done.
	Summary *ReplaySummary `json:"summary,omitempty"`
	// Series holds one point per second of the run. List endpoints omit it.
	Series []SeriesPoint `json:"series,omitempty"`
}

// ReplaySummary is the result of a replay run.
type ReplaySummary struct {
	// Planned is the number of requests the schedule will send: exact for a
	// single timeline/max pass (sum over items of copies × Amplify, with
	// error items counted k times) and capped by MaxRequests; for rate mode
	// the integral of the rate over the run; for duration-bounded loops an
	// estimate from the pass rate; -1 when unknown.
	Planned   int64 `json:"planned"`
	Sent      int64 `json:"sent"`
	Completed int64 `json:"completed"` // got a response (any status)
	Failed    int64 `json:"failed"`    // transport errors (no response)
	Timeouts  int64 `json:"timeouts"`  // subset of Failed
	Errors    int64 `json:"errors"`    // Failed + error statuses (IsErrorStatus)
	// Mismatches counts responses whose status differs from the recording
	// (only with CompareStatus).
	Mismatches int64 `json:"mismatches"`
	// StatusCounts maps status code (as a string) to count.
	StatusCounts map[string]int64 `json:"status_counts"`
	// Latency is service time: request sent → response body fully read.
	Latency LatencySummary `json:"latency"`
	// CorrectedLatency is measured from each request's intended send time, so
	// it includes time spent waiting for a free connection slot when the
	// target falls behind (corrects coordinated omission).
	CorrectedLatency LatencySummary `json:"corrected_latency"`
	TargetRPS        float64        `json:"target_rps"` // scheduled average (0 for max mode)
	AchievedRPS      float64        `json:"achieved_rps"`
	ErrorRate        float64        `json:"error_rate"`
	BytesSent        int64          `json:"bytes_sent"`
	BytesReceived    int64          `json:"bytes_received"`
	Elapsed          Duration       `json:"elapsed"`
	// MaxLag is the worst observed delay between a request's intended and
	// actual send time (scheduler or concurrency saturation).
	MaxLag Duration `json:"max_lag"`
	// MaxInFlight is the peak number of concurrent requests. Saturated is
	// true when every Concurrency slot was busy at least once, so lag and
	// corrected latency include client-side queueing: raise Concurrency.
	MaxInFlight int64             `json:"max_in_flight"`
	Saturated   bool              `json:"saturated"`
	Endpoints   []EndpointSummary `json:"endpoints"`
}
