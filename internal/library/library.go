package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

const (
	maxRoots       = 64
	maxTemplateLen = 512
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Manager stores library roots and the import policy. It holds no state
// of its own: everything is read from the store when it is used.
type Manager struct {
	store  *store.Store
	policy *Policy
	log    *slog.Logger
}

// New returns a Manager.
func New(st *store.Store, policy *Policy, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}

	return &Manager{store: st, policy: policy, log: log.With("component", "library")}
}

// Policy returns the local policy.
func (m *Manager) Policy() *Policy { return m.policy }

// Put validates and stores a root. The folder must exist, be inside an
// allowed root and be writable. Errors wrap one of the package's sentinel
// errors.
func (m *Manager) Put(ctx context.Context, tx *store.Tx, r *pb.LibraryRoot) error {
	switch {
	case !idPattern.MatchString(r.GetId()):
		return fmt.Errorf("%w: root id must be 1-64 letters, digits, - or _", ErrInvalid)
	case r.GetKind() != pb.LibraryKind_LIBRARY_KIND_MOVIES && r.GetKind() != pb.LibraryKind_LIBRARY_KIND_TV:
		return fmt.Errorf("%w: kind must be movies or tv", ErrInvalid)
	}

	for _, t := range []struct{ name, v string }{
		{"folder_template", r.GetFolderTemplate()},
		{"file_template", r.GetFileTemplate()},
		{"specials_folder", r.GetSpecialsFolder()},
	} {
		if err := checkTemplate(t.name, t.v); err != nil {
			return err
		}
	}

	resolved, err := m.policy.Check(r.GetPath())
	if err != nil {
		return err
	}

	if err := Writable(resolved); err != nil {
		return err
	}

	var others int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM library_roots WHERE id != ?", r.GetId()).Scan(&others); err != nil {
		return fmt.Errorf("count roots: %w", err)
	}

	if others >= maxRoots {
		return fmt.Errorf("%w: the agent holds at most %d", ErrLimit, maxRoots)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO library_roots (id, kind, path, folder_template, file_template, specials_folder, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			kind = excluded.kind, path = excluded.path, folder_template = excluded.folder_template,
			file_template = excluded.file_template, specials_folder = excluded.specials_folder,
			updated_at = excluded.updated_at`,
		r.GetId(), int32(r.GetKind()), r.GetPath(), r.GetFolderTemplate(), r.GetFileTemplate(), r.GetSpecialsFolder(), time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("store root: %w", err)
	}

	m.log.Info("library root configured", "root_id", r.GetId(), "kind", r.GetKind(), "path", r.GetPath(), "resolved", resolved)

	return nil
}

// checkTemplate refuses templates whose literal text could leave the root.
// Placeholders are filled with sanitised values when rendered, so only the
// fixed parts matter here.
func checkTemplate(name, t string) error {
	switch {
	case len(t) > maxTemplateLen:
		return fmt.Errorf("%w: %s is longer than %d bytes", ErrTemplateUnsafe, name, maxTemplateLen)
	case strings.ContainsRune(t, 0):
		return fmt.Errorf("%w: %s contains a NUL byte", ErrTemplateUnsafe, name)
	case strings.HasPrefix(t, "/"), strings.HasPrefix(t, `\`), len(t) > 1 && t[1] == ':':
		return fmt.Errorf("%w: %s is an absolute path", ErrTemplateUnsafe, name)
	}

	if slices.Contains(strings.FieldsFunc(t, func(r rune) bool { return r == '/' || r == '\\' }), "..") {
		return fmt.Errorf("%w: %s climbs out of the root (a .. part)", ErrTemplateUnsafe, name)
	}

	return nil
}

// Remove deletes a root. It refuses while an unfinished download targets
// it; removing an unknown root succeeds, so removals can be replayed.
func (m *Manager) Remove(ctx context.Context, tx *store.Tx, id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("%w: root id must be 1-64 letters, digits, - or _", ErrInvalid)
	}

	n, err := downloadsTargeting(ctx, tx, id)
	if err != nil {
		return err
	}

	if n > 0 {
		return fmt.Errorf("%w: %d unfinished downloads import into it; cancel them or let them finish first", ErrInUse, n)
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM library_roots WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete root: %w", err)
	}

	m.log.Info("library root removed", "root_id", id)

	return nil
}

// downloadsTargeting counts downloads not yet done or cancelled whose spec
// names root id.
func downloadsTargeting(ctx context.Context, tx *store.Tx, id string) (int, error) {
	rows, err := tx.QueryContext(ctx, "SELECT spec FROM downloads WHERE state NOT IN (?, ?)",
		int32(pb.DownloadState_DOWNLOAD_STATE_DONE), int32(pb.DownloadState_DOWNLOAD_STATE_CANCELLED))
	if err != nil {
		return 0, fmt.Errorf("list downloads: %w", err)
	}

	defer func() { _ = rows.Close() }()

	n := 0

	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return 0, fmt.Errorf("list downloads: %w", err)
		}

		var d pb.Download
		if err := proto.Unmarshal(raw, &d); err == nil && d.GetRootId() == id {
			n++
		}
	}

	return n, rows.Err()
}

// Root returns a stored root, or ErrInvalid wrapped when there is none.
func (m *Manager) Root(ctx context.Context, id string) (*pb.LibraryRoot, error) {
	var (
		r    pb.LibraryRoot
		kind int32
	)

	err := m.store.DB().QueryRowContext(ctx,
		"SELECT id, kind, path, folder_template, file_template, specials_folder FROM library_roots WHERE id = ?", id).
		Scan(&r.Id, &kind, &r.Path, &r.FolderTemplate, &r.FileTemplate, &r.SpecialsFolder)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: unknown library root %q", ErrInvalid, id)
	}

	if err != nil {
		return nil, fmt.Errorf("read root: %w", err)
	}

	r.Kind = pb.LibraryKind(kind)

	return &r, nil
}

// SetImportPolicy stores the import policy.
func (m *Manager) SetImportPolicy(ctx context.Context, tx *store.Tx, p *pb.SetImportPolicy) error {
	if p.GetMode() == pb.ImportMode_IMPORT_MODE_UNSPECIFIED {
		return fmt.Errorf("%w: import mode is required", ErrInvalid)
	}

	if r := p.GetSanitizer().GetReplacement(); strings.ContainsAny(r, `/\:*?"<>|`) || strings.ContainsRune(r, 0) || len(r) > 8 {
		return fmt.Errorf("%w: sanitizer replacement %q is not safe in a file name", ErrInvalid, r)
	}

	raw, err := proto.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode policy: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO import_policy (id, spec, updated_at) VALUES (1, ?, ?)
		ON CONFLICT (id) DO UPDATE SET spec = excluded.spec, updated_at = excluded.updated_at`,
		raw, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("store policy: %w", err)
	}

	m.log.Info("import policy set", "mode", p.GetMode(), "extras", p.GetExtras())

	return nil
}

// ImportPolicy returns the stored policy, or the default (AUTO, no extras)
// when the cloud has not sent one.
func (m *Manager) ImportPolicy(ctx context.Context) (*pb.SetImportPolicy, error) {
	var raw []byte

	err := m.store.DB().QueryRowContext(ctx, "SELECT spec FROM import_policy WHERE id = 1").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return &pb.SetImportPolicy{Mode: pb.ImportMode_IMPORT_MODE_AUTO}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read policy: %w", err)
	}

	var p pb.SetImportPolicy
	if err := proto.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode policy: %w", err)
	}

	return &p, nil
}
