package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/store"
)

// Process exit codes.
const (
	exitOK          = 0
	exitFail        = 1   // runtime failure, or a replay threshold was exceeded
	exitUsage       = 2   // bad command line
	exitInterrupted = 130 // 128 + SIGINT
)

// streams are the standard streams of one invocation, so commands can be
// tested without touching the process's own.
type streams struct {
	in  io.Reader
	out io.Writer
	err io.Writer
}

// multiFlag is a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// flagGroup names a section of the flag help.
type flagGroup struct {
	title string
	names []string
}

// newFlagSet returns a FlagSet whose -h output is the command's own help text
// (summary, usage lines, flags grouped by section, examples).
func newFlagSet(name, summary string, usage []string, groups []flagGroup, examples []string, s streams) *flag.FlagSet {
	fs := flag.NewFlagSet("mirage "+name, flag.ContinueOnError)
	fs.SetOutput(s.err)
	fs.Usage = func() {
		w := fs.Output()
		fmt.Fprintf(w, "mirage %s: %s\n\nUsage:\n", name, summary)
		for _, u := range usage {
			fmt.Fprintf(w, "  mirage %s %s\n", name, u)
		}
		fmt.Fprintln(w)
		printFlags(w, fs, groups)
		if len(examples) > 0 {
			fmt.Fprintln(w, "Examples:")
			for _, e := range examples {
				fmt.Fprintf(w, "  %s\n", e)
			}
			fmt.Fprintln(w)
		}
	}
	return fs
}

// parseFlags parses args. It returns ok=false with the exit code to use when
// the command should stop (help requested or a flag error). Flags may appear
// after positional arguments.
func parseFlags(fs *flag.FlagSet, args []string, s streams) (code int, ok bool) {
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "-h" || a == "--help" || a == "-help" {
			fs.SetOutput(s.out) // explicit help goes to stdout
			fs.Usage()
			return exitOK, false
		}
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, false
		}
		// The flag package has already printed the error and the usage.
		return exitUsage, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(s.err, "mirage %s: unexpected argument %q\n", strings.TrimPrefix(fs.Name(), "mirage "), fs.Arg(0))
		fs.Usage()
		return exitUsage, false
	}
	return exitOK, true
}

// usageError prints a one-line error followed by a hint and returns exitUsage.
func usageError(s streams, fs *flag.FlagSet, format string, a ...any) int {
	fmt.Fprintf(s.err, "%s: %s\n", fs.Name(), fmt.Sprintf(format, a...))
	fmt.Fprintf(s.err, "Run '%s -h' for usage.\n", fs.Name())
	return exitUsage
}

// helpWidth is the width help text is wrapped to.
const helpWidth = 100

// printFlags writes the flags of fs, grouped, with the descriptions aligned
// and wrapped. A usage string of the form "ARG|text" names the argument shown
// after the flag ("--data FILE"); without "|" a non-bool flag shows its type.
func printFlags(w io.Writer, fs *flag.FlagSet, groups []flagGroup) {
	byName := map[string]*flag.Flag{}
	var all []string
	fs.VisitAll(func(f *flag.Flag) { byName[f.Name] = f; all = append(all, f.Name) })
	shown := map[string]bool{}
	for _, g := range groups {
		for _, n := range g.names {
			shown[n] = true
		}
	}
	var rest []string
	for _, n := range all {
		if !shown[n] {
			rest = append(rest, n)
		}
	}
	if len(groups) == 0 {
		groups = []flagGroup{{title: "Flags", names: rest}}
	} else if len(rest) > 0 {
		groups = append(groups, flagGroup{title: "Other flags", names: rest})
	}
	// One left column width for all groups keeps the help easy to scan.
	type line struct{ left, right string }
	var lines [][]line
	width := 0
	for _, g := range groups {
		var ls []line
		for _, n := range g.names {
			f := byName[n]
			if f == nil {
				continue
			}
			arg, usage := splitUsage(f)
			left := "  --" + f.Name
			if arg != "" {
				left += " " + arg
			}
			if d := flagDefault(f); d != "" {
				usage += " (default " + d + ")"
			}
			ls = append(ls, line{left, usage})
			width = max(width, len(left))
		}
		lines = append(lines, ls)
	}
	width += 2
	for i, g := range groups {
		fmt.Fprintf(w, "%s:\n", g.title)
		for _, l := range lines[i] {
			wrapped := wrapText(l.right, max(helpWidth-width, 30))
			for j, part := range wrapped {
				if j == 0 {
					fmt.Fprintf(w, "%-*s%s\n", width, l.left, part)
				} else {
					fmt.Fprintf(w, "%*s%s\n", width, "", part)
				}
			}
		}
		fmt.Fprintln(w)
	}
}

// splitUsage separates the argument name from the description of a flag.
func splitUsage(f *flag.Flag) (arg, usage string) {
	if a, u, ok := strings.Cut(f.Usage, "|"); ok {
		return a, u
	}
	name, usage := flag.UnquoteUsage(f)
	if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
		return "", usage
	}
	return name, usage
}

