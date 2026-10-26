package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/justinburger/mirage/internal/model"
)

const tinyHAR = "../../internal/config/testdata/tiny.har"

// runCLI runs the CLI in process and returns its exit code and output.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(context.Background(), args, streams{in: strings.NewReader(""), out: &out, err: &errb})
	return code, out.String(), errb.String()
}

func TestUsageAndExitCodes(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		code     int
		stdout   string // substring
		stderr   string // substring
		notEmpty bool
	}{
		{"no arguments", nil, exitUsage, "", "Usage:", false},
		{"unknown command", []string{"frobnicate"}, exitUsage, "", `unknown command "frobnicate"`, false},
		{"typo is suggested", []string{"replya"}, exitUsage, "", `Did you mean "replay"`, false},
		{"help", []string{"help"}, exitOK, "Commands:", "", false},
		{"-h", []string{"-h"}, exitOK, "serve", "", false},
		{"help for a command", []string{"help", "replay"}, exitOK, "--max-error-rate", "", false},
		{"version", []string{"version"}, exitOK, "mirage ", "", false},
		{"--version", []string{"--version"}, exitOK, "mirage ", "", false},
		{"bad flag", []string{"replay", "--bogus"}, exitUsage, "", "flag provided but not defined", false},
		{"stray argument", []string{"recordings", "extra"}, exitUsage, "", "unexpected argument", false},
		{"replay needs a target", []string{"replay", "--har", tinyHAR}, exitUsage, "", "--target is required", false},
		{"replay needs a source", []string{"replay", "--target", "http://x"}, exitUsage, "", "--recording", false},
		{"replay rejects both sources", []string{"replay", "--target", "http://x", "--har", tinyHAR, "--recording", "r"}, exitUsage, "", "--recording", false},
		{"replay rejects a bad target", []string{"replay", "--target", "ftp://x", "--har", tinyHAR}, exitUsage, "", "http(s) URL", false},
		{"replay rejects a bad stage", []string{"replay", "--target", "http://x", "--har", tinyHAR, "--stage", "nope"}, exitUsage, "", "DURATION:RATE", false},
		{"replay rejects bad threshold", []string{"replay", "--target", "http://x", "--har", tinyHAR, "--max-error-rate", "2"}, exitUsage, "", "between 0 and 1", false},
		{"anonymize needs something to do", []string{"anonymize"}, exitUsage, "", "--in and --out", false},
		{"anonymize --in needs --out", []string{"anonymize", "--in", "a.har"}, exitUsage, "", "both required", false},
		{"export needs flags", []string{"export"}, exitUsage, "", "required", false},
		{"import needs --har", []string{"import"}, exitUsage, "", "--har is required", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, tc.args...)
			if code != tc.code {
				t.Fatalf("exit code %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, stdout, stderr)
			}
			if !strings.Contains(stdout, tc.stdout) {
				t.Errorf("stdout lacks %q:\n%s", tc.stdout, stdout)
			}
			if !strings.Contains(stderr, tc.stderr) {
				t.Errorf("stderr lacks %q:\n%s", tc.stderr, stderr)
			}
		})
	}
}

func TestEveryCommandHasHelpWithExamples(t *testing.T) {
	for _, c := range commands() {
		if c.name == "version" {
			continue
		}
		code, out, _ := runCLI(t, c.name, "-h")
		if code != exitOK || !strings.Contains(out, "Usage:") || !strings.Contains(out, "mirage "+c.name) {
			t.Errorf("%s -h: code %d, output:\n%s", c.name, code, out)
		}
		if c.name != "recordings" && !strings.Contains(out, "Examples:") {
			t.Errorf("%s -h should show examples:\n%s", c.name, out)
		}
	}
}

