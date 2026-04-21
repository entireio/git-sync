// Package pushreconcile decides when a per-reference push failure can be
// treated as a no-op because the target already reflects the desired state.
// It is shared by the replicate, incremental, and materialized strategies,
// all of which need to absorb CAS races with concurrent writers (commonly a
// sibling worker that already applied the same source state).
package pushreconcile

import (
	"context"
	"errors"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entirehq/git-sync/internal/gitproto"
	"github.com/entirehq/git-sync/internal/planner"
)

// Reason is the RelayReason value the strategies set when Check swallows a
// push error because the target was already at the desired state.
const Reason = "reconciled"

// Lister refreshes the target ref advertisement.
type Lister interface {
	ListRefs(ctx context.Context) (map[plumbing.ReferenceName]plumbing.Hash, error)
}

// Check reports whether every per-reference failure in err represents a
// no-op against the current target state. It returns false when:
//   - err is not a *gitproto.PushReportError,
//   - the report indicates an unpack-phase failure (nothing to reconcile),
//   - the lister call fails,
//   - any failed ref has no matching entry in plans, or
//   - any failed ref's current target hash does not match the plan's
//     intended outcome (source hash for create/update; absent for delete).
//
// A true result means the push can safely be treated as successful.
func Check(ctx context.Context, err error, plans []planner.BranchPlan, lister Lister) bool {
	var reportErr *gitproto.PushReportError
	if !errors.As(err, &reportErr) {
		return false
	}
	if reportErr.UnpackStatus != "" {
		return false
	}
	if len(reportErr.Failures) == 0 {
		return false
	}
	fresh, listErr := lister.ListRefs(ctx)
	if listErr != nil {
		return false
	}
	want := make(map[plumbing.ReferenceName]planner.BranchPlan, len(plans))
	for _, p := range plans {
		want[p.TargetRef] = p
	}
	for _, f := range reportErr.Failures {
		plan, ok := want[f.Ref]
		if !ok {
			return false
		}
		if plan.Action == planner.ActionDelete {
			if _, exists := fresh[f.Ref]; exists {
				return false
			}
			continue
		}
		if fresh[f.Ref] != plan.SourceHash {
			return false
		}
	}
	return true
}
