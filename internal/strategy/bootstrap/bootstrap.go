// Package bootstrap implements the bootstrap relay strategy for git-sync.
// This handles initial seeding of an empty target, both one-shot and batched.
package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"entire.io/entire/git-sync/internal/convert"
	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
	"entire.io/entire/git-sync/internal/useragent"
)

const (
	defaultTargetMaxPackBytes  = 512 * 1024 * 1024
	githubLargeRepoThresholdKB = 1536 * 1024
)

var bodyLimitPattern = regexp.MustCompile(`body exceeded size limit ([0-9]+)`)

// GitHubRepoAPIBaseURL is the base for GitHub API calls (replaceable in tests).
var GitHubRepoAPIBaseURL = "https://api.github.com"

// Params holds the inputs for a bootstrap execution.
type Params struct {
	SourceConn    gitproto.Conn
	SourceService interface {
		FetchPack(ctx context.Context, conn gitproto.Conn, desired map[plumbing.ReferenceName]gitproto.DesiredRef, haves map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error)
		FetchCommitParents(ctx context.Context, conn gitproto.Conn, ref gitproto.DesiredRef, haves []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error)
		SupportsBootstrapBatch() bool
	}
	TargetPusher interface {
		PushPack(ctx context.Context, cmds []gitproto.PushCommand, pack io.ReadCloser) error
		PushCommands(ctx context.Context, cmds []gitproto.PushCommand) error
	}
	DesiredRefs map[plumbing.ReferenceName]planner.DesiredRef
	TargetRefs  map[plumbing.ReferenceName]plumbing.Hash
	// SourceHeadTarget is the source ref that HEAD points to, when advertised.
	// Empty if unknown. When set, batched bootstrap plans this branch first and
	// uses its commit-graph reachability as a cutoff for subsequent branches.
	SourceHeadTarget plumbing.ReferenceName
	MaxPackBytes     int64
	TargetMaxPack    int64
	Verbose          bool
	Logger           *slog.Logger
	// Strategy selects the chain ordering for batched bootstrap. Empty
	// or "first-parent" walks the first-parent backbone (default,
	// matches historical behaviour). "topo" includes every reachable
	// commit in topological order so sub-pack boundaries can land
	// inside merge-pulled side branches — useful for repos like
	// "checkpoint" branches where each first-parent step drags in a
	// large second-parent ancestry that would otherwise have to be
	// pushed in one indivisible pack.
	//
	// Server requirement under "topo": successive checkpoints aren't
	// always in an ancestor-descendant relationship (topological
	// order can interleave parallel branches), so the internal
	// refs/gitsync/bootstrap/heads/<branch> temp ref may receive
	// non-fast-forward updates between checkpoints. The temp ref is
	// purely internal scaffolding — user-visible refs (refs/heads,
	// refs/tags) only receive a single fast-forward update at
	// cutover — but targets that enforce receive.denyNonFastforwards
	// across all refs (not just refs/heads) will reject these
	// updates and fail the bootstrap. Major hosts (GitHub, GitLab,
	// Bitbucket, Cloudflare) do not enable this by default; the
	// constraint only matters on hardened/locked-down deployments.
	Strategy string
	// OnPhase, when non-nil, is called with a short human-readable label
	// describing the current bootstrap activity (e.g. "pack 3/8") so a
	// live progress renderer can surface what is currently in flight.
	// Called from the goroutine driving Execute; implementations must not
	// block.
	OnPhase func(string)
	// OnNotice, when non-nil, receives one-time human-readable messages
	// about discrete events worth surfacing alongside progress (pack
	// subdivision, switching to batched mode). Implementations should
	// treat each call as one log line.
	OnNotice func(string)
	// RefOutcome reports what the target did with a ref in the push that just
	// returned: applied it, refused it (with the reason), or said nothing about
	// it at all. Consulted immediately after a push, since the answer describes
	// that one request.
	//
	// It exists because a nil error is not proof. When the caller pushes
	// best-effort the pusher hands a per-ref "ng" to its OnRejection callback
	// and returns nil, so a target that refuses one command out of a
	// multi-command request reports the whole push as a success. Silence is a
	// third answer, not a synonym for success: a target that never negotiated
	// report-status, or whose report skipped this command, has confirmed
	// nothing.
	//
	// Nil for a caller that cannot answer, which reads as silence throughout.
	RefOutcome func(plumbing.ReferenceName) (gitproto.RefOutcome, string)
	// TargetRefsNow re-reads the target's refs. Consulted only when a push
	// left the fate of a branch create in doubt — the target refused it, or
	// cannot report per-ref status at all — to settle whether the branch is
	// there before its resume marker is deleted. That question has one honest
	// answer, and it is the target's rather than an inference from the ng text
	// it happened to write. Nil or failing means unsettled, which keeps the
	// marker.
	TargetRefsNow func(context.Context) (map[plumbing.ReferenceName]plumbing.Hash, error)
	// AnnouncedTargetLimit is the receive-pack body limit the target itself
	// stated, parsed from a rejection (0 when it never said). It is NOT the
	// batching budget — TargetMaxPack is deliberately smaller, so a doomed or
	// interrupted push wastes less and the temp ref advances more often. This
	// is the authority of last resort: when checkpoint subdivision bottoms out
	// (one commit per gap) and our own smaller budget is what stopped the
	// upload, the push is retried against this limit so the SERVER decides
	// whether the indivisible pack is too big. See executeBatched.
	AnnouncedTargetLimit int64
}

// ErrCheckpointExceedsTargetLimit reports a single commit whose pack the
// target itself refused: checkpoint subdivision had already bottomed out at
// one commit per gap, so there is nothing left to split and no retry can
// help. Callers should treat it as permanent rather than redelivering — the
// only remedies are a larger server limit or object-level splitting.
//
// Deliberately NOT returned for our own budget aborts: those are retried
// against the announced server limit first (see executeBatched), so this
// sentinel always carries a real server verdict.
var ErrCheckpointExceedsTargetLimit = errors.New("bootstrap checkpoint exceeds target pack limit and cannot be subdivided further")

func (p Params) notice(msg string) {
	if p.OnNotice != nil {
		p.OnNotice(msg)
	}
}

// refOutcome reports what the target did with name in the push that just
// returned. Silence when the caller supplied no accessor.
func (p Params) refOutcome(name plumbing.ReferenceName) (gitproto.RefOutcome, string) {
	if p.RefOutcome == nil {
		return gitproto.RefOutcomeUnknown, ""
	}
	return p.RefOutcome(name)
}

// refRefused reports whether the target refused an update to name, and its
// reason. A refusal is a positive fact — never inferred from silence — because
// it is the one answer this strategy stops a run on.
func (p Params) refRefused(name plumbing.ReferenceName) (string, bool) {
	outcome, reason := p.refOutcome(name)
	return reason, outcome == gitproto.RefOutcomeRefused
}

// createConfirmed reports whether the push that just returned proves the branch
// create landed, and what leaves it in doubt when it does not. Doubt is not
// failure: it is settled against the target's own ref listing, because whether
// a branch is there is a fact the target can state and an ng reason's wording
// is not.
func (p Params) createConfirmed(name plumbing.ReferenceName) (bool, string) {
	switch outcome, reason := p.refOutcome(name); outcome {
	case gitproto.RefOutcomeApplied:
		return true, ""
	case gitproto.RefOutcomeRefused:
		return false, reason
	case gitproto.RefOutcomeUnknown:
		return false, "target reported no status for this ref"
	default:
		return false, "target reported no status for this ref"
	}
}

// pendingMarker is a resume marker whose branch create this run could not
// confirm from the push, held back until the target's own ref listing settles
// whether the branch is there.
type pendingMarker struct {
	branch     plumbing.ReferenceName
	tempRef    plumbing.ReferenceName
	resumeHash plumbing.Hash
	// createHash is the hash this run tried to create the branch at. The
	// marker is only scaffolding once the branch is there at THAT hash;
	// a branch someone else created at an older commit leaves the span
	// between the two reachable from nothing but the marker.
	createHash plumbing.Hash
	doubt      string
}

// Result holds the outcome of the bootstrap strategy. Pushed is the count
// of attempted ref creates; under BestEffort, callers that want a count
// excluding rejected refs need to consult Pusher.OnRejection or apply the
// same downgrade pass the syncer wrapper does.
type Result struct {
	Plans             []planner.BranchPlan
	Pushed            int
	Relay             bool
	RelayMode         string
	RelayReason       string
	Batching          bool
	BatchCount        int
	PlannedBatchCount int
	TempRefs          []string
}

type plannedBatch struct {
	planner.BootstrapBatch

	chain []plumbing.Hash // full first-parent chain (root→tip) for subdividing on push failure
	// subsumed is true when the branch tip is already reachable from the
	// trunk (planned first), so every object is already on the target after
	// trunk's batches. Execution skips the commit-graph fetch, the pack
	// fetch, the temp ref, and the pack push — emitting only a single ref
	// create command.
	subsumed bool
}