func TestStageFlag(t *testing.T) {
	var st stageFlag
	for _, v := range []string{"30s:500", "2m:2000.5:hold", "1m30s:0"} {
		if err := st.Set(v); err != nil {
			t.Fatalf("Set(%q): %v", v, err)
		}
	}
	want := []model.ReplayStage{
		{Duration: model.Duration(30e9), Rate: 500},
		{Duration: model.Duration(120e9), Rate: 2000.5, Hold: true},
		{Duration: model.Duration(90e9), Rate: 0},
	}
	if len(st) != len(want) {
		t.Fatalf("got %+v", st)
	}
	for i := range want {
		if st[i] != want[i] {
			t.Errorf("stage %d = %+v, want %+v", i, st[i], want[i])
		}
	}
	for _, bad := range []string{"", "30s", "x:5", "30s:y", "0s:5", "30s:-1", "30s:5:fast", "1:2:3:4"} {
		var s stageFlag
		if err := s.Set(bad); err == nil {
			t.Errorf("Set(%q) should fail", bad)
		}
	}
}

func TestHeaderFlag(t *testing.T) {
	var h headerFlag
	if err := h.Set("Authorization: Bearer abc:def"); err != nil {
		t.Fatal(err)
	}
	if h["Authorization"] != "Bearer abc:def" {
		t.Errorf("got %q", h["Authorization"])
	}
	for _, bad := range []string{"novalue", ": x", ""} {
		if err := (&headerFlag{}).Set(bad); err == nil {
			t.Errorf("Set(%q) should fail", bad)
		}
	}
}

func TestFormatting(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -4321: "-4,321"} {
		if got := fmtInt(in); got != want {
			t.Errorf("fmtInt(%d) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[float64]string{0.5: "500us", 3.14159: "3.14ms", 25.4: "25.4ms", 250: "250ms", 2500: "2.50s"} {
		if got := fmtMs(in); got != want {
			t.Errorf("fmtMs(%v) = %q, want %q", in, got, want)
		}
	}
	if got := fmtBytes(1536); got != "1.5 KiB" {
		t.Errorf("fmtBytes = %q", got)
	}
	if got := fmtFloat(1234.5678, 1); got != "1,234.6" {
		t.Errorf("fmtFloat = %q", got)
	}
	if got := fmtPct(0.0123); got != "1.23%" {
		t.Errorf("fmtPct = %q", got)
	}
}

// target is a test server that answers 200 and counts requests, or 500 when
// failing is set.
func target(t *testing.T, failing bool) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if failing {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestReplayFromHARWritesSummaryAndGates(t *testing.T) {
	srv, hits := target(t, false)
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "sum.json")
	code, stdout, stderr := runCLI(t, "replay", "--har", tinyHAR, "--target", srv.URL, "--speed", "1000", "--quiet",
		"--json", jsonPath, "--max-error-rate", "1", "--max-p99", "10s")
	if code != exitOK {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if hits.Load() != 2 {
		t.Errorf("target got %d requests, want 2", hits.Load())
	}
	for _, want := range []string{"Replay of", "Requests", "sent 2 of 2 planned", "Latency", "p99", "Status codes", "200: 2", "Endpoints", "Thresholds", "PASS"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("summary lacks %q:\n%s", want, stdout)
		}
	}
	b, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var sum model.ReplaySummary
	if err := json.Unmarshal(b, &sum); err != nil {
		t.Fatalf("summary JSON: %v", err)
	}
	if sum.Sent != 2 || sum.Completed != 2 || sum.StatusCounts["200"] != 2 {
		t.Errorf("summary = %+v", sum)
	}
}

func TestReplayThresholdsSetTheExitCode(t *testing.T) {
	failing, _ := target(t, true)
	code, stdout, stderr := runCLI(t, "replay", "--har", tinyHAR, "--target", failing.URL, "--speed", "1000", "--quiet", "--max-error-rate", "0.5")
	if code != exitFail {
		t.Fatalf("exit %d, want %d\n%s\n%s", code, exitFail, stdout, stderr)
	}
	if !strings.Contains(stdout, "FAIL") || !strings.Contains(stderr, "thresholds exceeded: error rate") {
		t.Errorf("failure should be explained:\nstdout: %s\nstderr: %s", stdout, stderr)
	}

	ok, _ := target(t, false)
	if code, _, stderr := runCLI(t, "replay", "--har", tinyHAR, "--target", ok.URL, "--speed", "1000", "--quiet", "--max-p99", "1ns"); code != exitFail || !strings.Contains(stderr, "p99") {
		t.Errorf("--max-p99 1ns: exit %d, stderr %q", code, stderr)
	}
	// An error rate of exactly zero is a valid, strict threshold.
	if code, _, _ := runCLI(t, "replay", "--har", tinyHAR, "--target", ok.URL, "--speed", "1000", "--quiet", "--max-error-rate", "0"); code != exitOK {
		t.Errorf("--max-error-rate 0 against a healthy target: exit %d", code)
	}
}

func TestReplayWithoutGatesIgnoresErrors(t *testing.T) {
	failing, _ := target(t, true)
	if code, _, stderr := runCLI(t, "replay", "--har", tinyHAR, "--target", failing.URL, "--speed", "1000", "--quiet"); code != exitOK {
		t.Errorf("without thresholds a replay against a failing target is still a completed run: exit %d, %s", code, stderr)
	}
}

func TestReplayRateModeIsInferredFromStages(t *testing.T) {
	srv, hits := target(t, false)
	code, stdout, stderr := runCLI(t, "replay", "--har", tinyHAR, "--target", srv.URL, "--quiet", "--stage", "200ms:50:hold", "--max-requests", "5")
	if code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "rate mode") || hits.Load() == 0 {
		t.Errorf("hits=%d\n%s", hits.Load(), stdout)
	}
}

