package bootstrap

import (
	"context"
	"errors"
	"fmt"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"entire.io/entire/git-sync/internal/convert"
	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
)

// maxGroupRefs bounds the ref creates in one group push. It keeps a single
// receive-pack request well under common per-push command limits.
const maxGroupRefs = 1000

// planAfterTrunk plans every branch after the trunk with one commit-graph
// fetch for all of them, against the trunk's history.
//
// A branch whose tip the trunk already holds is subsumed. Small branches are
// grouped so that one fetch and one push deliver many of them: on a repository
// with thousands of short-lived branches, a fetch and a push per branch cannot
// finish inside one sync attempt (ENT-3617). A branch with a resume marker, or
// one estimated over the batch budget, keeps its own checkpointed batch.
func planAfterTrunk(ctx context.Context, p Params, d deferredBranches) ([]plannedBatch, error) {
	out := make([]plannedBatch, 0, len(d.refs))
	divergent := make([]planner.DesiredRef, 0, len(d.refs))
	for _, ref := range d.refs {
		if _, subsumed := d.stopAt[ref.SourceHash]; subsumed {
			p.log("bootstrap batch branch subsumed by trunk",
				"branch", ref.TargetRef.String(),
				"source_hash", planner.ShortHash(ref.SourceHash))
			out = append(out, subsumedBatch(ref))
			continue
		}
		divergent = append(divergent, ref)
	}
	if len(divergent) == 0 {
		return out, nil
	}

	wants := make([]plumbing.Hash, len(divergent))
	for i, ref := range divergent {
		wants[i] = ref.SourceHash
	}
	p.log("bootstrap batch fetching commit graph for remaining branches",
		"branch_count", len(divergent),
		"have_count", len(d.haves),
		"stop_at_count", len(d.stopAt))
	parents, err := p.SourceService.FetchCommitParents(ctx, p.SourceConn, wants, d.haves)
	if err != nil {
		return nil, fmt.Errorf("fetch bootstrap commit parents for %d remaining branches: %w", len(divergent), err)
	}

	var (
		group      []plannedBatch
		groupBytes int64
		groups     int
		standalone int
	)
	flush := func() {
		if len(group) == 0 {
			return
		}
		out = append(out, plannedBatch{group: group})
		groups++
		group, groupBytes = nil, 0
	}
	for _, ref := range divergent {
		checkpoints, chain, err := checkpointsFromParents(p, ref, parents, d.stopAt)
		if err != nil {
			return nil, err
		}
		batch := newPlannedBatch(p, ref, checkpoints, chain)
		estimated := int64(len(chain)) * estimatedBytesPerCommit
		if !batch.ResumeHash.IsZero() || estimated > p.TargetMaxPack {
			out = append(out, batch)
			standalone++
			continue
		}
		if len(group) == maxGroupRefs || groupBytes+estimated > p.TargetMaxPack {
			flush()
		}
		group = append(group, batch)
		groupBytes += estimated
	}
	flush()
	p.log("bootstrap batch planned remaining branches",
		"subsumed", len(d.refs)-len(divergent),
		"grouped_packs", groups,
		"standalone_branches", standalone)
	return out, nil
}

// groupPushState is the executeBatched state a group push reads and updates.
type groupPushState struct {
	// completedRefs is shared with executeBatched: confirmed creates are
	// recorded in it, so later fetches offer them as haves.
	completedRefs map[plumbing.ReferenceName]plumbing.Hash
	budget        int64
	fetchLimit    int64
}

// pushBranchGroups delivers a group of branches. Each push is one source fetch
// and one receive-pack that creates every member ref directly, with no temp
// ref: a member is small, so a failed push costs little to repeat.
//
// A group whose pack is too big splits in half. A single branch that is still
// too big is returned, so the caller can run it on its own checkpoints.
func pushBranchGroups(ctx context.Context, p Params, group []plannedBatch, st groupPushState, result *Result) ([]plannedBatch, error) {
	var standalone []plannedBatch
	queue := [][]plannedBatch{group}
	for len(queue) > 0 {
		members := queue[0]
		queue = queue[1:]
		fits, err := pushBranchGroup(ctx, p, members, st, result)
		if err != nil {
			return nil, err
		}
		if fits {
			continue
		}
		if len(members) == 1 {
			p.log("bootstrap batch group member too big for a shared pack; pushing on its own checkpoints",
				"branch", members[0].Plan.TargetRef.String())
			standalone = append(standalone, members[0])
			continue
		}
		half := len(members) / 2
		queue = append([][]plannedBatch{members[:half], members[half:]}, queue...)
	}
	return standalone, nil
}

