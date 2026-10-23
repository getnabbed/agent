package cli

import (
	"bytes"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Run(args, &out, &errb)
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
