// Package usenet is the native usenet engine: it parses an NZB, fetches
// each article over NNTP, decodes yEnc and writes the parts at their
// offsets into the job's work directory.
package usenet

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Limits on untrusted NZB input.
const (
	maxNZBBytes    = 64 << 20
	maxNZBFiles    = 10_000
	maxNZBSegments = 2_000_000
	maxNameBytes   = 255
)

// NZB is a parsed NZB document.
type NZB struct {
	Meta  map[string]string
	Files []NZBFile
}

// NZBFile is one file of an NZB.
type NZBFile struct {
	// Name is a safe base name derived from the subject.
	Name     string
	Subject  string
	Groups   []string
	Segments []Segment
}

// Segment is one article.
type Segment struct {
	Number    int
	Bytes     int64
	MessageID string
}

// Bytes returns the sum of the file's encoded segment sizes.
func (f *NZBFile) Bytes() int64 {
	var n int64
	for _, s := range f.Segments {
		n += s.Bytes
	}

	return n
}

type xmlNZB struct {
	Head struct {
		Meta []struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"meta"`
	} `xml:"head"`
	Files []struct {
		Subject  string   `xml:"subject,attr"`
		Groups   []string `xml:"groups>group"`
		Segments []struct {
			Bytes  int64  `xml:"bytes,attr"`
			Number int    `xml:"number,attr"`
			ID     string `xml:",chardata"`
		} `xml:"segments>segment"`
	} `xml:"file"`
}

// ParseNZB reads an NZB, validating structure and sizes and assigning each
// file a unique, safe name.
func ParseNZB(r io.Reader) (*NZB, error) {
	dec := xml.NewDecoder(io.LimitReader(r, maxNZBBytes))
	dec.Strict = false
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }

	var raw xmlNZB
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse nzb: %w", err)
	}

	if len(raw.Files) == 0 {
		return nil, errors.New("nzb has no files")
	}

	if len(raw.Files) > maxNZBFiles {
		return nil, fmt.Errorf("nzb has %d files, limit %d", len(raw.Files), maxNZBFiles)
	}

	out := &NZB{Meta: map[string]string{}}
	for _, m := range raw.Head.Meta {
		out.Meta[strings.ToLower(m.Type)] = strings.TrimSpace(m.Value)
	}

	used := map[string]bool{}
	total := 0

	for i, f := range raw.Files {
		seen := map[int]bool{}
		file := NZBFile{Subject: f.Subject, Groups: f.Groups}

		for _, s := range f.Segments {
			id := strings.TrimSpace(s.ID)
			if id == "" || s.Number <= 0 || seen[s.Number] || strings.ContainsAny(id, "\r\n<> ") {
				continue // malformed or duplicate segment: skip, counts as missing
			}

			seen[s.Number] = true
			file.Segments = append(file.Segments, Segment{Number: s.Number, Bytes: max(s.Bytes, 0), MessageID: id})
		}

		total += len(file.Segments)
		if total > maxNZBSegments {
			return nil, fmt.Errorf("nzb has more than %d segments", maxNZBSegments)
		}

		sort.Slice(file.Segments, func(a, b int) bool { return file.Segments[a].Number < file.Segments[b].Number })

		file.Name = uniqueName(SafeName(nameFromSubject(f.Subject), i), used)
		out.Files = append(out.Files, file)
	}

	return out, nil
}

var quoted = regexp.MustCompile(`"([^"]+)"`)

// nameFromSubject extracts the conventional "name.ext" from a subject such
// as `[01/10] - "show.s01e01.mkv" yEnc (1/50)`.
func nameFromSubject(subject string) string {
	if m := quoted.FindStringSubmatch(subject); m != nil {
		return m[1]
	}

	return ""
}

// SafeName reduces an untrusted name to a single path element that cannot
// escape a directory or hide: no separators, no "." or "..", no control
// characters, no leading dots, at most 255 bytes. An unusable name becomes
// file-<index>.
func SafeName(name string, index int) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}

	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			return '_'
		}

		return r
	}, name)

	name = strings.TrimLeft(strings.TrimSpace(name), ".")
	name = strings.TrimRight(name, ". ") // Windows strips these silently

	for len(name) > maxNameBytes {
		_, size := lastRune(name)
		name = name[:len(name)-size]
	}

	if name == "" || isReservedWindowsName(name) {
		return "file-" + strconv.Itoa(index+1)
	}

	return name
}

func lastRune(s string) (rune, int) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i]&0xC0 != 0x80 {
			return rune(s[i]), len(s) - i
		}
	}

	return 0, 1
}

func isReservedWindowsName(name string) bool {
	base, _, _ := strings.Cut(strings.ToUpper(name), ".")
	switch base {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	}

	return false
}

func uniqueName(name string, used map[string]bool) string {
	key := strings.ToLower(name)
	if !used[key] {
		used[key] = true
		return name
	}

	stem, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		stem, ext = name[:i], name[i:]
	}

	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, n, ext)
		if !used[strings.ToLower(candidate)] {
			used[strings.ToLower(candidate)] = true
			return candidate
		}
	}
}

var par2Volume = regexp.MustCompile(`(?i)\.vol\d+\+\d+\.par2$`)

// IsPAR2Volume reports whether name is a PAR2 recovery volume. These are
// downloaded only when repair needs them.
func IsPAR2Volume(name string) bool { return par2Volume.MatchString(name) }
