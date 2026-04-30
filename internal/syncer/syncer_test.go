package syncer

import (
	"context"
	"strings"
	"testing"

	bstrap "entire.io/entire/git-sync/internal/strategy/bootstrap"
)

func TestGitHubOwnerRepo(t *testing.T) {
	stats := newStats(false)
	conn, err := newConn(Endpoint{URL: "https://github.com/torvalds/linux.git"}, "source", stats, nil)
	if err != nil {
		t.Fatalf("new conn: %v", err)
	}
	owner, repo, ok := bstrap.GitHubOwnerRepo(conn)
	if !ok {
		t.Fatalf("expected github owner/repo match")
	}
	if owner != "torvalds" || repo != "linux" {
		t.Fatalf("unexpected owner/repo: %s/%s", owner, repo)
	}
}

func TestGitHubOwnerRepoRejectsNonGitHubSource(t *testing.T) {
	stats := newStats(false)
	conn, err := newConn(Endpoint{URL: "https://gitlab.com/group/project.git"}, "source", stats, nil)
	if err != nil {
		t.Fatalf("new conn: %v", err)
	}
	if _, _, ok := bstrap.GitHubOwnerRepo(conn); ok {
		t.Fatalf("expected non-github source to be rejected")
	}
}

// TestPublicAPIRejectsIdenticalSourceAndTarget covers every entry point that
// touches both endpoints: same URL on source and target must fail before any
// network I/O. Probe with no target and Fetch are intentionally excluded
// because they do not have a target.
func TestPublicAPIRejectsIdenticalSourceAndTarget(t *testing.T) {
	t.Parallel()

	const url = "https://example.com/repo.git"
	cfg := Config{
		Source: Endpoint{URL: url},
		Target: Endpoint{URL: url},
	}

	tests := []struct {
		name string
		call func() error
	}{
		{name: "Run", call: func() error {
			_, err := Run(context.Background(), cfg)
			return err
		}},
		{name: "Bootstrap", call: func() error {
			_, err := Bootstrap(context.Background(), cfg)
			return err
		}},
		{name: "Probe with target", call: func() error {
			_, err := Probe(context.Background(), cfg)
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.call()
			if err == nil {
				t.Fatalf("%s with identical source/target URLs returned nil error", tt.name)
			}
			if !strings.Contains(err.Error(), "source and target must not be the same repository") {
				t.Fatalf("%s error = %v, want same-repository rejection", tt.name, err)
			}
		})
	}
}

// TestProbeWithoutTargetIgnoresEndpointEqualityCheck guards against a regression
// where the source-vs-target check would fire for a probe that never set a
// target — there is nothing to compare against.
func TestProbeWithoutTargetIgnoresEndpointEqualityCheck(t *testing.T) {
	t.Parallel()

	cfg := Config{Source: Endpoint{URL: "https://example.com/repo.git"}}
	_, err := Probe(context.Background(), cfg)
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "source and target must not be the same repository") {
		t.Fatalf("Probe without target tripped same-repository check: %v", err)
	}
}

// TestNewConn_PropagatesFollowInfoRefsRedirect proves the plumbing from
// Endpoint → gitproto.Conn is in place. Without this the flag on
// Endpoint is dead config.
func TestNewConn_PropagatesFollowInfoRefsRedirect(t *testing.T) {
	stats := newStats(false)

	off, err := newConn(Endpoint{URL: "https://node.example/repo.git"}, "target", stats, nil)
	if err != nil {
		t.Fatalf("new conn (off): %v", err)
	}
	if off.FollowInfoRefsRedirect {
		t.Error("FollowInfoRefsRedirect should default to false")
	}

	on, err := newConn(Endpoint{URL: "https://node.example/repo.git", FollowInfoRefsRedirect: true}, "target", stats, nil)
	if err != nil {
		t.Fatalf("new conn (on): %v", err)
	}
	if !on.FollowInfoRefsRedirect {
		t.Error("FollowInfoRefsRedirect was not propagated from Endpoint to Conn")
	}
}
