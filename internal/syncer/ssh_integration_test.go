package syncer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"entire.io/entire/git-sync/internal/gitproto"
	"github.com/go-git/go-git/v6/plumbing"
)

func TestRun_IntegrationSyncOverSSHShimV2(t *testing.T) {
	root := t.TempDir()
	sourceBare := filepath.Join(root, "source.git")
	targetBare := filepath.Join(root, "target.git")
	worktree := filepath.Join(root, "worktree")
	logFile := filepath.Join(root, "ssh.log")
	shim := filepath.Join(root, "ssh-shim.sh")

	runGit(t, root, "init", "--bare", sourceBare)
	runGit(t, root, "init", "--bare", targetBare)
	runGit(t, root, "init", worktree)
	runGit(t, worktree, "config", "user.name", "test")
	runGit(t, worktree, "config", "user.email", "test@example.com")
	writeFile(t, filepath.Join(worktree, "tracked.txt"), "hello over ssh\n")
	runGit(t, worktree, "add", "tracked.txt")
	runGit(t, worktree, "commit", "-m", "initial")
	runGit(t, worktree, "remote", "add", "origin", sourceBare)
	runGit(t, worktree, "push", "origin", "HEAD:refs/heads/"+testBranch)

	shimBody := strings.Join([]string{
		"#!/bin/sh",
		"if [ \"$1\" = \"-o\" ]; then",
		"  shift 2",
		"fi",
		"if [ \"$1\" = \"-p\" ]; then",
		"  shift 2",
		"fi",
		"dest=\"$1\"",
		"remote=\"$2\"",
		"printf '%s\\t%s\\n' \"$dest\" \"$remote\" >>" + shSingleQuote(logFile),
		"exec sh -c \"$remote\"",
	}, "\n")
	if err := os.WriteFile(shim, []byte(shimBody), 0o755); err != nil {
		t.Fatalf("write ssh shim: %v", err)
	}

	orig := gitproto.SSHLookPath
	t.Cleanup(func() { gitproto.SSHLookPath = orig })
	gitproto.SSHLookPath = func(string) (string, error) {
		return shim, nil
	}

	result, err := Run(context.Background(), Config{
		Source:       Endpoint{URL: "ssh://example.com" + sourceBare},
		Target:       Endpoint{URL: "ssh://example.com" + targetBare},
		ProtocolMode: protocolModeAuto,
	})
	if err != nil {
		t.Fatalf("Run over SSH shim failed: %v", err)
	}
	if result.Protocol != protocolModeV2 {
		t.Fatalf("result.Protocol = %q, want %q", result.Protocol, protocolModeV2)
	}

	assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewBranchReferenceName(testBranch))

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read ssh log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 5 {
		t.Fatalf("expected at least 5 ssh invocations, got %d\n%s", len(lines), string(data))
	}

	uploadPackCalls := 0
	receivePackCalls := 0
	v2Calls := 0
	for _, line := range lines {
		switch {
		case strings.Contains(line, "git-upload-pack"):
			uploadPackCalls++
		case strings.Contains(line, "git-receive-pack"):
			receivePackCalls++
		}
		if strings.Contains(line, "GIT_PROTOCOL='version=2' git-upload-pack") {
			v2Calls++
		}
	}
	if uploadPackCalls < 3 {
		t.Fatalf("expected repeated source upload-pack RPCs, got %d\n%s", uploadPackCalls, string(data))
	}
	if receivePackCalls < 2 {
		t.Fatalf("expected target receive-pack discovery + push, got %d\n%s", receivePackCalls, string(data))
	}
	if v2Calls == 0 {
		t.Fatalf("expected at least one protocol v2 upload-pack call\n%s", string(data))
	}
}

func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
