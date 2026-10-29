// Package notice reports conditions of the agent itself to the control
// plane as AgentNotice events (design 0008 §2).
//
// A condition is named by a code and a subject. Its state lives in the
// notices table, and an event goes through the outbox, in the same
// transaction, only when that state changes: once when the condition
// starts and once, with resolved set, when it clears. A restart therefore
// never resends an open notice. At most Limit notices leave per Window;
// changes held back by the limit stay in the table and go out from Run
// when the budget allows, folded with any later change.
package notice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// Codes, documented in docs/design/0008.
const (
	CodeAgentUpgraded           = "agent.upgraded"
	CodeLibraryNoneAllowed      = "library.none_allowed"
	CodeLibraryAllowedMissing   = "library.allowed_missing"
	CodeLibraryRootUnusable     = "library.root_unusable"
	CodeLibraryScanIncomplete   = "library.scan_incomplete"
	CodeDiskLow                 = "disk.low"
	CodeUsenetConnectionsCapped = "usenet.connections_capped"
)

const (
	// MaxMessageBytes bounds a notice's message.
	MaxMessageBytes = 256
	// MaxAttrs bounds a notice's attrs.
	MaxAttrs = 8
	// DefaultLimit is how many notices may leave per DefaultWindow.
	DefaultLimit = 30
	// DefaultWindow is the rate limit's sliding window.
	DefaultWindow = time.Hour
	// flushInterval is how often Run sends changes the limit held back.
	flushInterval = time.Minute
	// versionKey is the meta key holding the version that last ran.
	versionKey = "agent_version"
)

// Config configures a Reporter.
type Config struct {
	Store *store.Store
	Log   *slog.Logger
	// Now is the clock for the rate limit; nil uses time.Now.
	Now func() time.Time
	// Limit and Window bound the notices sent; zero uses DefaultLimit
	// per DefaultWindow.
	Limit  int
	Window time.Duration
}

// Reporter raises and resolves notices. It is safe for concurrent use.
type Reporter struct {
	store  *store.Store
	log    *slog.Logger
	now    func() time.Time
	limit  int
	window time.Duration

	mu   sync.Mutex
	sent []time.Time // send times inside the window, oldest first
}

// New returns a Reporter. The rate limit starts from the notices the
// table records as sent within the window, so restarts can't reset it.
func New(ctx context.Context, cfg Config) (*Reporter, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	if cfg.Limit <= 0 {
		cfg.Limit = DefaultLimit
	}

	if cfg.Window <= 0 {
		cfg.Window = DefaultWindow
	}

	r := &Reporter{store: cfg.Store, log: cfg.Log.With("component", "notice"), now: cfg.Now, limit: cfg.Limit, window: cfg.Window}

	rows, err := cfg.Store.DB().QueryContext(ctx,
		"SELECT sent_at FROM notices WHERE sent_at >= ? ORDER BY sent_at", r.now().Add(-r.window).UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("read notices: %w", err)
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var ms int64
		if err := rows.Scan(&ms); err != nil {
			return nil, fmt.Errorf("read notices: %w", err)
		}

		r.sent = append(r.sent, time.UnixMilli(ms))
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read notices: %w", err)
	}

	return r, nil
}

// Raise records that the condition code+subject holds, and queues a notice
// if it did not already. A condition already open is left as it is, even
// if message or attrs differ.
func (r *Reporter) Raise(ctx context.Context, code, subject string, sev pb.Severity, message string, attrs map[string]string) error {
	return r.store.Update(ctx, func(tx *store.Tx) error {
		return r.RaiseTx(ctx, tx, code, subject, sev, message, attrs)
	})
}

// Resolve records that the condition code+subject cleared, and queues a
// resolved notice if it was open.
func (r *Reporter) Resolve(ctx context.Context, code, subject string) error {
	return r.store.Update(ctx, func(tx *store.Tx) error {
		return r.ResolveTx(ctx, tx, code, subject)
	})
}

