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
	"time"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/ops"
	"github.com/justinburger/mirage/internal/store"
	"github.com/justinburger/mirage/internal/version"
)

// loadEngine builds the anonymization engine for an optional --profile.
func loadEngine(ctx context.Context, st store.Store, ref string) (*anonymize.Engine, error) {
	if ref == "" {
		return nil, nil
	}
	p, err := resolveProfile(ctx, st, ref)
	if err != nil {
		return nil, err
	}
	return ops.LoadEngine(ctx, st, p.ID)
}

func runImport(ctx context.Context, args []string, s streams) int {
	var dataPath, harPath, name, profileRef string
	fs := newFlagSet("import",
		"import a HAR file as a new recording",
		[]string{"--data DB --har FILE --name NAME [--profile ID]"},
		nil,
		[]string{
			"mirage import --data mirage.db --har session.har --name \"checkout session\"",
			"mirage import --data mirage.db --har session.har --name checkout --profile default",
			"    anonymize every exchange before it is stored",
		}, s)
	fs.StringVar(&dataPath, "data", "mirage.db", "FILE|SQLite database (created if missing)")
	fs.StringVar(&harPath, "har", "", "FILE|HAR file to import (\"-\" is stdin)")
	fs.StringVar(&name, "name", "", "NAME|name of the new recording (default: \"import <timestamp>\")")
	fs.StringVar(&profileRef, "profile", "", "ID|anonymize every exchange on the way in with this profile (id or name)")
	if code, ok := parseFlags(fs, args, s); !ok {
		return code
	}
	if harPath == "" {
		return usageError(s, fs, "--har is required")
	}
	ctx, stop := signalContext(ctx)
	defer stop()

	var in io.Reader = s.in
	if harPath != "-" {
		f, err := os.Open(harPath)
		if err != nil {
			fmt.Fprintf(s.err, "mirage import: %v\n", err)
			return exitFail
		}
		defer f.Close()
		in = f
	}
	st, err := store.Open(dataPath)
	if err != nil {
		fmt.Fprintf(s.err, "mirage import: open database %s: %v\n", dataPath, err)
		return exitFail
	}
	defer st.Close()
	eng, err := loadEngine(ctx, st, profileRef)
	if err != nil {
		fmt.Fprintf(s.err, "mirage import: %v\n", err)
		return exitFail
	}
	rec, err := ops.ImportHAR(ctx, st, in, name, eng, nil)
	if err != nil {
		fmt.Fprintf(s.err, "mirage import: %v\n", err)
		return exitFail
	}
	anon := ""
	if rec.Anonymized {
		anon = ", anonymized"
	}
	fmt.Fprintf(s.out, "Imported %s exchanges (%s errors, %s%s) into recording %q (%s)\n",
		fmtInt(rec.ExchangeCount), fmtInt(rec.ErrorCount), fmtBytes(rec.Bytes), anon, rec.Name, rec.ID)
	return exitOK
}