func TestReplayInvalidOptionsListEveryProblem(t *testing.T) {
	code, _, stderr := runCLI(t, "replay", "--har", tinyHAR, "--target", "http://127.0.0.1:1", "--mode", "rate", "--quiet", "--amplify", "3")
	if code != exitUsage {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr, "rate mode needs") || !strings.Contains(stderr, "amplify applies to timeline") {
		t.Errorf("stderr should list the problems:\n%s", stderr)
	}
}

func TestAnonymizeHARFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "anon.har")
	code, stdout, stderr := runCLI(t, "anonymize", "--in", tinyHAR, "--out", out)
	if code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"jane@acme.io", "hunter2"} {
		if bytes.Contains(b, []byte(raw)) {
			t.Errorf("anonymized HAR still contains %q", raw)
		}
	}
	for _, want := range []string{"Anonymized 2 exchanges", "values changed", "detector:email", "by source"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report lacks %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stderr, "random secret") {
		t.Errorf("a missing secret should be called out on stderr: %q", stderr)
	}
	var doc struct {
		Log struct{ Entries []json.RawMessage }
	}
	if err := json.Unmarshal(b, &doc); err != nil || len(doc.Log.Entries) != 2 {
		t.Errorf("output HAR: %v, %d entries", err, len(doc.Log.Entries))
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*.tmp")); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

func TestAnonymizeWithAProfileFileIsRepeatable(t *testing.T) {
	dir := t.TempDir()
	prof := filepath.Join(dir, "p.yaml")
	if err := os.WriteFile(prof, []byte("name: fixed\nsecret: abc\ndetectors: [email]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var outs [2][]byte
	for i := range outs {
		out := filepath.Join(dir, "o"+string(rune('a'+i))+".har")
		if code, _, stderr := runCLI(t, "anonymize", "--in", tinyHAR, "--out", out, "--profile-file", prof); code != exitOK {
			t.Fatalf("run %d: exit %d, %s", i, code, stderr)
		} else if strings.Contains(stderr, "random secret") {
			t.Errorf("a profile with a secret should not warn: %s", stderr)
		}
		outs[i], _ = os.ReadFile(out)
	}
	// Timestamps in the HAR are the recorded ones, so the files are equal.
	if !bytes.Equal(outs[0], outs[1]) {
		t.Error("the same profile secret must give the same anonymized output")
	}
}

func TestAnonymizeToStdoutKeepsTheReportOffStdout(t *testing.T) {
	code, stdout, stderr := runCLI(t, "anonymize", "--in", tinyHAR, "--out", "-")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout must be exactly the HAR: %v\n%.200s", err, stdout)
	}
	if !strings.Contains(stderr, "Anonymized 2 exchanges") {
		t.Errorf("report should go to stderr:\n%s", stderr)
	}
}

func TestImportExportRecordingsAndStoredAnonymizedCopy(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "m.db")

	if code, out, stderr := runCLI(t, "import", "--data", db, "--har", tinyHAR, "--name", "tiny"); code != exitOK || !strings.Contains(out, "Imported 2 exchanges") {
		t.Fatalf("import: %d\n%s\n%s", code, out, stderr)
	}
	code, out, _ := runCLI(t, "recordings", "--data", db)
	if code != exitOK || !strings.Contains(out, "tiny") || !strings.Contains(out, "har") {
		t.Fatalf("recordings:\n%s", out)
	}

	exported := filepath.Join(dir, "out.har")
	if code, _, stderr := runCLI(t, "export", "--data", db, "--recording", "tiny", "--out", exported); code != exitOK {
		t.Fatalf("export: %d %s", code, stderr)
	}
	if b, _ := os.ReadFile(exported); !bytes.Contains(b, []byte("jane@acme.io")) {
		t.Error("a plain export keeps the stored values")
	}
	anonymized := filepath.Join(dir, "anon.har")
	if code, _, stderr := runCLI(t, "export", "--data", db, "--recording", "tiny", "--out", anonymized, "--profile", "default"); code != exitOK {
		t.Fatalf("export --profile: %d %s", code, stderr)
	}
	if b, _ := os.ReadFile(anonymized); bytes.Contains(b, []byte("jane@acme.io")) {
		t.Error("export --profile must anonymize")
	}

	code, out, stderr := runCLI(t, "anonymize", "--data", db, "--recording", "tiny", "--name", "tiny copy")
	if code != exitOK || !strings.Contains(out, `Created recording "tiny copy"`) || !strings.Contains(out, "detector:email") {
		t.Fatalf("stored anonymize: %d\n%s\n%s", code, out, stderr)
	}
	code, out, _ = runCLI(t, "recordings", "--data", db, "--json")
	var recs []model.Recording
	if err := json.Unmarshal([]byte(out), &recs); code != exitOK || err != nil || len(recs) != 2 {
		t.Fatalf("recordings --json: %v\n%s", err, out)
	}
	if code, _, stderr := runCLI(t, "anonymize", "--data", db, "--recording", "nope", "--name", "x"); code != exitFail || !strings.Contains(stderr, "no recording") {
		t.Errorf("unknown recording: %d %s", code, stderr)
	}
}

func TestMissingDatabaseIsNotCreated(t *testing.T) {
	db := filepath.Join(t.TempDir(), "typo.db")
	for _, args := range [][]string{
		{"recordings", "--data", db},
		{"export", "--data", db, "--recording", "x", "--out", filepath.Join(t.TempDir(), "o.har")},
		{"replay", "--data", db, "--recording", "x", "--target", "http://127.0.0.1:1"},
	} {
		code, _, stderr := runCLI(t, args...)
		if code != exitFail || !strings.Contains(stderr, "does not exist") {
			t.Errorf("%v: exit %d, stderr %q", args, code, stderr)
		}
	}
	if _, err := os.Stat(db); err == nil {
		t.Error("a read-only command must not create the database file")
	}
}

func TestAmbiguousRecordingNameIsRefused(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "m.db")
	for i := 0; i < 2; i++ {
		if code, _, stderr := runCLI(t, "import", "--data", db, "--har", tinyHAR, "--name", "same"); code != exitOK {
			t.Fatalf("import: %s", stderr)
		}
	}
	code, _, stderr := runCLI(t, "export", "--data", db, "--recording", "same", "--out", filepath.Join(dir, "o.har"))
	if code != exitFail || !strings.Contains(stderr, "2 recordings are named") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

func TestWriteFileAtomicLeavesNothingOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.har")
	err := writeFileAtomic(path, 0o644, func(w io.Writer) error {
		w.Write([]byte("partial"))
		return os.ErrInvalid
	})
	if err == nil {
		t.Fatal("want the fill error")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("failed write left files behind: %v", entries)
	}
}
