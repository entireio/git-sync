// Package bootstrap implements the bootstrap relay strategy for git-sync.
// This handles initial seeding of an empty target, both one-shot and batched.
package bootstrap

import (
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

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/soph/git-sync/internal/convert"
	"github.com/soph/git-sync/internal/gitproto"
	"github.com/soph/git-sync/internal/planner"
)

const (
	defaultAutoBatchMaxPackBytes = 512 * 1024 * 1024
	githubLargeRepoThresholdKB   = 1536 * 1024
)

var bodyLimitPattern = regexp.MustCompile(`body exceeded size limit ([0-9]+)`)

// GitHubRepoAPIBaseURL is the base for GitHub API calls (replaceable in tests).
var GitHubRepoAPIBaseURL = "https://api.github.com"

// Params holds the inputs for a bootstrap execution.
type Params struct {
	SourceConn    *gitproto.Conn
	TargetConn    *gitproto.Conn
	SourceService *gitproto.RefService
	TargetAdv     *packp.AdvRefs
	DesiredRefs   map[plumbing.ReferenceName]planner.DesiredRef
	TargetRefs    map[plumbing.ReferenceName]plumbing.Hash
	MaxPackBytes  int64
	BatchMaxPack  int64
	Verbose       bool
	Logger        *slog.Logger
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

// Execute runs the bootstrap strategy (one-shot or batched).
func Execute(ctx context.Context, p Params, relayReason string) (Result, error) {
	plans, err := planner.BuildBootstrapPlans(p.DesiredRefs, p.TargetRefs)
	if err != nil {
		return Result{}, err
	}

	result := Result{
		Plans: plans, Relay: true, RelayMode: "bootstrap", RelayReason: relayReason,
	}

	// GitHub large-repo preflight
	if batchLimit, ok := githubBatchLimit(ctx, p); ok {
		p.BatchMaxPack = batchLimit
		p.log("bootstrap github preflight selected batched mode",
			"batch_max_pack_bytes", p.BatchMaxPack)
	}

	if p.BatchMaxPack > 0 {
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

	p.log("bootstrap pushing refs to target", "ref_count", len(plans))
	cmds := gitproto.ToPushCommands(convert.PlansToPushPlans(plans))
	pushErr := gitproto.PushPack(ctx, p.TargetConn, p.TargetAdv, cmds, packReader, p.Verbose)
	if pushErr != nil {
		autoBatch, ok := autoBatchMaxPackBytes(p, pushErr)
		if !ok {
			return result, fmt.Errorf("push target refs: %w", pushErr)
		}
		p.log("bootstrap retrying with batched mode after target rejection",
			"batch_max_pack_bytes", autoBatch)
		p.BatchMaxPack = autoBatch
		return executeBatched(ctx, p, plans, result)
	}

	result.Pushed = len(plans)
	return result, nil
}

// --- Batched bootstrap ---

func executeBatched(
	ctx context.Context,
	p Params,
	plans []planner.BranchPlan,
	result Result,
) (Result, error) {
	if p.SourceService.Protocol != "v2" {
		return result, fmt.Errorf("bootstrap batching currently requires protocol v2")
	}
	if p.SourceService.V2Caps == nil || !p.SourceService.V2Caps.FetchSupports("filter") {
		return result, fmt.Errorf("bootstrap batching requires source fetch filter support")
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

	var batches []planner.BootstrapBatch
	if len(planRefs) > 0 {
		p.log("bootstrap batch planning checkpoints", "branch_ref_count", len(planRefs))
		var err error
		batches, err = planBatches(ctx, p, planRefs)
		if err != nil {
			return result, err
		}
	}

	batchLimit := p.BatchMaxPack
	if p.MaxPackBytes > 0 && (batchLimit == 0 || p.MaxPackBytes < batchLimit) {
		batchLimit = p.MaxPackBytes
	}

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
		if err != nil {
			return result, fmt.Errorf("resume bootstrap batch for %s: %w", batch.Plan.TargetRef, err)
		}

		for idx := startIdx; idx < len(batch.Checkpoints); idx++ {
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

			desired := singleGP(batch.Plan.SourceRef, batch.TempRef, checkpoint)
			haves := planner.SingleHaveMap(current)
			packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, desired, haves)
			if err != nil {
				return result, fmt.Errorf("fetch source batch pack for %s: %w", batch.Plan.TargetRef, err)
			}
			packReader = gitproto.LimitPackReader(packReader, batchLimit)
			cmds := gitproto.ToPushCommands(convert.PlansToPushPlans(stagePlans))
			if err := gitproto.PushPack(ctx, p.TargetConn, p.TargetAdv, cmds, packReader, p.Verbose); err != nil {
				return result, fmt.Errorf("push bootstrap batch for %s: %w", batch.Plan.TargetRef, err)
			}
			p.log("bootstrap batch checkpoint complete",
				"branch", batch.Plan.TargetRef.String(),
				"batch", idx+1,
				"batch_total", len(batch.Checkpoints))
			current = checkpoint
			result.BatchCount++
		}

		if current.IsZero() {
			return result, fmt.Errorf("bootstrap batching for %s completed with no checkpoint state", batch.Plan.TargetRef)
		}
		if batch.ResumeHash == batch.Plan.SourceHash {
			cmds := []gitproto.PushCommand{{Name: batch.Plan.TargetRef, Old: plumbing.ZeroHash, New: batch.Plan.SourceHash}}
			if err := gitproto.PushCommands(ctx, p.TargetConn, p.TargetAdv, cmds, p.Verbose); err != nil {
				return result, fmt.Errorf("resume bootstrap cutover for %s: %w", batch.Plan.TargetRef, err)
			}
		}

		cmds := []gitproto.PushCommand{{Name: batch.TempRef, Old: current, Delete: true}}
		if err := gitproto.PushCommands(ctx, p.TargetConn, p.TargetAdv, cmds, p.Verbose); err != nil {
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
				cmds := gitproto.ToPushCommands(convert.PlansToPushPlans(tagPlans))
				if err := gitproto.PushCommands(ctx, p.TargetConn, p.TargetAdv, cmds, p.Verbose); err != nil {
					return result, fmt.Errorf("create tag refs after bootstrap: %w", err)
				}
			} else {
				return result, fmt.Errorf("fetch bootstrap tag pack: %w", err)
			}
		} else {
			packReader = gitproto.LimitPackReader(packReader, p.MaxPackBytes)
			cmds := gitproto.ToPushCommands(convert.PlansToPushPlans(tagPlans))
			if err := gitproto.PushPack(ctx, p.TargetConn, p.TargetAdv, cmds, packReader, p.Verbose); err != nil {
				return result, fmt.Errorf("push bootstrap tags: %w", err)
			}
		}
	}

	result.Pushed = len(plans)
	result.Batching = true
	result.RelayMode = "bootstrap-batch"
	return result, nil
}

