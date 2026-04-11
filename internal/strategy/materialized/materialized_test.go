package materialized

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/soph/git-sync/internal/convert"
	"github.com/soph/git-sync/internal/gitproto"
	"github.com/soph/git-sync/internal/planner"
)

func TestPlansToPushPlans(t *testing.T) {
	hash1 := plumbing.NewHash("1111111111111111111111111111111111111111")
	hash2 := plumbing.NewHash("2222222222222222222222222222222222222222")
	hash3 := plumbing.NewHash("3333333333333333333333333333333333333333")

	tests := []struct {
		name   string
		plans  []planner.BranchPlan
		expect []gitproto.PushPlan
	}{
		{
			name: "create plan",
			plans: []planner.BranchPlan{
				{
					TargetRef:  plumbing.NewBranchReferenceName("main"),
					TargetHash: plumbing.ZeroHash,
					SourceHash: hash1,
					Action:     planner.ActionCreate,
				},
			},
			expect: []gitproto.PushPlan{
				{
					TargetRef:  plumbing.NewBranchReferenceName("main"),
					TargetHash: plumbing.ZeroHash,
					SourceHash: hash1,
					Delete:     false,
				},
			},
		},
		{
			name: "update plan",
			plans: []planner.BranchPlan{
				{
					TargetRef:  plumbing.NewBranchReferenceName("main"),
					TargetHash: hash1,
					SourceHash: hash2,
					Action:     planner.ActionUpdate,
				},
			},
			expect: []gitproto.PushPlan{
				{
					TargetRef:  plumbing.NewBranchReferenceName("main"),
					TargetHash: hash1,
					SourceHash: hash2,
					Delete:     false,
				},
			},
		},
		{
			name: "delete plan",
			plans: []planner.BranchPlan{
				{
					TargetRef:  plumbing.NewBranchReferenceName("old"),
					TargetHash: hash3,
					SourceHash: plumbing.ZeroHash,
					Action:     planner.ActionDelete,
				},
			},
			expect: []gitproto.PushPlan{
				{
					TargetRef:  plumbing.NewBranchReferenceName("old"),
					TargetHash: hash3,
					SourceHash: plumbing.ZeroHash,
					Delete:     true,
				},
			},
		},
		{
			name:   "empty input",
			plans:  []planner.BranchPlan{},
			expect: []gitproto.PushPlan{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convert.PlansToPushPlans(tt.plans)
			if len(got) != len(tt.expect) {
				t.Fatalf("expected %d push plans, got %d", len(tt.expect), len(got))
			}
			for i := range tt.expect {
				if got[i].TargetRef != tt.expect[i].TargetRef {
					t.Errorf("[%d] TargetRef = %s, want %s", i, got[i].TargetRef, tt.expect[i].TargetRef)
				}
				if got[i].TargetHash != tt.expect[i].TargetHash {
					t.Errorf("[%d] TargetHash = %s, want %s", i, got[i].TargetHash, tt.expect[i].TargetHash)
				}
				if got[i].SourceHash != tt.expect[i].SourceHash {
					t.Errorf("[%d] SourceHash = %s, want %s", i, got[i].SourceHash, tt.expect[i].SourceHash)
				}
				if got[i].Delete != tt.expect[i].Delete {
					t.Errorf("[%d] Delete = %v, want %v", i, got[i].Delete, tt.expect[i].Delete)
				}
			}
		})
	}
}

func TestMaxMaterializedObjectsExported(t *testing.T) {
	// Verify the constant is exported and has a reasonable positive value.
	if MaxMaterializedObjects <= 0 {
		t.Fatalf("MaxMaterializedObjects should be positive, got %d", MaxMaterializedObjects)
	}
	// Sanity: it should be at least 1000 to be useful for real repos,
	// but not so large that it defeats its purpose as a safety limit.
	if MaxMaterializedObjects < 1_000 {
		t.Fatalf("MaxMaterializedObjects too small: %d", MaxMaterializedObjects)
	}
	if MaxMaterializedObjects > 10_000_000 {
		t.Fatalf("MaxMaterializedObjects unreasonably large: %d", MaxMaterializedObjects)
	}
}
