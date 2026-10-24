// Package postproc turns what an engine downloaded into complete files:
// PAR2 verification and name restoration, PAR2 repair with recovery
// volumes fetched on demand, then RAR, 7z and ZIP extraction (design 0005).
// It works only inside the job's work directory, through os.Root.
package postproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/getnabbed/agent/internal/engine"
	"github.com/getnabbed/agent/internal/engine/usenet"
	"github.com/getnabbed/agent/internal/par2"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// maxIndexBytes bounds one PAR2 index file. Real ones are kilobytes.
const maxIndexBytes = 32 << 20

// Assembler verifies downloads against their PAR2 recovery set, restores
// obfuscated file names, repairs damaged and missing files, extracts RAR,
// 7z and ZIP archives into extracted/, and then deletes the archives and
// PAR2 files. It implements download.Assembler.
type Assembler struct {
	Log *slog.Logger
	// Fetcher downloads recovery volumes the engine deferred, when repair
	// needs them. Without one, only volumes already on disk are used.
	Fetcher Fetcher
	// RepairMemory bounds the memory PAR2 repair uses; 0 means 256 MiB.
	RepairMemory int64
	// MaxEntries caps files and directories per job's archives; 0 means
	// 10000.
	MaxEntries int
	// Slack is how far archive output may exceed twice the downloaded size;
	// 0 means 1 GiB. Releases are mostly stored uncompressed, so more than
	// that is a decompression bomb.
	Slack int64

	// repairProgress, if set, is called after each repair pass (tests).
	repairProgress func(done, total int)
}

// Fetcher downloads files an engine deliberately left out of a job, such as
// PAR2 recovery volumes. The usenet engine implements it.
type Fetcher interface {
	// FetchFiles downloads the named files, as listed in the engine
	// result's Deferred, into the job's work directory. The result lists
	// the paths written; MissingArticles counts parts no provider had.
	FetchFiles(ctx context.Context, job engine.Job, names []string) (*engine.Result, error)
}

// Assemble implements download.Assembler. It can run again on its own
// output: files already renamed are found under their new names, and a
// finished extraction is not repeated.
func (a *Assembler) Assemble(ctx context.Context, job engine.Job, res *engine.Result) (*engine.Result, error) {
	log := a.Log
	if log == nil {
		log = slog.Default()
	}

	log = log.With("download_id", job.ID)

	root, err := os.OpenRoot(job.Dir)
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "open work dir: %v", err)
	}

	defer func() { _ = root.Close() }()

	if m, ok := readMarker(root); ok {
		return finish(root, m, log), nil
	}

	files, err := listFiles(root, res)
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "list files: %v", err)
	}

	var index, volumes, data []string
	for _, f := range files {
		switch {
		case par2.IsVolume(f):
			// Recovery volumes are only fetched for repair.
			volumes = append(volumes, f)
		case strings.EqualFold(path.Ext(f), ".par2"):
			index = append(index, f)
		default:
			data = append(data, f)
		}
	}

	par2Files := slices.Concat(index, volumes)

	if len(index) == 0 {
		if err := withoutPar2(res); err != nil {
			return nil, err
		}

		return a.unpack(ctx, root, files, par2Files, log)
	}

	p, set, err := readSet(root, index)
	if err != nil {
		log.Warn("cannot read PAR2 index", "err", err)

		if err := withoutPar2(res); err != nil {
			return nil, err
		}

		return a.unpack(ctx, root, files, par2Files, log)
	}

	dir := path.Dir(index[0])
	targets := map[*par2.File]string{}
	used := map[string]bool{}

	for i, f := range set.Files {
		targets[f] = path.Join(dir, uniqueTarget(usenet.SafeName(f.Name, i), used))
	}

	rep, err := par2.Verify(ctx, root.FS(), set, data, par2.VerifyOptions{
		Target: func(f *par2.File) string { return targets[f] },
		// The native engines check every byte in transit (yEnc CRC per usenet
		// article, SHA-1 per torrent piece). With nothing missing the data is
		// what was posted, so a size and 16 KiB match suffices.
		Quick: res.MissingArticles == 0,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "verify: %v", err)
	}

	renamed := a.restoreNames(root, rep, targets, log)

	current := make([]string, 0, len(files))
	for _, f := range files {
		if to, ok := renamed[f]; ok {
			f = to
		}

		current = append(current, f)
	}

	if rep.Complete() {
		log.Info("par2 verification passed", "files", len(rep.Files), "renamed", len(renamed))
		return a.unpack(ctx, root, current, par2Files, log)
	}

	// Repair works on the files where they are now.
	for i, r := range rep.Files {
		if to, ok := renamed[r.Found]; ok {
			rep.Files[i].Found = to
		}
	}

	r := &repair{
		a: a, job: job, res: res, root: root, parser: p, set: set, rep: rep,
		targets: targets, volumes: volumes, log: log,
	}

	if err := r.run(ctx); err != nil {
		return nil, err
	}

	// Repair may have created files and fetched volumes; list them afresh.
	files, err = listFiles(root, res)
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "list files: %v", err)
	}

	par2Files = par2Files[:0]
	for _, f := range files {
		if strings.EqualFold(path.Ext(f), ".par2") {
			par2Files = append(par2Files, f)
		}
	}

	return a.unpack(ctx, root, files, par2Files, log)
}

