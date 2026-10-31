package library

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

func placeSetup(t *testing.T) (*Policy, string, string) {
	t.Helper()

	base := t.TempDir()
	media, work := filepath.Join(base, "media"), filepath.Join(base, "work")

	for _, d := range []string{media, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	src := filepath.Join(work, "video.mkv")
	if err := os.WriteFile(src, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}

	p, _, _ := NewPolicy([]string{media}, 0o644, 0o755)

	return p, media, src
}

func TestPlaceModes(t *testing.T) {
	for _, tc := range []struct {
		mode    pb.ImportMode
		torrent bool
		method  string
		keepSrc bool
	}{
		{pb.ImportMode_IMPORT_MODE_MOVE, false, "rename", false},
		{pb.ImportMode_IMPORT_MODE_COPY, false, "copy", true},
		{pb.ImportMode_IMPORT_MODE_HARDLINK, false, "link", true},
		{pb.ImportMode_IMPORT_MODE_AUTO, false, "rename", false}, // usenet moves
		{pb.ImportMode_IMPORT_MODE_AUTO, true, "link", true},     // torrents keep seeding
	} {
		p, media, src := placeSetup(t)

		method, err := p.Place(context.Background(), Placement{Src: src, Root: media, Rel: "Show/Season 01/ep.mkv", Mode: tc.mode, Torrent: tc.torrent})
		if err != nil || method != tc.method {
			t.Fatalf("%v torrent=%v: got %q, %v; want %q", tc.mode, tc.torrent, method, err, tc.method)
		}

		dst := filepath.Join(media, "Show", "Season 01", "ep.mkv")
		if got, err := os.ReadFile(dst); err != nil || string(got) != "video" {
			t.Fatalf("%v: got %q, %v at the destination", tc.mode, got, err)
		}

		if _, err := os.Stat(src); (err == nil) != tc.keepSrc {
			t.Fatalf("%v: source kept = %v, want %v", tc.mode, err == nil, tc.keepSrc)
		}

		if runtime.GOOS != "windows" {
			fi, _ := os.Stat(filepath.Join(media, "Show"))
			if uint32(fi.Mode().Perm()) != 0o755&^umask() {
				t.Fatalf("%v: got folder mode %v", tc.mode, fi.Mode().Perm())
			}

			if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o644 {
				t.Fatalf("%v: got file mode %v, want 0644 so a media server can read it", tc.mode, fi.Mode().Perm())
			}
		}

		if left := partials(t, filepath.Dir(dst)); len(left) != 0 {
			t.Fatalf("%v: got partial files left behind: %v", tc.mode, left)
		}
	}
}

func TestAutoModeAcrossFileSystems(t *testing.T) {
	if got := autoMode(true, false); got != pb.ImportMode_IMPORT_MODE_COPY {
		t.Fatalf("got %v for a torrent across file systems, want COPY to keep seeding", got)
	}

	if got := autoMode(false, false); got != pb.ImportMode_IMPORT_MODE_MOVE {
		t.Fatalf("got %v for usenet across file systems, want MOVE", got)
	}
}

func TestPlaceNeverOverwritesUnlessReplacing(t *testing.T) {
	for _, mode := range []pb.ImportMode{pb.ImportMode_IMPORT_MODE_COPY, pb.ImportMode_IMPORT_MODE_MOVE, pb.ImportMode_IMPORT_MODE_HARDLINK} {
		p, media, src := placeSetup(t)

		dst := filepath.Join(media, "ep.mkv")
		if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := p.Place(context.Background(), Placement{Src: src, Root: media, Rel: "ep.mkv", Mode: mode}); !errors.Is(err, ErrExists) {
			t.Fatalf("%v: got %v, want ErrExists", mode, err)
		}

		if got, _ := os.ReadFile(dst); string(got) != "old" {
			t.Fatalf("%v: got %q, want the existing file kept", mode, got)
		}

		if _, err := p.Place(context.Background(), Placement{Src: src, Root: media, Rel: "ep.mkv", Mode: mode, Replace: true}); err != nil {
			t.Fatalf("%v: %v", mode, err)
		}

		if got, _ := os.ReadFile(dst); string(got) != "video" {
			t.Fatalf("%v: got %q after replacing", mode, got)
		}
	}
}

func TestPlaceIsSafeToRepeat(t *testing.T) {
	p, media, src := placeSetup(t)
	pl := Placement{Src: src, Root: media, Rel: "ep.mkv", Mode: pb.ImportMode_IMPORT_MODE_HARDLINK}

	for range 2 {
		if _, err := p.Place(context.Background(), pl); err != nil {
			t.Fatalf("got %v placing the same link twice", err)
		}
	}
}

func TestPlaceStaysInsideTheRoot(t *testing.T) {
	p, media, src := placeSetup(t)

	for _, rel := range []string{"../escape.mkv", "/abs.mkv", "a/../../escape.mkv", ""} {
		if _, err := p.Place(context.Background(), Placement{Src: src, Root: media, Rel: rel, Mode: pb.ImportMode_IMPORT_MODE_COPY}); err == nil {
			t.Errorf("got no error placing at %q", rel)
		}
	}

	if runtime.GOOS == "windows" {
		return
	}

	// A folder inside the library that links outside is refused.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(media, "link")); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Place(context.Background(), Placement{Src: src, Root: media, Rel: "link/ep.mkv", Mode: pb.ImportMode_IMPORT_MODE_COPY}); err == nil {
		t.Fatal("got a file placed through a symlink leading outside")
	}

	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("got %d files written outside the root", len(ents))
	}
}