// Execute runs the bootstrap strategy (one-shot or batched).
func Execute(ctx context.Context, p Params, relayReason string) (Result, error) {
	if p.TargetPusher == nil {
		return Result{Relay: true, RelayMode: "bootstrap", RelayReason: relayReason}, errors.New("bootstrap strategy requires TargetPusher")
	}

	// GitHub large-repo preflight
	if batchLimit, ok := githubBatchLimit(ctx, p); ok {
		p.TargetMaxPack = batchLimit
		p.log("bootstrap github preflight selected batched mode",
			"target_max_pack_bytes", p.TargetMaxPack)
	}

	planTargetRefs := p.TargetRefs
	if p.TargetMaxPack > 0 {
		planTargetRefs = adjustedBootstrapTargetRefs(p.DesiredRefs, p.TargetRefs)
	}
	plans, err := planner.BuildBootstrapPlans(p.DesiredRefs, planTargetRefs)
	if err != nil {
		return Result{}, fmt.Errorf("build bootstrap plans: %w", err)
	}

	result := Result{
		Plans: plans, Relay: true, RelayMode: "bootstrap", RelayReason: relayReason,
	}

	if p.TargetMaxPack > 0 {
		return executeBatched(ctx, p, plans, result)
	}

	// One-shot bootstrap
	p.log("bootstrap fetching refs from source", "ref_count", len(plans))
	gpDesired := convert.DesiredRefs(p.DesiredRefs)
	// Target refs ride along as fetch haves. On the designed path — a truly
	// empty target — the map is empty and this changes nothing; when the
	// resume-marker route lands here instead (batchable source, but the repo
	// is small enough that no batch limit engages), the marker anchors the
	// negotiation so only the remainder transfers instead of the whole repo
	// again. No git.NoErrAlreadyUpToDate handling: gitproto emits that
	// sentinel only for an empty want set, and Execute's callers guarantee a
	// non-empty desired set, so any error here is a real fetch failure.
	//
	// A marker that led us here is NOT deleted on completion, deliberately.
	// A nil push error does not mean the branch create landed: with
	// BestEffort the pusher swallows a per-ref "ng" and returns nil
	// (gitproto.PushPack via OnRejection), so deleting the marker here would
	// throw away the resume position for a branch that never got created —
	// the precise hazard the planner-side prune carve-out exists to prevent.
	// Prune is the cleaner instead: isLiveBootstrapMarker classifies the
	// marker as stale exactly when the branch really is present, so the cost
	// of being right is one extra run.
	packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, gpDesired, p.TargetRefs)
	if err != nil {
		return result, fmt.Errorf("fetch source pack: %w", err)
	}
	packReader = gitproto.LimitPackReader(packReader, p.MaxPackBytes)
	packReader = gitproto.CloseOnce(packReader)

	p.log("bootstrap pushing refs to target", "ref_count", len(plans))
	if p.OnPhase != nil {
		p.OnPhase("pushing pack")
	}
	cmds := convert.PlansToPushCommands(hoistSourceHeadPlan(plans, p.SourceHeadTarget), false)
	pushErr := p.TargetPusher.PushPack(ctx, cmds, packReader)
	_ = packReader.Close()
	if pushErr != nil {
		autoBatch, ok := autoTargetMaxPackBytes(p, pushErr)
		if !ok {
			return result, fmt.Errorf("push target refs: %w", actionableTargetPushError(p, pushErr))
		}
		reason := "target rejected pack"
		if isTargetPushDeadlineError(pushErr) {
			reason = "target push timed out"
		}
		p.log("bootstrap retrying with batched mode after batchable push failure",
			"target_max_pack_bytes", autoBatch, "reason", reason)
		p.notice(fmt.Sprintf("%s — switching to batched mode (limit %s)",
			reason, gitproto.HumanBytes(autoBatch)))
		p.TargetMaxPack = autoBatch
		// Remember what the target actually said, separately from the (smaller)
		// batching budget derived from it. Without this the announced figure is
		// consumed by autoTargetMaxPackBytes and lost, leaving the batch loop
		// unable to tell "too big for the server" from "too big for the budget
		// we chose".
		if announced := targetBodyLimit(pushErr); announced > p.AnnouncedTargetLimit {
			p.AnnouncedTargetLimit = announced
		}
		return executeBatched(ctx, p, plans, result)
	}

	result.Pushed = len(plans)
	return result, nil
}

// hoistSourceHeadPlan moves the plan whose source ref matches the
// source's symref HEAD target to the front, so the resulting push
// commands send that ref first. Hosts that pick the default branch
// from the first push on a fresh repo (GitHub, GitLab) end up with the
// right default. Matching is on SourceRef rather than TargetRef so
// --map remappings push the correct (mapped) target ref first.
func hoistSourceHeadPlan(plans []planner.BranchPlan, sourceHEAD plumbing.ReferenceName) []planner.BranchPlan {
	if sourceHEAD == "" {
		return plans
	}
	out, _ := hoistFirstMatch(plans, func(p planner.BranchPlan) bool {
		return p.SourceRef == sourceHEAD
	})
	return out
}

// hoistFirstMatch moves the first element satisfying match to the front
// of xs. Returns the (possibly new) slice and the original index of the
// hoisted element (-1 when no match). When the match is already at
// position 0, returns the input slice unchanged.
func hoistFirstMatch[T any](xs []T, match func(T) bool) ([]T, int) {
	for i, x := range xs {
		if !match(x) {
			continue
		}
		if i == 0 {
			return xs, 0
		}
		out := make([]T, 0, len(xs))
		out = append(out, x)
		out = append(out, xs[:i]...)
		out = append(out, xs[i+1:]...)
		return out, i
	}
	return xs, -1
}

func adjustedBootstrapTargetRefs(
	desiredRefs map[plumbing.ReferenceName]planner.DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) map[plumbing.ReferenceName]plumbing.Hash {
	if len(targetRefs) == 0 {
		return targetRefs
	}
	adjusted := planner.CopyRefHashMap(targetRefs)
	for targetRef, desired := range desiredRefs {
		if desired.Kind != planner.RefKindBranch {
			continue
		}
		tempRef := planner.BootstrapTempRef(targetRef)
		if adjusted[targetRef] == desired.SourceHash && adjusted[tempRef] == desired.SourceHash {
			adjusted[targetRef] = plumbing.ZeroHash
		}
	}
	return adjusted
}

// --- Batched bootstrap ---

