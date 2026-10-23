// Package cli implements the command-line interface for the agent binary.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"

	"github.com/getnabbed/agent/internal/config"
	"github.com/getnabbed/agent/internal/logging"
	"github.com/getnabbed/agent/internal/version"
)

// Exit codes returned by Run.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
	// ExitReauth means the agent was revoked or its credentials expired and
	// it must be paired again. Supervisors should not restart on it.
	ExitReauth = 3
)

// env carries the process-level dependencies handed to every command.
type env struct {
	stdout  io.Writer
	stderr  io.Writer
	log     *slog.Logger
	cfg     *config.Config
	dataDir string
}

// subcommand is one CLI verb.
type subcommand struct {
	name    string
	summary string
	run     func(ctx context.Context, e *env, args []string) int
}

var commands = map[string]subcommand{}

func register(c subcommand) { commands[c.name] = c }

func init() {
	register(subcommand{name: "version", summary: "Print version information", run: runVersion})
	register(subcommand{name: "pair", summary: "Pair this agent with your Nabbed account", run: runPair})
	register(subcommand{name: "run", summary: "Connect to the control plane and run (pairs first if needed)", run: runRun})
	register(subcommand{name: "status", summary: "Show pairing and identity details", run: runStatus})
	register(subcommand{name: "config", summary: "Print the effective configuration (secrets redacted)", run: runConfig})
}

// Run executes the CLI with the given arguments (excluding the program name)
// and returns a process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { printUsage(stderr) }

	// Global flags default to "" so that only flags the user actually set
	// override the config file and environment.
	showVersion := fs.Bool("version", false, "print version information and exit")
	configPath := fs.String("config", "", "config file (env "+config.EnvConfig+")")
	logLevel := fs.String("log-level", "", "minimum log level: debug, info, warn, error")
	logFormat := fs.String("log-format", "", "log output format: auto, json, text")
	dataDir := fs.String("data-dir", "", "directory for credentials and state")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}

		return ExitUsage
	}

	if *showVersion {
		return runVersion(ctx, &env{stdout: stdout, stderr: stderr, log: slog.Default()}, nil)
	}

	cfg, src, err := config.Load(*configPath, os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "agent: %v\n", err)
		return ExitUsage
	}

	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "log-level":
			cfg.Log.Level = *logLevel
		case "log-format":
			cfg.Log.Format = *logFormat
		case "data-dir":
			cfg.DataDir = *dataDir
		}
	})

	if err := cfg.Finish(); err != nil {
		fmt.Fprintf(stderr, "agent: %v\n", err)
		return ExitUsage
	}

	level, _ := logging.ParseLevel(cfg.Log.Level)
	format, _ := logging.ParseFormat(cfg.Log.Format)

	logger := logging.New(logging.Options{
		Format:    format,
		Level:     level,
		Output:    stderr,
		AddSource: level <= slog.LevelDebug,
	})
	slog.SetDefault(logger)

	e := &env{stdout: stdout, stderr: stderr, log: logger, cfg: cfg, dataDir: cfg.DataDir}
	e.log.Debug("configuration", "file", src.Path, "file_loaded", src.Loaded)

	for _, w := range src.Warnings {
		e.log.Warn(w)
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

	// Never log the arguments themselves: flags such as --nntp carry secrets.
	e.log.Debug("running command", "command", name, "arg_count", len(rest)-1)

	return cmd.run(ctx, e, rest[1:])
}

func runVersion(_ context.Context, e *env, _ []string) int {
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
	fmt.Fprintln(w, "  --config <file>        Config file (env NABBED_CONFIG)")
	fmt.Fprintln(w, "  --data-dir <dir>       Credentials and state directory (env NABBED_DATA_DIR)")
	fmt.Fprintln(w, "  --log-level <level>    Minimum log level: debug, info, warn, error (default info)")
	fmt.Fprintln(w, "  --log-format <format>  Log output format: auto, json, text (default auto)")
	fmt.Fprintln(w, "  --version              Print version information and exit")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run 'agent <command> -h' for command flags.")
}

func runConfig(_ context.Context, e *env, args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(e.stderr, "usage: agent config")
		return ExitUsage
	}

	fmt.Fprint(e.stdout, e.cfg.Redacted())

	return ExitOK
}
