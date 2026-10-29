package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestNumericVersionsGetAV(t *testing.T) {
	for in, want := range map[string]string{"0.1.0": "v0.1.0", "v0.1.0": "v0.1.0", "dev": "dev", "": ""} {
		if got := withV(in); got != want {
			t.Errorf("withV(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestString(t *testing.T) {
	s := String()
	if s == "" {
		t.Fatal("String() returned empty string")
	}

	if !strings.Contains(s, runtime.GOOS+"/"+runtime.GOARCH) {
		t.Fatalf("String() = %q, want to contain %s/%s", s, runtime.GOOS, runtime.GOARCH)
	}
}
