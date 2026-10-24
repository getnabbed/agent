package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

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

		if _, err := os.Stat(dst + partialSuffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%v: got a partial file left behind", tc.mode)
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
	p, media, src := placeSetup(t)

	dst := filepath.Join(media, "ep.mkv")
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Place(context.Background(), Placement{Src: src, Root: media, Rel: "ep.mkv", Mode: pb.ImportMode_IMPORT_MODE_COPY}); !errors.Is(err, ErrExists) {
		t.Fatalf("got %v, want ErrExists", err)
	}

	if _, err := p.Place(context.Background(), Placement{Src: src, Root: media, Rel: "ep.mkv", Mode: pb.ImportMode_IMPORT_MODE_COPY, Replace: true}); err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(dst); string(got) != "video" {
		t.Fatalf("got %q after replacing", got)
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