// RaiseTx is Raise as part of tx, for a condition found while changing
// other state.
func (r *Reporter) RaiseTx(ctx context.Context, tx *store.Tx, code, subject string, sev pb.Severity, message string, attrs map[string]string) error {
	open, ok, err := r.state(ctx, tx, code, subject)
	if err != nil {
		return err
	}

	if ok && open {
		return nil
	}

	raw, err := json.Marshal(boundAttrs(attrs))
	if err != nil {
		return fmt.Errorf("encode notice attrs: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO notices (code, subject_id, open, severity, message, attrs, updated_at) VALUES (?, ?, 1, ?, ?, ?, ?)
		ON CONFLICT (code, subject_id) DO UPDATE SET open = 1, severity = excluded.severity,
			message = excluded.message, attrs = excluded.attrs, updated_at = excluded.updated_at`,
		code, subject, int32(sev), Truncate(message, MaxMessageBytes), string(raw), r.now().UnixMilli())
	if err != nil {
		return fmt.Errorf("store notice: %w", err)
	}

	r.log.Debug("notice raised", "code", code, "subject_id", subject)

	return r.send(ctx, tx, code, subject)
}

// ResolveTx is Resolve as part of tx.
func (r *Reporter) ResolveTx(ctx context.Context, tx *store.Tx, code, subject string) error {
	open, ok, err := r.state(ctx, tx, code, subject)
	if err != nil {
		return err
	}

	if !ok || !open {
		return nil
	}

	_, err = tx.ExecContext(ctx, "UPDATE notices SET open = 0, updated_at = ? WHERE code = ? AND subject_id = ?",
		r.now().UnixMilli(), code, subject)
	if err != nil {
		return fmt.Errorf("store notice: %w", err)
	}

	r.log.Debug("notice resolved", "code", code, "subject_id", subject)

	return r.send(ctx, tx, code, subject)
}

// Reconcile resolves every open condition of code whose subject is not in
// keep, for subjects that went away, such as a removed root.
func (r *Reporter) Reconcile(ctx context.Context, code string, keep []string) error {
	return r.store.Update(ctx, func(tx *store.Tx) error {
		subjects, err := r.subjects(ctx, tx, "SELECT subject_id FROM notices WHERE code = ? AND open = 1", code)
		if err != nil {
			return err
		}

		for _, s := range subjects {
			if slices.Contains(keep, s) {
				continue
			}

			if err := r.ResolveTx(ctx, tx, code, s); err != nil {
				return err
			}
		}

		return nil
	})
}

// Open reports whether the condition code+subject is open.
func (r *Reporter) Open(ctx context.Context, code, subject string) (bool, error) {
	var open bool

	err := r.store.DB().QueryRowContext(ctx, "SELECT open FROM notices WHERE code = ? AND subject_id = ?", code, subject).Scan(&open)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}

	return open, err
}

// Upgraded stores version as the one running, and queues an
// agent.upgraded notice when it differs from the version stored by the
// last run. The upgrade is an event, not a condition, so it is sent once,
// already resolved, and keeps no state in the notices table. The first
// run with this table records the version without a notice.
func (r *Reporter) Upgraded(ctx context.Context, version string) error {
	return r.store.Update(ctx, func(tx *store.Tx) error {
		prev, err := tx.Meta(ctx, versionKey)
		if err != nil {
			return fmt.Errorf("read agent version: %w", err)
		}

		if prev == version {
			return nil
		}

		if err := tx.SetMeta(ctx, versionKey, version); err != nil {
			return fmt.Errorf("store agent version: %w", err)
		}

		if prev == "" {
			return nil
		}

		if !r.take() {
			r.log.Warn("notice dropped by the rate limit", "code", CodeAgentUpgraded)
			return nil
		}

		n := &pb.AgentNotice{
			Code:     CodeAgentUpgraded,
			Message:  Truncate(fmt.Sprintf("agent upgraded from %s to %s", prev, version), MaxMessageBytes),
			Severity: pb.Severity_SEVERITY_INFO,
			Attrs:    boundAttrs(map[string]string{"from": prev, "to": version}),
			Resolved: true,
		}

		_, err = tx.Enqueue(ctx, &pb.Event{Kind: &pb.Event_AgentNotice{AgentNotice: n}})

		return err
	})
}

// Run sends changes the rate limit held back, every minute until ctx ends.
func (r *Reporter) Run(ctx context.Context) error {
	t := time.NewTicker(flushInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := r.Flush(ctx); err != nil && ctx.Err() == nil {
				r.log.Warn("send held-back notices", "err", err)
			}
		}
	}
}

// Flush sends held-back changes, oldest first, while the budget allows.
func (r *Reporter) Flush(ctx context.Context) error {
	type key struct{ code, subject string }

	rows, err := r.store.DB().QueryContext(ctx,
		"SELECT code, subject_id FROM notices WHERE open != sent_open ORDER BY updated_at")
	if err != nil {
		return fmt.Errorf("list held-back notices: %w", err)
	}

	var pending []key

	for rows.Next() {
		var k key
		if err := rows.Scan(&k.code, &k.subject); err != nil {
			_ = rows.Close()
			return fmt.Errorf("list held-back notices: %w", err)
		}

		pending = append(pending, k)
	}

	_ = rows.Close()

	if err := rows.Err(); err != nil {
		return fmt.Errorf("list held-back notices: %w", err)
	}

	for _, k := range pending {
		if !r.available() {
			return nil
		}

		err := r.store.Update(ctx, func(tx *store.Tx) error { return r.send(ctx, tx, k.code, k.subject) })
		if err != nil {
			return err
		}
	}

	return nil
}

// state reads a condition: whether it is open, and whether it is known.
func (r *Reporter) state(ctx context.Context, tx *store.Tx, code, subject string) (open, ok bool, err error) {
	err = tx.QueryRowContext(ctx, "SELECT open FROM notices WHERE code = ? AND subject_id = ?", code, subject).Scan(&open)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}

	if err != nil {
		return false, false, fmt.Errorf("read notice: %w", err)
	}

	return open, true, nil
}

// send queues the condition's current state if it differs from the state
// last sent and the rate limit allows. A cleared condition whose clearing
// went out, or never needed to, is deleted.
func (r *Reporter) send(ctx context.Context, tx *store.Tx, code, subject string) error {
	var (
		open, sentOpen bool
		sev            int32
		message, raw   string
	)

	err := tx.QueryRowContext(ctx,
		"SELECT open, sent_open, severity, message, attrs FROM notices WHERE code = ? AND subject_id = ?", code, subject).
		Scan(&open, &sentOpen, &sev, &message, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("read notice: %w", err)
	}

	if open == sentOpen {
		return r.forget(ctx, tx, open, code, subject)
	}

	if !r.take() {
		r.log.Info("notice held back by the rate limit", "code", code, "subject_id", subject, "resolved", !open)
		return nil
	}

	var attrs map[string]string
	if err := json.Unmarshal([]byte(raw), &attrs); err != nil {
		attrs = nil
	}

	n := &pb.AgentNotice{
		Code: code, Message: message, Severity: pb.Severity(sev), Attrs: attrs,
		SubjectId: subject, Resolved: !open,
	}

	if _, err := tx.Enqueue(ctx, &pb.Event{Kind: &pb.Event_AgentNotice{AgentNotice: n}}); err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, "UPDATE notices SET sent_open = open, sent_at = ? WHERE code = ? AND subject_id = ?",
		r.now().UnixMilli(), code, subject)
	if err != nil {
		return fmt.Errorf("store notice: %w", err)
	}

	return r.forget(ctx, tx, open, code, subject)
}

// forget deletes a cleared condition the cloud already knows is clear.
func (r *Reporter) forget(ctx context.Context, tx *store.Tx, open bool, code, subject string) error {
	if open {
		return nil
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM notices WHERE code = ? AND subject_id = ?", code, subject); err != nil {
		return fmt.Errorf("delete notice: %w", err)
	}

	return nil
}

func (r *Reporter) subjects(ctx context.Context, tx *store.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list notices: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []string

	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("list notices: %w", err)
		}

		out = append(out, s)
	}

	return out, rows.Err()
}

// take spends one notice from the budget, if any is left. A notice whose
// transaction rolls back still counts, which errs on the side of fewer.
func (r *Reporter) take() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.availableLocked() {
		return false
	}

	r.sent = append(r.sent, r.now())

	return true
}

func (r *Reporter) available() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.availableLocked()
}

func (r *Reporter) availableLocked() bool {
	cutoff := r.now().Add(-r.window)

	i := 0
	for i < len(r.sent) && !r.sent[i].After(cutoff) {
		i++
	}

	r.sent = r.sent[i:]

	return len(r.sent) < r.limit
}

// boundAttrs keeps at most MaxAttrs entries, by key order, with values cut
// to MaxMessageBytes.
func boundAttrs(attrs map[string]string) map[string]string {
	if len(attrs) == 0 {
		return nil
	}

	out := make(map[string]string, min(len(attrs), MaxAttrs))

	for _, k := range slices.Sorted(maps.Keys(attrs)) {
		if len(out) == MaxAttrs {
			break
		}

		out[k] = Truncate(attrs[k], MaxMessageBytes)
	}

	return out
}

// Truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}
