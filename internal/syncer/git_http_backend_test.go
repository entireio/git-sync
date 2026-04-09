package syncer

import (
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

const gitHTTPBackendEnv = "GITSYNC_E2E_GIT_HTTP_BACKEND"

func TestRun_GitHTTPBackendSync(t *testing.T) {
	if os.Getenv(gitHTTPBackendEnv) == "" {
		t.Skip("set GITSYNC_E2E_GIT_HTTP_BACKEND=1 to run git-http-backend integration test")
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}

	root := t.TempDir()
	sourceBare := filepath.Join(root, "source.git")
	targetBare := filepath.Join(root, "target.git")
	worktree := filepath.Join(root, "work")

	runGit(t, root, "init", "--bare", sourceBare)
	runGit(t, root, "init", "--bare", targetBare)
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, root, "init", "-b", testBranch, worktree)
	runGit(t, worktree, "config", "user.name", "git-sync test")
	runGit(t, worktree, "config", "user.email", "git-sync@example.com")

	writeFile(t, filepath.Join(worktree, "README.md"), "one\n")
	runGit(t, worktree, "add", "README.md")
	runGit(t, worktree, "commit", "-m", "initial")
	runGit(t, worktree, "remote", "add", "origin", sourceBare)
	runGit(t, worktree, "push", "origin", "HEAD:refs/heads/"+testBranch)

	server := newGitHTTPBackendServer(t, root)
	defer server.Close()

	sourceURL := server.RepoURL("source.git")
	targetURL := server.RepoURL("target.git")

	result, err := Run(context.Background(), Config{
		Source: Endpoint{URL: sourceURL},
		Target: Endpoint{URL: targetURL},
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}
	if result.Pushed != 1 || result.Blocked != 0 {
		t.Fatalf("unexpected initial result: %+v", result)
	}
	if !result.Relay || result.RelayMode != "bootstrap" {
		t.Fatalf("expected initial empty-target sync to use bootstrap relay, got %+v", result)
	}

	assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewBranchReferenceName(testBranch))

	writeFile(t, filepath.Join(worktree, "README.md"), "one\ntwo\n")
	runGit(t, worktree, "add", "README.md")
	runGit(t, worktree, "commit", "-m", "second")
	runGit(t, worktree, "push", "origin", "HEAD:refs/heads/"+testBranch)

	result, err = Run(context.Background(), Config{
		Source: Endpoint{URL: sourceURL},
		Target: Endpoint{URL: targetURL},
	})
	if err != nil {
		t.Fatalf("incremental sync failed: %v", err)
	}
	if result.Pushed != 1 || result.Blocked != 0 {
		t.Fatalf("unexpected incremental result: %+v", result)
	}
	if !result.Relay || result.RelayMode != "incremental" {
		t.Fatalf("expected incremental sync to use incremental relay, got %+v", result)
	}

	assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewBranchReferenceName(testBranch))
}

func TestRun_GitHTTPBackendSyncDivergedTarget(t *testing.T) {
	if os.Getenv(gitHTTPBackendEnv) == "" {
		t.Skip("set GITSYNC_E2E_GIT_HTTP_BACKEND=1 to run git-http-backend integration test")
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}

	root := t.TempDir()
	sourceBare := filepath.Join(root, "source.git")
	targetBare := filepath.Join(root, "target.git")
	sourceWorktree := filepath.Join(root, "source-work")
	targetWorktree := filepath.Join(root, "target-work")

	runGit(t, root, "init", "--bare", sourceBare)
	runGit(t, root, "init", "--bare", targetBare)
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, root, "init", "-b", testBranch, sourceWorktree)
	runGit(t, sourceWorktree, "config", "user.name", "git-sync test")
	runGit(t, sourceWorktree, "config", "user.email", "git-sync@example.com")

	writeFile(t, filepath.Join(sourceWorktree, "README.md"), "base\n")
	runGit(t, sourceWorktree, "add", "README.md")
	runGit(t, sourceWorktree, "commit", "-m", "base")
	runGit(t, sourceWorktree, "remote", "add", "source", sourceBare)
	runGit(t, sourceWorktree, "remote", "add", "target", targetBare)
	runGit(t, sourceWorktree, "push", "source", "HEAD:refs/heads/"+testBranch)
	runGit(t, sourceWorktree, "push", "target", "HEAD:refs/heads/"+testBranch)

	runGit(t, root, "init", "-b", testBranch, targetWorktree)
	runGit(t, targetWorktree, "remote", "add", "origin", targetBare)
	runGit(t, targetWorktree, "fetch", "origin", testBranch)
	runGit(t, targetWorktree, "reset", "--hard", "origin/"+testBranch)
	runGit(t, targetWorktree, "config", "user.name", "git-sync test")
	runGit(t, targetWorktree, "config", "user.email", "git-sync@example.com")
	writeFile(t, filepath.Join(targetWorktree, "TARGET.txt"), "target-only\n")
	runGit(t, targetWorktree, "add", "TARGET.txt")
	runGit(t, targetWorktree, "commit", "-m", "target diverges")
	runGit(t, targetWorktree, "push", "origin", "HEAD:refs/heads/"+testBranch)

	writeFile(t, filepath.Join(sourceWorktree, "SOURCE.txt"), "source-only\n")
	runGit(t, sourceWorktree, "add", "SOURCE.txt")
	runGit(t, sourceWorktree, "commit", "-m", "source diverges")
	runGit(t, sourceWorktree, "push", "source", "HEAD:refs/heads/"+testBranch)

	server := newGitHTTPBackendServer(t, root)
	defer server.Close()

	_, err := Run(context.Background(), Config{
		Source: Endpoint{URL: server.RepoURL("source.git")},
		Target: Endpoint{URL: server.RepoURL("target.git")},
	})
	if err == nil {
		t.Fatalf("expected diverged target sync to fail")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected blocked error, got %v", err)
	}
}

