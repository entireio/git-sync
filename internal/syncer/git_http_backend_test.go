package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
	bstrap "entire.io/entire/git-sync/internal/strategy/bootstrap"
	"entire.io/entire/git-sync/internal/translate"
	"github.com/go-git/go-git/v6/plumbing"
)

const gitHTTPBackendEnv = "GITSYNC_E2E_GIT_HTTP_BACKEND"

func TestRun_GitHTTPBackendSync(t *testing.T) {
	if os.Getenv(gitHTTPBackendEnv) == "" {
		t.Skip("set GITSYNC_E2E_GIT_HTTP_BACKEND=1 to run git-http-backend integration test")
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}

	// Default sync mode plans in an in-memory store; a store left at sha1
	// cannot decode a sha256 source's pack, so both formats run.
	for _, objectFormat := range []string{"sha1", "sha256"} {
		t.Run(objectFormat, func(t *testing.T) {
			root := t.TempDir()
			sourceBare := filepath.Join(root, "source.git")
			targetBare := filepath.Join(root, "target.git")
			worktree := filepath.Join(root, "work")
			format := "--object-format=" + objectFormat

			runGit(t, root, "init", "--bare", format, sourceBare)
			runGit(t, root, "init", "--bare", format, targetBare)
			runGit(t, targetBare, "config", "http.receivepack", "true")
			runGit(t, root, "init", format, "-b", testBranch, worktree)
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
			if !result.Relay || result.RelayMode != relayModeBootstrap {
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
			if !result.Relay || result.RelayMode != relayModeIncremental {
				t.Fatalf("expected incremental sync to use incremental relay, got %+v", result)
			}

			assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewBranchReferenceName(testBranch))
		})
	}
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
	if !result.Relay || result.RelayMode != relayModeIncremental {
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
	if !result.Relay || result.RelayMode != relayModeBootstrap {
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
	if !result.Relay || result.RelayMode != relayModeIncremental {
		t.Fatalf("expected mapped incremental sync to use incremental relay, got %+v", result)
	}

	assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewBranchReferenceName(testBranch), plumbing.NewBranchReferenceName("stable"))
}

func TestRun_GitHTTPBackendSyncTagCreate(t *testing.T) {
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
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, root, "init", "-b", testBranch, worktree)
	runGit(t, worktree, "config", "user.name", "git-sync test")
	runGit(t, worktree, "config", "user.email", "git-sync@example.com")

	writeFile(t, filepath.Join(worktree, "README.md"), "tag\n")
	runGit(t, worktree, "add", "README.md")
	runGit(t, worktree, "commit", "-m", "initial")
	runGit(t, worktree, "remote", "add", "origin", sourceBare)
	runGit(t, worktree, "push", "origin", "HEAD:refs/heads/"+testBranch)

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

	runGit(t, worktree, "tag", "v1")
	runGit(t, worktree, "push", "origin", "refs/tags/v1")

	result, err := Run(context.Background(), Config{
		Source:      Endpoint{URL: sourceURL},
		Target:      Endpoint{URL: targetURL},
		IncludeTags: true,
	})
	if err != nil {
		t.Fatalf("tag-create sync failed: %v", err)
	}
	if result.Pushed != 1 || result.Blocked != 0 {
		t.Fatalf("unexpected tag-create result: %+v", result)
	}
	if !result.Relay || result.RelayMode != relayModeIncremental {
		t.Fatalf("expected tag-create sync to use incremental relay, got %+v", result)
	}

	assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewTagReferenceName("v1"))
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

func TestBootstrap_GitHTTPBackendBatchedBranch(t *testing.T) {
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
	runGit(t, sourceBare, "config", "uploadpack.allowFilter", "true")
	runGit(t, sourceBare, "config", "uploadpack.allowReachableSHA1InWant", "true")
	runGit(t, root, "init", "--bare", targetBare)
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, root, "init", "-b", testBranch, worktree)
	runGit(t, worktree, "config", "user.name", "git-sync test")
	runGit(t, worktree, "config", "user.email", "git-sync@example.com")
	runGit(t, worktree, "remote", "add", "origin", sourceBare)

	for i := range 6 {
		writePseudoRandomFile(t, filepath.Join(worktree, fmt.Sprintf("blob-%d.bin", i)), int64(200_000+i*17))
		runGit(t, worktree, "add", ".")
		runGit(t, worktree, "commit", "-m", fmt.Sprintf("commit-%d", i))
	}
	runGit(t, worktree, "push", "origin", "HEAD:refs/heads/"+testBranch)

	server := newGitHTTPBackendServer(t, root)
	defer server.Close()

	sourceURL := server.RepoURL("source.git")
	targetURL := server.RepoURL("target.git")

	result, err := Bootstrap(context.Background(), Config{
		Source:             Endpoint{URL: sourceURL},
		Target:             Endpoint{URL: targetURL},
		TargetMaxPackBytes: 350_000,
	})
	if err != nil {
		t.Fatalf("batched bootstrap failed: %v\nbackend-stderr:\n%s", err, server.Stderr())
	}
	if result.Pushed != 1 || result.Blocked != 0 {
		t.Fatalf("unexpected batched bootstrap result: %+v", result)
	}
	if !result.Relay || result.RelayMode != relayModeBootstrapBatch || !result.Batching {
		t.Fatalf("expected batched bootstrap relay result, got %+v", result)
	}
	if result.BatchCount < 2 {
		t.Fatalf("expected multiple bootstrap batches, got %+v", result)
	}
	// The plan sizes checkpoints from an estimate per commit, and these blobs
	// are bigger than it, so pushes split further against the budget as they
	// go. Every planned checkpoint still runs.
	if result.BatchCount < result.PlannedBatchCount {
		t.Fatalf("expected fresh batched bootstrap to run at least the planned batches, got %+v", result)
	}
	if len(result.TempRefs) != 1 || result.TempRefs[0] != planner.BootstrapTempRef(plumbing.NewBranchReferenceName(testBranch)).String() {
		t.Fatalf("unexpected temp refs in batched bootstrap result: %+v", result)
	}

	assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewBranchReferenceName(testBranch))
	assertGitRefAbsent(t, targetBare, planner.BootstrapTempRef(plumbing.NewBranchReferenceName(testBranch)))
}

