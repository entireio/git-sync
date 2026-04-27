package syncer

import (
	"net/http"
	"net/url"
	"testing"

	bstrap "github.com/entirehq/git-sync/internal/strategy/bootstrap"
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

// TestNewConn_PropagatesAfterInfoRefs proves the plumbing from
// Endpoint → gitproto.Conn is in place. Without this the hook on
// Endpoint is dead config.
func TestNewConn_PropagatesAfterInfoRefs(t *testing.T) {
	stats := newStats(false)

	off, err := newConn(Endpoint{URL: "https://node.example/repo.git"}, "target", stats, nil)
	if err != nil {
		t.Fatalf("new conn (off): %v", err)
	}
	if off.AfterInfoRefs != nil {
		t.Error("AfterInfoRefs should default to nil")
	}

	want := &url.URL{Scheme: "https", Host: "pinned.example"}
	hook := func(*http.Response) *url.URL { return want }
	on, err := newConn(Endpoint{URL: "https://node.example/repo.git", AfterInfoRefs: hook}, "target", stats, nil)
	if err != nil {
		t.Fatalf("new conn (on): %v", err)
	}
	if on.AfterInfoRefs == nil {
		t.Fatal("AfterInfoRefs was not propagated from Endpoint to Conn")
	}
	if got := on.AfterInfoRefs(nil); got != want {
		t.Errorf("propagated hook returned %v, want %v", got, want)
	}
}
