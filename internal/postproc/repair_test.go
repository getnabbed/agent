package postproc

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // PAR2 is defined in terms of MD5.
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/getnabbed/agent/internal/engine"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

var releaseVolumes = []string{"release.vol0+2.par2", "release.vol2+1.par2"}

// fakeFetcher serves deferred files from the par2 testdata, or from data.
type fakeFetcher struct {
	data  map[string][]byte
	err   error
	asked [][]string
}

func (f *fakeFetcher) FetchFiles(_ context.Context, job engine.Job, names []string) (*engine.Result, error) {
	f.asked = append(f.asked, slices.Clone(names))
	if f.err != nil {
		return nil, f.err
	}

	res := &engine.Result{}

	for _, name := range names {
		raw, ok := f.data[name]
		if !ok {
			var err error
			if raw, err = os.ReadFile(filepath.Join("..", "par2", "testdata", name)); err != nil {
				return nil, err
			}
		}

		if err := os.WriteFile(filepath.Join(job.Dir, "data", name), raw, 0o600); err != nil {
			return nil, err
		}

		res.Files = append(res.Files, engine.File{Path: "data/" + name, Size: int64(len(raw))})
	}

	return res, nil
}

// hurt overwrites part of each 16 KiB slice listed.
func hurt(b []byte, slices ...int) []byte {
	b = bytes.Clone(b)
	for _, i := range slices {
		clear(b[i*16384+100 : min(i*16384+900, len(b))])
	}

	return b
}

func damagedJob(t *testing.T, files map[string][]byte) (engine.Job, *engine.Result) {
	t.Helper()

	j, res := job(t, files)
	res.MissingArticles = 1
	res.Deferred = slices.Clone(releaseVolumes)

	return j, res
}

func assembleWith(f *fakeFetcher, j engine.Job, res *engine.Result) (*engine.Result, error) {
	return (&Assembler{Log: quiet, Fetcher: f}).Assemble(context.Background(), j, res)
}

