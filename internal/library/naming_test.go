package library

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// namingFixture is one case rendered by the web app's naming.ts. The file
// is testdata/naming.json, generated in the cloud repository; only cases
// that are part of the contract are kept.
type namingFixture struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Naming struct {
		Title         string   `json:"title"`
		Year          int32    `json:"year"`
		Season        *int32   `json:"season"`
		Episodes      []int32  `json:"episodes"`
		EpisodeTitles []string `json:"episode_titles"`
		Edition       *string  `json:"edition"`
	} `json:"naming"`
	FolderTemplate string `json:"folder_template"`
	FileTemplate   string `json:"file_template"`
	SpecialsFolder string `json:"specials_folder"`
	Sanitizer      struct {
		Replacement string `json:"replacement"`
		ASCIIOnly   bool   `json:"ascii_only"`
	} `json:"sanitizer"`
	Expected string `json:"expected_relative_path"`
}

func TestRenderMatchesTheWebPreview(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "naming.json"))
	if err != nil {
		t.Fatal(err)
	}

	var cases []namingFixture
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}

	if len(cases) < 6 {
		t.Fatalf("got %d fixtures, want the 6 contract cases", len(cases))
	}

	for _, c := range cases {
		root := &pb.LibraryRoot{
			Kind:           pb.LibraryKind(pb.LibraryKind_value["LIBRARY_KIND_"+c.Kind]),
			FolderTemplate: c.FolderTemplate, FileTemplate: c.FileTemplate, SpecialsFolder: c.SpecialsFolder,
		}

		n := &pb.NamingContext{
			Title: c.Naming.Title, Year: c.Naming.Year, Season: c.Naming.Season,
			Episodes: c.Naming.Episodes, EpisodeTitles: c.Naming.EpisodeTitles, Edition: c.Naming.Edition,
		}

		got, err := RelPath(root, n, &pb.PathSanitizer{Replacement: c.Sanitizer.Replacement, AsciiOnly: c.Sanitizer.ASCIIOnly}, ".MKV")
		if err != nil || got != c.Expected {
			t.Errorf("%s: got %q, %v; want %q", c.Name, got, err, c.Expected)
		}
	}
}

func tv(season int32, episode int32, title string) *pb.NamingContext {
	return &pb.NamingContext{Title: title, Year: 2005, Season: &season, Episodes: []int32{episode}, EpisodeTitles: []string{"Rose"}}
}

func TestSpecialsFolderReplacesTheSeasonFolder(t *testing.T) {
	root := &pb.LibraryRoot{
		Kind: pb.LibraryKind_LIBRARY_KIND_TV, FolderTemplate: "{series_title}/Season {season:00}",
		FileTemplate: "{series_title} - S{season:00}E{episode:00}", SpecialsFolder: "Specials",
	}

	got, err := RelPath(root, tv(0, 1, "Doctor Who"), nil, ".mkv")
	if err != nil || got != "Doctor Who/Specials/Doctor Who - S00E01.mkv" {
		t.Fatalf("got %q, %v", got, err)
	}

	root.SpecialsFolder = ""

	if got, _ := RelPath(root, tv(0, 1, "Doctor Who"), nil, ".mkv"); got != "Doctor Who/Season 00/Doctor Who - S00E01.mkv" {
		t.Fatalf("got %q without a specials folder", got)
	}
}

func TestTokensAndPadding(t *testing.T) {
	root := &pb.LibraryRoot{
		Kind: pb.LibraryKind_LIBRARY_KIND_TV, FolderTemplate: "{SERIES TITLE}",
		FileTemplate: "{series_title} {season:3} {episode} {quality} {nope:02} {Release_Year}",
	}

	got, err := RelPath(root, tv(1, 2, "Show"), nil, ".mkv")
	if err != nil || got != "Show/Show 001 2 {nope:02} 2005.mkv" {
		t.Fatalf("got %q, %v; want unknown tokens left as written", got, err)
	}
}

