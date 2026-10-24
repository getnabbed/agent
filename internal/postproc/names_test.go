package postproc

import (
	"bytes"
	"testing"
)

func TestLooksObfuscated(t *testing.T) {
	cases := map[string]bool{
		"18271c959a4746f8886a09441ba66999.part1.rar": true,
		"18271c959a4746f8886a09441ba66999.avi":       true,
		"data/a8f3k2b9":                              true,
		"x91mq":                                      true,
		"aB3dE6gH9jK2mN5p.mkv":                       true,
		"Monkey.Dust.S03E01.Episode.1.SDTV.XviD-MaG-Chamele0n.part01.rar":  false,
		"Monkey.Dust.S03E01.Episode.1.SDTV.XviD-MaG-Chamele0n.vol0+2.par2": false,
		"The.Movie.2020.1080p.WEB-DL.mkv":                                  false,
		"movie.mkv":                                                        false,
		"Sample.mkv":                                                       false,
		"s01e01.mkv":                                                       false,
		"subtitles_english.srt":                                            false,
		"release.r00":                                                      false,
	}

	for name, want := range cases {
		if got := looksObfuscated(name); got != want {
			t.Errorf("got looksObfuscated(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestReadableNamesAreNotTradedForPar2Names(t *testing.T) {
	// The PAR2 set calls this file movie.mkv; the NZB already gave it a
	// readable name, which is kept.
	j, res := job(t, map[string][]byte{"Readable.Name.2020.mkv": seq(40000), "movie.nfo": seq(300)})

	out, err := assemble(j, res)
	if err != nil {
		t.Fatal(err)
	}

	if got := paths(out); got != "data/Readable.Name.2020.mkv,data/movie.nfo" {
		t.Fatalf("got files %s, want the readable name kept", got)
	}
}

func TestObfuscatedMainFileIsNamedAfterTheRelease(t *testing.T) {
	video := bytes.Repeat([]byte("v"), 50_000)
	vols := rar5(1,
		rarEntry{name: "18271c959a4746f8886a09441ba66999.avi", data: video},
		rarEntry{name: "18271c959a4746f8886a09441ba66999.srt", data: []byte("subs")},
		rarEntry{name: "info.txt", data: []byte("x")},
	)
	j, res := workDir(t, map[string][]byte{"rel.part01.rar": vols[0]})
	j.Title = "Monkey.Dust.S03E01.Episode.1.SDTV.XviD-MaG-Chamele0n"

	out, err := assemble(j, res)
	if err != nil {
		t.Fatal(err)
	}

	want := "extracted/Monkey.Dust.S03E01.Episode.1.SDTV.XviD-MaG-Chamele0n.avi," +
		"extracted/Monkey.Dust.S03E01.Episode.1.SDTV.XviD-MaG-Chamele0n.srt,extracted/info.txt"
	if got := paths(out); got != want {
		t.Fatalf("got files %s, want the video and its subtitles named after the release", got)
	}

	if !bytes.Equal(readOut(t, j, "extracted/Monkey.Dust.S03E01.Episode.1.SDTV.XviD-MaG-Chamele0n.avi"), video) {
		t.Fatal("got different content after renaming")
	}

	// A rerun keeps the result.
	again, err := assemble(j, res)
	if err != nil || paths(again) != want {
		t.Fatalf("got %s, %v on a rerun", paths(again), err)
	}
}

func TestReleaseTitleCannotEscapeOrRenameReadableFiles(t *testing.T) {
	vols := rar5(1, rarEntry{name: "aB3dE6gH9jK2mN5p.mkv", data: []byte("video")})
	j, res := workDir(t, map[string][]byte{"rel.rar": vols[0]})
	j.Title = "../../etc/Show.S01E01"

	out, err := assemble(j, res)
	if err != nil || paths(out) != "extracted/Show.S01E01.mkv" {
		t.Fatalf("got %s, %v; want the title sanitised to a plain name", paths(out), err)
	}

	vols = rar5(1, rarEntry{name: "The.Show.S01E01.mkv", data: []byte("video")})
	j, res = workDir(t, map[string][]byte{"rel.rar": vols[0]})
	j.Title = "Something.Else"

	if out, err := assemble(j, res); err != nil || paths(out) != "extracted/The.Show.S01E01.mkv" {
		t.Fatalf("got %s, %v; want a readable name left alone", paths(out), err)
	}
}
