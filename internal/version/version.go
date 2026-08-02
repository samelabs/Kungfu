package version

import _ "embed"

// Version is set from VERSION file at compile time.
// Can be overridden with: go build -ldflags "-X kungfu.md/internal/version.Version=v1.2.0"

//go:embed VERSION
var versionFile string

// Version holds the application version string.
var Version = "v1.2.0"

func init() {
	v := versionFile
	if v != "" {
		Version = v
	}
}

// Get returns the current version string.
func Get() string {
	return Version
}
