package syncer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

// pushedPack returns the pack of a receive-pack request: what follows the
// flush that ends its commands or, when the commands ask for push-options,
// the flush that ends the options after them. A malformed request has none.
func pushedPack(body []byte) []byte {
	lists, first := 1, true
	for len(body) >= 4 {
		n, err := strconv.ParseUint(string(body[:4]), 16, 16)
		switch {
		case err != nil || (n > 0 && n < 4) || n > uint64(len(body)):
			return nil
		case n == 0:
			body = body[4:]
			if lists--; lists == 0 {
				return body
			}
			continue
		}
		if first {
			first = false
			_, caps, _ := bytes.Cut(body[4:n], []byte{0})
			if slices.Contains(strings.Fields(string(caps)), "push-options") {
				lists = 2
			}
		}
		body = body[n:]
	}
	return nil
}

func TestPushedPack(t *testing.T) {
	pkt := func(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
	cmd := strings.Repeat("0", 40) + " " + strings.Repeat("1", 40) + " refs/heads/main"
	for _, tc := range []struct {
		name, body, want string
	}{
		{"commands only", pkt(cmd+"\x00report-status\n") + "0000PACK", "PACK"},
		{"push-options", pkt(cmd+"\x00report-status push-options\n") + "0000" + pkt("ci.skip\n") + "0000PACK", "PACK"},
		{"push-options none sent", pkt(cmd+"\x00push-options\n") + "0000" + "0000PACK", "PACK"},
		{"no flush", pkt(cmd + "\x00report-status\n"), ""},
		{"malformed", "zzzz", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(pushedPack([]byte(tc.body))); got != tc.want {
				t.Fatalf("pushedPack = %q, want %q", got, tc.want)
			}
		})
	}
}

// isThinPack reports whether a pack needs objects it does not carry: git
// index-pack, without --fix-thin, cannot resolve it in an empty repository.
func isThinPack(t *testing.T, pack []byte) bool {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "--bare", "-q")
	cmd := exec.CommandContext(t.Context(), "git", "index-pack", "--stdin")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.Stdin = bytes.NewReader(pack)
	out, err := cmd.CombinedOutput()
	switch {
	case err == nil:
		return false
	case strings.Contains(string(out), "unresolved delta"):
		return true
	}
	t.Fatalf("index-pack: %v\n%s", err, out)
	return false
}

// writeLargeText writes a text file of numbered lines, where line changed
// reads differently, so two versions differ in one line.
func writeLargeText(t *testing.T, path string, changed int, word string) {
	t.Helper()
	var b strings.Builder
	for i := range 4000 {
		if i == changed {
			fmt.Fprintf(&b, "line %d %s\n", i, word)
			continue
		}
		fmt.Fprintf(&b, "line %d of a file that the target already holds, %x\n", i, i*7919)
	}
	writeFile(t, path, b.String())
}

// An incremental relay into git's receive-pack, which accepts thin packs, sends
// one: the changed file travels as a delta against the version the target
// holds, and the target completes the pack and stays fsck-clean.
func TestRun_GitHTTPBackendSyncThinPackWhereAccepted(t *testing.T) {
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
	runGit(t, worktree, "remote", "add", "origin", sourceBare)
	writeLargeText(t, filepath.Join(worktree, "large.txt"), 0, "first")
	runGit(t, worktree, "add", "large.txt")
	runGit(t, worktree, "commit", "-m", "initial")
	runGit(t, worktree, "push", "origin", "HEAD:refs/heads/"+testBranch)

	server := newGitHTTPBackendServer(t, root)
	defer server.Close()
	relay := func() Result {
		t.Helper()
		result, err := Run(context.Background(), Config{
			Source: Endpoint{URL: server.RepoURL("source.git")},
			Target: Endpoint{URL: server.RepoURL("target.git")},
		})
		if err != nil {
			t.Fatalf("sync: %v\nbackend-stderr:\n%s", err, server.Stderr())
		}
		return result
	}
	relay()

	writeLargeText(t, filepath.Join(worktree, "large.txt"), 2000, "second")
	runGit(t, worktree, "commit", "-am", "change one line")
	runGit(t, worktree, "push", "origin", "HEAD:refs/heads/"+testBranch)

	if result := relay(); !result.Relay || result.RelayMode != relayModeIncremental {
		t.Fatalf("expected an incremental relay, got %+v", result)
	}
	assertGitRefEqual(t, sourceBare, targetBare, plumbing.NewBranchReferenceName(testBranch))
	runGit(t, targetBare, "fsck", "--strict")
	// The file is about 240 KB and compresses to about 29 KB; a delta for one
	// line is a few hundred bytes.
	pack := server.LastPack(t, "target.git")
	if thin := isThinPack(t, pack); !thin || len(pack) > 4096 {
		t.Fatalf("relayed pack of %d bytes, thin: %v; want a thin pack of at most 4 KiB", len(pack), thin)
	}
}
