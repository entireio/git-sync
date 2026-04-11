package syncer

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/soph/git-sync/internal/gitproto"
	"github.com/soph/git-sync/internal/planner"
	bstrap "github.com/soph/git-sync/internal/strategy/bootstrap"
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
	if !result.Relay || result.RelayMode != "incremental" {
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
		Source:            Endpoint{URL: sourceURL},
		Target:            Endpoint{URL: targetURL},
		BatchMaxPackBytes: 350_000,
	})
	if err != nil {
		t.Fatalf("batched bootstrap failed: %v\nbackend-stderr:\n%s", err, server.Stderr())
	}
	if result.Pushed != 1 || result.Blocked != 0 {
		t.Fatalf("unexpected batched bootstrap result: %+v", result)
	}
	if !result.Relay || result.RelayMode != "bootstrap-batch" || !result.Batching {
		t.Fatalf("expected batched bootstrap relay result, got %+v", result)
	}
	if result.BatchCount < 2 {
		t.Fatalf("expected multiple bootstrap batches, got %+v", result)
	}
	if result.PlannedBatchCount != result.BatchCount {
		t.Fatalf("expected fresh batched bootstrap to complete all planned batches, got %+v", result)
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
		Source:            Endpoint{URL: sourceURL},
		Target:            Endpoint{URL: targetURL},
		BatchMaxPackBytes: 350_000,
		ProtocolMode:      protocolModeAuto,
	}

	stats := newStats(false)
	sourceConn, err := newConn(cfg.Source, "source", stats)
	if err != nil {
		t.Fatalf("create source transport: %v", err)
	}
	sourceRefs, sourceService, err := gitproto.ListSourceRefs(context.Background(), sourceConn, cfg.ProtocolMode, planner.RefPrefixes(cfg.Mappings, cfg.IncludeTags))
	if err != nil {
		t.Fatalf("list source refs: %v", err)
	}
	desired, _, err := planner.BuildDesiredRefs(gitproto.RefHashMap(sourceRefs), planner.PlanConfig{
		Branches: cfg.Branches, Mappings: cfg.Mappings, IncludeTags: cfg.IncludeTags,
		Force: cfg.Force, Prune: cfg.Prune,
	})
	if err != nil {
		t.Fatalf("build desired refs: %v", err)
	}
	ref := desired[plumbing.NewBranchReferenceName(testBranch)]
	bParams := bstrap.Params{
		SourceConn: sourceConn, SourceService: sourceService,
		BatchMaxPack: cfg.BatchMaxPackBytes, Verbose: cfg.Verbose,
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
	if !result.Relay || result.RelayMode != "bootstrap-batch" || !result.Batching {
		t.Fatalf("expected batched bootstrap resume relay result, got %+v", result)
	}
	if result.BatchCount >= len(checkpoints) {
		t.Fatalf("expected resume to execute fewer batches than full run, got %+v checkpoints=%d", result, len(checkpoints))
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
		Source:            Endpoint{URL: sourceURL},
		Target:            Endpoint{URL: targetURL},
		IncludeTags:       true,
		BatchMaxPackBytes: 350_000,
	})
	if err != nil {
		t.Fatalf("batched bootstrap with tags failed: %v\nbackend-stderr:\n%s", err, server.Stderr())
	}
	if result.Pushed != 2 || result.Blocked != 0 {
		t.Fatalf("unexpected batched bootstrap with tags result: %+v", result)
	}
	if !result.Relay || result.RelayMode != "bootstrap-batch" || !result.Batching {
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

func assertGitRefAbsent(t *testing.T, repoPath string, ref plumbing.ReferenceName) {
	t.Helper()
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", ref.String())
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