func executeBatched( //nolint:maintidx // complex batch logic is inherently branchy
	ctx context.Context,
	p Params,
	plans []planner.BranchPlan,
	result Result,
) (Result, error) {
	if !p.SourceService.SupportsBootstrapBatch() {
		// Left as the one-shot route deliberately: batching was unavailable, so
		// reporting it would misdescribe what ran.
		return result, errors.New("bootstrap batching requires protocol v2 source fetch filter support")
	}
	// Batching describes the route, not the outcome, so it is recorded here —
	// before checkpoint planning, whose commit-graph fetch is the likeliest
	// thing to fail for exactly the large repos that batch. Recording it after
	// planning would report those failures as one-shot bootstraps.
	result.Batching = true
	result.RelayMode = "bootstrap-batch"

	// Tags and other-kind refs are create-only and ride a single tail phase
	// after the checkpointed branch batches; they reuse branch-tip haves.
	planRefs := make([]planner.DesiredRef, 0, len(plans))
	tailPlans := make([]planner.BranchPlan, 0, len(plans))
	for _, plan := range plans {
		if plan.Kind == planner.RefKindTag || plan.Kind == planner.RefKindOther {
			tailPlans = append(tailPlans, plan)
			continue
		}
		if !plan.SourceRef.IsBranch() || !plan.TargetRef.IsBranch() {
			return result, errors.New("bootstrap batching currently supports branch refs, tags, and other-kind create-only refs")
		}
		planRefs = append(planRefs, p.DesiredRefs[plan.TargetRef])
	}
	tailDesired := convert.DesiredRefsForPlans(p.DesiredRefs, tailPlans)

	var batches []plannedBatch
	if len(planRefs) > 0 {
		p.log("bootstrap batch planning checkpoints", "branch_ref_count", len(planRefs))
		var err error
		batches, err = planBatches(ctx, p, planRefs)
		if err != nil {
			return result, err
		}
	}

	// MaxPackBytes is the hard abort threshold for any single source fetch.
	// TargetMaxPack controls checkpoint *placement* (how many batches) but
	// should not cap individual fetches — the estimate may undercount, and
	// the actual pack for a batch can legitimately exceed the planning
	// heuristic. If the resulting pack is too large for the target's
	// receive-pack, the push itself fails and resume handles retry.
	fetchLimit := p.MaxPackBytes
	// Track branch tips already pushed to the target so subsequent branches
	// can advertise them as haves. Without this, the second branch in a
	// multi-branch bootstrap re-sends all shared objects (e.g., linux's
	// master and nocache-cleanup share ~99% of history).
	completedRefs := planner.CopyRefHashMap(p.TargetRefs)
	// Markers whose branch create this run could not confirm, settled against
	// the target's own ref listing once every branch has had its cutover.
	var pending []pendingMarker

	// calibratedBytesPerObject tracks the per-object byte estimate
	// updated from observed rejected pushes. Starts at the static
	// default (which under-counts blob-heavy repos) and ratchets up as
	// 413s reveal that the real bytes/object are much higher than 750.
	// Used in checkPackSizeAndSubdivide so subsequent sub-pack fetches
	// are pre-emptively split when the calibrated estimate exceeds
	// p.TargetMaxPack — saving an entire ~limit-sized wasted upload.
	calibratedBytesPerObject := int64(estimatedBytesPerObject)

	// selfImposedBudget tracks the byte ceiling we use for mid-stream
	// abort. Initialised from the user-supplied target limit; ratchets
	// down each time we observe a smaller server-side cutoff (parsed
	// 413 limit or, more commonly with reverse proxies that don't
	// announce the limit in the response, the bytes we managed to send
	// before the connection was cut). Re-used across attempts so every
	// failed push refines the budget.
	selfImposedBudget := p.TargetMaxPack
	// budgetFromObservation records that selfImposedBudget came from bytes we
	// actually got to send — a measured server cutoff — rather than a figure we
	// chose. Declared beside the budget, not inside the per-branch loop: the
	// provenance has to travel with the value, or branch 2 escalates past a
	// cutoff branch 1 demonstrated.
	budgetFromObservation := false

	for _, batch := range batches {
		if batch.subsumed {
			cmds := []gitproto.PushCommand{{
				Name: batch.Plan.TargetRef,
				Old:  plumbing.ZeroHash,
				New:  batch.Plan.SourceHash,
			}}
			if err := p.TargetPusher.PushCommands(ctx, cmds); err != nil {
				return result, fmt.Errorf("create subsumed branch ref for %s: %w", batch.Plan.TargetRef, err)
			}
			// Same rule as the cutover below — only a create the target
			// confirmed counts as delivered — but settled here rather than
			// against a ref listing. A subsumed branch has no temp ref to
			// preserve, since trunk already delivered its objects, so nothing
			// hinges on the answer except the reported batch count and a have
			// that trunk's own tip already covers. Neither is worth a round
			// trip, and under-reporting work this run cannot confirm is the
			// same direction as everything else here.
			if confirmed, doubt := p.createConfirmed(batch.Plan.TargetRef); !confirmed {
				p.log("bootstrap batch subsumed branch create unconfirmed; not counted",
					"branch", batch.Plan.TargetRef.String(),
					"source_hash", planner.ShortHash(batch.Plan.SourceHash),
					"reason", doubt)
				continue
			}
			completedRefs[batch.Plan.TargetRef] = batch.Plan.SourceHash
			result.BatchCount++
			p.log("bootstrap batch subsumed branch finalized",
				"branch", batch.Plan.TargetRef.String(),
				"source_hash", planner.ShortHash(batch.Plan.SourceHash))
			continue
		}
		result.PlannedBatchCount += len(batch.Checkpoints)
		result.TempRefs = append(result.TempRefs, batch.TempRef.String())
		p.log("bootstrap batch branch plan",
			"branch", batch.Plan.TargetRef.String(),
			"temp_ref", batch.TempRef.String(),
			"planned_batches", len(batch.Checkpoints),
			"resume_hash", planner.ShortHash(batch.ResumeHash))

		current := batch.ResumeHash
		// Whether this branch's create rode a push in this iteration at all.
		// It does not when the branch and its marker already sit at the source
		// hash — adjustedBootstrapTargetRefs zeroes such a branch so it gets a
		// create plan, but the checkpoint loop and the at-tip cutover both have
		// nothing to do. Asking what the target said about a ref no push
		// carried would read the previous branch's answer.
		createPushed := false
		startIdx, err := planner.BootstrapResumeIndex(batch.Checkpoints, batch.ResumeHash)
		if err != nil && !batch.ResumeHash.IsZero() && len(batch.chain) > 0 {
			// Temp ref doesn't match any planned checkpoint (e.g., the user
			// changed --target-max-pack-bytes between runs). If the hash is
			// in the commit chain, reuse it as the starting point and re-plan
			// remaining checkpoints — preserving already-pushed data.
			if chainIdx := chainPosition(batch.chain, batch.ResumeHash); chainIdx >= 0 {
				remaining := batch.chain[chainIdx+1:]
				if len(remaining) > 0 {
					numBatches := estimateBatchCount(int64(len(remaining)), p.TargetMaxPack)
					batch.Checkpoints = evenCheckpoints(remaining, numBatches)
					p.log("bootstrap batch resuming from stale temp ref",
						"branch", batch.Plan.TargetRef.String(),
						"resume_hash", planner.ShortHash(batch.ResumeHash),
						"remaining_commits", len(remaining),
						"new_batches", len(batch.Checkpoints))
					startIdx = 0
					err = nil
				}
			}
		}
		if err != nil && !batch.ResumeHash.IsZero() {
			// Temp ref hash not in the chain at all — truly stale. Delete and start fresh.
			p.log("bootstrap batch clearing stale temp ref",
				"branch", batch.Plan.TargetRef.String(),
				"temp_ref", batch.TempRef.String(),
				"stale_hash", planner.ShortHash(batch.ResumeHash))
			delCmds := []gitproto.PushCommand{{Name: batch.TempRef, Old: batch.ResumeHash, Delete: true}}
			if delErr := p.TargetPusher.PushCommands(ctx, delCmds); delErr != nil {
				return result, fmt.Errorf("delete stale temp ref %s: %w (original: %w)", batch.TempRef, delErr, err)
			}
			// Restarting from zero is only sound if the marker really went
			// away. A refused delete (deletes blocked in the namespace, or the
			// ref moved so Old no longer matches) returns nil under
			// BestEffort, and continuing would push the first checkpoint with
			// Old=zero against a ref that still exists — a CAS failure every
			// run reproduces, with the marker never cleared.
			if reason, refused := p.refRefused(batch.TempRef); refused {
				return result, fmt.Errorf("clear stale bootstrap temp ref %s: target refused: %s (stale because: %w)",
					batch.TempRef, reason, err)
			}
			current = plumbing.ZeroHash
			startIdx = 0
		}

		// Track every commit known to be in the target so the source
		// can use them all as fetch haves. With topo ordering a merge's
		// "delta" otherwise drags in the full ancestry of the non-first
		// parent, even though we pushed those commits as their own
		// checkpoints earlier in the chain (or in earlier runs).
		//
		// On resume, seed from the chain prefix at-or-before `current`:
		// every prior topo iteration must have pushed those commits to
		// advance the temp ref to its current position. Declaring just
		// `current` is insufficient because in topo order the earlier
		// chain entries aren't necessarily its ancestors — they can sit
		// on side branches that only get merged later.
		pushedCheckpoints := make([]plumbing.Hash, 0, len(batch.chain))
		if !current.IsZero() && len(batch.chain) > 0 {
			if idx := chainPosition(batch.chain, current); idx >= 0 {
				pushedCheckpoints = append(pushedCheckpoints, batch.chain[:idx+1]...)
			}
		}

		// Manual index loop: subdivide may insert checkpoints at the current
		// index, so we must not auto-increment after a retry.
		idx := startIdx
		for idx < len(batch.Checkpoints) {
			checkpoint := batch.Checkpoints[idx]
			if p.OnPhase != nil {
				p.OnPhase(fmt.Sprintf("pack %d/%d", idx+1, len(batch.Checkpoints)))
			}
			p.log("bootstrap batch push checkpoint",
				"branch", batch.Plan.TargetRef.String(),
				"batch", idx+1,
				"batch_total", len(batch.Checkpoints),
				"from", planner.ShortHash(current),
				"to", planner.ShortHash(checkpoint))

			stagePlans := []planner.BranchPlan{{
				Branch: batch.Plan.Branch, SourceRef: batch.Plan.SourceRef,
				TargetRef: batch.TempRef, SourceHash: checkpoint, TargetHash: current,
				Kind: batch.Plan.Kind, Action: planner.ActionForTargetHash(current),
				Reason: fmt.Sprintf("%s -> %s via %s", planner.ShortHash(current), planner.ShortHash(checkpoint), batch.TempRef),
			}}
			if idx == len(batch.Checkpoints)-1 {
				createPushed = true
				stagePlans = append(stagePlans, planner.BranchPlan{
					Branch: batch.Plan.Branch, SourceRef: batch.Plan.SourceRef,
					TargetRef: batch.Plan.TargetRef, SourceHash: checkpoint,
					TargetHash: plumbing.ZeroHash, Kind: batch.Plan.Kind,
					Action: planner.ActionCreate,
					Reason: fmt.Sprintf("create %s at %s", batch.Plan.TargetRef, planner.ShortHash(checkpoint)),
				})
			}

			packReader, err := packReaderForCheckpoint(ctx, p, batch, checkpoint, pushedCheckpoints, completedRefs, fetchLimit)
			if err != nil {
				return result, fmt.Errorf("fetch source batch pack for %s: %w", batch.Plan.TargetRef, err)
			}
			packReader = gitproto.CloseOnce(packReader)

			// Peek at the PACK header (12 bytes) to get the object count.
			// If the estimated pack size (objectCount × calibrated
			// bytesPerObject) exceeds the batch limit, subdivide
			// immediately instead of pushing a pack the target will reject.
			// This avoids wasting a multi-GiB transfer on a doomed push.
			var packObjectCount int64
			if p.TargetMaxPack > 0 && len(batch.chain) > 0 {
				subdivided := false
				packReader, packObjectCount, err = checkPackSizeAndSubdivide(packReader, p.TargetMaxPack, calibratedBytesPerObject, func(estimated int64) bool {
					expanded := subdivideCheckpoints(batch.chain, current, batch.Checkpoints[idx:])
					if len(expanded) > len(batch.Checkpoints[idx:]) {
						oldRemaining := len(batch.Checkpoints[idx:])
						newCount := len(expanded)
						perPack := estimated / int64(newCount)
						p.log("bootstrap batch subdividing before push (pack header estimate)",
							"branch", batch.Plan.TargetRef.String(),
							"old_remaining", oldRemaining,
							"new_remaining", newCount,
							"estimated_bytes", estimated,
							"calibrated_bytes_per_object", calibratedBytesPerObject)
						p.notice(fmt.Sprintf(
							"estimated pack ~%s exceeds target limit %s — splitting %d → %d packs (~%s each)",
							gitproto.HumanBytes(estimated), gitproto.HumanBytes(p.TargetMaxPack),
							oldRemaining, newCount, gitproto.HumanBytes(perPack),
						))
						batch.Checkpoints = append(batch.Checkpoints[:idx], expanded...)
						subdivided = true
						return true
					}
					return false
				})
				if err != nil {
					return result, fmt.Errorf("check pack size for %s: %w", batch.Plan.TargetRef, err)
				}
				if subdivided {
					continue // retry at same idx with new (smaller) checkpoint
				}
			}
			p.log("bootstrap batch push attempting",
				"branch", batch.Plan.TargetRef.String(),
				"batch", idx+1,
				"batch_total", len(batch.Checkpoints),
				"estimated_bytes", packObjectCount*calibratedBytesPerObject,
				"object_count", packObjectCount,
				"target_limit_bytes", p.TargetMaxPack,
				"calibrated_bytes_per_object", calibratedBytesPerObject)

			cmds := convert.PlansToPushCommands(stagePlans, false)
			observer := newPackStreamObserver(packReader)
			// relaxedBudget, when set, raises the ceiling for THIS attempt only.
			// It must not be written back into selfImposedBudget: that variable
			// spans every checkpoint and every branch, so a raised ceiling would
			// leak forward and later packs would upload in full instead of
			// aborting at the small budget the pre-flight estimate still plans
			// against.
			// The abort budget for a span we cannot split is the target's
			// number, not ours. TargetMaxPack exists so a doomed push wastes
			// little and the temp ref advances often — both of which need a
			// smaller pack to be *possible*. On a one-commit gap neither is, so
			// cutting at our budget only defers the inevitable and costs a
			// second full source fetch to learn what the target thinks.
			//
			// Note this spends no extra bytes: a ceiling is an abort threshold,
			// not an upload size. A pack that fits under the small budget sends
			// exactly the same bytes under a larger ceiling.
			//
			// >= not >: when an in-batching rejection ratchets the budget down
			// to the announced limit the two are equal, and escalating still
			// sheds the 5% margin and the projection — without which a pack
			// sized in that last 5% aborts on every delivery forever.
			budget := selfImposedBudget
			atAnnounced := !budgetFromObservation && p.AnnouncedTargetLimit > 0 &&
				p.AnnouncedTargetLimit >= budget &&
				isIndivisibleCheckpoint(batch, current, idx)
			if atAnnounced {
				budget = p.AnnouncedTargetLimit
				p.log("bootstrap batch pushing indivisible checkpoint at announced target limit",
					"branch", batch.Plan.TargetRef.String(),
					"batch", idx+1,
					"self_imposed_budget", selfImposedBudget,
					"announced_target_limit", p.AnnouncedTargetLimit)
				p.notice(fmt.Sprintf(
					"cannot split further (1 commit) — pushing at the target's announced limit %s instead of %s",
					gitproto.HumanBytes(p.AnnouncedTargetLimit), gitproto.HumanBytes(selfImposedBudget)))
			}
			switch {
			case atAnnounced:
				// No margin and no projection: at the target's own limit an
				// early cut would manufacture a verdict it never gave, and a
				// front-loaded pack's bytes-per-object average overshoots badly
				// — exactly the giant-blob shape this path serves. Cut only once
				// we have genuinely exceeded what it said it accepts, where a
				// rejection is certain rather than predicted.
				ceiling := budget
				observer.SetAborter(func(bytesSent, _, _ int64) bool {
					return bytesSent > ceiling
				})
			case budget > 0:
				observer.SetAborter(func(bytesSent, objectsSent, totalObjects int64) bool {
					return shouldAbortPush(bytesSent, objectsSent, totalObjects, budget)
				})
			}
			pushErr := p.TargetPusher.PushPack(ctx, cmds, observer)
			sentBytes := observer.Bytes()
			objectsSent := observer.ObjectsSent()
			totalObjects := observer.TotalObjects()
			abortedEarly := observer.Aborted()
			if pushErr != nil {
				_ = packReader.Close()
				// A pack too big for the target is the unifying signal here,
				// whether the server announced it (413 body limit), we
				// detected it ourselves (abortedEarly), or the target ran out
				// of time receiving it (408/504 deadline). All three are fixed
				// the same way: subdivide so each push is smaller and faster.
				subdivide := abortedEarly || isBatchableTargetPushError(pushErr)
				p.log("bootstrap batch push failed",
					"branch", batch.Plan.TargetRef.String(),
					"batch", idx+1,
					"batch_total", len(batch.Checkpoints),
					"estimated_bytes", packObjectCount*calibratedBytesPerObject,
					"target_limit_bytes", p.TargetMaxPack,
					"sent_bytes", sentBytes,
					"object_count", packObjectCount,
					"objects_sent", objectsSent,
					"total_objects_in_pack", totalObjects,
					"aborted_early", abortedEarly,
					"will_subdivide", subdivide && len(batch.chain) > 0,
					"error", pushErr.Error())
				if subdivide && len(batch.chain) > 0 {
					parsedLimit := targetBodyLimit(pushErr)
					// Keep the announced figure even when the run never made a
					// one-shot attempt (an explicit --target-max-pack-bytes, or
					// the GitHub large-repo preflight, enters batching directly).
					// Without this the relaxed retry below could only ever fire
					// on runs that happened to start with a one-shot rejection.
					if parsedLimit > p.AnnouncedTargetLimit {
						p.AnnouncedTargetLimit = parsedLimit
					}
					limit := p.TargetMaxPack
					if parsedLimit > 0 {
						limit = parsedLimit
					} else if abortedEarly && budget > 0 {
						// budget, not selfImposedBudget: on a relaxed retry the
						// ceiling in force is the announced one, and reporting
						// the smaller figure would under-state it on exactly the
						// path this exists to make legible.
						limit = budget
					}
					// Calibrate before subdividing. The new value carries
					// over to the next iteration's pre-flight check, so a
					// blob-heavy repo's sub-packs get caught earlier.
					//
					// When we have an objectsSent observation from the
					// streaming parser, divide by THAT rather than the
					// full pack header count. sentBytes covers exactly
					// objectsSent objects (the front of the pack), so
					// sentBytes/objectsSent is the accurate per-object
					// average for the portion we actually observed —
					// and for blob-front-loaded repos that's a pessimistic
					// upper bound, which is what we want for pre-flight.
					//
					// When the header was parsed but no object completed
					// (a single front-loaded large blob exhausted the
					// abort floor), treat it as one observed object: we
					// know that first object alone consumed sentBytes,
					// which is the right pessimistic per-object input.
					effObjectsSent := effectiveObjectsSent(objectsSent, totalObjects, abortedEarly)
					calibrationDenom := packObjectCount
					if effObjectsSent > 0 && effObjectsSent < calibrationDenom {
						calibrationDenom = effObjectsSent
					}
					if updated := calibrateBytesPerObject(sentBytes, calibrationDenom, calibratedBytesPerObject); updated > 0 {
						p.log("bootstrap batch calibrated bytes-per-object",
							"branch", batch.Plan.TargetRef.String(),
							"previous_bytes_per_object", calibratedBytesPerObject,
							"observed_bytes_per_object", updated,
							"sent_bytes", sentBytes,
							"calibration_denom", calibrationDenom,
							"object_count", packObjectCount,
							"objects_sent", objectsSent)
						calibratedBytesPerObject = updated
					}
					if next := nextSelfImposedBudget(selfImposedBudget, parsedLimit, sentBytes, abortedEarly); next != selfImposedBudget {
						// A budget derived from bytes actually sent is evidence
						// of where the server cuts, not a number we picked, so
						// the relaxed retry must not jump past it.
						budgetFromObservation = parsedLimit <= 0
						selfImposedBudget = next
					}
					// Pick the byte count we use for sizing the next
					// subdivision. When the server cut us off, sentBytes
					// is roughly the cap and using it directly is right.
					// When *we* cut the upload early, sentBytes is just
					// the abort point (~minBytesBeforeAbort) — much less
					// than the real pack size — so the factor would
					// converge slowly. Project from observed bytes/object
					// to the full pack size when we have the data, so
					// factor reflects the real overshoot.
					sizingBytes := sentBytes
					if abortedEarly && totalObjects > 0 && effObjectsSent > 0 {
						if projected := sentBytes * totalObjects / effObjectsSent; projected > sizingBytes {
							sizingBytes = projected
						}
					}
					factor := observedSubdivisionFactor(sizingBytes, limit)
					expanded := subdivideToFactor(batch.chain, current, batch.Checkpoints[idx:], factor)
					if len(expanded) > len(batch.Checkpoints[idx:]) {
						oldRemaining := len(batch.Checkpoints[idx:])
						newCount := len(expanded)
						p.log("bootstrap batch subdividing after target size rejection",
							"branch", batch.Plan.TargetRef.String(),
							"old_remaining", oldRemaining,
							"new_remaining", newCount,
							"sent_bytes", sentBytes,
							"sizing_bytes", sizingBytes,
							"limit_bytes", limit,
							"factor", factor,
							"aborted_early", abortedEarly,
							"error", pushErr.Error())
						limitText := ""
						if limit > 0 {
							limitText = fmt.Sprintf(" (target limit %s)", gitproto.HumanBytes(limit))
						}
						reason := "target rejected pack"
						if abortedEarly {
							reason = "projected to exceed target limit"
						}
						p.notice(fmt.Sprintf("%s%s — splitting %d → %d packs",
							reason, limitText, oldRemaining, newCount))
						batch.Checkpoints = append(batch.Checkpoints[:idx], expanded...)
						continue // retry at same idx with new (smaller) checkpoint
					}

					// Subdivision has bottomed out: this checkpoint is one
					// commit, so no further split exists.
					//
					// Terminal only on evidence about SIZE from the target: a
					// body-limit rejection it actually sent, or an attempt made
					// at the limit it announced that still overshot. Both mean
					// the identical indivisible pack would be re-sent forever.
					//
					// A deadline (408/504) is availability, not size — with zero
					// bytes sent there is no size evidence at all — so it stays
					// retryable and heals on the next delivery. So does an abort
					// against our own smaller budget: a larger budget or a
					// server config change could still mirror this repo.
					if isTargetBodyLimitError(pushErr) || (atAnnounced && abortedEarly) {
						if isIndivisibleCheckpoint(batch, current, idx) {
							return result, fmt.Errorf("push bootstrap batch for %s: %w: %w",
								batch.Plan.TargetRef, ErrCheckpointExceedsTargetLimit, pushErr)
						}
					}
				}
				return result, fmt.Errorf("push bootstrap batch for %s: %w", batch.Plan.TargetRef, pushErr)
			}
			_ = packReader.Close()
			// The temp ref is the state machine: `current` advances only if
			// the target really moved it. A policy that refuses the whole
			// request (read-only repository, a blanket pre-receive block) ng's
			// this command too, and best-effort turns that into a nil error —
			// after which every subsequent checkpoint would negotiate against
			// a have the target does not have, and the cutover would delete a
			// ref at a hash the target never accepted. There is no partial
			// progress to keep here, so stop rather than warn: the branch
			// cannot be delivered by this run at all.
			//
			// Only a refusal stops the run. A target that cannot report
			// per-ref status says nothing about this ref either, and treating
			// that silence as a refusal would fail every batched bootstrap
			// against it.
			if reason, refused := p.refRefused(batch.TempRef); refused {
				return result, fmt.Errorf("push bootstrap batch for %s: target refused %s: %s",
					batch.Plan.TargetRef, batch.TempRef, reason)
			}
			p.log("bootstrap batch checkpoint complete",
				"branch", batch.Plan.TargetRef.String(),
				"batch", idx+1,
				"batch_total", len(batch.Checkpoints))
			current = checkpoint
			pushedCheckpoints = append(pushedCheckpoints, checkpoint)
			result.BatchCount++

			// Recombine: subdivision is a one-way ratchet, so the fine
			// granularity needed for one heavy commit sticks around for
			// the rest of the chain even when the deltas afterward are
			// tiny. Aim for the next pack to be roughly half the target
			// limit by dropping enough checkpoints that the span doubles
			// approximately log2(target/2 / sent) times. If we
			// overshoot, the abort-early + subdivision path re-splits.
			// Leave at least the final checkpoint after idx (it carries
			// the SourceHash cutover), so the cap is len-idx-2.
			if dropCount := recombineDropCount(sentBytes, p.TargetMaxPack, len(batch.Checkpoints)-idx-2); dropCount > 0 {
				dropped := batch.Checkpoints[idx+1]
				batch.Checkpoints = append(batch.Checkpoints[:idx+1], batch.Checkpoints[idx+1+dropCount:]...)
				p.log("bootstrap batch recombining after small push",
					"branch", batch.Plan.TargetRef.String(),
					"sent_bytes", sentBytes,
					"target_limit_bytes", p.TargetMaxPack,
					"dropped_count", dropCount,
					"first_dropped_checkpoint", planner.ShortHash(dropped),
					"remaining_checkpoints", len(batch.Checkpoints))
			}
			idx++
		}

		if current.IsZero() {
			return result, fmt.Errorf("bootstrap batching for %s completed with no checkpoint state", batch.Plan.TargetRef)
		}
		if batch.ResumeHash == batch.Plan.SourceHash && p.TargetRefs[batch.Plan.TargetRef].IsZero() {
			cmds := []gitproto.PushCommand{{Name: batch.Plan.TargetRef, Old: plumbing.ZeroHash, New: batch.Plan.SourceHash}}
			if err := p.TargetPusher.PushCommands(ctx, cmds); err != nil {
				return result, fmt.Errorf("resume bootstrap cutover for %s: %w", batch.Plan.TargetRef, err)
			}
			createPushed = true
		}

		// The temp ref is this import's only record of how far it got, so it
		// may only be deleted once the branch it was scaffolding for exists.
		// The create rode the final checkpoint's push (or, on an at-tip
		// resume, the command push just above), and a nil error from either
		// does not prove it landed — see Params.RefOutcome. A target that
		// refuses the create (protected branch, pre-receive policy) would
		// otherwise end the run with neither the branch nor the marker,
		// reported as a success — and with nothing on the target pointing at
		// the objects pushed so far, the next run has no have to negotiate
		// against and re-transfers the entire history, on precisely the large
		// repositories batching exists for.
		//
		// Unlike the one-shot path, this cannot defer to prune as its
		// cleaner: Bootstrap() rejects --prune outright, so on that route no
		// cleaner would ever exist and the marker would be permanent. Hence
		// the conditional delete rather than no delete.
		// A branch nothing was pushed for is one the target already holds at
		// the source hash — that is the only way to reach here without a
		// create — so its marker is stale and needs no confirming.
		confirmed, doubt := true, ""
		if createPushed {
			confirmed, doubt = p.createConfirmed(batch.Plan.TargetRef)
		}
		if !confirmed {
			// Held back rather than decided here: whether the branch is on the
			// target is a fact the target can state, and one listing after the
			// last branch settles every doubtful case at once. The marker
			// genuinely holds `current` meanwhile, so the objects behind it
			// stay usable as haves for the branches planned after this one.
			pending = append(pending, pendingMarker{
				branch: batch.Plan.TargetRef, tempRef: batch.TempRef,
				resumeHash: current, createHash: batch.Plan.SourceHash, doubt: doubt,
			})
			completedRefs[batch.TempRef] = current
			p.log("bootstrap batch branch create unconfirmed; deferring marker cleanup",
				"branch", batch.Plan.TargetRef.String(),
				"temp_ref", batch.TempRef.String(),
				"resume_hash", planner.ShortHash(current),
				"pushed_checkpoints", len(pushedCheckpoints),
				"reason", doubt)
			continue
		}

		cmds := []gitproto.PushCommand{{Name: batch.TempRef, Old: current, Delete: true}}
		if err := p.TargetPusher.PushCommands(ctx, cmds); err != nil {
			return result, fmt.Errorf("delete bootstrap temp ref for %s: %w", batch.Plan.TargetRef, err)
		}
		if reason, refused := p.refRefused(batch.TempRef); refused {
			// Not fatal — the branch is there, which is what the operator
			// asked for — but the scaffolding outlived it, and on the
			// bootstrap route prune is not available to reap it.
			p.log("bootstrap temp ref delete refused; marker left on target",
				"branch", batch.Plan.TargetRef.String(),
				"temp_ref", batch.TempRef.String(),
				"reason", reason)
			p.notice(fmt.Sprintf("target refused to delete %s (%s) — remove it by hand or let a --prune run reap it",
				batch.TempRef, reason))
		}
		completedRefs[batch.Plan.TargetRef] = batch.Plan.SourceHash
		completedRefs[batch.TempRef] = batch.Plan.SourceHash
		p.log("bootstrap batch branch finalized", "branch", batch.Plan.TargetRef.String())
	}

	// Settled here rather than after the tail phase: the last create has
	// happened, so the listing is already authoritative, and every extra
	// minute between reading the target's refs and acting on them is a minute
	// in which another run can pick the marker up and start resuming from it.
	resolvePendingMarkers(ctx, p, pending)

	// Tail phase: tags and other-kind refs (issue #1)
	if len(tailPlans) > 0 {
		p.log("bootstrap batch pushing tail refs after branch batches", "tail_count", len(tailPlans))
		if p.OnPhase != nil {
			p.OnPhase(tailPhaseLabel(tailPlans))
		}
		// completedRefs, not a fresh pass over `batches`: it is the map this
		// run maintains as what the target demonstrably holds — branch tips
		// whose creates were confirmed, and the temp refs still anchoring the
		// ones that were not. Re-deriving from the plans here would claim the
		// creates the cutover deliberately withheld.
		tailTargetRefs := planner.CopyRefHashMap(completedRefs)
		packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, tailDesired, tailTargetRefs)
		if err != nil {
			if errors.Is(err, git.NoErrAlreadyUpToDate) {
				cmds := convert.PlansToPushCommands(tailPlans, false)
				if err := p.TargetPusher.PushCommands(ctx, cmds); err != nil {
					return result, fmt.Errorf("create tail refs after bootstrap: %w", err)
				}
			} else {
				return result, fmt.Errorf("fetch bootstrap tail pack: %w", err)
			}
		} else {
			packReader = gitproto.LimitPackReader(packReader, p.MaxPackBytes)
			packReader = gitproto.CloseOnce(packReader)
			cmds := convert.PlansToPushCommands(tailPlans, false)
			if err := p.TargetPusher.PushPack(ctx, cmds, packReader); err != nil {
				_ = packReader.Close()
				return result, fmt.Errorf("push bootstrap tail refs: %w", err)
			}
			_ = packReader.Close()
		}
	}

	result.Pushed = len(plans)
	return result, nil
}

