package torrent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func TestFileStorageSpansFiles(t *testing.T) {
	dir := t.TempDir()
	// 11 bytes in 4-byte pieces: piece 0 is in a, piece 1 spans a and d/b,
	// and the empty file sits between them.
	info := &metainfo.Info{
		Name:        "rel",
		PieceLength: 4,
		Pieces:      make([]byte, 3*metainfo.HashSize),
		Files: []metainfo.FileInfo{
			{Path: []string{"a"}, Length: 5},
			{Path: []string{"empty"}, Length: 0},
			{Path: []string{"d", "b"}, Length: 6},
		},
	}

	pc := storage.NewMapPieceCompletion()
	ti, err := newFileStorage(dir, pc).OpenTorrent(context.Background(), info, metainfo.Hash{1})
	if err != nil {
		t.Fatal(err)
	}

	if st, err := os.Stat(filepath.Join(dir, "rel", "empty")); err != nil || st.Size() != 0 {
		t.Fatalf("got empty file %v, %v, want it created", st, err)
	}

	data := []byte("hello world")
	for i := range info.NumPieces() {
		p := info.Piece(i)
		if _, err := ti.Piece(p).WriteAt(data[p.Offset():p.Offset()+p.Length()], 0); err != nil {
			t.Fatalf("write piece %d: %v", i, err)
		}
	}

	if got, _ := os.ReadFile(filepath.Join(dir, "rel", "a")); string(got) != "hello" {
		t.Fatalf("got a = %q, want %q", got, "hello")
	}

	if got, _ := os.ReadFile(filepath.Join(dir, "rel", "d", "b")); string(got) != " world" {
		t.Fatalf("got d/b = %q, want %q", got, " world")
	}

	piece1 := ti.Piece(info.Piece(1))
	got := make([]byte, 4)
	if _, err := piece1.ReadAt(got, 0); err != nil || !bytes.Equal(got, data[4:8]) {
		t.Fatalf("got piece 1 = %q, %v, want %q", got, err, data[4:8])
	}

	if _, err := piece1.WriteAt([]byte("12345"), 0); err == nil {
		t.Fatal("got a write past the piece's end accepted")
	}

	if err := piece1.MarkComplete(); err != nil {
		t.Fatal(err)
	}

	if c := piece1.Completion(); !c.Ok || !c.Complete || c.Err != nil {
		t.Fatalf("got completion %+v, want complete", c)
	}

	// Nothing is left open: on Windows an open or mapped file cannot be
	// renamed, and a piece whose file has gone is no longer complete.
	if err := os.Rename(filepath.Join(dir, "rel", "d"), filepath.Join(dir, "moved")); err != nil {
		t.Fatal(err)
	}

	if c := piece1.Completion(); !c.Ok || c.Complete {
		t.Fatalf("got completion %+v after its file moved, want incomplete", c)
	}

	if n, err := piece1.ReadAt(got, 0); n != 1 || err == nil {
		t.Fatalf("got read of %d bytes, %v from a piece whose file moved, want 1 byte and an error", n, err)
	}
}

func TestFileStorageAdoptsPartFiles(t *testing.T) {
	dir := t.TempDir()
	info := &metainfo.Info{Name: "rel", PieceLength: 4, Pieces: make([]byte, metainfo.HashSize), Length: 4}

	// A finished file from anacrolix's storage was left read-only.
	if err := os.WriteFile(filepath.Join(dir, "rel.part"), []byte("data"), 0o444); err != nil {
		t.Fatal(err)
	}

	ti, err := newFileStorage(dir, storage.NewMapPieceCompletion()).OpenTorrent(context.Background(), info, metainfo.Hash{1})
	if err != nil {
		t.Fatal(err)
	}

	got := make([]byte, 4)
	if _, err := ti.Piece(info.Piece(0)).ReadAt(got, 0); err != nil || string(got) != "data" {
		t.Fatalf("got %q, %v, want the .part file's data", got, err)
	}

	if _, err := ti.Piece(info.Piece(0)).WriteAt([]byte("DATA"), 0); err != nil {
		t.Fatalf("rewrite a read-only file: %v", err)
	}
}

func TestFileStorageRefusesUnsafePaths(t *testing.T) {
	info := &metainfo.Info{
		Name:        "rel",
		PieceLength: 4,
		Pieces:      make([]byte, metainfo.HashSize),
		Files:       []metainfo.FileInfo{{Path: []string{"..", "..", "escape"}, Length: 4}},
	}

	if _, err := newFileStorage(t.TempDir(), storage.NewMapPieceCompletion()).OpenTorrent(context.Background(), info, metainfo.Hash{1}); err == nil {
		t.Fatal("got an unsafe path accepted")
	}
}
