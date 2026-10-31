package par2

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seq reproduces `seq 1 n`, the content the fixtures were made from.
func seq(n int) []byte {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}

	return []byte(b.String())
}

func parseFiles(t *testing.T, names ...string) *Set {
	t.Helper()

	p := NewParser()
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join("testdata", n))
		if err != nil {
			t.Fatal(err)
		}

		if err := p.Read(bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
	}

	s, err := p.Set()
	if err != nil {
		t.Fatal(err)
	}

	return s
}

// release writes the fixture's data files into a temp dir.
func release(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "movie.mkv"), seq(40000), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "movie.nfo"), seq(300), 0o600); err != nil {
		t.Fatal(err)
	}

	return dir
}

func byName(f *File) string { return f.Name }

func verify(t *testing.T, dir string, set *Set, candidates ...string) *Report {
	t.Helper()

	rep, err := Verify(context.Background(), os.DirFS(dir), set, candidates, VerifyOptions{Target: byName})
	if err != nil {
		t.Fatal(err)
	}

	return rep
}

func result(t *testing.T, rep *Report, name string) FileResult {
	t.Helper()

	for _, r := range rep.Files {
		if r.File.Name == name {
			return r
		}
	}

	t.Fatalf("got no result for %s", name)

	return FileResult{}
}

func TestParsesPar2cmdlineIndex(t *testing.T) {
	s := parseFiles(t, "release.par2")

	if s.SliceSize != 16384 || len(s.Files) != 2 || len(s.Unprotected) != 0 {
		t.Fatalf("got slice size %d, %d files, %d unprotected; want 16384, 2, 0", s.SliceSize, len(s.Files), len(s.Unprotected))
	}

	want := map[string]int64{"movie.mkv": 228894, "movie.nfo": 1092}
	for _, f := range s.Files {
		if want[f.Name] != f.Size || len(f.Slices) != f.SliceCount(s.SliceSize) {
			t.Fatalf("got %s size %d with %d slice checksums", f.Name, f.Size, len(f.Slices))
		}
	}

	if len(s.RecoveryExponents) != 0 || !strings.Contains(s.Creator, "par2cmdline") {
		t.Fatalf("got %d recovery slices and creator %q in the index", len(s.RecoveryExponents), s.Creator)
	}
}

func TestCountsRecoverySlicesInVolumes(t *testing.T) {
	s := parseFiles(t, "release.par2", "release.vol0+2.par2", "release.vol2+1.par2")

	if len(s.RecoveryExponents) != 3 {
		t.Fatalf("got %d recovery slices, want 3", len(s.RecoveryExponents))
	}
}

func TestVerifyIntact(t *testing.T) {
	dir := release(t)
	set := parseFiles(t, "release.par2")

	rep := verify(t, dir, set, "movie.mkv", "movie.nfo")
	if !rep.Complete() || result(t, rep, "movie.mkv").Status != OK || result(t, rep, "movie.nfo").Status != OK {
		t.Fatalf("got %+v, want both files OK", rep.Files)
	}
}

func TestVerifyFindsObfuscatedNames(t *testing.T) {
	dir := release(t)
	if err := os.Rename(filepath.Join(dir, "movie.mkv"), filepath.Join(dir, "a8f3k2b9.bin")); err != nil {
		t.Fatal(err)
	}

	rep := verify(t, dir, parseFiles(t, "release.par2"), "a8f3k2b9.bin", "movie.nfo")

	if r := result(t, rep, "movie.mkv"); r.Status != OK || r.Found != "a8f3k2b9.bin" {
		t.Fatalf("got %+v, want movie.mkv found intact under its obfuscated name", r)
	}
}

func TestVerifyCountsDamagedSlices(t *testing.T) {
	dir := release(t)
	data := seq(40000)
	clear(data[40000:41000]) // a lost article inside slice 2 (32768-49151)

	if err := os.WriteFile(filepath.Join(dir, "movie.mkv"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(dir, "movie.nfo")); err != nil {
		t.Fatal(err)
	}

	rep := verify(t, dir, parseFiles(t, "release.par2"), "movie.mkv")

	if r := result(t, rep, "movie.mkv"); r.Status != Damaged || r.BadSlices != 1 {
		t.Fatalf("got %+v, want 1 damaged slice", r)
	}

	if r := result(t, rep, "movie.nfo"); r.Status != Missing || r.BadSlices != 1 {
		t.Fatalf("got %+v, want missing with 1 slice", r)
	}

	if rep.Needed != 2 {
		t.Fatalf("got Needed = %d, want 2 (par2cmdline agrees: 2 blocks)", rep.Needed)
	}
}

func TestVerifyDetectsExtraTrailingBytes(t *testing.T) {
	dir := release(t)
	if err := os.WriteFile(filepath.Join(dir, "movie.nfo"), append(seq(300), "junk"...), 0o600); err != nil {
		t.Fatal(err)
	}

	rep := verify(t, dir, parseFiles(t, "release.par2"), "movie.mkv", "movie.nfo")

	if r := result(t, rep, "movie.nfo"); r.Status != Damaged {
		t.Fatalf("got %+v, want a file with trailing junk reported damaged", r)
	}
}

func TestParserSkipsGarbageAndDamagedPackets(t *testing.T) {
	index, err := os.ReadFile(filepath.Join("testdata", "release.par2"))
	if err != nil {
		t.Fatal(err)
	}

	vol, err := os.ReadFile(filepath.Join("testdata", "release.vol2+1.par2"))
	if err != nil {
		t.Fatal(err)
	}

	// Damage every packet in the index; the volume carries copies.
	broken := bytes.Clone(index)
	for i := 100; i < len(broken); i += 97 {
		broken[i] ^= 0xff
	}

	p := NewParser()
	if err := p.Read(bytes.NewReader(append([]byte("PAR2 garbage PAR2\x00PK"), broken...))); err != nil {
		t.Fatal(err)
	}

	if err := p.Read(bytes.NewReader(vol)); err != nil {
		t.Fatal(err)
	}

	s, err := p.Set()
	if err != nil {
		t.Fatal(err)
	}

	if len(s.Files) != 2 || len(s.RecoveryExponents) != 1 {
		t.Fatalf("got %d files and %d recovery slices, want 2 and 1", len(s.Files), len(s.RecoveryExponents))
	}
}

func TestNoRecoverySet(t *testing.T) {
	p := NewParser()
	if err := p.Read(strings.NewReader("not a par2 file")); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Set(); err == nil {
		t.Fatal("got a recovery set from garbage")
	}
}

func TestVolumeBlocks(t *testing.T) {
	cases := map[string]int{"release.vol0+2.par2": 2, "x.VOL07+08.PAR2": 8, "x.vol127+128.par2": 128}
	for name, want := range cases {
		if got, ok := VolumeBlocks(name); !ok || got != want {
			t.Fatalf("got VolumeBlocks(%q) = %d, %v; want %d", name, got, ok, want)
		}
	}

	if _, ok := VolumeBlocks("release.par2"); ok {
		t.Fatal("got blocks for an index file")
	}
}
