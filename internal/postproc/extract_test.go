package postproc

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getnabbed/agent/internal/engine"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// The RAR fixtures are written here rather than by WinRAR: stored (-m0)
// RAR5 is what most releases use, and its layout is short. Each block is a
// CRC32 of the header, then the header as vints. Compressed RAR decoding is
// rardecode's to test.

func vint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}

	return append(b, byte(v))
}

// rarBlock builds one RAR5 block header. dataSize < 0 means no data area.
func rarBlock(htype, flags uint64, fields, extra []byte, dataSize int) []byte {
	if len(extra) > 0 {
		flags |= 0x1
	}

	if dataSize >= 0 {
		flags |= 0x2
	}

	h := vint(nil, htype)
	h = vint(h, flags)

	if len(extra) > 0 {
		h = vint(h, uint64(len(extra)))
	}

	if dataSize >= 0 {
		h = vint(h, uint64(dataSize))
	}

	h = append(h, fields...)
	h = append(h, extra...)

	body := append(vint(nil, uint64(len(h))), h...)

	return append(binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(body)), body...)
}

type rarEntry struct {
	name    string
	data    []byte
	dir     bool
	symlink string
}

func rarFileHeader(e rarEntry, flags uint64, part []byte, crc uint32) []byte {
	var fields []byte

	fileFlags := uint64(0x4) // CRC32 present
	attrs := uint64(0o100644)
	if e.dir {
		fileFlags, attrs = 0x1, 0o40755
	}

	if e.symlink != "" {
		attrs = 0o120777
	}

	fields = vint(fields, fileFlags)
	fields = vint(fields, uint64(len(e.data)))
	fields = vint(fields, attrs)

	if fileFlags&0x4 != 0 {
		fields = binary.LittleEndian.AppendUint32(fields, crc)
	}

	fields = vint(fields, 0) // stored
	fields = vint(fields, 1) // unix
	fields = vint(fields, uint64(len(e.name)))
	fields = append(fields, e.name...)

	var extra []byte
	if e.symlink != "" {
		rec := vint(nil, 5) // redirection
		rec = vint(rec, 1)  // unix symlink
		rec = vint(rec, 0)  // flags
		rec = vint(rec, uint64(len(e.symlink)))
		rec = append(rec, e.symlink...)
		extra = append(vint(nil, uint64(len(rec))), rec...)
	}

	size := len(part)
	if e.dir {
		size = -1
	}

	return append(rarBlock(2, flags, fields, extra, size), part...)
}

// rar5 writes entries as a stored RAR5 archive. With parts > 1 it holds one
// file split evenly across that many volumes.
func rar5(parts int, entries ...rarEntry) [][]byte {
	sig := []byte("Rar!\x1a\x07\x01\x00")

	if parts <= 1 {
		vol := append([]byte(nil), sig...)
		vol = append(vol, rarBlock(1, 0, vint(nil, 0), nil, -1)...)

		for _, e := range entries {
			vol = append(vol, rarFileHeader(e, 0, e.data, crc32.ChecksumIEEE(e.data))...)
		}

		return [][]byte{append(vol, rarBlock(5, 0, vint(nil, 0), nil, -1)...)}
	}

	e := entries[0]
	step := (len(e.data) + parts - 1) / parts

	var vols [][]byte

	for i := range parts {
		arc := vint(nil, 0x1) // volume
		if i > 0 {
			arc = vint(vint(nil, 0x3), uint64(i))
		}

		vol := append([]byte(nil), sig...)
		vol = append(vol, rarBlock(1, 0, arc, nil, -1)...)

		part := e.data[min(i*step, len(e.data)):min((i+1)*step, len(e.data))]
		flags, crc := uint64(0), crc32.ChecksumIEEE(part)

		if i > 0 {
			flags |= 0x8
		}

		if i < parts-1 {
			flags |= 0x10
		} else {
			crc = crc32.ChecksumIEEE(e.data)
		}

		vol = append(vol, rarFileHeader(e, flags, part, crc)...)

		end := uint64(0)
		if i < parts-1 {
			end = 0x1
		}

		vols = append(vols, append(vol, rarBlock(5, 0, vint(nil, end), nil, -1)...))
	}

	return vols
}

// workDir writes files under data/ and returns the job and its engine
// result.
func workDir(t *testing.T, files map[string][]byte) (engine.Job, *engine.Result) {
	t.Helper()

	dir := t.TempDir()
	res := &engine.Result{}

	for name, data := range files {
		p := filepath.Join(dir, "data", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}

		res.Files = append(res.Files, engine.File{Path: "data/" + name, Size: int64(len(data))})
	}

	return engine.Job{ID: "d1", Dir: dir}, res
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()

	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}

	return b
}

func readOut(t *testing.T, j engine.Job, p string) []byte {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(j.Dir, filepath.FromSlash(p)))
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}

	return b
}

