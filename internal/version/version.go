// Package version exposes build metadata for the agent binary.
//
// Version, Commit and Date are overridden at build time via -ldflags
// (see the Makefile and .goreleaser.yaml). When built with plain `go build`
// or `go install`, the values fall back to module build info where possible.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

var (
	// Version is the semantic version of the build, e.g. "v1.2.3".
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = ""
	// Date is the build timestamp in RFC 3339 format.
	Date = ""
)

// withV prefixes "v" to a version that starts with a digit, such as
// "0.1.0". Others, such as "dev" or "v0.1.0", are kept.
func withV(v string) string {
	if v != "" && v[0] >= '0' && v[0] <= '9' {
		return "v" + v
	}

	return v
}

func init() {
	// GoReleaser passes the version without its "v"; the cloud shows it
	// as sent, so every numeric version gets one.
	defer func() { Version = withV(Version) }()

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}

	if Version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		Version = info.Main.Version
	}

	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if Commit == "" {
				Commit = s.Value
			}
		case "vcs.time":
			if Date == "" {
				Date = s.Value
			}
		}
	}
}

// String returns a single-line, human-readable description of the build.
func String() string {
	s := Version
	if Commit != "" {
		c := Commit
		if len(c) > 12 {
			c = c[:12]
		}

		s += " (" + c + ")"
	}

	if Date != "" {
		s += " built " + Date
	}

	return fmt.Sprintf("%s %s/%s %s", s, runtime.GOOS, runtime.GOARCH, runtime.Version())
}