func TestBootstrap_GitHTTPBackendBatchedBranchResume(t *testing.T) {
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
	runGit(t, sourceBare, "config", "uploadpack.allowFilter", "true")
	runGit(t, sourceBare, "config", "uploadpack.allowReachableSHA1InWant", "true")
	runGit(t, root, "init", "--bare", targetBare)
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, root, "init", "-b", testBranch, worktree)
	runGit(t, worktree, "config", "user.name", "git-sync test")
	runGit(t, worktree, "config", "user.email", "git-sync@example.com")
	runGit(t, worktree, "remote", "add", "origin", sourceBare)

	for i := range 6 {
		writePseudoRandomFile(t, filepath.Join(worktree, fmt.Sprintf("resume-%d.bin", i)), int64(200_000+i*23))
		runGit(t, worktree, "add", ".")
		runGit(t, worktree, "commit", "-m", fmt.Sprintf("resume-%d", i))
	}
	runGit(t, worktree, "push", "origin", "HEAD:refs/heads/"+testBranch)

	server := newGitHTTPBackendServer(t, root)
	defer server.Close()

	sourceURL := server.RepoURL("source.git")
	targetURL := server.RepoURL("target.git")
	cfg := Config{
		Source:             Endpoint{URL: sourceURL},
		Target:             Endpoint{URL: targetURL},
		TargetMaxPackBytes: 350_000,
		ProtocolMode:       protocolModeAuto,
	}

	stats := newStats(false)
	sourceConn, err := newConn(cfg.Source, "source", stats, nil)
	if err != nil {
		t.Fatalf("create source transport: %v", err)
	}
	sourceRefs, sourceService, err := gitproto.ListSourceRefs(context.Background(), sourceConn, cfg.ProtocolMode, planner.RefPrefixes(planConfig(cfg)))
	if err != nil {
		t.Fatalf("list source refs: %v", err)
	}
	desired, _, err := planner.BuildDesiredRefs(gitproto.RefHashMap(sourceRefs), planner.PlanConfig{
		Branches: cfg.Branches, Mappings: cfg.Mappings, IncludeTags: cfg.IncludeTags,
		Force: cfg.ForceAny(), Prune: cfg.Prune,
	})
	if err != nil {
		t.Fatalf("build desired refs: %v", err)
	}
	ref := desired[plumbing.NewBranchReferenceName(testBranch)]
	bParams := bstrap.Params{
		SourceConn: sourceConn, SourceService: sourceService,
		TargetMaxPack: cfg.TargetMaxPackBytes, Verbose: cfg.Verbose,
	}
	checkpoints, err := bstrap.PlanCheckpoints(context.Background(), bParams, ref)
	if err != nil {
		t.Fatalf("plan checkpoints: %v", err)
	}
	if len(checkpoints) < 2 {
		t.Fatalf("expected multiple checkpoints for resume test, got %v", checkpoints)
	}

	tempRef := planner.BootstrapTempRef(plumbing.NewBranchReferenceName(testBranch))
	runGit(t, worktree, "push", targetBare, checkpoints[0].String()+":"+tempRef.String())

	result, err := Bootstrap(context.Background(), cfg)
	if err != nil {
		t.Fatalf("batched bootstrap resume failed: %v\nbackend-stderr:\n%s", err, server.Stderr())
	}
	if result.Pushed != 1 || result.Blocked != 0 {
		t.Fatalf("unexpected batched resume result: %+v", result)
	}
	if !result.Relay || result.RelayMode != relayModeBootstrapBatch || !result.Batching {
		t.Fatalf("expected batched bootstrap resume relay result, got %+v", result)
	}
	// Packs split at push time against the budget, so the plan's checkpoint
	// count is no measure of a full run. Run one to a second target instead.
	fresh := filepath.Join(root, "fresh.git")
	runGit(t, root, "init", "--bare", fresh)
	runGit(t, fresh, "config", "http.receivepack", "true")
	freshCfg := cfg
	freshCfg.Target = Endpoint{URL: server.RepoURL("fresh.git")}
	full, err := Bootstrap(context.Background(), freshCfg)
	if err != nil {
		t.Fatalf("full batched bootstrap failed: %v\nbackend-stderr:\n%s", err, server.Stderr())
	}
	if result.BatchCount >= full.BatchCount {
		t.Fatalf("expected resume to execute fewer batches than a full run, got %d, full run %d", result.BatchCount, full.BatchCount)
	}
	if result.PlannedBatchCount != len(checkpoints) {
		t.Fatalf("expected resume result to report planned checkpoint count, got %+v checkpoints=%d", result, len(checkpoints))
	}
	if len(result.TempRefs) != 1 || result.TempRefs[0] != tempRef.String() {
		t.Fatalf("unexpected temp refs in batched resume result: %+v", result)
	}

	assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewBranchReferenceName(testBranch))
	assertGitRefAbsent(t, targetBare, tempRef)
}

