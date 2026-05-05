package syncer

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/storage/memory"
)

// TestBootstrap_ThroughputLineAndProgressTicker verifies end-to-end that
// running a real bootstrap with --stats and --progress populates per-side
// byte counters, prints a throughput line in the human output, and emits
// at least one render to the progress writer when progressOut is wired.
func TestBootstrap_ThroughputLineAndProgressTicker(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 4)

	targetRepo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	var progressBuf bytes.Buffer
	cfg := Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		Target:       Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode: protocolModeAuto,
		ShowStats:    true,
		Progress:     true,
		progressOut:  &progressBuf,
	}

	result, err := Bootstrap(context.Background(), cfg)
	if err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}
	if result.Pushed != 1 {
		t.Fatalf("expected one push, got %+v", result)
	}

	// Per-side counters should record bytes on both sides.
	sides := map[string]int64{}
	for _, side := range result.Stats.Sides {
		sides[side.Label] = side.Bytes
	}
	if sides["source"] <= 0 {
		t.Errorf("expected non-zero source bytes, got %d", sides["source"])
	}
	if sides["target"] <= 0 {
		t.Errorf("expected non-zero target bytes, got %d", sides["target"])
	}
	if result.Stats.ElapsedNanos <= 0 {
		t.Errorf("expected non-zero elapsed nanos, got %d", result.Stats.ElapsedNanos)
	}

	// Per-side displays should reflect the test server hostnames.
	displays := map[string]string{}
	for _, side := range result.Stats.Sides {
		displays[side.Label] = side.Display
	}
	if displays["source"] == "" || displays["target"] == "" {
		t.Errorf("expected non-empty display for both sides, got %+v", result.Stats.Sides)
	}

	// Human-formatted output should include the new throughput line.
	out := strings.Join(result.Lines(), "\n")
	if !strings.Contains(out, "throughput: ") {
		t.Errorf("missing throughput line in output:\n%s", out)
	}
	if !strings.Contains(out, displays["source"]) || !strings.Contains(out, displays["target"]) {
		t.Errorf("throughput line should mention both hostnames:\n%s", out)
	}
	if !strings.Contains(out, "→") || !strings.Contains(out, "│") {
		t.Errorf("throughput line should use arrow + vertical bar separator:\n%s", out)
	}

	// Progress writer should have received at least one frame. The bootstrap
	// is very fast so we may only get the final terminate() render.
	if progressBuf.Len() == 0 {
		// Allow a brief grace window in case the goroutine had not flushed
		// yet. terminate() inside finish() blocks on render so this should
		// be unnecessary, but keep a small fallback for slow CI.
		time.Sleep(50 * time.Millisecond)
	}
	if progressBuf.Len() == 0 {
		t.Errorf("expected progress reporter to write at least one frame")
	}
	if !bytes.Contains(progressBuf.Bytes(), []byte(displays["source"])) {
		t.Errorf("progress output should mention source host %q: %q",
			displays["source"], progressBuf.String())
	}
	if !bytes.Contains(progressBuf.Bytes(), []byte(displays["target"])) {
		t.Errorf("progress output should mention target host %q: %q",
			displays["target"], progressBuf.String())
	}

	// Print samples for human inspection when running with -v.
	for _, line := range result.Lines() {
		if strings.HasPrefix(line, "stats:") || strings.HasPrefix(line, "throughput:") {
			t.Logf("output line: %s", line)
		}
	}
	t.Logf("progress writer:\n%q", progressBuf.String())
}
