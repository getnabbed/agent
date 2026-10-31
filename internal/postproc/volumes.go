package postproc

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// maxVolume bounds a volume number. Real sets stay in the hundreds; a
// bound keeps a hostile NZB from naming one volume 999999999.
const maxVolume = 9999

// joiningSuffix marks a raw split being joined, so a rerun after a crash
// can tell a finished join from a partial one.
const joiningSuffix = ".joining"

var (
	errAmbiguousVolume = errors.New("two files claim the same volume number")
	errSplitSizes      = errors.New("split pieces differ in size")

	// reRawSplit matches a file cut into numbered pieces with no archive
	// format of its own (movie.mkv.001, release.rar.001). The piece's own
	// extension must start with a letter, so a name ending in a year, such
	// as The.Movie.2023, is never taken for a piece.
	reRawSplit = regexp.MustCompile(`(?i)\.[a-z][a-z0-9]{0,7}\.(\d{3})$`)
)

// volumeNumber parses base's volume number with re, whose first group is
// the number. key identifies the set: the name around the number, folded
// to lower case, so part09 and part010 of one release share it.
func volumeNumber(re *regexp.Regexp, base string) (n int, key string, ok bool) {
	m := re.FindStringSubmatchIndex(base)
	if m == nil {
		return 0, "", false
	}

	n, err := strconv.Atoi(base[m[2]:m[3]])
	if err != nil || n > maxVolume {
		return 0, "", false
	}

	return n, strings.ToLower(base[:m[2]] + "#" + base[m[3]:]), true
}

// volumeSet returns the files in first's directory that number like first
// under re, by volume number, whatever their zero padding. Two files with
// the same number make the set ambiguous; guessing which one the archive
// meant could splice in a file it never contained.
func volumeSet(files []string, re *regexp.Regexp, first string) (map[int]string, error) {
	_, key, ok := volumeNumber(re, path.Base(first))
	if !ok {
		return nil, fmt.Errorf("%s is not a numbered volume", path.Base(first))
	}

	dir := path.Dir(first)
	set := map[int]string{}

	for _, f := range files {
		if path.Dir(f) != dir {
			continue
		}

		n, k, ok := volumeNumber(re, path.Base(f))
		if !ok || k != key {
			continue
		}

		if prev, dup := set[n]; dup && prev != f {
			return nil, fmt.Errorf("%w: %s and %s", errAmbiguousVolume, path.Base(prev), path.Base(f))
		}

		set[n] = f
	}

	return set, nil
}

// inOrder lists a set's volumes from start up to its highest number. A gap
// is a missing volume, reported as fs.ErrNotExist like any other.
func inOrder(set map[int]string, start int) ([]string, error) {
	hi := start - 1
	for n := range set {
		hi = max(hi, n)
	}

	out := make([]string, 0, len(set))

	for n := start; n <= hi; n++ {
		f, ok := set[n]
		if !ok {
			return nil, fmt.Errorf("volume %d: %w", n, fs.ErrNotExist)
		}

		out = append(out, f)
	}

	return out, nil
}

// rarVolumes serves rardecode's volume requests by number. rardecode names
// each next volume by incrementing the previous name's number at the same
// width, so a set posted as part01…part09, part010…part0103 asks for
// part10.rar, which does not exist. A request for a numbered volume of the
// set gets the set's file with that number; anything else passes through.
// rardecode still checks every volume's header number against its place in
// the archive, so a file that only looks like the right volume is refused.
type rarVolumes struct {
	fsys fs.FS
	dir  string
	key  string
	set  map[int]string
	// opened lists the files actually opened, in order, which are the
	// volumes to delete after extraction.
	opened []string
}

func newRARVolumes(fsys fs.FS, files []string, first string) (*rarVolumes, error) {
	v := &rarVolumes{fsys: fsys, dir: path.Dir(first)}

	if _, key, ok := volumeNumber(reRarPart, path.Base(first)); ok {
		set, err := volumeSet(files, reRarPart, first)
		if err != nil {
			return nil, err
		}

		v.key, v.set = key, set
	}

	return v, nil
}

