// Package buildinfo holds version metadata stamped into the binaries at build
// time via -ldflags. It has no dependencies so both the server and the agent
// can import it cheaply.
package buildinfo

import (
	"fmt"
	"runtime"
)

// These are overridden at build time, e.g.
//
//	go build -ldflags "-X rewardd/internal/buildinfo.Version=1.2.3 \
//	  -X rewardd/internal/buildinfo.Commit=$(git rev-parse --short HEAD) \
//	  -X rewardd/internal/buildinfo.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
var (
	// Version is the semantic version of the build (e.g. "1.2.3"). "dev" for
	// local/un-stamped builds.
	Version = "dev"
	// Commit is the short git SHA the build was cut from.
	Commit = "none"
	// Date is the RFC3339 UTC build timestamp.
	Date = "unknown"
)

// Info is the machine-readable version payload returned by the /version route.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// Get returns the current build metadata.
func Get() Info {
	return Info{
		Version: Version,
		Commit:  Commit,
		Date:    Date,
		Go:      runtime.Version(),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}
}

// String renders a one-line human-readable version banner.
func String() string {
	return fmt.Sprintf("rewardd %s (commit %s, built %s, %s %s/%s)",
		Version, Commit, Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
