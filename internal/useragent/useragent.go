// Package useragent builds the User-Agent strings git-sync advertises to
// remote services. Two flavours: GoGit for git wire-protocol traffic
// (HTTP smart-protocol requests and the protocol-level "agent="
// capability), and Plain for non-git HTTP requests such as provider
// metadata APIs.
package useragent

import (
	"runtime/debug"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
)

// Identity is an optional "<service>/<version>" product token advertised
// ahead of git-sync's own token, so servers can attribute traffic to the
// embedding service rather than to the library. Empty means no prefix.
// Set it via gitsync.SetIdentity — this package is internal.
var Identity = ""

// Version is the git-sync version advertised in User-Agent strings. It
// defaults to the git-sync module version recorded in the running binary's
// build info ("dev" when that is unavailable, e.g. a plain `go build` of
// this repo). The CLI overrides it with the goreleaser-stamped version.
// Embedders don't touch this — they identify themselves with
// gitsync.SetIdentity, which prefixes the User-Agent instead of masking
// git-sync's own version.
var Version = moduleVersion()

// fallbackVersion is advertised when the build carries no usable version
// (a plain `go build` of this repo, or missing build info).
const fallbackVersion = "dev"

// GoGit returns the User-Agent for git wire-protocol traffic. Format:
// "[<identity> ]git-sync/<version> go-git/<go-git-version>". The go-git
// suffix is preserved because servers and operators commonly key off it.
func GoGit() string {
	return identityPrefix() + "git-sync/" + Version + " " + capability.DefaultAgent()
}

// Plain returns the User-Agent for non-git HTTP requests (e.g. provider
// REST APIs). Format: "[<identity> ]git-sync/<version>".
func Plain() string {
	return identityPrefix() + "git-sync/" + Version
}

func identityPrefix() string {
	if Identity == "" {
		return ""
	}
	return Identity + " "
}

// moduleVersion resolves git-sync's own version from the binary's embedded
// build info: the dependency entry when git-sync is embedded as a module,
// or the main-module version when the CLI was `go install`ed at a version.
// Locally built binaries carry no usable version ("(devel)") → "dev".
func moduleVersion() string {
	const modulePath = "entire.io/entire/git-sync"
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return fallbackVersion
	}
	mods := append([]*debug.Module{&bi.Main}, bi.Deps...)
	for _, m := range mods {
		if m.Path != modulePath {
			continue
		}
		if m.Replace != nil {
			m = m.Replace
		}
		if v := strings.TrimPrefix(m.Version, "v"); v != "" && v != "(devel)" {
			return v
		}
	}
	return fallbackVersion
}
