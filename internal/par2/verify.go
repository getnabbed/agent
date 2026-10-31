package par2

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // PAR2 is defined in terms of MD5.
	"errors"
	"io"
	"io/fs"
)

// Status is a recovery-set file's state on disk.
type Status int

// Status values.
const (
	// OK means the file is present and matches.
	OK Status = iota
	// Damaged means a file was found but some slices do not match, or it
	// has the wrong length.
	Damaged
	// Missing means no candidate matches the file at all.
	Missing
)

func (s Status) String() string {
	switch s {
	case OK:
		return "ok"
	case Damaged:
		return "damaged"
	default:
		return "missing"
	}
}

// FileResult is the verification of one recovery-set file.
type FileResult struct {
	File *File
	// Found is the candidate path that holds the file, or "" when missing.
	Found  string
	Status Status
	// BadSlices is how many slices repair must rebuild.
	BadSlices int
	// Bad lists the indexes of those slices within the file. A damaged
	// file with no bad slices only has the wrong length.
	Bad []int
}

// Report is the verification of a recovery set.
type Report struct {
	Files []FileResult
	// Needed is the number of recovery blocks repair needs.
	Needed int
}

// Complete reports whether every protected file verified.
func (r *Report) Complete() bool {
	for _, f := range r.Files {
		if f.Status != OK {
			return false
		}
	}

	return true
}

// VerifyOptions configure Verify.
type VerifyOptions struct {
	// Target returns the path a recovery-set file should have among the
	// candidates, so a damaged file can be found under its real name.
	Target func(*File) string
}

type candidate struct {
	path string
	size int64
	h16k [16]byte
}

// Verify checks the protected files of set against candidates, paths in
// fsys of downloaded files. Each candidate matches at most one file.
func Verify(ctx context.Context, fsys fs.FS, set *Set, candidates []string, o VerifyOptions) (*Report, error) {
	var cands []candidate

	for _, p := range candidates {
		c, err := describe(fsys, p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}

		if err != nil {
			return nil, err
		}

		cands = append(cands, c)
	}

	used := map[string]bool{}
	byPath := map[string]candidate{}
	for _, c := range cands {
		byPath[c.path] = c
	}

	rep := &Report{}

	for _, f := range set.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Prefer the real name when it also matches by content, then any
		// candidate with the same size and 16k hash (an obfuscated name),
		// then the real name holding damaged data.
		target := ""
		if o.Target != nil {
			target = o.Target(f)
		}

		found := ""
		if c, ok := byPath[target]; ok && !used[target] && c.size == f.Size && c.h16k == f.MD5_16k {
			found = target
		}

		if found == "" {
			for _, c := range cands {
				if !used[c.path] && c.size == f.Size && c.h16k == f.MD5_16k {
					found = c.path
					break
				}
			}
		}

		if found == "" {
			if _, ok := byPath[target]; ok && !used[target] {
				found = target
			}
		}

		if found == "" {
			rep.Files = append(rep.Files, missing(f, set.SliceSize))
			continue
		}

		used[found] = true

		res, err := check(ctx, fsys, found, f, set.SliceSize)
		if err != nil {
			return nil, err
		}

		rep.Files = append(rep.Files, res)
	}

	// A file whose first 16 KiB is damaged and whose name is obfuscated
	// matches nothing above. Downloads keep their exact size, so try the
	// leftover candidates of that size and keep one with any good slice.
	for i, r := range rep.Files {
		if r.Status != Missing || r.File.Slices == nil {
			continue
		}

		for _, c := range cands {
			if used[c.path] || c.size != r.File.Size {
				continue
			}

			res, err := check(ctx, fsys, c.path, r.File, set.SliceSize)
			if err != nil {
				return nil, err
			}

			if len(res.Bad) < r.File.SliceCount(set.SliceSize) {
				used[c.path] = true
				rep.Files[i] = res

				break
			}
		}
	}

	for _, r := range rep.Files {
		rep.Needed += r.BadSlices
	}

	return rep, nil
}

func missing(f *File, sliceSize int64) FileResult {
	return FileResult{File: f, Status: Missing, Bad: allSlices(f, sliceSize), BadSlices: f.SliceCount(sliceSize)}
}

func allSlices(f *File, sliceSize int64) []int {
	bad := make([]int, f.SliceCount(sliceSize))
	for i := range bad {
		bad[i] = i
	}

	return bad
}

func describe(fsys fs.FS, p string) (candidate, error) {
	fi, err := fs.Stat(fsys, p)
	if err != nil {
		return candidate{}, err
	}

	if !fi.Mode().IsRegular() {
		return candidate{}, fs.ErrNotExist
	}

	f, err := fsys.Open(p)
	if err != nil {
		return candidate{}, err
	}

	defer func() { _ = f.Close() }()

	h := md5.New() //nolint:gosec // see import
	if _, err := io.CopyN(h, f, hash16k); err != nil && !errors.Is(err, io.EOF) {
		return candidate{}, err
	}

	c := candidate{path: p, size: fi.Size()}
	copy(c.h16k[:], h.Sum(nil))

	return c, nil
}

// check compares the file at p with f slice by slice and as a whole. A
// matching size and first 16 KiB only find the candidate; every byte is
// hashed before a file counts as OK.
func check(ctx context.Context, fsys fs.FS, p string, f *File, sliceSize int64) (FileResult, error) {
	res := FileResult{File: f, Found: p, Status: OK}

	r, err := fsys.Open(p)
	if err != nil {
		return res, err
	}

	defer func() { _ = r.Close() }()

	whole := md5.New() //nolint:gosec // see import
	total := f.SliceCount(sliceSize)
	buf := make([]byte, sliceSize)
	var read int64

	for i := range total {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		n, err := io.ReadFull(r, buf)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return res, err
		}

		// Only the file's real length counts toward its MD5; slices are
		// hashed zero-padded to the slice size.
		want := min(sliceSize, f.Size-read)
		valid := min(int64(n), want)
		whole.Write(buf[:valid])
		clear(buf[valid:])
		read += want

		if f.Slices != nil && md5.Sum(buf) != f.Slices[i].MD5 { //nolint:gosec // see import
			res.Bad = append(res.Bad, i)
		}
	}

	// Without slice checksums any content mismatch means rebuilding it all.
	// With them, a mismatch here means a collision; rebuild it all too.
	if len(res.Bad) == 0 && !bytes.Equal(whole.Sum(nil), f.MD5[:]) {
		res.Bad = allSlices(f, sliceSize)
	}

	res.BadSlices = len(res.Bad)

	// A file with the right content and the wrong length (trailing bytes, or
	// a missing tail that was all zeros) only needs truncating.
	if res.BadSlices > 0 || !sameSize(fsys, p, f.Size) {
		res.Status = Damaged
	}

	return res, nil
}

func sameSize(fsys fs.FS, p string, size int64) bool {
	fi, err := fs.Stat(fsys, p)
	return err == nil && fi.Size() == size
}
