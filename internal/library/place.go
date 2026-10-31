package library

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

const (
	// partialSuffix ends the name of a file being copied into a library. A
	// copy is written under a hidden, unique name (see partialName) and
	// renamed to its final name only once complete.
	partialSuffix = ".nabbed-partial"
	// partialRandBytes is the random part of a partial name, hex-encoded.
	partialRandBytes = 4
	// maxNameBytes is the longest file name most file systems accept.
	maxNameBytes = 255
	// maxPartialStem is how much of a file's name its partial copies keep:
	// what fits beside the two dots, the random part and partialSuffix.
	maxPartialStem = maxNameBytes - len("..") - 2*partialRandBytes - len(partialSuffix)
)

// ErrExists means the destination already holds a file and replacing it
// was not requested.
var ErrExists = errors.New("destination file already exists")

// Placement says how one file goes into a library root.
type Placement struct {
	// Src is the downloaded file, an absolute path in the work directory.
	Src string
	// Root is the library root's path as stored, re-checked here.
	Root string
	// Rel is the destination relative to Root, slash-separated, already
	// rendered and sanitised.
	Rel string
	// Mode is the import policy's mode.
	Mode pb.ImportMode
	// Torrent is set for torrent downloads. AUTO then copies across file
	// systems instead of moving, so the original keeps seeding.
	Torrent bool
	// Replace allows overwriting an existing file at Rel, for upgrades.
	Replace bool
}

// Place puts p.Src at p.Root/p.Rel and returns the method it used ("link",
// "rename" or "copy").
//
// The destination folder is created and opened through os.Root, so no link
// leads out of the root. On Unix the file is then written, linked or
// renamed through that open folder, so swapping a folder on the way for a
// link afterwards cannot redirect it; Windows has no such calls and uses the
// folder's path (see place_windows.go). Copies go to a new file created
// exclusively, so nothing planted under its name is followed. Without
// p.Replace the last step fails rather than overwrite a file that appeared
// meanwhile, and that failure is ErrExists.
//
// It is safe to call again after a crash: a finished placement is detected
// and kept.
func (pol *Policy) Place(ctx context.Context, p Placement) (string, error) {
	root, err := pol.Check(p.Root)
	if err != nil {
		return "", err
	}

	rel := filepath.FromSlash(p.Rel)
	if rel == "" || filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%w: destination %q is not inside the root", ErrTemplateUnsafe, p.Rel)
	}

	src, err := os.Stat(p.Src)
	if err != nil {
		return "", fmt.Errorf("read download: %w", err)
	}

	d, err := openFolder(root, filepath.Dir(rel), pol.DirMode)
	if err != nil {
		return "", err
	}

	defer d.close()

	name := filepath.Base(rel)

	fi, err := d.root.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// The name is free.
	case err != nil:
		return "", fmt.Errorf("check %s: %w", p.Rel, err)
	case os.SameFile(fi, src):
		// An earlier run linked it. The link is the download's inode, so
		// set its mode through the download's own path.
		_ = os.Chmod(p.Src, pol.FileMode)

		return "link", nil
	case !fi.Mode().IsRegular():
		return "", fmt.Errorf("%w: %s is not a regular file", ErrExists, p.Rel)
	case !p.Replace:
		return "", fmt.Errorf("%w: %s", ErrExists, p.Rel)
	}

	mode := p.Mode
	if mode == pb.ImportMode_IMPORT_MODE_AUTO || mode == pb.ImportMode_IMPORT_MODE_UNSPECIFIED {
		mode = autoMode(p.Torrent, sameDevice(p.Src, d))
	}

	switch mode {
	case pb.ImportMode_IMPORT_MODE_HARDLINK:
		err := d.link(p.Src, name, p.Replace)
		if err == nil {
			// The link shares the download's inode; give it library
			// permissions so a media server running as another user can read it.
			_ = os.Chmod(p.Src, pol.FileMode)
			d.sync()

			return "link", nil
		}

		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("%w: %s", ErrExists, p.Rel)
		}

		// Different file systems, or links not supported: copy instead and
		// keep the download.
		return "copy", pol.copyInto(ctx, d, name, p)
	case pb.ImportMode_IMPORT_MODE_MOVE:
		// The mode moves with the file. A copy sets its own.
		_ = os.Chmod(p.Src, pol.FileMode)

		err := d.move(p.Src, name, p.Replace)
		switch {
		case err == nil:
			d.sync()

			return "rename", nil
		case errors.Is(err, fs.ErrExist):
			return "", fmt.Errorf("%w: %s", ErrExists, p.Rel)
		case !isCrossDevice(err):
			return "", fmt.Errorf("move into library: %w", err)
		}

		if err := pol.copyInto(ctx, d, name, p); err != nil {
			return "", err
		}

		// The copy is complete and synced; only now drop the download.
		if err := os.Remove(p.Src); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "copy", fmt.Errorf("remove download after copying: %w", err)
		}

		return "copy", nil
	case pb.ImportMode_IMPORT_MODE_COPY:
		return "copy", pol.copyInto(ctx, d, name, p)
	}

	return "", fmt.Errorf("%w: unknown import mode %v", ErrInvalid, p.Mode)
}