func TestBootstrap_GitHTTPBackendBatchedPlanningTracksBatchLimit(t *testing.T) {
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
	runGit(t, sourceBare, "config", "uploadpack.allowFilter", "true")
	runGit(t, sourceBare, "config", "uploadpack.allowReachableSHA1InWant", "true")
	runGit(t, root, "init", "--bare", targetBare)
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, root, "init", "-b", testBranch, worktree)
	runGit(t, worktree, "config", "user.name", "git-sync test")
	runGit(t, worktree, "config", "user.email", "git-sync@example.com")
	runGit(t, worktree, "remote", "add", "origin", sourceBare)

	for i := range 8 {
		writePseudoRandomFile(t, filepath.Join(worktree, fmt.Sprintf("limit-%d.bin", i)), int64(180_000+i*29))
		runGit(t, worktree, "add", ".")
		runGit(t, worktree, "commit", "-m", fmt.Sprintf("limit-%d", i))
	}
	runGit(t, worktree, "push", "origin", "HEAD:refs/heads/"+testBranch)

	server := newGitHTTPBackendServer(t, root)
	defer server.Close()

	sourceURL := server.RepoURL("source.git")
	cfg := Config{
		Source:       Endpoint{URL: sourceURL},
		Target:       Endpoint{URL: server.RepoURL("target.git")},
		ProtocolMode: protocolModeAuto,
	}

	stats := newStats(false)
	sourceConn, err := newConn(cfg.Source, "source", stats, nil)
	if err != nil {
		t.Fatalf("create source transport: %v", err)
	}
	sourceRefs, sourceService, err := gitproto.ListSourceRefs(context.Background(), sourceConn, cfg.ProtocolMode, planner.RefPrefixes(planConfig(cfg)))
	if err != nil {
		t.Fatalf("list source refs: %v", err)
	}
	desired, _, err := planner.BuildDesiredRefs(gitproto.RefHashMap(sourceRefs), planner.PlanConfig{
		Branches: cfg.Branches, Mappings: cfg.Mappings, IncludeTags: cfg.IncludeTags,
		Force: cfg.ForceAny(), Prune: cfg.Prune,
	})
	if err != nil {
		t.Fatalf("build desired refs: %v", err)
	}
	ref := desired[plumbing.NewBranchReferenceName(testBranch)]

	plan := func(limit int64) []plumbing.Hash {
		t.Helper()
		checkpoints, err := bstrap.PlanCheckpoints(context.Background(), bstrap.Params{
			SourceConn:    sourceConn,
			SourceService: sourceService,
			TargetMaxPack: limit,
			Verbose:       cfg.Verbose,
		}, ref)
		if err != nil {
			t.Fatalf("plan checkpoints with limit %d: %v", limit, err)
		}
		return checkpoints
	}

	smaller := plan(250_000)
	larger := plan(450_000)

	if len(smaller) < 2 {
		t.Fatalf("expected smaller limit to produce multiple checkpoints, got %d (%v)", len(smaller), smaller)
	}
	if len(larger) == 0 {
		t.Fatal("expected larger limit to produce at least one checkpoint")
	}
	if len(smaller) < len(larger) {
		t.Fatalf("expected smaller batch limit to require at least as many checkpoints as larger limit, got smaller=%d larger=%d", len(smaller), len(larger))
	}
	if smaller[len(smaller)-1] != ref.SourceHash || larger[len(larger)-1] != ref.SourceHash {
		t.Fatalf("expected final checkpoint to be branch tip, got smaller=%s larger=%s tip=%s", smaller[len(smaller)-1], larger[len(larger)-1], ref.SourceHash)
	}
}

func TestBootstrap_GitHTTPBackendBatchedBranchWithTags(t *testing.T) {
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
	runGit(t, sourceBare, "config", "uploadpack.allowFilter", "true")
	runGit(t, sourceBare, "config", "uploadpack.allowReachableSHA1InWant", "true")
	runGit(t, root, "init", "--bare", targetBare)
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, root, "init", "-b", testBranch, worktree)
	runGit(t, worktree, "config", "user.name", "git-sync test")
	runGit(t, worktree, "config", "user.email", "git-sync@example.com")
	runGit(t, worktree, "remote", "add", "origin", sourceBare)

	for i := range 5 {
		writePseudoRandomFile(t, filepath.Join(worktree, fmt.Sprintf("tagged-%d.bin", i)), int64(200_000+i*31))
		runGit(t, worktree, "add", ".")
		runGit(t, worktree, "commit", "-m", fmt.Sprintf("tagged-%d", i))
	}
	runGit(t, worktree, "tag", "-a", "v1", "-m", "version 1")
	runGit(t, worktree, "push", "origin", "HEAD:refs/heads/"+testBranch)
	runGit(t, worktree, "push", "origin", "refs/tags/v1")

	server := newGitHTTPBackendServer(t, root)
	defer server.Close()

	sourceURL := server.RepoURL("source.git")
	targetURL := server.RepoURL("target.git")

	result, err := Bootstrap(context.Background(), Config{
		Source:             Endpoint{URL: sourceURL},
		Target:             Endpoint{URL: targetURL},
		IncludeTags:        true,
		TargetMaxPackBytes: 350_000,
	})
	if err != nil {
		t.Fatalf("batched bootstrap with tags failed: %v\nbackend-stderr:\n%s", err, server.Stderr())
	}
	if result.Pushed != 2 || result.Blocked != 0 {
		t.Fatalf("unexpected batched bootstrap with tags result: %+v", result)
	}
	if !result.Relay || result.RelayMode != relayModeBootstrapBatch || !result.Batching {
		t.Fatalf("expected batched bootstrap with tags relay result, got %+v", result)
	}

	assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewBranchReferenceName(testBranch))
	assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewTagReferenceName("v1"))
	assertGitRefAbsent(t, targetBare, planner.BootstrapTempRef(plumbing.NewBranchReferenceName(testBranch)))
}

