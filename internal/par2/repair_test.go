package par2

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

// fixture is a recovery set made by par2cmdline-turbo (testdata/README.md)
// and the data it protects, regenerated here.
type fixture struct {
	index   string
	volumes []string
	files   map[string][]byte
}

// random reproduces the fixtures' ChaCha8 data.
func random(seed byte, n int) []byte {
	var s [32]byte
	s[0] = seed

	b := make([]byte, n)
	_, _ = rand.NewChaCha8(s).Read(b)

	return b
}

// releaseSet: 16 KiB slices, 15 input slices, 3 recovery slices.
func releaseSet() fixture {
	return fixture{
		index:   "release.par2",
		volumes: []string{"release.vol0+2.par2", "release.vol2+1.par2"},
		files:   map[string][]byte{"movie.mkv": seq(40000), "movie.nfo": seq(300)},
	}
}

// smallSet: 4 KiB slices, 57 input slices, 12 recovery slices.
func smallSet() fixture {
	return fixture{
		index:   "s4096.par2",
		volumes: []string{"s4096.vol00+1.par2", "s4096.vol01+2.par2", "s4096.vol03+4.par2", "s4096.vol07+5.par2"},
		files:   map[string][]byte{"movie.mkv": seq(40000), "movie.nfo": seq(300)},
	}
}

// oddSet: 6000-byte slices, an odd-length file whose last slice is 4001
// bytes, and recovery exponents 1000-1007.
func oddSet() fixture {
	return fixture{
		index:   "odd.par2",
		volumes: []string{"odd.vol1000+4.par2", "odd.vol1004+4.par2"},
		files:   map[string][]byte{"bin.dat": random(1, 100001), "small.txt": random(2, 777)},
	}
}

// layout writes the fixture's data and par2 files into a new directory.
func (fx fixture) layout(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	for name, data := range fx.files {
		write(t, dir, name, data)
	}

	for _, name := range append([]string{fx.index}, fx.volumes...) {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}

		write(t, dir, name, raw)
	}

	return dir
}

func write(t *testing.T, dir, name string, data []byte) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// load parses the index and every volume in dir.
func (fx fixture) load(t *testing.T, dir string) *Set {
	t.Helper()

	p := NewParser()
	for _, name := range append([]string{fx.index}, fx.volumes...) {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}

		err = p.ReadFile(name, f)
		_ = f.Close()

		if err != nil {
			t.Fatal(err)
		}
	}

	s, err := p.Set()
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func (fx fixture) names() []string {
	var out []string
	for name := range fx.files {
		out = append(out, name)
	}

	return out
}

// damage overwrites part of slice i of name.
func damage(t *testing.T, dir, name string, size int64, slices ...int) {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}

	for _, i := range slices {
		off := int64(i)*size + 7
		end := min(off+100, int64(len(data)))
		for j := off; j < end; j++ {
			data[j] ^= 0x5a
		}
	}

	write(t, dir, name, data)
}

// repairDir verifies and repairs dir, returning the report.
func repairDir(t *testing.T, dir string, set *Set, candidates []string, o RepairOptions) (*Report, error) {
	t.Helper()

	rep, err := Verify(context.Background(), os.DirFS(dir), set, candidates, VerifyOptions{Target: byName})
	if err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = root.Close() }()

	if o.Target == nil {
		o.Target = byName
	}

	return rep, Repair(context.Background(), root, set, rep, o)
}

// intact fails unless every file in dir equals the fixture's original.
func (fx fixture) intact(t *testing.T, dir string, rename map[string]string) {
	t.Helper()

	for name, want := range fx.files {
		at := name
		if to, ok := rename[name]; ok {
			at = to
		}

		got, err := os.ReadFile(filepath.Join(dir, at))
		if err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(got, want) {
			t.Fatalf("got %s with %d bytes differing from the original (%d vs %d long)", at, diff(got, want), len(got), len(want))
		}
	}
}

func diff(a, b []byte) int {
	n := max(len(a), len(b))
	for i := range min(len(a), len(b)) {
		if a[i] == b[i] {
			n--
		}
	}

	return n
}

