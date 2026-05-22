package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"

	transporthttp "github.com/go-git/go-git/v6/plumbing/transport/http"
)

const defaultGitUsername = "git"

// Method authorizes outbound HTTP requests for a remote. It is satisfied
// by *transporthttp.BasicAuth and *transporthttp.TokenAuth, whose Authorizer
// methods replaced the Method interface that go-git removed in v6 alpha.2.
type Method interface {
	Authorizer(req *http.Request) error
}

// Endpoint holds the authentication-related fields for a remote.
type Endpoint struct {
	Username      string
	Token         string
	BearerToken   string
	SkipTLSVerify bool
}

// Resolve resolves the auth method for the given endpoint configuration.
// Order: explicit flags → Entire DB token → anonymous (with the git credential
// helper deferred until the server returns 401, matching git's own behaviour).
func Resolve(raw Endpoint, ep *url.URL) (Method, error) {
	if auth := explicitAuth(raw); auth != nil {
		return auth, nil
	}
	if !isHTTPEndpoint(ep) {
		return nil, nil //nolint:nilnil // nil signals no auth method found at this stage
	}
	if username, password, ok, err := LookupEntireDBCredential(raw, ep); err != nil {
		return nil, err // issue #7: surface refresh failure explicitly
	} else if ok {
		return &transporthttp.BasicAuth{Username: username, Password: password}, nil
	}
	// Note: we deliberately do not consult the git credential helper here.
	// Doing so eagerly would leak stored credentials to public repos that
	// don't require auth, and previously caused interactive prompts when
	// no helper had credentials (issue #63). The credential helper is now
	// consulted on demand when an HTTP request returns 401 — see
	// GitCredentialHelper, wired up by the HTTP connection layer.
	return nil, nil //nolint:nilnil // nil signals no auth method found at this stage
}

func explicitAuth(raw Endpoint) Method {
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

// newGitCredentialCmd builds the `git credential <op>` invocation used by
// GitCredentialCommand. Extracted so tests can inspect the command's
// environment without exec'ing git.
func newGitCredentialCmd(ctx context.Context, op, input string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", "credential", op)
	cmd.Stdin = strings.NewReader(input)
	// Disable git's interactive terminal prompt fallback. When no credential
	// helper has credentials for the host (e.g. a public repo on a server
	// the user has never authenticated against), git would otherwise drop
	// to an interactive username/password prompt on /dev/tty. git-sync is a
	// non-interactive tool — failing here lets us cleanly surface a 401
	// rather than block waiting for input. See issue #63.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// GitCredentialCommand invokes `git credential <op>` with the given input
// (in the git-credential text format). op is one of "fill", "approve", or
// "reject". Replaceable for testing.
var GitCredentialCommand = func(ctx context.Context, op, input string) ([]byte, error) {
	return newGitCredentialCmd(ctx, op, input).Output()
}

// GitCredentialHelper bridges Git's credential helper protocol to HTTP auth.
// It looks up credentials on demand (typically in response to a 401) and
// signals back to the helper whether the credentials worked.
//
// Implementations are best-effort: a missing or misbehaving helper must not
// fail the surrounding sync, only deny credentials. Errors from approve/reject
// are silently swallowed since those steps are advisory.
type GitCredentialHelper struct{}

// Lookup queries the git credential helper for credentials for ep. Returns
// ok=false if no credentials are available (so the caller can surface a
// clean 401 rather than block).
//
//nolint:unparam // err is always nil today but kept for the CredentialHelper interface.
func (GitCredentialHelper) Lookup(ctx context.Context, ep *url.URL) (username, password string, ok bool, err error) {
	if !isHTTPEndpoint(ep) {
		// git's credential helper protocol only knows about HTTP.
		return "", "", false, nil
	}
	input := credentialInput(ep, "", "")
	if input == "" {
		return "", "", false, nil
	}
	output, helperErr := GitCredentialCommand(ctx, "fill", input)
	if helperErr != nil {
		// Helper exited non-zero — typically means "no credentials found"
		// or "terminal prompts disabled" (when no helper has creds). Treat
		// both as "no credentials available" so the original 401 surfaces.
		return "", "", false, nil //nolint:nilerr // intentional: swallow helper failure as "no credentials"
	}
	values := parseCredentialOutput(output)
	password = values["password"]
	if password == "" {
		return "", "", false, nil
	}
	username = values["username"]
	if username == "" {
		if ep.User != nil && ep.User.Username() != "" {
			username = ep.User.Username()
		} else {
			username = defaultGitUsername
		}
	}
	return username, password, true, nil
}

// isHTTPEndpoint reports whether ep is a non-nil HTTP or HTTPS endpoint.
func isHTTPEndpoint(ep *url.URL) bool {
	return ep != nil && (ep.Scheme == "http" || ep.Scheme == "https")
}

// Approve tells the helper the credentials worked, so it can persist them.
// Best-effort: helper failures are swallowed.
func (GitCredentialHelper) Approve(ctx context.Context, ep *url.URL, username, password string) {
	input := credentialInput(ep, username, password)
	if input == "" {
		return
	}
	_, _ = GitCredentialCommand(ctx, "approve", input) //nolint:errcheck // best-effort signal
}

// Reject tells the helper the credentials failed, so it can forget them.
// Best-effort: helper failures are swallowed.
func (GitCredentialHelper) Reject(ctx context.Context, ep *url.URL, username, password string) {
	input := credentialInput(ep, username, password)
	if input == "" {
		return
	}
	_, _ = GitCredentialCommand(ctx, "reject", input) //nolint:errcheck // best-effort signal
}

// credentialInput builds a git-credential format request body for the given
// endpoint. When username/password are set, they are appended (for use with
// `git credential approve`/`reject`). When both are empty, the result is a
// query body suitable for `git credential fill`. Explicit username overrides
// any user embedded in the endpoint URL.
func credentialInput(ep *url.URL, username, password string) string {
	if ep == nil || ep.Hostname() == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "protocol=%s\nhost=%s\n", ep.Scheme, ep.Hostname())
	if path := strings.TrimPrefix(ep.Path, "/"); path != "" {
		fmt.Fprintf(&b, "path=%s\n", path)
	}
	user := username
	if user == "" && ep.User != nil {
		user = ep.User.Username()
	}
	if user != "" {
		fmt.Fprintf(&b, "username=%s\n", user)
	}
	if password != "" {
		fmt.Fprintf(&b, "password=%s\n", password)
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
