package postproc

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"slices"

	"github.com/getnabbed/agent/internal/engine"
	"github.com/getnabbed/agent/internal/par2"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// repair is the repair stage of one Assemble: read the recovery volumes on
// disk, fetch more deferred ones while the damage needs them, then rebuild
// the damaged and missing files.
type repair struct {
	a       *Assembler
	job     engine.Job
	res     *engine.Result
	root    *os.Root
	parser  *par2.Parser
	set     *par2.Set
	rep     *par2.Report
	targets map[*par2.File]string
	volumes []string // volumes already in the work directory
	log     *slog.Logger
	fetched int // recovery volumes fetched for this repair
}

func (r *repair) run(ctx context.Context) error {
	damaged, missing := 0, 0
	for _, f := range r.rep.Files {
		switch f.Status {
		case par2.Damaged:
			damaged++
		case par2.Missing:
			missing++
		}
	}

	if err := r.read(r.volumes); err != nil {
		return err
	}

	r.log.Warn("par2 verification failed", "damaged_files", damaged, "missing_files", missing,
		"blocks_needed", r.rep.Needed, "blocks_on_disk", len(r.set.Recovery), "volumes_deferred", len(r.res.Deferred))

	if err := r.fetch(ctx); err != nil {
		return err
	}

	if have := len(r.set.Recovery); r.rep.Needed > have {
		r.job.Notes.Error("par2.repair_failed", fmt.Sprintf("PAR2 repair needs %d recovery blocks; only %d could be obtained", r.rep.Needed, have),
			"missing_blocks", r.rep.Needed-have, "damaged_files", damaged, "missing_files", missing)

		if r.a.Fetcher == nil && have+r.deferredBlocks() >= r.rep.Needed {
			// The release could be repaired by an agent that can fetch.
			return engine.Fail(pb.FailureClass_FAILURE_CLASS_REPAIR, true,
				"%d damaged and %d missing files need %d recovery blocks; %d are on disk and this agent cannot fetch the deferred volumes",
				damaged, missing, r.rep.Needed, have)
		}

		return engine.Fail(pb.FailureClass_FAILURE_CLASS_INCOMPLETE, false,
			"%d damaged and %d missing files need %d recovery blocks; only %d could be obtained", damaged, missing, r.rep.Needed, have)
	}

	r.log.Info("repairing with par2", "blocks_needed", r.rep.Needed, "blocks_available", len(r.set.Recovery))

	err := par2.Repair(ctx, r.root, r.set, r.rep, par2.RepairOptions{
		Target:   func(f *par2.File) string { return r.targets[f] },
		Memory:   r.a.RepairMemory,
		Progress: r.a.repairProgress,
	})
	if err != nil {
		return classifyRepair(ctx, err)
	}

	r.log.Info("par2 repair finished", "damaged_files", damaged, "missing_files", missing, "blocks_used", r.rep.Needed)
	r.job.Notes.Info("par2.repaired",
		fmt.Sprintf("PAR2 repaired %d damaged and %d missing files with %d recovery blocks", damaged, missing, r.rep.Needed),
		"blocks", r.rep.Needed, "volumes_fetched", r.fetched, "damaged_files", damaged, "missing_files", missing)

	return nil
}

// classifyRepair turns a par2.Repair error into the failure the cloud sees.
func classifyRepair(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	switch {
	case errors.Is(err, par2.ErrNotEnoughRecovery), errors.Is(err, par2.ErrSingular),
		errors.Is(err, par2.ErrTooLarge), errors.Is(err, par2.ErrTooManySlices):
		return engine.Fail(pb.FailureClass_FAILURE_CLASS_INCOMPLETE, false, "cannot repair: %v", err)
	case errors.Is(err, par2.ErrMismatch):
		return engine.Fail(pb.FailureClass_FAILURE_CLASS_REPAIR, false,
			"repair finished but the result is wrong (%v); the release's recovery data does not match its files", err)
	}

	return engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "repair: %v", err)
}

// read adds volumes to the parser, recording where their recovery slices
// are, and refreshes the set.
func (r *repair) read(volumes []string) error {
	for _, name := range volumes {
		f, err := r.root.Open(name)
		if err != nil {
			return engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "open %s: %v", name, err)
		}

		err = r.parser.ReadFile(name, f)
		_ = f.Close()

		if err != nil {
			return engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "read %s: %v", name, err)
		}
	}

	set, err := r.parser.Set()
	if err != nil {
		// The index parsed before, so the set cannot have gone away.
		return engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "par2: %v", err)
	}

	r.set = set

	return nil
}

