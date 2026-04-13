package main

import (
	"testing"

	"github.com/soph/git-sync/pkg/gitsync/unstable"
)

func TestSummarizeRuns(t *testing.T) {
	runs := []runSummary{
		{
			WallMillis: 100,
			Result: unstable.Result{
				RelayMode: "bootstrap",
				Batching:  false,
				Measurement: unstable.Measurement{
					ElapsedMillis:      90,
					PeakAllocBytes:     10,
					PeakHeapInuseBytes: 20,
					TotalAllocBytes:    30,
					GCCount:            1,
				},
			},
		},
		{
			WallMillis: 140,
			Result: unstable.Result{
				RelayMode:         "bootstrap-batch",
				Batching:          true,
				BatchCount:        3,
				PlannedBatchCount: 4,
				Measurement: unstable.Measurement{
					ElapsedMillis:      130,
					PeakAllocBytes:     50,
					PeakHeapInuseBytes: 60,
					TotalAllocBytes:    70,
					GCCount:            2,
				},
			},
		},
		{
			Error: "boom",
		},
	}

	got := summarizeRuns(runs)
	if got.SuccessfulRuns != 2 || got.FailedRuns != 1 {
		t.Fatalf("unexpected run counts: %+v", got)
	}
	if got.MinWallMillis != 100 || got.MaxWallMillis != 140 {
		t.Fatalf("unexpected wall bounds: %+v", got)
	}
	if got.AvgWallMillis != 120 {
		t.Fatalf("unexpected avg wall: %+v", got)
	}
	if got.MinSyncElapsedMillis != 90 || got.MaxSyncElapsedMillis != 130 {
		t.Fatalf("unexpected elapsed bounds: %+v", got)
	}
	if got.AvgSyncElapsedMillis != 110 {
		t.Fatalf("unexpected avg elapsed: %+v", got)
	}
	if got.BatchedRuns != 1 {
		t.Fatalf("unexpected batched run count: %+v", got)
	}
	if got.MinBatchCount != 3 || got.MaxBatchCount != 3 || got.AvgBatchCount != 3 {
		t.Fatalf("unexpected batch count summary: %+v", got)
	}
	if got.MinPlannedBatchCount != 4 || got.MaxPlannedBatchCount != 4 || got.AvgPlannedBatchCount != 4 {
		t.Fatalf("unexpected planned batch count summary: %+v", got)
	}
	if got.MaxPeakAllocBytes != 50 || got.MaxPeakHeapInuseBytes != 60 || got.MaxTotalAllocBytes != 70 || got.MaxGCCount != 2 {
		t.Fatalf("unexpected maxima: %+v", got)
	}
	if len(got.RelayModes) != 2 || got.RelayModes[0] != "bootstrap" || got.RelayModes[1] != "bootstrap-batch" {
		t.Fatalf("unexpected relay modes: %+v", got.RelayModes)
	}
}

func TestNormalizeRepoURL(t *testing.T) {
	got, err := normalizeRepoURL("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("normalizeRepoURL: %v", err)
	}
	if got != "https://example.com/repo.git" {
		t.Fatalf("unexpected url: %s", got)
	}
}