// encode computes recovery slice e from the data alone, as PAR 2.0 defines
// it: the sum over input slices i of c_i^e times slice i, in 16-bit
// little-endian words, each file's last slice zero-padded.
func encode(set *Set, data map[string][]byte, e uint32) []byte {
	g := gf()
	acc := make([]uint16, set.SliceSize/2)
	in := make([]uint16, len(acc))
	raw := make([]byte, set.SliceSize)

	var tab mulTable

	global := 0
	for _, f := range set.Files {
		d := data[f.Name]

		for i := range f.SliceCount(set.SliceSize) {
			clear(raw)
			copy(raw, d[int64(i)*set.SliceSize:])

			for w := range in {
				in[w] = binary.LittleEndian.Uint16(raw[2*w:])
			}

			g.table(g.coefficient(global, e), &tab)
			mulAdd(acc, in, &tab)
			global++
		}
	}

	out := make([]byte, set.SliceSize)
	for w, v := range acc {
		binary.LittleEndian.PutUint16(out[2*w:], v)
	}

	return out
}

// The encoder reproduces every recovery slice par2cmdline wrote, byte for
// byte. That pins the field, the input constants, the slice order and the
// word layout, which decoding shares.
func TestEncoderMatchesPar2cmdline(t *testing.T) {
	for _, fx := range []fixture{releaseSet(), smallSet(), oddSet()} {
		dir := fx.layout(t)
		set := fx.load(t, dir)

		want := map[string]int{"release.par2": 3, "s4096.par2": 12, "odd.par2": 8}[fx.index]
		if len(set.Recovery) != want {
			t.Fatalf("%s: got %d recovery slices, want %d", fx.index, len(set.Recovery), want)
		}

		for _, r := range set.Recovery {
			raw, err := os.ReadFile(filepath.Join(dir, r.Source))
			if err != nil {
				t.Fatal(err)
			}

			got := raw[r.Offset : r.Offset+set.SliceSize]
			if enc := encode(set, fx.files, r.Exponent); !bytes.Equal(enc, got) {
				t.Fatalf("%s: got recovery slice %d differing in %d bytes from par2cmdline's", fx.index, r.Exponent, diff(enc, got))
			}
		}
	}
}

func TestRecoverySlicesAreLocated(t *testing.T) {
	fx := oddSet()
	set := fx.load(t, fx.layout(t))

	for i, r := range set.Recovery {
		if r.Exponent != uint32(1000+i) || r.Source != fx.volumes[i/4] || r.Offset <= 0 {
			t.Fatalf("got recovery slice %d = %+v, want exponent %d in %s", i, r, 1000+i, fx.volumes[i/4])
		}
	}

	// Read without a name, recovery slices are counted but not located.
	p := NewParser()
	raw, _ := os.ReadFile(filepath.Join("testdata", fx.volumes[0]))
	if err := p.Read(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}

	raw, _ = os.ReadFile(filepath.Join("testdata", fx.index))
	if err := p.Read(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}

	s, err := p.Set()
	if err != nil || len(s.RecoveryExponents) != 4 || len(s.Recovery) != 0 {
		t.Fatalf("got %v, %d exponents and %d located slices; want 4 and 0", err, len(s.RecoveryExponents), len(s.Recovery))
	}
}

// repairCase damages a fixture, repairs it with default and with minimal
// memory (many passes per slice), and expects the original files back.
func repairCase(t *testing.T, fx fixture, needed int, hurt func(t *testing.T, dir string, set *Set)) {
	t.Helper()

	for _, memory := range []int64{0, tinyMemory} {
		dir := fx.layout(t)
		set := fx.load(t, dir)
		hurt(t, dir, set)

		rep, err := repairDir(t, dir, set, fx.names(), RepairOptions{Memory: memory})
		if err != nil {
			t.Fatalf("memory=%d: got %v, want a repair", memory, err)
		}

		if rep.Needed != needed {
			t.Fatalf("got Needed = %d, want %d", rep.Needed, needed)
		}

		fx.intact(t, dir, nil)

		// The repaired set now verifies.
		if again, _ := repairDir(t, dir, set, fx.names(), RepairOptions{}); !again.Complete() {
			t.Fatalf("got %+v after repair, want every file OK", again.Files)
		}
	}
}

