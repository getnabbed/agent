package logging

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode"
)

// sanitizer wraps a handler and escapes control and format characters
// (newlines, ESC, bidi overrides) in messages, keys and string values.
// Log text often carries untrusted data, such as peer and server error
// strings or file names from releases; unescaped, it could forge log lines
// or inject terminal escape sequences when logs are viewed.
type sanitizer struct{ next slog.Handler }

// Sanitize wraps h. NewHandler applies it to both built-in formats.
func Sanitize(h slog.Handler) slog.Handler { return sanitizer{next: h} }

func (s sanitizer) Enabled(ctx context.Context, l slog.Level) bool { return s.next.Enabled(ctx, l) }

func (s sanitizer) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, clean(r.Message), r.PC)

	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(cleanAttr(a))
		return true
	})

	return s.next.Handle(ctx, out)
}

func (s sanitizer) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = cleanAttr(a)
	}

	return sanitizer{next: s.next.WithAttrs(clean)}
}

func (s sanitizer) WithGroup(name string) slog.Handler {
	return sanitizer{next: s.next.WithGroup(clean(name))}
}

func cleanAttr(a slog.Attr) slog.Attr {
	a.Key = clean(a.Key)
	a.Value = a.Value.Resolve()

	switch a.Value.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(clean(a.Value.String()))
	case slog.KindGroup:
		group := a.Value.Group()
		out := make([]slog.Attr, len(group))

		for i, g := range group {
			out[i] = cleanAttr(g)
		}

		a.Value = slog.GroupValue(out...)
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			a.Value = slog.StringValue(clean(err.Error()))
		}
	}

	return a
}

func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

// clean escapes unsafe runes as \uXXXX; safe strings are returned as-is.
func clean(s string) string {
	if !strings.ContainsFunc(s, unsafeRune) {
		return s
	}

	var b strings.Builder

	for _, r := range s {
		if unsafeRune(r) {
			fmt.Fprintf(&b, `\u%04x`, r)
			continue
		}

		b.WriteRune(r)
	}

	return b.String()
}
