package lock

import (
	"errors"
	"testing"
)

func TestExclusive(t *testing.T) {
	dir := t.TempDir()
	l1, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Acquire(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Acquire err = %v, want ErrLocked", err)
	}

	if err := l1.Release(); err != nil {
		t.Fatal(err)
	}

	l2, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}

	_ = l2.Release()
}
