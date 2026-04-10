package syncer

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
	transporthttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

func TestResolveAuthMethodPrefersExplicitToken(t *testing.T) {
	ep, err := transport.NewEndpoint("https://github.com/entireio/cli.git")
	if err != nil {
		t.Fatalf("new endpoint: %v", err)
	}

	originalFill := gitCredentialFillCommand
	t.Cleanup(func() {
		gitCredentialFillCommand = originalFill
	})
	gitCredentialFillCommand = func(ctx context.Context, input string) ([]byte, error) {
		t.Fatalf("unexpected git credential fill call with input %q", input)
		return nil, nil
	}

	auth, err := resolveAuthMethod(Endpoint{
		Username: "git",
		Token:    "explicit-token",
	}, ep)
	if err != nil {
		t.Fatalf("resolve auth: %v", err)
	}

	basic, ok := auth.(*transporthttp.BasicAuth)
	if !ok {
		t.Fatalf("expected basic auth, got %T", auth)
	}
	if basic.Username != "git" || basic.Password != "explicit-token" {
		t.Fatalf("unexpected auth: %+v", basic)
	}
}

func TestNewTransportConnSkipTLSVerify(t *testing.T) {
	conn, err := newTransportConn(Endpoint{
		URL:           "https://example.com/repo.git",
		SkipTLSVerify: true,
	}, "source", newStats(false))
	if err != nil {
		t.Fatalf("new transport conn: %v", err)
	}

	roundTripper, ok := conn.http.Transport.(*countingRoundTripper)
	if !ok {
		t.Fatalf("expected countingRoundTripper, got %T", conn.http.Transport)
	}
	base, ok := roundTripper.base.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport base, got %T", roundTripper.base)
	}
	if base.TLSClientConfig == nil || !base.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("expected InsecureSkipVerify transport, got %#v", base.TLSClientConfig)
	}
}
