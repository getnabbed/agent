package postproc

import (
	"log/slog"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/getnabbed/agent/internal/engine/usenet"
)

var (
	// volumeSuffix matches the parts of a name that number archive or PAR2
	// volumes, so "abc.part01.rar" and "abc.vol0+2.par2" both reduce to "abc".
	volumeSuffix = regexp.MustCompile(`(?i)(\.part\d+\.rar|\.r\d\d|\.7z\.\d+|\.vol\d+\+\d+\.par2|\.z\d\d)$`)
	hexStem      = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	// randomStem is a long run of letters and digits with no word breaks,
	// as obfuscators produce; release names always have separators.
	randomStem = regexp.MustCompile(`^[A-Za-z0-9]{12,}$`)
)

// looksObfuscated reports whether a file name looks generated rather than
// chosen by a person: no extension at all, a long hex string, or a long run
// of mixed letters and digits with no separators.
func looksObfuscated(name string) bool {
	base := path.Base(name)

	// A volume suffix is the extension: "release.r00" has a stem "release".
	stem := volumeSuffix.ReplaceAllString(base, "")
	if stem == base {
		ext := path.Ext(base)
		if ext == "" || ext == base {
			return true
		}

		stem = strings.TrimSuffix(base, ext)
	}

	if hexStem.MatchString(stem) {
		return true
	}

	return randomStem.MatchString(stem) && strings.ContainsAny(stem, "0123456789") &&
		strings.ToLower(stem) != stem && strings.ToUpper(stem) != stem
}

// nameMainFile gives the largest output file the release title when its
// own name is obfuscated, keeping the extension. Other files sharing its
// stem, such as a matching .srt or .nfo, are renamed alike. Names already
// taken are left alone. It returns files with the new paths.
func nameMainFile(root *os.Root, files []string, title string, log *slog.Logger) []string {
	if strings.TrimSpace(title) == "" {
		return files
	}

	main, size := "", int64(-1)
	for _, f := range files {
		if fi, err := root.Stat(f); err == nil && fi.Size() > size {
			main, size = f, fi.Size()
		}
	}

	if main == "" || !looksObfuscated(main) {
		return files
	}

	stem := strings.TrimSuffix(path.Base(main), path.Ext(main))
	// Sanitised like an NZB name; the title comes from the control plane.
	newStem := usenet.SafeName(title, 0)

	out := make([]string, 0, len(files))

	for _, f := range files {
		base := path.Base(f)
		if path.Dir(f) != path.Dir(main) || !strings.HasPrefix(base, stem+".") {
			out = append(out, f)
			continue
		}

		to := path.Join(path.Dir(f), newStem+base[len(stem):])

		if _, err := root.Lstat(to); err == nil {
			log.Warn("not naming file after the release: the name is taken", "file", f, "to", to)
			out = append(out, f)

			continue
		}

		if err := root.Rename(f, to); err != nil {
			log.Warn("name file after the release", "file", f, "err", err)
			out = append(out, f)

			continue
		}

		log.Info("named obfuscated file after the release", "from", f, "to", to)
		out = append(out, to)
	}

	return out
}