// resolvePendingMarkers settles the markers whose branch create no push could
// confirm, by asking the target which branches it actually has. A marker whose
// branch is present at the hash this run pushed is stale scaffolding and is
// deleted; anything else is kept, with the reason stated. When the target
// cannot be asked, everything is kept: one run's worth of leftover scaffolding
// is recoverable, a discarded resume position is a full re-transfer.
//
// Deliberately not fatal. The branches that did land are on the target, the
// operator asked for those, and a listing failure at the very end of a
// multi-gigabyte import must not turn a delivered import into a failed run.
func resolvePendingMarkers(ctx context.Context, p Params, pending []pendingMarker) {
	if len(pending) == 0 {
		return
	}
	if p.TargetRefsNow == nil {
		for _, marker := range pending {
			keepMarker(p, marker, "no way to list the target's refs")
		}
		return
	}
	refsNow, err := p.TargetRefsNow(ctx)
	if err != nil {
		for _, marker := range pending {
			keepMarker(p, marker, fmt.Sprintf("could not list the target's refs: %v", err))
		}
		return
	}

	cmds := make([]gitproto.PushCommand, 0, len(pending))
	deleted := make([]pendingMarker, 0, len(pending))
	for _, marker := range pending {
		switch onTarget := refsNow[marker.branch]; {
		case onTarget.IsZero():
			keepMarker(p, marker, fmt.Sprintf("%s is not on the target", marker.branch))
		case onTarget != marker.createHash:
			// Someone else's branch, at their commit. Everything between it
			// and this import's tip is reachable from the marker and nothing
			// else, so the marker is still the only thing holding it.
			keepMarker(p, marker, fmt.Sprintf("%s is on the target at %s, not the %s this run pushed",
				marker.branch, planner.ShortHash(onTarget), planner.ShortHash(marker.createHash)))
		default:
			p.log("bootstrap deleting resume marker; branch present at pushed hash",
				"branch", marker.branch.String(), "temp_ref", marker.tempRef.String(),
				"target_hash", planner.ShortHash(onTarget), "doubt", marker.doubt)
			cmds = append(cmds, gitproto.PushCommand{Name: marker.tempRef, Old: marker.resumeHash, Delete: true})
			deleted = append(deleted, marker)
		}
	}
	if len(cmds) == 0 {
		return
	}
	if err := p.TargetPusher.PushCommands(ctx, cmds); err != nil {
		p.log("bootstrap resume marker cleanup failed; markers left on target",
			"marker_count", len(cmds), "error", err.Error())
		p.notice(fmt.Sprintf("could not delete %d stale resume marker(s) — remove them by hand or let a --prune run reap them", len(cmds)))
		return
	}
	// Per marker, because best-effort swallows a per-ref "ng" here exactly as
	// it does at the cutover: a delete the target refused, or one whose Old no
	// longer matches because a concurrent run moved the marker, returns nil.
	for _, marker := range deleted {
		if reason, refused := p.refRefused(marker.tempRef); refused {
			p.log("bootstrap temp ref delete refused; marker left on target",
				"branch", marker.branch.String(), "temp_ref", marker.tempRef.String(),
				"reason", reason)
			p.notice(fmt.Sprintf("target refused to delete %s (%s) — remove it by hand or let a --prune run reap it",
				marker.tempRef, reason))
		}
	}
}

