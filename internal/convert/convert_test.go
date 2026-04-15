package convert

import (
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entirehq/git-sync/internal/gitproto"
	"github.com/entirehq/git-sync/internal/planner"
)

func TestDesiredRefsForPlans(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	tagRef := plumbing.NewTagReferenceName("v1.0.0")
	mainHash := plumbing.NewHash("1111111111111111111111111111111111111111")
	tagHash := plumbing.NewHash("2222222222222222222222222222222222222222")

	desired := map[plumbing.ReferenceName]planner.DesiredRef{
		mainRef: {
			Kind:       planner.RefKindBranch,
			SourceRef:  mainRef,
			TargetRef:  mainRef,
			SourceHash: mainHash,
		},
		tagRef: {
			Kind:       planner.RefKindTag,
			SourceRef:  tagRef,
			TargetRef:  tagRef,
			SourceHash: tagHash,
		},
	}
	plans := []planner.BranchPlan{{TargetRef: tagRef}}

	got := DesiredRefsForPlans(desired, plans)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	ref, ok := got[tagRef]
	if !ok {
		t.Fatalf("missing desired ref for %s", tagRef)
	}
	if ref.SourceRef != tagRef || ref.TargetRef != tagRef || ref.SourceHash != tagHash || !ref.IsTag {
		t.Fatalf("unexpected desired ref: %+v", ref)
	}
}

func TestPlansToPushCommands(t *testing.T) {
	ref := plumbing.NewBranchReferenceName("main")
	oldHash := plumbing.NewHash("1111111111111111111111111111111111111111")
	newHash := plumbing.NewHash("2222222222222222222222222222222222222222")

	plans := []planner.BranchPlan{
		{TargetRef: ref, TargetHash: oldHash, SourceHash: newHash, Action: planner.ActionUpdate},
		{TargetRef: plumbing.NewBranchReferenceName("old"), TargetHash: oldHash, Action: planner.ActionDelete},
	}

	got := PlansToPushCommands(plans)
	want := []gitproto.PushCommand{
		{Name: ref, Old: oldHash, New: newHash},
		{Name: plumbing.NewBranchReferenceName("old"), Old: oldHash, Delete: true},
	}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
