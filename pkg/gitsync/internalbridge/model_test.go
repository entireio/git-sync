package internalbridge

import (
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entirehq/git-sync/internal/planner"
	"github.com/entirehq/git-sync/internal/syncer"
)

func TestHashStringZeroHashIsEmpty(t *testing.T) {
	got := HashString(plumbing.ZeroHash)
	if got != "" {
		t.Fatalf("HashString(zero) = %q, want empty string", got)
	}
}

func TestFromProbeResultCopiesStableFields(t *testing.T) {
	got := FromProbeResult(syncer.ProbeResult{
		SourceURL:     "https://source.example/repo.git",
		TargetURL:     "https://target.example/repo.git",
		RequestedMode: "auto",
		Protocol:      "v2",
		RefPrefixes:   []string{"refs/heads/"},
		Capabilities:  []string{"ls-refs", "fetch"},
		TargetCaps:    []string{"report-status"},
		Refs: []syncer.RefInfo{
			{Name: "refs/heads/main", Hash: plumbing.NewHash("1111111111111111111111111111111111111111")},
		},
		Stats: syncer.Stats{
			Enabled: true,
			Items: map[string]*syncer.ServiceStats{
				"source": {Name: "source", Requests: 2, Wants: 3},
			},
		},
		Measurement: syncer.Measurement{Enabled: true, ElapsedMillis: 42},
	})

	if got.SourceURL != "https://source.example/repo.git" || got.TargetURL != "https://target.example/repo.git" {
		t.Fatalf("unexpected URLs: %+v", got)
	}
	if len(got.Refs) != 1 || got.Refs[0].Hash != "1111111111111111111111111111111111111111" {
		t.Fatalf("unexpected refs: %+v", got.Refs)
	}
	if !got.Stats.Enabled || got.Stats.Items["source"].Requests != 2 || got.Measurement.ElapsedMillis != 42 {
		t.Fatalf("unexpected stats/measurement: %+v %+v", got.Stats, got.Measurement)
	}
}

func TestFromSyncResultShapesStableSummary(t *testing.T) {
	got := FromSyncResult(syncer.Result{
		Plans: []planner.BranchPlan{
			{
				Branch:     "main",
				SourceRef:  plumbing.ReferenceName("refs/heads/main"),
				TargetRef:  plumbing.ReferenceName("refs/heads/main"),
				SourceHash: plumbing.NewHash("1111111111111111111111111111111111111111"),
				TargetHash: plumbing.NewHash("2222222222222222222222222222222222222222"),
				Kind:       planner.RefKindBranch,
				Action:     planner.ActionUpdate,
				Reason:     "fast-forward",
			},
		},
		Pushed:             1,
		Skipped:            2,
		Blocked:            3,
		Deleted:            4,
		DryRun:             true,
		OperationMode:      "replicate",
		Relay:              true,
		RelayMode:          "incremental-relay",
		RelayReason:        "fast-forward",
		Batching:           true,
		BatchCount:         5,
		PlannedBatchCount:  6,
		TempRefs:           []string{"refs/gitsync/bootstrap/heads/main/1"},
		BootstrapSuggested: true,
		Protocol:           "v2",
	})

	if len(got.Refs) != 1 || got.Refs[0].Branch != "main" {
		t.Fatalf("unexpected refs: %+v", got.Refs)
	}
	if got.Counts.Applied != 1 || got.Counts.Skipped != 2 || got.Counts.Blocked != 3 || got.Counts.Deleted != 4 {
		t.Fatalf("unexpected counts: %+v", got.Counts)
	}
	if !got.Execution.DryRun || !got.Execution.Relay || got.Execution.OperationMode != "replicate" || got.Execution.TransferMode != "incremental-relay" || got.Execution.Reason != "fast-forward" {
		t.Fatalf("unexpected execution summary: %+v", got.Execution)
	}
	if !got.Execution.Batch.Enabled || got.Execution.Batch.Done != 5 || got.Execution.Batch.Planned != 6 {
		t.Fatalf("unexpected batch summary: %+v", got.Execution.Batch)
	}
	if !got.Execution.BootstrapSuggested {
		t.Fatalf("expected bootstrap suggestion in execution summary")
	}
}