// keepMarker logs and surfaces a resume marker this run is leaving in place.
//
// On the replicate and sync routes the next --prune run reaps it once its
// branch lands, so a marker kept unnecessarily costs one run. On the bootstrap
// route it does not: Bootstrap() rejects --prune, so nothing here will ever
// remove a marker this function keeps, and against a target that neither
// reports per-ref status nor answers a ref listing it is permanent. Accepted
// deliberately, in that order of harm: a marker is inert scaffolding — the
// resume route requires the branch to be absent, so a stale one cannot
// misroute anything — while a marker deleted in error costs a full re-import.
func keepMarker(p Params, marker pendingMarker, why string) {
	p.log("bootstrap keeping resume marker",
		"branch", marker.branch.String(), "temp_ref", marker.tempRef.String(),
		"resume_hash", planner.ShortHash(marker.resumeHash),
		"doubt", marker.doubt, "reason", why)
	p.notice(fmt.Sprintf("%s (%s) — keeping %s at %s so the next run resumes instead of re-importing",
		why, marker.doubt, marker.tempRef, planner.ShortHash(marker.resumeHash)))
}

// tailPhaseLabel returns a phase label matching what's in plans.
func tailPhaseLabel(plans []planner.BranchPlan) string {
	hasTag, hasOther := false, false
	for _, plan := range plans {
		switch plan.Kind {
		case planner.RefKindTag:
			hasTag = true
		case planner.RefKindOther:
			hasOther = true
		case planner.RefKindBranch:
		}
	}
	switch {
	case hasTag && hasOther:
		return "pushing tail refs"
	case hasOther:
		return "pushing other refs"
	default:
		return "pushing tags"
	}
}

