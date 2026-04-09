package syncer

import (
	"context"
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
