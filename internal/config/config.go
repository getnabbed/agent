// Package config resolves the agent's settings from, in increasing order
// of precedence: built-in defaults, an optional YAML file, NABBED_*
// environment variables, then command-line flags (applied by the caller).
//
// Every environment variable also accepts a _FILE variant naming a file to
// read the value from (NABBED_NNTP_FILE=/run/secrets/nntp), the Docker and
// Kubernetes convention for secrets.
//
// The file is meant to be owned by the host and mounted read-only. It will
// also carry the local policy from design 0001 (allowed roots, ceilings),
// which the cloud cannot change.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/getnabbed/agent/internal/logging"
	"github.com/getnabbed/agent/internal/paths"
)

// EnvConfig names the config file.
const EnvConfig = "NABBED_CONFIG"

// Config is the agent's resolved configuration.
type Config struct {
	APIURL  string `yaml:"api_url"`
	Name    string `yaml:"name"`
	DataDir string `yaml:"data_dir"`

	Log struct {
		Level  string `yaml:"level"`
		Format string `yaml:"format"`
	} `yaml:"log"`

	TLS struct {
		CAFile string `yaml:"ca_file"`
	} `yaml:"tls"`

	Downloads struct {
		// Dir defaults to <data_dir>/downloads.
		Dir           string `yaml:"dir"`
		MaxConcurrent int    `yaml:"max_concurrent"`
	} `yaml:"downloads"`

	Torrent struct {
		Port int `yaml:"port"`
		// AllowLAN lets torrents reach peers, trackers and web seeds on the
		// local network. Off by default: torrent metadata is untrusted.
		AllowLAN bool `yaml:"allow_lan"`
	} `yaml:"torrent"`

	Usenet struct {
		// Provider is temporary until sources are sealed to the agent.
		Provider Secret `yaml:"provider"`
	} `yaml:"usenet"`
}

// Secret is a string that never prints.
type Secret string

func (s Secret) String() string {
	if s == "" {
		return ""
	}

	return "<redacted>"
}

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

// MarshalYAML redacts the value; Config is only marshalled for display.
func (s Secret) MarshalYAML() (any, error) { return s.String(), nil }

// Default returns the built-in defaults.
func Default() *Config {
	c := &Config{DataDir: paths.DefaultDataDir()}
	c.Name, _ = os.Hostname()
	c.Log.Level = "info"
	c.Log.Format = "auto"
	c.Downloads.MaxConcurrent = 3

	return c
}

// Source describes where the config file came from, for logs.
type Source struct {
	Path   string
	Loaded bool
	// Warnings are problems worth logging that do not stop the agent.
	Warnings []string
}

// Load returns defaults overlaid with the config file and the environment.
// path is the --config flag value; when empty, NABBED_CONFIG and then the
// platform default are tried, and a missing file there is not an error.
func Load(path string, getenv func(string) string) (*Config, Source, error) {
	c := Default()

	required := path != ""
	if path == "" {
		path = getenv(EnvConfig)
	}

	if path == "" {
		path = paths.DefaultConfigFile()
	}

	src := Source{Path: path}

	if err := c.loadFile(path); err != nil {
		if !errors.Is(err, fs.ErrNotExist) || required {
			return nil, src, err
		}
	} else {
		src.Loaded = true

		if c.Usenet.Provider != "" && worldReadable(path) {
			src.Warnings = append(src.Warnings, path+" holds a usenet password but other users can read it; chmod 600 it, or use NABBED_NNTP_FILE")
		}
	}

	if err := c.applyEnv(getenv); err != nil {
		return nil, src, err
	}

	return c, src, nil
}

func (c *Config) loadFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	if err := dec.Decode(c); err != nil && !errors.Is(err, errEOF) {
		return fmt.Errorf("config %s: %w", path, err)
	}

	return nil
}

var errEOF = func() error {
	err := yaml.NewDecoder(strings.NewReader("")).Decode(&struct{}{})
	return err
}()

// envVar binds one setting to its environment variable.
type envVar struct {
	name string
	set  func(c *Config, v string) error
}

func str(dst func(*Config) *string) func(*Config, string) error {
	return func(c *Config, v string) error {
		*dst(c) = v
		return nil
	}
}