// --- Checkpoint planning ---

func planBatches(ctx context.Context, p Params, desired []planner.DesiredRef) ([]plannedBatch, error) {
	ordered, trunkIdx := orderTrunkFirst(desired, p.SourceHeadTarget)
	switch {
	case p.SourceHeadTarget == "":
		p.log("bootstrap batch trunk unset", "reason", "source did not advertise a HEAD symref")
	case trunkIdx < 0:
		p.log("bootstrap batch trunk unset",
			"reason", "HEAD ref not in desired set (filtered by --branch or --map)",
			"source_head_target", p.SourceHeadTarget.String())
	default:
		p.log("bootstrap batch trunk selected",
			"source_head_target", p.SourceHeadTarget.String(),
			"trunk_target_ref", ordered[trunkIdx].TargetRef.String())
	}
	out := make([]plannedBatch, 0, len(ordered))

	var (
		trunkStopSet map[plumbing.Hash]struct{}
		trunkHaves   []plumbing.Hash
	)

	for i, ref := range ordered {
		// Branches whose tip is already reachable from trunk's ancestry need
		// no pack transfer — trunk's batches already delivered every object.
		// Emit a subsumed batch that the executor handles with a single ref
		// create command.
		if i != trunkIdx && trunkStopSet != nil {
			if _, subsumed := trunkStopSet[ref.SourceHash]; subsumed {
				p.log("bootstrap batch branch subsumed by trunk",
					"branch", ref.TargetRef.String(),
					"source_hash", planner.ShortHash(ref.SourceHash))
				out = append(out, plannedBatch{
					BootstrapBatch: planner.BootstrapBatch{
						Plan: planner.BranchPlan{
							Branch: ref.Label, SourceRef: ref.SourceRef,
							TargetRef: ref.TargetRef, SourceHash: ref.SourceHash,
							Kind: ref.Kind, Action: planner.ActionCreate,
						},
					},
					subsumed: true,
				})
				continue
			}
		}
		var (
			haves  []plumbing.Hash
			stopAt map[plumbing.Hash]struct{}
		)
		if i != trunkIdx && trunkStopSet != nil {
			haves = trunkHaves
			stopAt = trunkStopSet
		}
		checkpoints, chain, ancestors, err := planCheckpointsFromChain(ctx, p, ref, haves, stopAt)
		if err != nil {
			return nil, err
		}
		if i == trunkIdx && trunkIdx >= 0 {
			trunkStopSet = ancestors
			trunkHaves = []plumbing.Hash{ref.SourceHash}
		}
		out = append(out, plannedBatch{
			BootstrapBatch: planner.BootstrapBatch{
				Plan: planner.BranchPlan{
					Branch: ref.Label, SourceRef: ref.SourceRef,
					TargetRef: ref.TargetRef, SourceHash: ref.SourceHash,
					Kind: ref.Kind, Action: planner.ActionCreate,
				},
				TempRef:     planner.BootstrapTempRef(ref.TargetRef),
				ResumeHash:  p.TargetRefs[planner.BootstrapTempRef(ref.TargetRef)],
				Checkpoints: checkpoints,
			},
			chain: chain,
		})
	}
	return out, nil
}

// orderTrunkFirst returns desired reordered so the branch matching
// sourceHeadTarget is first. If no match, the original order is preserved and
// trunkIdx is -1. Otherwise trunkIdx is 0 (the position of the trunk).
func orderTrunkFirst(desired []planner.DesiredRef, sourceHeadTarget plumbing.ReferenceName) ([]planner.DesiredRef, int) {
	if sourceHeadTarget == "" {
		return desired, -1
	}
	reordered, idx := hoistFirstMatch(desired, func(r planner.DesiredRef) bool {
		return r.SourceRef == sourceHeadTarget
	})
	if idx < 0 {
		return desired, -1
	}
	return reordered, 0
}

// PlanCheckpoints plans the checkpoint hashes for a single branch during batched bootstrap.
func PlanCheckpoints(ctx context.Context, p Params, ref planner.DesiredRef) ([]plumbing.Hash, error) {
	checkpoints, _, _, err := planCheckpointsFromChain(ctx, p, ref, nil, nil)
	return checkpoints, err
}

// estimatedBytesPerCommit is the heuristic for estimating pack size from commit
// count. Real repos range from ~5 KiB/commit (small web apps) to ~120 KiB
// (blob-heavy monorepos); most mature repos fall in 20–80 KiB. 64 KiB produces
// accurate batch counts for large repos (linux is ~66 KiB/commit) while
// slightly overestimating for small ones (harmless — extra batches finish fast).
// The PACK header pre-check and target-rejection retry catch remaining error.
const estimatedBytesPerCommit = 65536

// planCheckpointsFromChain fetches the source-side commit graph for a branch
// and derives checkpoint hashes for batched bootstrap. When trunkStopAt is
// provided, the walk terminates at commits already reachable from the trunk,
// and trunkHaves is passed to the source fetch so shared history is not
// resent. The returned ancestors set covers every commit in the fetched graph
// (not just the first-parent chain), so callers can accumulate it as a
// cutoff for subsequent branches.
func planCheckpointsFromChain(
	ctx context.Context,
	p Params,
	ref planner.DesiredRef,
	trunkHaves []plumbing.Hash,
	trunkStopAt map[plumbing.Hash]struct{},
) ([]plumbing.Hash, []plumbing.Hash, map[plumbing.Hash]struct{}, error) {
	p.log("bootstrap batch fetching commit graph",
		"branch", ref.TargetRef.String(),
		"have_count", len(trunkHaves),
		"stop_at_count", len(trunkStopAt))

	// Stream the tree:0-filtered commit graph and extract (commit ->
	// parent hashes) directly from the pack, discarding all object
	// content as it arrives. For linux this drops the planning-phase
	// peak from ~4.6 GiB (full in-memory store of ~1.4M decoded
	// commits) to ~100 MiB (parents map plus a small fixed delta
	// cache). See internal/gitproto.ExtractCommitParents. With trunk
	// haves the source sends only divergent commits, so feature
	// branches' planning is already small either way.
	gpRef := gitproto.DesiredRef{SourceRef: ref.SourceRef, TargetRef: ref.TargetRef, SourceHash: ref.SourceHash}
	parentsMap, err := p.SourceService.FetchCommitParents(ctx, p.SourceConn, gpRef, trunkHaves)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("fetch bootstrap commit parents for %s: %w", ref.TargetRef, err)
	}
	var chain []plumbing.Hash
	switch p.Strategy {
	case "", "first-parent":
		c, walkErr := planner.FirstParentChainFromParents(parentsMap, ref.SourceHash, trunkStopAt)
		if walkErr != nil {
			return nil, nil, nil, fmt.Errorf("walk first-parent chain for %s: %w", ref.TargetRef, walkErr)
		}
		chain = c
	case "topo":
		c, walkErr := planner.TopoChainFromParents(parentsMap, ref.SourceHash, trunkStopAt)
		if walkErr != nil {
			return nil, nil, nil, fmt.Errorf("walk topo chain for %s: %w", ref.TargetRef, walkErr)
		}
		chain = c
	default:
		return nil, nil, nil, fmt.Errorf("unsupported bootstrap strategy %q (want \"first-parent\" or \"topo\")", p.Strategy)
	}
	// Every commit the source sent is now an ancestor we can use as a
	// stop set for subsequent branches. Map keys give us the set
	// directly.
	ancestors := make(map[plumbing.Hash]struct{}, len(parentsMap))
	for h := range parentsMap {
		ancestors[h] = struct{}{}
	}

	if len(chain) == 0 {
		// Tip is already covered by the stop set. Emit a single-checkpoint
		// batch so downstream push logic still creates the target ref; the
		// accompanying fetch will find nothing new via haves.
		chain = []plumbing.Hash{ref.SourceHash}
	}

	numBatches := estimateBatchCount(int64(len(chain)), p.TargetMaxPack)
	checkpoints := evenCheckpoints(chain, numBatches)

	p.log("bootstrap batch planned checkpoints",
		"branch", ref.TargetRef.String(),
		"chain_len", len(chain),
		"estimated_batches", len(checkpoints))

	return checkpoints, chain, ancestors, nil
}

func estimateBatchCount(chainLen int64, batchMaxPack int64) int {
	if batchMaxPack <= 0 || chainLen <= 0 {
		return 1
	}
	estimated := chainLen * estimatedBytesPerCommit
	n := int((estimated + batchMaxPack - 1) / batchMaxPack)
	return max(n, 1)
}

// estimatedBytesPerObject is a conservative average for compressed git objects
// in a packfile. Used with the PACK header's object count to estimate total
// pack size before streaming the full pack. Real values range from ~200 bytes
// (tiny commits in a sparse repo) to ~2 KiB (blob-heavy repos), with most
// mature repos averaging 500–1000 bytes. 750 is a reasonable middle ground.
const estimatedBytesPerObject = 750

// checkPackSizeAndSubdivide reads the 12-byte PACK header, multiplies
// the object count by bytesPerObject to estimate total pack size, and
// subdivides via the callback when the estimate exceeds batchLimit.
// Returns (nil, objectCount, nil) when subdivided (caller should retry
// at the same idx), (prepended reader, objectCount, nil) to proceed
// with push, or (prepended reader, 0, nil) when the header could not
// be parsed.
//
// bytesPerObject lets the caller use a per-run calibrated value
// instead of the static estimatedBytesPerObject default. Calibrating
// after each rejection (using the bytes that flowed through
// packStreamObserver) catches blob-heavy repos where the static 750-byte
// average is 10–20× too low — without calibration the pre-flight
// would let oversized sub-packs through and the loop would only learn
// after another wasted ~limit-sized upload. The subdivide callback
// receives the estimated total bytes so user-facing messages can
// quote the projected size that triggered the split.
func checkPackSizeAndSubdivide(
	r io.ReadCloser,
	batchLimit int64,
	bytesPerObject int64,
	subdivide func(estimatedBytes int64) bool,
) (io.ReadCloser, int64, error) { //nolint:unparam // error return kept for future use
	if bytesPerObject <= 0 {
		bytesPerObject = estimatedBytesPerObject
	}
	var header [12]byte
	n, err := io.ReadFull(r, header[:])
	if err != nil {
		// Short pack or error — let the push handle it
		prefixed := io.MultiReader(bytes.NewReader(header[:n]), r)
		return &wrappedMultiRC{Reader: prefixed, Closer: r}, 0, nil //nolint:nilerr // below threshold is not an error, we return the original reader
	}
	if string(header[:4]) != "PACK" {
		// Not a standard packfile — can't estimate, proceed
		prefixed := io.MultiReader(bytes.NewReader(header[:]), r)
		return &wrappedMultiRC{Reader: prefixed, Closer: r}, 0, nil
	}
	objectCount := int64(header[8])<<24 | int64(header[9])<<16 | int64(header[10])<<8 | int64(header[11])
	estimated := objectCount * bytesPerObject

	if estimated > batchLimit && subdivide(estimated) {
		_ = r.Close()
		return nil, objectCount, nil
	}

	prefixed := io.MultiReader(bytes.NewReader(header[:]), r)
	return &wrappedMultiRC{Reader: prefixed, Closer: r}, objectCount, nil
}