func TestRun_GitHTTPBackendSyncMultiBranchFastForward(t *testing.T) {
	if os.Getenv(gitHTTPBackendEnv) == "" {
		t.Skip("set GITSYNC_E2E_GIT_HTTP_BACKEND=1 to run git-http-backend integration test")
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}

	root := t.TempDir()
	sourceBare := filepath.Join(root, "source.git")
	targetBare := filepath.Join(root, "target.git")
	worktree := filepath.Join(root, "work")

	runGit(t, root, "init", "--bare", sourceBare)
	runGit(t, root, "init", "--bare", targetBare)
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, root, "init", "-b", testBranch, worktree)
	runGit(t, worktree, "config", "user.name", "git-sync test")
	runGit(t, worktree, "config", "user.email", "git-sync@example.com")

	writeFile(t, filepath.Join(worktree, "README.md"), "base\n")
	runGit(t, worktree, "add", "README.md")
	runGit(t, worktree, "commit", "-m", "initial")
	runGit(t, worktree, "branch", "release")
	runGit(t, worktree, "remote", "add", "origin", sourceBare)
	runGit(t, worktree, "push", "origin", "HEAD:refs/heads/"+testBranch)
	runGit(t, worktree, "push", "origin", "release:refs/heads/release")

	server := newGitHTTPBackendServer(t, root)
	defer server.Close()

	sourceURL := server.RepoURL("source.git")
	targetURL := server.RepoURL("target.git")

	if _, err := Run(context.Background(), Config{
		Source: Endpoint{URL: sourceURL},
		Target: Endpoint{URL: targetURL},
	}); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	runGit(t, worktree, "checkout", testBranch)
	writeFile(t, filepath.Join(worktree, "main.txt"), "main update\n")
	runGit(t, worktree, "add", "main.txt")
	runGit(t, worktree, "commit", "-m", "main update")
	runGit(t, worktree, "push", "origin", "HEAD:refs/heads/"+testBranch)

	runGit(t, worktree, "checkout", "release")
	writeFile(t, filepath.Join(worktree, "release.txt"), "release update\n")
	runGit(t, worktree, "add", "release.txt")
	runGit(t, worktree, "commit", "-m", "release update")
	runGit(t, worktree, "push", "origin", "HEAD:refs/heads/release")

	result, err := Run(context.Background(), Config{
		Source: Endpoint{URL: sourceURL},
		Target: Endpoint{URL: targetURL},
	})
	if err != nil {
		t.Fatalf("multi-branch incremental sync failed: %v", err)
	}
	if result.Pushed != 2 || result.Blocked != 0 {
		t.Fatalf("unexpected multi-branch result: %+v", result)
	}
	if !result.Relay || result.RelayMode != "incremental" {
		t.Fatalf("expected multi-branch fast-forward sync to use incremental relay, got %+v", result)
	}

	assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewBranchReferenceName(testBranch))
	assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewBranchReferenceName("release"))
}

