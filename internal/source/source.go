// Package source manages download sources configured from the control
// plane (PutSource, RemoveSource). Each source's configuration arrives
// sealed to the agent's X25519 key, so the cloud stores it but cannot read
// it. The agent keeps it sealed at rest and opens it only in memory.
//
// The control plane can seal a configuration too, since anyone with the
// public key can. A source is therefore treated as untrusted input: its
// host must be public, unless the host owner allowed its address in the
// agent's own configuration (network.allowed_ranges), and dials go through
// the netguard LAN policy.
package source

import (
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"regexp"
	"sync"
	"time"

	"github.com/getnabbed/agent/internal/engine/usenet"
	"github.com/getnabbed/agent/internal/netguard"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// KindUsenet is a usenet provider account, the web app's built-in kind.
const KindUsenet = "usenet"

const (
	maxSources        = 32
	maxSealedBytes    = 16 << 10
	maxDisplayNameLen = 128
	dialTimeout       = 60 * time.Second

	// allowHint tells the user how to allow a local host. It follows
	// ErrHostBlocked in messages the web app shows.
	allowHint = "to allow it, add the address to network.allowed_ranges in the agent's config (NABBED_ALLOWED_RANGES) and restart the agent"
)

var (
	// ErrKindUnsupported means this agent cannot use the source's kind.
	ErrKindUnsupported = errors.New("source kind not supported")
	// ErrUnsealable means the configuration cannot be opened with this
	// agent's key.
	ErrUnsealable = errors.New("source configuration cannot be unsealed")
	// ErrHostBlocked means the source points at a local network address.
	ErrHostBlocked = errors.New("source host is on a local network")
	// ErrInvalid means a field is missing or out of range.
	ErrInvalid = errors.New("invalid source")
	// ErrLimit means the agent already holds the maximum number of sources.
	ErrLimit = errors.New("too many sources")
)

// Engine receives the providers of enabled sources. *usenet.Engine
// implements it.
type Engine interface {
	SetProvider(id string, p usenet.Provider)
	RemoveProvider(id string)
	SetHealthHook(fn func(id string))
	Health(id string) (usenet.Health, bool)
	Check(ctx context.Context, id string) error
}

// Config configures a Manager.
type Config struct {
	Store *store.Store
	// Key opens sealed configurations.
	Key    *ecdh.PrivateKey
	Engine Engine
	Log    *slog.Logger
	// AllowRanges are local ranges a source's host may be in, from
	// network.allowed_ranges. Any other local address is refused.
	AllowRanges []netip.Prefix
}

// Manager stores sources and keeps the engine in step with them.
type Manager struct {
	store  *store.Store
	key    *ecdh.PrivateKey
	engine Engine
	log    *slog.Logger
	// guard decides which addresses sources may reach.
	guard netguard.Policy

	mu sync.Mutex
	// applied fingerprints what the engine holds per source, so a repeated
	// PutSource (as in rehydration) does not reset live connections.
	applied map[string][sha256.Size]byte
	// unusable holds stored sources that no longer open, reported in Hello
	// so the control plane can ask for them to be sealed again.
	unusable map[string]bool
	// Health reporting, see health.go.
	wake     chan struct{}
	changed  map[string]bool
	toCheck  map[string]bool
	reported map[string]usenet.Health
	recheck  recheck
}

// New returns a Manager. Call Load before applying commands, and Run to
// report source health.
func New(cfg Config) *Manager {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	m := &Manager{
		store: cfg.Store, key: cfg.Key, engine: cfg.Engine,
		guard:    netguard.Policy{Allow: cfg.AllowRanges},
		log:      cfg.Log.With("component", "source"),
		applied:  map[string][sha256.Size]byte{},
		unusable: map[string]bool{},
		wake:     make(chan struct{}, 1),
		changed:  map[string]bool{},
		toCheck:  map[string]bool{},
		reported: map[string]usenet.Health{},
		recheck:  recheck{base: recheckBase, max: recheckMax, tick: recheckTick, due: map[string]*retry{}},
	}

	if m.engine != nil {
		m.engine.SetHealthHook(m.onHealth)
	}

	return m
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Put validates, stores and (after commit) applies a source. Errors wrap
// one of the package's sentinel errors and never contain secrets.
func (m *Manager) Put(ctx context.Context, tx *store.Tx, s *pb.Source) error {
	p, err := m.open(s)
	if err != nil {
		return err
	}

	var others int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sources WHERE id != ?", s.GetId()).Scan(&others); err != nil {
		return fmt.Errorf("count sources: %w", err)
	}

	if others >= maxSources {
		return fmt.Errorf("%w: the agent holds at most %d", ErrLimit, maxSources)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO sources (id, kind, display_name, sealed_config, sealed_to_key, enabled, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			kind = excluded.kind, display_name = excluded.display_name,
			sealed_config = excluded.sealed_config, sealed_to_key = excluded.sealed_to_key,
			enabled = excluded.enabled, updated_at = excluded.updated_at`,
		s.GetId(), s.GetKind(), s.GetDisplayName(), s.GetSealedConfig(), s.GetSealedToKey(), s.GetEnabled(), time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("store source: %w", err)
	}

	fp := fingerprint(s)
	tx.AfterCommit(func() { m.apply(s.GetId(), s.GetDisplayName(), s.GetEnabled(), p, fp) })

	return nil
}

// Remove deletes a source. Removing an unknown source succeeds, so the
// control plane can replay removals to a fresh agent.
func (m *Manager) Remove(ctx context.Context, tx *store.Tx, id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%w: source id must be 1-64 letters, digits, - or _", ErrInvalid)
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM sources WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete source: %w", err)
	}

	tx.AfterCommit(func() {
		m.mu.Lock()
		defer m.mu.Unlock()

		delete(m.unusable, id)
		delete(m.reported, id)

		if _, ok := m.applied[id]; ok {
			m.engine.RemoveProvider(id)
			delete(m.applied, id)
			m.log.Info("usenet source removed", "source_id", id)
		}
	})

	return nil
}

// Load applies every stored source. A source that no longer opens, for
// example after the agent was paired again with a new key, is skipped and
// logged; the control plane must send it again.
func (m *Manager) Load(ctx context.Context) error {
	rows, err := m.store.DB().QueryContext(ctx,
		"SELECT id, kind, display_name, sealed_config, sealed_to_key, enabled FROM sources ORDER BY rowid")
	if err != nil {
		return fmt.Errorf("load sources: %w", err)
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		s := &pb.Source{}
		if err := rows.Scan(&s.Id, &s.Kind, &s.DisplayName, &s.SealedConfig, &s.SealedToKey, &s.Enabled); err != nil {
			return fmt.Errorf("load sources: %w", err)
		}

		p, err := m.open(s)
		if err != nil {
			m.mu.Lock()
			m.unusable[s.GetId()] = true
			m.mu.Unlock()

			m.log.Warn("skipping stored source", "source_id", s.GetId(), "err", err)
			continue
		}

		m.apply(s.GetId(), s.GetDisplayName(), s.GetEnabled(), p, fingerprint(s))
	}

	return rows.Err()
}

// Inventory lists the stored sources for Hello, in the order they were
// first stored. It reads the store, so it reflects committed commands
// only, and it never includes configuration contents.
func (m *Manager) Inventory(ctx context.Context) ([]*pb.LocalSourceSummary, error) {
	rows, err := m.store.DB().QueryContext(ctx, "SELECT id, kind, sealed_config, enabled FROM sources ORDER BY rowid")
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []*pb.LocalSourceSummary

	for rows.Next() {
		var (
			s      pb.LocalSourceSummary
			sealed []byte
		)

		if err := rows.Scan(&s.Id, &s.Kind, &sealed, &s.Enabled); err != nil {
			return nil, fmt.Errorf("list sources: %w", err)
		}

		sum := sha256.Sum256(sealed)
		s.SealedConfigSha256 = sum[:]
		out = append(out, &s)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}

	m.mu.Lock()
	for _, s := range out {
		s.Usable = !m.unusable[s.Id]
	}
	m.mu.Unlock()

	return out, nil
}

// open validates s and unseals its configuration.
func (m *Manager) open(s *pb.Source) (usenet.Provider, error) {
	switch {
	case !idPattern.MatchString(s.GetId()):
		return usenet.Provider{}, fmt.Errorf("%w: source id must be 1-64 letters, digits, - or _", ErrInvalid)
	case len(s.GetDisplayName()) > maxDisplayNameLen:
		return usenet.Provider{}, fmt.Errorf("%w: display_name is longer than %d bytes", ErrInvalid, maxDisplayNameLen)
	case s.GetKind() != KindUsenet:
		return usenet.Provider{}, fmt.Errorf("%w: %q (this agent supports %q)", ErrKindUnsupported, truncate(s.GetKind(), 32), KindUsenet)
	case len(s.GetSealedConfig()) == 0 || len(s.GetSealedConfig()) > maxSealedBytes:
		return usenet.Provider{}, fmt.Errorf("%w: sealed_config must be 1-%d bytes", ErrInvalid, maxSealedBytes)
	}

	plain, err := Open(m.key, s.GetSealedConfig(), s.GetSealedToKey())
	if err != nil {
		return usenet.Provider{}, err
	}

	defer clear(plain)

	p, err := parseUsenet(plain, m.guard)
	if err != nil {
		return usenet.Provider{}, err
	}

	// Checked again at every dial, after DNS: a public name can resolve to
	// a private address.
	p.Dial = m.guard.Dialer(&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second})

	return p, nil
}

func (m *Manager) apply(id, name string, enabled bool, p usenet.Provider, fp [sha256.Size]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.unusable, id)
	have, ok := m.applied[id]

	switch {
	case !enabled:
		if ok {
			m.engine.RemoveProvider(id)
			delete(m.applied, id)
			delete(m.reported, id)
			m.log.Info("usenet source disabled", "source_id", id, "name", name)
		}
	case ok && have == fp:
		// Unchanged: keep the live pool and its connections.
	default:
		m.engine.SetProvider(id, p)
		m.applied[id] = fp
		m.queueCheck(id)
		m.log.Info("usenet source configured", "source_id", id, "name", name,
			"host", p.Host, "connections", p.Connections, "backup", p.Backup)
	}
}

func fingerprint(s *pb.Source) [sha256.Size]byte {
	h := sha256.New()
	for _, b := range [][]byte{[]byte(s.GetKind()), s.GetSealedConfig()} {
		_, _ = fmt.Fprintf(h, "%d:", len(b))
		_, _ = h.Write(b)
	}

	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))

	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n]
}
