// Package convert provides shared type conversions between planner and gitproto
// types. It exists to avoid duplicating these helpers across strategy packages,
// while keeping planner and gitproto free of circular imports.
package convert

import (
	"github.com/go-git/go-git/v6/plumbing"

	"entire.io/entire/gitsync/internal/gitproto"
	"entire.io/entire/gitsync/internal/planner"
)

// DesiredRefs converts planner desired refs to gitproto desired refs.
func DesiredRefs(desired map[plumbing.ReferenceName]planner.DesiredRef) map[plumbing.ReferenceName]gitproto.DesiredRef {
	out := make(map[plumbing.ReferenceName]gitproto.DesiredRef, len(desired))
	for k, v := range desired {
		out[k] = gitproto.DesiredRef{
			SourceRef:  v.SourceRef,
			TargetRef:  v.TargetRef,
			SourceHash: v.SourceHash,
			IsTag:      v.Kind == planner.RefKindTag,
		}
	}
	return out
}

// DesiredRefsForPlans converts only the desired refs referenced by the plans.
func DesiredRefsForPlans(
	desired map[plumbing.ReferenceName]planner.DesiredRef,
	plans []planner.BranchPlan,
) map[plumbing.ReferenceName]gitproto.DesiredRef {
	out := make(map[plumbing.ReferenceName]gitproto.DesiredRef, len(plans))
	for _, plan := range plans {
		v, ok := desired[plan.TargetRef]
		if !ok {
			continue
		}
		out[plan.TargetRef] = gitproto.DesiredRef{
			SourceRef:  v.SourceRef,
			TargetRef:  v.TargetRef,
			SourceHash: v.SourceHash,
			IsTag:      v.Kind == planner.RefKindTag,
		}
	}
	return out
}

// PlansToPushPlans converts planner BranchPlans to gitproto PushPlans.
func PlansToPushPlans(plans []planner.BranchPlan) []gitproto.PushPlan {
	out := make([]gitproto.PushPlan, len(plans))
	for i, p := range plans {
		out[i] = gitproto.PushPlan{
			TargetRef:  p.TargetRef,
			TargetHash: p.TargetHash,
			SourceHash: p.SourceHash,
			Delete:     p.Action == planner.ActionDelete,
		}
	}
	return out
}

// PlansToPushCommands converts planner BranchPlans directly to gitproto PushCommands.
func PlansToPushCommands(plans []planner.BranchPlan) []gitproto.PushCommand {
	out := make([]gitproto.PushCommand, len(plans))
	for i, p := range plans {
		out[i] = gitproto.PushCommand{
			Name:   p.TargetRef,
			Old:    p.TargetHash,
			New:    p.SourceHash,
			Delete: p.Action == planner.ActionDelete,
		}
		if out[i].Delete {
			out[i].New = plumbing.ZeroHash
		}
	}
	return out
}
