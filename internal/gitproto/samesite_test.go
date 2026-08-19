package gitproto

import (
	"net/url"
	"testing"
)

func TestSameSite(t *testing.T) {
	tests := []struct {
		name string
		orig string
		dest string
		want bool
	}{
		// Same origin.
		{"identical", "https://github.com/o/r.git", "https://github.com/o/r.git", true},
		{"different path is irrelevant", "https://github.com/o/r.git", "https://github.com/other/path", true},
		{"host case is insensitive", "https://GitHub.com/o/r.git", "https://github.com/o/r.git", true},
		{"trailing dot is the same host", "https://github.com./o/r.git", "https://github.com/o/r.git", true},

		// Subdomains: the hosting-replica case the redirect flag exists for.
		{"subdomain of endpoint", "https://github.com/o/r.git", "https://codeload.github.com/o/r.git", true},
		{"deep subdomain", "https://example.com/r.git", "https://a.b.example.com/r.git", true},

		// Not same-site.
		{"unrelated host", "https://github.com/o/r.git", "https://evil.example/o/r.git", false},
		{"parent of endpoint is not a subdomain", "https://codeload.github.com/r.git", "https://github.com/r.git", false},
		// The classic near-miss: a registered domain that merely ends with the
		// endpoint's name. Suffix matching without the dot separator would pass.
		{"suffix without dot boundary", "https://github.com/r.git", "https://notgithub.com/r.git", false},
		{"attacker-controlled prefix domain", "https://github.com/r.git", "https://github.com.evil.example/r.git", false},

		// A downgrade would put the credential on the wire in plaintext.
		{"https to http", "https://github.com/r.git", "http://github.com/r.git", false},
		// An upgrade is allowed: the credential was already going to that host
		// in the clear, and GitHub and most reverse proxies redirect this way.
		{"http to https same host", "http://github.com/r.git", "https://github.com/r.git", true},
		{"http to https with default ports spelled out", "http://host:80/r.git", "https://host:443/r.git", true},
		{"http to https on a subdomain", "http://example.com/r.git", "https://replica.example.com/r.git", true},
		// The upgrade allowance must not smuggle in a host change.
		{"http to https different host", "http://github.com/r.git", "https://evil.example/r.git", false},
		{"http to https different port", "http://host/r.git", "https://host:8443/r.git", false},

		// Port must match: same host on another port is another service.
		{"different explicit port", "https://host:8443/r.git", "https://host:9443/r.git", false},
		{"explicit non-default vs implicit", "https://host/r.git", "https://host:8443/r.git", false},
		// A proxy or load balancer may spell the default port out in a Location
		// header; that is the same origin, not a different site.
		{"explicit default port https", "https://host/r.git", "https://host:443/r.git", true},
		{"implicit from explicit default https", "https://host:443/r.git", "https://host/r.git", true},
		{"explicit default port http", "http://host/r.git", "http://host:80/r.git", true},
		{"default port on a subdomain", "https://example.com/r.git", "https://replica.example.com:443/r.git", true},
		// The default port for the *other* scheme is not a default here.
		{"http default port on https", "https://host/r.git", "https://host:80/r.git", false},
		// This is the case that made the original proof-of-concept leak: Go's
		// own redirect rule compares hostnames only, so two ports on loopback
		// looked like the same host to it.
		{"loopback, different ports", "http://127.0.0.1:51966/r.git", "http://127.0.0.1:51965/r.git", false},

		// IP addresses have no subdomains.
		{"same ip", "https://10.0.0.1/r.git", "https://10.0.0.1/r.git", true},
		{"subdomain-looking ip", "https://1.2.3.4/r.git", "https://evil.1.2.3.4/r.git", false},
		{"ip suffix of hostname", "https://2.3.4/r.git", "https://1.2.3.4/r.git", false},
		{"ipv6 same", "https://[::1]/r.git", "https://[::1]/r.git", true},

		// Degenerate input.
		{"empty destination host", "https://github.com/r.git", "https:///r.git", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig, err := url.Parse(tt.orig)
			if err != nil {
				t.Fatalf("parse orig %q: %v", tt.orig, err)
			}
			dest, err := url.Parse(tt.dest)
			if err != nil {
				t.Fatalf("parse dest %q: %v", tt.dest, err)
			}
			if got := sameSite(orig, dest); got != tt.want {
				t.Errorf("sameSite(%q, %q) = %v, want %v", tt.orig, tt.dest, got, tt.want)
			}
		})
	}
}

func TestSameSiteNilURLs(t *testing.T) {
	u, err := url.Parse("https://github.com/r.git")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sameSite(nil, u) || sameSite(u, nil) || sameSite(nil, nil) {
		t.Error("a nil URL must never be treated as same-site")
	}
}
