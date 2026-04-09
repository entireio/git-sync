package syncer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

const liveLinuxEnv = "GITSYNC_E2E_LIVE_LINUX"

func TestBootstrap_LiveLinuxSource(t *testing.T) {
	if os.Getenv(liveLinuxEnv) == "" {
		t.Skip("set GITSYNC_E2E_LIVE_LINUX=1 to run the live linux bootstrap smoke test")
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}

	root := t.TempDir()
	targetBare := filepath.Join(root, "target.git")

	runGit(t, root, "init", "--bare", targetBare)
	runGit(t, targetBare, "config", "http.receivepack", "true")

	server := newGitHTTPBackendServer(t, root)
	defer server.Close()

	result, err := Bootstrap(context.Background(), Config{
		Source:        Endpoint{URL: "https://github.com/torvalds/linux.git"},
		Target:        Endpoint{URL: server.RepoURL("target.git")},
		Branches:      []string{"master"},
		ProtocolMode:  protocolModeAuto,
		ShowStats:     true,
		MeasureMemory: true,
	})
	if err != nil {
		t.Fatalf("live linux bootstrap failed: %v\nbackend-stderr:\n%s", err, server.Stderr())
	}
	if result.Protocol == "" {
		t.Fatalf("expected negotiated protocol, got empty result")
	}
	if result.Pushed != 1 {
		t.Fatalf("expected one pushed ref, got %+v", result)
	}
	if !result.Relay || result.RelayMode != "bootstrap" {
		t.Fatalf("expected bootstrap relay result, got %+v", result)
	}
	if !result.Measurement.Enabled {
		t.Fatalf("expected measurement to be enabled")
	}
	if _, err := exec.LookPath("git"); err == nil {
		targetHash := runGit(t, targetBare, "rev-parse", plumbing.NewBranchReferenceName("master").String())
		if targetHash == "" {
			t.Fatalf("expected target master ref to exist")
		}
	}
}
