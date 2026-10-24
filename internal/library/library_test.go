package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/getnabbed/agent/internal/store"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

var quiet = slog.New(slog.DiscardHandler)

// layout makes allowed/{movies,tv} and an outside/ folder next to it.
func layout(t *testing.T) (allowed, outside string) {
	t.Helper()

	base := t.TempDir()
	allowed, outside = filepath.Join(base, "media"), filepath.Join(base, "outside")

	for _, d := range []string{filepath.Join(allowed, "movies"), filepath.Join(allowed, "tv"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	return allowed, outside
}

func newManager(t *testing.T, allowed ...string) *Manager {
	t.Helper()

	st, err := store.Open(context.Background(), t.TempDir(), "agent-1", quiet)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	p, _, err := NewPolicy(allowed, 0o644, 0o755)
	if err != nil {
		t.Fatal(err)
	}

	return New(st, p, quiet)
}

func (m *Manager) put(r *pb.LibraryRoot) error {
	return m.store.Update(context.Background(), func(tx *store.Tx) error { return m.Put(context.Background(), tx, r) })
}

func root(id, path string) *pb.LibraryRoot {
	return &pb.LibraryRoot{
		Id: id, Kind: pb.LibraryKind_LIBRARY_KIND_MOVIES, Path: path,
		FolderTemplate: "{title} ({year})", FileTemplate: "{title} ({year})",
	}
}

func TestNothingIsAllowedByDefault(t *testing.T) {
	allowed, _ := layout(t)
	m := newManager(t)

	err := m.put(root("r1", filepath.Join(allowed, "movies")))
	if !errors.Is(err, ErrNotAllowed) || !strings.Contains(err.Error(), "NABBED_LIBRARY_ROOTS") {
		t.Fatalf("got %v, want ErrNotAllowed explaining how to allow folders", err)
	}
}

func TestRootsMustBeInsideAnAllowedFolder(t *testing.T) {
	allowed, outside := layout(t)
	m := newManager(t, allowed)

	if err := m.put(root("r1", filepath.Join(allowed, "movies"))); err != nil {
		t.Fatalf("got %v for a folder inside the allowed root", err)
	}

	if err := m.put(root("r2", allowed)); err != nil {
		t.Fatalf("got %v for the allowed root itself", err)
	}

	cases := map[string]error{
		outside: ErrNotAllowed,
		filepath.Join(allowed, "movies", "..", "..", "outside"): ErrNotAllowed,
		filepath.Join(allowed, "missing"):                       ErrInvalid,
		"relative/path":                                         ErrInvalid,
	}

	for path, want := range cases {
		if err := m.put(root("rx", path)); !errors.Is(err, want) {
			t.Errorf("got %v for %s, want %v", err, path, want)
		}
	}
}

func TestSymlinkCannotLeadOutside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}

	allowed, outside := layout(t)
	m := newManager(t, allowed)

	link := filepath.Join(allowed, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	if err := m.put(root("r1", link)); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("got %v, want a symlink to outside refused", err)
	}

	// The allowed root may itself be a symlink to the real mount.
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(allowed, alias); err != nil {
		t.Fatal(err)
	}

	m2 := newManager(t, alias)
	if err := m2.put(root("r1", filepath.Join(allowed, "tv"))); err != nil {
		t.Fatalf("got %v for a real path under a symlinked allowed root", err)
	}
}

func TestReadOnlyRootIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permissions do not bind root or Windows")
	}

	allowed, _ := layout(t)
	ro := filepath.Join(allowed, "movies")

	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })

	if err := newManager(t, allowed).put(root("r1", ro)); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "cannot write") {
		t.Fatalf("got %v, want a refusal naming the missing write access", err)
	}
}

func TestUnsafeTemplatesAreRefused(t *testing.T) {
	allowed, _ := layout(t)
	m := newManager(t, allowed)

	for _, tpl := range []string{"../{title}", "{title}/../../etc", "/etc/{title}", `C:\{title}`, `\\server\share`, "a\x00b", strings.Repeat("x", 600)} {
		r := root("r1", filepath.Join(allowed, "movies"))
		r.FolderTemplate = tpl

		if err := m.put(r); !errors.Is(err, ErrTemplateUnsafe) {
			t.Errorf("got %v for template %q, want ErrTemplateUnsafe", err, tpl)
		}
	}

	r := root("r1", filepath.Join(allowed, "tv"))
	r.Kind, r.FolderTemplate, r.SpecialsFolder = pb.LibraryKind_LIBRARY_KIND_TV, "{title}/Season {season:00}", "Specials"

	if err := m.put(r); err != nil {
		t.Fatalf("got %v for an ordinary TV template", err)
	}
}

func TestPutStoresAndUpdatesARoot(t *testing.T) {
	allowed, _ := layout(t)
	m := newManager(t, allowed)

	r := root("r1", filepath.Join(allowed, "movies"))
	if err := m.put(r); err != nil {
		t.Fatal(err)
	}

	r.FileTemplate = "{title}"
	if err := m.put(r); err != nil {
		t.Fatal(err)
	}

	got, err := m.Root(context.Background(), "r1")
	if err != nil || !proto.Equal(got, r) {
		t.Fatalf("got %v, %v; want the updated root", got, err)
	}

	if _, err := m.Root(context.Background(), "nope"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v for an unknown root", err)
	}
}

