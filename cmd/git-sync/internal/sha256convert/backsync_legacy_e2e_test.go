package sha256convert

import (
	"context"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitsync "entire.io/entire/git-sync"
)

// TestBackSync_LegacyNormalizedTree syncs back from a SHA256 repository
// made by a convert-sha256 release that decoded and re-encoded trees. Those
// releases wrote a mode such as 100011 as 100644 and dropped the zero from
// 040000, so the converted base commit's tree does not convert back to the
// original SHA1 tree. A post-cutover commit that reuses that tree needs its
// SHA1 translation pushed even though the target holds the base commit.
func TestBackSync_LegacyNormalizedTree(t *testing.T) {
	if os.Getenv(gitHTTPBackendEnv) == "" {
		t.Skipf("set %s=1 to run the SHA256 to SHA1 back-sync end-to-end test", gitHTTPBackendEnv)
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git binary not available: %v", err)
	}

	const token = "back-sync-token"
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	canonical := filepath.Join(root, "canonical.git")
	oracle := filepath.Join(root, "oracle.git")

	gitAt(t, root, "", "init", "--bare", "--object-format=sha1", "--initial-branch=main", origin)
	gitAt(t, origin, "", "config", "http.receivepack", "true")
	gitAt(t, origin, "", "config", "uploadpack.allowReachableSHA1InWant", "false")
	gitAt(t, origin, "", "config", "uploadpack.allowAnySHA1InWant", "false")
	blob := writeRawObject(t, origin, "blob", []byte("hello\n"))
	inner := writeRawObject(t, origin, "tree", treeEntry("100644", "f", blob))
	tree := writeRawObject(t, origin, "tree", append(treeEntry("100011", "a", blob), treeEntry("040000", "d", inner)...))
	base := commitTreeAt(t, origin, "1700000000 +0000", tree, "", "base")
	gitAt(t, origin, "", "update-ref", "refs/heads/main", base)
	gitAt(t, root, "", "clone", "--bare", origin, oracle)

	gitAt(t, root, "", "init", "--bare", "--object-format=sha256", "--initial-branch=main", canonical)
	gitAt(t, canonical, "", "config", "http.receivepack", "true")
	blob256 := writeRawObject(t, canonical, "blob", []byte("hello\n"))
	inner256 := writeRawObject(t, canonical, "tree", treeEntry("100644", "f", blob256))
	tree256 := writeRawObject(t, canonical, "tree", append(treeEntry("100644", "a", blob256), treeEntry("40000", "d", inner256)...))
	base256 := commitTreeAt(t, canonical, "1700000000 +0000", tree256, "", "base")
	gitAt(t, canonical, "", "-c", "user.name=a", "-c", "user.email=a@example.com",
		"notes", "--ref=refs/notes/sha1-origin", "add", "-m", base, base256)
	next256 := commitTreeAt(t, canonical, "1700001000 +0000", tree256, base256, "empty")
	gitAt(t, canonical, "", "update-ref", "refs/heads/main", next256)

	normalized := writeRawObject(t, oracle, "tree", append(treeEntry("100644", "a", blob), treeEntry("40000", "d", inner)...))
	want := commitTreeAt(t, oracle, "1700001000 +0000", normalized, base, "empty")

	srv := newReadOnlyOriginServer(t, gitBin, root, "origin.git", token)
	defer srv.Close()
	client := gitsync.New(gitsync.Options{
		HTTPClient: srv.Client(),
		Auth: gitsync.StaticAuthProvider{
			Target: gitsync.EndpointAuth{Username: "git-sync", Token: token},
		},
	})
	res, err := client.Sync(context.Background(), gitsync.SyncRequest{
		Source: gitsync.Endpoint{URL: srv.URL + "/canonical.git"},
		Target: gitsync.Endpoint{URL: srv.URL + "/origin.git"},
		Scope:  gitsync.RefScope{Branches: []string{"main"}},
		Policy: gitsync.SyncPolicy{Protocol: gitsync.ProtocolAuto},
	})
	if err != nil {
		t.Fatalf("back-sync: %v", err)
	}
	if res.Counts.Applied != 1 {
		t.Fatalf("back-sync: want main applied, got %+v", res.Refs)
	}
	if got := revParse(t, origin, "refs/heads/main"); got != want {
		t.Fatalf("main: got %s, want %s", got, want)
	}
	if out, err := exec.CommandContext(t.Context(), "git", "-C", origin, "fsck", "--connectivity-only").CombinedOutput(); err != nil {
		t.Fatalf("git fsck --connectivity-only: %v\n%s", err, out)
	}
}

func treeEntry(mode, name, oid string) []byte {
	raw, err := hex.DecodeString(oid)
	if err != nil {
		panic(err)
	}
	return append([]byte(mode+" "+name+"\x00"), raw...)
}

// writeRawObject writes content as is. Git refuses the odd tree modes these
// tests need unless asked to write the object literally.
func writeRawObject(t *testing.T, repo, typ string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "object")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(mustGitOutput(t, repo, "hash-object", "-w", "--literally", "-t", typ, path))
}

func commitTreeAt(t *testing.T, repo, date, tree, parent, msg string) string {
	t.Helper()
	args := []string{"commit-tree", tree, "-m", msg}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date,
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com",
		"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git commit-tree: %v", err)
	}
	return strings.TrimSpace(string(out))
}
