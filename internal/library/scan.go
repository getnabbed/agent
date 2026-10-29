package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// reportSize bounds the entries in one InventoryReport.
const reportSize = 500

// ErrUnknownRoot means no library root with that id is configured.
var ErrUnknownRoot = errors.New("unknown library root")

// errSuperseded stops a scan whose request was replaced or removed.
var errSuperseded = errors.New("scan superseded")

// scanRun is a scan in progress.
type scanRun struct {
	id     string
	cancel context.CancelFunc
	done   chan struct{}
}

// Rescan records a scan of a root, which starts once tx commits. The root
// must exist and still pass the local policy. A scan already running for
// the root is cancelled and replaced. scanID is reported in every
// InventoryReport of the scan.
func (m *Manager) Rescan(ctx context.Context, tx *store.Tx, rootID, scanID string) error {
	var path string

	err := tx.QueryRowContext(ctx, "SELECT path FROM library_roots WHERE id = ?", rootID).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w %q", ErrUnknownRoot, rootID)
	}

	if err != nil {
		return fmt.Errorf("read root: %w", err)
	}

	if _, err := m.policy.Check(path); err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO library_scans (root_id, scan_id, created_at) VALUES (?, ?, ?)
		ON CONFLICT (root_id) DO UPDATE SET scan_id = excluded.scan_id, created_at = excluded.created_at`,
		rootID, scanID, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("store scan: %w", err)
	}

	tx.AfterCommit(func() { m.startScan(rootID, scanID) })

	return nil
}

// RunScans runs requested scans until ctx ends, starting with those a
// restart interrupted. Scans requested before it is called wait for it.
func (m *Manager) RunScans(ctx context.Context) error {
	m.mu.Lock()
	m.base = ctx
	m.mu.Unlock()

	pending, err := m.pendingScans(ctx)
	if err != nil {
		// New requests still run; the interrupted ones wait for the next
		// start.
		m.log.Warn("resume library scans", "err", err)
	}

	for _, s := range pending {
		m.log.Info("resuming library scan", "root_id", s[0], "scan_id", s[1])
		m.startScan(s[0], s[1])
	}

	<-ctx.Done()
	m.wg.Wait()

	return err
}

// pendingScans lists requested scans as (root id, scan id), oldest first.
func (m *Manager) pendingScans(ctx context.Context) ([][2]string, error) {
	rows, err := m.store.DB().QueryContext(ctx, "SELECT root_id, scan_id FROM library_scans ORDER BY created_at")
	if err != nil {
		return nil, fmt.Errorf("list scans: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var pending [][2]string

	for rows.Next() {
		var s [2]string
		if err := rows.Scan(&s[0], &s[1]); err != nil {
			return nil, fmt.Errorf("list scans: %w", err)
		}

		pending = append(pending, s)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list scans: %w", err)
	}

	return pending, nil
}

func (m *Manager) startScan(rootID, scanID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Before RunScans, the request waits in the store.
	if m.base == nil || m.base.Err() != nil {
		return
	}

	prev := m.scans[rootID]
	if prev != nil && prev.id == scanID {
		return
	}

	if prev != nil {
		prev.cancel()
	}

	ctx, cancel := context.WithCancel(m.base)
	run := &scanRun{id: scanID, cancel: cancel, done: make(chan struct{})}
	m.scans[rootID] = run

	m.wg.Go(func() {
		defer close(run.done)
		defer cancel()

		// One scan of a root at a time, so reports of the old scan can't
		// interleave with the new one's.
		if prev != nil {
			<-prev.done
		}

		m.scan(ctx, rootID, scanID)

		m.mu.Lock()
		if m.scans[rootID] == run {
			delete(m.scans, rootID)
		}
		m.mu.Unlock()
	})
}

// stopScan cancels a running scan of a root.
func (m *Manager) stopScan(rootID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if run := m.scans[rootID]; run != nil {
		run.cancel()
	}
}

func (m *Manager) scan(ctx context.Context, rootID, scanID string) {
	log := m.log.With("root_id", rootID, "scan_id", scanID)
	start := time.Now()

	root, err := m.Root(ctx, rootID)
	if errors.Is(err, ErrUnknownRoot) {
		// The root was removed; its scan row went with it.
		return
	}

	if err != nil {
		log.Warn("scan library root", "err", err)

		return
	}

	rep := &reporter{m: m, rootID: rootID, scanID: scanID}

	files, incomplete, err := m.policy.walkVideos(ctx, root.GetPath(), m.otherRoots(ctx, rootID), func(rel string, fi fs.FileInfo) error {
		return rep.add(ctx, &pb.InventoryEntry{
			RootId: rootID, RelativePath: rel, Size: uint64(max(fi.Size(), 0)), MtimeMs: fi.ModTime().UnixMilli(),
		})
	})

	switch {
	case ctx.Err() != nil, errors.Is(err, errSuperseded):
		// Shut down, replaced or removed. An interrupted scan restarts on
		// the next run.
		return
	case err != nil:
		log.Warn("scan library root", "err", err)

		incomplete = true
	}

	if err := rep.finish(ctx, incomplete); err != nil {
		if !errors.Is(err, errSuperseded) && ctx.Err() == nil {
			log.Warn("report library scan", "err", err)
		}

		return
	}

	log.Info("scanned library root", "files", files, "incomplete", incomplete, "took", time.Since(start).Round(time.Millisecond))
}

// otherRoots returns the folders of the other roots, so a root holding
// another one, such as a TV root inside a movies root, doesn't report the
// other root's files.
func (m *Manager) otherRoots(ctx context.Context, rootID string) []os.FileInfo {
	rows, err := m.store.DB().QueryContext(ctx, "SELECT path FROM library_roots WHERE id != ?", rootID)
	if err != nil {
		return nil
	}

	defer func() { _ = rows.Close() }()

	var out []os.FileInfo

	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return out
		}

		if fi, err := os.Stat(path); err == nil && fi.IsDir() {
			out = append(out, fi)
		}
	}

	// A partial list only means a nested root may be reported twice.
	_ = rows.Err()

	return out
}

// reporter batches a scan's entries into InventoryReport events.
type reporter struct {
	m       *Manager
	rootID  string
	scanID  string
	entries []*pb.InventoryEntry
}

func (r *reporter) add(ctx context.Context, e *pb.InventoryEntry) error {
	r.entries = append(r.entries, e)
	if len(r.entries) < reportSize {
		return nil
	}

	return r.send(ctx, false, false)
}

func (r *reporter) finish(ctx context.Context, incomplete bool) error {
	return r.send(ctx, true, incomplete)
}

// send queues one report, if the scan is still the root's current one. The
// last report removes the scan in the same transaction.
func (r *reporter) send(ctx context.Context, last, incomplete bool) error {
	err := r.m.store.Update(ctx, func(tx *store.Tx) error {
		var current string

		err := tx.QueryRowContext(ctx, "SELECT scan_id FROM library_scans WHERE root_id = ?", r.rootID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && current != r.scanID) {
			return errSuperseded
		}

		if err != nil {
			return fmt.Errorf("read scan: %w", err)
		}

		rep := &pb.InventoryReport{Entries: r.entries, Last: last, RootId: r.rootID, ScanId: r.scanID, Incomplete: incomplete}
		if _, err := tx.Enqueue(ctx, &pb.Event{Kind: &pb.Event_InventoryReport{InventoryReport: rep}}); err != nil {
			return err
		}

		if last {
			if _, err := tx.ExecContext(ctx, "DELETE FROM library_scans WHERE root_id = ?", r.rootID); err != nil {
				return fmt.Errorf("finish scan: %w", err)
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	r.entries = nil

	return nil
}

// walkVideos calls fn for each video file under path, with its path
// relative to the root (slash-separated) and its file info. It skips hidden
// names, sample folders and the folders in skip. Symlinked files are
// followed only to targets inside the allowed roots; symlinked folders are
// not followed. incomplete reports folders that could not be read.
func (p *Policy) walkVideos(ctx context.Context, path string, skip []os.FileInfo, fn func(rel string, fi fs.FileInfo) error) (files int, incomplete bool, err error) {
	dir, err := p.Check(path)
	if err != nil {
		return 0, true, err
	}

	err = filepath.WalkDir(dir, func(full string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if err != nil {
			if full == dir {
				return err
			}

			// Unreadable folder: carry on, but the scan can't prove
			// anything missing from it is gone.
			incomplete = true

			return nil
		}

		if full == dir {
			return nil
		}

		name := d.Name()

		if d.IsDir() {
			if strings.HasPrefix(name, ".") || isSample(name) || sameAsAny(full, skip) {
				return filepath.SkipDir
			}

			return nil
		}

		if strings.HasPrefix(name, ".") || !videoExts[strings.ToLower(filepath.Ext(name))] {
			return nil
		}

		fi, ok := p.videoInfo(full, d)
		if !ok {
			return nil
		}

		rel, err := filepath.Rel(dir, full)
		if err != nil {
			return nil
		}

		files++

		return fn(filepath.ToSlash(rel), fi)
	})

	return files, incomplete, err
}

// videoInfo returns the info of a regular file, following a symlink only to
// a target inside the allowed roots.
func (p *Policy) videoInfo(full string, d fs.DirEntry) (fs.FileInfo, bool) {
	if d.Type()&fs.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(full)
		if err != nil || !p.inside(target) {
			return nil, false
		}

		fi, err := os.Stat(target)

		return fi, err == nil && fi.Mode().IsRegular()
	}

	if !d.Type().IsRegular() {
		return nil, false
	}

	// A file removed during the scan is simply not reported.
	fi, err := d.Info()

	return fi, err == nil
}

func sameAsAny(path string, dirs []os.FileInfo) bool {
	if len(dirs) == 0 {
		return false
	}

	fi, err := os.Stat(path)
	if err != nil {
		return false
	}

	for _, d := range dirs {
		if os.SameFile(fi, d) {
			return true
		}
	}

	return false
}
