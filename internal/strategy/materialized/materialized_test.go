package materialized

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"

	"entire.io/entire/git-sync/internal/convert"
	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
	"entire.io/entire/git-sync/internal/syncertest"
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

func TestDefaultMaxMaterializedObjectsExported(t *testing.T) {
	// Verify the constant is exported and has a reasonable positive value.
	if DefaultMaxMaterializedObjects <= 0 {
		t.Fatalf("DefaultMaxMaterializedObjects should be positive, got %d", DefaultMaxMaterializedObjects)
	}
	// Sanity: it should be at least 1000 to be useful for real repos,
	// but not so large that it defeats its purpose as a safety limit.
	if DefaultMaxMaterializedObjects < 1_000 {
		t.Fatalf("DefaultMaxMaterializedObjects too small: %d", DefaultMaxMaterializedObjects)
	}
	if DefaultMaxMaterializedObjects > 10_000_000 {
		t.Fatalf("DefaultMaxMaterializedObjects unreasonably large: %d", DefaultMaxMaterializedObjects)
	}
}

func TestEffectiveMaxObjects(t *testing.T) {
	if got := effectiveMaxObjects(123); got != 123 {
		t.Fatalf("effectiveMaxObjects(123) = %d, want 123", got)
	}
	if got := effectiveMaxObjects(0); got != DefaultMaxMaterializedObjects {
		t.Fatalf("effectiveMaxObjects(0) = %d, want %d", got, DefaultMaxMaterializedObjects)
	}
}

func TestCollectObjectClosureForBranchUpdateExample(t *testing.T) {
	repo, fs := syncertest.NewMemoryRepo(t)
	syncertest.MakeCommits(t, repo, fs, 1)

	head, err := repo.Reference(plumbing.NewBranchReferenceName("master"), true)
	if err != nil {
		t.Fatalf("resolve head: %v", err)
	}

	exec := executor{
		params: Params{
			Store: repo.Storer,
			PushPlans: []planner.BranchPlan{{
				TargetRef:  plumbing.NewBranchReferenceName("master"),
				SourceHash: head.Hash(),
				Action:     planner.ActionCreate,
				Kind:       planner.RefKindBranch,
			}},
		},
	}

	hashes, err := exec.collectObjectClosure()
	if err != nil {
		t.Fatalf("collectObjectClosure: %v", err)
	}
	if len(hashes) < 3 {
		t.Fatalf("expected commit/tree/blob closure, got %d objects: %v", len(hashes), hashes)
	}
	if !containsHash(hashes, head.Hash()) {
		t.Fatalf("expected closure to include commit %s", head.Hash())
	}
}

func TestCollectObjectClosureStopsAtTargetHaveExample(t *testing.T) {
	repo, fs := syncertest.NewMemoryRepo(t)
	syncertest.MakeCommits(t, repo, fs, 2)

	head, err := repo.Reference(plumbing.NewBranchReferenceName("master"), true)
	if err != nil {
		t.Fatalf("resolve head: %v", err)
	}
	headCommit, err := object.GetCommit(repo.Storer, head.Hash())
	if err != nil {
		t.Fatalf("load head commit: %v", err)
	}
	if len(headCommit.ParentHashes) != 1 {
		t.Fatalf("expected single-parent history, got %d parents", len(headCommit.ParentHashes))
	}
	parent := headCommit.ParentHashes[0]

	exec := executor{
		params: Params{
			Store: repo.Storer,
			TargetRefs: map[plumbing.ReferenceName]plumbing.Hash{
				plumbing.NewBranchReferenceName("master"): parent,
			},
			PushPlans: []planner.BranchPlan{{
				TargetRef:  plumbing.NewBranchReferenceName("master"),
				TargetHash: parent,
				SourceHash: head.Hash(),
				Action:     planner.ActionUpdate,
				Kind:       planner.RefKindBranch,
			}},
		},
	}

	hashes, err := exec.collectObjectClosure()
	if err != nil {
		t.Fatalf("collectObjectClosure: %v", err)
	}
	if len(hashes) == 0 {
		t.Fatal("expected new objects for branch update")
	}
	if containsHash(hashes, parent) {
		t.Fatalf("expected closure to stop at target have %s, got %v", parent, hashes)
	}
	if !containsHash(hashes, head.Hash()) {
		t.Fatalf("expected closure to include new tip %s", head.Hash())
	}
}

func TestExecuteDeleteOnlyIsRefOnlyExample(t *testing.T) {
	var gotCmds []gitproto.PushCommand
	var gotHashes []plumbing.Hash

	err := Execute(context.Background(), Params{
		PushPlans: []planner.BranchPlan{{
			TargetRef:  plumbing.NewBranchReferenceName("old"),
			TargetHash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
			Action:     planner.ActionDelete,
			Kind:       planner.RefKindBranch,
		}},
		TargetPusher: fakeTargetPusher{
			pushObjects: func(_ context.Context, cmds []gitproto.PushCommand, _ storer.Storer, hashes []plumbing.Hash) error {
				gotCmds = append([]gitproto.PushCommand(nil), cmds...)
				gotHashes = append([]plumbing.Hash(nil), hashes...)
				return nil
			},
		},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(gotCmds) != 1 || !gotCmds[0].Delete {
		t.Fatalf("expected single delete command, got %+v", gotCmds)
	}
	if len(gotHashes) != 0 {
		t.Fatalf("expected delete-only materialized push to need no objects, got %v", gotHashes)
	}
}

type fakeTargetPusher struct {
	pushObjects func(context.Context, []gitproto.PushCommand, storer.Storer, []plumbing.Hash) error
}

func (f fakeTargetPusher) PushObjects(ctx context.Context, cmds []gitproto.PushCommand, store storer.Storer, hashes []plumbing.Hash) error {
	return f.pushObjects(ctx, cmds, store, hashes)
}

func containsHash(hashes []plumbing.Hash, want plumbing.Hash) bool {
	for _, h := range hashes {
		if h == want {
			return true
		}
	}
	return false
}