func runExport(ctx context.Context, args []string, s streams) int {
	var dataPath, recRef, outPath, profileRef string
	fs := newFlagSet("export",
		"export a recording as a HAR file",
		[]string{"--data DB --recording ID|NAME --out FILE.har [--profile ID]"},
		nil,
		[]string{
			"mirage export --data mirage.db --recording checkout --out checkout.har",
			"mirage export --data mirage.db --recording checkout --out - --profile strict | gzip > checkout.har.gz",
			"    anonymize on the way out (the stored recording is untouched); \"-\" is stdout",
		}, s)
	fs.StringVar(&dataPath, "data", "mirage.db", "FILE|SQLite database")
	fs.StringVar(&recRef, "recording", "", "ID|recording (id or name) to export")
	fs.StringVar(&outPath, "out", "", "FILE|HAR file to write (\"-\" is stdout)")
	fs.StringVar(&profileRef, "profile", "", "ID|anonymize on the way out with this profile (id or name); the stored recording is untouched")
	if code, ok := parseFlags(fs, args, s); !ok {
		return code
	}
	if recRef == "" || outPath == "" {
		return usageError(s, fs, "--recording and --out are required")
	}
	ctx, stop := signalContext(ctx)
	defer stop()

	st, err := openExistingStore(dataPath)
	if err != nil {
		fmt.Fprintf(s.err, "mirage export: %v\n", err)
		return exitFail
	}
	defer st.Close()
	rec, err := resolveRecording(ctx, st, recRef)
	if err != nil {
		fmt.Fprintf(s.err, "mirage export: %v\n", err)
		return exitFail
	}
	eng, err := loadEngine(ctx, st, profileRef)
	if err != nil {
		fmt.Fprintf(s.err, "mirage export: %v\n", err)
		return exitFail
	}

	var written int64
	export := func(w io.Writer) error {
		cw := &countWriter{w: w}
		if err := ops.ExportHAR(ctx, st, rec.ID, cw, eng, version.Version); err != nil {
			return err
		}
		written = cw.n
		return nil
	}
	if outPath == "-" {
		bw := bufio.NewWriter(s.out)
		err = export(bw)
		if ferr := bw.Flush(); err == nil {
			err = ferr
		}
	} else {
		err = writeFileAtomic(outPath, 0o644, export)
	}
	if err != nil {
		fmt.Fprintf(s.err, "mirage export: %v\n", err)
		if errors.Is(err, context.Canceled) {
			return exitInterrupted
		}
		return exitFail
	}
	dst := s.out
	if outPath == "-" {
		dst = s.err
	}
	anon := ""
	if eng != nil {
		anon = " (anonymized)"
	}
	fmt.Fprintf(dst, "Exported %s exchanges of %q to %s, %s%s\n", fmtInt(rec.ExchangeCount), rec.Name, outPath, fmtBytes(written), anon)
	return exitOK
}

func runRecordings(ctx context.Context, args []string, s streams) int {
	var dataPath string
	var asJSON bool
	fs := newFlagSet("recordings",
		"list the recordings in a database",
		[]string{"[--data DB] [--json]"},
		nil,
		[]string{
			"mirage recordings --data mirage.db",
			"mirage recordings --data mirage.db --json | jq -r '.[].id'",
		}, s)
	fs.StringVar(&dataPath, "data", "mirage.db", "FILE|SQLite database")
	fs.BoolVar(&asJSON, "json", false, "print the recordings as JSON")
	if code, ok := parseFlags(fs, args, s); !ok {
		return code
	}
	st, err := openExistingStore(dataPath)
	if err != nil {
		fmt.Fprintf(s.err, "mirage recordings: %v\n", err)
		return exitFail
	}
	defer st.Close()
	recs, err := st.ListRecordings(ctx)
	if err != nil {
		fmt.Fprintf(s.err, "mirage recordings: %v\n", err)
		return exitFail
	}
	if asJSON {
		if recs == nil {
			recs = []model.Recording{}
		}
		enc := json.NewEncoder(s.out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(recs); err != nil {
			fmt.Fprintf(s.err, "mirage recordings: %v\n", err)
			return exitFail
		}
		return exitOK
	}
	if len(recs) == 0 {
		fmt.Fprintf(s.out, "No recordings in %s.\n", dataPath)
		return exitOK
	}
	tw := tabwriter.NewWriter(s.out, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSOURCE\tEXCHANGES\tERRORS\tSIZE\tANON\tFIRST\tLAST")
	for _, r := range recs {
		anon := "no"
		if r.Anonymized {
			anon = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, r.Name, r.Source, fmtInt(r.ExchangeCount), fmtInt(r.ErrorCount), fmtBytes(r.Bytes), anon, fmtTime(r.FirstAt), fmtTime(r.LastAt))
	}
	tw.Flush()
	return exitOK
}

func fmtTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
