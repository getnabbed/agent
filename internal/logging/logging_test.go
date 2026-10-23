package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestParseFormat(t *testing.T) {
	cases := map[string]Format{"auto": FormatAuto, "JSON": FormatJSON, "text": FormatText}
	cases[" text "] = FormatText // surrounding whitespace is trimmed
	for in, want := range cases {
		got, err := ParseFormat(in)
		if err != nil || got != want {
			t.Errorf("ParseFormat(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseFormat("yaml"); err == nil {
		t.Error("ParseFormat(\"yaml\") expected error")
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug, "INFO": slog.LevelInfo,
		"warn": slog.LevelWarn, "error": slog.LevelError, "warn+2": slog.LevelWarn + 2,
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseLevel("loud"); err == nil {
		t.Error("ParseLevel(\"loud\") expected error")
	}
}

func TestNewJSON(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Format: FormatJSON, Output: &buf})
	l.Info("hello", "n", 1, slog.Group("g", "k", "v"))

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, buf.String())
	}
	if rec["msg"] != "hello" || rec["n"] != float64(1) {
		t.Errorf("unexpected record: %v", rec)
	}
	if g, ok := rec["g"].(map[string]any); !ok || g["k"] != "v" {
		t.Errorf("group not nested: %v", rec)
	}
}

func TestNewAutoFallsBackToJSONForNonTerminal(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Output: &buf})
	l.Info("hello")
	if !strings.HasPrefix(buf.String(), "{") {
		t.Errorf("expected JSON for non-terminal writer, got %q", buf.String())
	}
}

func TestNewLevelFilter(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Format: FormatText, Output: &buf, Level: slog.LevelWarn})
	l.Info("dropped")
	l.Warn("kept")
	if strings.Contains(buf.String(), "dropped") || !strings.Contains(buf.String(), "kept") {
		t.Errorf("level filter not applied: %q", buf.String())
	}
}
