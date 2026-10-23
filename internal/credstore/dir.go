package credstore

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// agentEntries are the names the agent itself creates in its data
// directory. A directory holding anything else is not dedicated to the
// agent, and its permissions are not ours to change.
var agentEntries = []string{
	FileName, "agent.lock", "agent.yaml", "downloads",
	"state.db", "state.db-wal", "state.db-shm", "state.db-journal",
}

func isAgentEntry(name string) bool {
	for _, e := range agentEntries {
		if name == e {
			return true
		}
	}

	return strings.HasPrefix(name, ".credentials-") && strings.HasSuffix(name, ".tmp") ||
		strings.HasPrefix(name, "state.db.") && strings.HasSuffix(name, ".bak")
}

// checkDedicated refuses a pre-existing directory that holds files the
// agent did not create, so pointing data_dir at a shared folder fails
// loudly instead of locking other users out of it.
func checkDedicated(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if !isAgentEntry(e.Name()) {
			return fmt.Errorf("data dir %s contains %q and is not dedicated to the agent; use an empty directory",
				dir, filepath.Join(dir, e.Name()))
		}
	}

	return nil
}

// OpenExisting returns a Store for reading credentials without creating,
// re-permissioning or otherwise touching the directory. `agent status`
// uses it so that inspecting a service's data dir never modifies it.
func OpenExisting(dir string, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}

	return &Store{dir: dir, prot: PlatformProtector(), log: log}
}
