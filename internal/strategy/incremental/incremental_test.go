package incremental

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

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
			got := plansToPushPlans(tt.plans)
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

func TestToGP(t *testing.T) {
	hash1 := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	hash2 := plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	tests := []struct {
		name      string
		desired   map[plumbing.ReferenceName]planner.DesiredRef
		wantIsTag map[plumbing.ReferenceName]bool
	}{
		{
			name: "branch ref sets IsTag false",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				plumbing.NewBranchReferenceName("main"): {
					Kind:       planner.RefKindBranch,
					SourceRef:  plumbing.NewBranchReferenceName("main"),
					TargetRef:  plumbing.NewBranchReferenceName("main"),
					SourceHash: hash1,
				},
			},
			wantIsTag: map[plumbing.ReferenceName]bool{
				plumbing.NewBranchReferenceName("main"): false,
			},
		},
		{
			name: "tag ref sets IsTag true",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				plumbing.NewTagReferenceName("v1.0"): {
					Kind:       planner.RefKindTag,
					SourceRef:  plumbing.NewTagReferenceName("v1.0"),
					TargetRef:  plumbing.NewTagReferenceName("v1.0"),
					SourceHash: hash2,
				},
			},
			wantIsTag: map[plumbing.ReferenceName]bool{
				plumbing.NewTagReferenceName("v1.0"): true,
			},
		},
		{
			name: "mixed branch and tag",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				plumbing.NewBranchReferenceName("dev"): {
					Kind:       planner.RefKindBranch,
					SourceRef:  plumbing.NewBranchReferenceName("dev"),
					TargetRef:  plumbing.NewBranchReferenceName("dev"),
					SourceHash: hash1,
				},
				plumbing.NewTagReferenceName("v2.0"): {
					Kind:       planner.RefKindTag,
					SourceRef:  plumbing.NewTagReferenceName("v2.0"),
					TargetRef:  plumbing.NewTagReferenceName("v2.0"),
					SourceHash: hash2,
				},
			},
			wantIsTag: map[plumbing.ReferenceName]bool{
				plumbing.NewBranchReferenceName("dev"):  false,
				plumbing.NewTagReferenceName("v2.0"):    true,
			},
		},
		{
			name:    "empty input",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{},
			wantIsTag: map[plumbing.ReferenceName]bool{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toGP(tt.desired)
			if len(got) != len(tt.desired) {
				t.Fatalf("expected %d results, got %d", len(tt.desired), len(got))
			}
			for ref, wantTag := range tt.wantIsTag {
				gp, ok := got[ref]
				if !ok {
					t.Errorf("missing ref %s in output", ref)
					continue
				}
				if gp.IsTag != wantTag {
					t.Errorf("ref %s: IsTag = %v, want %v", ref, gp.IsTag, wantTag)
				}
				src := tt.desired[ref]
				if gp.SourceRef != src.SourceRef {
					t.Errorf("ref %s: SourceRef = %s, want %s", ref, gp.SourceRef, src.SourceRef)
				}
				if gp.TargetRef != src.TargetRef {
					t.Errorf("ref %s: TargetRef = %s, want %s", ref, gp.TargetRef, src.TargetRef)
				}
				if gp.SourceHash != src.SourceHash {
					t.Errorf("ref %s: SourceHash = %s, want %s", ref, gp.SourceHash, src.SourceHash)
				}
			}
		})
	}
}
