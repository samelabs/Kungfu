package version

import (
	_ "embed"
	"runtime/debug"
	"strings"
)

// Version is set from VERSION file at compile time.
// Can be overridden with: go build -ldflags "-X kungfu.md/internal/version.Version=v1.3.0"

//go:embed VERSION
var versionFile string

// Version holds the application version string.
var Version = "v1.3.0"

func init() {
	v := strings.TrimSpace(versionFile)
	if v != "" {
		Version = v
	}
}

// Get returns the current version string.
func Get() string {
	return Version
}

// commit is the source revision the binary was built from, injected by
// the release build (scripts/deploy.sh):
//
//	go build -ldflags "-X kungfu.md/internal/version.commit=<sha>"
var commit string

// Commit returns the source revision of this binary: the injected value,
// else the revision the Go toolchain recorded (with "-dirty" for a
// modified tree), else "unknown".
func Commit() string {
	if commit != "" {
		return commit
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		var rev string
		var dirty bool
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if len(rev) > 12 {
			rev = rev[:12]
		}
		if rev != "" && dirty {
			rev += "-dirty"
		}
		if rev != "" {
			return rev
		}
	}
	return "unknown"
}
