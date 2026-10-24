package library

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/getnabbed/agent/internal/engine"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// importMarker records a finished import in the work directory, so a rerun
// after a crash reports the same files instead of looking for downloads
// that were already moved.
const importMarker = "import.json"

var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".wmv": true,
	".ts": true, ".m2ts": true, ".webm": true, ".mpg": true, ".mpeg": true,
}

// Importer places a finished download into its library root, named from
// the download's NamingContext. It implements download.Importer.
type Importer struct {
	Library *Manager
	Log     *slog.Logger
}

// Import implements download.Importer. It places the largest video file,
// and the extras the import policy names, and returns what it placed,
// relative to the root.
func (im *Importer) Import(ctx context.Context, job engine.Job, res *engine.Result, d *pb.Download) (*pb.ImportCompleted, error) {
	log := im.Log
	if log == nil {
		log = slog.Default()
	}

	log = log.With("download_id", d.GetId())

	if done, ok := readImportMarker(job.Dir); ok {
		return done, nil
	}

	root, err := im.Library.Root(ctx, d.GetRootId())
	if err != nil {
		// The cloud may still send the root; the download stays on disk.
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_IMPORT, true, "library root %q is not set up on this agent", d.GetRootId())
	}

	policy, err := im.Library.ImportPolicy(ctx)
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "read import policy: %v", err)
	}

	main, extras := pickFiles(res, policy.GetExtras())
	if main == "" {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_IMPORT, false, "the download has no video file to import")
	}

	rel, err := RelPath(root, d.GetNaming(), policy.GetSanitizer(), path.Ext(main))
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_IMPORT, false, "%v", err)
	}

	// Upgrades replace the old file only when it has the same name. The
	// cloud does not send replaces_path yet, and nothing else is deleted.
	replace := d.ReplacesPath != nil && path.Clean(d.GetReplacesPath()) == rel

	plan := []placed{{src: main, rel: rel, replace: replace}}
	stem := strings.TrimSuffix(rel, path.Ext(rel))
	used := map[string]bool{strings.ToLower(rel): true}

	for _, x := range extras {
		to := stem + strings.ToLower(path.Ext(x))
		for i := 2; used[strings.ToLower(to)]; i++ {
			to = fmt.Sprintf("%s.%d%s", stem, i, strings.ToLower(path.Ext(x)))
		}

		used[strings.ToLower(to)] = true
		plan = append(plan, placed{src: x, rel: to})
	}

	done := &pb.ImportCompleted{DownloadId: d.GetId(), RootId: root.GetId(), Quality: d.GetRelease().GetQuality()}
	torrent := d.GetRelease().GetProtocol() == pb.Protocol_PROTOCOL_TORRENT

	for _, p := range plan {
		size, err := im.place(ctx, job, root, policy.GetMode(), torrent, p, log)
		if err != nil {
			return nil, err
		}

		done.Files = append(done.Files, &pb.ImportedFile{RelativePath: p.rel, Size: uint64(max(size, 0))})
	}

	if err := writeImportMarker(job.Dir, done); err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "record import: %v", err)
	}

	log.Info("imported download", "root_id", root.GetId(), "files", len(done.Files), "path", rel)

	return done, nil
}

type placed struct {
	src, rel string
	replace  bool
}

func (im *Importer) place(ctx context.Context, job engine.Job, root *pb.LibraryRoot, mode pb.ImportMode, torrent bool, p placed, log *slog.Logger) (int64, error) {
	src := filepath.Join(job.Dir, filepath.FromSlash(p.src))
	dst := filepath.Join(root.GetPath(), filepath.FromSlash(p.rel))

	fi, err := os.Stat(src)
	if errors.Is(err, fs.ErrNotExist) {
		// A rerun after a crash: the move already happened.
		if dfi, derr := os.Stat(dst); derr == nil && dfi.Mode().IsRegular() {
			return dfi.Size(), nil
		}
	}

	if err != nil {
		return 0, engine.Fail(pb.FailureClass_FAILURE_CLASS_IMPORT, false, "downloaded file %s is missing", p.src)
	}

	method, err := im.Library.Policy().Place(ctx, Placement{Src: src, Root: root.GetPath(), Rel: p.rel, Mode: mode, Torrent: torrent, Replace: p.replace})

	switch {
	case err == nil:
		log.Debug("placed file", "from", p.src, "to", p.rel, "method", method)
		return fi.Size(), nil
	case ctx.Err() != nil:
		return 0, ctx.Err()
	case errors.Is(err, ErrExists):
		return 0, engine.Fail(pb.FailureClass_FAILURE_CLASS_IMPORT, false, "%s already exists in the library; remove it or pick another release", p.rel)
	case errors.Is(err, ErrNotAllowed), errors.Is(err, ErrInvalid), errors.Is(err, ErrTemplateUnsafe):
		// The local policy or the root changed; fixable without a new release.
		return 0, engine.Fail(pb.FailureClass_FAILURE_CLASS_IMPORT, true, "%v", err)
	}

	return 0, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "place %s: %v", p.rel, err)
}

// pickFiles returns the main video, the largest video file, and the
// extras whose extensions the policy lists. Extras in a folder named
// "sample" are skipped, as are other videos.
func pickFiles(res *engine.Result, extras []string) (string, []string) {
	want := map[string]bool{}
	for _, e := range extras {
		e = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), "."))
		if e != "" {
			want["."+e] = true
		}
	}

	main, size := "", int64(-1)

	for _, f := range res.Files {
		p := filepath.ToSlash(f.Path)
		if videoExts[strings.ToLower(path.Ext(p))] && f.Size > size {
			main, size = p, f.Size
		}
	}

	var out []string

	for _, f := range res.Files {
		p := filepath.ToSlash(f.Path)
		if p == main || !want[strings.ToLower(path.Ext(p))] || isSample(p) {
			continue
		}

		out = append(out, p)
	}

	slices.Sort(out)

	return main, out
}

func isSample(p string) bool {
	for _, part := range strings.Split(strings.ToLower(p), "/") {
		if part == "sample" || part == "samples" {
			return true
		}
	}

	return false
}

func readImportMarker(dir string) (*pb.ImportCompleted, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, importMarker))
	if err != nil {
		return nil, false
	}

	var done pb.ImportCompleted
	if err := protojson.Unmarshal(raw, &done); err != nil {
		return nil, false
	}

	return &done, true
}

func writeImportMarker(dir string, done *pb.ImportCompleted) error {
	raw, err := protojson.Marshal(done)
	if err != nil {
		return err
	}

	tmp := filepath.Join(dir, importMarker+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}

	return os.Rename(tmp, filepath.Join(dir, importMarker))
}
