package par2

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // PAR2 is defined in terms of MD5.
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
)

const (
	// defaultMemory bounds repair buffers when RepairOptions.Memory is 0.
	defaultMemory = 256 << 20
	// minChunk is the smallest part of a slice processed per pass.
	minChunk = 4 << 10
	// spareRows are recovery slices offered beyond the minimum, so a
	// combination that happens to be singular can be swapped out.
	spareRows = 32
)

var (
	// ErrNotEnoughRecovery means the damage needs more recovery slices than
	// were read.
	ErrNotEnoughRecovery = errors.New("par2: not enough recovery slices")
	// ErrSingular means the recovery slices at hand cannot rebuild the
	// missing slices, because they are not independent.
	ErrSingular = errors.New("par2: recovery slices cannot solve the damage")
	// ErrTooLarge means repair would need more memory than allowed.
	ErrTooLarge = errors.New("par2: repair too large")
	// ErrMismatch means a repaired file does not match its MD5, so the
	// recovery data or the data assumed good was wrong.
	ErrMismatch = errors.New("par2: repaired file does not match its checksum")
)

// RepairOptions configure Repair.
type RepairOptions struct {
	// Target returns the path to create a missing file at. It is required
	// when any file is missing.
	Target func(*File) string
	// Memory bounds the slice buffers and matrix repair holds, in bytes; 0
	// means 256 MiB. More memory means fewer passes over the input.
	Memory int64
	// Progress, if set, is called after each pass over the input.
	Progress func(done, total int)
}

// slot is one input slice of the recovery set.
type slot struct {
	path   string
	file   *File
	index  int // within the file
	global int // within the recovery set: selects its constant
}

// repairJob is a repair being carried out.
type repairJob struct {
	ctx   context.Context
	root  *os.Root
	g     *field
	size  int64 // slice size
	have  []slot
	lost  []slot
	rows  []RecoverySlice // chosen recovery slices, one per equation
	inv   [][]uint16      // inv[m][p]: factor of equation p in lost slice m
	chunk int64
}

// Repair rebuilds the damaged and missing files in rep, which Verify
// returned for set, writing only inside root. Damaged files are repaired
// where they were found; missing files are created at RepairOptions.Target.
// Every repaired file is truncated to its declared size and must then match
// its MD5, or Repair returns ErrMismatch.
//
// Recovery slices are read from set.Recovery, so the volumes must have been
// read with Parser.ReadFile using paths relative to root. Only bytes that
// belong in a slice are ever written, so an interrupted repair can be run
// again after verifying anew.
func Repair(ctx context.Context, root *os.Root, set *Set, rep *Report, o RepairOptions) error {
	byID := map[ID]FileResult{}
	for _, r := range rep.Files {
		byID[r.File.ID] = r
	}

	j := &repairJob{ctx: ctx, root: root, g: gf(), size: set.SliceSize}

	var fix []FileResult // files to rewrite, with the path to use

	global := 0
	for _, f := range set.Files {
		r, ok := byID[f.ID]
		if !ok {
			return fmt.Errorf("par2: report does not cover %q", f.Name)
		}

		n := f.SliceCount(set.SliceSize)
		if global+n > maxInputSlices {
			return fmt.Errorf("%w: more than %d", ErrTooManySlices, maxInputSlices)
		}

		if r.Status == Missing {
			if o.Target == nil {
				return fmt.Errorf("par2: no target for missing file %q", f.Name)
			}

			r.Found = o.Target(f)
			r.Bad = allSlices(f, set.SliceSize)
		}

		bad := make([]bool, n)
		for _, i := range r.Bad {
			if i >= 0 && i < n {
				bad[i] = true
			}
		}

		for i := range n {
			s := slot{path: r.Found, file: f, index: i, global: global + i}
			if bad[i] {
				j.lost = append(j.lost, s)
			} else {
				j.have = append(j.have, s)
			}
		}

		if r.Status != OK {
			fix = append(fix, r)
		}

		global += n
	}

	if len(fix) == 0 {
		return nil
	}

	memory := o.Memory
	if memory <= 0 {
		memory = defaultMemory
	}

	if err := j.plan(set.Recovery, memory); err != nil {
		return err
	}

	for _, r := range fix {
		if r.Status != Missing {
			continue
		}

		// Never replace a file that appeared under the name since Verify.
		f, err := root.OpenFile(r.Found, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return fmt.Errorf("create %s: %w", r.Found, err)
		}

		if err := f.Close(); err != nil {
			return fmt.Errorf("create %s: %w", r.Found, err)
		}
	}

	if len(j.lost) > 0 {
		passes := int((j.size + j.chunk - 1) / j.chunk)

		for pass := range passes {
			if err := j.pass(int64(pass) * j.chunk); err != nil {
				return err
			}

			if o.Progress != nil {
				o.Progress(pass+1, passes)
			}
		}
	}

	for _, r := range fix {
		if err := finish(ctx, root, r.Found, r.File); err != nil {
			return err
		}
	}

	return nil
}

