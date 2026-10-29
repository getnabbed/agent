package torrent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

const (
	filePerm os.FileMode = 0o644
	dirPerm  os.FileMode = 0o755
)

// fileStorage stores a torrent's pieces in plain files under dir, at
// relPath, with piece completion kept in pc.
//
// It replaces anacrolix's file storage, whose default I/O memory-maps each
// file it touches and only unmaps it on rename, never on close. Those
// mappings outlive the torrent. On Windows a mapped file cannot be renamed or
// deleted, so a download resumed in the same process could not promote its
// .part file, and a finished download's files stayed locked until the agent
// exited. Here every read, write and sync opens the file, does its I/O and
// closes it, so no handle outlives the call. Files keep their final names
// throughout; completion lives in pc, not in file names.
type fileStorage struct {
	dir string
	pc  storage.PieceCompletion
}

func newFileStorage(dir string, pc storage.PieceCompletion) *fileStorage {
	return &fileStorage{dir: dir, pc: pc}
}

// OpenTorrent implements storage.ClientImpl.
func (s *fileStorage) OpenTorrent(_ context.Context, info *metainfo.Info, infoHash metainfo.Hash) (storage.TorrentImpl, error) {
	t := &fileTorrent{infoHash: infoHash, pc: s.pc}

	var off int64

	for _, fi := range info.UpvertedFiles() {
		rel := relPath(info, &fi)
		if !filepath.IsLocal(rel) {
			return storage.TorrentImpl{}, fmt.Errorf("torrent contains unsafe path %q", rel)
		}

		f := torrentFile{path: filepath.Join(s.dir, rel), offset: off, length: fi.Length}
		if err := f.adoptPartFile(); err != nil {
			return storage.TorrentImpl{}, err
		}

		if f.length == 0 {
			// No piece covers an empty file, so nothing else would create it.
			if err := f.createEmpty(); err != nil {
				return storage.TorrentImpl{}, err
			}
		}

		t.files = append(t.files, f)
		off += fi.Length
	}

	return storage.TorrentImpl{Piece: t.piece, Close: func() error { return nil }}, nil
}

// fileTorrent is one torrent's files, laid end to end as the torrent's
// byte stream.
type fileTorrent struct {
	infoHash metainfo.Hash
	pc       storage.PieceCompletion
	files    []torrentFile
}

func (t *fileTorrent) piece(p metainfo.Piece) storage.PieceImpl {
	return &filePiece{t: t, p: p, ReaderAt: io.NewSectionReader(t, p.Offset(), p.Length())}
}

// fileAt returns the file holding torrent byte off. Empty files hold none.
func (t *fileTorrent) fileAt(off int64) (*torrentFile, bool) {
	// File ends never decrease, so the first file ending after off is the
	// one that holds it.
	i := sort.Search(len(t.files), func(i int) bool { return t.files[i].offset+t.files[i].length > off })
	if i == len(t.files) {
		return nil, false
	}

	return &t.files[i], true
}

// ReadAt reads torrent bytes. A missing or short file reads as io.EOF, as
// anacrolix's own storage does, so the piece is simply not there yet.
func (t *fileTorrent) ReadAt(b []byte, off int64) (int, error) {
	var n int

	for len(b) > 0 {
		f, ok := t.fileAt(off)
		if !ok {
			return n, io.EOF
		}

		chunk := b[:min(int64(len(b)), f.offset+f.length-off)]
		m, err := f.readAt(chunk, off-f.offset)
		n += m
		if m < len(chunk) {
			if err == nil {
				err = io.EOF
			}

			return n, err
		}

		b = b[m:]
		off += int64(m)
	}

	return n, nil
}

func (t *fileTorrent) writeAt(b []byte, off int64) (int, error) {
	var n int

	for len(b) > 0 {
		f, ok := t.fileAt(off)
		if !ok {
			return n, io.ErrShortWrite
		}

		chunk := b[:min(int64(len(b)), f.offset+f.length-off)]
		m, err := f.writeAt(chunk, off-f.offset)
		n += m
		if err != nil {
			return n, err
		}

		b = b[m:]
		off += int64(m)
	}

	return n, nil
}

// overlapping returns the non-empty files that hold any of the n bytes
// from off.
func (t *fileTorrent) overlapping(off, n int64) []torrentFile {
	var out []torrentFile

	for off < t.end() && n > 0 {
		f, ok := t.fileAt(off)
		if !ok {
			break
		}

		out = append(out, *f)
		step := f.offset + f.length - off
		off += step
		n -= step
	}

	return out
}

