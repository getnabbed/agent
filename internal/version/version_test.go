package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	s := String()
	if s == "" {
		t.Fatal("String() returned empty string")
	}

	if !strings.Contains(s, runtime.GOOS+"/"+runtime.GOARCH) {
		t.Fatalf("String() = %q, want to contain %s/%s", s, runtime.GOOS, runtime.GOARCH)
	}
}
