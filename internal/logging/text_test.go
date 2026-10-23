package logging

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/slogtest"
	"time"
)

var timeRE = regexp.MustCompile(`^\d\d:\d\d:\d\d\.\d{3}$`)
var attrRE = regexp.MustCompile(`^[^\s="]+=`)

// parseLine converts one TextHandler line back into the nested map form that
// testing/slogtest expects.
func parseLine(t *testing.T, line string) map[string]any {
	t.Helper()
	toks := tokenize(t, line)
	m := map[string]any{}

	i := 0
	if i < len(toks) && timeRE.MatchString(toks[i]) {
		m[slog.TimeKey] = toks[i]
		i++
	}

	if i >= len(toks) {
		t.Fatalf("line %q has no level", line)
	}

	m[slog.LevelKey] = strings.TrimSpace(toks[i])
	i++

	var msg []string
	for i < len(toks) && !attrRE.MatchString(toks[i]) {
		msg = append(msg, toks[i])
		i++
	}

	m[slog.MessageKey] = strings.Join(msg, " ")

	for ; i < len(toks); i++ {
		k, v, _ := strings.Cut(toks[i], "=")
		if uq, err := strconv.Unquote(v); err == nil {
			v = uq
		}

		cur := m
		parts := strings.Split(k, ".")
		for _, p := range parts[:len(parts)-1] {
			next, ok := cur[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[p] = next
			}

			cur = next
		}

		cur[parts[len(parts)-1]] = v
	}

	return m
}

// tokenize splits on spaces, keeping double-quoted sections together.
func tokenize(t *testing.T, line string) []string {
	t.Helper()
	var toks []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && inQuote && i+1 < len(line):
			cur.WriteByte(c)
			i++
			cur.WriteByte(line[i])
		case c == '"':
			inQuote = !inQuote
			cur.WriteByte(c)
		case c == ' ' && !inQuote:
			if cur.Len() > 0 {
				toks = append(toks, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}

	if cur.Len() > 0 {
		toks = append(toks, cur.String())
	}

	return toks
}

func TestTextHandlerConformance(t *testing.T) {
	var buf bytes.Buffer
	h := NewTextHandler(&buf, &TextOptions{Level: slog.LevelDebug})
	results := func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}

			out = append(out, parseLine(t, line))
		}

		return out
	}

	if err := slogtest.TestHandler(h, results); err != nil {
		t.Fatal(err)
	}
}

func TestTextHandlerFormat(t *testing.T) {
	var buf bytes.Buffer
	h := NewTextHandler(&buf, nil)
	l := slog.New(h.WithAttrs([]slog.Attr{slog.String("svc", "agent")}).WithGroup("http"))
	ts := time.Date(2026, 10, 23, 12, 34, 56, 789_000_000, time.UTC)

	r := slog.NewRecord(ts, slog.LevelWarn, "slow request", 0)
	r.AddAttrs(
		slog.String("path", "/a b"),
		slog.Int("status", 503),
		slog.Duration("dur", 1500*time.Millisecond),
		slog.Any("err", errors.New("boom=bad")),
		slog.String("empty", ""),
		slog.Bool("ok", false),
	)
	if err := l.Handler().Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}

	want := `12:34:56.789 WARN  slow request svc=agent http.path="/a b" http.status=503 http.dur=1.5s http.err="boom=bad" http.empty="" http.ok=false` + "\n"
	if got := buf.String(); got != want {
		t.Errorf("got:  %q\nwant: %q", got, want)
	}
}

func TestTextHandlerColor(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(NewTextHandler(&buf, &TextOptions{Color: true}))
	l.Error("bad", "k", "v")
	got := buf.String()
	if !strings.Contains(got, ansiRed+"ERROR"+ansiReset) {
		t.Errorf("expected coloured level in %q", got)
	}

	if !strings.Contains(got, ansiFaint+"k="+ansiReset+"v") {
		t.Errorf("expected faint key in %q", got)
	}
}

func TestTextHandlerNoColorByDefault(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(NewTextHandler(&buf, nil))
	l.Info("plain")
	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("unexpected escape codes in %q", buf.String())
	}
}

func TestTextHandlerAddSource(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(NewTextHandler(&buf, &TextOptions{AddSource: true}))
	l.Info("where")
	if !strings.Contains(buf.String(), "source=") || !strings.Contains(buf.String(), "source=logging/text_test.go:") {
		t.Errorf("expected source attr in %q", buf.String())
	}
}
