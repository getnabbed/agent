package par2

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // PAR2 is defined in terms of MD5.
	"encoding/binary"
	"errors"
	"math/bits"
	"os"
	"path/filepath"
	"testing"
)

// packet frames body as a PAR2 packet of type typ in set.
func packet(set ID, typ [16]byte, body []byte) []byte {
	h := md5.New() //nolint:gosec // see import
	h.Write(set[:])
	h.Write(typ[:])
	h.Write(body)

	var b bytes.Buffer
	b.Write(magic)
	_ = binary.Write(&b, binary.LittleEndian, uint64(headerSize+len(body)))
	b.Write(h.Sum(nil))
	b.Write(set[:])
	b.Write(typ[:])
	b.Write(body)

	return b.Bytes()
}

// hostileIndex builds an index claiming one file of size bytes in slices of
// sliceSize, with valid ids and checksums throughout.
func hostileIndex(sliceSize, size uint64) []byte {
	name := []byte("huge.bin")

	var sz [8]byte
	binary.LittleEndian.PutUint64(sz[:], size)

	var h16k [16]byte

	h := md5.New() //nolint:gosec // see import
	h.Write(h16k[:])
	h.Write(sz[:])
	h.Write(name)

	var fid ID
	copy(fid[:], h.Sum(nil))

	desc := slicesConcat(fid[:], make([]byte, 16), h16k[:], sz[:], name)

	main := binary.LittleEndian.AppendUint64(nil, sliceSize)
	main = binary.LittleEndian.AppendUint32(main, 1)
	main = append(main, fid[:]...)

	set := ID(md5.Sum(main)) //nolint:gosec // see import

	return append(packet(set, typeMain, main), packet(set, typeFileDesc, desc)...)
}

func slicesConcat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}

	return out
}

func TestSetRejectsTooManySlices(t *testing.T) {
	for _, c := range []struct{ slice, size uint64 }{
		{4, 1 << 40},                  // 2^38 slices
		{4, 4 * (maxInputSlices + 1)}, // one too many
	} {
		p := NewParser()
		if err := p.Read(bytes.NewReader(hostileIndex(c.slice, c.size))); err != nil {
			t.Fatal(err)
		}

		if _, err := p.Set(); !errors.Is(err, ErrTooManySlices) {
			t.Fatalf("slice %d, size %d: got %v, want ErrTooManySlices", c.slice, c.size, err)
		}
	}

	// Exactly the limit is a valid set.
	p := NewParser()
	if err := p.Read(bytes.NewReader(hostileIndex(4, 4*maxInputSlices))); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Set(); err != nil {
		t.Fatalf("got %v for %d slices, want a set", err, maxInputSlices)
	}
}

