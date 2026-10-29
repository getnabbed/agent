package postproc

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"testing"

	"github.com/getnabbed/agent/internal/engine"
)

// pieces cuts b into n pieces the way splitters do: all the same size but
// the last.
func pieces(b []byte, n int) [][]byte {
	step := (len(b) + n - 1) / n

	var out [][]byte
	for i := 0; i < len(b); i += step {
		out = append(out, b[i:min(i+step, len(b))])
	}

	return out
}

// mixedPadding names volume n the way the For All Mankind release did:
// part01…part09, then part010, part011, … where part10 was expected.
func mixedPadding(n int) string {
	if n < 10 {
		return fmt.Sprintf("rel.part%02d.rar", n)
	}

	return fmt.Sprintf("rel.part0%d.rar", n)
}

func TestExtractsRARWithMixedVolumePadding(t *testing.T) {
	movie := seq(40000)
	vols := rar5(11, rarEntry{name: "movie.mkv", data: movie})

	files := map[string][]byte{}
	for i, v := range vols {
		files[mixedPadding(i+1)] = v
	}

	j, res := workDir(t, files)
	j.Notes = &engine.Notes{}

	out, err := assemble(j, res)
	if err != nil {
		t.Fatal(err)
	}

	if got := paths(out); got != "extracted/movie.mkv" {
		t.Fatalf("got files %s, want the extracted movie", got)
	}

	if !bytes.Equal(readOut(t, j, "extracted/movie.mkv"), movie) {
		t.Fatal("got different movie content after extraction")
	}

	if a := noted(j)["extract.done"].GetAttrs(); a["volumes"] != "11" {
		t.Fatalf("got extract.done attrs %v, want 11 volumes", a)
	}

	for i := range vols {
		if exists(j, "data/"+mixedPadding(i+1)) {
			t.Fatalf("got %s left after extraction; the volumes actually read must be deleted", mixedPadding(i+1))
		}
	}
}

func TestRARVolumeClaimedTwiceFailsForGood(t *testing.T) {
	vols := rar5(3, rarEntry{name: "movie.mkv", data: seq(40000)})
	j, res := workDir(t, map[string][]byte{
		"rel.part1.rar": vols[0], "rel.part2.rar": vols[1], "rel.part002.rar": vols[1], "rel.part3.rar": vols[2],
	})

	_, err := assemble(j, res)
	wantExtractFailure(t, err, "same volume number")
}

func TestRARVolumeInTheWrongPlaceFailsForGood(t *testing.T) {
	// part2 holds volume 3 and part3 holds volume 2: the names are right
	// but the headers are not, so resolving by name must not let them in.
	vols := rar5(3, rarEntry{name: "movie.mkv", data: seq(40000)})
	j, res := workDir(t, map[string][]byte{
		"rel.part1.rar": vols[0], "rel.part2.rar": vols[2], "rel.part3.rar": vols[1],
	})

	_, err := assemble(j, res)
	wantExtractFailure(t, err, "rel.part1.rar")

	if exists(j, "extracted/movie.mkv") {
		t.Fatal("got a movie spliced from volumes in the wrong order")
	}
}

func TestRAROtherReleaseIsNotTakenAsAVolume(t *testing.T) {
	// Another release's part2 in the same folder must not stand in for this
	// one's missing part2.
	vols := rar5(3, rarEntry{name: "movie.mkv", data: seq(40000)})
	other := rar5(3, rarEntry{name: "other.mkv", data: seq(40000)})
	j, res := workDir(t, map[string][]byte{
		"rel.part1.rar": vols[0], "rel.part3.rar": vols[2], "other.part2.rar": other[1],
	})

	_, err := assemble(j, res)
	wantExtractFailure(t, err, "a volume is missing")
}

