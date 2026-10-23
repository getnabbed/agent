// Package cli implements the command-line interface for the agent binary.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"sort"

	"github.com/getnabbed/agent/internal/logging"
	"github.com/getnabbed/agent/internal/version"
)

// Exit codes returned by Run.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// env carries the process-level dependencies handed to every command.
type env struct {
	stdout io.Writer
	stderr io.Writer
	log    *slog.Logger
}

// command is a single subcommand.
type command struct {
	name    string
	summary string
	run     func(e *env, args []string) int
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
	logLevel := fs.String("log-level", "info", "minimum log level: debug, info, warn, error")
	logFormat := fs.String("log-format", "auto", "log output format: auto, json, text")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}

	if *showVersion {
		return runVersion(&env{stdout: stdout, stderr: stderr, log: slog.Default()}, nil)
	}

	level, err := logging.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintf(stderr, "agent: %v\n", err)
		return ExitUsage
	}
	format, err := logging.ParseFormat(*logFormat)
	if err != nil {
		fmt.Fprintf(stderr, "agent: %v\n", err)
		return ExitUsage
	}
	logger := logging.New(logging.Options{
		Format:    format,
		Level:     level,
		Output:    stderr,
		AddSource: level <= slog.LevelDebug,
	})
	slog.SetDefault(logger)

	e := &env{stdout: stdout, stderr: stderr, log: logger}

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
	e.log.Debug("running command", "command", name, "args", rest[1:])
	return cmd.run(e, rest[1:])
}

func runVersion(e *env, _ []string) int {
	fmt.Fprintln(e.stdout, "agent", version.String())
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
	fmt.Fprintln(w, "  --log-level <level>    Minimum log level: debug, info, warn, error (default info)")
	fmt.Fprintln(w, "  --log-format <format>  Log output format: auto, json, text (default auto)")
	fmt.Fprintln(w, "  --version              Print version information and exit")
}