func num(name string, dst func(*Config) *int) func(*Config, string) error {
	return func(c *Config, v string) error {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("%s: %q is not a number", name, v)
		}

		*dst(c) = n

		return nil
	}
}

func boolean(name string, dst func(*Config) *bool) func(*Config, string) error {
	return func(c *Config, v string) error {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("%s: %q is not true or false", name, v)
		}

		*dst(c) = b

		return nil
	}
}

// Env lists every environment variable the agent reads, in documentation
// order.
var Env = []envVar{
	{"NABBED_API_URL", str(func(c *Config) *string { return &c.APIURL })},
	{"NABBED_NAME", str(func(c *Config) *string { return &c.Name })},
	{"NABBED_DATA_DIR", str(func(c *Config) *string { return &c.DataDir })},
	{"NABBED_LOG_LEVEL", str(func(c *Config) *string { return &c.Log.Level })},
	{"NABBED_LOG_FORMAT", str(func(c *Config) *string { return &c.Log.Format })},
	{"NABBED_CA_FILE", str(func(c *Config) *string { return &c.TLS.CAFile })},
	{"NABBED_DOWNLOAD_DIR", str(func(c *Config) *string { return &c.Downloads.Dir })},
	{"NABBED_MAX_DOWNLOADS", num("NABBED_MAX_DOWNLOADS", func(c *Config) *int { return &c.Downloads.MaxConcurrent })},
	{"NABBED_TORRENT_PORT", num("NABBED_TORRENT_PORT", func(c *Config) *int { return &c.Torrent.Port })},
	{"NABBED_TORRENT_ALLOW_LAN", boolean("NABBED_TORRENT_ALLOW_LAN", func(c *Config) *bool { return &c.Torrent.AllowLAN })},
	{"NABBED_NNTP", func(c *Config, v string) error { c.Usenet.Provider = Secret(v); return nil }},
}

// Name returns the variable's name.
func (e envVar) Name() string { return e.name }

func (c *Config) applyEnv(getenv func(string) string) error {
	for _, e := range Env {
		v, ok, err := lookup(e.name, getenv)
		if err != nil {
			return err
		}

		if !ok {
			continue
		}

		if err := e.set(c, v); err != nil {
			return err
		}
	}

	return nil
}

// lookup reads NAME, or the contents of the file named by NAME_FILE.
// Setting both is an error, since it is ambiguous which one wins.
func lookup(name string, getenv func(string) string) (string, bool, error) {
	v, file := getenv(name), getenv(name+"_FILE")

	switch {
	case v != "" && file != "":
		return "", false, fmt.Errorf("both %s and %s_FILE are set; use one", name, name)
	case file != "":
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", false, fmt.Errorf("%s_FILE: %w", name, err)
		}

		return strings.TrimRight(string(raw), "\r\n"), true, nil
	case v != "":
		return v, true, nil
	}

	return "", false, nil
}

// Finish fills derived defaults and validates. Call it after applying
// flags.
func (c *Config) Finish() error {
	if c.Downloads.Dir == "" {
		c.Downloads.Dir = filepath.Join(c.DataDir, "downloads")
	}

	if _, err := logging.ParseLevel(c.Log.Level); err != nil {
		return err
	}

	if _, err := logging.ParseFormat(c.Log.Format); err != nil {
		return err
	}

	if c.Downloads.MaxConcurrent < 1 || c.Downloads.MaxConcurrent > 64 {
		return fmt.Errorf("downloads.max_concurrent must be 1-64, got %d", c.Downloads.MaxConcurrent)
	}

	if c.Torrent.Port < 0 || c.Torrent.Port > 65535 {
		return fmt.Errorf("torrent.port must be 0-65535, got %d", c.Torrent.Port)
	}

	if c.APIURL != "" {
		if _, err := url.Parse(c.APIURL); err != nil {
			return fmt.Errorf("api_url: %w", err)
		}
	}

	return nil
}

// Redacted renders the config as YAML with secrets hidden.
func (c *Config) Redacted() string {
	var b bytes.Buffer

	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)

	if err := enc.Encode(c); err != nil {
		return err.Error()
	}

	return b.String()
}