// wrapText breaks text into lines of at most width characters.
func wrapText(text string, width int) []string {
	var out []string
	cur := ""
	for _, word := range strings.Fields(text) {
		if cur != "" && len(cur)+1+len(word) > width {
			out = append(out, cur)
			cur = word
			continue
		}
		if cur == "" {
			cur = word
		} else {
			cur += " " + word
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

// flagDefault is the default to show for f, empty for zero values.
func flagDefault(f *flag.Flag) string {
	switch f.DefValue {
	case "", "0", "false", "0s", "[]", "map[]":
		return ""
	}
	return f.DefValue
}

// explicitFlags returns the names of the flags set on the command line.
func explicitFlags(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// newLogger builds the slog logger of long-running commands.
func newLogger(level, format string, w io.Writer) (*slog.Logger, error) {
	var lv slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lv = slog.LevelDebug
	case "", "info":
		lv = slog.LevelInfo
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		return nil, fmt.Errorf("log level %q must be debug, info, warn or error", level)
	}
	opts := &slog.HandlerOptions{Level: lv}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("log format %q must be text or json", format)
}

// openExistingStore opens the database at path and refuses to create it:
// a typo in --data must not silently produce an empty database.
func openExistingStore(path string) (*store.SQLite, error) {
	if path == "" {
		return nil, errors.New("--data is required")
	}
	if path != store.MemoryPath {
		fi, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("database %s does not exist (run 'mirage serve --data %s' or 'mirage import' first)", path, path)
			}
			return nil, err
		}
		if fi.IsDir() {
			return nil, fmt.Errorf("database %s is a directory", path)
		}
	}
	st, err := store.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	return st, nil
}

// resolveRecording finds a recording by ID or, failing that, by name.
func resolveRecording(ctx context.Context, st store.Store, ref string) (model.Recording, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return model.Recording{}, errors.New("a recording id or name is required")
	}
	if rec, err := st.GetRecording(ctx, ref); err == nil {
		return rec, nil
	} else if !errors.Is(err, model.ErrNotFound) {
		return model.Recording{}, err
	}
	recs, err := st.ListRecordings(ctx)
	if err != nil {
		return model.Recording{}, err
	}
	var exact, folded []model.Recording
	for _, r := range recs {
		switch {
		case r.Name == ref:
			exact = append(exact, r)
		case strings.EqualFold(r.Name, ref):
			folded = append(folded, r)
		}
	}
	cands := exact
	if len(cands) == 0 {
		cands = folded
	}
	switch len(cands) {
	case 0:
		return model.Recording{}, fmt.Errorf("no recording with id or name %q (see 'mirage recordings')", ref)
	case 1:
		return cands[0], nil
	}
	ids := make([]string, len(cands))
	for i, r := range cands {
		ids[i] = r.ID
	}
	return model.Recording{}, fmt.Errorf("%d recordings are named %q (%s); use the id", len(cands), ref, strings.Join(ids, ", "))
}

// resolveProfile finds an anonymization profile by ID or name. An empty ref
// selects the built-in default profile.
func resolveProfile(ctx context.Context, st store.Store, ref string) (model.AnonymizationProfile, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = model.DefaultProfileID
	}
	if p, err := st.GetProfile(ctx, ref); err == nil {
		return p, nil
	} else if !errors.Is(err, model.ErrNotFound) {
		return model.AnonymizationProfile{}, err
	}
	profs, err := st.ListProfiles(ctx)
	if err != nil {
		return model.AnonymizationProfile{}, err
	}
	for _, p := range profs {
		if strings.EqualFold(strings.TrimSpace(p.Name), ref) {
			return p, nil
		}
	}
	return model.AnonymizationProfile{}, fmt.Errorf("no anonymization profile with id or name %q", ref)
}

// Number and duration formatting for human output.

// fmtInt formats n with thousands separators.
func fmtInt(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// fmtFloat formats f with thousands separators and the given decimals.
func fmtFloat(f float64, decimals int) string {
	s := strconv.FormatFloat(f, 'f', decimals, 64)
	intPart, frac, _ := strings.Cut(s, ".")
	n, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return s
	}
	out := fmtInt(n)
	if n == 0 && strings.HasPrefix(intPart, "-") {
		out = "-0"
	}
	if frac != "" {
		out += "." + frac
	}
	return out
}

// fmtMs formats a latency in milliseconds with a sensible unit.
func fmtMs(ms float64) string {
	switch {
	case ms <= 0:
		return "0"
	case ms < 1:
		return fmt.Sprintf("%.0fus", ms*1000)
	case ms < 10:
		return fmt.Sprintf("%.2fms", ms)
	case ms < 100:
		return fmt.Sprintf("%.1fms", ms)
	case ms < 1000:
		return fmt.Sprintf("%.0fms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.2fs", ms/1000)
	}
	return (time.Duration(ms * float64(time.Millisecond))).Round(time.Second).String()
}

// fmtDur formats a duration compactly ("8.2s", "1m30s").
func fmtDur(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// fmtBytes formats a byte count with binary units.
func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// fmtPct formats a fraction as a percentage.
func fmtPct(frac float64) string {
	p := frac * 100
	switch {
	case p == 0:
		return "0%"
	case p < 0.01:
		return "<0.01%"
	case p < 10:
		return fmt.Sprintf("%.2f%%", p)
	}
	return fmt.Sprintf("%.1f%%", p)
}

// sortedCounts returns the keys of m ordered by descending count, then name.
func sortedCounts(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

// signalContext returns a context cancelled by SIGINT or SIGTERM. A second
// signal gets the default behaviour (the process dies).
func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

// isTerminal reports whether w is a character device (a terminal).
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// writeFileAtomic writes to a temporary file next to path and renames it into
// place when fill succeeds, so a failed run never leaves a truncated file.
func writeFileAtomic(path string, perm os.FileMode, fill func(w io.Writer) error) error {
	dir, base := splitPath(path)
	tmp, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	if err := fill(tmp); err != nil {
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func splitPath(path string) (dir, base string) {
	i := strings.LastIndexByte(path, os.PathSeparator)
	if i < 0 {
		return ".", path
	}
	if i == 0 {
		return string(os.PathSeparator), path[1:]
	}
	return path[:i], path[i+1:]
}