func exists(j engine.Job, p string) bool {
	_, err := os.Lstat(filepath.Join(j.Dir, filepath.FromSlash(p)))
	return err == nil
}

// wantExtractFailure checks for a permanent EXTRACT failure, which makes the
// cloud pick another release.
func wantExtractFailure(t *testing.T, err error, detail string) {
	t.Helper()

	var f *engine.Failure
	if !errors.As(err, &f) || f.Class != pb.FailureClass_FAILURE_CLASS_EXTRACT || f.Retryable || !strings.Contains(f.Detail, detail) {
		t.Fatalf("got %v, want a permanent EXTRACT failure mentioning %q", err, detail)
	}
}

func TestExtractsMultiVolumeRAR(t *testing.T) {
	movie := seq(40000)
	vols := rar5(3, rarEntry{name: "Movie/movie.mkv", data: movie})
	j, res := workDir(t, map[string][]byte{
		"rel.part1.rar": vols[0], "rel.part2.rar": vols[1], "rel.part3.rar": vols[2],
		"rel.nfo": []byte("about"),
	})
	j.Notes = &engine.Notes{}

	out, err := assemble(j, res)
	if err != nil {
		t.Fatal(err)
	}

	if got := paths(out); got != "data/rel.nfo,extracted/Movie/movie.mkv" {
		t.Fatalf("got files %s, want the nfo and the extracted movie", got)
	}

	if a := noted(j)["extract.done"].GetAttrs(); a["archive"] != "rel.part1.rar" || a["volumes"] != "3" || a["kind"] != "rar" {
		t.Fatalf("got extract.done attrs %v, want the 3-volume RAR", a)
	}

	if !bytes.Equal(readOut(t, j, "extracted/Movie/movie.mkv"), movie) {
		t.Fatal("got different movie content after extraction")
	}

	if exists(j, "data/rel.part1.rar") || exists(j, "data/rel.part3.rar") {
		t.Fatal("got archive volumes left after extraction")
	}

	// A rerun, as after a crash before the state was saved, starts from the
	// engine's result, whose volumes are gone.
	again, err := assemble(j, res)
	if err != nil || paths(again) != paths(out) {
		t.Fatalf("got %s, %v on a rerun, want the same files", paths(again), err)
	}
}

func TestRARSkipsLinksAndUnsafePaths(t *testing.T) {
	vols := rar5(1,
		rarEntry{name: "../../escape.txt", data: []byte("x")},
		rarEntry{name: "Sub", dir: true},
		rarEntry{name: "Sub/ok.srt", data: []byte("subs")},
		rarEntry{name: "link", symlink: "/etc/passwd"},
		rarEntry{name: "/abs/CON", data: []byte("y")},
	)
	j, res := workDir(t, map[string][]byte{"rel.rar": vols[0]})
	j.Notes = &engine.Notes{}

	out, err := assemble(j, res)
	if err != nil {
		t.Fatal(err)
	}

	if got := paths(out); got != "extracted/Sub/ok.srt,extracted/abs/file-3" {
		t.Fatalf("got files %s, want only the safe entries", got)
	}

	if n := noted(j)["extract.entries_skipped"]; n.GetSeverity() != pb.Severity_SEVERITY_WARNING || n.GetAttrs()["count"] != "2" {
		t.Fatalf("got %v, want a warning for the 2 skipped entries", n)
	}

	if exists(j, "../escape.txt") || exists(j, "escape.txt") || exists(j, "extracted/link") {
		t.Fatal("got an escaped file or a link created")
	}
}

func TestRARMissingVolumeFailsForGood(t *testing.T) {
	vols := rar5(3, rarEntry{name: "movie.mkv", data: seq(40000)})
	j, res := workDir(t, map[string][]byte{"rel.part1.rar": vols[0], "rel.part3.rar": vols[2]})

	_, err := assemble(j, res)
	wantExtractFailure(t, err, "rel.part1.rar")

	if !exists(j, "data/rel.part1.rar") {
		t.Fatal("got volumes deleted after a failed extraction")
	}
}

func TestRARBadChecksumFailsForGood(t *testing.T) {
	vols := rar5(1, rarEntry{name: "movie.mkv", data: seq(4000)})
	vols[0][len(vols[0])-100] ^= 0xff

	j, res := workDir(t, map[string][]byte{"rel.rar": vols[0]})

	_, err := assemble(j, res)
	wantExtractFailure(t, err, "corrupt")

	if exists(j, "extracted/movie.mkv") {
		t.Fatal("got a partial file left after a checksum failure")
	}
}

func TestExtracts7z(t *testing.T) {
	j, res := workDir(t, map[string][]byte{"rel.7z": fixture(t, "release.7z")})

	out, err := assemble(j, res)
	if err != nil {
		t.Fatal(err)
	}

	if got := paths(out); got != "extracted/Movie/movie.mkv,extracted/movie.nfo" {
		t.Fatalf("got files %s", got)
	}

	if !bytes.Equal(readOut(t, j, "extracted/Movie/movie.mkv"), seq(40000)) {
		t.Fatal("got different content from the 7z")
	}
}

