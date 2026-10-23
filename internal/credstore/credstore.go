// Package credstore persists the agent's credentials: identity keys, the
// rotating refresh token and the pairing metadata.
//
// Everything lives in one file, credentials.json, inside the data directory.
// One file means one atomic replace, so the refresh token and the refresh
// intent can never be half-written relative to each other. The access token
// is never persisted.
//
// Protection layers, outermost first:
//
//  1. The data directory is owner-only (0700 on Unix, a protected DACL for
//     SYSTEM, Administrators and the owner on Windows).
//  2. The file is written 0600 via temp file + fsync + rename.
//  3. The payload is wrapped by a platform Protector where one exists without
//     user interaction (DPAPI on Windows). Elsewhere the file is plaintext
//     JSON and relies on 1 and 2, like an SSH private key.
package credstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// FileName is the credentials file inside the data directory.
const FileName = "credentials.json"

const formatVersion = 1

// ErrNotPaired is returned by Load when no credentials exist yet.
var ErrNotPaired = errors.New("agent is not paired")

// Credentials is everything the agent needs to authenticate as itself.
type Credentials struct {
	AgentID    string `json:"agent_id"`
	TenantID   string `json:"tenant_id"`
	APIURL     string `json:"api_url"`
	GatewayURL string `json:"gateway_url"`

	// SignSeed is the Ed25519 identity seed. Secret.
	SignSeed []byte `json:"sign_seed"`
	// SealKey is the X25519 private key. Secret.
	SealKey []byte `json:"seal_key"`
	// RefreshToken is single-use and rotated on every refresh. Secret.
	RefreshToken string `json:"refresh_token"`
	// PendingRefresh is the write-ahead record of a refresh in flight. It is
	// written before the refresh token is spent and cleared once the new
	// token is saved. See auth.TokenSource.
	PendingRefresh *PendingRefresh `json:"pending_refresh,omitempty"`

	PairedAt    time.Time `json:"paired_at"`
	RefreshedAt time.Time `json:"refreshed_at,omitzero"`
}

// PendingRefresh records a refresh attempt so that a crash between "server
// rotated the token" and "agent saved the new one" can be retried with the
// same nonce, which the server answers idempotently instead of treating as
// refresh-token reuse.
type PendingRefresh struct {
	Nonce        string    `json:"nonce"`
	RefreshToken string    `json:"refresh_token"`
	StartedAt    time.Time `json:"started_at"`
}

// LogValue keeps secrets out of logs.
func (c *Credentials) LogValue() slog.Value {
	if c == nil {
		return slog.StringValue("<nil>")
	}
	return slog.GroupValue(
		slog.String("agent_id", c.AgentID),
		slog.String("tenant_id", c.TenantID),
		slog.String("api_url", c.APIURL),
		slog.String("gateway_url", c.GatewayURL),
		slog.Bool("refresh_pending", c.PendingRefresh != nil),
	)
}

// Clone returns a deep copy.
func (c *Credentials) Clone() *Credentials {
	out := *c
	out.SignSeed = append([]byte(nil), c.SignSeed...)
	out.SealKey = append([]byte(nil), c.SealKey...)
	if c.PendingRefresh != nil {
		pr := *c.PendingRefresh
		out.PendingRefresh = &pr
	}
	return &out
}

// envelope is the on-disk wrapper that names the protector used.
type envelope struct {
	Format    int    `json:"format"`
	Protector string `json:"protector"`
	Data      []byte `json:"data"`
}

// Store reads and writes credentials in a data directory.
type Store struct {
	dir  string
	prot Protector
	log  *slog.Logger
}

// Open prepares dir (creating it owner-only if needed) and returns a Store
// that protects new writes with the platform default Protector.
func Open(dir string, log *slog.Logger) (*Store, error) {
	return OpenWith(dir, PlatformProtector(), log)
}

// OpenWith is Open with an explicit Protector.
func OpenWith(dir string, prot Protector, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := secureDir(dir, log); err != nil {
		return nil, fmt.Errorf("prepare data dir %s: %w", dir, err)
	}
	return &Store{dir: dir, prot: prot, log: log}, nil
}

// Dir returns the data directory.
func (s *Store) Dir() string { return s.dir }

// Path returns the credentials file path.
func (s *Store) Path() string { return filepath.Join(s.dir, FileName) }

// Protector returns the protector used for writes.
func (s *Store) Protector() Protector { return s.prot }

// Load reads credentials, returning ErrNotPaired if there are none.
func (s *Store) Load() (*Credentials, error) {
	raw, err := os.ReadFile(s.Path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotPaired
	}
	if err != nil {
		return nil, err
	}
	checkFileMode(s.Path(), s.log)

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.Path(), err)
	}
	if env.Format != formatVersion {
		return nil, fmt.Errorf("%s: unsupported format %d", s.Path(), env.Format)
	}
	prot, err := protectorByName(env.Protector)
	if err != nil {
		return nil, err
	}
	plain, err := prot.Unprotect(env.Data)
	if err != nil {
		return nil, fmt.Errorf("unprotect credentials (%s): %w", env.Protector, err)
	}
	var c Credentials
	if err := json.Unmarshal(plain, &c); err != nil {
		return nil, fmt.Errorf("decode credentials: %w", err)
	}
	return &c, nil
}

// Save atomically replaces the credentials file.
func (s *Store) Save(c *Credentials) error {
	plain, err := json.Marshal(c)
	if err != nil {
		return err
	}
	data, err := s.prot.Protect(plain)
	if err != nil {
		return fmt.Errorf("protect credentials (%s): %w", s.prot.Name(), err)
	}
	out, err := json.MarshalIndent(envelope{Format: formatVersion, Protector: s.prot.Name(), Data: data}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.Path(), append(out, '\n'))
}

// Delete removes the credentials file.
func (s *Store) Delete() error {
	err := os.Remove(s.Path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// writeFileAtomic writes data to a temp file in the same directory, fsyncs
// it, renames it over path and fsyncs the directory, so a crash leaves
// either the old file or the new one, never a torn mix.
func writeFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".credentials-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(0o600); err != nil && !isChmodUnsupported(err) {
		_ = f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}