// --- Checkpoint planning ---

func planBatches(ctx context.Context, p Params, desired []planner.DesiredRef) ([]planner.BootstrapBatch, error) {
	out := make([]planner.BootstrapBatch, 0, len(desired))
	for _, ref := range desired {
		checkpoints, err := PlanCheckpoints(ctx, p, ref)
		if err != nil {
			return nil, err
		}
		out = append(out, planner.BootstrapBatch{
			Plan: planner.BranchPlan{
				Branch: ref.Label, SourceRef: ref.SourceRef,
				TargetRef: ref.TargetRef, SourceHash: ref.SourceHash,
				Kind: ref.Kind, Action: planner.ActionCreate,
			},
			TempRef:     planner.BootstrapTempRef(ref.TargetRef),
			ResumeHash:  p.TargetRefs[planner.BootstrapTempRef(ref.TargetRef)],
			Checkpoints: checkpoints,
		})
	}
	return out, nil
}

// PlanCheckpoints plans the checkpoint hashes for a single branch during batched bootstrap.
func PlanCheckpoints(ctx context.Context, p Params, ref planner.DesiredRef) ([]plumbing.Hash, error) {
	p.log("bootstrap batch fetching commit graph", "branch", ref.TargetRef.String())
	graphStore := memory.NewStorage()
	gpRef := gitproto.DesiredRef{SourceRef: ref.SourceRef, TargetRef: ref.TargetRef, SourceHash: ref.SourceHash}
	if err := p.SourceService.FetchCommitGraph(ctx, graphStore, p.SourceConn, gpRef); err != nil {
		return nil, fmt.Errorf("fetch bootstrap planning graph for %s: %w", ref.TargetRef, err)
	}
	chain, err := planner.FirstParentChain(graphStore, ref.SourceHash)
	if err != nil {
		return nil, fmt.Errorf("walk first-parent chain for %s: %w", ref.TargetRef, err)
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("empty first-parent chain for %s", ref.TargetRef)
	}

	// Issue #14: Use a commit-count heuristic for the initial span estimate
	// to reduce expensive fetch-and-discard probes. Typical compressed commit
	// + tree overhead averages ~2-5 KiB per commit in a mature repo.
	const avgBytesPerCommit = 4096
	initialSpan := 0
	if p.BatchMaxPack > 0 {
		initialSpan = int(p.BatchMaxPack / avgBytesPerCommit)
		if initialSpan > len(chain) {
			initialSpan = len(chain)
		}
		if initialSpan < 1 {
			initialSpan = 1
		}
	}

	checkpoints := make([]plumbing.Hash, 0, len(chain))
	prevIdx := -1
	prevHash := plumbing.ZeroHash
	prevSpan := initialSpan
	probeCache := make(map[string]bool)
	for prevIdx < len(chain)-1 {
		bestIdx, err := planner.SampledCheckpointUnderLimit(chain, prevIdx, prevSpan, func(idx int) (bool, error) {
			cacheKey := prevHash.String() + ":" + strconv.Itoa(idx)
			if tooLarge, ok := probeCache[cacheKey]; ok {
				return tooLarge, nil
			}
			tooLarge, err := packExceedsLimit(ctx, p, ref, chain[idx], prevHash, p.BatchMaxPack)
			if err != nil {
				return false, fmt.Errorf("measure bootstrap batch for %s at %s: %w", ref.TargetRef, planner.ShortHash(chain[idx]), err)
			}
			probeCache[cacheKey] = tooLarge
			return tooLarge, nil
		})
		if err != nil {
			return nil, err
		}
		if bestIdx <= prevIdx {
			return nil, fmt.Errorf("could not find bootstrap checkpoint for %s under batch-max-pack-bytes=%d", ref.TargetRef, p.BatchMaxPack)
		}
		prevSpan = bestIdx - prevIdx
		prevIdx = bestIdx
		prevHash = chain[bestIdx]
		checkpoints = append(checkpoints, prevHash)
		p.log("bootstrap batch planned checkpoint",
			"branch", ref.TargetRef.String(),
			"checkpoint", planner.ShortHash(prevHash),
			"selected", len(checkpoints),
			"chain_len", len(chain))
	}
	return checkpoints, nil
}

