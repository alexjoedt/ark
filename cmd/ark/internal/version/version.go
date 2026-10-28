// Package version holds the build information set through -ldflags.
package version

import (
	"fmt"
	"runtime"
)

// Set at build time, see Taskfile.yml and .goreleaser.yaml.
var (
	ArkVersion = "v0.0.0-dev"
	BuildDate  = "unknown"
	CommitHash = "unknown"
)

// Info is the JSON shape of ark version --json.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
}

// GetVersion returns the version string alone.
func GetVersion() string { return ArkVersion }

// Get returns the build information.
func Get() Info {
	return Info{Version: ArkVersion, Commit: CommitHash, Date: BuildDate, Go: runtime.Version()}
}

// Line renders the one line ark version and ark --version print.
func Line() string {
	i := Get()
	return fmt.Sprintf("ark %s (%s, %s, %s)", i.Version, i.Go, i.Date, i.Commit)
}
