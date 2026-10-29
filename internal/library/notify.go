package library

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"time"

	"github.com/getnabbed/agent/internal/notice"
	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// Notifier reports agent notices (design 0008 §2). *notice.Reporter
// implements it.
type Notifier interface {
	Raise(ctx context.Context, code, subject string, sev pb.Severity, message string, attrs map[string]string) error
	Resolve(ctx context.Context, code, subject string) error
	RaiseTx(ctx context.Context, tx *store.Tx, code, subject string, sev pb.Severity, message string, attrs map[string]string) error
	ResolveTx(ctx context.Context, tx *store.Tx, code, subject string) error
	Reconcile(ctx context.Context, code string, keep []string) error
}

// SetNotifier makes the manager report library notices to n. Call it
// before RunScans, WatchFolders or the first import; nil, the default,
// reports none.
func (m *Manager) SetNotifier(n Notifier) { m.notifier = n }

// Roots lists the stored roots.
func (m *Manager) Roots(ctx context.Context) ([]*pb.LibraryRoot, error) {
	rows, err := m.store.DB().QueryContext(ctx, "SELECT id, kind, path FROM library_roots ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("list roots: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []*pb.LibraryRoot

	for rows.Next() {
		var (
			r    pb.LibraryRoot
			kind int32
		)

		if err := rows.Scan(&r.Id, &kind, &r.Path); err != nil {
			return nil, fmt.Errorf("list roots: %w", err)
		}

		r.Kind = pb.LibraryKind(kind)
		out = append(out, &r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list roots: %w", err)
	}

	return out, nil
}

// WatchFolders checks the allowed folders now and every interval until ctx
// ends, raising library.none_allowed and library.allowed_missing.
func (m *Manager) WatchFolders(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		m.CheckFolders(ctx)

		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// CheckFolders raises library.none_allowed when no folder is allowed while
// the store holds roots, and library.allowed_missing for each allowed
// folder that isn't there. Both resolve once fixed.
func (m *Manager) CheckFolders(ctx context.Context) {
	if m.notifier == nil {
		return
	}

	allowed := m.policy.Allowed()

	var roots int
	if err := m.store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM library_roots").Scan(&roots); err != nil {
		m.log.Warn("check library folders", "err", err)
		return
	}

	if len(allowed) == 0 && roots > 0 {
		m.raise(ctx, notice.CodeLibraryNoneAllowed, "", pb.Severity_SEVERITY_WARNING,
			fmt.Sprintf("no library folders are allowed, so %d library roots can't be used; %s", roots, howTo),
			map[string]string{"roots": strconv.Itoa(roots)})
	} else {
		m.resolve(ctx, notice.CodeLibraryNoneAllowed, "")
	}

	for _, a := range allowed {
		if fi, err := os.Stat(a); err == nil && fi.IsDir() {
			m.resolve(ctx, notice.CodeLibraryAllowedMissing, a)
			continue
		}

		m.raise(ctx, notice.CodeLibraryAllowedMissing, a, pb.Severity_SEVERITY_WARNING,
			fmt.Sprintf("allowed library folder %s does not exist or is not a folder", a),
			map[string]string{"path": a})
	}

	// Folders taken out of the config are no longer missing.
	if err := m.notifier.Reconcile(ctx, notice.CodeLibraryAllowedMissing, allowed); err != nil {
		m.log.Warn("report notice", "code", notice.CodeLibraryAllowedMissing, "err", err)
	}
}

// RootFailed raises library.root_unusable when err says the root fails the
// local policy or can't be written. Other errors are ignored.
func (m *Manager) RootFailed(ctx context.Context, rootID string, err error) {
	if m.notifier == nil || !rootUnusable(err) {
		return
	}

	m.raise(ctx, notice.CodeLibraryRootUnusable, rootID, pb.Severity_SEVERITY_ERROR,
		fmt.Sprintf("library root %s can't be used: %v", rootID, err), map[string]string{"root_id": rootID})
}

// RootUsed resolves library.root_unusable after a successful use of the
// root.
func (m *Manager) RootUsed(ctx context.Context, rootID string) {
	if m.notifier != nil {
		m.resolve(ctx, notice.CodeLibraryRootUnusable, rootID)
	}
}

func rootUnusable(err error) bool {
	return errors.Is(err, ErrNotAllowed) || errors.Is(err, ErrInvalid) || errors.Is(err, fs.ErrPermission)
}

// scanFinishedTx raises or resolves library.scan_incomplete in the
// transaction that queues a scan's last report.
func (m *Manager) scanFinishedTx(ctx context.Context, tx *store.Tx, rootID string, incomplete bool) {
	if m.notifier == nil {
		return
	}

	var err error
	if incomplete {
		err = m.notifier.RaiseTx(ctx, tx, notice.CodeLibraryScanIncomplete, rootID, pb.Severity_SEVERITY_WARNING,
			fmt.Sprintf("the last scan of library root %s could not read every folder", rootID), map[string]string{"root_id": rootID})
	} else {
		err = m.notifier.ResolveTx(ctx, tx, notice.CodeLibraryScanIncomplete, rootID)
	}

	if err != nil {
		m.log.Warn("report notice", "code", notice.CodeLibraryScanIncomplete, "root_id", rootID, "err", err)
	}
}

// rootRemovedTx resolves a removed root's conditions.
func (m *Manager) rootRemovedTx(ctx context.Context, tx *store.Tx, rootID string) error {
	if m.notifier == nil {
		return nil
	}

	for _, code := range []string{notice.CodeLibraryRootUnusable, notice.CodeLibraryScanIncomplete} {
		if err := m.notifier.ResolveTx(ctx, tx, code, rootID); err != nil {
			return fmt.Errorf("resolve notice: %w", err)
		}
	}

	return nil
}

func (m *Manager) raise(ctx context.Context, code, subject string, sev pb.Severity, message string, attrs map[string]string) {
	if err := m.notifier.Raise(ctx, code, subject, sev, message, attrs); err != nil && ctx.Err() == nil {
		m.log.Warn("report notice", "code", code, "subject_id", subject, "err", err)
	}
}

func (m *Manager) resolve(ctx context.Context, code, subject string) {
	if err := m.notifier.Resolve(ctx, code, subject); err != nil && ctx.Err() == nil {
		m.log.Warn("report notice", "code", code, "subject_id", subject, "err", err)
	}
}
