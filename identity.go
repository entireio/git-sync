package gitsync

import (
	"strings"

	"entire.io/entire/git-sync/internal/useragent"
)

// SetIdentity names the embedding service in every request git-sync makes:
// the HTTP User-Agent and the git-protocol "agent=" capability become
// "<service>/<version> git-sync/<git-sync-version> go-git/<go-git-version>"
// (non-git provider requests carry the same string without the go-git
// token). git-sync's own version is reported regardless; SetIdentity adds
// the service's identity in front so server operators can attribute traffic
// to the service, not just the library.
//
// The identity is process-wide and read on every request: call SetIdentity
// once at startup, before issuing requests from any Client. An empty
// service removes the identity; an empty version advertises the bare
// service name. Whitespace within either value is collapsed to "-" so the
// result stays a single User-Agent product token.
func SetIdentity(service, version string) {
	service = sanitizeToken(service)
	if service == "" {
		useragent.Identity = ""
		return
	}
	if version = sanitizeToken(version); version != "" {
		service += "/" + version
	}
	useragent.Identity = service
}

func sanitizeToken(s string) string {
	return strings.Join(strings.Fields(s), "-")
}
