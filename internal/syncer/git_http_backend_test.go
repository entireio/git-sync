package syncer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
	bstrap "entire.io/entire/git-sync/internal/strategy/bootstrap"
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
		root: root,
	}
	handler.Stderr = &lockedBuffer{server: s}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// net/http/cgi refuses chunked request bodies, and git-sync streams
		// its pushes. Buffer the body so the CGI gets a Content-Length.
		if len(r.TransferEncoding) > 0 {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.TransferEncoding = nil
		}
		if limit := s.maxPushBody.Load(); limit > 0 && strings.HasSuffix(r.URL.Path, "/git-receive-pack") && r.ContentLength > limit {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	return s
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
	}{
		{name: "empty target", wantReason: planner.ReasonEmptyTargetManagedRefs},
		{name: "trunk already on target", seedTrunk: testBranch, wantReason: planner.ReasonReplicatePackRejected},
		// The trunk moved on since: one update rides along with the creates.
		{name: "trunk behind on target", seedTrunk: testBranch + "~1", wantReason: planner.ReasonReplicatePackRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sourceBare := filepath.Join(root, "source.git")
			targetBare := filepath.Join(root, "target.git")
			worktree := filepath.Join(root, "work")

			runGit(t, root, "init", "--bare", sourceBare)
			runGit(t, sourceBare, "config", "uploadpack.allowFilter", "true")
			runGit(t, sourceBare, "config", "uploadpack.allowReachableSHA1InWant", "true")
			runGit(t, root, "init", "--bare", targetBare)
			runGit(t, targetBare, "config", "http.receivepack", "true")
			runGit(t, root, "init", "-b", testBranch, worktree)
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