func wantRelease(t *testing.T, j engine.Job, out *engine.Result) {
	t.Helper()

	if got := paths(out); got != "data/movie.mkv,data/movie.nfo" {
		t.Fatalf("got files %s, want the repaired release with par2 files removed", got)
	}

	for name, want := range map[string][]byte{"movie.mkv": seq(40000), "movie.nfo": seq(300)} {
		got, err := os.ReadFile(filepath.Join(j.Dir, "data", name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("got %s err=%v, equal=%v; want the original content", name, err, bytes.Equal(got, want))
		}
	}

	left, _ := filepath.Glob(filepath.Join(j.Dir, "data", "*.par2"))
	if len(left) != 0 {
		t.Fatalf("got par2 files %v left after repair", left)
	}
}

func TestRepairFetchesTheSmallestVolumeAndRenames(t *testing.T) {
	// Damage past the first 16 KiB, under an obfuscated name.
	j, res := damagedJob(t, map[string][]byte{"a8f3k2b9": hurt(seq(40000), 5), "x91mq": seq(300)})
	f := &fakeFetcher{}

	out, err := assembleWith(f, j, res)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(f.asked, [][]string{{"release.vol2+1.par2"}}) {
		t.Fatalf("got fetches %v, want only the 1-block volume", f.asked)
	}

	wantRelease(t, j, out)

	// Running again on its own output changes nothing.
	again, err := assembleWith(f, j, out)
	if err != nil || paths(again) != paths(out) || len(f.asked) != 1 {
		t.Fatalf("got %s, %v and %d fetches on a second run, want the same files and no fetch", paths(again), err, len(f.asked))
	}
}

func TestRepairMissingFileAndDamageUsingEveryVolume(t *testing.T) {
	// movie.nfo is gone and two slices of movie.mkv are damaged: 3 blocks.
	j, res := damagedJob(t, map[string][]byte{"movie.mkv": hurt(seq(40000), 0, 13)})
	f := &fakeFetcher{}

	out, err := assembleWith(f, j, res)
	if err != nil {
		t.Fatal(err)
	}

	if len(f.asked) != 1 || len(f.asked[0]) != 2 {
		t.Fatalf("got fetches %v, want both volumes at once", f.asked)
	}

	wantRelease(t, j, out)
}

func TestRepairTruncatesWithoutFetching(t *testing.T) {
	j, res := damagedJob(t, map[string][]byte{"movie.mkv": append(seq(40000), "junk"...), "movie.nfo": seq(300)})
	f := &fakeFetcher{}

	out, err := assembleWith(f, j, res)
	if err != nil {
		t.Fatal(err)
	}

	if len(f.asked) != 0 {
		t.Fatalf("got fetches %v, want none for a truncation", f.asked)
	}

	wantRelease(t, j, out)
}

func TestRepairBeyondTheRecoveryVolumesFailsForGood(t *testing.T) {
	j, res := damagedJob(t, map[string][]byte{"movie.mkv": hurt(seq(40000), 1, 2, 3)})
	f := &fakeFetcher{}

	_, err := assembleWith(f, j, res)

	var fail *engine.Failure
	if !errors.As(err, &fail) || fail.Class != pb.FailureClass_FAILURE_CLASS_INCOMPLETE || fail.Retryable || !strings.Contains(fail.Detail, "need 4") {
		t.Fatalf("got %v, want a permanent INCOMPLETE failure needing 4 blocks", err)
	}

	if len(f.asked) != 1 || len(f.asked[0]) != 2 {
		t.Fatalf("got fetches %v, want every volume tried", f.asked)
	}
}

func TestRepairWithVolumesMissingArticlesFetchesMore(t *testing.T) {
	vol, err := os.ReadFile(filepath.Join("..", "par2", "testdata", "release.vol2+1.par2"))
	if err != nil {
		t.Fatal(err)
	}

	// The 1-block volume arrives with a hole, so a second round fetches the
	// 2-block one.
	clear(vol[2000:3000])

	j, res := damagedJob(t, map[string][]byte{"movie.mkv": hurt(seq(40000), 4), "movie.nfo": seq(300)})
	f := &fakeFetcher{data: map[string][]byte{"release.vol2+1.par2": vol}}

	out, err := assembleWith(f, j, res)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(f.asked, [][]string{{"release.vol2+1.par2"}, {"release.vol0+2.par2"}}) {
		t.Fatalf("got fetches %v, want the small volume, then the other", f.asked)
	}

	wantRelease(t, j, out)
}

func TestRepairFetchFailurePassesThrough(t *testing.T) {
	j, res := damagedJob(t, map[string][]byte{"movie.mkv": hurt(seq(40000), 4), "movie.nfo": seq(300)})
	down := engine.Fail(pb.FailureClass_FAILURE_CLASS_CLIENT, true, "cannot reach any usenet provider")

	_, err := assembleWith(&fakeFetcher{err: down}, j, res)
	if !errors.Is(err, down) {
		t.Fatalf("got %v, want the engine's failure", err)
	}
}

// forge rewrites the data of the first recovery slice in a volume and fixes
// up its packet MD5, as a hostile poster could.
func forge(t *testing.T, vol []byte) []byte {
	t.Helper()

	vol = bytes.Clone(vol)

	i := bytes.Index(vol, []byte("PAR 2.0\x00RecvSlic"))
	if i < 48 {
		t.Fatal("got no recovery slice packet")
	}

	start := i - 48
	length := int(binary.LittleEndian.Uint64(vol[start+8:]))
	vol[start+64+4+10] ^= 0xff

	h := md5.New() //nolint:gosec // see import
	h.Write(vol[start+32 : start+length])
	copy(vol[start+16:start+32], h.Sum(nil))

	return vol
}

func TestRepairWithForgedRecoveryDataFailsForGood(t *testing.T) {
	vol, err := os.ReadFile(filepath.Join("..", "par2", "testdata", "release.vol2+1.par2"))
	if err != nil {
		t.Fatal(err)
	}

	j, res := damagedJob(t, map[string][]byte{"movie.mkv": hurt(seq(40000), 4), "movie.nfo": seq(300)})

	_, err = assembleWith(&fakeFetcher{data: map[string][]byte{"release.vol2+1.par2": forge(t, vol)}}, j, res)

	var fail *engine.Failure
	if !errors.As(err, &fail) || fail.Class != pb.FailureClass_FAILURE_CLASS_REPAIR || fail.Retryable || !strings.Contains(fail.Detail, "movie.mkv") {
		t.Fatalf("got %v, want a permanent REPAIR failure naming movie.mkv", err)
	}
}

func TestRepairDiskErrorIsRetryable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix permissions enforced")
	}

	j, res := damagedJob(t, map[string][]byte{"movie.mkv": hurt(seq(40000), 4), "movie.nfo": seq(300)})
	if err := os.Chmod(filepath.Join(j.Dir, "data", "movie.mkv"), 0o400); err != nil {
		t.Fatal(err)
	}

	_, err := assembleWith(&fakeFetcher{}, j, res)

	var fail *engine.Failure
	if !errors.As(err, &fail) || fail.Class != pb.FailureClass_FAILURE_CLASS_DISK || !fail.Retryable {
		t.Fatalf("got %v, want a retryable DISK failure", err)
	}
}