func TestUnsafeValuesNeverLeaveTheirSegment(t *testing.T) {
	root := &pb.LibraryRoot{Kind: pb.LibraryKind_LIBRARY_KIND_MOVIES, FolderTemplate: "{movie_title}", FileTemplate: "{movie_title}"}

	cases := map[string]string{
		"../../etc/passwd":       ".._.._etc_passwd/.._.._etc_passwd.mkv",
		"CON":                    "_CON/_CON.mkv",
		"aux.txt":                "_aux.txt/_aux.txt.mkv",
		"  spaced  .":            "spaced/spaced.mkv",
		"tab\there\x00nul\u200b": "tabherenul/tabherenul.mkv",
	}

	for title, want := range cases {
		got, err := RelPath(root, &pb.NamingContext{Title: title}, &pb.PathSanitizer{Replacement: "_"}, ".mkv")
		if err != nil || got != want {
			t.Errorf("got %q, %v for title %q; want %q", got, err, title, want)
		}
	}

	for _, title := range []string{"...", "   ", "\x01\x02"} {
		if _, err := RelPath(root, &pb.NamingContext{Title: title}, nil, ".mkv"); !errors.Is(err, ErrNaming) {
			t.Errorf("got %v for title %q, want ErrNaming", err, title)
		}
	}
}

func TestLongNamesAreCutToFit(t *testing.T) {
	root := &pb.LibraryRoot{Kind: pb.LibraryKind_LIBRARY_KIND_MOVIES, FolderTemplate: "{movie_title}", FileTemplate: "{movie_title}"}

	got, err := RelPath(root, &pb.NamingContext{Title: strings.Repeat("é", 300)}, nil, ".mkv")
	if err != nil {
		t.Fatal(err)
	}

	for _, seg := range strings.Split(got, "/") {
		if len(seg) > maxSegmentBytes || !strings.HasPrefix(seg, "é") {
			t.Fatalf("got segment of %d bytes, want at most %d, cut on a rune boundary", len(seg), maxSegmentBytes)
		}
	}

	if !strings.HasSuffix(got, ".mkv") {
		t.Fatalf("got %q, want the extension kept", got)
	}
}

func TestMissingEpisodeDetailsFail(t *testing.T) {
	root := &pb.LibraryRoot{Kind: pb.LibraryKind_LIBRARY_KIND_TV, FolderTemplate: "{series_title}", FileTemplate: "{series_title}"}

	if _, err := RelPath(root, &pb.NamingContext{Title: "Show"}, nil, ".mkv"); !errors.Is(err, ErrNaming) {
		t.Fatalf("got %v, want ErrNaming without season and episode", err)
	}

	if _, err := RelPath(root, &pb.NamingContext{}, nil, ".mkv"); !errors.Is(err, ErrNaming) {
		t.Fatalf("got %v, want ErrNaming without a title", err)
	}
}

func TestEmptyTokensCollapse(t *testing.T) {
	movie := func(folder, file string) *pb.LibraryRoot {
		return &pb.LibraryRoot{Kind: pb.LibraryKind_LIBRARY_KIND_MOVIES, FolderTemplate: folder, FileTemplate: file}
	}

	cases := []struct {
		root *pb.LibraryRoot
		n    *pb.NamingContext
		want string
	}{
		{movie("{movie_title} ({year})", "{movie_title} ({year}) {edition}"), &pb.NamingContext{Title: "Arrival"}, "Arrival/Arrival.mkv"},
		{movie("{movie_title} ({year})", "{movie_title} ({year}) {edition}"), &pb.NamingContext{Title: "Blade Runner", Year: 1982}, "Blade Runner (1982)/Blade Runner (1982).mkv"},
		{movie("{movie_title}", "{movie_title} [{quality}] - {release_group}"), &pb.NamingContext{Title: "Heat"}, "Heat/Heat.mkv"},
		{movie("{movie_title}", "{movie_title} - {quality}"), &pb.NamingContext{Title: "Heat", Quality: &pb.Quality{Resolution: "2160p", Source: "REMUX"}}, "Heat/Heat - REMUX-2160p.mkv"},
	}

	for _, c := range cases {
		if got, err := RelPath(c.root, c.n, nil, ".mkv"); err != nil || got != c.want {
			t.Errorf("got %q, %v; want %q", got, err, c.want)
		}
	}
}

