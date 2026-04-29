package syncer

import (
	"testing"

	bstrap "entire.io/entire/gitsync/internal/strategy/bootstrap"
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
