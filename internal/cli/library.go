package cli

import (
	"github.com/getnabbed/agent/internal/config"
	"github.com/getnabbed/agent/internal/download"
	"github.com/getnabbed/agent/internal/library"
	"github.com/getnabbed/agent/internal/store"
)

// newLibrary builds the library manager from the local policy in the
// config, which the control plane cannot change.
func newLibrary(e *env, st *store.Store) (*library.Manager, error) {
	fileMode, err := config.Mode("library.file_mode", e.cfg.Library.FileMode)
	if err != nil {
		return nil, err
	}

	dirMode, err := config.Mode("library.dir_mode", e.cfg.Library.DirMode)
	if err != nil {
		return nil, err
	}

	policy, warnings, err := library.NewPolicy(e.cfg.Library.AllowedRoots, fileMode, dirMode)
	if err != nil {
		return nil, err
	}

	for _, w := range warnings {
		e.log.Warn(w)
	}

	if len(policy.Allowed()) == 0 {
		e.log.Info("no library folders are allowed; imports are refused until library.allowed_roots or NABBED_LIBRARY_ROOTS is set")
	} else {
		e.log.Info("library folders allowed", "allowed_roots", policy.Allowed())
	}

	return library.New(st, policy, e.log), nil
}

// CapLibraryImport says the agent imports into library roots. It is only
// advertised once the host owner allowed a folder: the cloud re-sends the
// library configuration when an agent gains it, so allowing the first
// folder and restarting brings the user's roots in.
const CapLibraryImport = "library.import"

func capabilities(downloads *download.Manager, lib *library.Manager) func() []string {
	return func() []string {
		caps := downloads.Capabilities()
		if len(lib.Policy().Allowed()) > 0 {
			caps = append(caps, CapLibraryImport)
		}

		return caps
	}
}