func TestExtractsSplit7zWithMixedVolumePadding(t *testing.T) {
	j, res := workDir(t, map[string][]byte{
		"rel.7z.001":  fixture(t, "split.7z.001"),
		"rel.7z.02":   fixture(t, "split.7z.002"),
		"rel.7z.0003": fixture(t, "split.7z.003"),
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

	if exists(j, "data/rel.7z.0003") {
		t.Fatal("got a 7z volume left after extraction")
	}
}

func TestSplit7zWithAGapFailsForGood(t *testing.T) {
	j, res := workDir(t, map[string][]byte{
		"rel.7z.001": fixture(t, "split.7z.001"),
		"rel.7z.003": fixture(t, "split.7z.003"),
	})

	_, err := assemble(j, res)
	wantExtractFailure(t, err, "a volume is missing")
}

func TestJoinsRawSplitFile(t *testing.T) {
	movie := seq(40000)
	p := pieces(movie, 3)
	j, res := workDir(t, map[string][]byte{
		"movie.mkv.001": p[0], "movie.mkv.002": p[1], "movie.mkv.003": p[2],
		"movie.nfo": []byte("about"),
	})
	j.Notes = &engine.Notes{}

	out, err := assemble(j, res)
	if err != nil {
		t.Fatal(err)
	}

	if got := paths(out); got != "data/movie.mkv,data/movie.nfo" {
		t.Fatalf("got files %s, want the joined movie and the nfo", got)
	}

	if !bytes.Equal(readOut(t, j, "data/movie.mkv"), movie) {
		t.Fatal("got different content after joining")
	}

	if a := noted(j)["split.joined"].GetAttrs(); a["pieces"] != "3" {
		t.Fatalf("got split.joined attrs %v, want 3 pieces", a)
	}

	if exists(j, "data/movie.mkv.001") || exists(j, "data/movie.mkv.003") {
		t.Fatal("got split pieces left after joining")
	}

	again, err := assemble(j, res)
	if err != nil || paths(again) != paths(out) {
		t.Fatalf("got %s, %v on a rerun, want the same files", paths(again), err)
	}
}

func TestJoinsRawSplitArchiveThenExtractsIt(t *testing.T) {
	movie := seq(40000)
	p := pieces(rar5(1, rarEntry{name: "movie.mkv", data: movie})[0], 4)

	files := map[string][]byte{}
	for i, b := range p {
		files[fmt.Sprintf("rel.rar.%03d", i+1)] = b
	}

	j, res := workDir(t, files)

	out, err := assemble(j, res)
	if err != nil {
		t.Fatal(err)
	}

	if got := paths(out); got != "extracted/movie.mkv" {
		t.Fatalf("got files %s, want the movie from the joined archive", got)
	}

	if !bytes.Equal(readOut(t, j, "extracted/movie.mkv"), movie) {
		t.Fatal("got different content from the joined archive")
	}

	if exists(j, "data/rel.rar") || exists(j, "data/rel.rar.001") {
		t.Fatal("got the joined archive or its pieces left after extraction")
	}
}

func TestRawSplitWithAMissingPieceFailsForGood(t *testing.T) {
	p := pieces(seq(40000), 4)
	j, res := workDir(t, map[string][]byte{
		"movie.mkv.001": p[0], "movie.mkv.002": p[1], "movie.mkv.004": p[3],
	})

	_, err := assemble(j, res)
	wantExtractFailure(t, err, "missing piece")

	if exists(j, "data/movie.mkv") || !exists(j, "data/movie.mkv.001") {
		t.Fatal("got a joined file, or pieces deleted, after a failed join")
	}
}

func TestRawSplitWithUnevenPiecesFailsForGood(t *testing.T) {
	// A short piece in the middle means data went missing from it.
	p := pieces(seq(40000), 3)
	j, res := workDir(t, map[string][]byte{
		"movie.mkv.001": p[0], "movie.mkv.002": p[1][:len(p[1])-10], "movie.mkv.003": p[2],
	})

	_, err := assemble(j, res)
	wantExtractFailure(t, err, "differ in size")
}

func TestVolumeClaimedTwiceIsAmbiguous(t *testing.T) {
	// Names that differ only in case can only coexist on a case-sensitive
	// filesystem, so the rule is tested on file lists rather than on disk.
	for _, tc := range []struct {
		re    *regexp.Regexp
		files []string
	}{
		{reRawSplit, []string{"data/movie.mkv.001", "data/movie.mkv.002", "data/Movie.MKV.002"}},
		{reRarPart, []string{"data/rel.part1.rar", "data/rel.part2.rar", "data/rel.part002.rar"}},
		{re7zSplit, []string{"data/rel.7z.001", "data/rel.7z.2", "data/rel.7z.002"}},
	} {
		_, err := volumeSet(tc.files, tc.re, tc.files[0])
		if !errors.Is(err, errAmbiguousVolume) {
			t.Fatalf("got %v for %v, want errAmbiguousVolume", err, tc.files)
		}
	}
}

func TestVolumeSetKeepsToItsOwnFolderAndName(t *testing.T) {
	files := []string{
		"data/rel.part01.rar", "data/rel.part010.rar", "data/REL.PART2.rar",
		"other/rel.part3.rar", "data/rel2.part3.rar", "data/rel.part4.rar.bak",
	}

	set, err := volumeSet(files, reRarPart, files[0])
	if err != nil {
		t.Fatal(err)
	}

	want := map[int]string{1: "data/rel.part01.rar", 2: "data/REL.PART2.rar", 10: "data/rel.part010.rar"}
	if fmt.Sprint(set) != fmt.Sprint(want) {
		t.Fatalf("got set %v, want %v", set, want)
	}
}

func TestNumberedNamesThatAreNotSplitsAreLeftAlone(t *testing.T) {
	// A year is not a piece number, and a piece without its first piece is
	// not a split this job can join.
	j, res := workDir(t, map[string][]byte{
		"The.Movie.2023":  seq(300),
		"clip.mkv.005":    seq(300),
		"Show.S01E01.mkv": seq(300),
	})

	out, err := assemble(j, res)
	if err != nil {
		t.Fatal(err)
	}

	if got := paths(out); got != "data/Show.S01E01.mkv,data/The.Movie.2023,data/clip.mkv.005" {
		t.Fatalf("got files %s, want all three left as they were", got)
	}
}

func TestInterruptedJoinStartsAgain(t *testing.T) {
	movie := seq(40000)
	p := pieces(movie, 2)
	j, res := workDir(t, map[string][]byte{
		"movie.mkv.001": p[0], "movie.mkv.002": p[1],
		// What a crash in the middle of joining leaves behind.
		"movie.mkv" + joiningSuffix: p[0][:100],
	})

	out, err := assemble(j, res)
	if err != nil {
		t.Fatal(err)
	}

	if got := paths(out); got != "data/movie.mkv" {
		t.Fatalf("got files %s, want only the joined movie", got)
	}

	if !bytes.Equal(readOut(t, j, "data/movie.mkv"), movie) {
		t.Fatal("got a joined file built on the interrupted one")
	}
}
