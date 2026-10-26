package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/config"
	"github.com/justinburger/mirage/internal/har"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/ops"
	"github.com/justinburger/mirage/internal/store"
	"github.com/justinburger/mirage/internal/version"
)

// anonReport tallies what an anonymization run changed.
type anonReport struct {
	exchanges        int64
	exchangesChanged int64
	values           int64
	skipped          int64
	bySource         map[string]int64
}

func newAnonReport() *anonReport { return &anonReport{bySource: map[string]int64{}} }

// add records one exchange and the changes made to it.
func (r *anonReport) add(changes []model.AnonChange) {
	r.exchanges++
	if len(changes) > 0 {
		r.exchangesChanged++
	}
	for _, c := range changes {
		r.values++
		r.bySource[sourceLabel(c.Source)]++
	}
}

// sourceLabel groups the engine's change sources for the report: a rule is
// shown by name, a detector as "detector:email".
func sourceLabel(src string) string {
	if src == "" {
		return "(other)"
	}
	return src
}

// print writes the short report.
func (r *anonReport) print(w io.Writer, prof model.AnonymizationProfile, extra ...string) {
	fmt.Fprintf(w, "Anonymized %s exchange", fmtInt(r.exchanges))
	if r.exchanges != 1 {
		fmt.Fprint(w, "s")
	}
	fmt.Fprintf(w, " with profile %q\n", prof.Name)
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintf(tw, "  exchanges changed\t%s of %s\n", fmtInt(r.exchangesChanged), fmtInt(r.exchanges))
	fmt.Fprintf(tw, "  values changed\t%s\n", fmtInt(r.values))
	if r.skipped > 0 {
		fmt.Fprintf(tw, "  entries skipped\t%s (not HTTP or HTTPS requests)\n", fmtInt(r.skipped))
	}
	tw.Flush()
	if len(r.bySource) > 0 {
		const top = 15
		fmt.Fprintln(w, "  by source:")
		keys := sortedCounts(r.bySource)
		st := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
		var other int64
		for i, k := range keys {
			if i >= top {
				other += r.bySource[k]
				continue
			}
			fmt.Fprintf(st, "    %s\t%s\n", k, fmtInt(r.bySource[k]))
		}
		if other > 0 {
			fmt.Fprintf(st, "    (%d more sources)\t%s\n", len(keys)-top, fmtInt(other))
		}
		st.Flush()
	}
	for _, e := range extra {
		fmt.Fprintln(w, e)
	}
}