func TestQualityToken(t *testing.T) {
	cases := map[string]*pb.Quality{
		"WEBDL-1080p":  {Source: "WEB-DL", Resolution: "1080p"},
		"BluRay-1080p": {Source: "BluRay", Resolution: "1080p"},
		"WEBRip-720p":  {Source: "WEBRip", Resolution: "720p"},
		"1080p":        {Resolution: "1080p"},
		"HDTV":         {Source: "HDTV"},
		"":             nil,
	}

	for want, q := range cases {
		if got := quality(q); got != want {
			t.Errorf("got quality(%v) = %q, want %q", q, got, want)
		}
	}
}

// TestMatchesTheWebPreviewChecks mirrors the cases naming.ts checks
// (cloud scratchpad naming2/check.ts, 2026-10-24).
func TestMatchesTheWebPreviewChecks(t *testing.T) {
	o := &pb.PathSanitizer{Replacement: " -"}
	movie := func(folder string) *pb.LibraryRoot {
		return &pb.LibraryRoot{Kind: pb.LibraryKind_LIBRARY_KIND_MOVIES, FolderTemplate: folder, FileTemplate: "f"}
	}
	show := func(folder, specials string) *pb.LibraryRoot {
		return &pb.LibraryRoot{Kind: pb.LibraryKind_LIBRARY_KIND_TV, FolderTemplate: folder, FileTemplate: "f", SpecialsFolder: specials}
	}
	ed := "Final Cut"

	folder := func(r *pb.LibraryRoot, n *pb.NamingContext) string {
		t.Helper()

		got, err := RelPath(r, n, o, ".mkv")
		if err != nil {
			t.Fatal(err)
		}

		return strings.TrimSuffix(got, "/f.mkv")
	}

	cases := []struct{ got, want, name string }{
		{folder(movie("{movie_title} ({year})"), &pb.NamingContext{Title: "Arrival"}), "Arrival", "no year"},
		{folder(movie("{movie_title} ({year}) {edition}"), &pb.NamingContext{Title: "Blade Runner", Year: 1982}), "Blade Runner (1982)", "no edition"},
		{folder(movie("{movie_title} ({year}) {edition}"), &pb.NamingContext{Title: "Blade Runner", Year: 1982, Edition: &ed}), "Blade Runner (1982) Final Cut", "edition"},
		{folder(show("{series_title}/Season {season:02}", "Specials"), tv(0, 1, "Doctor Who")), "Doctor Who/Specials", "specials"},
		{folder(show("{series_title}/Season {season:02}", ""), tv(0, 1, "Doctor Who")), "Doctor Who/Season 00", "specials empty"},
		{folder(show("{series_title}/Season {season:02}", "Specials"), tv(3, 1, "The Bear")), "The Bear/Season 03", "normal season"},
		{folder(movie("{movie_title} [{quality}]"), &pb.NamingContext{Title: "Dune"}), "Dune", "empty quality brackets"},
		{quality(&pb.Quality{Source: "WEB-DL", Resolution: "1080p"}), "WEBDL-1080p", "quality webdl"},
		{quality(&pb.Quality{Source: "BluRay", Resolution: "2160p"}), "BluRay-2160p", "quality bluray"},
		{quality(&pb.Quality{Resolution: "720p"}), "720p", "quality res only"},
		{quality(nil), "", "quality none"},
		{collapse("  A  (  )  [ ] - "), "A", "collapse mixed"},
	}

	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}

	// The episode-title case is a file name.
	n := tv(1, 2, "X")
	n.EpisodeTitles = []string{""}
	r := &pb.LibraryRoot{Kind: pb.LibraryKind_LIBRARY_KIND_TV, FolderTemplate: "", FileTemplate: "{series_title} - S{season:02}E{episode:02} - {episode_title}"}

	if got, err := RelPath(r, n, o, ".mkv"); err != nil || got != "X - S01E02.mkv" {
		t.Errorf("empty title dash: got %q, %v", got, err)
	}

	// A folder made only of empty tokens is dropped.
	if got := folder(movie("{release_group}/{movie_title}"), &pb.NamingContext{Title: "Heat"}); got != "Heat" {
		t.Errorf("got %q, want the empty folder dropped", got)
	}
}
