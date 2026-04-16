package auth

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/transport"
	transporthttp "github.com/go-git/go-git/v6/plumbing/transport/http"
)

const defaultGitUsername = "git"

// Endpoint holds the authentication-related fields for a remote.
type Endpoint struct {
	Username      string
	Token         string
	BearerToken   string
	SkipTLSVerify bool
}

// Resolve resolves the auth method for the given endpoint configuration.
// Order: explicit flags → Entire DB token → git credential helper → anonymous.
func Resolve(raw Endpoint, ep *transport.Endpoint) (transport.AuthMethod, error) {
	if auth := explicitAuth(raw); auth != nil {
		return auth, nil
	}
	if ep == nil {
		return nil, nil //nolint:nilnil // nil signals no auth method found at this stage
	}
	if ep.Scheme != "http" && ep.Scheme != "https" {
		return nil, nil //nolint:nilnil // nil signals no auth method found at this stage
	}
	if username, password, ok, err := LookupEntireDBCredential(raw, ep); err != nil {
		return nil, err // issue #7: surface refresh failure explicitly
	} else if ok {
		return &transporthttp.BasicAuth{Username: username, Password: password}, nil
	}
	if username, password, ok := lookupGitCredential(ep); ok {
		return &transporthttp.BasicAuth{Username: username, Password: password}, nil
	}
	return nil, nil //nolint:nilnil // nil signals no auth method found at this stage
}

func explicitAuth(raw Endpoint) transport.AuthMethod {
	if raw.BearerToken != "" {
		return &transporthttp.TokenAuth{Token: raw.BearerToken}
	}
	if raw.Token != "" {
		username := raw.Username
		if username == "" {
			username = defaultGitUsername
		}
		return &transporthttp.BasicAuth{Username: username, Password: raw.Token}
	}
	return nil
}

// GitCredentialFillCommand is replaceable for testing.
var GitCredentialFillCommand = func(ctx context.Context, input string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "credential", "fill")
	cmd.Stdin = strings.NewReader(input)
	return cmd.Output()
}

func lookupGitCredential(ep *transport.Endpoint) (string, string, bool) {
	input := credentialFillInput(ep)
	if input == "" {
		return "", "", false
	}
	output, err := GitCredentialFillCommand(context.Background(), input)
	if err != nil {
		return "", "", false
	}
	values := parseCredentialOutput(output)
	password := values["password"]
	if password == "" {
		return "", "", false
	}
	username := values["username"]
	if username == "" {
		if ep.User != nil && ep.User.Username() != "" {
			username = ep.User.Username()
		} else {
			username = defaultGitUsername
		}
	}
	return username, password, true
}

func credentialFillInput(ep *transport.Endpoint) string {
	if ep == nil || ep.Hostname() == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "protocol=%s\nhost=%s\n", ep.Scheme, ep.Hostname())
	if path := strings.TrimPrefix(ep.Path, "/"); path != "" {
		fmt.Fprintf(&b, "path=%s\n", path)
	}
	if ep.User != nil && ep.User.Username() != "" {
		fmt.Fprintf(&b, "username=%s\n", ep.User.Username())
	}
	b.WriteString("\n")
	return b.String()
}

func parseCredentialOutput(output []byte) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			values[k] = v
		}
	}
	return values
}
