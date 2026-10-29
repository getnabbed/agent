package postproc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/bodgit/sevenzip"
	"github.com/stangelandcl/ppmd"
	"github.com/ulikunitz/xz/lzma"
)

var (
	errDictTooLarge     = fmt.Errorf("decoder memory exceeds %d MiB", maxDictBytes>>20)
	errBadProperties    = errors.New("invalid coder properties")
	errUnsupportedCoder = errors.New("compression method not supported")
	errMissingVolume    = errors.New("volume missing")
)

// The 7z library allocates whatever dictionary an archive declares, up to
// 4 GiB, before decoding a byte. These replacements for its LZMA, LZMA2 and
// PPMd decoders refuse anything above maxDictBytes first. Methods only
// third-party 7-Zip builds write are refused outright.
//
//nolint:gochecknoinits // the library has no per-reader registry
func init() {
	sevenzip.RegisterDecompressor([]byte{0x03, 0x01, 0x01}, newLZMA)
	sevenzip.RegisterDecompressor([]byte{0x21}, newLZMA2)
	sevenzip.RegisterDecompressor([]byte{0x03, 0x04, 0x01}, newPPMd)

	for _, m := range [][]byte{{0x04, 0xf7, 0x11, 0x01}, {0x04, 0xf7, 0x11, 0x02}, {0x04, 0xf7, 0x11, 0x04}} {
		sevenzip.RegisterDecompressor(m, unsupportedCoder)
	}
}

func unsupportedCoder([]byte, uint64, []io.ReadCloser) (io.ReadCloser, error) {
	return nil, errUnsupportedCoder
}

// coder closes the packed stream along with the decoder.
type coder struct {
	io.Reader
	c io.Closer
}

func (c *coder) Close() error { return c.c.Close() }

func newLZMA(p []byte, size uint64, in []io.ReadCloser) (io.ReadCloser, error) {
	if len(in) != 1 || len(p) != 5 {
		return nil, errBadProperties
	}

	if binary.LittleEndian.Uint32(p[1:]) > maxDictBytes {
		return nil, errDictTooLarge
	}

	// Classic LZMA streams start with the properties and size, which 7z
	// stores in its own headers instead.
	h := bytes.NewBuffer(bytes.Clone(p))
	_ = binary.Write(h, binary.LittleEndian, size)

	r, err := lzma.ReaderConfig{DictCap: maxDictBytes}.NewReader(io.MultiReader(h, in[0]))
	if err != nil {
		return nil, fmt.Errorf("lzma: %w", err)
	}

	return &coder{Reader: r, c: in[0]}, nil
}

func newLZMA2(p []byte, _ uint64, in []io.ReadCloser) (io.ReadCloser, error) {
	if len(in) != 1 || len(p) != 1 || p[0] > 40 {
		return nil, errBadProperties
	}

	dict := int64(2|(p[0]&1)) << (p[0]/2 + 11) // as in Lzma2Dec.c
	if dict > maxDictBytes {
		return nil, errDictTooLarge
	}

	cfg := lzma.Reader2Config{DictCap: int(dict)}
	if err := cfg.Verify(); err != nil {
		return nil, fmt.Errorf("lzma2: %w", err)
	}

	r, err := cfg.NewReader2(in[0])
	if err != nil {
		return nil, fmt.Errorf("lzma2: %w", err)
	}

	return &coder{Reader: r, c: in[0]}, nil
}

func newPPMd(p []byte, size uint64, in []io.ReadCloser) (io.ReadCloser, error) {
	if len(in) != 1 || len(p) != 5 {
		return nil, errBadProperties
	}

	order, mem := int(p[0]), binary.LittleEndian.Uint32(p[1:])
	if order < 2 || order > 64 || mem > maxDictBytes || size > 1<<62 {
		return nil, errDictTooLarge
	}

	r, err := ppmd.NewH7zReader(in[0], order, int(mem), int(size)) //nolint:gosec // bounded above
	if err != nil {
		return nil, fmt.Errorf("ppmd: %w", err)
	}

	return &coder{Reader: &r, c: in[0]}, nil
}

// sevenZip extracts a 7z archive. A split archive (name.7z.001, .002, …) is
// read as one stream across its volumes, found by number whatever their
// zero padding.
func (x *extractor) sevenZip(name string) ([]string, error) {
	volumes := []string{name}

	if re7zSplit.MatchString(path.Base(name)) {
		set, err := volumeSet(x.files, re7zSplit, name)
		if err != nil {
			return nil, err
		}

		if volumes, err = inOrder(set, 1); err != nil {
			return nil, err
		}
	}

	ra, size, closeAll, err := x.join(volumes)
	if err != nil {
		return nil, err
	}

	defer closeAll()

	r, err := sevenzip.NewReader(ra, size)
	if err != nil {
		return nil, sevenZipError(err)
	}

	if len(r.File) > x.entries {
		return nil, errEntries
	}

	for _, f := range r.File {
		mode := f.Mode()

		switch {
		case mode.IsDir():
			err = x.mkdir(f.Name)
		case !mode.IsRegular():
			x.skip(f.Name, "not a regular file")
		default:
			err = x.open7z(f)
		}

		if err != nil {
			return nil, sevenZipError(err)
		}
	}

	return volumes, nil
}

func (x *extractor) open7z(f *sevenzip.File) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}

	defer func() { _ = rc.Close() }()

	return x.create(f.Name, rc)
}

func sevenZipError(err error) error {
	// The library returns *ReadError, though ReadError has value methods.
	var rp *sevenzip.ReadError
	if errors.As(err, &rp) && rp.Encrypted {
		return errEncrypted
	}

	var rv sevenzip.ReadError
	if errors.As(err, &rv) && rv.Encrypted {
		return errEncrypted
	}

	return err
}

// join opens volumes through the root and presents them as one ReaderAt.
func (x *extractor) join(volumes []string) (io.ReaderAt, int64, func(), error) {
	j := &joined{}
	closeAll := func() {
		for _, f := range j.parts {
			_ = f.Close()
		}
	}

	for _, v := range volumes {
		f, err := x.root.Open(v)
		if err != nil {
			closeAll()
			return nil, 0, nil, err
		}

		fi, err := f.Stat()
		if err != nil {
			_ = f.Close()
			closeAll()

			return nil, 0, nil, err
		}

		j.parts = append(j.parts, f)
		j.starts = append(j.starts, j.size)
		j.size += fi.Size()
	}

	return j, j.size, closeAll, nil
}

// joined reads consecutive files as one.
type joined struct {
	parts  []*os.File
	starts []int64
	size   int64
}

func (j *joined) ReadAt(p []byte, off int64) (int, error) {
	n := 0

	for n < len(p) {
		pos := off + int64(n)
		if pos >= j.size {
			return n, io.EOF
		}

		i := len(j.starts) - 1
		for j.starts[i] > pos {
			i--
		}

		m, err := j.parts[i].ReadAt(p[n:], pos-j.starts[i])
		n += m

		if err != nil && !errors.Is(err, io.EOF) {
			return n, err
		}

		if m == 0 {
			return n, io.ErrUnexpectedEOF
		}
	}

	return n, nil
}
