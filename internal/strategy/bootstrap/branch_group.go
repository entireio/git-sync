package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"

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
//
// budget is the run's current byte budget, which the trunk's pushes may
// already have lowered below TargetMaxPack.
func planAfterTrunk(ctx context.Context, p Params, d deferredBranches, budget int64) ([]plannedBatch, error) {
	if budget <= 0 {
		budget = p.TargetMaxPack
	}
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
	stopAt := d.stopAt
	if stopAt == nil && len(d.haves) > 0 {
		stopAt = boundaryCommits(parents)
	}

	var (
		group      []plannedBatch
		groupBytes int64
		groups     int
		standalone int
		onTarget   int
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
		// The source sends no commit reachable from a have. A tip it left out
		// is already on the target, with every object it needs.
		if _, sent := parents[ref.SourceHash]; !sent && len(d.haves) > 0 {
			p.log("bootstrap batch branch already reachable on target",
				"branch", ref.TargetRef.String(),
				"source_hash", planner.ShortHash(ref.SourceHash))
			out = append(out, subsumedBatch(ref))
			onTarget++
			continue
		}
		checkpoints, chain, err := checkpointsFromParents(p, ref, parents, stopAt)
		if err != nil {
			return nil, err
		}
		batch := newPlannedBatch(p, ref, checkpoints, chain)
		estimated := int64(len(chain)) * estimatedBytesPerCommit
		if !batch.ResumeHash.IsZero() || estimated > budget {
			out = append(out, batch)
			standalone++
			continue
		}
		if len(group) == maxGroupRefs || groupBytes+estimated > budget {
			flush()
		}
		group = append(group, batch)
		groupBytes += estimated
	}
	flush()
	p.log("bootstrap batch planned remaining branches",
		"subsumed", len(d.refs)-len(divergent),
		"already_on_target", onTarget,
		"grouped_packs", groups,
		"standalone_branches", standalone)
	return out, nil
}

// boundaryCommits returns the parents a commit-graph fetch named but did not
// send. Sent with haves, the fetch leaves out every commit reachable from
// them, so these are where the target's history begins. The topological walk
// needs them as a stop set: it fails on a parent it cannot find.
func boundaryCommits(parents map[plumbing.Hash][]plumbing.Hash) map[plumbing.Hash]struct{} {
	boundary := make(map[plumbing.Hash]struct{})
	for _, ps := range parents {
		for _, parent := range ps {
			if _, sent := parents[parent]; !sent {
				boundary[parent] = struct{}{}
			}
		}
	}
	return boundary
}

// groupPushState is the executeBatched state a group push reads and updates.
// Every pointer refers to an executeBatched variable: what a group push learns
// about the target applies to every later push, grouped or not.
type groupPushState struct {
	// completedRefs records confirmed creates, so later fetches offer them as
	// haves.
	completedRefs map[plumbing.ReferenceName]plumbing.Hash
	// budget is selfImposedBudget, and fromObservation its provenance.
	budget          *int64
	fromObservation *bool
	// bytesPerObject is the calibration the checkpointed path's pre-check uses.
	bytesPerObject *int64
	// announced is Params.AnnouncedTargetLimit.
	announced  *int64
	fetchLimit int64
}

// pushBranchGroups delivers a group of branches. Each push is one source fetch
// and one receive-pack that creates every member ref directly, with no temp
// ref: a member is small, so a failed push costs little to repeat.
//
// A group whose pack is too big splits into as many parts as its size needs.
// It returns the batches the caller must run next, in order. When a single
// branch is too big to share a pack, it stops there and returns that branch
// first, then the groups still pending. The branch then runs on its own
// checkpoints before those groups fetch again. When branches share a large
// history, that history lands once and becomes a have for the groups, instead
// of being re-fetched by every smaller split.
func pushBranchGroups(ctx context.Context, p Params, group []plannedBatch, st groupPushState, result *Result) ([]plannedBatch, error) {
	queue := [][]plannedBatch{group}
	for len(queue) > 0 {
		members := queue[0]
		queue = queue[1:]
		pending, parts, err := pushBranchGroup(ctx, p, members, st, result)
		if err != nil {
			return nil, err
		}
		switch len(pending) {
		case 0:
			continue
		case 1:
			p.log("bootstrap batch group member too big for a shared pack; pushing on its own checkpoints",
				"branch", pending[0].Plan.TargetRef.String(),
				"groups_pending", len(queue))
			next := make([]plannedBatch, 0, 1+len(queue))
			next = append(next, pending[0])
			for _, g := range queue {
				next = append(next, plannedBatch{group: g})
			}
			return next, nil
		default:
			queue = append(splitGroup(pending, parts), queue...)
		}
	}
	return nil, nil
}

// splitGroup divides members into parts groups of near-equal size, at least
// two and at most one per member.
func splitGroup(members []plannedBatch, parts int) [][]plannedBatch {
	parts = min(max(parts, 2), len(members))
	out := make([][]plannedBatch, 0, parts)
	for i := range parts {
		lo, hi := i*len(members)/parts, (i+1)*len(members)/parts
		out = append(out, members[lo:hi])
	}
	return out
}

// limitWatcher records whether the source pack passed its byte limit, which
// arrives as a read error the pusher may wrap or replace.
type limitWatcher struct {
	io.ReadCloser

	hit bool
}

func (w *limitWatcher) Read(b []byte) (int, error) {
	n, err := w.ReadCloser.Read(b)
	if errors.Is(err, gitproto.ErrSourcePackTooLarge) {
		w.hit = true
	}
	return n, err //nolint:wrapcheck // passes the reader's own error through, io.EOF included
}

