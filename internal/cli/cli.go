// Package cli implements the command-line interface for the agent binary.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"

	"github.com/getnabbed/agent/internal/version"
)

// Exit codes returned by Run.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// command is a single subcommand.
type command struct {
	name    string
	summary string
	run     func(args []string, stdout, stderr io.Writer) int
}

var commands = map[string]command{
	"version": {
		name:    "version",
		summary: "Print version information",
		run:     runVersion,
	},
}

// Run executes the CLI with the given arguments (excluding the program name)
// and returns a process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { printUsage(stderr) }

	showVersion := fs.Bool("version", false, "print version information and exit")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}

	if *showVersion {
		return runVersion(nil, stdout, stderr)
	}

	rest := fs.Args()
	if len(rest) == 0 {
		printUsage(stderr)
		return ExitUsage
	}

	name := rest[0]
	if name == "help" {
		printUsage(stdout)
		return ExitOK
	}

	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(stderr, "agent: unknown command %q\n\n", name)
		printUsage(stderr)
		return ExitUsage
	}
	return cmd.run(rest[1:], stdout, stderr)
}

func runVersion(_ []string, stdout, _ io.Writer) int {
	fmt.Fprintln(stdout, "agent", version.String())
	return ExitOK
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: agent [flags] <command> [args]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")

	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "  %-10s %s\n", name, commands[name].summary)
	}
	fmt.Fprintf(w, "  %-10s %s\n", "help", "Show this help")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --version  Print version information and exit")
}
