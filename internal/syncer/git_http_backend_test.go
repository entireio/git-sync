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

func assertGitRefEqual(t *testing.T, sourceRepoPath, targetRepoPath string, ref plumbing.ReferenceName) {
	t.Helper()
	sourceHash := strings.TrimSpace(runGit(t, sourceRepoPath, "rev-parse", ref.String()))
	targetHash := strings.TrimSpace(runGit(t, targetRepoPath, "rev-parse", ref.String()))
	if sourceHash != targetHash {
		t.Fatalf("ref mismatch for %s: source=%s target=%s", ref, sourceHash, targetHash)
	}
}
