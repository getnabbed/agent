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
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Limits on untrusted NZB input. They are enforced while streaming, so a
// hostile document is rejected before it is held in memory. Real releases
// stay far below them: a 100 GB release in 750 KB articles is ~140k
// segments.
const (
	maxNZBBytes      = 64 << 20
	maxNZBFiles      = 10_000
	maxNZBSegments   = 500_000
	maxNameBytes     = 255
	maxSubjectBytes  = 4096
	maxTextBytes     = 1024 // message-id, group or meta value
	maxGroupsPerFile = 64
	maxMetaEntries   = 64
	// nameRoom is kept free when truncating names so that a " (NNNN)"
	// de-duplication suffix still fits in maxNameBytes.
	nameRoom = 16
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

// ParseNZB reads an NZB, validating structure and sizes as it streams and
// assigning each file a unique, safe name.
func ParseNZB(r io.Reader) (*NZB, error) {
	dec := xml.NewDecoder(io.LimitReader(r, maxNZBBytes))
	dec.Strict = false
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }

	p := parser{out: &NZB{Meta: map[string]string{}}, used: map[string]bool{}}

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("parse nzb: %w", err)
		}

		if err := p.token(tok); err != nil {
			return nil, err
		}
	}

	if len(p.out.Files) == 0 {
		return nil, errors.New("nzb has no files")
	}

	return p.out, nil
}

// parser holds streaming state. Only one file, segment and text value are
// in progress at a time.
type parser struct {
	out  *NZB
	used map[string]bool

	file     *NZBFile
	seen     map[int]bool
	segments int

	// Text collection for the element being read.
	collecting bool
	overflow   bool
	text       strings.Builder

	segNumber int
	segBytes  int64
	metaType  string
}

func (p *parser) token(tok xml.Token) error {
	switch t := tok.(type) {
	case xml.StartElement:
		return p.start(t)
	case xml.CharData:
		if p.collecting && !p.overflow {
			if p.text.Len()+len(t) > maxTextBytes {
				p.overflow = true
			} else {
				p.text.Write(t)
			}
		}
	case xml.EndElement:
		p.end(t.Name.Local)
	}

	return nil
}

func (p *parser) collect() {
	p.collecting, p.overflow = true, false
	p.text.Reset()
}

func (p *parser) start(t xml.StartElement) error {
	switch t.Name.Local {
	case "file":
		if p.file != nil {
			return errors.New("nzb: nested <file>")
		}

		if len(p.out.Files) >= maxNZBFiles {
			return fmt.Errorf("nzb has more than %d files", maxNZBFiles)
		}

		subject := attr(t, "subject")
		if len(subject) > maxSubjectBytes {
			subject = subject[:maxSubjectBytes]
		}

		p.file = &NZBFile{Subject: subject}
		p.seen = map[int]bool{}
	case "segment":
		if p.file == nil {
			return nil
		}

		if p.segments >= maxNZBSegments {
			return fmt.Errorf("nzb has more than %d segments", maxNZBSegments)
		}

		p.segNumber, _ = strconv.Atoi(attr(t, "number"))
		p.segBytes, _ = strconv.ParseInt(attr(t, "bytes"), 10, 64)
		p.collect()
	case "group":
		if p.file != nil {
			p.collect()
		}
	case "meta":
		p.metaType = strings.ToLower(attr(t, "type"))
		p.collect()
	}

	return nil
}

func (p *parser) end(name string) {
	text := strings.TrimSpace(p.text.String())
	ok := p.collecting && !p.overflow

	switch name {
	case "segment":
		if p.file != nil && ok {
			p.addSegment(text)
		}
	case "group":
		if p.file != nil && ok && text != "" && len(p.file.Groups) < maxGroupsPerFile {
			p.file.Groups = append(p.file.Groups, text)
		}
	case "meta":
		if ok && p.metaType != "" && len(p.out.Meta) < maxMetaEntries {
			p.out.Meta[p.metaType] = text
		}
	case "file":
		if p.file != nil {
			p.finishFile()
		}
	}

	if name == "segment" || name == "group" || name == "meta" {
		p.collecting = false
	}
}

func (p *parser) addSegment(id string) {
	// Malformed or duplicate segments are skipped and count as missing.
	// The filter also stops NNTP command injection through message-ids.
	if id == "" || p.segNumber <= 0 || p.seen[p.segNumber] || strings.ContainsAny(id, "\r\n<> \x00") {
		return
	}

	p.seen[p.segNumber] = true
	p.segments++
	p.file.Segments = append(p.file.Segments, Segment{Number: p.segNumber, Bytes: max(p.segBytes, 0), MessageID: id})
}

func (p *parser) finishFile() {
	f := p.file
	sort.Slice(f.Segments, func(a, b int) bool { return f.Segments[a].Number < f.Segments[b].Number })

	f.Name = uniqueName(SafeName(nameFromSubject(f.Subject), len(p.out.Files)), p.used)
	p.out.Files = append(p.out.Files, *f)
	p.file, p.seen = nil, nil
}

func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}

	return ""
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
// escape a directory, hide, or impersonate another name: no separators, no
// "." or "..", no control, format (bidi) or invalid UTF-8 characters, no
// leading dots, no trailing dots or spaces, no Windows device names, and
// short enough to take a de-duplication suffix. An unusable name becomes
// file-<index>.
func SafeName(name string, index int) string {
	name = strings.ToValidUTF8(name, "_")
	name = strings.ReplaceAll(name, "\\", "/")

	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}

	name = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return '_'
		case strings.ContainsRune(`:*?"<>|`, r):
			return '_'
		}

		return r
	}, name)

	name = trimName(name)
	name = truncateRunes(name, maxNameBytes-nameRoom)
	name = trimName(name) // truncation can expose a trailing dot or space

	if name == "" || isReservedWindowsName(name) {
		return "file-" + strconv.Itoa(index+1)
	}

	return name
}

// trimName drops leading dots and spaces (hidden files) and trailing dots
// and spaces, which Windows strips silently and would cause collisions.
func trimName(s string) string {
	s = strings.TrimLeft(strings.TrimSpace(s), ".")

	return strings.TrimRight(s, ". ")
}

func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}

	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}

	return s
}

// isReservedWindowsName matches Windows device names, which refer to
// devices in any directory regardless of extension, following Go's own
// list: CON, PRN, AUX, NUL, COM0-9, LPT0-9 (including superscript digits),
// CONIN$ and CONOUT$. Trailing spaces before the extension do not help.
func isReservedWindowsName(name string) bool {
	base, _, _ := strings.Cut(name, ".")
	base = strings.ToUpper(strings.TrimRight(base, " "))

	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}

	if len(base) >= 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		suffix := base[3:]

		switch suffix {
		case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
			return true
		}
	}

	return false
}

// collisionKey folds case and Unicode normalisation, so names that the
// filesystem would treat as the same (NTFS, APFS) are detected.
func collisionKey(name string) string {
	return strings.ToLower(norm.NFC.String(name))
}

func uniqueName(name string, used map[string]bool) string {
	if key := collisionKey(name); !used[key] {
		used[key] = true
		return name
	}

	stem, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		stem, ext = name[:i], name[i:]
	}

	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, n, ext)
		if key := collisionKey(candidate); !used[key] {
			used[key] = true
			return candidate
		}
	}
}

var par2Volume = regexp.MustCompile(`(?i)\.vol\d+\+\d+\.par2$`)

// IsPAR2Volume reports whether name is a PAR2 recovery volume. These are
// downloaded only when repair needs them.
func IsPAR2Volume(name string) bool { return par2Volume.MatchString(name) }
