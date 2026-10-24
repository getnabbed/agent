package library

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// Naming renders library paths exactly as the web app previews them
// (packages/app/src/lib/naming.ts in the cloud repository):
//
//   - Tokens match {name} or {name:NN}, case-insensitively; spaces in a
//     name count as underscores, and release_year is year.
//   - :NN pads with zeros to the larger of its digit count and its value,
//     so :00 and :02 give width 2 and :3 gives 3. Strings are padded too.
//   - Unknown tokens stay as written.
//   - Each token value is sanitised: <>:"/\|?* become the replacement and,
//     with ascii_only, the value is NFKD-normalised and stripped to
//     printable ASCII.
//
// The agent then makes every path segment safe on every platform (see
// safeSegment), which the preview does not do yet.

var tokenPattern = regexp.MustCompile(`(?i)\{([a-z][a-z _]*)(?::(\d+))?\}`)

const (
	maxSegmentBytes = 240
	unsafeChars     = `<>:"/\|?*`
)

// ErrNaming means a path cannot be rendered from the naming context.
var ErrNaming = fmt.Errorf("%w: cannot name the file", ErrInvalid)

// tokens returns the values a library kind's templates may use.
func tokens(kind pb.LibraryKind, n *pb.NamingContext) map[string]string {
	t := map[string]string{"year": num(n.GetYear())}

	switch kind {
	case pb.LibraryKind_LIBRARY_KIND_TV:
		t["series_title"] = n.GetTitle()
		if n.Season != nil {
			t["season"] = strconv.Itoa(int(n.GetSeason()))
		}

		if eps := n.GetEpisodes(); len(eps) > 0 {
			t["episode"] = strconv.Itoa(int(eps[0]))
		}

		if titles := n.GetEpisodeTitles(); len(titles) > 0 {
			t["episode_title"] = titles[0]
		}
	case pb.LibraryKind_LIBRARY_KIND_MOVIES:
		t["movie_title"] = n.GetTitle()
		t["edition"] = n.GetEdition()
	}

	return t
}

func num(v int32) string {
	if v == 0 {
		return ""
	}

	return strconv.Itoa(int(v))
}

// render fills one template.
func render(tpl string, values map[string]string, s *pb.PathSanitizer) string {
	return tokenPattern.ReplaceAllStringFunc(tpl, func(tok string) string {
		m := tokenPattern.FindStringSubmatch(tok)

		name := strings.ReplaceAll(strings.ToLower(m[1]), " ", "_")
		if name == "release_year" {
			name = "year"
		}

		v, ok := values[name]
		if !ok {
			return tok
		}

		v = sanitizeValue(v, s)

		if m[2] != "" {
			width, _ := strconv.Atoi(m[2])
			width = min(max(width, len(m[2])), 16)

			if n := utf8.RuneCountInString(v); n < width {
				v = strings.Repeat("0", width-n) + v
			}
		}

		return v
	})
}

func sanitizeValue(v string, s *pb.PathSanitizer) string {
	var b strings.Builder

	for _, r := range v {
		if strings.ContainsRune(unsafeChars, r) {
			b.WriteString(s.GetReplacement())
			continue
		}

		b.WriteRune(r)
	}

	v = b.String()

	if s.GetAsciiOnly() {
		v = strings.Map(func(r rune) rune {
			if r < 0x20 || r > 0x7e {
				return -1
			}

			return r
		}, norm.NFKD.String(v))
	}

	return v
}

// RelPath renders the slash-separated path, relative to the root, for a
// file with extension ext (".mkv"), and checks that every value the
// templates need is present. Season 0 goes into the specials folder when
// one is set.
func RelPath(root *pb.LibraryRoot, n *pb.NamingContext, s *pb.PathSanitizer, ext string) (string, error) {
	if n.GetTitle() == "" {
		return "", fmt.Errorf("%w: the download has no title", ErrNaming)
	}

	if root.GetKind() == pb.LibraryKind_LIBRARY_KIND_TV && (n.Season == nil || len(n.GetEpisodes()) == 0) {
		return "", fmt.Errorf("%w: the episode has no season or episode number", ErrNaming)
	}

	values := tokens(root.GetKind(), n)

	var segments []string

	for part := range strings.SplitSeq(root.GetFolderTemplate(), "/") {
		if part == "" {
			continue
		}

		seg := render(part, values, s)
		if root.GetKind() == pb.LibraryKind_LIBRARY_KIND_TV && n.GetSeason() == 0 && root.GetSpecialsFolder() != "" &&
			tokenNamed(part, "season") {
			seg = root.GetSpecialsFolder()
		}

		safe, err := safeSegment(seg, "")
		if err != nil {
			return "", err
		}

		segments = append(segments, safe)
	}

	file, err := safeSegment(render(root.GetFileTemplate(), values, s), strings.ToLower(ext))
	if err != nil {
		return "", err
	}

	return strings.Join(append(segments, file), "/"), nil
}

// tokenNamed reports whether template part uses token name.
func tokenNamed(part, name string) bool {
	for _, m := range tokenPattern.FindAllStringSubmatch(part, -1) {
		if strings.ReplaceAll(strings.ToLower(m[1]), " ", "_") == name {
			return true
		}
	}

	return false
}

var reservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// safeSegment makes one path component valid on Linux, macOS and Windows:
// no control characters or slashes, no leading or trailing spaces, no
// trailing dots, not a reserved Windows name, at most 240 bytes. ext, if
// any, is appended after the checks and counts toward the limit. An empty
// result is an error rather than a guess.
func safeSegment(seg, ext string) (string, error) {
	seg = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return -1
		case r == '/' || r == '\\' || r == 0:
			return '_'
		}

		return r
	}, strings.ToValidUTF8(seg, ""))

	seg = strings.TrimRight(strings.TrimSpace(seg), ". ")

	for len(seg)+len(ext) > maxSegmentBytes && seg != "" {
		_, size := utf8.DecodeLastRuneInString(seg)
		seg = strings.TrimRight(strings.TrimSpace(seg[:len(seg)-size]), ". ")
	}

	if seg == "" || seg == "." || seg == ".." {
		return "", fmt.Errorf("%w: a path part is empty after removing unsafe characters", ErrNaming)
	}

	if stem, _, _ := strings.Cut(seg, "."); reservedNames[strings.ToLower(strings.TrimSpace(stem))] {
		seg = "_" + seg
	}

	return seg + ext, nil
}
