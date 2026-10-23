package credstore

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenWith(filepath.Join(t.TempDir(), "data"), PlaintextProtector{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func TestLoadNotPaired(t *testing.T) {
	if _, err := testStore(t).Load(); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("Load() err = %v, want ErrNotPaired", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := testStore(t)
	in := &Credentials{
		AgentID: "a1", TenantID: "t1", APIURL: "https://api.example", GatewayURL: "https://gw.example",
		SignSeed: bytes.Repeat([]byte{1}, 32), SealKey: bytes.Repeat([]byte{2}, 32),
		RefreshToken:   "r1",
		PendingRefresh: &PendingRefresh{Nonce: "n", RefreshToken: "r1", StartedAt: time.Unix(1, 0).UTC()},
		PairedAt:       time.Unix(2, 0).UTC(),
	}

	if err := s.Save(in); err != nil {
		t.Fatal(err)
	}

	out, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}

	if out.AgentID != "a1" || out.RefreshToken != "r1" || !bytes.Equal(out.SealKey, in.SealKey) ||
		out.PendingRefresh == nil || out.PendingRefresh.Nonce != "n" || !out.PairedAt.Equal(in.PairedAt) {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not apply on Windows")
	}

	s := testStore(t)
	if err := s.Save(&Credentials{AgentID: "a"}); err != nil {
		t.Fatal(err)
	}

	for path, want := range map[string]os.FileMode{s.Dir(): 0o700, s.Path(): 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}

		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %v, want %v", path, got, want)
		}
	}
}

func TestSaveLeavesNoTempFiles(t *testing.T) {
	s := testStore(t)
	for range 3 {
		if err := s.Save(&Credentials{AgentID: "a"}); err != nil {
			t.Fatal(err)
		}
	}

	entries, _ := os.ReadDir(s.Dir())
	if len(entries) != 1 {
		t.Fatalf("expected only %s, found %d entries", FileName, len(entries))
	}
}

func TestUnknownProtectorRejected(t *testing.T) {
	s := testStore(t)
	if err := os.WriteFile(s.Path(), []byte(`{"format":1,"protector":"tpm2","data":""}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := s.Load()
	if err == nil || !strings.Contains(err.Error(), "tpm2") {
		t.Fatalf("Load() err = %v, want unknown protector error", err)
	}
}

func TestLogValueRedactsSecrets(t *testing.T) {
	var buf bytes.Buffer
	c := &Credentials{AgentID: "a1", RefreshToken: "super-secret", SignSeed: []byte("seed-bytes")}
	slog.New(slog.NewTextHandler(&buf, nil)).Info("creds", "c", c)
	if strings.Contains(buf.String(), "super-secret") || strings.Contains(buf.String(), "seed") {
		t.Fatalf("secret leaked into log: %s", buf.String())
	}
}

func TestSharedDirectoryIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not apply on Windows")
	}

	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "family-photos.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := OpenWith(dir, PlaintextProtector{}, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "not dedicated") {
		t.Fatalf("err = %v, want refusal of a shared directory", err)
	}

	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o755 {
		t.Fatalf("shared directory permissions were changed to %v", fi.Mode().Perm())
	}
}

func TestAgentOwnedLooseDirectoryIsTightened(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not apply on Windows")
	}

	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(filepath.Join(dir, "downloads"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := OpenWith(dir, PlaintextProtector{}, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v, want 0700", fi.Mode().Perm())
	}
}

func TestOpenExistingTouchesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")

	if _, err := OpenExisting(dir, nil).Load(); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("Load err = %v, want ErrNotPaired", err)
	}

	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("OpenExisting created the directory")
	}
}
