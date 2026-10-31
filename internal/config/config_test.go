package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func write(t *testing.T, body string) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return p
}

func TestPrecedenceFileThenEnv(t *testing.T) {
	p := write(t, "api_url: https://file.example\ndownloads:\n  max_concurrent: 5\ntorrent:\n  port: 6881\n")

	c, src, err := Load(p, env(map[string]string{"NABBED_MAX_DOWNLOADS": "7"}))
	if err != nil {
		t.Fatal(err)
	}

	if !src.Loaded || c.APIURL != "https://file.example" || c.Downloads.MaxConcurrent != 7 || c.Torrent.Port != 6881 {
		t.Fatalf("got %+v (loaded=%v)", c, src.Loaded)
	}

	if c.Log.Level != "info" {
		t.Fatalf("default not kept for unset key: level=%q", c.Log.Level)
	}
}

func TestConfigPathFromEnvIsOptional(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.yaml")

	if _, src, err := Load("", env(map[string]string{EnvConfig: missing})); err != nil || src.Loaded {
		t.Fatalf("missing env-named file: err=%v loaded=%v, want no error", err, src.Loaded)
	}

	if _, _, err := Load(missing, env(nil)); err == nil {
		t.Fatal("missing --config file must be an error")
	}
}

func TestUnknownKeyIsRejected(t *testing.T) {
	_, _, err := Load(write(t, "api_ulr: x\n"), env(nil))
	if err == nil || !strings.Contains(err.Error(), "api_ulr") {
		t.Fatalf("err = %v, want unknown field error naming api_ulr", err)
	}
}

func TestEmptyFileIsFine(t *testing.T) {
	if _, _, err := Load(write(t, "# nothing yet\n"), env(nil)); err != nil {
		t.Fatal(err)
	}
}

func TestFileVariant(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "nntp")
	if err := os.WriteFile(secret, []byte("nntps://u:p@h\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c, _, err := Load("", env(map[string]string{EnvConfig: "/nonexistent", "NABBED_NNTP_FILE": secret}))
	if err != nil {
		t.Fatal(err)
	}

	if c.Usenet.Provider != "nntps://u:p@h" {
		t.Fatalf("provider = %q, want trailing newline trimmed", string(c.Usenet.Provider))
	}

	_, _, err = Load("", env(map[string]string{EnvConfig: "/nonexistent", "NABBED_NNTP": "x", "NABBED_NNTP_FILE": secret}))
	if err == nil {
		t.Fatal("setting both NAME and NAME_FILE must be an error")
	}
}

func TestBadNumber(t *testing.T) {
	_, _, err := Load("", env(map[string]string{EnvConfig: "/nonexistent", "NABBED_TORRENT_PORT": "abc"}))
	if err == nil || !strings.Contains(err.Error(), "NABBED_TORRENT_PORT") {
		t.Fatalf("err = %v, want it to name the variable", err)
	}
}

func TestFinishDerivesAndValidates(t *testing.T) {
	c := Default()
	c.DataDir = "/data"

	if err := c.Finish(); err != nil {
		t.Fatal(err)
	}

	if c.Downloads.Dir != filepath.Join("/data", "downloads") {
		t.Fatalf("download dir = %q", c.Downloads.Dir)
	}

	for _, mutate := range []func(*Config){
		func(c *Config) { c.Log.Level = "loud" },
		func(c *Config) { c.Log.Format = "xml" },
		func(c *Config) { c.Downloads.MaxConcurrent = 0 },
		func(c *Config) { c.Torrent.Port = 70000 },
	} {
		bad := Default()
		mutate(bad)

		if err := bad.Finish(); err == nil {
			t.Errorf("Finish accepted %+v", bad)
		}
	}
}

func TestAllowedRanges(t *testing.T) {
	p := write(t, "network:\n  allowed_ranges: [192.168.1.10, \"fd00:1::/64\"]\n")

	c, _, err := Load(p, env(nil))
	if err != nil {
		t.Fatal(err)
	}

	if err := c.Finish(); err != nil || strings.Join(c.Network.AllowedRanges, " ") != "192.168.1.10 fd00:1::/64" {
		t.Fatalf("got %q, %v from the file", c.Network.AllowedRanges, err)
	}

	// The environment replaces the file's list; commas and spaces both separate.
	c, _, err = Load(p, env(map[string]string{"NABBED_ALLOWED_RANGES": "10.0.0.0/24, 10.0.1.5 fd00::/8"}))
	if err != nil {
		t.Fatal(err)
	}

	if strings.Join(c.Network.AllowedRanges, " ") != "10.0.0.0/24 10.0.1.5 fd00::/8" {
		t.Fatalf("got %q from the environment", c.Network.AllowedRanges)
	}

	bad := Default()
	bad.Network.AllowedRanges = []string{"192.168.1.10/24"}

	if err := bad.Finish(); err == nil || !strings.Contains(err.Error(), "network.allowed_ranges") || !strings.Contains(err.Error(), "192.168.1.0/24") {
		t.Fatalf("got %v, want an error naming the setting and the network meant", err)
	}
}

func TestRedactedHidesSecrets(t *testing.T) {
	c := Default()
	c.Usenet.Provider = "nntps://user:hunter2@news.example"

	if out := c.Redacted(); strings.Contains(out, "hunter2") || !strings.Contains(out, "<redacted>") {
		t.Fatalf("secret leaked or not marked:\n%s", out)
	}
}

func TestEveryEnvVarIsDocumented(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range Env {
		if !strings.Contains(string(readme), "`"+e.Name()+"`") {
			t.Errorf("%s is not documented in README.md", e.Name())
		}
	}
}

func TestWarnsOnReadableSecretFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not apply on Windows")
	}

	p := write(t, "usenet:\n  provider: nntps://u:p@h\n")
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}

	_, src, err := Load(p, env(nil))
	if err != nil || len(src.Warnings) != 1 {
		t.Fatalf("warnings = %v, err = %v; want one warning", src.Warnings, err)
	}

	_ = os.Chmod(p, 0o600)

	if _, src, _ := Load(p, env(nil)); len(src.Warnings) != 0 {
		t.Fatalf("warnings for a private file: %v", src.Warnings)
	}
}