func (t *fileTorrent) end() int64 {
	if len(t.files) == 0 {
		return 0
	}

	last := t.files[len(t.files)-1]

	return last.offset + last.length
}

// filePiece is one piece of a fileTorrent.
type filePiece struct {
	t *fileTorrent
	p metainfo.Piece
	io.ReaderAt
}

func (p *filePiece) key() metainfo.PieceKey {
	return metainfo.PieceKey{InfoHash: p.t.infoHash, Index: p.p.Index()}
}

// WriteAt implements storage.PieceImpl.
func (p *filePiece) WriteAt(b []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(b)) > p.p.Length() {
		return 0, fmt.Errorf("write of %d bytes at %d is outside piece %d", len(b), off, p.p.Index())
	}

	return p.t.writeAt(b, p.p.Offset()+off)
}

// MarkComplete implements storage.PieceImpl. The piece's data is synced
// before it is recorded as complete, so a crash cannot leave a persistent
// completion record for data that never reached the disk.
func (p *filePiece) MarkComplete() error {
	for _, f := range p.t.overlapping(p.p.Offset(), p.p.Length()) {
		if err := f.sync(); err != nil {
			return fmt.Errorf("sync %s: %w", f.path, err)
		}
	}

	return p.t.pc.Set(p.key(), true)
}

// MarkNotComplete implements storage.PieceImpl.
func (p *filePiece) MarkNotComplete() error { return p.t.pc.Set(p.key(), false) }

// Completion implements storage.PieceImpl. A piece recorded as complete
// whose files are now missing or too short is reported incomplete, so it is
// downloaded again.
func (p *filePiece) Completion() storage.Completion {
	c, err := p.t.pc.Get(p.key())
	c.Err = errors.Join(c.Err, err)
	if c.Err != nil || !c.Ok || !c.Complete {
		return c
	}

	end := p.p.Offset() + p.p.Length()
	for _, f := range p.t.overlapping(p.p.Offset(), p.p.Length()) {
		st, err := os.Stat(f.path)
		if errors.Is(err, fs.ErrNotExist) {
			return storage.Completion{Ok: true}
		}
		if err != nil {
			return storage.Completion{Err: err}
		}

		if st.Size() < min(f.length, end-f.offset) {
			return storage.Completion{Ok: true}
		}
	}

	return c
}

// torrentFile is one file of a torrent and where it sits in the torrent's
// byte stream.
type torrentFile struct {
	path   string
	offset int64
	length int64
}

func (f *torrentFile) readAt(b []byte, off int64) (int, error) {
	fh, err := os.Open(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, io.EOF
	}
	if err != nil {
		return 0, err
	}

	defer func() { _ = fh.Close() }()

	return fh.ReadAt(b, off)
}

func (f *torrentFile) writeAt(b []byte, off int64) (int, error) {
	fh, err := f.open(os.O_WRONLY | os.O_CREATE)
	if err != nil {
		return 0, err
	}

	n, err := fh.WriteAt(b, off)
	if cerr := fh.Close(); err == nil {
		err = cerr
	}

	return n, err
}

func (f *torrentFile) sync() error {
	fh, err := os.OpenFile(f.path, os.O_WRONLY, filePerm)
	if err != nil {
		return err
	}

	if err := fh.Sync(); err != nil {
		_ = fh.Close()
		return err
	}

	return fh.Close()
}

// adoptPartFile renames a download's .part file, left by agents that used
// anacrolix's file storage, to the file's final name, so its pieces still
// count when the download resumes.
func (f *torrentFile) adoptPartFile() error {
	if _, err := os.Stat(f.path); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	err := os.Rename(f.path+".part", f.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("adopt %s.part: %w", f.path, err)
	}

	return nil
}

func (f *torrentFile) createEmpty() error {
	fh, err := f.open(os.O_WRONLY | os.O_CREATE)
	if err != nil {
		return fmt.Errorf("create empty file: %w", err)
	}

	return fh.Close()
}

// open opens the file, creating its directory if needed. A file made
// read-only, as anacrolix's file storage did with finished files, is made
// writable again.
func (f *torrentFile) open(flag int) (*os.File, error) {
	fh, err := os.OpenFile(f.path, flag, filePerm)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		err = os.MkdirAll(filepath.Dir(f.path), dirPerm)
	case errors.Is(err, fs.ErrPermission):
		err = os.Chmod(f.path, filePerm)
	default:
		return fh, err
	}

	if err != nil {
		return nil, err
	}

	return os.OpenFile(f.path, flag, filePerm)
}