// Open implements fs.FS.
func (v *rarVolumes) Open(name string) (fs.File, error) {
	name = path.Clean(name)

	if v.set != nil && path.Dir(name) == v.dir {
		if n, key, ok := volumeNumber(reRarPart, path.Base(name)); ok && key == v.key {
			f, ok := v.set[n]
			if !ok {
				return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
			}

			name = f
		}
	}

	f, err := v.fsys.Open(name)
	if err == nil && !slices.Contains(v.opened, name) {
		v.opened = append(v.opened, name)
	}

	return f, err
}

// joinSplits joins each raw split among data (movie.mkv.001, .002, …) into
// the file it was cut from, next to its pieces. It returns data with the
// pieces replaced by the joined files, and the pieces to delete once the
// job is done. A joined archive is then extracted like any other.
//
// A set must hold its first piece (000 or 001) to count as a split; then
// every number up to the last must be present, and every piece but the
// last must be the same size, as every splitter writes them. Anything else
// fails the job rather than importing a damaged file.
func (x *extractor) joinSplits(data []string) (out, pieces []string, err error) {
	done := map[string]bool{}

	for _, f := range data {
		if done[f] || re7zSplit.MatchString(path.Base(f)) {
			continue
		}

		n, _, ok := volumeNumber(reRawSplit, path.Base(f))
		if !ok || n > 1 {
			continue
		}

		set, err := volumeSet(data, reRawSplit, f)
		if err != nil {
			return nil, nil, err
		}

		start := 1
		if _, ok := set[0]; ok {
			start = 0
		}

		if _, ok := set[start]; !ok || (start == 0 && f != set[0]) {
			continue
		}

		parts, err := inOrder(set, start)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path.Base(f), err)
		}

		for _, p := range parts {
			done[p] = true
		}

		joined := strings.TrimSuffix(f, path.Ext(f))
		if err := x.joinFile(joined, parts); err != nil {
			return nil, nil, fmt.Errorf("join %s: %w", path.Base(joined), err)
		}

		x.log.Info("joined split file", "file", joined, "pieces", len(parts))
		out = append(out, joined)
		pieces = append(pieces, parts...)
	}

	for _, f := range data {
		if !done[f] && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}

	return out, pieces, nil
}

// joinFile writes parts, in order, to name. It writes to a temporary file and
// renames it into place, so name either does not exist or is complete. A
// name that already exists without its temporary file is a join finished
// by an earlier run, as after a crash before clean-up, and is kept.
func (x *extractor) joinFile(name string, parts []string) error {
	tmp := name + joiningSuffix

	if err := x.root.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return diskError{err}
	}

	if _, err := x.root.Lstat(name); err == nil {
		return nil
	}

	var size int64

	for i, p := range parts {
		fi, err := x.root.Stat(p)
		if err != nil {
			return err
		}

		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path.Base(p))
		}

		if i == 0 {
			size = fi.Size()

			if size == 0 && len(parts) > 1 {
				return fmt.Errorf("%w: %s is empty", errSplitSizes, path.Base(p))
			}
		}

		if (i < len(parts)-1 && fi.Size() != size) || fi.Size() > size {
			return fmt.Errorf("%w: %s", errSplitSizes, path.Base(p))
		}
	}

	f, err := x.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return diskError{err}
	}

	err = x.copyParts(f, parts)
	if cerr := f.Close(); err == nil && cerr != nil {
		err = diskError{cerr}
	}

	if err != nil {
		_ = x.root.Remove(tmp)
		return err
	}

	if err := x.root.Rename(tmp, name); err != nil {
		_ = x.root.Remove(tmp)
		return diskError{err}
	}

	x.joined = append(x.joined, name)

	return nil
}

func (x *extractor) copyParts(w io.Writer, parts []string) error {
	bw := &budgetWriter{x: x, w: w}

	for _, p := range parts {
		r, err := x.root.Open(p)
		if err != nil {
			return err
		}

		_, err = io.Copy(bw, r)
		_ = r.Close()

		if err != nil {
			return err
		}
	}

	return nil
}
