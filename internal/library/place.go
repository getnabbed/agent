package library

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// partialSuffix marks a file being copied into a library. It is never left
// under the final name half-written, and a rerun after a crash replaces it.
const partialSuffix = ".nabbed-partial"

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
// "rename" or "copy"). Destination folders are created through os.Root, so
// they cannot leave the root, and the final folder is checked against the
// allowed roots again just before the file lands. It is safe to call again
// after a crash: a finished placement is detected and kept.
func (pol *Policy) Place(ctx context.Context, p Placement) (string, error) {
	root, err := pol.Check(p.Root)
	if err != nil {
		return "", err
	}

	rel := filepath.FromSlash(p.Rel)
	if rel == "" || filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%w: destination %q is not inside the root", ErrTemplateUnsafe, p.Rel)
	}

	r, err := os.OpenRoot(root)
	if err != nil {
		return "", fmt.Errorf("open root: %w", err)
	}

	defer func() { _ = r.Close() }()

	if err := r.MkdirAll(filepath.Dir(rel), pol.DirMode); err != nil {
		return "", fmt.Errorf("create %s: %w", filepath.Dir(p.Rel), err)
	}

	// MkdirAll followed no link out of the root, but a component created
	// earlier could be a symlink to elsewhere inside it; resolve the final
	// folder once more against the policy.
	dir, err := pol.Check(filepath.Join(root, filepath.Dir(rel)))
	if err != nil {
		return "", err
	}

	dst := filepath.Join(dir, filepath.Base(rel))

	src, err := os.Stat(p.Src)
	if err != nil {
		return "", fmt.Errorf("read download: %w", err)
	}

	if fi, err := os.Lstat(dst); err == nil {
		switch {
		case os.SameFile(fi, src):
			_ = os.Chmod(dst, pol.FileMode)

			return "link", nil // an earlier run linked it
		case !fi.Mode().IsRegular():
			return "", fmt.Errorf("%w: %s is not a regular file", ErrExists, p.Rel)
		case !p.Replace:
			return "", fmt.Errorf("%w: %s", ErrExists, p.Rel)
		}
	}

	mode := p.Mode
	if mode == pb.ImportMode_IMPORT_MODE_AUTO || mode == pb.ImportMode_IMPORT_MODE_UNSPECIFIED {
		mode = autoMode(p.Torrent, sameDevice(p.Src, dir))
	}

	switch mode {
	case pb.ImportMode_IMPORT_MODE_HARDLINK:
		if err := link(p.Src, dst, p.Replace); err == nil {
			// The link shares the download's inode; give it library
			// permissions so a media server running as another user can read it.
			_ = os.Chmod(dst, pol.FileMode)

			return "link", nil
		}

		// Different file systems, or links not supported: copy instead and
		// keep the download.
		return "copy", pol.copyInto(ctx, p.Src, dst)
	case pb.ImportMode_IMPORT_MODE_MOVE:
		if err := os.Rename(p.Src, dst); err == nil {
			_ = os.Chmod(dst, pol.FileMode)
			return "rename", nil
		} else if !isCrossDevice(err) {
			return "", fmt.Errorf("move into library: %w", err)
		}

		if err := pol.copyInto(ctx, p.Src, dst); err != nil {
			return "", err
		}

		// The copy is complete and synced; only now drop the download.
		if err := os.Remove(p.Src); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "copy", fmt.Errorf("remove download after copying: %w", err)
		}

		return "copy", nil
	case pb.ImportMode_IMPORT_MODE_COPY:
		return "copy", pol.copyInto(ctx, p.Src, dst)
	}

	return "", fmt.Errorf("%w: unknown import mode %v", ErrInvalid, p.Mode)
}

// link hard-links src to dst, replacing dst through a temporary name when
// asked so the old file stays until the new one is in place.
func link(src, dst string, replace bool) error {
	if !replace {
		return os.Link(src, dst)
	}

	tmp := dst + partialSuffix
	_ = os.Remove(tmp)

	if err := os.Link(src, tmp); err != nil {
		return err
	}

	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	return nil
}

// copyInto copies src to dst through a partial file in the same folder:
// written, synced, given its permissions, then renamed over dst.
func (pol *Policy) copyInto(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("read download: %w", err)
	}

	defer func() { _ = in.Close() }()

	tmp := dst + partialSuffix

	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, pol.FileMode)
	if err != nil {
		return fmt.Errorf("create in library: %w", err)
	}

	_, err = io.Copy(out, &ctxReader{ctx: ctx, r: in})
	if err == nil {
		err = out.Sync()
	}

	if cerr := out.Close(); err == nil {
		err = cerr
	}

	if err == nil {
		err = os.Chmod(tmp, pol.FileMode)
	}

	if err == nil {
		err = os.Rename(tmp, dst)
	}

	if err != nil {
		_ = os.Remove(tmp)

		if ctx.Err() != nil {
			return ctx.Err()
		}

		return fmt.Errorf("copy into library: %w", err)
	}

	syncDir(filepath.Dir(dst))

	return nil
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

func isCrossDevice(err error) bool {
	return errors.Is(err, syscall.EXDEV) || strings.Contains(strings.ToLower(err.Error()), "not same device")
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