func TestRepairOneSlice(t *testing.T) {
	repairCase(t, releaseSet(), 1, func(t *testing.T, dir string, s *Set) {
		damage(t, dir, "movie.mkv", s.SliceSize, 2)
	})
}

func TestRepairSlicesAcrossTwoFiles(t *testing.T) {
	repairCase(t, smallSet(), 6, func(t *testing.T, dir string, s *Set) {
		damage(t, dir, "movie.mkv", s.SliceSize, 0, 9, 10, 31, 55)
		damage(t, dir, "movie.nfo", s.SliceSize, 0)
	})
}

func TestRepairMissingFile(t *testing.T) {
	repairCase(t, releaseSet(), 1, func(t *testing.T, dir string, _ *Set) {
		if err := os.Remove(filepath.Join(dir, "movie.nfo")); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRepairUsesEveryRecoverySlice(t *testing.T) {
	repairCase(t, releaseSet(), 3, func(t *testing.T, dir string, s *Set) {
		damage(t, dir, "movie.mkv", s.SliceSize, 0, 13)
		damage(t, dir, "movie.nfo", s.SliceSize, 0)
	})

	// Twelve lost slices among 57 take every block of the 4 KiB set.
	repairCase(t, smallSet(), 12, func(t *testing.T, dir string, s *Set) {
		damage(t, dir, "movie.mkv", s.SliceSize, 1, 2, 3, 5, 8, 13, 21, 34, 40, 50, 55)
		damage(t, dir, "movie.nfo", s.SliceSize, 0)
	})
}

func TestRepairOddSizes(t *testing.T) {
	// bin.dat is 100001 bytes: 16 slices of 6000 and a last one of 4001.
	repairCase(t, oddSet(), 3, func(t *testing.T, dir string, s *Set) {
		damage(t, dir, "bin.dat", s.SliceSize, 0, 16)
		damage(t, dir, "small.txt", s.SliceSize, 0)
	})

	repairCase(t, oddSet(), 8, func(t *testing.T, dir string, s *Set) {
		damage(t, dir, "bin.dat", s.SliceSize, 3, 4, 5, 6, 7, 15, 16)

		if err := os.Remove(filepath.Join(dir, "small.txt")); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRepairTruncatesTrailingJunk(t *testing.T) {
	fx := releaseSet()
	dir := fx.layout(t)
	write(t, dir, "movie.nfo", append(seq(300), "junk"...))

	// No recovery slice is needed, so none need be read.
	set := fx.load(t, dir)
	set.Recovery = nil

	rep, err := repairDir(t, dir, set, fx.names(), RepairOptions{})
	if err != nil || rep.Needed != 0 || rep.Complete() {
		t.Fatalf("got %v with Needed = %d, complete %v; want a truncation needing no blocks", err, rep.Needed, rep.Complete())
	}

	fx.intact(t, dir, nil)
}

func TestRepairShortFile(t *testing.T) {
	repairCase(t, oddSet(), 8, func(t *testing.T, dir string, _ *Set) {
		// Cut mid-slice: slice 9 is partly there, 10-16 are gone.
		write(t, dir, "bin.dat", random(1, 100001)[:57000])
	})
}

func TestRepairObfuscatedName(t *testing.T) {
	for _, slice := range []int{0, 5} {
		fx := releaseSet()
		dir := fx.layout(t)
		set := fx.load(t, dir)

		// Slice 0 holds the first 16 KiB, so the file is only found by size.
		damage(t, dir, "movie.mkv", set.SliceSize, slice)

		if err := os.Rename(filepath.Join(dir, "movie.mkv"), filepath.Join(dir, "x7f3a")); err != nil {
			t.Fatal(err)
		}

		rep, err := repairDir(t, dir, set, []string{"x7f3a", "movie.nfo"}, RepairOptions{})
		if err != nil {
			t.Fatalf("slice %d: got %v, want a repair", slice, err)
		}

		if r := result(t, rep, "movie.mkv"); r.Found != "x7f3a" || r.Status != Damaged || r.BadSlices != 1 {
			t.Fatalf("slice %d: got %+v, want the obfuscated file found with 1 damaged slice", slice, r)
		}

		fx.intact(t, dir, map[string]string{"movie.mkv": "x7f3a"})
	}
}

func TestRepairNeedsMoreThanExists(t *testing.T) {
	fx := releaseSet()
	dir := fx.layout(t)
	set := fx.load(t, dir)
	damage(t, dir, "movie.mkv", set.SliceSize, 0, 1, 2, 3)

	_, err := repairDir(t, dir, set, fx.names(), RepairOptions{})
	if !errors.Is(err, ErrNotEnoughRecovery) {
		t.Fatalf("got %v, want ErrNotEnoughRecovery", err)
	}

	// Nothing was written.
	if got, _ := os.ReadFile(filepath.Join(dir, "movie.nfo")); !bytes.Equal(got, seq(300)) {
		t.Fatal("got movie.nfo changed by a failed repair")
	}
}

func TestRepairCancelledAndRerun(t *testing.T) {
	fx := oddSet()
	dir := fx.layout(t)
	set := fx.load(t, dir)
	damage(t, dir, "bin.dat", set.SliceSize, 2, 9)

	// Also damage the part of slice 2 that the second pass rebuilds.
	data, _ := os.ReadFile(filepath.Join(dir, "bin.dat"))
	clear(data[2*6000+5000 : 2*6000+5100])
	write(t, dir, "bin.dat", data)

	if err := os.Remove(filepath.Join(dir, "small.txt")); err != nil {
		t.Fatal(err)
	}

	rep, err := Verify(context.Background(), os.DirFS(dir), set, fx.names(), VerifyOptions{Target: byName})
	if err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = root.Close() }()

	// Minimal memory makes two passes per slice (4096 + 1904 bytes);
	// cancel after the first, as a crash would stop it.
	ctx, cancel := context.WithCancel(context.Background())
	passes := 0

	err = Repair(ctx, root, set, rep, RepairOptions{Target: byName, Memory: tinyMemory, Progress: func(_, total int) {
		passes = total
		cancel()
	}})
	if !errors.Is(err, context.Canceled) || passes != 2 {
		t.Fatalf("got %v after %d passes, want context.Canceled with 2 passes planned", err, passes)
	}

	// Half-written: small.txt was created, and bin.dat's two slices have
	// only their first 4096 bytes rebuilt.
	if _, err := os.Stat(filepath.Join(dir, "small.txt")); err != nil {
		t.Fatalf("got %v, want small.txt created", err)
	}

	got, _ := os.ReadFile(filepath.Join(dir, "bin.dat"))
	want := fx.files["bin.dat"]
	if !bytes.Equal(got[2*6000:2*6000+4096], want[2*6000:2*6000+4096]) || bytes.Equal(got, want) {
		t.Fatal("got bin.dat not half repaired after one pass")
	}

	// Verify again and repair: the partial files are found and finished.
	if _, err := repairDir(t, dir, set, fx.names(), RepairOptions{}); err != nil {
		t.Fatalf("got %v on the rerun, want a repair", err)
	}

	fx.intact(t, dir, nil)
}

func TestRepairCancelledBeforeStart(t *testing.T) {
	fx := releaseSet()
	dir := fx.layout(t)
	set := fx.load(t, dir)
	damage(t, dir, "movie.mkv", set.SliceSize, 3)

	rep := verify(t, dir, set, false, fx.names()...)

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = root.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := Repair(ctx, root, set, rep, RepairOptions{Target: byName}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestRepairRejectsDependentRecoverySlices(t *testing.T) {
	fx := releaseSet()
	dir := fx.layout(t)
	set := fx.load(t, dir)
	damage(t, dir, "movie.mkv", set.SliceSize, 4, 5)

	// Exponents 1 and 65536 give the same row, since c^65535 = 1.
	r := set.Recovery[1]
	twin := r
	twin.Exponent += gfOrder
	set.Recovery = []RecoverySlice{r, twin}

	if _, err := repairDir(t, dir, set, fx.names(), RepairOptions{}); !errors.Is(err, ErrSingular) {
		t.Fatalf("got %v, want ErrSingular", err)
	}

	// With a third, independent slice the twin is skipped.
	set = fx.load(t, dir)
	set.Recovery = []RecoverySlice{set.Recovery[1], twin, set.Recovery[2]}

	if _, err := repairDir(t, dir, set, fx.names(), RepairOptions{}); err != nil {
		t.Fatalf("got %v, want the dependent slice passed over", err)
	}

	fx.intact(t, dir, nil)
}

func TestRepairDetectsWrongRecoveryData(t *testing.T) {
	fx := releaseSet()
	dir := fx.layout(t)
	set := fx.load(t, dir)
	damage(t, dir, "movie.mkv", set.SliceSize, 6)

	// A slice labelled with the wrong exponent passes its packet MD5 but
	// rebuilds garbage; the file MD5 catches it.
	set.Recovery = []RecoverySlice{set.Recovery[0]}
	set.Recovery[0].Exponent = 77

	if _, err := repairDir(t, dir, set, fx.names(), RepairOptions{}); !errors.Is(err, ErrMismatch) {
		t.Fatalf("got %v, want ErrMismatch", err)
	}
}

func TestRepairBoundsMemory(t *testing.T) {
	fx := smallSet()
	dir := fx.layout(t)
	set := fx.load(t, dir)
	damage(t, dir, "movie.mkv", set.SliceSize, 1, 2, 3)

	// Three rows of three columns need 18 bytes of matrix.
	if _, err := repairDir(t, dir, set, fx.names(), RepairOptions{Memory: 16}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

func TestRepairFailsOnVolumeCutAfterParsing(t *testing.T) {
	fx := releaseSet()
	dir := fx.layout(t)
	set := fx.load(t, dir)
	damage(t, dir, "movie.mkv", set.SliceSize, 1)

	if err := os.Truncate(filepath.Join(dir, set.Recovery[0].Source), 1000); err != nil {
		t.Fatal(err)
	}

	_, err := repairDir(t, dir, set, fx.names(), RepairOptions{})
	if err == nil || errors.Is(err, ErrMismatch) {
		t.Fatalf("got %v, want a read error", err)
	}
}

func TestRepairNeverReplacesAFileCreatedSinceVerify(t *testing.T) {
	fx := releaseSet()
	dir := fx.layout(t)
	set := fx.load(t, dir)

	if err := os.Remove(filepath.Join(dir, "movie.nfo")); err != nil {
		t.Fatal(err)
	}

	rep := verify(t, dir, set, false, fx.names()...)
	write(t, dir, "movie.nfo", []byte("mine"))

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = root.Close() }()

	if err := Repair(context.Background(), root, set, rep, RepairOptions{Target: byName}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("got %v, want os.ErrExist", err)
	}

	if got, _ := os.ReadFile(filepath.Join(dir, "movie.nfo")); string(got) != "mine" {
		t.Fatalf("got movie.nfo = %q, want it untouched", got)
	}
}

func TestRepairCannotEscapeRoot(t *testing.T) {
	fx := releaseSet()
	dir := fx.layout(t)
	set := fx.load(t, dir)

	if err := os.Remove(filepath.Join(dir, "movie.nfo")); err != nil {
		t.Fatal(err)
	}

	escape := func(*File) string { return "../escaped.nfo" }

	if _, err := repairDir(t, dir, set, fx.names(), RepairOptions{Target: escape}); err == nil {
		t.Fatal("got a repair writing outside the root")
	}

	if _, err := os.Stat(filepath.Join(dir, "..", "escaped.nfo")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v, want no file outside the root", err)
	}
}

// tinyMemory forces the smallest chunk, so each slice takes several passes.
const tinyMemory = 4096
