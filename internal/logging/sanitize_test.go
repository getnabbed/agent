package logging

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestSanitizeEscapesInjection(t *testing.T) {
	for _, format := range []Format{FormatText, FormatJSON} {
		var buf bytes.Buffer
		l := New(Options{Format: format, Output: &buf})

		l.Info("peer said: ok\n12:00:00.000 ERROR forged line\x1b[2J",
			"file", "x\u202egnp.exe",
			"bad\nkey", "v",
			"err", errors.New("boom\r\ninjected"))

		out := buf.String()
		if strings.Count(out, "\n") != 1 {
			t.Errorf("%s: output spans lines:\n%s", format, out)
		}

		for _, raw := range []string{"\x1b", "\u202e", "\r"} {
			if strings.Contains(out, raw) {
				t.Errorf("%s: raw %q in output: %q", format, raw, out)
			}
		}
	}
}

func TestSanitizeLeavesSafeTextAlone(t *testing.T) {
	if got := clean("plain text, ünïcödé"); got != "plain text, ünïcödé" {
		t.Fatalf("clean altered safe text: %q", got)
	}
}

var _ slog.Handler = Sanitize(nil)