func TestCancelledCopyLeavesNothing(t *testing.T) {
	p, media, src := placeSetup(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := p.Place(ctx, Placement{Src: src, Root: media, Rel: "ep.mkv", Mode: pb.ImportMode_IMPORT_MODE_COPY}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}

	if ents, _ := os.ReadDir(media); len(ents) != 0 {
		t.Fatalf("got %d files after a cancelled copy", len(ents))
	}
}

// partials lists the partial copies in dir.
func partials(t *testing.T, dir string) []string {
	t.Helper()

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	var out []string

	for _, e := range ents {
		if strings.HasSuffix(e.Name(), partialSuffix) {
			out = append(out, e.Name())
		}
	}

	return out
}

func TestPlaceIgnoresAPlantedPartialFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}

	p, media, src := placeSetup(t)

	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Links at the name copies used to write to, and at a name shaped like
	// a leftover partial copy, both pointing outside the library.
	for _, name := range []string{"ep.mkv" + partialSuffix, partialPrefix("ep.mkv") + "00000000" + partialSuffix} {
		if err := os.Symlink(victim, filepath.Join(media, name)); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := p.Place(context.Background(), Placement{Src: src, Root: media, Rel: "ep.mkv", Mode: pb.ImportMode_IMPORT_MODE_COPY}); err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Fatalf("got %q in a file outside the library, want it untouched", got)
	}

	fi, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}

	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("got mode %v for a file outside the library, want 0600", fi.Mode().Perm())
	}

	if got, _ := os.ReadFile(filepath.Join(media, "ep.mkv")); string(got) != "video" {
		t.Fatalf("got %q at the destination", got)
	}
}

func TestPlaceRemovesItsOwnLeftoverPartials(t *testing.T) {
	p, media, src := placeSetup(t)

	stale := filepath.Join(media, partialPrefix("ep.mkv")+"0123abcd"+partialSuffix)
	other := filepath.Join(media, partialPrefix("other.mkv")+"0123abcd"+partialSuffix)

	for _, f := range []string{stale, other} {
		if err := os.WriteFile(f, []byte("half"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := p.Place(context.Background(), Placement{Src: src, Root: media, Rel: "ep.mkv", Mode: pb.ImportMode_IMPORT_MODE_COPY}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v for a partial copy of the same file left by a crash, want it removed", err)
	}

	if _, err := os.Stat(other); err != nil {
		t.Fatalf("got %v for another file's partial copy, want it kept", err)
	}
}

func TestPartialNamesFit(t *testing.T) {
	long := strings.Repeat("é", 120) + ".mkv" // 244 bytes, as long as naming allows

	for _, name := range []string{"ep.mkv", long} {
		a, b := partialName(name), partialName(name)
		if a == b {
			t.Fatalf("got the same partial name twice: %q", a)
		}

		if len(a) > maxNameBytes || !utf8.ValidString(a) || !strings.HasPrefix(a, ".") {
			t.Fatalf("got %q (%d bytes), want a hidden valid name of at most %d bytes", a, len(a), maxNameBytes)
		}

		if !isPartial(a, name) || isPartial(a, "x"+name) {
			t.Fatalf("isPartial(%q) is wrong", a)
		}
	}
}

// openTestFolder opens dir as a destination folder.
func openTestFolder(t *testing.T, dir string) *folder {
	t.Helper()

	d, err := openFolder(dir, ".", 0o755)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(d.close)

	return d
}

// The last step of every placement refuses a name taken since Place
// checked it, as by a concurrent import, instead of replacing the file.
func TestFolderNeverReplacesWithoutAsking(t *testing.T) {
	_, media, src := placeSetup(t)
	d := openTestFolder(t, media)

	taken := filepath.Join(media, "ep.mkv")
	if err := os.WriteFile(taken, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	tmp := partialName("ep.mkv")
	if err := os.WriteFile(filepath.Join(media, tmp), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"link", func() error { return d.link(src, "ep.mkv", false) }},
		{"move", func() error { return d.move(src, "ep.mkv", false) }},
		{"commit", func() error { return d.commit(tmp, "ep.mkv", false) }},
	} {
		if err := step.run(); !errors.Is(err, fs.ErrExist) {
			t.Errorf("%s: got %v, want fs.ErrExist", step.name, err)
		}
	}

	if got, _ := os.ReadFile(taken); string(got) != "old" {
		t.Fatalf("got %q, want the existing file kept", got)
	}

	if _, err := os.Stat(src); err != nil {
		t.Fatalf("got %v, want the download kept after a refused move", err)
	}

	// Asked to replace, the commit does.
	if err := d.commit(tmp, "ep.mkv", true); err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(taken); string(got) != "new" {
		t.Fatalf("got %q after replacing", got)
	}
}
