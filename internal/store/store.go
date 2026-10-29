// Package store is the agent's durable local state: a SQLite database in
// the data directory, using the pure-Go modernc.org/sqlite driver so the
// binary stays cgo-free.
//
// Two invariants from design 0001 §7 live here:
//
//   - A command's state change and its recorded result commit in one
//     transaction (ApplyCommand). A redelivered command replays the stored
//     result.
//   - A state change and the event describing it commit in one transaction
//     (Update + Tx.Enqueue). Events leave through the outbox and are deleted
//     only when the cloud acknowledges them.
//
// Secrets never go in this database; see credstore and design 0003 §9.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// FileName is the database file inside the data directory.
const FileName = "state.db"

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store is an open state database.
type Store struct {
	db   *sql.DB
	path string
	log  *slog.Logger

	// events is signalled after a commit that enqueued outbox events.
	events chan struct{}
}

// Open opens (creating if needed) the state database in dir and binds it to
// agentID. A database that belongs to a different agent, left behind by
// `agent pair --force`, is moved aside and a new one is created, because
// its commands and events refer to an agent the cloud no longer knows.
func Open(ctx context.Context, dir, agentID string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}

	if agentID == "" {
		return nil, errors.New("store: agent id is required")
	}

	path := filepath.Join(dir, FileName)

	s, err := open(ctx, path, log)
	if err != nil {
		return nil, err
	}

	owner, err := s.meta(ctx, "agent_id")
	if err != nil {
		_ = s.Close()
		return nil, err
	}

	switch owner {
	case agentID:
		return s, nil
	case "":
		if err := s.setMeta(ctx, "agent_id", agentID); err != nil {
			_ = s.Close()
			return nil, err
		}

		return s, nil
	}

	// Belongs to another identity: set it aside and start over.
	_ = s.Close()

	// The owner id came from the cloud; hash it rather than trust it in a
	// file name.
	sum := sha256.Sum256([]byte(owner))
	aside := fmt.Sprintf("%s.%s.%d.bak", path, hex.EncodeToString(sum[:8]), time.Now().Unix())
	if err := os.Rename(path, aside); err != nil {
		return nil, fmt.Errorf("store: move aside state of previous agent %s: %w", owner, err)
	}

	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}

	log.Warn("state database belonged to a previous agent identity; starting fresh",
		"previous_agent_id", owner, "moved_to", aside)

	return Open(ctx, dir, agentID, log)
}

func open(ctx context.Context, path string, log *slog.Logger) (*Store, error) {
	// WAL lets reads proceed during writes. synchronous=FULL makes every
	// commit durable before CommandResult or EventAck handling moves on: a
	// power cut must not roll back a command whose result the cloud has
	// already acknowledged. _txlock=immediate takes the write lock at BEGIN,
	// avoiding SQLITE_BUSY on lock upgrade.
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	// Keep temporary tables and sort spills in memory: the container root is
	// read-only and /tmp may not exist.
	q.Add("_pragma", "temp_store(MEMORY)")
	q.Set("_txlock", "immediate")

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	// One connection serialises all access. The agent's write rate is low,
	// and this rules out SQLITE_BUSY between our own goroutines.
	db.SetMaxOpenConns(1)

	s := &Store{db: db, path: path, log: log, events: make(chan struct{}, 1)}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	return s, nil
}

// Close closes the database. Closing the last connection checkpoints the
// WAL into the main file.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// DB exposes the database for reads by domain packages. Writes must go
// through Update or ApplyCommand so that events and command results stay
// atomic with the state they describe.
func (s *Store) DB() *sql.DB { return s.db }

// migrate applies embedded migrations newer than PRAGMA user_version, each
// in its own transaction together with the version bump.
func (s *Store) migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return err
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	sort.Strings(names)

	var current int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}

	if current > len(names) {
		return fmt.Errorf("store: database schema version %d is newer than this agent supports (%d); upgrade the agent", current, len(names))
	}

	for i := current; i < len(names); i++ {
		version := i + 1

		prefix, _, _ := strings.Cut(names[i], "_")
		if n, err := strconv.Atoi(prefix); err != nil || n != version {
			return fmt.Errorf("store: migration %s out of sequence, want %04d_*.sql", names[i], version)
		}

		body, err := migrationFS.ReadFile("migrations/" + names[i])
		if err != nil {
			return err
		}

		err = s.inTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return err
			}

			_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version))

			return err
		})
		if err != nil {
			return fmt.Errorf("store: apply migration %s: %w", names[i], err)
		}

		s.log.Debug("applied state migration", "version", version, "file", names[i])
	}

	return nil
}