// pushBranchGroup pushes members in one pack. It reports false, with no
// error, when the pack is too big for the budget, so the caller can split.
func pushBranchGroup(ctx context.Context, p Params, members []plannedBatch, st groupPushState, result *Result) (bool, error) {
	plans := make([]planner.BranchPlan, len(members))
	for i, member := range members {
		plans[i] = member.Plan
	}
	if p.OnPhase != nil {
		p.OnPhase(fmt.Sprintf("pushing %d branches", len(plans)))
	}
	result.PlannedBatchCount++
	cmds := convert.PlansToPushCommands(plans, false)

	desired := convert.DesiredRefsForPlans(p.DesiredRefs, plans)
	packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, desired, planner.CopyRefHashMap(st.completedRefs))
	switch {
	case errors.Is(err, git.NoErrAlreadyUpToDate):
		// Earlier pushes already delivered every object; only the refs are new.
		if err := p.TargetPusher.PushCommands(ctx, cmds); err != nil {
			return false, fmt.Errorf("create bootstrap branch group refs: %w", err)
		}
	case err != nil:
		return false, fmt.Errorf("fetch bootstrap branch group pack: %w", err)
	default:
		packReader = gitproto.CloseOnce(gitproto.LimitPackReader(packReader, st.fetchLimit))
		// The static per-object estimate, not the run's calibrated one: the
		// calibration comes from the trunk's checkpoints, and one large blob
		// there would make every group of small commits look over budget.
		// The push aborter below measures real bytes, so it catches a group
		// this estimate lets through.
		tooBig := false
		packReader, objectCount, err := checkPackSizeAndSubdivide(packReader, st.budget, estimatedBytesPerObject, func(int64) bool {
			tooBig = true
			return true
		})
		if err != nil {
			return false, fmt.Errorf("check bootstrap branch group pack size: %w", err)
		}
		if tooBig {
			p.log("bootstrap batch group pack estimate over budget; splitting",
				"branch_count", len(plans),
				"object_count", objectCount,
				"budget_bytes", st.budget)
			return false, nil
		}

		p.log("bootstrap batch group push attempting",
			"branch_count", len(plans),
			"object_count", objectCount,
			"budget_bytes", st.budget)
		observer := newPackStreamObserver(packReader)
		budget := st.budget
		observer.SetAborter(func(bytesSent, objectsSent, totalObjects int64, atEOF bool) bool {
			return shouldAbortPush(bytesSent, objectsSent, totalObjects, budget, atEOF)
		})
		pushErr := p.TargetPusher.PushPack(ctx, cmds, observer)
		if closeErr := observer.Close(); pushErr == nil {
			pushErr = closeErr
		}
		if pushErr != nil {
			if observer.Aborted() || isBatchableTargetPushError(pushErr) {
				p.log("bootstrap batch group push too big or too slow; splitting",
					"branch_count", len(plans),
					"sent_bytes", observer.Bytes(),
					"aborted_early", observer.Aborted(),
					"error", pushErr.Error())
				return false, nil
			}
			return false, fmt.Errorf("push bootstrap branch group: %w", pushErr)
		}
	}

	// Same rule as a subsumed create: only a create the target confirmed is
	// recorded. There is no temp ref to keep, so an unconfirmed create costs
	// nothing but a have.
	confirmed := 0
	for _, member := range members {
		if ok, doubt := p.createConfirmed(member.Plan.TargetRef); !ok {
			p.log("bootstrap batch group branch create unconfirmed; not counted",
				"branch", member.Plan.TargetRef.String(),
				"source_hash", planner.ShortHash(member.Plan.SourceHash),
				"reason", doubt)
			continue
		}
		st.completedRefs[member.Plan.TargetRef] = member.Plan.SourceHash
		confirmed++
	}
	result.BatchCount++
	p.log("bootstrap batch group pushed",
		"branch_count", len(plans),
		"confirmed", confirmed)
	return true, nil
}