func TestExtractsSplit7z(t *testing.T) {
	j, res := workDir(t, map[string][]byte{
		"rel.7z.001": fixture(t, "split.7z.001"),
		"rel.7z.002": fixture(t, "split.7z.002"),
		"rel.7z.003": fixture(t, "split.7z.003"),
	})

	out, err := assemble(j, res)
	if err != nil {
		t.Fatal(err)
	}

	if got := paths(out); got != "extracted/Movie/movie.mkv" {
		t.Fatalf("got files %s", got)
	}

	if !bytes.Equal(readOut(t, j, "extracted/Movie/movie.mkv"), seq(40000)) {
		t.Fatal("got different content from the split 7z")
	}

	if exists(j, "data/rel.7z.002") {
		t.Fatal("got a 7z volume left after extraction")
	}
}

func TestPasswordProtected7zFailsForGood(t *testing.T) {
	j, res := workDir(t, map[string][]byte{"rel.7z": fixture(t, "secret.7z")})

	_, err := assemble(j, res)
	wantExtractFailure(t, err, "password")
}

type zipEntry struct {
	name string
	data []byte
	mode os.FileMode
}

func zipOf(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()

	var buf bytes.Buffer

	w := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}

		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := f.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func TestZipKeepsEntriesInsideAndApart(t *testing.T) {
	j, res := workDir(t, map[string][]byte{"rel.zip": zipOf(t,
		zipEntry{name: "../../evil.sh", data: []byte("x")},
		zipEntry{name: `C:\Windows\a.txt`, data: []byte("a")},
		zipEntry{name: "A.txt", data: []byte("upper")},
		zipEntry{name: "a.txt", data: []byte("lower")},
		zipEntry{name: "lnk", data: []byte("/etc"), mode: os.ModeSymlink | 0o777},
	)})

	out, err := assemble(j, res)
	if err != nil {
		t.Fatal(err)
	}

	if got := paths(out); got != "extracted/A.txt,extracted/C_/Windows/a.txt,extracted/a (1).txt" {
		t.Fatalf("got files %s", got)
	}

	if string(readOut(t, j, "extracted/a (1).txt")) != "lower" {
		t.Fatal("got a case-only name collision overwriting the first file")
	}
}

func TestDecompressionBombFailsForGood(t *testing.T) {
	j, res := workDir(t, map[string][]byte{"rel.zip": zipOf(t, zipEntry{name: "zeros", data: make([]byte, 8<<20)})})

	_, err := (&Assembler{Log: quiet, Slack: 1 << 20}).Assemble(t.Context(), j, res)
	wantExtractFailure(t, err, "exceeds the limit")

	if exists(j, "extracted/zeros") {
		t.Fatal("got the partial bomb output left on disk")
	}
}

func TestTooManyEntriesFailsForGood(t *testing.T) {
	j, res := workDir(t, map[string][]byte{"rel.zip": zipOf(t,
		zipEntry{name: "1", data: nil}, zipEntry{name: "2", data: nil}, zipEntry{name: "3", data: nil})})

	_, err := (&Assembler{Log: quiet, MaxEntries: 2}).Assemble(t.Context(), j, res)
	wantExtractFailure(t, err, "too many entries")
}

func TestMislabelledArchiveIsLeftAlone(t *testing.T) {
	j, res := workDir(t, map[string][]byte{"notes.rar": []byte("just text")})

	out, err := assemble(j, res)
	if err != nil || paths(out) != "data/notes.rar" {
		t.Fatalf("got %s, %v; want the file passed through", paths(out), err)
	}
}

func TestDecoderMemoryIsCapped(t *testing.T) {
	in := []io.ReadCloser{io.NopCloser(bytes.NewReader(nil))}

	lzma := []byte{0x5d, 0, 0, 0, 0x80} // 2 GiB dictionary
	if _, err := newLZMA(lzma, 0, in); !errors.Is(err, errDictTooLarge) {
		t.Fatalf("got %v for a 2 GiB LZMA dictionary", err)
	}

	if _, err := newLZMA2([]byte{40}, 0, in); !errors.Is(err, errDictTooLarge) {
		t.Fatalf("got %v for a 4 GiB LZMA2 dictionary", err)
	}

	if _, err := newPPMd([]byte{6, 0, 0, 0, 0xf0}, 0, in); !errors.Is(err, errDictTooLarge) {
		t.Fatalf("got %v for 3.75 GiB of PPMd memory", err)
	}

	if _, err := newLZMA2([]byte{24}, 0, in); err != nil {
		t.Fatalf("got %v for a 64 MiB LZMA2 dictionary, want it allowed", err)
	}
}
