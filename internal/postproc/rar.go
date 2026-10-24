package postproc

import (
	"errors"
	"io"
	"path"

	"github.com/nwaples/rardecode/v2"
)

// rar extracts a RAR archive, single or multi-volume, and returns the
// volumes it read. Volumes are opened through the work directory's root.
func (x *extractor) rar(name string) ([]string, error) {
	rc, err := rardecode.OpenReader(name,
		rardecode.FileSystem(x.root.FS()),
		rardecode.MaxDictionarySize(maxDictBytes))
	if err != nil {
		return nil, rarError(err)
	}

	defer func() { _ = rc.Close() }()

	for {
		h, err := rc.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, rarError(err)
		}

		switch {
		case h.Encrypted:
			return nil, errEncrypted
		case h.LinkType != rardecode.LinkTypeNone:
			x.skip(h.Name, "link")
		case h.IsDir:
			err = x.mkdir(h.Name)
		case !h.Mode().IsRegular():
			x.skip(h.Name, "not a regular file")
		default:
			err = x.create(h.Name, rc)
		}

		if err != nil {
			return nil, rarError(err)
		}
	}

	dir := path.Dir(name)

	var volumes []string
	for _, v := range rc.Volumes() {
		volumes = append(volumes, path.Join(dir, path.Base(v)))
	}

	return volumes, nil
}

func rarError(err error) error {
	switch {
	case errors.Is(err, rardecode.ErrArchiveEncrypted),
		errors.Is(err, rardecode.ErrArchivedFileEncrypted),
		errors.Is(err, rardecode.ErrBadPassword):
		return errEncrypted
	case errors.Is(err, rardecode.ErrMultiVolume):
		return errors.Join(err, errMissingVolume)
	}

	return err
}