func packExceedsLimit(ctx context.Context, p Params, ref planner.DesiredRef, want, have plumbing.Hash, limit int64) (bool, error) {
	desired := singleGP(ref.SourceRef, ref.TargetRef, want)
	haves := planner.SingleHaveMap(have)
	packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, desired, haves)
	if err != nil {
		return false, err
	}
	defer packReader.Close()
	_, err = io.Copy(io.Discard, gitproto.LimitPackReader(packReader, limit))
	if err == nil {
		return false, nil
	}
	if strings.Contains(err.Error(), "source pack exceeded max-pack-bytes limit") {
		return true, nil
	}
	return false, err
}

// --- GitHub preflight ---

func githubBatchLimit(ctx context.Context, p Params) (int64, bool) {
	if p.BatchMaxPack > 0 || p.SourceConn == nil || p.SourceConn.Endpoint == nil {
		return 0, false
	}
	if p.SourceService == nil || p.SourceService.Protocol != "v2" {
		return 0, false
	}
	if p.SourceService.V2Caps == nil || !p.SourceService.V2Caps.FetchSupports("filter") {
		return 0, false
	}
	repoSizeKB, ok := lookupGitHubRepoSizeKB(ctx, p.SourceConn)
	if !ok || repoSizeKB < githubLargeRepoThresholdKB {
		return 0, false
	}
	limit := int64(defaultAutoBatchMaxPackBytes)
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
	var payload struct{ Size int64 `json:"size"` }
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
	if ep.Protocol != "http" && ep.Protocol != "https" {
		return "", "", false
	}
	if !strings.EqualFold(ep.Host, "github.com") {
		return "", "", false
	}
	path := strings.TrimSuffix(strings.Trim(ep.Path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func autoBatchMaxPackBytes(p Params, err error) (int64, bool) {
	if p.BatchMaxPack > 0 || !isTargetBodyLimitError(err) {
		return 0, false
	}
	if p.SourceService == nil || p.SourceService.Protocol != "v2" {
		return 0, false
	}
	if p.SourceService.V2Caps == nil || !p.SourceService.V2Caps.FetchSupports("filter") {
		return 0, false
	}
	limit := int64(defaultAutoBatchMaxPackBytes)
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
