// Package bootstrap implements the bootstrap relay strategy for gitsync.
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
	"runtime"
	"strconv"
	"strings"
	"sync"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/storage/memory"

	"entire.io/entire/gitsync/internal/convert"
	"entire.io/entire/gitsync/internal/gitproto"
	"entire.io/entire/gitsync/internal/planner"
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
	SourceConn    *gitproto.Conn
	SourceService interface {
		FetchPack(ctx context.Context, conn *gitproto.Conn, desired map[plumbing.ReferenceName]gitproto.DesiredRef, haves map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error)
		FetchCommitGraph(ctx context.Context, store storer.Storer, conn *gitproto.Conn, ref gitproto.DesiredRef, haves []plumbing.Hash) error
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
}

// Result holds the outcome of the bootstrap strategy.
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
	packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, gpDesired, nil)
	if err != nil {
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			return result, nil
		}
		return result, fmt.Errorf("fetch source pack: %w", err)
	}
	packReader = gitproto.LimitPackReader(packReader, p.MaxPackBytes)
	packReader = closeOnce(packReader)

	p.log("bootstrap pushing refs to target", "ref_count", len(plans))
	cmds := convert.PlansToPushCommands(plans)
	pushErr := p.TargetPusher.PushPack(ctx, cmds, packReader)
	_ = packReader.Close()
	if pushErr != nil {
		autoBatch, ok := autoTargetMaxPackBytes(p, pushErr)
		if !ok {
			return result, fmt.Errorf("push target refs: %w", pushErr)
		}
		p.log("bootstrap retrying with batched mode after target rejection",
			"target_max_pack_bytes", autoBatch)
		p.TargetMaxPack = autoBatch
		return executeBatched(ctx, p, plans, result)
	}

	result.Pushed = len(plans)
	return result, nil
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
		return result, errors.New("bootstrap batching requires protocol v2 source fetch filter support")
	}

	planRefs := make([]planner.DesiredRef, 0, len(plans))
	tagPlans := make([]planner.BranchPlan, 0, len(plans))
	tagDesired := make(map[plumbing.ReferenceName]gitproto.DesiredRef)
	for _, plan := range plans {
		if plan.Kind == planner.RefKindTag {
			tagPlans = append(tagPlans, plan)
			if d, ok := p.DesiredRefs[plan.TargetRef]; ok {
				tagDesired[plan.TargetRef] = gitproto.DesiredRef{
					SourceRef: d.SourceRef, TargetRef: d.TargetRef,
					SourceHash: d.SourceHash, IsTag: true,
				}
			}
			continue
		}
		if !plan.SourceRef.IsBranch() || !plan.TargetRef.IsBranch() {
			return result, errors.New("bootstrap batching currently supports branch refs and create-only tags")
		}
		planRefs = append(planRefs, p.DesiredRefs[plan.TargetRef])
	}

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
			current = plumbing.ZeroHash
			startIdx = 0
		}

		// Manual index loop: subdivide may insert checkpoints at the current
		// index, so we must not auto-increment after a retry.
		idx := startIdx
		for idx < len(batch.Checkpoints) {
			checkpoint := batch.Checkpoints[idx]
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
				stagePlans = append(stagePlans, planner.BranchPlan{
					Branch: batch.Plan.Branch, SourceRef: batch.Plan.SourceRef,
					TargetRef: batch.Plan.TargetRef, SourceHash: checkpoint,
					TargetHash: plumbing.ZeroHash, Kind: batch.Plan.Kind,
					Action: planner.ActionCreate,
					Reason: fmt.Sprintf("create %s at %s", batch.Plan.TargetRef, planner.ShortHash(checkpoint)),
				})
			}

			packReader, err := packReaderForCheckpoint(ctx, p, batch, checkpoint, current, completedRefs, fetchLimit)
			if err != nil {
				return result, fmt.Errorf("fetch source batch pack for %s: %w", batch.Plan.TargetRef, err)
			}
			packReader = closeOnce(packReader)

			// Peek at the PACK header (12 bytes) to get the object count.
			// If the estimated pack size exceeds the batch limit, subdivide
			// immediately instead of pushing a pack the target will reject.
			// This avoids wasting a multi-GiB transfer on a doomed push.
			if p.TargetMaxPack > 0 && len(batch.chain) > 0 {
				subdivided := false
				packReader, err = checkPackSizeAndSubdivide(packReader, p.TargetMaxPack, func() bool {
					expanded := subdivideCheckpoints(batch.chain, current, batch.Checkpoints[idx:])
					if len(expanded) > len(batch.Checkpoints[idx:]) {
						p.log("bootstrap batch subdividing before push (pack header estimate)",
							"branch", batch.Plan.TargetRef.String(),
							"old_remaining", len(batch.Checkpoints[idx:]),
							"new_remaining", len(expanded))
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

			cmds := convert.PlansToPushCommands(stagePlans)
			if err := p.TargetPusher.PushPack(ctx, cmds, packReader); err != nil {
				_ = packReader.Close()
				if isTargetBodyLimitError(err) && len(batch.chain) > 0 {
					expanded := subdivideCheckpoints(batch.chain, current, batch.Checkpoints[idx:])
					if len(expanded) > len(batch.Checkpoints[idx:]) {
						p.log("bootstrap batch subdividing after target size rejection",
							"branch", batch.Plan.TargetRef.String(),
							"old_remaining", len(batch.Checkpoints[idx:]),
							"new_remaining", len(expanded),
							"error", err.Error())
						batch.Checkpoints = append(batch.Checkpoints[:idx], expanded...)
						continue // retry at same idx with new (smaller) checkpoint
					}
				}
				return result, fmt.Errorf("push bootstrap batch for %s: %w", batch.Plan.TargetRef, err)
			}
			_ = packReader.Close()
			p.log("bootstrap batch checkpoint complete",
				"branch", batch.Plan.TargetRef.String(),
				"batch", idx+1,
				"batch_total", len(batch.Checkpoints))
			current = checkpoint
			result.BatchCount++
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
		}

		cmds := []gitproto.PushCommand{{Name: batch.TempRef, Old: current, Delete: true}}
		if err := p.TargetPusher.PushCommands(ctx, cmds); err != nil {
			return result, fmt.Errorf("delete bootstrap temp ref for %s: %w", batch.Plan.TargetRef, err)
		}
		completedRefs[batch.Plan.TargetRef] = batch.Plan.SourceHash
		completedRefs[batch.TempRef] = batch.Plan.SourceHash
		p.log("bootstrap batch branch finalized", "branch", batch.Plan.TargetRef.String())
	}

	// Tag phase (issue #1)
	if len(tagPlans) > 0 {
		p.log("bootstrap batch pushing tags after branch batches", "tag_count", len(tagPlans))
		tagTargetRefs := planner.CopyRefHashMap(p.TargetRefs)
		for _, batch := range batches {
			tagTargetRefs[batch.Plan.TargetRef] = batch.Plan.SourceHash
		}
		packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, tagDesired, tagTargetRefs)
		if err != nil {
			if errors.Is(err, git.NoErrAlreadyUpToDate) {
				cmds := convert.PlansToPushCommands(tagPlans)
				if err := p.TargetPusher.PushCommands(ctx, cmds); err != nil {
					return result, fmt.Errorf("create tag refs after bootstrap: %w", err)
				}
			} else {
				return result, fmt.Errorf("fetch bootstrap tag pack: %w", err)
			}
		} else {
			packReader = gitproto.LimitPackReader(packReader, p.MaxPackBytes)
			packReader = closeOnce(packReader)
			cmds := convert.PlansToPushCommands(tagPlans)
			if err := p.TargetPusher.PushPack(ctx, cmds, packReader); err != nil {
				_ = packReader.Close()
				return result, fmt.Errorf("push bootstrap tags: %w", err)
			}
			_ = packReader.Close()
		}
	}

	result.Pushed = len(plans)
	result.Batching = true
	result.RelayMode = "bootstrap-batch"
	return result, nil
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
	for i, ref := range desired {
		if ref.SourceRef == sourceHeadTarget {
			if i == 0 {
				return desired, 0
			}
			reordered := make([]planner.DesiredRef, 0, len(desired))
			reordered = append(reordered, ref)
			reordered = append(reordered, desired[:i]...)
			reordered = append(reordered, desired[i+1:]...)
			return reordered, 0
		}
	}
	return desired, -1
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

	// Fetch all commits (tree:0 filter) into a temporary in-memory store.
	// Without haves this is ~1.4M commits for linux (~4.6 GB transient).
	// With trunk haves, the source only sends commits not reachable from
	// trunk, which for typical feature branches is tiny. We extract the
	// first-parent chain immediately and discard the store so GC can
	// reclaim it. The transient spike is unavoidable with git protocol v2
	// (no first-parent-only fetch) and go-git's pack parser (needs full
	// store for delta resolution).
	graphStore := memory.NewStorage()
	gpRef := gitproto.DesiredRef{SourceRef: ref.SourceRef, TargetRef: ref.TargetRef, SourceHash: ref.SourceHash}
	if err := p.SourceService.FetchCommitGraph(ctx, graphStore, p.SourceConn, gpRef, trunkHaves); err != nil {
		return nil, nil, nil, fmt.Errorf("fetch bootstrap planning graph for %s: %w", ref.TargetRef, err)
	}
	chain, err := planner.FirstParentChainStoppingAt(graphStore, ref.SourceHash, trunkStopAt)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("walk first-parent chain for %s: %w", ref.TargetRef, err)
	}
	// Collect all commit hashes in the fetched graph so callers can extend
	// their stop set. We only need the keys — ~8 bytes per commit, so the
	// linux ancestry set is ~11 MB vs the store's ~4.6 GB.
	ancestors, err := collectCommitHashes(graphStore)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("collect commit hashes for %s: %w", ref.TargetRef, err)
	}
	// graphStore is unused beyond this point; runtime.GC reclaims its
	// transient allocations before we move on to the next branch.
	runtime.GC()

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

