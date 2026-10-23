package ids

import (
	"regexp"
	"testing"
	"time"
)

var v7 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewV7Format(t *testing.T) {
	id := NewV7()
	if !v7.MatchString(id) {
		t.Fatalf("NewV7() = %q, not a UUIDv7", id)
	}
}

func TestNewV7SortsByTime(t *testing.T) {
	a := NewV7()
	time.Sleep(2 * time.Millisecond)

	b := NewV7()
	if a >= b {
		t.Fatalf("ids not time-ordered: %s >= %s", a, b)
	}
}