// plan chooses recovery slices for the lost slices, inverts their matrix
// and sizes the buffers within memory.
func (j *repairJob) plan(recovery []RecoverySlice, memory int64) error {
	k := len(j.lost)
	if k == 0 {
		return nil
	}

	if len(recovery) < k {
		return fmt.Errorf("%w: %d slices are damaged and %d recovery slices are available", ErrNotEnoughRecovery, k, len(recovery))
	}

	rows := recovery[:min(len(recovery), k+spareRows)]

	matrix := int64(len(rows)) * int64(k) * 2
	if matrix > memory/2 {
		return fmt.Errorf("%w: %d damaged slices", ErrTooLarge, k)
	}

	cols := make([]int, k)
	for m, s := range j.lost {
		cols[m] = s.global
	}

	inv, chosen, err := solve(j.ctx, j.g, cols, rows)
	if err != nil {
		return err
	}

	j.inv, j.rows = inv, chosen

	// k accumulators, one output and one input buffer per pass.
	chunk := (memory - matrix) / int64(k+2)
	chunk = max(chunk&^3, minChunk)
	j.chunk = min(chunk, j.size)

	return nil
}

// solve inverts the matrix of the lost slices' constants raised to the
// recovery exponents, A[p][m] = c_cols[m] ^ e_p. It picks k independent rows
// among rows and returns the inverse with the rows it used, in order.
// Elimination is Gauss–Jordan in place: once column p is reduced it holds
// the inverse's column p, so the matrix needs no identity beside it.
func solve(ctx context.Context, g *field, cols []int, rows []RecoverySlice) ([][]uint16, []RecoverySlice, error) {
	k := len(cols)

	a := make([][]uint16, len(rows))
	for p, r := range rows {
		a[p] = make([]uint16, k)
		for m, c := range cols {
			a[p][m] = g.coefficient(c, r.Exponent)
		}
	}

	order := slices.Clone(rows)

	var t mulTable

	for p := range k {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}

		piv := -1
		for r := p; r < len(a); r++ {
			if a[r][p] != 0 {
				piv = r
				break
			}
		}

		if piv < 0 {
			return nil, nil, fmt.Errorf("%w: %d damaged slices, %d recovery slices tried", ErrSingular, k, len(rows))
		}

		a[p], a[piv] = a[piv], a[p]
		order[p], order[piv] = order[piv], order[p]

		inv := g.inv(a[p][p])
		a[p][p] = 1
		g.table(inv, &t)

		for i, w := range a[p] {
			a[p][i] = t.lo[w&0xff] ^ t.hi[w>>8]
		}

		for r := range a {
			f := a[r][p]
			if r == p || f == 0 {
				continue
			}

			a[r][p] = 0
			g.table(f, &t)
			mulAdd(a[r], a[p], &t)
		}
	}

	return a[:k], order[:k], nil
}