// withoutPar2 decides a release that has no usable PAR2 index: it can
// only pass when nothing is missing.
func withoutPar2(res *engine.Result) error {
	if res.MissingArticles > 0 {
		return engine.Fail(pb.FailureClass_FAILURE_CLASS_INCOMPLETE, false,
			"%d articles missing and the release has no usable PAR2 files to repair them", res.MissingArticles)
	}

	return nil
}

// restoreNames renames files found under another name to their recovery-set
// name, when that name is free. It returns old path to new path.
func (a *Assembler) restoreNames(root *os.Root, rep *par2.Report, targets map[*par2.File]string, log *slog.Logger) map[string]string {
	renamed := map[string]string{}

	for _, r := range rep.Files {
		to := targets[r.File]
		if r.Found == "" || r.Found == to {
			continue
		}

		if _, err := root.Lstat(to); err == nil {
			log.Warn("not restoring file name: the name is taken", "from", r.Found, "to", to)
			continue
		}

		if err := root.Rename(r.Found, to); err != nil {
			log.Warn("restore file name", "from", r.Found, "to", to, "err", err)
			continue
		}

		log.Info("restored file name from par2", "from", r.Found, "to", to)
		renamed[r.Found] = to
	}

	return renamed
}

// listFiles returns the regular files in the directories that hold the
// result's files, so names changed by an earlier run are still seen. Paths
// are relative to the work directory, sorted.
func listFiles(root *os.Root, res *engine.Result) ([]string, error) {
	dirs := map[string]bool{}
	for _, f := range res.Files {
		dirs[path.Dir(filepath.ToSlash(f.Path))] = true
	}

	var out []string

	for d := range dirs {
		entries, err := fs.ReadDir(root.FS(), d)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}

		if err != nil {
			return nil, err
		}

		for _, e := range entries {
			if e.Type().IsRegular() {
				out = append(out, path.Join(d, e.Name()))
			}
		}
	}

	slices.Sort(out)

	return out, nil
}

// readSet parses the index files. The parser is returned so repair can add
// volumes to it.
func readSet(root *os.Root, index []string) (*par2.Parser, *par2.Set, error) {
	p := par2.NewParser()

	for _, name := range index {
		f, err := root.Open(name)
		if err != nil {
			return nil, nil, err
		}

		err = p.Read(io.LimitReader(f, maxIndexBytes))
		_ = f.Close()

		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
	}

	set, err := p.Set()

	return p, set, err
}

// uniqueTarget keeps two recovery-set files from claiming the same
// sanitised name.
func uniqueTarget(name string, used map[string]bool) string {
	key := strings.ToLower(name)
	if !used[key] {
		used[key] = true
		return name
	}

	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)

	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if k := strings.ToLower(cand); !used[k] {
			used[k] = true
			return cand
		}
	}
}
