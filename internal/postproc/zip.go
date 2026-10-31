package postproc

import (
	"archive/zip"
	"errors"
)

// zip extracts a ZIP archive. Split ZIPs (.z01, …) are not supported and
// fail as corrupt.
func (x *extractor) zip(name string) ([]string, error) {
	f, err := x.root.Open(name)
	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	// Unsafe entry names are handled per entry by entryPath, so the reader's
	// own refusal is not needed.
	r, err := zip.NewReader(f, fi.Size())
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return nil, err
	}

	if len(r.File) > x.entries {
		return nil, errEntries
	}

	for _, e := range r.File {
		if err := x.count(); err != nil {
			return nil, err
		}

		mode := e.Mode()

		switch {
		case e.Flags&0x1 != 0:
			return nil, errEncrypted
		case mode.IsDir():
			err = x.mkdir(e.Name)
		case !mode.IsRegular():
			x.skip(e.Name, "not a regular file")
		default:
			err = x.openZip(e)
		}

		if err != nil {
			return nil, err
		}
	}

	return []string{name}, nil
}

func (x *extractor) openZip(e *zip.File) error {
	rc, err := e.Open()
	if err != nil {
		return err
	}

	defer func() { _ = rc.Close() }()

	return x.create(e.Name, rc)
}
