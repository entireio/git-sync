package redact

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no credentials", "https://github.com/o/r.git", "https://github.com/o/r.git"},
		{"user and password", "https://user:secret@github.com/o/r.git", "https://github.com/o/r.git"},
		// Token-as-username: url.URL.Redacted() would leave this intact
		// (no password component), so we must strip the whole userinfo.
		{"token as username", "https://ghp_abc123@github.com/o/r.git", "https://github.com/o/r.git"},
		{"x-access-token form", "https://x-access-token:ghp_abc@github.com/o/r.git", "https://github.com/o/r.git"},
		{"empty userinfo", "https://@github.com/o/r.git", "https://github.com/o/r.git"},
		{"port and query preserved", "https://u:p@host:8443/r.git?a=b", "https://host:8443/r.git?a=b"},
		{"ssh scp-style has no userinfo to strip", "ssh://git@host/r.git", "ssh://host/r.git"},
		{"empty input", "", ""},
		// Unparseable input must not echo whatever credentials it carried.
		{"unparseable", "https://tok@ho%zz/r.git", placeholder},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := URL(tt.in); got != tt.want {
				t.Errorf("URL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// No secret must survive redaction in any form, including as a substring of a
// longer field. Guards against a future change that masks only the password.
func TestURLLeavesNoSecretSubstring(t *testing.T) {
	const secret = "s3cr3t-token-value"
	for _, raw := range []string{
		"https://user:" + secret + "@host/r.git",
		"https://" + secret + "@host/r.git",
		"http://" + secret + ":x@host:9000/r.git?ref=main#frag",
	} {
		if got := URL(raw); strings.Contains(got, secret) {
			t.Errorf("URL(%q) = %q, still contains the secret", raw, got)
		}
	}
}

func TestEndpointDoesNotMutateInput(t *testing.T) {
	u, err := url.Parse("https://user:secret@host/r.git")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := Endpoint(u), "https://host/r.git"; got != want {
		t.Errorf("Endpoint() = %q, want %q", got, want)
	}
	// The caller's URL still authenticates — redaction is display-only.
	if u.User == nil || u.User.String() != "user:secret" {
		t.Errorf("Endpoint mutated its input: userinfo is now %v", u.User)
	}
}

func TestEndpointNil(t *testing.T) {
	if got := Endpoint(nil); got != placeholder {
		t.Errorf("Endpoint(nil) = %q, want %q", got, placeholder)
	}
}

func TestURLError(t *testing.T) {
	// Held in a slice rather than written inline: a literal here trips the
	// linter's URL-validity check on a string we deliberately malform.
	malformed := []string{"https://tok@ho%zz/r.git"}
	_, parseErr := url.Parse(malformed[0])
	if parseErr == nil {
		t.Fatal("expected a parse error to build the test on")
	}
	if strings.Contains(parseErr.Error(), "tok") {
		// Confirms the premise: url.Parse really does embed the raw URL.
		t.Logf("premise holds, raw error carries the URL: %v", parseErr)
	}

	got := URLError(parseErr)
	if strings.Contains(got.Error(), "tok") {
		t.Errorf("URLError kept the raw URL: %v", got)
	}

	// Wrapped errors are unwrapped through.
	if got := URLError(fmt.Errorf("outer: %w", parseErr)); strings.Contains(got.Error(), "tok") {
		t.Errorf("URLError kept the raw URL through a wrap: %v", got)
	}

	// Non-url.Error values pass through unchanged.
	plain := errors.New("some other failure")
	if got := URLError(plain); !errors.Is(got, plain) {
		t.Errorf("URLError(%v) = %v, want the original error", plain, got)
	}
	if got := URLError(nil); got != nil {
		t.Errorf("URLError(nil) = %v, want nil", got)
	}
}