type gitHTTPBackendServer struct {
	server *httptest.Server
	root   string
	mu     sync.Mutex
	stderr bytes.Buffer
	// maxPushBody, when set, refuses receive-pack POST bodies over that many
	// bytes with a 413, as a hosted target's request body limit does.
	maxPushBody atomic.Int64
	// lastPack holds the pack of the last push that carried one, per
	// repository.
	lastPack map[string][]byte
	// uploadPackFetches counts upload-pack requests that fetch objects, so a
	// test can tell a run that failed before fetching anything.
	uploadPackFetches atomic.Int64
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
	s := &gitHTTPBackendServer{
		root:     root,
		lastPack: map[string][]byte{},
	}
	handler.Stderr = &lockedBuffer{server: s}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isPush := strings.HasSuffix(r.URL.Path, "/git-receive-pack")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			// A v2 ls-refs is a POST too; count only requests that fetch.
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.TransferEncoding = nil
			if !bytes.Contains(body, []byte("command=ls-refs")) {
				s.uploadPackFetches.Add(1)
			}
		}
		// net/http/cgi refuses chunked request bodies, and git-sync streams
		// its pushes. Buffer the body so the CGI gets a Content-Length.
		// Buffer every push, chunked or not, so LastPack sees each one.
		if len(r.TransferEncoding) > 0 || isPush {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.TransferEncoding = nil
			if pack := pushedPack(body); len(pack) > 0 && isPush {
				repo, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
				s.mu.Lock()
				s.lastPack[repo] = pack
				s.mu.Unlock()
			}
		}
		if limit := s.maxPushBody.Load(); limit > 0 && isPush && r.ContentLength > limit {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	return s
}

// LastPack is the pack of the last push into repo that carried one.
func (s *gitHTTPBackendServer) LastPack(t *testing.T, repo string) []byte {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	pack, ok := s.lastPack[repo]
	if !ok {
		t.Fatalf("no pack pushed into %s", repo)
	}
	return pack
}

func (s *gitHTTPBackendServer) Close() {
	s.server.Close()
}

func (s *gitHTTPBackendServer) RepoURL(name string) string {
	return s.server.URL + "/" + strings.TrimPrefix(name, "/")
}

func (s *gitHTTPBackendServer) Stderr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stderr.String()
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
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

func assertGitRefAbsent(t *testing.T, repoPath string, ref plumbing.ReferenceName) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "show-ref", "--verify", "--quiet", ref.String())
	cmd.Dir = repoPath
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err == nil {
		t.Fatalf("expected ref %s to be absent", ref)
	}
}

func writePseudoRandomFile(t *testing.T, path string, size int64) {
	t.Helper()
	rng := rand.New(rand.NewSource(size))
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte(rng.Intn(256))
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

type lockedBuffer struct {
	server *gitHTTPBackendServer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.server.mu.Lock()
	defer b.server.mu.Unlock()
	return b.server.stderr.Write(p)
}

// TestBootstrap_GitHTTPBackendBatchedManyBranches seeds a target from a source
// shaped like a busy public repository: a trunk, many short branches, branches
// already merged into the trunk, one branch too big to share a pack, and a tag.
// Every ref must land, and no temp ref may be left behind.
func TestBootstrap_GitHTTPBackendBatchedManyBranches(t *testing.T) {
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
	runGit(t, sourceBare, "config", "uploadpack.allowFilter", "true")
	runGit(t, sourceBare, "config", "uploadpack.allowReachableSHA1InWant", "true")
	runGit(t, root, "init", "--bare", targetBare)
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, root, "init", "-b", testBranch, worktree)
	runGit(t, worktree, "config", "user.name", "git-sync test")
	runGit(t, worktree, "config", "user.email", "git-sync@example.com")
	runGit(t, worktree, "remote", "add", "origin", sourceBare)

	for i := range 5 {
		writePseudoRandomFile(t, filepath.Join(worktree, fmt.Sprintf("trunk-%d.bin", i)), int64(200_000+i*13))
		runGit(t, worktree, "add", ".")
		runGit(t, worktree, "commit", "-m", fmt.Sprintf("trunk-%d", i))
	}
	runGit(t, worktree, "tag", "-a", "v1", "-m", "version 1")

	branches := []string{testBranch}
	const small = 30
	for i := range small {
		name := fmt.Sprintf("feature-%02d", i)
		runGit(t, worktree, "switch", "-q", "-c", name, testBranch+"~2")
		writeFile(t, filepath.Join(worktree, name+".txt"), name+"\n")
		runGit(t, worktree, "add", ".")
		runGit(t, worktree, "commit", "-m", name)
		branches = append(branches, name)
	}
	for i := range 2 {
		name := fmt.Sprintf("merged-%d", i)
		runGit(t, worktree, "branch", name, fmt.Sprintf("%s~%d", testBranch, i+1))
		branches = append(branches, name)
	}
	runGit(t, worktree, "switch", "-q", "-c", "big", testBranch+"~3")
	for i := range 3 {
		writePseudoRandomFile(t, filepath.Join(worktree, fmt.Sprintf("big-%d.bin", i)), int64(210_000+i*7))
		runGit(t, worktree, "add", ".")
		runGit(t, worktree, "commit", "-m", fmt.Sprintf("big-%d", i))
	}
	branches = append(branches, "big")
	runGit(t, worktree, "switch", "-q", testBranch)
	runGit(t, worktree, "push", "-q", "origin", "--all")
	runGit(t, worktree, "push", "-q", "origin", "refs/tags/v1")
	runGit(t, sourceBare, "symbolic-ref", "HEAD", "refs/heads/"+testBranch)

	server := newGitHTTPBackendServer(t, root)
	defer server.Close()

	result, err := Bootstrap(context.Background(), Config{
		Source:             Endpoint{URL: server.RepoURL("source.git")},
		Target:             Endpoint{URL: server.RepoURL("target.git")},
		IncludeTags:        true,
		TargetMaxPackBytes: 350_000,
	})
	if err != nil {
		t.Fatalf("batched bootstrap failed: %v\nbackend-stderr:\n%s", err, server.Stderr())
	}
	if !result.Batching || result.RelayMode != relayModeBootstrapBatch {
		t.Fatalf("expected a batched bootstrap, got %+v", result)
	}
	if want := len(branches) + 1; result.Pushed != want {
		t.Fatalf("Pushed = %d, want %d", result.Pushed, want)
	}
	// Small branches share packs. One push per branch would need at least as
	// many batches as there are branches, before the trunk's own checkpoints.
	if result.BatchCount >= len(branches) {
		t.Fatalf("BatchCount = %d for %d branches; small branches were not grouped", result.BatchCount, len(branches))
	}
	for _, name := range branches {
		ref := plumbing.NewBranchReferenceName(name)
		assertGitRefEqual(t, sourceBare, targetBare, ref)
		assertGitRefAbsent(t, targetBare, planner.BootstrapTempRef(ref))
	}
	assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewTagReferenceName("v1"))
	runGit(t, targetBare, "fsck", "--connectivity-only")
}

