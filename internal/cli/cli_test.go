package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersionCommand(t *testing.T) {
	code, out, _ := run(t, "version")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	if !strings.HasPrefix(out, "agent ") {
		t.Fatalf("stdout = %q, want prefix %q", out, "agent ")
	}
}

func TestVersionFlag(t *testing.T) {
	code, out, _ := run(t, "--version")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	if !strings.HasPrefix(out, "agent ") {
		t.Fatalf("stdout = %q, want prefix %q", out, "agent ")
	}
}

func TestNoArgsPrintsUsage(t *testing.T) {
	code, _, errOut := run(t)
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(errOut, "Usage:") {
		t.Fatalf("stderr = %q, want usage text", errOut)
	}
}

func TestHelpCommand(t *testing.T) {
	code, out, _ := run(t, "help")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(out, "Usage:") {
		t.Fatalf("stdout = %q, want usage text", out)
	}
}

func TestUnknownCommand(t *testing.T) {
	code, _, errOut := run(t, "bogus")
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(errOut, `unknown command "bogus"`) {
		t.Fatalf("stderr = %q, want unknown command message", errOut)
	}
}

func TestLogFlagsJSON(t *testing.T) {
	code, _, errOut := run(t, "--log-level", "debug", "--log-format", "json", "version")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(errOut, `"msg":"running command"`) || !strings.Contains(errOut, `"command":"version"`) {
		t.Fatalf("stderr = %q, want JSON debug log line", errOut)
	}
}

func TestLogFlagsText(t *testing.T) {
	code, _, errOut := run(t, "--log-level", "debug", "--log-format", "text", "version")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	if !strings.Contains(errOut, "DEBUG running command") || !strings.Contains(errOut, "command=version") {
		t.Fatalf("stderr = %q, want text debug log line", errOut)
	}
}

func TestLogFlagsInvalid(t *testing.T) {
	for _, args := range [][]string{{"--log-level", "loud", "version"}, {"--log-format", "yaml", "version"}} {
		code, _, errOut := run(t, args...)
		if code != ExitUsage {
			t.Errorf("%v: exit code = %d, want %d", args, code, ExitUsage)
		}
		if !strings.Contains(errOut, "unknown log") {
			t.Errorf("%v: stderr = %q, want unknown log error", args, errOut)
		}
	}
}
