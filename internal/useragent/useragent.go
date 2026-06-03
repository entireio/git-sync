// Package useragent builds the User-Agent strings git-sync advertises to
// remote services. Two flavours: GoGit for git wire-protocol traffic
// (HTTP smart-protocol requests and the protocol-level "agent="
// capability), and Plain for non-git HTTP requests such as provider
// metadata APIs.
package useragent

import "github.com/go-git/go-git/v6/plumbing/protocol/capability"

// Version is the git-sync version advertised in User-Agent strings.
// CLI builds set this from versioninfo.Version; SDK consumers may
// overwrite it before issuing any requests if they want to identify a
// different version.
var Version = "dev"

// GoGit returns the User-Agent for git wire-protocol traffic. Format:
// "git-sync/<version> go-git/<go-git-version>". The go-git suffix is
// preserved because servers and operators commonly key off it.
func GoGit() string {
	return "git-sync/" + Version + " " + capability.DefaultAgent()
}

// Plain returns the User-Agent for non-git HTTP requests (e.g. provider
// REST APIs). Format: "git-sync/<version>".
func Plain() string {
	return "git-sync/" + Version
}