// TestReplicate_GitHTTPBackendResumeAfterTrunkLanded covers a replicate run
// that follows a timed-out bootstrap. Bootstrap pushes the trunk first and
// creates grouped branches directly, so a run cut short leaves real refs on
// the target and no temp ref to mark it as resumable. The next run must still
// fit its pushes under the target's request body limit.
func TestReplicate_GitHTTPBackendResumeAfterTrunkLanded(t *testing.T) {
	if os.Getenv(gitHTTPBackendEnv) == "" {
		t.Skip("set GITSYNC_E2E_GIT_HTTP_BACKEND=1 to run git-http-backend integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}

	const (
		branchCount = 20
		branchBlob  = 120_000
		pushLimit   = 1_000_000
	)

	for _, tc := range []struct {
		name string
		// seedTrunk is the trunk revision already on the target, if any.
		seedTrunk  string
		wantReason string
		// objectFormat initializes every repository with this format; ""
		// keeps git's default (sha1).
		objectFormat string
	}{
		{name: "empty target", wantReason: planner.ReasonEmptyTargetManagedRefs},
		{name: "empty sha256 target", wantReason: planner.ReasonEmptyTargetManagedRefs, objectFormat: "sha256"},
		{name: "sha256 trunk already on target", seedTrunk: testBranch, wantReason: planner.ReasonReplicatePackRejected, objectFormat: "sha256"},
		{name: "trunk already on target", seedTrunk: testBranch, wantReason: planner.ReasonReplicatePackRejected},
		// The trunk moved on since: one update rides along with the creates.
		{name: "trunk behind on target", seedTrunk: testBranch + "~1", wantReason: planner.ReasonReplicatePackRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sourceBare := filepath.Join(root, "source.git")
			targetBare := filepath.Join(root, "target.git")
			worktree := filepath.Join(root, "work")

			var formatArgs []string
			if tc.objectFormat != "" {
				formatArgs = []string{"--object-format=" + tc.objectFormat}
			}
			runGit(t, root, append([]string{"init", "--bare", sourceBare}, formatArgs...)...)
			runGit(t, sourceBare, "config", "uploadpack.allowFilter", "true")
			runGit(t, sourceBare, "config", "uploadpack.allowReachableSHA1InWant", "true")
			runGit(t, root, append([]string{"init", "--bare", targetBare}, formatArgs...)...)
			runGit(t, targetBare, "config", "http.receivepack", "true")
			runGit(t, root, append([]string{"init", "-b", testBranch, worktree}, formatArgs...)...)
			runGit(t, worktree, "config", "user.name", "git-sync test")
			runGit(t, worktree, "config", "user.email", "git-sync@example.com")
			runGit(t, worktree, "remote", "add", "origin", sourceBare)

			for i := range 3 {
				writePseudoRandomFile(t, filepath.Join(worktree, fmt.Sprintf("trunk-%d.bin", i)), int64(150_000+i*13))
				runGit(t, worktree, "add", ".")
				runGit(t, worktree, "commit", "-m", fmt.Sprintf("trunk-%d", i))
			}
			branches := []string{testBranch}
			for i := range branchCount {
				name := fmt.Sprintf("feature-%02d", i)
				runGit(t, worktree, "switch", "-q", "-c", name, testBranch)
				writePseudoRandomFile(t, filepath.Join(worktree, name+".bin"), int64(branchBlob+i*11))
				runGit(t, worktree, "add", ".")
				runGit(t, worktree, "commit", "-m", name)
				branches = append(branches, name)
			}
			runGit(t, worktree, "switch", "-q", testBranch)
			runGit(t, worktree, "push", "-q", "origin", "--all")
			runGit(t, sourceBare, "symbolic-ref", "HEAD", "refs/heads/"+testBranch)

			// What a bootstrap that timed out after the trunk leaves behind:
			// the trunk under its real name, every other branch missing.
			if tc.seedTrunk != "" {
				runGit(t, worktree, "push", "-q", targetBare, tc.seedTrunk+":refs/heads/"+testBranch)
			}

			server := newGitHTTPBackendServer(t, root)
			defer server.Close()
			server.maxPushBody.Store(pushLimit)

			// The mirror-pipeline worker's policy: replicate, prune, tags, and
			// no configured pack limit, so the target's 413 is the only signal.
			result, err := Run(context.Background(), Config{
				Source:      Endpoint{URL: server.RepoURL("source.git")},
				Target:      Endpoint{URL: server.RepoURL("target.git")},
				Mode:        modeReplicate,
				Prune:       true,
				IncludeTags: true,
			})
			t.Logf("relay mode %q, reason %q, pushed %d, batches %d", result.RelayMode, result.RelayReason, result.Pushed, result.BatchCount)
			if err != nil {
				t.Fatalf("replicate failed: %v", err)
			}
			if result.RelayMode != relayModeBootstrapBatch || result.RelayReason != tc.wantReason {
				t.Fatalf("relay = %q (%q), want %q (%q)", result.RelayMode, result.RelayReason, relayModeBootstrapBatch, tc.wantReason)
			}
			for _, name := range branches {
				assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewBranchReferenceName(name))
			}
			runGit(t, targetBare, "fsck", "--connectivity-only")
		})
	}
}