// volume is a deferred recovery volume that could still add blocks.
type volume struct {
	name   string
	blocks int
}

// fetch downloads deferred volumes until the set has the blocks repair
// needs or none are left to try. Each round fetches the fewest blocks that
// would cover the shortfall; a volume with missing articles yields fewer
// blocks than its name promises, so another round may follow.
func (r *repair) fetch(ctx context.Context) error {
	if r.a.Fetcher == nil {
		return nil
	}

	tried := map[string]bool{}

	for len(r.set.Recovery) < r.rep.Needed {
		cands := r.candidates(tried)
		if len(cands) == 0 {
			return nil
		}

		pick := chooseVolumes(cands, r.rep.Needed-len(r.set.Recovery))

		names := make([]string, len(pick))
		for i, v := range pick {
			names[i] = v.name
			tried[v.name] = true
		}

		r.log.Info("fetching par2 recovery volumes", "volumes", len(names), "blocks_short", r.rep.Needed-len(r.set.Recovery))

		got, err := r.a.Fetcher.FetchFiles(ctx, r.job, names)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			var f *engine.Failure
			if errors.As(err, &f) {
				return f
			}

			return engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "fetch recovery volumes: %v", err)
		}

		if got.MissingArticles > 0 {
			r.log.Warn("recovery volumes have missing articles", "articles", got.MissingArticles)
		}

		var fetched []string
		for _, f := range got.Files {
			fetched = append(fetched, f.Path)
		}

		r.fetched += len(fetched)

		if err := r.read(fetched); err != nil {
			return err
		}
	}

	return nil
}

// candidates are the deferred volumes not yet tried that would add blocks:
// absent, or on disk with fewer slices than promised.
func (r *repair) candidates(tried map[string]bool) []volume {
	have := map[string]int{}
	for _, s := range r.set.Recovery {
		have[path.Base(s.Source)]++
	}

	var out []volume

	for _, name := range r.res.Deferred {
		n, ok := par2.VolumeBlocks(name)
		if !ok || tried[name] || have[name] >= n {
			continue
		}

		out = append(out, volume{name: name, blocks: n - have[name]})
	}

	return out
}

// deferredBlocks counts the blocks the deferred volumes promise beyond
// those already read.
func (r *repair) deferredBlocks() int {
	n := 0
	for _, v := range r.candidates(nil) {
		n += v.blocks
	}

	return n
}

// maxChoiceWork bounds the subset search; beyond it volumes are taken
// smallest first.
const maxChoiceWork = 1 << 26

// chooseVolumes picks volumes whose blocks cover need with the fewest
// blocks in total, preferring smaller volumes among equal totals. If no
// choice covers need it returns them all.
func chooseVolumes(cands []volume, need int) []volume {
	cands = slices.Clone(cands)
	slices.SortFunc(cands, func(a, b volume) int {
		return cmp.Or(cmp.Compare(a.blocks, b.blocks), cmp.Compare(a.name, b.name))
	})

	total, largest := 0, 0
	for _, v := range cands {
		total += v.blocks
		largest = max(largest, v.blocks)
	}

	if total <= need {
		return cands
	}

	// A minimal cover exceeds need by less than its largest volume.
	limit := need + largest
	if len(cands)*limit > maxChoiceWork {
		return smallestFirst(cands, need)
	}

	// from[s] is the volume that first reached a total of s, a 0/1
	// knapsack over the volumes in ascending size.
	reached := make([]bool, limit+1)
	from := make([]int, limit+1)
	reached[0] = true

	for i, v := range cands {
		for s := limit; s >= v.blocks; s-- {
			if !reached[s] && reached[s-v.blocks] {
				reached[s], from[s] = true, i
			}
		}
	}

	for s := need; s <= limit; s++ {
		if !reached[s] {
			continue
		}

		var pick []volume
		for s > 0 {
			v := cands[from[s]]
			pick = append(pick, v)
			s -= v.blocks
		}

		return pick
	}

	return cands
}

func smallestFirst(sorted []volume, need int) []volume {
	got := 0
	for i, v := range sorted {
		got += v.blocks
		if got >= need {
			return sorted[:i+1]
		}
	}

	return sorted
}

// String describes a volume for logs and test failures.
func (v volume) String() string { return fmt.Sprintf("%s(%d)", v.name, v.blocks) }