// calibrateBytesPerObject derives a per-object byte estimate from a
// rejected push's transmitted-bytes lower bound. sentBytes is the
// amount our request-body counter saw before the server cut us off,
// which means the true pack size is at least sentBytes — likely more.
// The 2× safety multiplier projects from observed lower bound to a
// pessimistic upper bound, so future pre-flight estimates err on the
// side of subdividing rather than over-shooting and re-paying for the
// rejected upload.
//
// Returns 0 when calibration is not possible (no signal, or the new
// value would not be an improvement over the current one) so the
// caller can keep its existing value.
func calibrateBytesPerObject(sentBytes, objectCount, current int64) int64 {
	if sentBytes <= 0 || objectCount <= 0 {
		return 0
	}
	const safetyMultiplier = 2
	calibrated := safetyMultiplier * sentBytes / objectCount
	if calibrated <= current {
		return 0
	}
	return calibrated
}

type wrappedMultiRC struct {
	io.Reader
	io.Closer
}

func (w *wrappedMultiRC) Read(p []byte) (int, error) {
	n, err := w.Reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("read prepended pack: %w", err)
	}
	return n, err //nolint:wrapcheck // io.EOF must not be wrapped to preserve io.Reader contract
}

// chainPosition returns the index of hash in chain, or -1 if not found.
func chainPosition(chain []plumbing.Hash, hash plumbing.Hash) int {
	for i, h := range chain {
		if h == hash {
			return i
		}
	}
	return -1
}

// recombineDropCount picks how many of the upcoming checkpoints to
// drop after a small successful push. Each dropped checkpoint roughly
// doubles the span of the next pack — so doubling sentBytes until
// hitting target/2 gives the count. Capped by maxDrop (always leave
// at least one checkpoint ahead, including the final one) and by a
// hard ceiling that keeps any single overshoot's recovery cost
// bounded. Returns 0 when sentBytes already used at least half the
// limit, when we have no headroom to estimate, or when nothing can be
// dropped.
func recombineDropCount(sentBytes, targetLimit int64, maxDrop int) int {
	const hardCap = 8
	if sentBytes <= 0 || targetLimit <= 0 || maxDrop <= 0 {
		return 0
	}
	target := targetLimit / 2
	if sentBytes >= target {
		return 0
	}
	count := 0
	span := sentBytes
	for span*2 <= target && count < maxDrop && count < hardCap {
		span *= 2
		count++
	}
	return count
}

// minBytesBeforeAbort is the floor below which the projection-based
// abort heuristic stays silent. The first few KB of a pack are header
// + small objects; their bytes/object ratio doesn't represent the
// rest. Waiting until at least this many bytes have flowed avoids
// pathological "abort the second the header arrives" behaviour while
// still cutting losses well before the configured budget is reached.
const minBytesBeforeAbort = 8 * 1024 * 1024

// shouldAbortPush decides whether an in-flight push has crossed the
// "we are clearly going to overshoot the budget" threshold. Two
// regimes:
//
//   - Pack header has been parsed (totalObjects > 0) AND at least one
//     object has fully gone through (objectsSent > 0): project the
//     final pack size as bytesSent × totalObjects ÷ objectsSent and
//     abort if that projection exceeds budget × safety.
//
//   - Header not yet observed or no full object yet: fall back to a
//     simple bytesSent ≥ budget × safety check. Catches the common
//     "we have a budget from a prior 413, just don't send past it
//     again" case before the parser has anything to say.
//
// effectiveObjectsSent returns the divisor used for post-failure
// calibration and projection. When the upload self-aborted after
// the pack header was parsed but before any object completed, the
// observer reports objectsSent == 0 — yet the partially-observed
// first object alone consumed sentBytes worth of upload, so a
// pessimistic-but-bounded estimate treats it as one observation
// instead of falling back to the full pack header count (which
// would understate per-object size and produce a factor of 2,
// recreating the slow 1→2→4→… convergence streaming-pack-parse is
// trying to remove). Returns objectsSent as-is otherwise, which
// may itself be zero when the header hasn't been parsed.
func effectiveObjectsSent(objectsSent, totalObjects int64, abortedEarly bool) int64 {
	if objectsSent == 0 && totalObjects > 0 && abortedEarly {
		return 1
	}
	return objectsSent
}

// minBytesBeforeAbort gates only the projection path: the pack header
// alone shouldn't be allowed to project a doomed upload off the noise
// of the first KB. The absolute "we already crossed the budget"
// trigger fires regardless of the floor — once the server (or a
// learned proxy cutoff) said the cap is N and we've sent ≥ N, there
// is nothing left to learn by sending more.
func shouldAbortPush(bytesSent, objectsSent, totalObjects, budget int64) bool {
	if budget <= 0 {
		return false
	}
	const safety = 95 // percent of budget at which we cut
	threshold := budget * safety / 100
	if bytesSent >= threshold {
		return true
	}
	if bytesSent < minBytesBeforeAbort {
		return false
	}
	if objectsSent > 0 && totalObjects > 0 {
		// Guard against divide-by-zero and against late-stage spikes
		// when objectsSent has caught up to totalObjects (projection
		// becomes the current bytesSent itself, harmless).
		projected := bytesSent * totalObjects / objectsSent
		return projected > threshold
	}
	return false
}

// observedSubdivisionFactor estimates how many sub-packs a rejected
// push should be split into based on bytes actually transmitted before
// the server cut us off.
//
// The safety multiplier varies with how close sentBytes came to the
// limit. When the rejection arrived within ~10% of the limit (the
// common reverse-proxy case where the server cuts mid-stream at its
// body cap), the true pack size is essentially unknown — it is at
// least sentBytes but may be many times larger. A 4× multiplier in
// that regime converges in one or two rounds instead of dancing
// through 1 → 2 → 4 → 8 → … one round per rejection. When the
// rejection arrived comfortably under the limit (a server with
// stricter limits announcing the failure early), 2× is enough since
// sentBytes is closer to the real pack size.
// nextSelfImposedBudget refines the in-flight self-imposed upload
// ceiling after a server-rejected push (i.e. not one the client
// aborted itself). It prefers the explicit body limit when the server
// announced one — that's authoritative — and falls back to the
// empirical sent-bytes cutoff only when no parseable limit is
// available (e.g. Cloudflare's HTML 413). Reverse proxies sometimes
// reject after only a few MiB even though the actual cap is much
// higher; ratcheting to that early-cutoff would cause subsequent runs
// to over-subdivide for no reason.
//
// The budget only ratchets down: if the new candidate isn't smaller
// than the current ceiling, the current value stays.
func nextSelfImposedBudget(current, parsedLimit, sentBytes int64, abortedEarly bool) int64 {
	if abortedEarly {
		return current
	}
	candidate := int64(0)
	switch {
	case parsedLimit > 0:
		candidate = parsedLimit
	case sentBytes > 0:
		candidate = sentBytes
	}
	if candidate <= 0 {
		return current
	}
	if current == 0 || candidate < current {
		return candidate
	}
	return current
}

func observedSubdivisionFactor(sentBytes, limit int64) int {
	if sentBytes <= 0 || limit <= 0 {
		return 2
	}
	safetyMultiplier := int64(2)
	if sentBytes*10 >= limit*9 {
		safetyMultiplier = 4
	}
	factor := int((sentBytes*safetyMultiplier + limit - 1) / limit)
	if factor < 2 {
		factor = 2
	}
	return factor
}

// isIndivisibleCheckpoint reports whether the span from current to the
// checkpoint at idx is a single commit — the point at which subdivision has
// nothing left to split.
//
// It measures against `current`, the commit the pack is actually being built
// from, not against Checkpoints[idx-1]: on the stale-temp-ref resume path the
// checkpoint list is re-planned from ResumeHash with startIdx 0, so idx-1 does
// not exist and the gap would be counted from the chain root — reporting
// "divisible" for a span subdivideToFactor just proved otherwise, on exactly
// the resume route this all exists to serve.
//
// A hash missing from the chain answers false, which merely keeps the error
// retryable — the safe direction.
func isIndivisibleCheckpoint(batch plannedBatch, current plumbing.Hash, idx int) bool {
	if len(batch.chain) == 0 || idx >= len(batch.Checkpoints) {
		return false
	}
	cpIdx := chainPosition(batch.chain, batch.Checkpoints[idx])
	if cpIdx < 0 {
		return false
	}
	prevIdx := -1 // nothing pushed yet: the span starts before the chain root
	if !current.IsZero() {
		if prevIdx = chainPosition(batch.chain, current); prevIdx < 0 {
			return false
		}
	}
	return cpIdx-prevIdx == 1
}

// subdivideToFactor halves the remaining checkpoint ranges at least
// once and keeps going while the result is still under targetCount.
// Returns the input unchanged only when no further split is possible
// (every remaining gap is already 1 commit).
//
// The unconditional first round matters when targetCount <=
// len(remaining): each surviving range may still produce a pack over
// the target limit (factor is computed from one rejected attempt, but
// a multi-piece batch can keep over-shooting if sent_bytes ≈ limit on
// every retry, so factor stays at 2 indefinitely). Always subdividing
// once guarantees post-rejection retries make forward progress instead
// of returning the caller into a hard failure.
func subdivideToFactor(
	chain []plumbing.Hash,
	current plumbing.Hash,
	remaining []plumbing.Hash,
	targetCount int,
) []plumbing.Hash {
	expanded := subdivideCheckpoints(chain, current, remaining)
	if len(expanded) <= len(remaining) {
		// Cannot split further — every gap is already 1 commit.
		return remaining
	}
	for len(expanded) < targetCount {
		next := subdivideCheckpoints(chain, current, expanded)
		if len(next) <= len(expanded) {
			break
		}
		expanded = next
	}
	return expanded
}

