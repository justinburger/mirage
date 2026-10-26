// Command mirage is a service virtualization and traffic replay platform for
// performance testing.
//
//	mirage serve        run the proxy services, the admin API and the web UI
//	mirage replay       replay a recording against a target as a load test (CI friendly)
//	mirage anonymize    anonymize a HAR file or a stored recording
//	mirage import       import a HAR file as a recording
//	mirage export       export a recording as a HAR file
//	mirage recordings   list the recordings in a database
//	mirage version      print the version
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/justinburger/mirage/internal/version"
)

// command is one subcommand.
type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string, s streams) int
}

// commands lists the subcommands in the order shown by the help.
func commands() []command {
	return []command{
		{"serve", "run the proxy services, the admin API and the web UI", runServe},
		{"replay", "replay a recording against a target as a load test (headless, CI friendly)", runReplay},
		{"anonymize", "anonymize a HAR file or a stored recording", runAnonymize},
		{"import", "import a HAR file as a recording", runImport},
		{"export", "export a recording as a HAR file", runExport},
		{"recordings", "list the recordings in a database", runRecordings},
		{"version", "print the version", runVersion},
	}
}

func main() {
	ctx := context.Background()
	os.Exit(run(ctx, os.Args[1:], streams{in: os.Stdin, out: os.Stdout, err: os.Stderr}))
}

// run dispatches to a subcommand and returns the process exit code.
func run(ctx context.Context, args []string, s streams) int {
	if len(args) == 0 {
		printUsage(s.err)
		return exitUsage
	}
	name, rest := args[0], args[1:]
	switch name {
	case "-h", "--help", "-help", "help":
		if name == "help" && len(rest) > 0 {
			// mirage help <command> is mirage <command> -h
			return run(ctx, []string{rest[0], "-h"}, s)
		}
		printUsage(s.out)
		return exitOK
	case "-v", "--version", "-version":
		return runVersion(ctx, nil, s)
	}
	for _, c := range commands() {
		if c.name == name {
			return c.run(ctx, rest, s)
		}
	}
	fmt.Fprintf(s.err, "mirage: unknown command %q\n\n", name)
	if sug := suggestCommand(name); sug != "" {
		fmt.Fprintf(s.err, "Did you mean %q?\n\n", sug)
	}
	printUsage(s.err)
	return exitUsage
}

// printUsage writes the top-level help.
func printUsage(w io.Writer) {
	fmt.Fprintf(w, `Mirage %s: service virtualization and traffic replay for performance testing.

Mirage sits between your clients and an HTTP API. It records the traffic,
serves it back without the real API, injects faults, anonymizes what it
captured, and replays it as a load test at a multiple of its original rate
or error mix.

Usage:
  mirage <command> [flags]

Commands:
`, version.Version)
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-11s %s\n", c.name, c.summary)
	}
	fmt.Fprint(w, `
Run 'mirage <command> -h' for the flags and examples of a command.

Examples:
  mirage serve --config examples/mirage.yaml
  mirage replay --data mirage.db --recording checkout --target http://staging:8080 --speed 10
  mirage anonymize --in session.har --out session.anon.har
`)
}

// suggestCommand returns the command closest to a mistyped name, if any is
// close enough.
func suggestCommand(name string) string {
	type cand struct {
		name string
		d    int
	}
	var cs []cand
	for _, c := range commands() {
		if strings.HasPrefix(c.name, name) && name != "" {
			return c.name
		}
		cs = append(cs, cand{c.name, editDistance(name, c.name)})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].d < cs[j].d })
	if len(cs) > 0 && cs[0].d <= 2 {
		return cs[0].name
	}
	return ""
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// runVersion prints the version.
func runVersion(_ context.Context, args []string, s streams) int {
	fs := newFlagSet("version", "print the version", []string{""}, nil, nil, s)
	if code, ok := parseFlags(fs, args, s); !ok {
		return code
	}
	fmt.Fprintf(s.out, "mirage %s (%s, %s/%s)\n", version.String(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return exitOK
}
