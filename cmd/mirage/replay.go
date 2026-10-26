package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/justinburger/mirage/internal/metrics"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/ops"
	"github.com/justinburger/mirage/internal/replay"
	"github.com/justinburger/mirage/internal/store"
)

// stageFlag is the repeatable --stage DURATION:RATE[:hold] flag.
type stageFlag []model.ReplayStage

func (st *stageFlag) String() string {
	parts := make([]string, len(*st))
	for i, s := range *st {
		parts[i] = fmt.Sprintf("%s:%g", s.Duration, s.Rate)
		if s.Hold {
			parts[i] += ":hold"
		}
	}
	return strings.Join(parts, ", ")
}

func (st *stageFlag) Set(v string) error {
	parts := strings.Split(v, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return fmt.Errorf("%q is not DURATION:RATE (for example 30s:500 or 2m:2000:hold)", v)
	}
	d, err := time.ParseDuration(strings.TrimSpace(parts[0]))
	if err != nil || d <= 0 {
		return fmt.Errorf("%q: %q is not a positive duration", v, parts[0])
	}
	rate, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil || rate < 0 {
		return fmt.Errorf("%q: %q is not a rate (requests per second)", v, parts[1])
	}
	hold := false
	if len(parts) == 3 {
		if strings.TrimSpace(parts[2]) != "hold" {
			return fmt.Errorf("%q: the optional third field must be \"hold\"", v)
		}
		hold = true
	}
	*st = append(*st, model.ReplayStage{Duration: model.Duration(d), Rate: rate, Hold: hold})
	return nil
}

// headerFlag is the repeatable --header "K: V" flag.
type headerFlag map[string]string

func (h *headerFlag) String() string { return fmt.Sprint(map[string]string(*h)) }
func (h *headerFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, ":")
	k = strings.TrimSpace(k)
	if !ok || k == "" {
		return fmt.Errorf("%q is not in the form \"Name: value\"", v)
	}
	if *h == nil {
		*h = map[string]string{}
	}
	(*h)[k] = strings.TrimSpace(val)
	return nil
}