// pass rebuilds bytes [off, off+chunk) of every lost slice. With R_p the
// recovery slice of equation p, the lost slices satisfy
// sum_m A[p][m] D_m = R_p + sum_present c_i^e_p D_i, so D = inv · that.
func (j *repairJob) pass(off int64) error {
	n := min(j.chunk, j.size-off)
	words := int(n / 2) // slice sizes and chunks are multiples of 4
	raw := make([]byte, n)

	acc := make([][]uint16, len(j.rows))
	for p := range acc {
		acc[p] = make([]uint16, words)
	}

	files := &handles{root: j.root, flag: os.O_RDONLY}
	defer func() { _ = files.close() }()

	for p, r := range j.rows {
		if err := j.ctx.Err(); err != nil {
			return err
		}

		f, err := files.open(r.Source)
		if err != nil {
			return err
		}

		// A recovery slice cut short since it was parsed is an error, not
		// zeros.
		if err := readWords(f, r.Offset+off, n, true, raw, acc[p]); err != nil {
			return fmt.Errorf("read recovery slice %d from %s: %w", r.Exponent, r.Source, err)
		}
	}

	in := make([]uint16, words)

	var t mulTable

	for _, s := range j.have {
		if err := j.ctx.Err(); err != nil {
			return err
		}

		f, err := files.open(s.path)
		if err != nil {
			return err
		}

		if err := readWords(f, int64(s.index)*j.size+off, j.valid(s, off, n), false, raw, in); err != nil {
			return fmt.Errorf("read %s: %w", s.path, err)
		}

		for p, r := range j.rows {
			j.g.table(j.g.coefficient(s.global, r.Exponent), &t)
			mulAdd(acc[p], in, &t)
		}
	}

	_ = files.close()

	out := in
	writes := &handles{root: j.root, flag: os.O_RDWR}
	defer func() { _ = writes.close() }()

	for m, s := range j.lost {
		if err := j.ctx.Err(); err != nil {
			return err
		}

		clear(out)

		for p := range j.rows {
			if c := j.inv[m][p]; c != 0 {
				j.g.table(c, &t)
				mulAdd(out, acc[p], &t)
			}
		}

		valid := j.valid(s, off, n)
		if valid == 0 {
			continue
		}

		for i, w := range out {
			binary.LittleEndian.PutUint16(raw[2*i:], w)
		}

		f, err := writes.open(s.path)
		if err != nil {
			return err
		}

		if _, err := f.WriteAt(raw[:valid], int64(s.index)*j.size+off); err != nil {
			return fmt.Errorf("write %s: %w", s.path, err)
		}
	}

	return writes.close()
}

// valid is how many of the n bytes at off within slot s's slice lie inside
// its file; the rest is the zero padding of a last slice.
func (j *repairJob) valid(s slot, off, n int64) int64 {
	inFile := min(j.size, s.file.Size-int64(s.index)*j.size)

	return max(min(n, inFile-off), 0)
}

// readWords reads valid bytes at off into raw, zero-pads raw and decodes it
// as little-endian words into w. Unless strict, a file shorter than
// expected reads as zeros; a present slice is only read where its file
// verified, so that is the zero padding of a short file.
func readWords(f *os.File, off, valid int64, strict bool, raw []byte, w []uint16) error {
	clear(raw)

	if valid > 0 {
		_, err := f.ReadAt(raw[:valid], off)
		if errors.Is(err, io.EOF) && strict {
			return io.ErrUnexpectedEOF
		}

		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}

	for i := range w {
		w[i] = binary.LittleEndian.Uint16(raw[2*i:])
	}

	return nil
}

// finish truncates a repaired file to its declared size, syncs it and
// checks its MD5.
func finish(ctx context.Context, root *os.Root, name string, file *File) error {
	f, err := root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}

	defer func() { _ = f.Close() }()

	if err := f.Truncate(file.Size); err != nil {
		return fmt.Errorf("truncate %s: %w", name, err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}

	h := md5.New() //nolint:gosec // see import
	if _, err := io.Copy(h, &ctxReader{ctx: ctx, r: f}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		return fmt.Errorf("read %s: %w", name, err)
	}

	if !bytes.Equal(h.Sum(nil), file.MD5[:]) {
		return fmt.Errorf("%w: %s", ErrMismatch, file.Name)
	}

	return f.Close()
}

// ctxReader stops a long read when ctx is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}

	return c.r.Read(b)
}

// maxHandles bounds the files one handles keeps open.
const maxHandles = 64

// handles keeps files open across the slices of one pass.
type handles struct {
	root  *os.Root
	flag  int
	files map[string]*os.File
}

func (h *handles) open(name string) (*os.File, error) {
	if f, ok := h.files[name]; ok {
		return f, nil
	}

	if len(h.files) >= maxHandles {
		if err := h.close(); err != nil {
			return nil, err
		}
	}

	f, err := h.root.OpenFile(name, h.flag, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}

	if h.files == nil {
		h.files = map[string]*os.File{}
	}

	h.files[name] = f

	return f, nil
}

func (h *handles) close() error {
	var first error

	for name, f := range h.files {
		if h.flag != os.O_RDONLY {
			if err := f.Sync(); err != nil && first == nil {
				first = fmt.Errorf("sync %s: %w", name, err)
			}
		}

		if err := f.Close(); err != nil && first == nil {
			first = fmt.Errorf("close %s: %w", name, err)
		}

		delete(h.files, name)
	}

	return first
}