// folder is a destination folder, held open. Names are resolved inside it,
// and os.Root follows no link out of it.
type folder struct {
	root *os.Root
	// dir is the folder itself, opened on first use by platforms that make
	// system calls relative to it.
	dir *os.File
}

// openFolder creates rel under root and opens it.
func openFolder(root, rel string, perm os.FileMode) (*folder, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open root: %w", err)
	}

	defer func() { _ = r.Close() }()

	if err := r.MkdirAll(rel, perm); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.ToSlash(rel), err)
	}

	sub, err := r.OpenRoot(rel)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.ToSlash(rel), err)
	}

	return &folder{root: sub}, nil
}

func (d *folder) close() {
	if d.dir != nil {
		_ = d.dir.Close()
	}

	_ = d.root.Close()
}

// link hard-links src, a path outside the library, as name. Without
// replace it never replaces name; with it, the old file stays until the
// link is in place.
func (d *folder) link(src, name string, replace bool) error {
	if !replace {
		return d.linkFrom(src, name)
	}

	tmp := partialName(name)
	if err := d.linkFrom(src, tmp); err != nil {
		return fmt.Errorf("link to a temporary name: %w", err)
	}

	if err := d.root.Rename(tmp, name); err != nil {
		_ = d.root.Remove(tmp)
		return err
	}

	return nil
}

// move renames src, a path outside the library, to name. Without replace
// it never replaces name. Across file systems it fails with an error
// isCrossDevice recognises.
func (d *folder) move(src, name string, replace bool) error {
	if replace {
		return d.moveFrom(src, name)
	}

	return d.moveFromNoReplace(src, name)
}

// commit renames the finished partial file tmp to name. Without replace it
// never replaces name.
func (d *folder) commit(tmp, name string, replace bool) error {
	if replace {
		return d.root.Rename(tmp, name)
	}

	return d.renameNoReplace(tmp, name)
}

// copyInto copies p.Src to name in d through a partial file in the same
// folder: created exclusively, written, given its permissions and synced,
// then renamed to name.
func (pol *Policy) copyInto(ctx context.Context, d *folder, name string, p Placement) error {
	in, err := os.Open(p.Src)
	if err != nil {
		return fmt.Errorf("read download: %w", err)
	}

	defer func() { _ = in.Close() }()

	d.removePartials(name)

	tmp := partialName(name)

	out, err := d.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, pol.FileMode)
	if err != nil {
		return fmt.Errorf("create in library: %w", err)
	}

	_, err = io.Copy(out, &ctxReader{ctx: ctx, r: in})
	if err == nil {
		// Through the open file, not a path; the umask may have narrowed it.
		err = out.Chmod(pol.FileMode)
	}

	if err == nil {
		err = out.Sync()
	}

	if cerr := out.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		_ = d.root.Remove(tmp)

		if ctx.Err() != nil {
			return ctx.Err()
		}

		return fmt.Errorf("copy into library: %w", err)
	}

	if err := d.commit(tmp, name, p.Replace); err != nil {
		_ = d.root.Remove(tmp)

		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrExists, p.Rel)
		}

		return fmt.Errorf("copy into library: %w", err)
	}

	d.sync()

	return nil
}

// partialPrefix starts the names of name's partial copies. They are hidden,
// so library scans and media servers skip them, and fit in maxNameBytes
// with the random part and partialSuffix.
func partialPrefix(name string) string {
	stem := name
	for len(stem) > maxPartialStem {
		_, n := utf8.DecodeLastRuneInString(stem)
		stem = stem[:len(stem)-n]
	}

	return "." + stem + "."
}

// partialName returns a new, unpredictable name for a partial copy of name.
func partialName(name string) string {
	var b [partialRandBytes]byte
	_, _ = rand.Read(b[:])

	return partialPrefix(name) + hex.EncodeToString(b[:]) + partialSuffix
}

// isPartial reports whether entry is a partial copy of name.
func isPartial(entry, name string) bool {
	prefix := partialPrefix(name)

	return len(entry) == len(prefix)+2*partialRandBytes+len(partialSuffix) &&
		strings.HasPrefix(entry, prefix) && strings.HasSuffix(entry, partialSuffix)
}

// removePartials deletes partial copies of name that an earlier run left,
// such as after a crash partway through a copy. A copy of the same name
// that is running now loses its file, fails at the rename and is retried.
func (d *folder) removePartials(name string) {
	entries, err := fs.ReadDir(d.root.FS(), ".")
	if err != nil {
		return
	}

	for _, e := range entries {
		if isPartial(e.Name(), name) {
			_ = d.root.Remove(e.Name())
		}
	}
}

// ctxReader stops a long copy when the import is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}

	return c.r.Read(p)
}

// autoMode is IMPORT_MODE_AUTO as the web app describes it: torrents are
// hard-linked when possible and copied otherwise, so they keep seeding;
// usenet downloads are moved.
func autoMode(torrent, sameFS bool) pb.ImportMode {
	switch {
	case !torrent:
		return pb.ImportMode_IMPORT_MODE_MOVE
	case sameFS:
		return pb.ImportMode_IMPORT_MODE_HARDLINK
	default:
		return pb.ImportMode_IMPORT_MODE_COPY
	}
}
