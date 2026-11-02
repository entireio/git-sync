package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/storage/memory"
)

// An embedder's BootstrapLogger, not -v, is how a production bootstrap is
// observed. The planning lines must reach it with the embedder's attributes,
// or a run that stalls before its first push leaves nothing between the size
// lookup and a timeout.
func TestBootstrapPlanningLinesReachBootstrapLoggerWithoutVerbose(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeLargeCommits(t, sourceRepo, sourceFS, 3, 200_000)
	targetRepo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}
	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil)).With(slog.String("sync_id", "sync-1"))
	result, err := Bootstrap(context.Background(), Config{
		Source:             Endpoint{URL: sourceServer.RepoURL()},
		Target:             Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode:       protocolModeAuto,
		TargetMaxPackBytes: 350_000,
		BootstrapLogger:    logger,
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !result.Batching {
		t.Fatalf("expected a batched bootstrap, got %+v", result)
	}

	seen := map[any]bool{}
	dec := json.NewDecoder(&out)
	for dec.More() {
		var record map[string]any
		if err := dec.Decode(&record); err != nil {
			t.Fatalf("decode log record: %v", err)
		}
		if record["sync_id"] != "sync-1" {
			t.Fatalf("record %v lost the embedder's attributes: %v", record["msg"], record)
		}
		seen[record["msg"]] = true
	}
	for _, msg := range []string{
		"bootstrap batch planning checkpoints",
		"bootstrap batch fetching commit graph",
		"bootstrap batch planned checkpoints",
		"bootstrap batch branch finalized",
	} {
		if !seen[msg] {
			t.Errorf("BootstrapLogger missing planning line %q; saw %v", msg, seen)
		}
	}
}
