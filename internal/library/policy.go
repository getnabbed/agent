// Package library manages library roots, the folders imports write into,
// and the local policy that bounds them. The control plane chooses roots,
// but only under folders the host owner allowed in the agent's own
// configuration, which the cloud cannot change. With none allowed, every
// root is refused.
package library

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var (
	// ErrNotAllowed means a path is outside every allowed root.
	ErrNotAllowed = errors.New("folder is not allowed on this agent")
	// ErrInvalid means a request is malformed or the folder is unusable.
	ErrInvalid = errors.New("invalid library request")
	// ErrInUse means a download still targets the root.
	ErrInUse = errors.New("library root is in use")
	// ErrTemplateUnsafe means a naming template could produce a path
	// outside its root.
	ErrTemplateUnsafe = errors.New("naming template is unsafe")
	// ErrLimit means the agent holds the maximum number of roots.
	ErrLimit = errors.New("too many library roots")
)

// howTo is appended to refusals so the user knows what to change.
const howTo = "add it, or a folder above it, to library.allowed_roots in the agent's config or NABBED_LIBRARY_ROOTS, then restart the agent"

// Policy is the host owner's local limit on where imports may write.
type Policy struct {
	allowed  []string
	FileMode os.FileMode
	DirMode  os.FileMode
}

// NewPolicy returns a policy allowing paths under allowed. Each entry must
// be absolute. Entries that do not exist yet are kept, since a mount may
// appear later, and returned as warnings.
func NewPolicy(allowed []string, fileMode, dirMode os.FileMode) (*Policy, []string, error) {
	p := &Policy{FileMode: fileMode, DirMode: dirMode}

	var warnings []string

	for _, a := range allowed {
		if a == "" {
			continue
		}

		if !filepath.IsAbs(a) {
			return nil, nil, fmt.Errorf("library.allowed_roots: %q is not an absolute path", a)
		}

		if fi, err := os.Stat(a); err != nil || !fi.IsDir() {
			warnings = append(warnings, fmt.Sprintf("library.allowed_roots: %s is not a folder yet", a))
		}

		p.allowed = append(p.allowed, filepath.Clean(a))
	}

	return p, warnings, nil
}

// Allowed lists the allowed roots as configured.
func (p *Policy) Allowed() []string { return append([]string(nil), p.allowed...) }

// Check resolves path, following symlinks, and returns it if it is an
// existing folder inside an allowed root. Comparing with os.SameFile on
// each parent handles symlinked mounts and case-insensitive file systems.
func (p *Policy) Check(path string) (string, error) {
	if len(p.allowed) == 0 {
		return "", fmt.Errorf("%w: no library folders are allowed yet; %s", ErrNotAllowed, howTo)
	}

	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: %q is not an absolute path", ErrInvalid, path)
	}

	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("%w: %s cannot be opened: %w", ErrInvalid, path, unwrapPath(err))
	}

	fi, err := os.Stat(resolved)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%w: %s is not a folder", ErrInvalid, path)
	}

	if !p.inside(resolved) {
		return "", fmt.Errorf("%w: %s; %s", ErrNotAllowed, path, howTo)
	}

	return resolved, nil
}

func (p *Policy) inside(resolved string) bool {
	var roots []os.FileInfo

	for _, a := range p.allowed {
		if fi, err := os.Stat(a); err == nil && fi.IsDir() {
			roots = append(roots, fi)
		}
	}

	for dir := resolved; ; dir = filepath.Dir(dir) {
		if fi, err := os.Stat(dir); err == nil {
			for _, r := range roots {
				if os.SameFile(fi, r) {
					return true
				}
			}
		}

		if filepath.Dir(dir) == dir {
			return false
		}
	}
}

// Writable reports whether the agent can create files in dir.
func Writable(dir string) error {
	f, err := os.CreateTemp(dir, ".nabbed-write-test-*")
	if err != nil {
		return fmt.Errorf("%w: the agent cannot write to %s: %w", ErrInvalid, dir, unwrapPath(err))
	}

	_ = f.Close()
	_ = os.Remove(f.Name())

	return nil
}

// unwrapPath drops the path from a *PathError, which callers already name.
func unwrapPath(err error) error {
	if pe, ok := errors.AsType[*os.PathError](err); ok {
		return pe.Err
	}

	return err
}
