// Package paths resolves where the agent keeps its state on each platform.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

// DefaultDataDir returns the data directory for the current platform and
// account:
//
//	Linux, root      /var/lib/nabbed-agent
//	Linux, user      $XDG_STATE_HOME/nabbed-agent (~/.local/state/nabbed-agent)
//	macOS, root      /Library/Application Support/Nabbed Agent
//	macOS, user      ~/Library/Application Support/Nabbed Agent
//	Windows          %LOCALAPPDATA%\Nabbed\Agent
//
// Windows services that should share state across accounts set data_dir to
// %ProgramData%\Nabbed\Agent. NABBED_DATA_DIR overrides it (see config).
func DefaultDataDir() string { return platformDataDir() }

func platformDataDir() string {
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

// DefaultConfigFile returns where the optional config file is looked for
// when neither --config nor NABBED_CONFIG is set:
//
//	Linux, root      /etc/nabbed-agent/agent.yaml
//	Linux, user      $XDG_CONFIG_HOME/nabbed-agent/agent.yaml (~/.config/...)
//	macOS            <data dir>/agent.yaml
//	Windows          %APPDATA%\Nabbed\Agent\agent.yaml
//
// The container image sets NABBED_CONFIG=/config/agent.yaml.
func DefaultConfigFile() string {
	home, _ := os.UserHomeDir()

	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("APPDATA"); d != "" {
			return filepath.Join(d, "Nabbed", "Agent", "agent.yaml")
		}

		return filepath.Join(home, "AppData", "Roaming", "Nabbed", "Agent", "agent.yaml")
	case "darwin":
		return filepath.Join(platformDataDir(), "agent.yaml")
	default:
		if os.Geteuid() == 0 {
			return "/etc/nabbed-agent/agent.yaml"
		}

		if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
			return filepath.Join(d, "nabbed-agent", "agent.yaml")
		}

		return filepath.Join(home, ".config", "nabbed-agent", "agent.yaml")
	}
}
