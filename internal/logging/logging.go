// Package logging configures structured logging for the agent binary.
//
// It builds on log/slog and provides two output formats:
//
//   - JSON, one object per line, intended for machines (log shippers,
//     containers, CI).
//   - Text, a compact human-readable line format with optional ANSI colour,
//     intended for interactive terminals.
//
// [New] picks a format automatically unless one is forced via [Options].
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Format selects the log output encoding.
type Format string

const (
	// FormatAuto uses FormatText when the output is a terminal and
	// FormatJSON otherwise.
	FormatAuto Format = "auto"
	// FormatJSON emits one JSON object per line via slog.JSONHandler.
	FormatJSON Format = "json"
	// FormatText emits human-readable lines via TextHandler.
	FormatText Format = "text"
)

// Formats lists the accepted format names, for help text and validation.
var Formats = []Format{FormatAuto, FormatJSON, FormatText}

// ParseFormat converts a user-supplied string into a Format.
func ParseFormat(s string) (Format, error) {
	f := Format(strings.ToLower(strings.TrimSpace(s)))
	for _, known := range Formats {
		if f == known {
			return f, nil
		}
	}
	return "", fmt.Errorf("unknown log format %q (want one of: %s)", s, joinFormats())
}

func joinFormats() string {
	names := make([]string, len(Formats))
	for i, f := range Formats {
		names[i] = string(f)
	}
	return strings.Join(names, ", ")
}

// ParseLevel converts a user-supplied string such as "debug", "INFO" or
// "warn+2" into a slog.Level.
func ParseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.TrimSpace(s))); err != nil {
		return 0, fmt.Errorf("unknown log level %q (want one of: debug, info, warn, error)", s)
	}
	return l, nil
}

// Options control the logger built by New.
type Options struct {
	// Format selects the encoding. Zero value means FormatAuto.
	Format Format
	// Level is the minimum level to emit. Zero value means slog.LevelInfo.
	Level slog.Leveler
	// Output is where log lines are written. Nil means os.Stderr.
	Output io.Writer
	// AddSource includes the file:line of the log call in each record.
	AddSource bool
	// Color forces ANSI colour on or off for FormatText. Nil means
	// auto-detect: on when Output is a terminal and NO_COLOR is unset.
	Color *bool
}

// New returns a slog.Logger configured by opts.
func New(opts Options) *slog.Logger {
	return slog.New(NewHandler(opts))
}

// NewHandler returns the slog.Handler that New wraps, for callers that want
// to compose it with their own handler middleware.
func NewHandler(opts Options) slog.Handler {
	w := opts.Output
	if w == nil {
		w = os.Stderr
	}
	level := opts.Level
	if level == nil {
		level = slog.LevelInfo
	}

	format := opts.Format
	if format == "" || format == FormatAuto {
		if isTerminal(w) {
			format = FormatText
		} else {
			format = FormatJSON
		}
	}

	switch format {
	case FormatText:
		color := false
		if opts.Color != nil {
			color = *opts.Color
		} else {
			color = isTerminal(w) && os.Getenv("NO_COLOR") == ""
		}
		return NewTextHandler(w, &TextOptions{
			Level:     level,
			AddSource: opts.AddSource,
			Color:     color,
		})
	default:
		return slog.NewJSONHandler(w, &slog.HandlerOptions{
			Level:     level,
			AddSource: opts.AddSource,
		})
	}
}

// isTerminal reports whether w is an *os.File attached to a character device.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
