package library

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// browsePage bounds one BrowseFilesystem answer.
const browsePage = 200

// BrowseEntry is one folder in a BrowseFilesystem answer.
type BrowseEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// BrowseResult is the JSON payload of a BrowseFilesystem answer.
type BrowseResult struct {
	// Path is the folder listed, "" for the list of allowed roots.
	Path          string        `json:"path"`
	Entries       []BrowseEntry `json:"entries"`
	NextPageToken string        `json:"next_page_token,omitempty"`
}

// Browse lists the folders inside path for choosing a library root. With
// path "" it lists the allowed roots. Only folders are listed, and only
// inside allowed roots; a symlink that leads outside is refused like any
// other path. pageToken is the last name of the previous page.
func (p *Policy) Browse(path, pageToken string) ([]byte, error) {
	res := BrowseResult{Path: path, Entries: []BrowseEntry{}}

	if path == "" {
		for _, a := range p.allowed {
			if fi, err := os.Stat(a); err == nil && fi.IsDir() {
				res.Entries = append(res.Entries, BrowseEntry{Name: a, Path: a})
			}
		}

		return json.Marshal(res)
	}

	dir, err := p.Check(path)
	if err != nil {
		return nil, err
	}

	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot list %s: %w", ErrInvalid, path, unwrapPath(err))
	}

	var names []string

	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}

		// Symlinked folders are listed only when they stay inside.
		if !e.IsDir() {
			if e.Type()&os.ModeSymlink == 0 {
				continue
			}

			if _, err := p.Check(filepath.Join(path, e.Name())); err != nil {
				continue
			}
		}

		names = append(names, e.Name())
	}

	slices.Sort(names)

	start, _ := slices.BinarySearch(names, pageToken)
	if pageToken != "" && start < len(names) && names[start] == pageToken {
		start++
	}

	end := min(start+browsePage, len(names))

	for _, n := range names[start:end] {
		res.Entries = append(res.Entries, BrowseEntry{Name: n, Path: filepath.Join(path, n)})
	}

	if end < len(names) {
		res.NextPageToken = names[end-1]
	}

	return json.Marshal(res)
}