// pushBranchGroup pushes members in one pack. When the push lands it returns
// no pending members. When the pack is too big it returns, with no error, the
// members still to deliver and the number of parts to split them into.
func pushBranchGroup(ctx context.Context, p Params, members []plannedBatch, st groupPushState, result *Result) ([]plannedBatch, int, error) {
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
			return nil, 0, fmt.Errorf("create bootstrap branch group refs: %w", err)
		}
	case err != nil:
		return nil, 0, fmt.Errorf("fetch bootstrap branch group pack: %w", err)
	default:
		// MaxPackBytes bounds one source fetch. Each member can fit while
		// their union does not, so passing it splits the group rather than
		// failing the run. The limit itself still holds for every fetch.
		watcher := &limitWatcher{ReadCloser: gitproto.LimitPackReader(packReader, st.fetchLimit)}
		packReader = gitproto.CloseOnce(watcher)
		// The static per-object estimate, not the run's calibrated one: the
		// calibration comes from the trunk's checkpoints, and one large blob
		// there would make every group of small commits look over budget.
		// The push aborter below measures real bytes, so it catches a group
		// this estimate lets through.
		budget := *st.budget
		parts := 0
		packReader, objectCount, err := checkPackSizeAndSubdivide(packReader, budget, estimatedBytesPerObject, func(estimated int64) bool {
			parts = int((estimated + budget - 1) / budget)
			return true
		})
		if err != nil {
			return nil, 0, fmt.Errorf("check bootstrap branch group pack size: %w", err)
		}
		if parts > 0 {
			p.log("bootstrap batch group pack estimate over budget; splitting",
				"branch_count", len(plans),
				"object_count", objectCount,
				"budget_bytes", budget,
				"parts", parts)
			return members, max(parts, 2), nil
		}

		p.log("bootstrap batch group push attempting",
			"branch_count", len(plans),
			"object_count", objectCount,
			"budget_bytes", budget)
		observer := newPackStreamObserver(packReader)
		observer.SetAborter(func(bytesSent, objectsSent, totalObjects int64, atEOF bool) bool {
			return shouldAbortPush(bytesSent, objectsSent, totalObjects, budget, atEOF)
		})
		pushErr := p.TargetPusher.PushPack(ctx, cmds, observer)
		if closeErr := observer.Close(); pushErr == nil {
			pushErr = closeErr
		}
		if pushErr != nil {
			var parts int
			switch {
			case watcher.hit:
				// A source-side limit says nothing about the target.
				parts = 2
			case observer.Aborted() || isBatchableTargetPushError(pushErr):
				parts = learnFromGroupRejection(st, observer, objectCount, budget, pushErr)
			default:
				return nil, 0, fmt.Errorf("push bootstrap branch group: %w", pushErr)
			}
			pending := dropAppliedMembers(p, members, st)
			p.log("bootstrap batch group push too big or too slow; splitting",
				"branch_count", len(plans),
				"pending", len(pending),
				"sent_bytes", observer.Bytes(),
				"aborted_early", observer.Aborted(),
				"source_limit_hit", watcher.hit,
				"budget_bytes", *st.budget,
				"parts", parts,
				"error", pushErr.Error())
			return pending, parts, nil
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
	return nil, 0, nil
}

// dropAppliedMembers records the members a failed push still created and
// returns the rest. The pusher splits ref updates across requests, so an
// earlier request can land before a later one fails. Retrying a created ref
// with a zero old hash fails on a target that checks it.
func dropAppliedMembers(p Params, members []plannedBatch, st groupPushState) []plannedBatch {
	pending := make([]plannedBatch, 0, len(members))
	for _, member := range members {
		if outcome, _ := p.refOutcome(member.Plan.TargetRef); outcome == gitproto.RefOutcomeApplied {
			st.completedRefs[member.Plan.TargetRef] = member.Plan.SourceHash
			continue
		}
		pending = append(pending, member)
	}
	return pending
}

// learnFromGroupRejection records what a failed group push showed about the
// target, by the same rules as a checkpoint push in executeBatched: the
// announced limit, the calibration, and the budget with its provenance. A
// later group then starts under the real cutoff instead of rediscovering it.
// It returns how many parts the group needs.
func learnFromGroupRejection(st groupPushState, observer *packStreamObserver, objectCount, budget int64, pushErr error) int {
	sentBytes := observer.Bytes()
	abortedEarly := observer.Aborted()
	totalObjects := observer.TotalObjects()
	parsedLimit := targetBodyLimit(pushErr)
	if parsedLimit > *st.announced {
		*st.announced = parsedLimit
	}

	effObjectsSent := effectiveObjectsSent(observer.ObjectsSent(), totalObjects, abortedEarly)
	calibrationDenom := objectCount
	if effObjectsSent > 0 && effObjectsSent < calibrationDenom {
		calibrationDenom = effObjectsSent
	}
	if updated := calibrateBytesPerObject(sentBytes, calibrationDenom, *st.bytesPerObject); updated > 0 {
		*st.bytesPerObject = updated
	}
	*st.fromObservation = nextBudgetProvenance(*st.fromObservation, parsedLimit, sentBytes, abortedEarly, pushErr)
	*st.budget = nextSelfImposedBudget(*st.budget, parsedLimit, sentBytes, abortedEarly)

	limit := budget
	if parsedLimit > 0 {
		limit = parsedLimit
	}
	sizingBytes := sentBytes
	if abortedEarly && totalObjects > 0 && effObjectsSent > 0 {
		if projected := sentBytes * totalObjects / effObjectsSent; projected > sizingBytes {
			sizingBytes = projected
		}
	}
	return observedSubdivisionFactor(sizingBytes, limit)
}
