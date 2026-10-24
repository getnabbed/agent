package par2

import (
	"math/rand/v2"
	"testing"
)

// slowMul is carry-less multiplication reduced by the PAR2 polynomial,
// written independently of the log tables.
func slowMul(a, b uint16) uint16 {
	var p uint32

	for i := range 16 {
		if b&(1<<i) != 0 {
			p ^= uint32(a) << i
		}
	}

	for i := 31; i >= 16; i-- {
		if p&(1<<i) != 0 {
			p ^= gfPoly << (i - 16)
		}
	}

	return uint16(p)
}

func TestFieldGeneratorCoversEveryElement(t *testing.T) {
	g := gf()
	seen := make([]bool, 1<<16)

	for i := range gfOrder {
		x := g.exp[i]
		if x == 0 || seen[x] {
			t.Fatalf("got exp[%d] = %d repeated or zero, want 2 to generate the field", i, x)
		}

		seen[x] = true
	}
}

func TestFieldMultiplication(t *testing.T) {
	g := gf()
	r := rand.New(rand.NewPCG(1, 2))

	var tab mulTable

	for range 2000 {
		a, b := uint16(r.Uint32()), uint16(r.Uint32())

		if got, want := g.mul(a, b), slowMul(a, b); got != want {
			t.Fatalf("got %d*%d = %d, want %d", a, b, got, want)
		}

		g.table(a, &tab)
		if got, want := tab.lo[b&0xff]^tab.hi[b>>8], slowMul(a, b); got != want {
			t.Fatalf("got table %d*%d = %d, want %d", a, b, got, want)
		}

		if a != 0 && g.mul(a, g.inv(a)) != 1 {
			t.Fatalf("got %d * inv(%d) = %d, want 1", a, a, g.mul(a, g.inv(a)))
		}
	}
}

func TestInputSliceConstants(t *testing.T) {
	g := gf()

	// The logarithms not divisible by 3, 5, 17 or 257, in order.
	want := []uint32{1, 2, 4, 7, 8, 11, 13, 14, 16, 19, 22, 23, 26, 28, 29, 31, 32, 37, 38, 41}
	for i, l := range want {
		if g.inputLogs[i] != l {
			t.Fatalf("got input log %d = %d, want %d", i, g.inputLogs[i], l)
		}
	}

	if last := g.inputLogs[maxInputSlices-1]; last >= gfOrder {
		t.Fatalf("got last input log %d, want below %d", last, gfOrder)
	}

	// c^0 is 1 and c^65535 wraps around to 1.
	if g.coefficient(5, 0) != 1 || g.coefficient(5, gfOrder) != 1 || g.coefficient(0, 1) != 2 {
		t.Fatal("got wrong coefficients for exponents 0, 65535 or slice 0")
	}
}

func BenchmarkMulAdd(b *testing.B) {
	g := gf()
	src := make([]uint16, 1<<19)
	dst := make([]uint16, len(src))

	for i := range src {
		src[i] = uint16(i * 7919)
	}

	var tab mulTable
	g.table(12345, &tab)

	b.SetBytes(int64(2 * len(src)))

	for b.Loop() {
		mulAdd(dst, src, &tab)
	}
}
