// Package convert provides shared type conversions between planner and gitproto
// types. It exists to avoid duplicating these helpers across strategy packages,
// while keeping planner and gitproto free of circular imports.
package convert

import (
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/soph/git-sync/internal/gitproto"
	"github.com/soph/git-sync/internal/planner"
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