func TestRootInUseCannotBeRemoved(t *testing.T) {
	allowed, _ := layout(t)
	m := newManager(t, allowed)

	if err := m.put(root("r1", filepath.Join(allowed, "movies"))); err != nil {
		t.Fatal(err)
	}

	spec, _ := proto.Marshal(&pb.Download{Id: "d1", RootId: "r1"})
	insert := func(id string, state pb.DownloadState) {
		t.Helper()

		_, err := m.store.DB().Exec(`INSERT INTO downloads (id, acquisition_id, state, protocol, spec, created_at, updated_at)
			VALUES (?, 'a', ?, 1, ?, ?, ?)`, id, int32(state), spec, time.Now().UnixMilli(), time.Now().UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
	}

	insert("d1", pb.DownloadState_DOWNLOAD_STATE_FAILED) // may still be retried
	insert("d2", pb.DownloadState_DOWNLOAD_STATE_DONE)

	remove := func(id string) error {
		return m.store.Update(context.Background(), func(tx *store.Tx) error { return m.Remove(context.Background(), tx, id) })
	}

	if err := remove("r1"); !errors.Is(err, ErrInUse) || !strings.Contains(err.Error(), "1 unfinished") {
		t.Fatalf("got %v, want ErrInUse counting the failed download", err)
	}

	if _, err := m.store.DB().Exec("UPDATE downloads SET state = ? WHERE id = 'd1'", int32(pb.DownloadState_DOWNLOAD_STATE_CANCELLED)); err != nil {
		t.Fatal(err)
	}

	if err := remove("r1"); err != nil {
		t.Fatalf("got %v after the download was cancelled", err)
	}

	if err := remove("r1"); err != nil {
		t.Fatalf("got %v removing it again, want replays to succeed", err)
	}
}

func TestRootLimit(t *testing.T) {
	allowed, _ := layout(t)
	m := newManager(t, allowed)

	for i := range maxRoots {
		if err := m.put(root(fmt.Sprintf("r%d", i), allowed)); err != nil {
			t.Fatal(err)
		}
	}

	if err := m.put(root("one-more", allowed)); !errors.Is(err, ErrLimit) {
		t.Fatalf("got %v, want ErrLimit", err)
	}
}

func TestImportPolicy(t *testing.T) {
	m := newManager(t)

	p, err := m.ImportPolicy(context.Background())
	if err != nil || p.GetMode() != pb.ImportMode_IMPORT_MODE_AUTO {
		t.Fatalf("got %v, %v; want AUTO before any policy arrives", p, err)
	}

	set := func(p *pb.SetImportPolicy) error {
		return m.store.Update(context.Background(), func(tx *store.Tx) error { return m.SetImportPolicy(context.Background(), tx, p) })
	}

	want := &pb.SetImportPolicy{Mode: pb.ImportMode_IMPORT_MODE_HARDLINK, Sanitizer: &pb.PathSanitizer{Replacement: "_"}, Extras: []string{"subtitles"}}
	if err := set(want); err != nil {
		t.Fatal(err)
	}

	if got, _ := m.ImportPolicy(context.Background()); !proto.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	for _, bad := range []*pb.SetImportPolicy{
		{},
		{Mode: pb.ImportMode_IMPORT_MODE_MOVE, Sanitizer: &pb.PathSanitizer{Replacement: "/"}},
	} {
		if err := set(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("got %v for %v, want ErrInvalid", err, bad)
		}
	}
}

func browse(t *testing.T, p *Policy, path, token string) BrowseResult {
	t.Helper()

	raw, err := p.Browse(path, token)
	if err != nil {
		t.Fatal(err)
	}

	var res BrowseResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}

	return res
}

func TestBrowseStaysInsideAllowedRoots(t *testing.T) {
	allowed, outside := layout(t)
	p, _, _ := NewPolicy([]string{allowed, filepath.Join(t.TempDir(), "not-mounted-yet")}, 0o644, 0o755)

	if res := browse(t, p, "", ""); len(res.Entries) != 1 || res.Entries[0].Path != allowed {
		t.Fatalf("got %+v, want the existing allowed root as the top level", res)
	}

	if err := os.WriteFile(filepath.Join(allowed, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(filepath.Join(allowed, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}

	if runtime.GOOS != "windows" {
		if err := os.Symlink(outside, filepath.Join(allowed, "escape")); err != nil {
			t.Fatal(err)
		}

		if err := os.Symlink(filepath.Join(allowed, "tv"), filepath.Join(allowed, "shows")); err != nil {
			t.Fatal(err)
		}
	}

	res := browse(t, p, allowed, "")

	var names []string
	for _, e := range res.Entries {
		names = append(names, e.Name)
	}

	want := "movies,shows,tv"
	if runtime.GOOS == "windows" {
		want = "movies,tv"
	}

	if got := strings.Join(names, ","); got != want {
		t.Fatalf("got %s, want folders only, no hidden ones, no symlink leading outside", got)
	}

	if _, err := p.Browse(outside, ""); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("got %v browsing outside", err)
	}
}

func TestBrowsePages(t *testing.T) {
	allowed, _ := layout(t)

	for i := range browsePage + 5 {
		if err := os.Mkdir(filepath.Join(allowed, "movies", fmt.Sprintf("m%03d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	p, _, _ := NewPolicy([]string{allowed}, 0o644, 0o755)
	dir := filepath.Join(allowed, "movies")

	first := browse(t, p, dir, "")
	if len(first.Entries) != browsePage || first.NextPageToken == "" {
		t.Fatalf("got %d entries, token %q", len(first.Entries), first.NextPageToken)
	}

	second := browse(t, p, dir, first.NextPageToken)
	if len(second.Entries) != 5 || second.NextPageToken != "" || second.Entries[0].Name != fmt.Sprintf("m%03d", browsePage) {
		t.Fatalf("got second page %+v", second)
	}
}

func TestNewPolicyRefusesRelativeRoots(t *testing.T) {
	if _, _, err := NewPolicy([]string{"media"}, 0o644, 0o755); err == nil {
		t.Fatal("got a policy with a relative allowed root")
	}
}
