package sha256convert

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	gitsync "entire.io/entire/git-sync"
)

// TestBackSync_NoteWithoutCommit has the target keep a note for a commit it
// no longer holds, as when a non-atomic push lands the notes but not the
// branch and the commit is later pruned. The next sync translates the
// commit again instead of trusting the note.
func TestBackSync_NoteWithoutCommit(t *testing.T) {
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
	seed := filepath.Join(root, "seed")
	dev := filepath.Join(root, "dev")

	gitAt(t, root, "", "init", "--bare", "--object-format=sha1", "--initial-branch=main", origin)
	gitAt(t, origin, "", "config", "http.receivepack", "true")
	gitAt(t, root, "", "init", "-b", "main", seed)
	configIdentity(t, seed)
	mustWrite(t, filepath.Join(seed, "README"), "hello\n")
	gitAt(t, seed, "1700000000 +0000", "add", "README")
	gitAt(t, seed, "1700000000 +0000", "commit", "-m", "initial")
	gitAt(t, seed, "", "push", origin, "main")
	preCutover := revParse(t, origin, "refs/heads/main")

	srv := newReadOnlyOriginServer(t, gitBin, root, "origin.git", token)
	defer srv.Close()
	if _, err := Run(context.Background(), Request{SourceURL: srv.URL + "/origin.git", TargetDir: canonical, Out: io.Discard}); err != nil {
		t.Fatalf("convert-sha256: %v", err)
	}
	gitAt(t, canonical, "", "config", "http.receivepack", "true")
	gitAt(t, root, "", "clone", canonical, dev)
	configIdentity(t, dev)
	mustWrite(t, filepath.Join(dev, "README"), "hello\nagain\n")
	gitAt(t, dev, "1700001000 +0000", "commit", "-am", "post-cutover")
	gitAt(t, dev, "", "push", "origin", "main")

	client := gitsync.New(gitsync.Options{
		HTTPClient: srv.Client(),
		Auth:       gitsync.StaticAuthProvider{Target: gitsync.EndpointAuth{Username: "git-sync", Token: token}},
	})
	req := gitsync.SyncRequest{
		Source: gitsync.Endpoint{URL: srv.URL + "/canonical.git"},
		Target: gitsync.Endpoint{URL: srv.URL + "/origin.git"},
		Scope:  gitsync.RefScope{Branches: []string{"main"}},
		Policy: gitsync.SyncPolicy{Protocol: gitsync.ProtocolAuto},
	}
	if _, err := client.Sync(context.Background(), req); err != nil {
		t.Fatalf("first back-sync: %v", err)
	}
	synced := revParse(t, origin, "refs/heads/main")

	gitAt(t, origin, "", "update-ref", "refs/heads/main", preCutover)
	gitAt(t, origin, "", "reflog", "expire", "--expire=now", "--all")
	gitAt(t, origin, "", "gc", "--prune=now", "--quiet")
	if err := exec.CommandContext(t.Context(), "git", "-C", origin, "cat-file", "-e", synced).Run(); err == nil {
		t.Fatalf("setup: %s survived gc", synced)
	}

	plan, err := client.Plan(context.Background(), gitsync.PlanRequest{Source: req.Source, Target: req.Target, Scope: req.Scope, Policy: req.Policy})
	if err != nil {
		t.Fatalf("plan with a note for a pruned commit: %v", err)
	}
	if len(plan.Refs) != 1 || plan.Refs[0].Action != gitsync.ActionUpdate {
		t.Fatalf("plan: want main updated, got %+v", plan.Refs)
	}
	res, err := client.Sync(context.Background(), req)
	if err != nil {
		t.Fatalf("back-sync with a note for a pruned commit: %v", err)
	}
	if res.Counts.Applied != 1 {
		t.Fatalf("back-sync: want main applied, got %+v", res.Refs)
	}
	if got := revParse(t, origin, "refs/heads/main"); got != synced {
		t.Fatalf("main: got %s, want %s", got, synced)
	}
	assertSHA1AndClean(t, origin)
}