func runReplay(ctx context.Context, args []string, s streams) int {
	var (
		dataPath, recRef, harPath string
		target, mode              string
		speed, rate, errAmp       float64
		maxGap                    time.Duration
		amplify, concurrency      int
		duration, timeout         time.Duration
		loop, varyClones          bool
		compareStatus, insecure   bool
		maxRequests               int64
		seed                      uint64
		hostHeader, profileRef    string
		jsonPath                  string
		maxErrRate                float64
		maxP99                    time.Duration
		quiet                     bool
		stages                    stageFlag
		headers                   headerFlag
		removeHeaders             multiFlag
		include, exclude          multiFlag
	)
	fs := newFlagSet("replay",
		"replay a recording against a target as a load test (headless, for CI)",
		[]string{"--data DB --recording ID|NAME --target URL [flags]", "--har FILE --target URL [flags]"},
		[]flagGroup{
			{"Source (one of)", []string{"data", "recording", "har"}},
			{"Target", []string{"target", "header", "remove-header", "host-header", "insecure", "timeout"}},
			{"Schedule", []string{"mode", "speed", "max-gap", "rate", "stage", "amplify", "error-amplification", "concurrency", "duration", "loop", "max-requests", "seed"}},
			{"Selection and fidelity", []string{"include", "exclude", "profile", "vary-clones", "compare-status"}},
			{"Output and CI gating", []string{"json", "quiet", "max-error-rate", "max-p99"}},
		},
		[]string{
			"mirage replay --data mirage.db --recording checkout --target http://staging:8080",
			"    one pass through the recording at its original pace",
			"mirage replay --data mirage.db --recording checkout --target http://staging:8080 --speed 10 --error-amplification 5",
			"    the same traffic shape at 10x the rate, with 5x as many error-inducing requests",
			"mirage replay --har session.har --target http://localhost:9001 --stage 30s:100 --stage 2m:1000:hold --duration 3m",
			"    ramp from 0 to 100 rps over 30s, step to 1000 rps and hold it (rate mode is implied)",
			"mirage replay --data mirage.db --recording checkout --target http://staging:8080 --mode max --concurrency 128 --duration 30s --loop \\",
			"    --json result.json --max-error-rate 0.01 --max-p99 250ms",
			"    closed-loop throughput test; exit status 1 when the error rate exceeds 1% or p99 exceeds 250ms,",
			"    and always when no request got a response (target down), even without thresholds",
		}, s)
	fs.StringVar(&dataPath, "data", "mirage.db", "FILE|SQLite database holding the recording")
	fs.StringVar(&recRef, "recording", "", "ID|recording id or name to replay (with --data)")
	fs.StringVar(&harPath, "har", "", "FILE|replay a HAR file instead of a stored recording (it is read into memory; nothing is written)")
	fs.StringVar(&target, "target", "", "URL|base URL to send requests to, for example http://staging:8080 (required)")
	fs.StringVar(&mode, "mode", "", "MODE|timeline (recorded pace times --speed), rate (fixed or staged arrival rate) or max (closed loop); inferred from --rate and --stage when left out, otherwise timeline")
	fs.Float64Var(&speed, "speed", 0, "FACTOR|timeline: compress time by this factor, so 10 is the same traffic shape at 10x the rate (default 1)")
	fs.DurationVar(&maxGap, "max-gap", 0, "DURATION|timeline: shorten idle gaps in the recording longer than this, so a quiet night does not stall the run")
	fs.Float64Var(&rate, "rate", 0, "RPS|rate mode: constant arrival rate in requests per second")
	fs.Var(&stages, "stage", "DURATION:RATE[:hold]|rate mode: a stage that ramps linearly from the previous rate to RATE over DURATION; with :hold it steps straight to RATE and holds it (repeatable)")
	fs.IntVar(&amplify, "amplify", 0, "N|timeline: send every request N times, spread across the gap to the next request (default 1)")
	fs.Float64Var(&errAmp, "error-amplification", 0, "FACTOR|send requests that recorded an error this many times as often; 0.5 halves them (default 1)")
	fs.IntVar(&concurrency, "concurrency", 0, "N|most requests in flight, or the number of workers in max mode (default 2048, 64 in max mode)")
	fs.DurationVar(&duration, "duration", 0, "DURATION|stop after this long (default: one pass through the recording; rate mode needs this, --stage or --max-requests)")
	fs.BoolVar(&loop, "loop", false, "repeat the recording until --duration or --max-requests is reached (timeline and max mode)")
	fs.Int64Var(&maxRequests, "max-requests", 0, "N|stop after sending this many requests")
	fs.Uint64Var(&seed, "seed", 0, "N|random seed, for a reproducible schedule (default: random)")
	fs.DurationVar(&timeout, "timeout", 0, "DURATION|per-request timeout (default 30s)")
	fs.Var(&headers, "header", "\"NAME: VALUE\"|set a header on every request, for example an Authorization header valid for the target (repeatable)")
	fs.Var(&removeHeaders, "remove-header", "NAME|remove this header from every request (repeatable)")
	fs.StringVar(&hostHeader, "host-header", "", "HOST|override the Host header (default: the target's host)")
	fs.Var(&include, "include", "REGEX|only replay requests matching this expression on \"METHOD /path?query\" (repeatable)")
	fs.Var(&exclude, "exclude", "REGEX|skip requests matching this expression on \"METHOD /path?query\" (repeatable)")
	fs.StringVar(&profileRef, "profile", "", "ID|anonymize every request on the fly with this profile (id or name) before sending")
	fs.BoolVar(&varyClones, "vary-clones", false, "with --amplify and --profile: give every copy different but consistent identities")
	fs.BoolVar(&compareStatus, "compare-status", false, "count responses whose status differs from the recording")
	fs.BoolVar(&insecure, "insecure", false, "skip TLS certificate verification of the target")
	fs.StringVar(&jsonPath, "json", "", "FILE|write the final summary (the ReplaySummary of the API) as JSON to this file")
	fs.BoolVar(&quiet, "quiet", false, "no live progress line on stderr")
	fs.Float64Var(&maxErrRate, "max-error-rate", 0, "FRACTION|exit with status 1 when the error rate exceeds this fraction (0.01 is 1%)")
	fs.DurationVar(&maxP99, "max-p99", 0, "DURATION|exit with status 1 when p99 latency exceeds this; measured from each request's intended send time (coordinated-omission corrected)")
	if code, ok := parseFlags(fs, args, s); !ok {
		return code
	}
	set := explicitFlags(fs)

	if target == "" {
		return usageError(s, fs, "--target is required")
	}
	if (recRef == "") == (harPath == "") {
		return usageError(s, fs, "give the traffic to replay with --recording (and --data) or with --har")
	}
	if set["max-error-rate"] && (maxErrRate < 0 || maxErrRate > 1) {
		return usageError(s, fs, "--max-error-rate must be between 0 and 1")
	}
	if u, err := url.Parse(target); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return usageError(s, fs, "--target %q must be an absolute http(s) URL", target)
	}

	// Ctrl-C stops the run and still prints what was measured.
	sigCtx, stop := signalContext(ctx)
	defer stop()

	// Open the traffic source.
	var (
		st  store.Store
		rec model.Recording
		err error
	)
	if harPath != "" {
		mem, oerr := store.Open(store.MemoryPath)
		if oerr != nil {
			fmt.Fprintf(s.err, "mirage replay: %v\n", oerr)
			return exitFail
		}
		defer mem.Close()
		st = mem
		f, oerr := os.Open(harPath)
		if oerr != nil {
			fmt.Fprintf(s.err, "mirage replay: %v\n", oerr)
			return exitFail
		}
		rec, err = ops.ImportHAR(sigCtx, st, f, harPath, nil, nil)
		f.Close()
		if err != nil {
			fmt.Fprintf(s.err, "mirage replay: read %s: %v\n", harPath, err)
			return exitFail
		}
	} else {
		db, oerr := openExistingStore(dataPath)
		if oerr != nil {
			fmt.Fprintf(s.err, "mirage replay: %v\n", oerr)
			return exitFail
		}
		defer db.Close()
		st = db
		if rec, err = resolveRecording(sigCtx, st, recRef); err != nil {
			fmt.Fprintf(s.err, "mirage replay: %v\n", err)
			return exitFail
		}
	}
	profileID := ""
	if profileRef != "" {
		p, perr := resolveProfile(sigCtx, st, profileRef)
		if perr != nil {
			fmt.Fprintf(s.err, "mirage replay: %v\n", perr)
			return exitFail
		}
		profileID = p.ID
	}

	// Build and validate the configuration like the API does.
	cfg := model.ReplayConfig{
		RecordingID:        rec.ID,
		Target:             target,
		Mode:               model.ReplayMode(mode),
		Speed:              speed,
		MaxGap:             model.Duration(maxGap),
		Rate:               rate,
		Stages:             stages,
		Amplify:            amplify,
		ErrorAmplification: errAmp,
		Concurrency:        concurrency,
		Duration:           model.Duration(duration),
		Loop:               loop,
		MaxRequests:        maxRequests,
		Timeout:            model.Duration(timeout),
		Include:            include,
		Exclude:            exclude,
		SetHeaders:         headers,
		RemoveHeaders:      removeHeaders,
		HostHeader:         hostHeader,
		InsecureSkipVerify: insecure,
		ProfileID:          profileID,
		VaryClones:         varyClones,
		CompareStatus:      compareStatus,
		Seed:               seed,
	}
	if cfg.Mode == "" && (cfg.Rate > 0 || len(cfg.Stages) > 0) {
		cfg.Mode = model.ReplayRate
	}
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		var ve *model.ValidationError
		if errors.As(err, &ve) {
			fmt.Fprintln(s.err, "mirage replay: invalid options:")
			for _, p := range ve.Problems {
				fmt.Fprintf(s.err, "  - %s\n", p)
			}
			fmt.Fprintln(s.err, "Run 'mirage replay -h' for usage.")
			return exitUsage
		}
		fmt.Fprintf(s.err, "mirage replay: %v\n", err)
		return exitUsage
	}

	col := metrics.NewCollector(metrics.ReplayCollectorName("cli"), model.KindReplay, "cli")
	prog := newProgress(s.err, !quiet)
	if !quiet {
		fmt.Fprintf(s.err, "replaying %q (%s exchanges) against %s, mode %s\n", rec.Name, fmtInt(rec.ExchangeCount), target, cfg.Mode)
	}
	start := time.Now()
	sum, _, runErr := replay.Run(sigCtx, st, cfg, replay.RunOptions{
		Collector:     col,
		TickCollector: true,
		Progress:      prog.update,
		JobID:         "cli",
	})
	prog.finish()
	interrupted := sigCtx.Err() != nil && ctx.Err() == nil

	if runErr != nil && !(interrupted && errors.Is(runErr, context.Canceled)) {
		fmt.Fprintf(s.err, "mirage replay: %v\n", runErr)
		if sum.Sent == 0 {
			return exitFail
		}
	}

	printReplaySummary(s.out, rec, cfg, sum, time.Since(start))
	if interrupted {
		fmt.Fprintln(s.out, "\nInterrupted: the numbers above cover the part of the run that completed.")
	}

	if jsonPath != "" {
		if err := writeSummaryJSON(jsonPath, sum); err != nil {
			fmt.Fprintf(s.err, "mirage replay: write %s: %v\n", jsonPath, err)
			return exitFail
		}
	}

	failures := evaluateThresholds(s.out, sum, set["max-error-rate"], maxErrRate, maxP99, runErr != nil && !interrupted)
	// A run that sent requests but never got a single response (target down,
	// wrong port, DNS failure) measured nothing, so it must not look like a
	// success to a CI job that set no thresholds. Error statuses are
	// responses: only transport failures count here.
	noResponses := !interrupted && sum.Sent > 0 && sum.Completed == 0
	if noResponses {
		fmt.Fprintf(s.err, "mirage replay: every request failed: %s sent, none got a response from %s\n", fmtInt(sum.Sent), target)
	}
	switch {
	case runErr != nil && !interrupted:
		return exitFail
	case noResponses:
		return exitFail
	case len(failures) > 0:
		fmt.Fprintf(s.err, "mirage replay: thresholds exceeded: %s\n", strings.Join(failures, "; "))
		return exitFail
	case interrupted:
		return exitInterrupted
	}
	return exitOK
}

