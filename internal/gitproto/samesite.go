package gitproto

import (
	"net"
	"net/url"
	"strings"
)

// sameSite reports whether dest is close enough to orig that credentials the
// user supplied for orig may be sent to dest.
//
// This exists because git-sync follows /info/refs redirects (see
// FollowInfoRefsRedirect) and then targets the redirect host directly for the
// follow-up RPCs. Go's http.Client deliberately strips the Authorization header
// on a cross-host redirect; re-attaching an explicit token to a request aimed at
// whatever host the redirect named would undo that protection and hand the
// credential to that host. Any 30x reachable by an attacker — an open redirect
// on the real host, a hostile mirror, a MITM on a plain-http source — would be
// enough.
//
// The rule mirrors the one Go applies to redirect header retention: same host,
// or a subdomain of it. That keeps the case the flag exists for working
// (github.com redirecting to codeload.github.com, a hosting replica under the
// same domain) while refusing an unrelated host. It is deliberately stricter
// than Go's version in two ways:
//
//   - The scheme must match, so an https endpoint never leaks a token over
//     plaintext http.
//   - The port must match. Same host on a different port is a different
//     service, which on shared infrastructure can be a different tenant. An
//     explicitly written default port is the same port as an omitted one, so a
//     redirect that only spells out ":443" is still same-site.
//
// A literal IP address has no subdomains, so those must match exactly.
//
// Credentials obtained from the git credential helper are out of scope here:
// those are looked up keyed on the host actually being challenged, so they are
// already bound to their destination by construction.
func sameSite(orig, dest *url.URL) bool {
	if orig == nil || dest == nil {
		return false
	}
	if !strings.EqualFold(orig.Scheme, dest.Scheme) {
		return false
	}
	if effectivePort(orig) != effectivePort(dest) {
		return false
	}

	origHost := strings.ToLower(strings.TrimSuffix(orig.Hostname(), "."))
	destHost := strings.ToLower(strings.TrimSuffix(dest.Hostname(), "."))
	if origHost == "" || destHost == "" {
		return false
	}
	if origHost == destHost {
		return true
	}
	// An IP address is not a domain: "1.2.3.4" must not be treated as a parent
	// of "evil.1.2.3.4", nor "10.0.0.1" as a suffix match for anything.
	if net.ParseIP(origHost) != nil || net.ParseIP(destHost) != nil {
		return false
	}
	return strings.HasSuffix(destHost, "."+origHost)
}

// effectivePort returns the port u addresses, with a default port for the
// scheme normalized to the empty string so "https://host" and
// "https://host:443" compare equal. Load balancers and proxies do sometimes
// spell the default port out in a Location header, and treating that as a
// different site would withhold credentials from a genuinely same-origin
// replica.
func effectivePort(u *url.URL) string {
	port := u.Port()
	switch {
	case port == "":
		return ""
	case strings.EqualFold(u.Scheme, "https") && port == "443":
		return ""
	case strings.EqualFold(u.Scheme, "http") && port == "80":
		return ""
	default:
		return port
	}
}