func TestRun_GitHTTPBackendSyncMappedBranchFastForward(t *testing.T) {
	if os.Getenv(gitHTTPBackendEnv) == "" {
		t.Skip("set GITSYNC_E2E_GIT_HTTP_BACKEND=1 to run git-http-backend integration test")
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}

	root := t.TempDir()
	sourceBare := filepath.Join(root, "source.git")
	targetBare := filepath.Join(root, "target.git")
	worktree := filepath.Join(root, "work")

	runGit(t, root, "init", "--bare", sourceBare)
	runGit(t, root, "init", "--bare", targetBare)
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, root, "init", "-b", testBranch, worktree)
	runGit(t, worktree, "config", "user.name", "git-sync test")
	runGit(t, worktree, "config", "user.email", "git-sync@example.com")

	writeFile(t, filepath.Join(worktree, "README.md"), "mapped\n")
	runGit(t, worktree, "add", "README.md")
	runGit(t, worktree, "commit", "-m", "initial")
	runGit(t, worktree, "remote", "add", "origin", sourceBare)
	runGit(t, worktree, "push", "origin", "HEAD:refs/heads/"+testBranch)

	server := newGitHTTPBackendServer(t, root)
	defer server.Close()

	sourceURL := server.RepoURL("source.git")
	targetURL := server.RepoURL("target.git")

	result, err := Run(context.Background(), Config{
		Source:   Endpoint{URL: sourceURL},
		Target:   Endpoint{URL: targetURL},
		Mappings: []RefMapping{{Source: testBranch, Target: "stable"}},
	})
	if err != nil {
		t.Fatalf("initial mapped sync failed: %v", err)
	}
	if result.Pushed != 1 || result.Blocked != 0 {
		t.Fatalf("unexpected initial mapped result: %+v", result)
	}
	if !result.Relay || result.RelayMode != "bootstrap" {
		t.Fatalf("expected initial mapped sync to use bootstrap relay, got %+v", result)
	}

	writeFile(t, filepath.Join(worktree, "README.md"), "mapped\nupdate\n")
	runGit(t, worktree, "add", "README.md")
	runGit(t, worktree, "commit", "-m", "mapped update")
	runGit(t, worktree, "push", "origin", "HEAD:refs/heads/"+testBranch)

	result, err = Run(context.Background(), Config{
		Source:   Endpoint{URL: sourceURL},
		Target:   Endpoint{URL: targetURL},
		Mappings: []RefMapping{{Source: testBranch, Target: "stable"}},
	})
	if err != nil {
		t.Fatalf("mapped incremental sync failed: %v", err)
	}
	if result.Pushed != 1 || result.Blocked != 0 {
		t.Fatalf("unexpected mapped incremental result: %+v", result)
	}
	if !result.Relay || result.RelayMode != "incremental" {
		t.Fatalf("expected mapped incremental sync to use incremental relay, got %+v", result)
	}

	assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewBranchReferenceName(testBranch), plumbing.NewBranchReferenceName("stable"))
}

func TestBootstrap_GitHTTPBackendSync(t *testing.T) {
	if os.Getenv(gitHTTPBackendEnv) == "" {
		t.Skip("set GITSYNC_E2E_GIT_HTTP_BACKEND=1 to run git-http-backend integration test")
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}

	root := t.TempDir()
	sourceBare := filepath.Join(root, "source.git")
	targetBare := filepath.Join(root, "target.git")
	worktree := filepath.Join(root, "work")

	runGit(t, root, "init", "--bare", sourceBare)
	runGit(t, root, "init", "--bare", targetBare)
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, root, "init", "-b", testBranch, worktree)
	runGit(t, worktree, "config", "user.name", "git-sync test")
	runGit(t, worktree, "config", "user.email", "git-sync@example.com")

	writeFile(t, filepath.Join(worktree, "README.md"), "bootstrap\n")
	runGit(t, worktree, "add", "README.md")
	runGit(t, worktree, "commit", "-m", "initial")
	runGit(t, worktree, "remote", "add", "origin", sourceBare)
	runGit(t, worktree, "push", "origin", "HEAD:refs/heads/"+testBranch)

	server := newGitHTTPBackendServer(t, root)
	defer server.Close()

	sourceURL := server.RepoURL("source.git")
	targetURL := server.RepoURL("target.git")

	result, err := Bootstrap(context.Background(), Config{
		Source: Endpoint{URL: sourceURL},
		Target: Endpoint{URL: targetURL},
	})
	if err != nil {
		t.Fatalf("bootstrap sync failed: %v", err)
	}
	if result.Pushed != 1 || result.Blocked != 0 {
		t.Fatalf("unexpected bootstrap result: %+v", result)
	}

	assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewBranchReferenceName(testBranch))
}

type gitHTTPBackendServer struct {
	server *httptest.Server
	root   string
}

func newGitHTTPBackendServer(t *testing.T, root string) *gitHTTPBackendServer {
	t.Helper()

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find git: %v", err)
	}

	handler := &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}

	return &gitHTTPBackendServer{
		server: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handler.ServeHTTP(w, r)
		})),
		root: root,
	}
}

func (s *gitHTTPBackendServer) Close() {
	s.server.Close()
}

func (s *gitHTTPBackendServer) RepoURL(name string) string {
	return s.server.URL + "/" + strings.TrimPrefix(name, "/")
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertGitRefEqual(t *testing.T, sourceRepoPath, targetRepoPath string, refs ...plumbing.ReferenceName) {
	t.Helper()
	sourceRef := refs[0]
	targetRef := sourceRef
	if len(refs) > 1 {
		targetRef = refs[1]
	}
	sourceHash := strings.TrimSpace(runGit(t, sourceRepoPath, "rev-parse", sourceRef.String()))
	targetHash := strings.TrimSpace(runGit(t, targetRepoPath, "rev-parse", targetRef.String()))
	if sourceHash != targetHash {
		t.Fatalf("ref mismatch for %s -> %s: source=%s target=%s", sourceRef, targetRef, sourceHash, targetHash)
	}
}
