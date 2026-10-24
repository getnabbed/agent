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
	if err != nil || got != "Show/Show 001 2 {quality} {nope:02} 2005.mkv" {
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