// TestReplicate_GitHTTPBackendSHA256 replicates a sha256 repository the way
// the mirror-pipeline native worker does: an empty-target bootstrap, then an
// incremental sync after a new push. Protocol v2 requires the client to echo
// the server's object-format on every command; a client that stays silent is
// treated as sha1 and rejected by a sha256 server.
func TestReplicate_GitHTTPBackendSHA256(t *testing.T) {
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

	runGit(t, root, "init", "--bare", "--object-format=sha256", sourceBare)
	runGit(t, root, "init", "--bare", "--object-format=sha256", targetBare)
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, root, "init", "--object-format=sha256", "-b", testBranch, worktree)
	runGit(t, worktree, "config", "user.name", "git-sync test")
	runGit(t, worktree, "config", "user.email", "git-sync@example.com")
	runGit(t, worktree, "remote", "add", "origin", sourceBare)

	writeFile(t, filepath.Join(worktree, "README.md"), "one\n")
	runGit(t, worktree, "add", "README.md")
	runGit(t, worktree, "commit", "-m", "initial")
	runGit(t, worktree, "tag", "-a", "v1", "-m", "v1")
	runGit(t, worktree, "switch", "-q", "-c", "feature")
	writeFile(t, filepath.Join(worktree, "FEATURE.md"), "feature\n")
	runGit(t, worktree, "add", "FEATURE.md")
	runGit(t, worktree, "commit", "-m", "feature")
	runGit(t, worktree, "switch", "-q", testBranch)
	runGit(t, worktree, "push", "-q", "origin", "--all")
	runGit(t, worktree, "push", "-q", "origin", "--tags")
	runGit(t, sourceBare, "symbolic-ref", "HEAD", "refs/heads/"+testBranch)

	server := newGitHTTPBackendServer(t, root)
	defer server.Close()

	run := func() {
		t.Helper()
		result, err := Run(context.Background(), Config{
			Source:      Endpoint{URL: server.RepoURL("source.git")},
			Target:      Endpoint{URL: server.RepoURL("target.git")},
			Mode:        modeReplicate,
			Prune:       true,
			IncludeTags: true,
		})
		if err != nil {
			t.Fatalf("replicate failed: %v", err)
		}
		t.Logf("relay mode %q, reason %q, pushed %d", result.RelayMode, result.RelayReason, result.Pushed)
	}
	assertAll := func() {
		t.Helper()
		for _, ref := range []plumbing.ReferenceName{
			plumbing.NewBranchReferenceName(testBranch),
			plumbing.NewBranchReferenceName("feature"),
			plumbing.NewTagReferenceName("v1"),
		} {
			assertGitRefEqual(t, sourceBare, targetBare, ref)
		}
		runGit(t, targetBare, "fsck", "--connectivity-only")
	}

	run()
	assertAll()

	// Incremental: a fast-forward, a new branch and a deleted branch.
	writeFile(t, filepath.Join(worktree, "README.md"), "one\ntwo\n")
	runGit(t, worktree, "add", "README.md")
	runGit(t, worktree, "commit", "-m", "second")
	runGit(t, worktree, "switch", "-q", "-c", "other")
	runGit(t, worktree, "switch", "-q", testBranch)
	runGit(t, worktree, "push", "-q", "origin", testBranch, "other", ":feature")

	run()
	for _, ref := range []plumbing.ReferenceName{
		plumbing.NewBranchReferenceName(testBranch),
		plumbing.NewBranchReferenceName("other"),
		plumbing.NewTagReferenceName("v1"),
	} {
		assertGitRefEqual(t, sourceBare, targetBare, ref)
	}
	if out := runGit(t, targetBare, "for-each-ref", "refs/heads/feature"); strings.TrimSpace(out) != "" {
		t.Fatalf("feature not pruned on target: %q", out)
	}
	runGit(t, targetBare, "fsck", "--connectivity-only")
}

