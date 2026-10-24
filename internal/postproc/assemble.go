// Package postproc turns what an engine downloaded into complete files:
// PAR2 verification and name restoration now, repair and extraction later
// (design 0005). It works only inside the job's work directory, through
// os.Root.
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
	"slices"
	"strings"

	"github.com/getnabbed/agent/internal/engine"
	"github.com/getnabbed/agent/internal/engine/usenet"
	"github.com/getnabbed/agent/internal/par2"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// maxIndexBytes bounds one PAR2 index file. Real ones are kilobytes.
const maxIndexBytes = 32 << 20

// Assembler verifies downloads against their PAR2 recovery set and restores
// obfuscated file names. It implements download.Assembler.
type Assembler struct {
	Log *slog.Logger
}

// Assemble implements download.Assembler. It can run again on its own
// output: files already renamed are found under their new names.
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

	files, err := listFiles(root, res)
	if err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "list files: %v", err)
	}

	var index, data []string
	for _, f := range files {
		switch {
		case par2.IsVolume(f):
			// Recovery volumes are only fetched for repair.
		case strings.EqualFold(path.Ext(f), ".par2"):
			index = append(index, f)
		default:
			data = append(data, f)
		}
	}

	if len(index) == 0 {
		return a.withoutPar2(res)
	}

	set, err := readSet(root, index)
	if err != nil {
		log.Warn("cannot read PAR2 index", "err", err)
		return a.withoutPar2(res)
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

	out := &engine.Result{MissingArticles: res.MissingArticles, Deferred: res.Deferred}
	for _, f := range files {
		if to, ok := renamed[f]; ok {
			f = to
		}

		fi, err := root.Stat(f)
		if err != nil {
			continue
		}

		out.Files = append(out.Files, engine.File{Path: f, Size: fi.Size()})
	}

	slices.SortFunc(out.Files, func(a, b engine.File) int { return strings.Compare(a.Path, b.Path) })

	if rep.Complete() {
		log.Info("par2 verification passed", "files", len(rep.Files), "renamed", len(renamed))
		return out, nil
	}

	available := len(set.RecoveryExponents)
	for _, name := range res.Deferred {
		if n, ok := par2.VolumeBlocks(name); ok {
			available += n
		}
	}

	damaged, missing := 0, 0
	for _, r := range rep.Files {
		switch r.Status {
		case par2.Damaged:
			damaged++
		case par2.Missing:
			missing++
		}
	}

	log.Warn("par2 verification failed", "damaged_files", damaged, "missing_files", missing,
		"blocks_needed", rep.Needed, "blocks_available", available)

	if rep.Needed > available {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_INCOMPLETE, false,
			"%d damaged and %d missing files need %d recovery blocks; the release has %d", damaged, missing, rep.Needed, available)
	}

	// Repairable once repair ships: retrying then finishes from the data on
	// disk.
	return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_REPAIR, true,
		"%d damaged and %d missing files need %d of %d recovery blocks, and PAR2 repair is not implemented yet",
		damaged, missing, rep.Needed, available)
}

// withoutPar2 decides a release that has no usable PAR2 index: it passes
// unchanged when nothing is missing.
func (a *Assembler) withoutPar2(res *engine.Result) (*engine.Result, error) {
	if res.MissingArticles > 0 {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_INCOMPLETE, false,
			"%d articles missing and the release has no usable PAR2 files to repair them", res.MissingArticles)
	}

	return nil, nil
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
		dirs[path.Dir(f.Path)] = true
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

func readSet(root *os.Root, index []string) (*par2.Set, error) {
	p := par2.NewParser()

	for _, name := range index {
		f, err := root.Open(name)
		if err != nil {
			return nil, err
		}

		err = p.Read(io.LimitReader(f, maxIndexBytes))
		_ = f.Close()

		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}

	return p.Set()
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