// writeSummaryJSON writes the summary to path ("-" is stdout is not offered:
// the human summary owns stdout).
func writeSummaryJSON(path string, sum model.ReplaySummary) error {
	b, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

// evaluateThresholds prints the gate results and returns what failed.
func evaluateThresholds(w io.Writer, sum model.ReplaySummary, errRateSet bool, maxErrRate float64, maxP99 time.Duration, runFailed bool) []string {
	if !errRateSet && maxP99 <= 0 {
		return nil
	}
	fmt.Fprintln(w, "\nThresholds")
	var failures []string
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	verdict := func(ok bool) string {
		if ok {
			return "PASS"
		}
		return "FAIL"
	}
	if sum.Sent == 0 && !runFailed {
		fmt.Fprintln(tw, "  requests sent\t0\tFAIL")
		failures = append(failures, "no requests were sent")
	}
	if errRateSet {
		ok := sum.ErrorRate <= maxErrRate
		fmt.Fprintf(tw, "  error rate\t%s\t<= %s\t%s\n", fmtPct(sum.ErrorRate), fmtPct(maxErrRate), verdict(ok))
		if !ok {
			failures = append(failures, fmt.Sprintf("error rate %s exceeds %s", fmtPct(sum.ErrorRate), fmtPct(maxErrRate)))
		}
	}
	if maxP99 > 0 {
		lat, which := sum.CorrectedLatency, "corrected p99"
		if lat.Count == 0 {
			lat, which = sum.Latency, "p99"
		}
		limit := float64(maxP99) / float64(time.Millisecond)
		if sum.Sent > 0 && (sum.Completed == 0 || lat.Count == 0) {
			// Requests were sent but nothing measured a latency (target down,
			// refused connections): a 0 ms p99 would pass the gate on a run
			// that proved nothing.
			fmt.Fprintf(tw, "  %s\tnot measured\t<= %s\tFAIL\n", which, fmtMs(limit))
			failures = append(failures, fmt.Sprintf("%s not measured: %s sent, none got a response", which, fmtInt(sum.Sent)))
		} else {
			ok := lat.P99 <= limit
			fmt.Fprintf(tw, "  %s\t%s\t<= %s\t%s\n", which, fmtMs(lat.P99), fmtMs(limit), verdict(ok))
			if !ok {
				failures = append(failures, fmt.Sprintf("%s %s exceeds %s", which, fmtMs(lat.P99), fmtMs(limit)))
			}
		}
	}
	tw.Flush()
	return failures
}

// progress prints the live one-line status on stderr, once per second.
type progress struct {
	w       io.Writer
	enabled bool
	tty     bool
	start   time.Time
	printed bool
}

func newProgress(w io.Writer, enabled bool) *progress {
	return &progress{w: w, enabled: enabled, tty: isTerminal(w), start: time.Now()}
}

func (p *progress) update(sum model.ReplaySummary) {
	if !p.enabled {
		return
	}
	line := progressLine(sum, time.Since(p.start))
	if p.tty {
		fmt.Fprintf(p.w, "\r\033[K%s", line)
	} else {
		fmt.Fprintln(p.w, line)
	}
	p.printed = true
}

// finish ends the live line so later output starts on a fresh line.
func (p *progress) finish() {
	if p.enabled && p.tty && p.printed {
		fmt.Fprint(p.w, "\r\033[K")
	}
}

// progressLine formats one progress update.
func progressLine(sum model.ReplaySummary, elapsed time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%6s  sent %s", fmtDur(elapsed.Round(time.Second)), fmtInt(sum.Sent))
	if sum.Planned > 0 {
		pct := float64(sum.Sent) / float64(sum.Planned) * 100
		fmt.Fprintf(&b, " (%.0f%%)", min(pct, 100))
	}
	fmt.Fprintf(&b, "  %s rps", fmtFloat(sum.AchievedRPS, 0))
	fmt.Fprintf(&b, "  p50 %s  p95 %s  p99 %s", fmtMs(sum.Latency.P50), fmtMs(sum.Latency.P95), fmtMs(sum.Latency.P99))
	errRate := 0.0
	if sum.Sent > 0 {
		errRate = float64(sum.Errors) / float64(sum.Sent)
	}
	fmt.Fprintf(&b, "  errors %s (%s)", fmtInt(sum.Errors), fmtPct(errRate))
	return b.String()
}

// printReplaySummary writes the final human summary.
func printReplaySummary(w io.Writer, rec model.Recording, cfg model.ReplayConfig, sum model.ReplaySummary, wall time.Duration) {
	fmt.Fprintf(w, "\nReplay of %q (%s) against %s\n", rec.Name, rec.ID, cfg.Target)
	fmt.Fprintf(w, "%s\n\n", describeSchedule(cfg))

	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	row := func(label, format string, a ...any) {
		fmt.Fprintf(tw, "  %s\t%s\n", label, fmt.Sprintf(format, a...))
	}
	planned := "unknown"
	if sum.Planned >= 0 {
		planned = fmtInt(sum.Planned)
	}
	elapsed := sum.Elapsed.D()
	if elapsed <= 0 {
		elapsed = wall
	}
	row("Requests", "sent %s of %s planned, completed %s, failed %s (timeouts %s)", fmtInt(sum.Sent), planned, fmtInt(sum.Completed), fmtInt(sum.Failed), fmtInt(sum.Timeouts))
	if sum.TargetRPS > 0 {
		row("Throughput", "%s rps achieved, %s rps scheduled (%s of schedule) over %s", fmtFloat(sum.AchievedRPS, 1), fmtFloat(sum.TargetRPS, 1), fmtPct(safeDiv(sum.AchievedRPS, sum.TargetRPS)), fmtDur(elapsed))
	} else {
		row("Throughput", "%s rps over %s", fmtFloat(sum.AchievedRPS, 1), fmtDur(elapsed))
	}
	row("Errors", "%s (%s of requests sent)", fmtInt(sum.Errors), fmtPct(sum.ErrorRate))
	if cfg.CompareStatus {
		row("Mismatches", "%s responses differ in status from the recording", fmtInt(sum.Mismatches))
	}
	row("Data", "%s sent, %s received", fmtBytes(sum.BytesSent), fmtBytes(sum.BytesReceived))
	row("Concurrency", "peak %s in flight of %s allowed, max scheduling lag %s", fmtInt(sum.MaxInFlight), fmtInt(int64(cfg.Concurrency)), fmtDur(sum.MaxLag.D()))
	tw.Flush()

	if sum.Saturated {
		fmt.Fprintf(w, "\n  WARNING: saturated. Every concurrency slot was busy at least once, so lag and the\n"+
			"  corrected latency include time spent waiting on the client side. Raise --concurrency\n"+
			"  (now %d) for numbers that reflect the target alone.\n", cfg.Concurrency)
	}

	fmt.Fprintln(w, "\nLatency")
	lt := tabwriter.NewWriter(w, 0, 8, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(lt, "\tmin\tmean\tp50\tp90\tp95\tp99\tp99.9\tmax\t")
	latRow := func(name string, l model.LatencySummary) {
		fmt.Fprintf(lt, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t\n", name,
			fmtMs(l.Min), fmtMs(l.Mean), fmtMs(l.P50), fmtMs(l.P90), fmtMs(l.P95), fmtMs(l.P99), fmtMs(l.P999), fmtMs(l.Max))
	}
	latRow("service", sum.Latency)
	if sum.CorrectedLatency.Count > 0 {
		latRow("corrected", sum.CorrectedLatency)
	}
	lt.Flush()
	if sum.CorrectedLatency.Count > 0 {
		fmt.Fprintln(w, "  (service = request sent to body read; corrected = from the intended send time)")
	}

	if len(sum.StatusCounts) > 0 {
		fmt.Fprintln(w, "\nStatus codes")
		var parts []string
		for _, code := range sortedStatusCodes(sum.StatusCounts) {
			parts = append(parts, fmt.Sprintf("%s: %s", code, fmtInt(sum.StatusCounts[code])))
		}
		fmt.Fprintf(w, "  %s\n", strings.Join(parts, "   "))
	}

	if len(sum.Endpoints) > 0 {
		const top = 10
		fmt.Fprintf(w, "\nEndpoints (%d busiest of %d)\n", min(top, len(sum.Endpoints)), len(sum.Endpoints))
		et := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
		fmt.Fprintln(et, "  ENDPOINT\tCOUNT\tERRORS\tP50\tP95\tP99")
		for i, e := range sum.Endpoints {
			if i >= top {
				break
			}
			fmt.Fprintf(et, "  %s\t%s\t%s\t%s\t%s\t%s\n", e.Endpoint, fmtInt(e.Count), fmtInt(e.Errors), fmtMs(e.Latency.P50), fmtMs(e.Latency.P95), fmtMs(e.Latency.P99))
		}
		et.Flush()
	}
}

// describeSchedule is the one-line description of what was scheduled.
func describeSchedule(cfg model.ReplayConfig) string {
	var parts []string
	switch cfg.Mode {
	case model.ReplayTimeline:
		parts = append(parts, fmt.Sprintf("timeline at %gx speed", cfg.Speed))
		if cfg.Amplify > 1 {
			parts = append(parts, fmt.Sprintf("%dx amplified", cfg.Amplify))
		}
		if cfg.MaxGap > 0 {
			parts = append(parts, fmt.Sprintf("gaps capped at %s", cfg.MaxGap))
		}
	case model.ReplayRate:
		if len(cfg.Stages) > 0 {
			parts = append(parts, fmt.Sprintf("rate mode, %d stage(s)", len(cfg.Stages)))
		} else {
			parts = append(parts, fmt.Sprintf("rate mode at %g rps", cfg.Rate))
		}
	case model.ReplayMax:
		parts = append(parts, fmt.Sprintf("max throughput, %d workers", cfg.Concurrency))
	}
	if cfg.ErrorAmplification != 1 {
		parts = append(parts, fmt.Sprintf("error requests x%g", cfg.ErrorAmplification))
	}
	if cfg.Duration > 0 {
		parts = append(parts, fmt.Sprintf("for %s", cfg.Duration))
	}
	if cfg.Loop {
		parts = append(parts, "looping")
	}
	if cfg.MaxRequests > 0 {
		parts = append(parts, fmt.Sprintf("at most %s requests", fmtInt(cfg.MaxRequests)))
	}
	if cfg.ProfileID != "" {
		parts = append(parts, "anonymized on the fly")
	}
	return strings.Join(parts, ", ")
}

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// sortedStatusCodes orders status code keys numerically ("0" is no response).
func sortedStatusCodes(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	less := func(a, b string) bool {
		na, ea := strconv.Atoi(a)
		nb, eb := strconv.Atoi(b)
		if ea == nil && eb == nil {
			return na < nb
		}
		return a < b
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && less(keys[j], keys[j-1]); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