// TestReplicate_GitHTTPBackendSplitsTrunkAcrossPushes covers a trunk too big
// for one push: the batched bootstrap must walk the trunk's commit parents to
// cut it into pushes under the target's body limit. The parents come from a
// tree:0 pack whose commit headers carry 64-hex ids in a sha256 repository,
// so a parser that only knows 40-hex ids sees a one-commit chain and the
// single oversized push hits the 413.
func TestReplicate_GitHTTPBackendSplitsTrunkAcrossPushes(t *testing.T) {
	if os.Getenv(gitHTTPBackendEnv) == "" {
		t.Skip("set GITSYNC_E2E_GIT_HTTP_BACKEND=1 to run git-http-backend integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}

	for _, objectFormat := range []string{"sha1", "sha256"} {
		t.Run(objectFormat, func(t *testing.T) {
			root := t.TempDir()
			sourceBare := filepath.Join(root, "source.git")
			targetBare := filepath.Join(root, "target.git")
			worktree := filepath.Join(root, "work")
			format := "--object-format=" + objectFormat

			runGit(t, root, "init", "--bare", format, sourceBare)
			runGit(t, sourceBare, "config", "uploadpack.allowFilter", "true")
			runGit(t, sourceBare, "config", "uploadpack.allowReachableSHA1InWant", "true")
			runGit(t, root, "init", "--bare", format, targetBare)
			runGit(t, targetBare, "config", "http.receivepack", "true")
			runGit(t, root, "init", format, "-b", testBranch, worktree)
			runGit(t, worktree, "config", "user.name", "git-sync test")
			runGit(t, worktree, "config", "user.email", "git-sync@example.com")
			runGit(t, worktree, "remote", "add", "origin", sourceBare)
			for i := range 3 {
				writePseudoRandomFile(t, filepath.Join(worktree, fmt.Sprintf("trunk-%d.bin", i)), int64(150_000+i*13))
				runGit(t, worktree, "add", ".")
				runGit(t, worktree, "commit", "-m", fmt.Sprintf("trunk-%d", i))
			}
			runGit(t, worktree, "push", "-q", "origin", testBranch)
			runGit(t, sourceBare, "symbolic-ref", "HEAD", "refs/heads/"+testBranch)

			server := newGitHTTPBackendServer(t, root)
			defer server.Close()
			// Room for any one commit, not for the whole trunk.
			server.maxPushBody.Store(325_000)

			// Each checkpoint's upload diagnostics; the pack observer scans in
			// the source's format, so a landed checkpoint must not report an
			// observer failure.
			var diagnostics bytes.Buffer
			result, err := Run(context.Background(), Config{
				Source:          Endpoint{URL: server.RepoURL("source.git")},
				Target:          Endpoint{URL: server.RepoURL("target.git")},
				Mode:            modeReplicate,
				Prune:           true,
				IncludeTags:     true,
				BootstrapLogger: slog.New(slog.NewTextHandler(&diagnostics, nil)),
			})
			t.Logf("relay mode %q, reason %q, pushed %d, batches %d", result.RelayMode, result.RelayReason, result.Pushed, result.BatchCount)
			if err != nil {
				t.Fatalf("replicate failed: %v", err)
			}
			if result.BatchCount < 2 {
				t.Fatalf("batches = %d, want the trunk split across at least 2 pushes", result.BatchCount)
			}
			assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewBranchReferenceName(testBranch))
			runGit(t, targetBare, "fsck", "--connectivity-only")
			if !strings.Contains(diagnostics.String(), "observer_failed=false") {
				t.Fatalf("no checkpoint upload diagnostics logged:\n%s", diagnostics.String())
			}
			if strings.Contains(diagnostics.String(), "observer_failed=true") {
				t.Fatalf("a landed checkpoint reported observer_failed=true:\n%s", diagnostics.String())
			}
		})
	}
}

// TestRun_GitHTTPBackendSyncObjectFormatMismatch syncs between repositories of
// different object formats. git-sync relays the source's ids and packs
// unchanged and cannot convert them, so the run must stop once both formats
// are known, before fetching anything, with an error naming both.
func TestRun_GitHTTPBackendSyncObjectFormatMismatch(t *testing.T) {
	if os.Getenv(gitHTTPBackendEnv) == "" {
		t.Skip("set GITSYNC_E2E_GIT_HTTP_BACKEND=1 to run git-http-backend integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}

	for _, tc := range []struct{ source, target string }{
		{source: "sha1", target: "sha256"},
		{source: "sha256", target: "sha1"},
	} {
		t.Run(tc.source+"_to_"+tc.target, func(t *testing.T) {
			root := t.TempDir()
			sourceBare := filepath.Join(root, "source.git")
			targetBare := filepath.Join(root, "target.git")
			worktree := filepath.Join(root, "work")

			runGit(t, root, "init", "--bare", "--object-format="+tc.source, sourceBare)
			runGit(t, root, "init", "--bare", "--object-format="+tc.target, targetBare)
			runGit(t, targetBare, "config", "http.receivepack", "true")
			runGit(t, root, "init", "--object-format="+tc.source, "-b", testBranch, worktree)
			runGit(t, worktree, "config", "user.name", "git-sync test")
			runGit(t, worktree, "config", "user.email", "git-sync@example.com")
			writeFile(t, filepath.Join(worktree, "README.md"), "one\n")
			runGit(t, worktree, "add", "README.md")
			runGit(t, worktree, "commit", "-m", "initial")
			runGit(t, worktree, "push", "-q", sourceBare, "HEAD:refs/heads/"+testBranch)

			server := newGitHTTPBackendServer(t, root)
			defer server.Close()

			base := Config{
				Source: Endpoint{URL: server.RepoURL("source.git")},
				Target: Endpoint{URL: server.RepoURL("target.git")},
			}
			// Every operation that would push: sync, plan, replicate, bootstrap.
			// A SHA256 source syncs into a SHA1 target by translating objects,
			// so there only replicate and bootstrap must refuse.
			translates := tc.source == "sha256" && tc.target == "sha1"
			for name, run := range map[string]func(Config) (Result, error){
				"sync": func(c Config) (Result, error) { c.Mode = modeSync; return Run(context.Background(), c) },
				"plan": func(c Config) (Result, error) {
					c.Mode = modeSync
					c.DryRun = true
					return Run(context.Background(), c)
				},
				"replicate": func(c Config) (Result, error) { c.Mode = modeReplicate; return Run(context.Background(), c) },
				"bootstrap": func(c Config) (Result, error) { return Bootstrap(context.Background(), c) },
			} {
				if translates && (name == "sync" || name == "plan") {
					continue
				}
				_, err := run(base)
				if !errors.Is(err, gitproto.ErrObjectFormatMismatch) {
					t.Fatalf("%s: err = %v, want ErrObjectFormatMismatch", name, err)
				}
				for _, want := range []string{"source is " + tc.source, "target is " + tc.target} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%s: error %q does not contain %q", name, err, want)
					}
				}
			}
			if n := server.uploadPackFetches.Load(); n != 0 {
				t.Errorf("fetched from the source %d times before failing, want 0", n)
			}

			// Probe is diagnostic: it must still show what each endpoint
			// advertised, the formats included, rather than fail.
			probe, err := Probe(context.Background(), Config{
				Source: Endpoint{URL: server.RepoURL("source.git")},
				Target: Endpoint{URL: server.RepoURL("target.git")},
			})
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if len(probe.Refs) == 0 {
				t.Errorf("probe returned no source refs")
			}
			for side, caps := range map[string][]string{"source": probe.Capabilities, "target": probe.TargetCaps} {
				format := tc.source
				if side == "target" {
					format = tc.target
				}
				if !slices.Contains(caps, "object-format="+format) {
					t.Errorf("probe %s capabilities %q lack object-format=%s", side, caps, format)
				}
			}
			if out := runGit(t, targetBare, "for-each-ref"); strings.TrimSpace(out) != "" {
				t.Errorf("target changed: %q", out)
			}
			if translates {
				if _, err := Run(context.Background(), Config{
					Source: base.Source, Target: base.Target, Mode: modeSync,
				}); err != nil {
					t.Fatalf("translated sync: %v", err)
				}
			}
		})
	}
}

// TestCheckTargetHas_GitHTTPBackendNotesOnlyTarget covers a target whose only
// ref is refs/notes/sha256-origin, as after a non-atomic push that landed the
// notes and had its branch refused. The commits the notes name are on the
// target, though no ref reaches them.
func TestCheckTargetHas_GitHTTPBackendNotesOnlyTarget(t *testing.T) {
	if os.Getenv(gitHTTPBackendEnv) == "" {
		t.Skip("set GITSYNC_E2E_GIT_HTTP_BACKEND=1 to run git-http-backend integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}

	root := t.TempDir()
	targetBare := filepath.Join(root, "target.git")
	worktree := filepath.Join(root, "work")
	runGit(t, root, "init", "--bare", targetBare)
	runGit(t, targetBare, "config", "http.receivepack", "true")
	runGit(t, targetBare, "config", "uploadpack.allowReachableSHA1InWant", "false")
	runGit(t, targetBare, "config", "uploadpack.allowAnySHA1InWant", "false")
	runGit(t, root, "init", "--bare", filepath.Join(root, "source.git"))
	runGit(t, root, "init", "-b", testBranch, worktree)
	runGit(t, worktree, "config", "user.name", "git-sync test")
	runGit(t, worktree, "config", "user.email", "git-sync@example.com")
	runGit(t, worktree, "commit", "--allow-empty", "-m", "landed without its branch")
	commit := plumbing.NewHash(strings.TrimSpace(runGit(t, worktree, "rev-parse", "HEAD")))
	runGit(t, worktree, "notes", "--ref=sha256-origin", "add", "-m", strings.Repeat("ab", 32), "HEAD")
	runGit(t, worktree, "push", targetBare, "refs/notes/sha256-origin", "HEAD:refs/heads/"+testBranch)
	runGit(t, targetBare, "update-ref", "-d", "refs/heads/"+testBranch)

	server := newGitHTTPBackendServer(t, root)
	defer server.Close()
	targetURL := server.RepoURL("target.git")
	missing := plumbing.NewHash(strings.Repeat("7", 40))

	for _, protocol := range []string{"v1", "v2"} {
		s, err := newSession(t.Context(), Config{
			Source:       Endpoint{URL: server.RepoURL("source.git")},
			Target:       Endpoint{URL: targetURL},
			ProtocolMode: protocol,
		}, true)
		if err != nil {
			t.Fatalf("%s: new session: %v", protocol, err)
		}
		if _, err := s.checkTargetHas(t.Context(), []translate.Pair{{Target: commit}}, nil); err != nil {
			t.Errorf("%s: target holds %s, got %v", protocol, commit, err)
		}
		if _, err := s.checkTargetHas(t.Context(), []translate.Pair{{Target: missing}}, nil); err == nil || !strings.Contains(err.Error(), "does not have") {
			t.Errorf("%s: target lacks %s, got %v", protocol, missing, err)
		}
		recorded := plumbing.NewHash(strings.Repeat("8", 64))
		lost, err := s.checkTargetHas(t.Context(), []translate.Pair{{Source: recorded, Target: missing}},
			map[plumbing.Hash]struct{}{recorded: {}})
		if err != nil || !slices.Equal(lost, []plumbing.Hash{recorded}) {
			t.Errorf("%s: a commit only the target's notes record: got %v, %v, want it returned to translate again", protocol, lost, err)
		}
		s.finish()
	}
}
