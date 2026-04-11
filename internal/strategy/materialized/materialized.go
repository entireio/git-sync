// Package materialized implements the materialized fallback push strategy.
// This path fetches objects into local memory, then encodes and pushes them
// to the target. Used when relay is not safe.
package materialized

import (
	"context"
	"fmt"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/soph/git-sync/internal/gitproto"
	"github.com/soph/git-sync/internal/planner"
)

// Params holds the inputs for a materialized push.
type Params struct {
	Store         storer.Storer
	SourceConn    *gitproto.Conn
	SourceService *gitproto.RefService
	TargetConn    *gitproto.Conn
	TargetAdv     *packp.AdvRefs
	DesiredRefs   map[plumbing.ReferenceName]planner.DesiredRef
	TargetRefs    map[plumbing.ReferenceName]plumbing.Hash
	PushPlans     []planner.BranchPlan
	Verbose       bool
}

// MaxMaterializedObjects is the safety limit for the materialized fallback path.
// Beyond this count, the in-memory object store would consume excessive memory.
// Fail early rather than OOM (issue #15).
const MaxMaterializedObjects = 500_000

// Execute runs the materialized fallback: ensures tag objects are local,
// computes the object closure, and pushes to the target.
func Execute(ctx context.Context, p Params) error {
	if len(p.PushPlans) == 0 {
		return nil
	}

	// Ensure tag objects are fetched locally
	if err := ensureTagObjects(ctx, p); err != nil {
		return fmt.Errorf("prepare local objects for push: %w", err)
	}

	objects := make([]plumbing.Hash, 0, len(p.PushPlans))
	for _, plan := range p.PushPlans {
		if plan.Action == planner.ActionCreate || plan.Action == planner.ActionUpdate {
			objects = append(objects, plan.SourceHash)
		}
	}
	hashes, err := planner.ObjectsToPush(p.Store, objects, p.TargetRefs)
	if err != nil {
		return fmt.Errorf("compute objects to push: %w", err)
	}

	// Issue #15: guard against unbounded memory usage on large non-relay syncs.
	if len(hashes) > MaxMaterializedObjects {
		return fmt.Errorf(
			"materialized push requires %d objects (limit %d); use bootstrap for large initial syncs",
			len(hashes), MaxMaterializedObjects,
		)
	}

	cmds := gitproto.ToPushCommands(plansToPushPlans(p.PushPlans))
	if err := gitproto.PushObjects(ctx, p.TargetConn, p.TargetAdv, cmds, p.Store, hashes, p.Verbose); err != nil {
		return fmt.Errorf("push target refs: %w", err)
	}
	return nil
}

func ensureTagObjects(ctx context.Context, p Params) error {
	tagDesired := make(map[plumbing.ReferenceName]gitproto.DesiredRef)
	for _, plan := range p.PushPlans {
		if plan.Kind != planner.RefKindTag {
			continue
		}
		if d, ok := p.DesiredRefs[plan.TargetRef]; ok {
			tagDesired[plan.TargetRef] = gitproto.DesiredRef{
				SourceRef: d.SourceRef, TargetRef: d.TargetRef,
				SourceHash: d.SourceHash, IsTag: true,
			}
		}
	}
	if len(tagDesired) == 0 {
		return nil
	}
	err := p.SourceService.FetchToStore(ctx, p.Store, p.SourceConn, tagDesired, nil)
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return err
	}
	return nil
}

func plansToPushPlans(plans []planner.BranchPlan) []gitproto.PushPlan {
	out := make([]gitproto.PushPlan, len(plans))
	for i, p := range plans {
		out[i] = gitproto.PushPlan{
			TargetRef: p.TargetRef, TargetHash: p.TargetHash, SourceHash: p.SourceHash,
			Delete: p.Action == planner.ActionDelete,
		}
	}
	return out
}
