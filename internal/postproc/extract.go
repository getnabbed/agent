package postproc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/getnabbed/agent/internal/engine"
	"github.com/getnabbed/agent/internal/engine/usenet"
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

const (
	// extractDir holds archive contents inside the work directory.
	extractDir = "extracted"
	// markerFile records a finished extraction, so a rerun after a crash
	// during clean-up does not look for archives it already deleted.
	markerFile = "extract.json"
	// maxDictBytes caps the decoder memory one archive may ask for. RAR5
	// defaults to 32 MiB and 7-Zip's ultra preset to 64 MiB; the formats
	// allow 4 GiB and more, which would let one release exhaust memory.
	maxDictBytes = 1 << 30
	// maxDepth bounds directory nesting inside an archive.
	maxDepth = 32

	defaultMaxEntries = 10000
	defaultSlack      = 1 << 30
)

var (
	errBudget    = errors.New("archive output exceeds the limit")
	errEntries   = errors.New("archive has too many entries")
	errEncrypted = errors.New("archive is password protected")
)

// diskError marks a failure writing output, which is worth retrying.
type diskError struct{ err error }

func (e diskError) Error() string { return e.err.Error() }
func (e diskError) Unwrap() error { return e.err }

type kind int

const (
	kindRAR kind = iota
	kind7z
	kindZIP
)

func (k kind) String() string {
	switch k {
	case kindRAR:
		return "rar"
	case kind7z:
		return "7z"
	default:
		return "zip"
	}
}

// archive is one archive to extract, named by its first volume.
type archive struct {
	kind kind
	name string
}

var (
	reRarPart = regexp.MustCompile(`(?i)\.part0*(\d+)\.rar$`)
	reRar     = regexp.MustCompile(`(?i)\.rar$`)
	re7zSplit = regexp.MustCompile(`(?i)\.7z\.(\d+)$`)
	re7z      = regexp.MustCompile(`(?i)\.7z$`)
	reZip     = regexp.MustCompile(`(?i)\.zip$`)

	magicRAR = []byte("Rar!\x1a\x07")
	magic7z  = []byte("7z\xbc\xaf\x27\x1c")
	magicZIP = []byte("PK\x03\x04")
)

// findArchives returns the first volume of each archive among files, by
// name, confirmed by the file's signature. Later volumes are found by the
// readers.
func findArchives(root *os.Root, files []string, log *slog.Logger) []archive {
	var out []archive

	for _, f := range files {
		base := path.Base(f)

		var k kind

		switch {
		case reRarPart.MatchString(base):
			if n, _ := strconv.Atoi(reRarPart.FindStringSubmatch(base)[1]); n != 1 {
				continue
			}

			k = kindRAR
		case reRar.MatchString(base):
			k = kindRAR
		case re7zSplit.MatchString(base):
			if n, _ := strconv.Atoi(re7zSplit.FindStringSubmatch(base)[1]); n != 1 {
				continue
			}

			k = kind7z
		case re7z.MatchString(base):
			k = kind7z
		case reZip.MatchString(base):
			k = kindZIP
		default:
			continue
		}

		if !hasMagic(root, f, k) {
			log.Warn("not extracting: file does not look like its extension says", "file", f, "kind", k)
			continue
		}

		out = append(out, archive{kind: k, name: f})
	}

	return out
}

func hasMagic(root *os.Root, name string, k kind) bool {
	f, err := root.Open(name)
	if err != nil {
		return false
	}

	defer func() { _ = f.Close() }()

	head := make([]byte, 8)
	n, _ := io.ReadFull(f, head)
	head = head[:n]

	switch k {
	case kindRAR:
		return bytes.HasPrefix(head, magicRAR)
	case kind7z:
		return bytes.HasPrefix(head, magic7z)
	default:
		return bytes.HasPrefix(head, magicZIP)
	}
}

// extractor writes archive entries under extractDir within the limits of
// one job.
type extractor struct {
	ctx     context.Context
	root    *os.Root
	log     *slog.Logger
	budget  int64
	entries int
	used    map[string]bool
	written []string
	skipped int
}

// entryPath maps an archive entry name to a path under extractDir. Every
// component is sanitised like an NZB file name; ".." is refused.
func entryPath(name string) (string, bool) {
	name = strings.ReplaceAll(name, "\\", "/")

	parts := []string{extractDir}
	for _, c := range strings.Split(name, "/") {
		switch c {
		case "", ".":
			continue
		case "..":
			return "", false
		}

		parts = append(parts, usenet.SafeName(c, len(parts)))
	}

	if len(parts) == 1 || len(parts) > maxDepth+1 {
		return "", false
	}

	return path.Join(parts...), true
}

func (x *extractor) count() error {
	x.entries--
	if x.entries < 0 {
		return errEntries
	}

	return nil
}

// skip records an entry that is not a regular file or directory, such as a
// link, which is never created.
func (x *extractor) skip(name, why string) {
	x.skipped++
	x.log.Warn("skipping archive entry", "entry", name, "reason", why)
}

func (x *extractor) mkdir(name string) error {
	if err := x.count(); err != nil {
		return err
	}

	p, ok := entryPath(name)
	if !ok {
		x.skip(name, "unsafe path")
		return nil
	}

	if err := x.root.MkdirAll(p, 0o750); err != nil {
		return diskError{err}
	}

	return nil
}

// create writes one regular file from r. A name used twice, or two names
// that differ only in case, get a numbered suffix.
func (x *extractor) create(name string, r io.Reader) error {
	if err := x.count(); err != nil {
		return err
	}

	p, ok := entryPath(name)
	if !ok {
		x.skip(name, "unsafe path")
		return nil
	}

	p = uniqueTarget(p, x.used)

	if err := x.root.MkdirAll(path.Dir(p), 0o750); err != nil {
		return diskError{err}
	}

	f, err := x.root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return diskError{err}
	}

	_, err = io.Copy(&budgetWriter{x: x, w: f}, r)
	if cerr := f.Close(); err == nil && cerr != nil {
		err = diskError{cerr}
	}

	if err != nil {
		_ = x.root.Remove(p)
		return err
	}

	x.written = append(x.written, p)

	return nil
}

