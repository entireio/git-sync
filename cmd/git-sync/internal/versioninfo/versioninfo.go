package versioninfo

import "fmt"

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String renders the one-line build description shared by `git-sync version`
// and `git-sync --version`.
func String() string {
	return fmt.Sprintf("git-sync %s (commit %s, built %s)", Version, Commit, Date)
}