// collectCommitHashes returns the set of commit hashes in store. Used to build
// the trunk reachability set for subsequent branches' stop-at walks.
func collectCommitHashes(store *memory.Storage) (map[plumbing.Hash]struct{}, error) {
	iter, err := store.IterEncodedObjects(plumbing.CommitObject)
	if err != nil {
		return nil, fmt.Errorf("iterate commit objects: %w", err)
	}
	defer iter.Close()
	out := map[plumbing.Hash]struct{}{}
	if err := iter.ForEach(func(obj plumbing.EncodedObject) error {
		out[obj.Hash()] = struct{}{}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("walk commit objects: %w", err)
	}
	return out, nil
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

// checkPackSizeAndSubdivide reads the 12-byte PACK header to get the object
// count, estimates total pack size, and if it exceeds batchLimit, closes the
// reader and calls subdivide(). Returns (nil, nil) when subdivided (caller
// should continue to retry), or (prepended reader, nil) to proceed with push.
func checkPackSizeAndSubdivide(
	r io.ReadCloser,
	batchLimit int64,
	subdivide func() bool,
) (io.ReadCloser, error) { //nolint:unparam // error return kept for future use
	var header [12]byte
	n, err := io.ReadFull(r, header[:])
	if err != nil {
		// Short pack or error — let the push handle it
		prefixed := io.MultiReader(bytes.NewReader(header[:n]), r)
		return &wrappedMultiRC{Reader: prefixed, Closer: r}, nil //nolint:nilerr // below threshold is not an error, we return the original reader
	}
	if string(header[:4]) != "PACK" {
		// Not a standard packfile — can't estimate, proceed
		prefixed := io.MultiReader(bytes.NewReader(header[:]), r)
		return &wrappedMultiRC{Reader: prefixed, Closer: r}, nil
	}
	objectCount := int64(header[8])<<24 | int64(header[9])<<16 | int64(header[10])<<8 | int64(header[11])
	estimated := objectCount * estimatedBytesPerObject

	if estimated > batchLimit && subdivide() {
		_ = r.Close()
		return nil, nil //nolint:nilnil // nil reader signals no subdivision needed
	}

	prefixed := io.MultiReader(bytes.NewReader(header[:]), r)
	return &wrappedMultiRC{Reader: prefixed, Closer: r}, nil
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

func packReaderForCheckpoint(
	ctx context.Context,
	p Params,
	batch plannedBatch,
	checkpoint plumbing.Hash,
	current plumbing.Hash,
	completedRefs map[plumbing.ReferenceName]plumbing.Hash,
	batchLimit int64,
) (io.ReadCloser, error) {
	desired := singleGP(batch.Plan.SourceRef, batch.TempRef, checkpoint)
	// Merge the current checkpoint (within this branch) with any
	// already-completed branch tips so the source can delta-compress
	// against objects the target already has from previous branches.
	haves := planner.CopyRefHashMap(completedRefs)
	if !current.IsZero() {
		haves[batch.TempRef] = current
	}
	packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, desired, haves)
	if err != nil {
		return nil, fmt.Errorf("fetch checkpoint pack: %w", err)
	}
	return gitproto.LimitPackReader(packReader, batchLimit), nil
}

// --- GitHub preflight ---

func githubBatchLimit(ctx context.Context, p Params) (int64, bool) {
	if p.TargetMaxPack > 0 || p.SourceConn == nil || p.SourceConn.Endpoint == nil {
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

func lookupGitHubRepoSizeKB(ctx context.Context, conn *gitproto.Conn) (int64, bool) {
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
	req.Header.Set("User-Agent", capability.DefaultAgent())
	req.Header.Set(gitproto.StatsPhaseHeader, "github repo metadata")
	resp, err := conn.HTTP.Do(req)
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
func GitHubOwnerRepo(conn *gitproto.Conn) (string, string, bool) {
	if conn == nil || conn.Endpoint == nil {
		return "", "", false
	}
	ep := conn.Endpoint
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
	if p.TargetMaxPack > 0 || !isTargetBodyLimitError(err) {
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

type closeOnceReadCloser struct {
	io.ReadCloser

	once sync.Once
}

func (c *closeOnceReadCloser) Close() error {
	var err error
	c.once.Do(func() {
		err = c.ReadCloser.Close()
	})
	if err != nil {
		return fmt.Errorf("close pack reader: %w", err)
	}
	return nil
}

func closeOnce(rc io.ReadCloser) io.ReadCloser {
	if rc == nil {
		return nil
	}
	if _, ok := rc.(*closeOnceReadCloser); ok {
		return rc
	}
	return &closeOnceReadCloser{ReadCloser: rc}
}