// Tx is a write transaction handed to Update and ApplyCommand callbacks.
// Domain packages run their own statements on it with the embedded *sql.Tx.
type Tx struct {
	*sql.Tx

	enqueued bool
	after    []func()
}

// AfterCommit runs fn once the transaction has committed, and never if it
// rolls back. Use it for in-memory changes that must match stored state.
func (t *Tx) AfterCommit(fn func()) { t.after = append(t.after, fn) }

func runAfter(fns []func()) {
	for _, fn := range fns {
		fn()
	}
}

// Update runs fn in a write transaction. If fn returns an error the
// transaction rolls back and no enqueued events become visible.
func (s *Store) Update(ctx context.Context, fn func(tx *Tx) error) error {
	var (
		enqueued bool
		after    []func()
	)

	err := s.inTx(ctx, func(sqlTx *sql.Tx) error {
		tx := &Tx{Tx: sqlTx}
		if err := fn(tx); err != nil {
			return err
		}

		enqueued, after = tx.enqueued, tx.after

		return nil
	})
	if err != nil {
		return err
	}

	runAfter(after)

	if enqueued {
		s.signalEvents()
	}

	return nil
}

func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}

	return tx.Commit()
}

func (s *Store) meta(ctx context.Context, key string) (string, error) {
	var v string

	err := s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}

	return v, err
}

func (s *Store) setMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)

	return err
}

// HelloState reports what the agent already holds, for Hello. fresh is true
// until the first command has been applied to this database, so a cloud
// rehydrate interrupted before any command landed is simply repeated.
func (s *Store) HelloState(ctx context.Context) (fresh bool, lastCommandID string, pendingEvents uint32, err error) {
	err = s.db.QueryRowContext(ctx,
		"SELECT id FROM applied_commands ORDER BY seq DESC LIMIT 1").Scan(&lastCommandID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, "", 0, err
	}

	var n int64
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM outbox").Scan(&n); err != nil {
		return false, "", 0, err
	}

	return lastCommandID == "", lastCommandID, uint32(min(n, int64(^uint32(0)))), nil
}

// Summary is a read-only view of the database for `agent status`.
type Summary struct {
	AgentID       string
	SchemaVersion int
	LastCommandID string
	AppliedCount  int64
	PendingEvents int64
}

// Inspect reads a Summary without migrating, binding or otherwise writing,
// so it is safe while another process owns the database. It returns
// fs.ErrNotExist if there is no database yet.
func Inspect(ctx context.Context, dir string) (*Summary, error) {
	path := filepath.Join(dir, FileName)
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("mode", "ro")
	// With no WAL present the daemon is not running: open immutable so SQLite
	// creates no -wal or -shm files, which, made by another user such as root,
	// would lock the daemon out of its own database.
	if _, err := os.Stat(path + "-wal"); errors.Is(err, fs.ErrNotExist) {
		q.Set("immutable", "1")
	}

	q.Add("_pragma", "busy_timeout(5000)")

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?"+q.Encode())
	if err != nil {
		return nil, err
	}

	defer func() { _ = db.Close() }()

	var sum Summary
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&sum.SchemaVersion); err != nil {
		return nil, err
	}

	if sum.SchemaVersion == 0 {
		return &sum, nil
	}

	err = db.QueryRowContext(ctx, `
		SELECT
			coalesce((SELECT value FROM meta WHERE key = 'agent_id'), ''),
			coalesce((SELECT id FROM applied_commands ORDER BY seq DESC LIMIT 1), ''),
			(SELECT count(*) FROM applied_commands),
			(SELECT count(*) FROM outbox)`).
		Scan(&sum.AgentID, &sum.LastCommandID, &sum.AppliedCount, &sum.PendingEvents)
	if err != nil {
		return nil, err
	}

	return &sum, nil
}

// Meta returns the value of a meta key read in tx, or "" if it is unset.
func (t *Tx) Meta(ctx context.Context, key string) (string, error) {
	var v string

	err := t.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}

	return v, err
}

// SetMeta sets a meta key as part of tx.
func (t *Tx) SetMeta(ctx context.Context, key, value string) error {
	_, err := t.ExecContext(ctx,
		"INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)

	return err
}

// Meta returns the value of a meta key, or "" if it is unset.
func (s *Store) Meta(ctx context.Context, key string) (string, error) { return s.meta(ctx, key) }