func TestHostileRecoveryPackets(t *testing.T) {
	fx := releaseSet()
	dir := fx.layout(t)
	set := fx.load(t, dir)
	damage(t, dir, "movie.mkv", set.SliceSize, 3)

	body := func(exp uint32, n int64) []byte {
		b := binary.LittleEndian.AppendUint32(nil, exp)
		return append(b, bytes.Repeat([]byte{0xa5}, int(n))...)
	}

	// The largest exponent, and a slice of the wrong length. Only the first
	// is usable; its data is garbage, which the file MD5 must catch.
	vol := slicesConcat(
		packet(set.ID, typeRecv, body(^uint32(0), set.SliceSize)),
		packet(set.ID, typeRecv, body(5, set.SliceSize-4)),
	)
	write(t, dir, "evil.vol0+2.par2", vol)

	p := NewParser()
	for _, name := range []string{fx.index, "evil.vol0+2.par2"} {
		raw, _ := os.ReadFile(filepath.Join(dir, name))
		if err := p.ReadFile(name, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
	}

	evil, err := p.Set()
	if err != nil {
		t.Fatal(err)
	}

	if len(evil.Recovery) != 1 || evil.Recovery[0].Exponent != ^uint32(0) {
		t.Fatalf("got recovery %+v, want only the full-length slice", evil.Recovery)
	}

	if _, err := repairDir(t, dir, evil, fx.names(), RepairOptions{}); !errors.Is(err, ErrMismatch) {
		t.Fatalf("got %v, want ErrMismatch", err)
	}

	// Two lost slices and one usable recovery slice.
	damage(t, dir, "movie.mkv", set.SliceSize, 4)

	if _, err := repairDir(t, dir, evil, fx.names(), RepairOptions{}); !errors.Is(err, ErrNotEnoughRecovery) {
		t.Fatalf("got %v, want ErrNotEnoughRecovery", err)
	}
}

func TestReportOutOfRangeSlicesAreIgnored(t *testing.T) {
	fx := releaseSet()
	dir := fx.layout(t)
	set := fx.load(t, dir)
	damage(t, dir, "movie.mkv", set.SliceSize, 1)

	rep := verify(t, dir, set, fx.names()...)
	for i := range rep.Files {
		rep.Files[i].Bad = append(rep.Files[i].Bad, -1, 1<<30)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = root.Close() }()

	if err := Repair(context.Background(), root, set, rep, RepairOptions{Target: byName}); err != nil {
		t.Fatalf("got %v, want a repair", err)
	}

	fx.intact(t, dir, nil)
}

func FuzzParser(f *testing.F) {
	for _, name := range []string{"release.par2", "release.vol2+1.par2", "s4096.par2", "odd.par2"} {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			f.Fatal(err)
		}

		f.Add(raw)
	}

	f.Add(hostileIndex(4, 1<<40))

	f.Fuzz(func(t *testing.T, raw []byte) {
		p := NewParser()
		if err := p.ReadFile("in.par2", bytes.NewReader(raw)); err != nil {
			t.Fatalf("got %v from an in-memory reader", err)
		}

		s, err := p.Set()
		if err != nil {
			return
		}

		total := 0
		for _, f := range s.Files {
			total += f.SliceCount(s.SliceSize)
		}

		if total > maxInputSlices {
			t.Fatalf("got a set with %d input slices", total)
		}

		for _, r := range s.Recovery {
			if r.Offset < 0 || r.Offset+s.SliceSize > int64(len(raw)) {
				t.Fatalf("got recovery slice at %d+%d in %d bytes", r.Offset, s.SliceSize, len(raw))
			}
		}
	})
}

// FuzzRepair damages any combination of the release's 15 slices and
// corrupts one byte of a volume. Repair must either restore the files
// exactly or fail with a classified error, and never panic.
func FuzzRepair(f *testing.F) {
	f.Add(uint16(1<<2), uint32(0), byte(0))
	f.Add(uint16(1<<0|1<<13|1<<14), uint32(0), byte(0))
	f.Add(uint16(0x7fff), uint32(0), byte(0))
	f.Add(uint16(1<<5|1<<6), uint32(20000), byte(1))
	f.Add(uint16(1<<5), uint32(70), byte(0xff))

	f.Fuzz(func(t *testing.T, mask uint16, at uint32, flip byte) {
		mask &= 1<<15 - 1
		fx := releaseSet()
		dir := fx.layout(t)

		if flip != 0 {
			vol := filepath.Join(dir, fx.volumes[0])
			raw, _ := os.ReadFile(vol)
			raw[int(at)%len(raw)] ^= flip
			write(t, dir, fx.volumes[0], raw)
		}

		set := fx.load(t, dir)

		for i := range 15 {
			if mask&(1<<i) == 0 {
				continue
			}

			if i < 14 {
				damage(t, dir, "movie.mkv", set.SliceSize, i)
			} else {
				damage(t, dir, "movie.nfo", set.SliceSize, 0)
			}
		}

		_, err := repairDir(t, dir, set, fx.names(), RepairOptions{Memory: tinyMemory})

		lost := bits.OnesCount16(mask)
		switch {
		case err == nil:
			fx.intact(t, dir, nil)
		case lost > len(set.Recovery) && errors.Is(err, ErrNotEnoughRecovery):
		case errors.Is(err, ErrSingular):
		default:
			t.Fatalf("got %v for %d lost slices and %d recovery slices", err, lost, len(set.Recovery))
		}
	})
}
