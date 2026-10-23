// Package paths resolves where the agent keeps its state on each platform.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

// EnvDataDir overrides the data directory. Container images set it.
const EnvDataDir = "NABBED_AGENT_DATA_DIR"

// DefaultDataDir returns the data directory for the current platform and
// account:
//
//	Linux, root      /var/lib/nabbed-agent
//	Linux, user      $XDG_STATE_HOME/nabbed-agent (~/.local/state/nabbed-agent)
//	macOS, root      /Library/Application Support/Nabbed Agent
//	macOS, user      ~/Library/Application Support/Nabbed Agent
//	Windows          %LOCALAPPDATA%\Nabbed\Agent
//
// Windows services that should share state across accounts pass
// --data-dir %ProgramData%\Nabbed\Agent explicitly.
func DefaultDataDir() string {
	if d := os.Getenv(EnvDataDir); d != "" {
		return d
	}

	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "Nabbed", "Agent")
		}

		return filepath.Join(home, "AppData", "Local", "Nabbed", "Agent")
	case "darwin":
		if os.Geteuid() == 0 {
			return "/Library/Application Support/Nabbed Agent"
		}

		return filepath.Join(home, "Library", "Application Support", "Nabbed Agent")
	default:
		if os.Geteuid() == 0 {
			return "/var/lib/nabbed-agent"
		}

		if d := os.Getenv("XDG_STATE_HOME"); d != "" {
			return filepath.Join(d, "nabbed-agent")
		}

		return filepath.Join(home, ".local", "state", "nabbed-agent")
	}
}
