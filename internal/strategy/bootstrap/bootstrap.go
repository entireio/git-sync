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
	"sync"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/storage/memory"

	"github.com/soph/git-sync/internal/convert"
	"github.com/soph/git-sync/internal/gitproto"
	"github.com/soph/git-sync/internal/planner"
)

const (
	defaultTargetMaxPackBytes = 512 * 1024 * 1024
	githubLargeRepoThresholdKB   = 1536 * 1024
)

var bodyLimitPattern = regexp.MustCompile(`body exceeded size limit ([0-9]+)`)

// GitHubRepoAPIBaseURL is the base for GitHub API calls (replaceable in tests).
var GitHubRepoAPIBaseURL = "https://api.github.com"

// Params holds the inputs for a bootstrap execution.
type Params struct {
	SourceConn    *gitproto.Conn
	SourceService interface {
		FetchPack(context.Context, *gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error)
		FetchCommitGraph(context.Context, storer.Storer, *gitproto.Conn, gitproto.DesiredRef) error
		SupportsBootstrapBatch() bool
	}
	TargetPusher interface {
		PushPack(context.Context, []gitproto.PushCommand, io.ReadCloser) error
		PushCommands(context.Context, []gitproto.PushCommand) error
	}
	DesiredRefs  map[plumbing.ReferenceName]planner.DesiredRef
	TargetRefs   map[plumbing.ReferenceName]plumbing.Hash
	MaxPackBytes int64
	TargetMaxPack int64
	Verbose      bool
	Logger       *slog.Logger
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
}

// Execute runs the bootstrap strategy (one-shot or batched).
func Execute(ctx context.Context, p Params, relayReason string) (Result, error) {
	if p.TargetPusher == nil {
		return Result{Relay: true, RelayMode: "bootstrap", RelayReason: relayReason}, fmt.Errorf("bootstrap strategy requires TargetPusher")
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
		return Result{}, err
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

func executeBatched(
	ctx context.Context,
	p Params,
	plans []planner.BranchPlan,
	result Result,
) (Result, error) {
	if !p.SourceService.SupportsBootstrapBatch() {
		return result, fmt.Errorf("bootstrap batching requires protocol v2 source fetch filter support")
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
			return result, fmt.Errorf("bootstrap batching currently supports branch refs and create-only tags")
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

	for _, batch := range batches {
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

			packReader, err := packReaderForCheckpoint(ctx, p, batch, checkpoint, current, fetchLimit)
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
	out := make([]plannedBatch, 0, len(desired))
	for _, ref := range desired {
		checkpoints, chain, err := planCheckpointsFromChain(ctx, p, ref)
		if err != nil {
			return nil, err
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

// PlanCheckpoints plans the checkpoint hashes for a single branch during batched bootstrap.
func PlanCheckpoints(ctx context.Context, p Params, ref planner.DesiredRef) ([]plumbing.Hash, error) {
	checkpoints, _, err := planCheckpointsFromChain(ctx, p, ref)
	return checkpoints, err
}

// estimatedBytesPerCommit is the heuristic for estimating pack size from commit
// count. Real repos range from ~5 KiB/commit (small web apps) to ~120 KiB
// (blob-heavy monorepos); most mature repos fall in 20–80 KiB. 64 KiB produces
// accurate batch counts for large repos (linux is ~66 KiB/commit) while
// slightly overestimating for small ones (harmless — extra batches finish fast).
// The PACK header pre-check and target-rejection retry catch remaining error.
const estimatedBytesPerCommit = 65536

func planCheckpointsFromChain(ctx context.Context, p Params, ref planner.DesiredRef) ([]plumbing.Hash, []plumbing.Hash, error) {
	p.log("bootstrap batch fetching commit graph", "branch", ref.TargetRef.String())
	graphStore := memory.NewStorage()
	gpRef := gitproto.DesiredRef{SourceRef: ref.SourceRef, TargetRef: ref.TargetRef, SourceHash: ref.SourceHash}
	if err := p.SourceService.FetchCommitGraph(ctx, graphStore, p.SourceConn, gpRef); err != nil {
		return nil, nil, fmt.Errorf("fetch bootstrap planning graph for %s: %w", ref.TargetRef, err)
	}
	chain, err := planner.FirstParentChain(graphStore, ref.SourceHash)
	if err != nil {
		return nil, nil, fmt.Errorf("walk first-parent chain for %s: %w", ref.TargetRef, err)
	}
	if len(chain) == 0 {
		return nil, nil, fmt.Errorf("empty first-parent chain for %s", ref.TargetRef)
	}

	numBatches := estimateBatchCount(int64(len(chain)), p.TargetMaxPack)
	checkpoints := evenCheckpoints(chain, numBatches)

	p.log("bootstrap batch planned checkpoints",
		"branch", ref.TargetRef.String(),
		"chain_len", len(chain),
		"estimated_batches", len(checkpoints))

	return checkpoints, chain, nil
}

func estimateBatchCount(chainLen int64, batchMaxPack int64) int {
	if batchMaxPack <= 0 || chainLen <= 0 {
		return 1
	}
	estimated := chainLen * estimatedBytesPerCommit
	n := int((estimated + batchMaxPack - 1) / batchMaxPack)
	if n < 1 {
		n = 1
	}
	return n
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
) (io.ReadCloser, error) {
	var header [12]byte
	n, err := io.ReadFull(r, header[:])
	if err != nil {
		// Short pack or error — let the push handle it
		prefixed := io.MultiReader(bytes.NewReader(header[:n]), r)
		return &wrappedMultiRC{Reader: prefixed, Closer: r}, nil
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
		return nil, nil
	}

	prefixed := io.MultiReader(bytes.NewReader(header[:]), r)
	return &wrappedMultiRC{Reader: prefixed, Closer: r}, nil
}

type wrappedMultiRC struct {
	io.Reader
	io.Closer
}

func (w *wrappedMultiRC) Read(p []byte) (int, error) { return w.Reader.Read(p) }

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
	if numBatches <= 1 || len(chain) <= 1 {
		return []plumbing.Hash{chain[len(chain)-1]}
	}
	checkpoints := make([]plumbing.Hash, 0, numBatches)
	batchSize := len(chain) / numBatches
	for i := 0; i < numBatches-1; i++ {
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
	batchLimit int64,
) (io.ReadCloser, error) {
	desired := singleGP(batch.Plan.SourceRef, batch.TempRef, checkpoint)
	haves := planner.SingleHaveMap(current)
	packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, desired, haves)
	if err != nil {
		return nil, err
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
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
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
	return err
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