func TestRepairCancelledThenResumed(t *testing.T) {
	j, res := damagedJob(t, map[string][]byte{"obf": hurt(seq(40000), 2, 9)})
	f := &fakeFetcher{}

	ctx, cancel := context.WithCancel(context.Background())
	a := &Assembler{Log: quiet, Fetcher: f, RepairMemory: 4096, repairProgress: func(int, int) { cancel() }}

	if _, err := a.Assemble(ctx, j, res); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}

	// The engine result is unchanged; the next run starts from the disk:
	// the renamed, half-repaired file and the volumes already fetched.
	out, err := assembleWith(f, j, res)
	if err != nil {
		t.Fatal(err)
	}

	if len(f.asked) != 1 {
		t.Fatalf("got fetches %v, want the volumes fetched once", f.asked)
	}

	wantRelease(t, j, out)
}

func TestChooseVolumes(t *testing.T) {
	vols := func(blocks ...int) []volume {
		var out []volume
		for i, b := range blocks {
			out = append(out, volume{name: string(rune('a' + i)), blocks: b})
		}

		return out
	}

	sum := func(v []volume) int {
		n := 0
		for _, x := range v {
			n += x.blocks
		}

		return n
	}

	cases := []struct {
		blocks    []int
		need      int
		wantTotal int
		wantCount int
	}{
		{[]int{1, 2, 4, 8, 16}, 1, 1, 1},
		{[]int{1, 2, 4, 8, 16}, 5, 5, 2},
		{[]int{1, 2, 4, 8, 16}, 11, 11, 3},
		{[]int{16, 8, 4, 2, 1}, 31, 31, 5},
		{[]int{10, 10, 3}, 12, 13, 2},
		{[]int{50}, 3, 50, 1},
		{[]int{2, 2, 2}, 9, 6, 3}, // not enough: everything
	}

	for _, c := range cases {
		got := chooseVolumes(vols(c.blocks...), c.need)
		if sum(got) != c.wantTotal || len(got) != c.wantCount {
			t.Fatalf("got %v for %v needing %d, want %d blocks in %d volumes", got, c.blocks, c.need, c.wantTotal, c.wantCount)
		}
	}

	// Equal totals prefer the smaller volumes.
	if got := chooseVolumes(vols(4, 1, 3), 4); len(got) != 2 || sum(got) != 4 {
		t.Fatalf("got %v, want 1+3 over 4", got)
	}
}
