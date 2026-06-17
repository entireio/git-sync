package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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
func Resolve(ctx context.Context, raw Endpoint, ep *url.URL) (Method, error) {
	if auth := explicitAuth(raw); auth != nil {
		return auth, nil
	}
	if !isHTTPEndpoint(ep) {
		return nil, nil //nolint:nilnil // nil signals no auth method found at this stage
	}
	if username, password, ok, err := LookupEntireDBCredential(ctx, raw, ep); err != nil {
		return nil, err // issue #7: surface refresh failure explicitly
	} else if ok {
		return &transporthttp.BasicAuth{Username: username, Password: password}, nil
	}
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

// CredentialOp identifies a `git credential` subcommand.
type CredentialOp string

const (
	CredentialOpFill    CredentialOp = "fill"
	CredentialOpApprove CredentialOp = "approve"
	CredentialOpReject  CredentialOp = "reject"
)

// newGitCredentialCmd builds the `git credential <op>` invocation. Extracted
// so tests can inspect the command's environment without exec'ing git.
//
// We inherit the parent environment unchanged — in particular, we do NOT
// force GIT_TERMINAL_PROMPT=0. The original #63 symptom (interactive prompt
// on a public-and-anonymous repo) is already prevented by Resolve no longer
// invoking the helper proactively: with no 401 there's no Lookup, no
// `git credential fill`, and so no prompt. Once the server actually
// challenges with a 401, prompting is the right behaviour when there's a
// terminal and a helper that has no entry for the host yet — same as
// vanilla `git push`. Non-interactive callers (CI, daemons, the syncer
// background loop) set GIT_TERMINAL_PROMPT=0 in their own environment the
// same way they would for plain git, and we pass that through.
func newGitCredentialCmd(ctx context.Context, op CredentialOp, input string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", "credential", string(op))
	cmd.Stdin = strings.NewReader(input)
	return cmd
}

// GitCredentialCommand invokes `git credential <op>` with the given input
// (git-credential text format). Replaceable for testing.
var GitCredentialCommand = func(ctx context.Context, op CredentialOp, input string) ([]byte, error) {
	return newGitCredentialCmd(ctx, op, input).Output()
}

// GitCredentialHelper bridges Git's credential helper protocol to HTTP auth.
// Best-effort: a missing or misbehaving helper denies credentials rather
// than failing the surrounding sync.
type GitCredentialHelper struct{}

// Lookup queries the git credential helper for credentials for ep. Returns
// ok=false if no credentials are available so the caller can surface a
// clean 401. A non-nil error means the lookup itself couldn't complete
// (e.g. the context was cancelled) and the caller should surface that
// rather than fall back to the original 401.
//
// Lookup may block on user interaction when the helper falls through to a
// terminal prompt (vanilla `git credential fill` behaviour). Callers that
// must not block should set GIT_TERMINAL_PROMPT=0 in the process
// environment; the credential subprocess inherits it. See
// newGitCredentialCmd for the rationale on not forcing that ourselves.
func (GitCredentialHelper) Lookup(ctx context.Context, ep *url.URL) (username, password string, ok bool, err error) {
	if !isHTTPEndpoint(ep) {
		return "", "", false, nil
	}
	input := credentialInput(ep, "", "")
	if input == "" {
		return "", "", false, nil
	}
	output, helperErr := GitCredentialCommand(ctx, CredentialOpFill, input)
	if helperErr != nil {
		// A cancelled or timed-out context kills the `git credential fill`
		// subprocess; surface that as the real cause instead of masking it
		// as "no credentials available", which would report the original
		// HTTP 401 rather than context.Canceled/DeadlineExceeded.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", "", false, fmt.Errorf("git credential fill: %w", ctxErr)
		}
		return "", "", false, nil
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

// Approve tells the helper the credentials worked.
func (h GitCredentialHelper) Approve(ctx context.Context, ep *url.URL, username, password string) {
	h.signal(ctx, CredentialOpApprove, ep, username, password)
}

// Reject tells the helper the credentials failed.
func (h GitCredentialHelper) Reject(ctx context.Context, ep *url.URL, username, password string) {
	h.signal(ctx, CredentialOpReject, ep, username, password)
}

func (GitCredentialHelper) signal(ctx context.Context, op CredentialOp, ep *url.URL, username, password string) {
	input := credentialInput(ep, username, password)
	if input == "" {
		return
	}
	_, _ = GitCredentialCommand(ctx, op, input) //nolint:errcheck // advisory signal; helper failures swallowed
}

func isHTTPEndpoint(ep *url.URL) bool {
	return ep != nil && (ep.Scheme == "http" || ep.Scheme == "https")
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