// subdivideCheckpoints splits each remaining checkpoint range in half using
// the full commit chain. Called when a batch push is rejected for exceeding
// the target's body-size limit. Returns the expanded checkpoint list; if no
// split is possible (ranges are already 1 commit), returns the input unchanged.
func subdivideCheckpoints(chain []plumbing.Hash, current plumbing.Hash, remaining []plumbing.Hash) []plumbing.Hash {
	chainIdx := make(map[plumbing.Hash]int, len(chain))
	for i, h := range chain {
		chainIdx[h] = i
	}
	curIdx, ok := chainIdx[current]
	if !ok && !current.IsZero() {
		return remaining
	}
	if current.IsZero() {
		curIdx = -1
	}

	expanded := make([]plumbing.Hash, 0, len(remaining)*2)
	prev := curIdx
	for _, cp := range remaining {
		cpIdx, ok := chainIdx[cp]
		if !ok {
			expanded = append(expanded, cp)
			continue
		}
		gap := cpIdx - prev
		if gap > 1 {
			midIdx := prev + gap/2
			expanded = append(expanded, chain[midIdx])
		}
		expanded = append(expanded, cp)
		prev = cpIdx
	}
	return expanded
}

func evenCheckpoints(chain []plumbing.Hash, numBatches int) []plumbing.Hash {
	if numBatches <= 1 || len(chain) <= 1 || numBatches >= len(chain) {
		return []plumbing.Hash{chain[len(chain)-1]}
	}
	checkpoints := make([]plumbing.Hash, 0, numBatches)
	batchSize := len(chain) / numBatches
	for i := range numBatches - 1 {
		idx := (i+1)*batchSize - 1
		if idx >= len(chain)-1 {
			break
		}
		checkpoints = append(checkpoints, chain[idx])
	}
	checkpoints = append(checkpoints, chain[len(chain)-1])
	return checkpoints
}

// buildCheckpointHaves merges every checkpoint already pushed in this
// batch with the already-completed branch tips into a single haves
// map for the next checkpoint fetch. Under topo ordering each
// checkpoint covers a different region of the graph: the latest one
// is on the chain's frontier, but earlier checkpoints capture side
// branches that aren't ancestors of the latest. Declaring all of
// them keeps merge commits' deltas to genuinely-new content instead
// of re-sending side-branch ancestry we already pushed.
//
// Each pushed checkpoint gets a synthetic ref name so the map
// deduplicates by position; the wire only carries the hash values.
func buildCheckpointHaves(
	tempRef plumbing.ReferenceName,
	pushedCheckpoints []plumbing.Hash,
	completedRefs map[plumbing.ReferenceName]plumbing.Hash,
) map[plumbing.ReferenceName]plumbing.Hash {
	haves := planner.CopyRefHashMap(completedRefs)
	for i, h := range pushedCheckpoints {
		if h.IsZero() {
			continue
		}
		name := plumbing.ReferenceName(fmt.Sprintf("%s-have-%d", tempRef, i))
		haves[name] = h
	}
	return haves
}

func packReaderForCheckpoint(
	ctx context.Context,
	p Params,
	batch plannedBatch,
	checkpoint plumbing.Hash,
	pushedCheckpoints []plumbing.Hash,
	completedRefs map[plumbing.ReferenceName]plumbing.Hash,
	batchLimit int64,
) (io.ReadCloser, error) {
	desired := singleGP(batch.Plan.SourceRef, batch.TempRef, checkpoint)
	haves := buildCheckpointHaves(batch.TempRef, pushedCheckpoints, completedRefs)
	packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, desired, haves)
	if err != nil {
		return nil, fmt.Errorf("fetch checkpoint pack: %w", err)
	}
	return gitproto.LimitPackReader(packReader, batchLimit), nil
}

// --- GitHub preflight ---

func githubBatchLimit(ctx context.Context, p Params) (int64, bool) {
	if p.TargetMaxPack > 0 || p.SourceConn == nil || p.SourceConn.Endpoint() == nil {
		return 0, false
	}
	if p.SourceService == nil || !p.SourceService.SupportsBootstrapBatch() {
		return 0, false
	}
	repoSizeKB, ok := lookupGitHubRepoSizeKB(ctx, p.SourceConn)
	if !ok || repoSizeKB < githubLargeRepoThresholdKB {
		return 0, false
	}
	limit := int64(defaultTargetMaxPackBytes)
	if p.MaxPackBytes > 0 && p.MaxPackBytes < limit {
		limit = p.MaxPackBytes
	}
	if limit <= 0 {
		return 0, false
	}
	return limit, true
}

func lookupGitHubRepoSizeKB(ctx context.Context, conn gitproto.Conn) (int64, bool) {
	httpConn, ok := conn.(*gitproto.HTTPConn)
	if !ok {
		return 0, false
	}
	owner, repo, ok := GitHubOwnerRepo(conn)
	if !ok {
		return 0, false
	}
	apiURL := strings.TrimRight(GitHubRepoAPIBaseURL, "/") + "/repos/" + owner + "/" + repo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Github-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", useragent.Plain())
	req.Header.Set(gitproto.StatsPhaseHeader, "github repo metadata")
	resp, err := httpConn.HTTP.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var payload struct {
		Size int64 `json:"size"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || payload.Size <= 0 {
		return 0, false
	}
	return payload.Size, true
}

// GitHubOwnerRepo extracts the owner/repo from a GitHub endpoint.
func GitHubOwnerRepo(conn gitproto.Conn) (string, string, bool) {
	if conn == nil || conn.Endpoint() == nil {
		return "", "", false
	}
	ep := conn.Endpoint()
	if ep.Scheme != "http" && ep.Scheme != "https" {
		return "", "", false
	}
	if !strings.EqualFold(ep.Hostname(), "github.com") {
		return "", "", false
	}
	path := strings.TrimSuffix(strings.Trim(ep.Path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func autoTargetMaxPackBytes(p Params, err error) (int64, bool) {
	if p.TargetMaxPack > 0 || !isBatchableTargetPushError(err) {
		return 0, false
	}
	if p.SourceService == nil || !p.SourceService.SupportsBootstrapBatch() {
		return 0, false
	}
	limit := int64(defaultTargetMaxPackBytes)
	if targetLimit := targetBodyLimit(err); targetLimit > 0 {
		derived := targetLimit / 2
		if derived <= 0 {
			derived = targetLimit
		}
		if derived < limit {
			limit = derived
		}
	}
	if p.MaxPackBytes > 0 && p.MaxPackBytes < limit {
		limit = p.MaxPackBytes
	}
	if limit <= 0 {
		return 0, false
	}
	return limit, true
}

func isTargetBodyLimitError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "body exceeded size limit") ||
		(strings.Contains(msg, "request body") && strings.Contains(msg, "too large")) ||
		(strings.Contains(msg, "payload") && strings.Contains(msg, "too large")) ||
		strings.Contains(msg, "http 413")
}

// isTargetPushDeadlineError reports whether err indicates the target cut the
// receive-pack POST short because it ran past a server-side deadline rather
// than because the pack exceeded an announced size limit. GitHub returns 408
// (Request Timeout) when a slow or oversized push outlasts its receive-pack
// wall-clock window — common when relaying a large repo over a slow source
// link, where the upstream read rate throttles the downstream write. Gateways
// fronting other hosts surface the same condition as 504 (Gateway Timeout).
//
// Both are remedied the way a body-limit rejection is: smaller packs each
// finish inside the window, so callers route them into the same batched
// bootstrap retry. Kept distinct from isTargetBodyLimitError because the
// trigger is a timeout, not a size rejection, and there's no body limit to
// parse out of the message.
func isTargetPushDeadlineError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "http 408") || strings.Contains(msg, "http 504")
}

// isBatchableTargetPushError reports whether err is a target-side push failure
// that batched bootstrap can work around by sending smaller packs: an explicit
// body-size rejection (413 / "body exceeded size limit") or a receive-pack
// deadline (408 / 504).
func isBatchableTargetPushError(err error) bool {
	return isTargetBodyLimitError(err) || isTargetPushDeadlineError(err)
}

// actionableTargetPushError augments a one-shot push failure with guidance
// when the target couldn't receive the pack — too large or too slow — but batched
// bootstrap couldn't take over — which, on the one-shot path, means the source
// can't serve the protocol-v2 fetch filter that checkpointing requires. The
// extra context tells the user why the obvious knob (--target-max-pack-bytes)
// won't help here, instead of leaving a bare "http 408". Returns err unchanged
// for non-batchable failures or when batching is in fact available.
func actionableTargetPushError(p Params, err error) error {
	if !isBatchableTargetPushError(err) {
		return err
	}
	if p.SourceService != nil && p.SourceService.SupportsBootstrapBatch() {
		return err
	}
	return fmt.Errorf("%w (target could not receive the pack — too large, or too slow to "+
		"receive within its deadline; batched bootstrap could split it into smaller pushes, "+
		"but the source does not support the protocol-v2 fetch filter batched bootstrap requires)", err)
}

func targetBodyLimit(err error) int64 {
	if err == nil {
		return 0
	}
	matches := bodyLimitPattern.FindStringSubmatch(strings.ToLower(err.Error()))
	if len(matches) != 2 {
		return 0
	}
	limit, parseErr := strconv.ParseInt(matches[1], 10, 64)
	if parseErr != nil {
		return 0
	}
	return limit
}

// --- Shared helpers ---

func singleGP(sourceRef, targetRef plumbing.ReferenceName, hash plumbing.Hash) map[plumbing.ReferenceName]gitproto.DesiredRef {
	return map[plumbing.ReferenceName]gitproto.DesiredRef{
		targetRef: {SourceRef: sourceRef, TargetRef: targetRef, SourceHash: hash},
	}
}

func (p Params) log(msg string, args ...any) {
	if p.Logger == nil {
		return
	}
	p.Logger.Info(msg, args...)
}
