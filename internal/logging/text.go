package logging

import (
	"context"
	"encoding"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// TextOptions configure a TextHandler.
type TextOptions struct {
	// Level is the minimum level to emit. Nil means slog.LevelInfo.
	Level slog.Leveler
	// AddSource appends a source=file:line attribute to each record.
	AddSource bool
	// Color enables ANSI colour escapes.
	Color bool
	// TimeFormat is the layout used for the leading timestamp. Empty means
	// "15:04:05.000". Records with a zero time omit the timestamp.
	TimeFormat string
}

// TextHandler is a slog.Handler that writes compact, human-readable lines:
//
//	15:04:05.000 INFO  server started addr=:8080 tls=false
//	15:04:05.001 WARN  slow request path="/api/v1/items" dur=1.2s
//
// Attributes are rendered as key=value pairs, with nested groups flattened
// to dotted keys (http.method=GET). Values are quoted only when needed.
type TextHandler struct {
	opts TextOptions

	mu *sync.Mutex
	w  io.Writer

	// prefix holds attrs added via WithAttrs, already formatted.
	prefix string
	// groups holds the open group names, joined by "." when rendering keys.
	groups []string
}

// NewTextHandler returns a TextHandler writing to w. A nil opts uses defaults.
func NewTextHandler(w io.Writer, opts *TextOptions) *TextHandler {
	h := &TextHandler{mu: &sync.Mutex{}, w: w}
	if opts != nil {
		h.opts = *opts
	}
	if h.opts.Level == nil {
		h.opts.Level = slog.LevelInfo
	}
	if h.opts.TimeFormat == "" {
		h.opts.TimeFormat = "15:04:05.000"
	}
	return h
}

// Enabled implements slog.Handler.
func (h *TextHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.opts.Level.Level()
}

// WithAttrs implements slog.Handler.
func (h *TextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	h2 := h.clone()
	var sb strings.Builder
	sb.WriteString(h.prefix)
	for _, a := range attrs {
		h2.appendAttr(&sb, h.groupPrefix(), a)
	}
	h2.prefix = sb.String()
	return h2
}

// WithGroup implements slog.Handler.
func (h *TextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := h.clone()
	h2.groups = append(h2.groups[:len(h2.groups):len(h2.groups)], name)
	return h2
}

func (h *TextHandler) clone() *TextHandler {
	return &TextHandler{
		opts:   h.opts,
		mu:     h.mu, // shared: all clones serialise writes to the same w
		w:      h.w,
		prefix: h.prefix,
		groups: h.groups,
	}
}

func (h *TextHandler) groupPrefix() string {
	if len(h.groups) == 0 {
		return ""
	}
	return strings.Join(h.groups, ".") + "."
}

// Handle implements slog.Handler.
func (h *TextHandler) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder

	if !r.Time.IsZero() {
		h.color(&sb, ansiFaint, r.Time.Format(h.opts.TimeFormat))
		sb.WriteByte(' ')
	}

	h.color(&sb, levelColor(r.Level), levelLabel(r.Level))
	sb.WriteByte(' ')

	sb.WriteString(r.Message)

	if h.opts.AddSource && r.PC != 0 {
		fs := runtime.CallersFrames([]uintptr{r.PC})
		f, _ := fs.Next()
		if f.File != "" {
			h.writeKV(&sb, slog.SourceKey, shortFile(f.File)+":"+strconv.Itoa(f.Line))
		}
	}

	sb.WriteString(h.prefix)

	gp := h.groupPrefix()
	r.Attrs(func(a slog.Attr) bool {
		h.appendAttr(&sb, gp, a)
		return true
	})

	sb.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, sb.String())
	return err
}

// appendAttr writes " key=value" for a, flattening groups with prefix.
func (h *TextHandler) appendAttr(sb *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		attrs := a.Value.Group()
		if len(attrs) == 0 {
			return
		}
		if a.Key != "" {
			prefix += a.Key + "."
		}
		for _, ga := range attrs {
			h.appendAttr(sb, prefix, ga)
		}
		return
	}
	h.writeKV(sb, prefix+a.Key, formatValue(a.Value))
}

func (h *TextHandler) writeKV(sb *strings.Builder, key, val string) {
	sb.WriteByte(' ')
	h.color(sb, ansiFaint, key+"=")
	sb.WriteString(val)
}

func (h *TextHandler) color(sb *strings.Builder, code, s string) {
	if !h.opts.Color || code == "" {
		sb.WriteString(s)
		return
	}
	sb.WriteString(code)
	sb.WriteString(s)
	sb.WriteString(ansiReset)
}

// formatValue renders a non-group slog.Value as text, quoting when needed.
func formatValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		return quote(v.String())
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(v.Float64(), 'g', -1, 64)
	case slog.KindBool:
		return strconv.FormatBool(v.Bool())
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return quote(v.Time().Format(time.RFC3339Nano))
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return quote(x.Error())
		case encoding.TextMarshaler:
			if b, err := x.MarshalText(); err == nil {
				return quote(string(b))
			}
		case fmt.Stringer:
			return quote(x.String())
		}
		return quote(fmt.Sprintf("%+v", v.Any()))
	default:
		return quote(v.String())
	}
}

// quote wraps s in double quotes when it is empty or contains whitespace,
// quotes, '=' or non-printable characters.
func quote(s string) string {
	if s == "" {
		return `""`
	}
	for _, r := range s {
		if r == '"' || r == '=' || r == utf8.RuneError || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return strconv.Quote(s)
		}
	}
	return s
}

const (
	ansiReset  = "\x1b[0m"
	ansiFaint  = "\x1b[2m"
	ansiCyan   = "\x1b[36m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiRed    = "\x1b[31m"
)

func levelColor(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return ansiCyan
	case l < slog.LevelWarn:
		return ansiGreen
	case l < slog.LevelError:
		return ansiYellow
	default:
		return ansiRed
	}
}

// levelLabel returns a fixed-width label so messages align in a column.
func levelLabel(l slog.Level) string {
	switch l {
	case slog.LevelDebug:
		return "DEBUG"
	case slog.LevelInfo:
		return "INFO "
	case slog.LevelWarn:
		return "WARN "
	case slog.LevelError:
		return "ERROR"
	default:
		return l.String()
	}
}

// shortFile trims a source path to its last two elements (pkg/file.go) so
// text logs stay readable with -trimpath builds.
func shortFile(path string) string {
	n := 0
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			n++
			if n == 2 {
				return path[i+1:]
			}
		}
	}
	return path
}