// budgetWriter stops an archive from writing more than the job allows and
// stops copying when the job is cancelled.
type budgetWriter struct {
	x *extractor
	w io.Writer
}

func (b *budgetWriter) Write(p []byte) (int, error) {
	if err := b.x.ctx.Err(); err != nil {
		return 0, err
	}

	if int64(len(p)) > b.x.budget {
		return 0, errBudget
	}

	n, err := b.w.Write(p)
	b.x.budget -= int64(n)

	if err != nil {
		return n, diskError{err}
	}

	return n, nil
}

// marker is written once extraction has finished, before clean-up.
type marker struct {
	// Remove lists archive volumes and PAR2 files to delete.
	Remove []string `json:"remove"`
	// Files is the assembled output.
	Files []string `json:"files"`
}

func readMarker(root *os.Root) (*marker, bool) {
	raw, err := fs.ReadFile(root.FS(), markerFile)
	if err != nil {
		return nil, false
	}

	var m marker
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}

	return &m, true
}

func writeMarker(root *os.Root, m *marker) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}

	tmp := markerFile + ".tmp"
	if err := root.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}

	return root.Rename(tmp, markerFile)
}

// unpack extracts the archives among files into extractDir, then deletes
// the archives and par2 files. It returns the assembled result: extracted
// files plus downloaded files that were neither archives nor PAR2.
func (a *Assembler) unpack(ctx context.Context, root *os.Root, files, par2Files []string, log *slog.Logger) (*engine.Result, error) {
	var data []string
	for _, f := range files {
		if !slices.Contains(par2Files, f) {
			data = append(data, f)
		}
	}

	archives := findArchives(root, data, log)

	var input int64
	for _, f := range data {
		if fi, err := root.Stat(f); err == nil {
			input += fi.Size()
		}
	}

	if err := root.RemoveAll(extractDir); err != nil {
		return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "clear %s: %v", extractDir, err)
	}

	x := &extractor{
		ctx:     ctx,
		root:    root,
		log:     log,
		budget:  2*input + a.slack(),
		entries: a.maxEntries(),
		used:    map[string]bool{},
	}

	remove := slices.Clone(par2Files)

	for _, arc := range archives {
		volumes, err := x.extract(arc)
		if err != nil {
			return nil, classify(ctx, arc, err)
		}

		log.Info("extracted archive", "archive", arc.name, "kind", arc.kind, "volumes", len(volumes))
		remove = append(remove, volumes...)
	}

	m := &marker{Remove: remove}
	for _, f := range data {
		if !slices.Contains(remove, f) {
			m.Files = append(m.Files, f)
		}
	}

	m.Files = append(m.Files, x.written...)

	if len(archives) > 0 || len(remove) > 0 {
		if err := writeMarker(root, m); err != nil {
			return nil, engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "record extraction: %v", err)
		}
	}

	if x.skipped > 0 {
		log.Warn("archives had entries that were not extracted", "entries", x.skipped)
	}

	return finish(root, m, log), nil
}

func (x *extractor) extract(arc archive) ([]string, error) {
	switch arc.kind {
	case kindRAR:
		return x.rar(arc.name)
	case kind7z:
		return x.sevenZip(arc.name)
	default:
		return x.zip(arc.name)
	}
}

// finish deletes what m says to remove and lists the output that remains.
func finish(root *os.Root, m *marker, log *slog.Logger) *engine.Result {
	for _, f := range m.Remove {
		if err := root.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Warn("remove after extraction", "file", f, "err", err)
		}
	}

	out := &engine.Result{}
	for _, f := range m.Files {
		fi, err := root.Stat(f)
		if err != nil {
			continue
		}

		out.Files = append(out.Files, engine.File{Path: f, Size: fi.Size()})
	}

	slices.SortFunc(out.Files, func(a, b engine.File) int { return strings.Compare(a.Path, b.Path) })

	return out
}

// classify turns an extraction error into the failure the cloud sees. A
// broken archive in a release whose files verified is broken for good, so
// only write errors are retryable.
func classify(ctx context.Context, arc archive, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	var de diskError

	switch {
	case errors.As(err, &de):
		return engine.Fail(pb.FailureClass_FAILURE_CLASS_DISK, true, "extract %s: %v", path.Base(arc.name), de.err)
	case errors.Is(err, errEncrypted):
		return engine.Fail(pb.FailureClass_FAILURE_CLASS_EXTRACT, false, "%s is password protected", path.Base(arc.name))
	case errors.Is(err, errBudget), errors.Is(err, errEntries):
		return engine.Fail(pb.FailureClass_FAILURE_CLASS_EXTRACT, false, "%s: %v", path.Base(arc.name), err)
	case errors.Is(err, fs.ErrNotExist):
		return engine.Fail(pb.FailureClass_FAILURE_CLASS_EXTRACT, false, "%s: a volume is missing: %v", path.Base(arc.name), err)
	}

	return engine.Fail(pb.FailureClass_FAILURE_CLASS_EXTRACT, false, "%s is corrupt or unsupported: %v", path.Base(arc.name), err)
}

func (a *Assembler) maxEntries() int {
	if a.MaxEntries > 0 {
		return a.MaxEntries
	}

	return defaultMaxEntries
}

func (a *Assembler) slack() int64 {
	if a.Slack > 0 {
		return a.Slack
	}

	return defaultSlack
}