func runAnonymize(ctx context.Context, args []string, s streams) int {
	var (
		inPath, outPath, profileFile string
		dataPath, recRef, name       string
		profileRef                   string
		jsonReport                   bool
	)
	fs := newFlagSet("anonymize",
		"anonymize a HAR file, or a stored recording into a new recording",
		[]string{
			"--in FILE.har --out FILE.har [--profile-file FILE | --data DB --profile ID]",
			"--data DB --recording ID|NAME --name COPYNAME [--profile ID]",
		},
		[]flagGroup{
			{"HAR file to HAR file", []string{"in", "out"}},
			{"Stored recording to anonymized copy", []string{"data", "recording", "name"}},
			{"Profile", []string{"profile-file", "profile"}},
			{"Output", []string{"json"}},
		},
		[]string{
			"mirage anonymize --in session.har --out session.anon.har",
			"    built-in default profile; a fresh secret, so tokens differ from run to run",
			"mirage anonymize --in session.har --out session.anon.har --profile-file strict.yaml",
			"    your own rules; put \"secret:\" in the file to get the same tokens every run",
			"mirage anonymize --in session.har --out - --data mirage.db --profile strict | gzip > anon.har.gz",
			"    use a profile stored in a database; \"-\" is stdout (or stdin for --in)",
			"mirage anonymize --data mirage.db --recording checkout --name \"checkout (anon)\"",
			"    a new stored recording; the original is untouched",
		}, s)
	fs.StringVar(&inPath, "in", "", "FILE|HAR file to read (\"-\" is stdin)")
	fs.StringVar(&outPath, "out", "", "FILE|HAR file to write (\"-\" is stdout)")
	fs.StringVar(&dataPath, "data", "", "FILE|SQLite database, for stored profiles and recordings")
	fs.StringVar(&recRef, "recording", "", "ID|recording (id or name) to copy; needs --data")
	fs.StringVar(&name, "name", "", "NAME|name of the anonymized copy (default: \"<recording> (anon)\")")
	fs.StringVar(&profileFile, "profile-file", "", "FILE|anonymization profile as YAML or JSON")
	fs.StringVar(&profileRef, "profile", "", "ID|profile stored in --data (id or name); default: the built-in default profile")
	fs.BoolVar(&jsonReport, "json", false, "print the report as JSON instead of text")
	if code, ok := parseFlags(fs, args, s); !ok {
		return code
	}

	fileMode := inPath != "" || outPath != ""
	storedMode := recRef != ""
	switch {
	case fileMode && storedMode:
		return usageError(s, fs, "use either --in/--out or --recording, not both")
	case !fileMode && !storedMode:
		return usageError(s, fs, "give --in and --out (HAR file) or --data and --recording (stored recording)")
	case fileMode && (inPath == "" || outPath == ""):
		return usageError(s, fs, "--in and --out are both required")
	case storedMode && dataPath == "":
		return usageError(s, fs, "--recording needs --data")
	case profileFile != "" && profileRef != "":
		return usageError(s, fs, "use either --profile-file or --profile, not both")
	case profileRef != "" && dataPath == "":
		return usageError(s, fs, "--profile needs --data")
	}

	ctx, stop := signalContext(ctx)
	defer stop()

	// Resolve the profile.
	var (
		st   *store.SQLite
		prof model.AnonymizationProfile
		note string
	)
	if dataPath != "" {
		var err error
		if st, err = openExistingStore(dataPath); err != nil {
			fmt.Fprintf(s.err, "mirage anonymize: %v\n", err)
			return exitFail
		}
		defer st.Close()
	}
	switch {
	case profileFile != "":
		p, err := config.LoadProfile(profileFile)
		if err != nil {
			fmt.Fprintf(s.err, "mirage anonymize: %v\n", err)
			return exitFail
		}
		prof = p
		if prof.Secret == "" {
			prof.Secret = model.NewSecret()
			note = "Note: the profile has no \"secret:\", so a random one was used; the tokens will differ on the next run."
		}
	case st != nil:
		p, err := resolveProfile(ctx, st, profileRef)
		if err != nil {
			fmt.Fprintf(s.err, "mirage anonymize: %v\n", err)
			return exitFail
		}
		prof = p
	default:
		prof = model.DefaultAnonymizationProfile()
		prof.Secret = model.NewSecret()
		note = "Note: no profile given, so the built-in default profile with a random secret was used; the tokens will differ\n" +
			"on the next run. Pass --profile-file with a \"secret:\" to make them repeatable."
	}
	eng, err := anonymize.New(prof)
	if err != nil {
		fmt.Fprintf(s.err, "mirage anonymize: profile %q: %v\n", prof.Name, err)
		return exitFail
	}

	rep := newAnonReport()
	reportOut := s.out
	var extra []string

	if fileMode {
		if outPath == "-" {
			reportOut = s.err // stdout carries the HAR
		}
		n, err := anonymizeHARFile(ctx, s, inPath, outPath, eng, rep)
		if err != nil {
			fmt.Fprintf(s.err, "mirage anonymize: %v\n", err)
			if errors.Is(err, context.Canceled) {
				return exitInterrupted
			}
			return exitFail
		}
		if outPath != "-" {
			extra = append(extra, fmt.Sprintf("Wrote %s (%s)", outPath, fmtBytes(n)))
		}
	} else {
		src, err := resolveRecording(ctx, st, recRef)
		if err != nil {
			fmt.Fprintf(s.err, "mirage anonymize: %v\n", err)
			return exitFail
		}
		out, err := ops.CopyAnonymizedReport(ctx, st, src.ID, eng, name, nil, rep.add)
		if err != nil {
			fmt.Fprintf(s.err, "mirage anonymize: %v\n", err)
			return exitFail
		}
		extra = append(extra, fmt.Sprintf("Created recording %q (%s) with %s exchanges", out.Name, out.ID, fmtInt(out.ExchangeCount)))
	}

	if jsonReport {
		enc := json.NewEncoder(reportOut)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"profile":           prof.Name,
			"exchanges":         rep.exchanges,
			"exchanges_changed": rep.exchangesChanged,
			"values_changed":    rep.values,
			"skipped":           rep.skipped,
			"by_source":         rep.bySource,
		})
		return exitOK
	}
	rep.print(reportOut, prof, extra...)
	if note != "" {
		fmt.Fprintln(s.err, note)
	}
	return exitOK
}

// anonymizeHARFile streams a HAR file through the engine into another HAR
// file without a database, so memory use does not depend on its size. It
// returns the number of bytes written.
func anonymizeHARFile(ctx context.Context, s streams, inPath, outPath string, eng *anonymize.Engine, rep *anonReport) (int64, error) {
	var in io.Reader = s.in
	if inPath != "-" {
		f, err := os.Open(inPath)
		if err != nil {
			return 0, err
		}
		defer f.Close()
		in = f
	}

	var written int64
	fill := func(w io.Writer) error {
		cw := &countWriter{w: w}
		bw := bufio.NewWriterSize(cw, 64<<10)
		enc, err := har.NewEncoder(bw, version.Version)
		if err != nil {
			return err
		}
		_, err = har.DecodeEntries(in, func(index int, e *har.Entry) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			ex, err := har.ToExchange(e)
			if err != nil {
				if errors.Is(err, har.ErrUnsupported) {
					rep.skipped++
					return nil
				}
				return fmt.Errorf("entry %d: %w", index, err)
			}
			changes, err := eng.ExchangeWithReport(ex)
			if err != nil {
				return fmt.Errorf("entry %d (%s %s): %w", index, ex.Request.Method, ex.Request.Path, err)
			}
			rep.add(changes)
			return enc.Write(ex)
		})
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("read %s: %w", inPath, err)
			}
			return err
		}
		if err := enc.Close(); err != nil {
			return err
		}
		if err := bw.Flush(); err != nil {
			return err
		}
		written = cw.n
		return nil
	}

	if outPath == "-" {
		return 0, fill(s.out)
	}
	if err := writeFileAtomic(outPath, 0o644, fill); err != nil {
		return 0, err
	}
	return written, nil
}

// countWriter counts the bytes written through it.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
