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
	defaultAutoBatchMaxPackBytes = 512 * 1024 * 1024
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
	BatchMaxPack int64
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
	prefetchedPacks map[plumbing.Hash][]byte
}

// Execute runs the bootstrap strategy (one-shot or batched).
func Execute(ctx context.Context, p Params, relayReason string) (Result, error) {
	if p.TargetPusher == nil {
		return Result{Relay: true, RelayMode: "bootstrap", RelayReason: relayReason}, fmt.Errorf("bootstrap strategy requires TargetPusher")
	}

	// GitHub large-repo preflight
	if batchLimit, ok := githubBatchLimit(ctx, p); ok {
		p.BatchMaxPack = batchLimit
		p.log("bootstrap github preflight selected batched mode",
			"batch_max_pack_bytes", p.BatchMaxPack)
	}

	planTargetRefs := p.TargetRefs
	if p.BatchMaxPack > 0 {
		planTargetRefs = adjustedBootstrapTargetRefs(p.DesiredRefs, p.TargetRefs)
	}
	plans, err := planner.BuildBootstrapPlans(p.DesiredRefs, planTargetRefs)
	if err != nil {
		return Result{}, err
	}

	result := Result{
		Plans: plans, Relay: true, RelayMode: "bootstrap", RelayReason: relayReason,
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
	packReader = closeOnce(packReader)

	p.log("bootstrap pushing refs to target", "ref_count", len(plans))
	cmds := convert.PlansToPushCommands(plans)
	pushErr := p.TargetPusher.PushPack(ctx, cmds, packReader)
	_ = packReader.Close()
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

			packReader, err := packReaderForCheckpoint(ctx, p, batch, checkpoint, current, batchLimit)
			if err != nil {
				return result, fmt.Errorf("fetch source batch pack for %s: %w", batch.Plan.TargetRef, err)
			}
			packReader = closeOnce(packReader)
			cmds := convert.PlansToPushCommands(stagePlans)
			if err := p.TargetPusher.PushPack(ctx, cmds, packReader); err != nil {
				_ = packReader.Close()
				return result, fmt.Errorf("push bootstrap batch for %s: %w", batch.Plan.TargetRef, err)
			}
			_ = packReader.Close()
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
		checkpoints, prefetched, err := planCheckpointsWithCache(ctx, p, ref)
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
			prefetchedPacks: prefetched,
		})
	}
	return out, nil
}

// PlanCheckpoints plans the checkpoint hashes for a single branch during batched bootstrap.
func PlanCheckpoints(ctx context.Context, p Params, ref planner.DesiredRef) ([]plumbing.Hash, error) {
	checkpoints, _, err := planCheckpointsWithCache(ctx, p, ref)
	return checkpoints, err
}

func planCheckpointsWithCache(ctx context.Context, p Params, ref planner.DesiredRef) ([]plumbing.Hash, map[plumbing.Hash][]byte, error) {
	cp, err := newCheckpointPlanner(ctx, p, ref)
	if err != nil {
		return nil, nil, err
	}
	return cp.plan()
}

type probeResult struct {
	tooLarge bool
	data     []byte
}

type checkpointPlanner struct {
	ctx        context.Context
	params     Params
	ref        planner.DesiredRef
	chain      []plumbing.Hash
	probeCache map[string]probeResult
	prefetched map[plumbing.Hash][]byte
}

func newCheckpointPlanner(ctx context.Context, p Params, ref planner.DesiredRef) (*checkpointPlanner, error) {
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
	return &checkpointPlanner{
		ctx:        ctx,
		params:     p,
		ref:        ref,
		chain:      chain,
		probeCache: make(map[string]probeResult),
		prefetched: make(map[plumbing.Hash][]byte),
	}, nil
}

func (p *checkpointPlanner) plan() ([]plumbing.Hash, map[plumbing.Hash][]byte, error) {
	checkpoints := make([]plumbing.Hash, 0, len(p.chain))
	prevIdx := -1
	prevHash := plumbing.ZeroHash
	prevSpan := initialCheckpointSpan(p.params.BatchMaxPack, len(p.chain))
	prevMeasuredBytes := 0
	for prevIdx < len(p.chain)-1 {
		bestIdx, err := p.selectNextCheckpoint(prevIdx, prevHash, prevSpan, prevMeasuredBytes)
		if err != nil {
			return nil, nil, err
		}
		if bestIdx <= prevIdx {
			return nil, nil, fmt.Errorf("could not find bootstrap checkpoint for %s under batch-max-pack-bytes=%d", p.ref.TargetRef, p.params.BatchMaxPack)
		}
		if result, ok := p.cachedProbe(prevHash, bestIdx); ok && !result.tooLarge && len(result.data) > 0 {
			p.prefetched[p.chain[bestIdx]] = result.data
			prevSpan = adaptiveNextProbeSpan(p.params.BatchMaxPack, len(result.data), bestIdx-prevIdx, len(p.chain)-1-bestIdx)
			prevMeasuredBytes = len(result.data)
		} else {
			prevSpan = bestIdx - prevIdx
			prevMeasuredBytes = 0
		}
		prevIdx = bestIdx
		prevHash = p.chain[bestIdx]
		checkpoints = append(checkpoints, prevHash)
		p.params.log("bootstrap batch planned checkpoint",
			"branch", p.ref.TargetRef.String(),
			"checkpoint", planner.ShortHash(prevHash),
			"selected", len(checkpoints),
			"chain_len", len(p.chain))
	}
	return checkpoints, p.prefetched, nil
}

func (p *checkpointPlanner) selectNextCheckpoint(prevIdx int, prevHash plumbing.Hash, prevSpan int, prevMeasuredBytes int) (int, error) {
	bestIdx := -1
	remaining := len(p.chain) - 1 - prevIdx
	if shouldSelectTipWithoutProbe(p.params.BatchMaxPack, prevMeasuredBytes, prevSpan, remaining) {
		bestIdx = len(p.chain) - 1
	} else if shouldProbeTipFirst(p.params.BatchMaxPack, prevMeasuredBytes, prevSpan, remaining) {
		tipIdx := len(p.chain) - 1
		tooLarge, err := p.probe(prevHash, tipIdx)
		if err != nil {
			return -1, err
		}
		if !tooLarge {
			bestIdx = tipIdx
		}
	}
	if bestIdx != -1 {
		return bestIdx, nil
	}
	return planner.SampledCheckpointUnderLimit(p.chain, prevIdx, prevSpan, func(idx int) (bool, error) {
		return p.probe(prevHash, idx)
	})
}

func (p *checkpointPlanner) probe(prevHash plumbing.Hash, idx int) (bool, error) {
	if result, ok := p.cachedProbe(prevHash, idx); ok {
		return result.tooLarge, nil
	}
	data, tooLarge, err := fetchPackForProbe(p.ctx, p.params, p.ref, p.chain[idx], prevHash, p.params.BatchMaxPack)
	if err != nil {
		return false, fmt.Errorf("measure bootstrap batch for %s at %s: %w", p.ref.TargetRef, planner.ShortHash(p.chain[idx]), err)
	}
	p.probeCache[probeKey(prevHash, idx)] = probeResult{tooLarge: tooLarge, data: data}
	return tooLarge, nil
}

func (p *checkpointPlanner) cachedProbe(prevHash plumbing.Hash, idx int) (probeResult, bool) {
	result, ok := p.probeCache[probeKey(prevHash, idx)]
	return result, ok
}

func probeKey(prevHash plumbing.Hash, idx int) string {
	return prevHash.String() + ":" + strconv.Itoa(idx)
}

func initialCheckpointSpan(batchMaxPack int64, chainLen int) int {
	// Issue #14: Use a commit-count heuristic for the initial span estimate
	// to reduce expensive fetch-and-discard probes. Typical compressed commit
	// + tree overhead averages ~2-5 KiB per commit in a mature repo.
	const avgBytesPerCommit = 4096
	if batchMaxPack <= 0 {
		return 0
	}
	initialSpan := int(batchMaxPack / avgBytesPerCommit)
	if initialSpan > chainLen {
		initialSpan = chainLen
	}
	if initialSpan < 1 {
		initialSpan = 1
	}
	return initialSpan
}

func adaptiveNextProbeSpan(limit int64, measuredBytes int, selectedSpan int, remaining int) int {
	if selectedSpan < 1 {
		selectedSpan = 1
	}
	if remaining < 1 {
		return selectedSpan
	}
	if limit <= 0 || measuredBytes <= 0 {
		if selectedSpan > remaining {
			return remaining
		}
		return selectedSpan
	}

	next := int((int64(selectedSpan) * limit) / int64(measuredBytes))
	if next < 1 {
		next = 1
	}
	if next > remaining {
		next = remaining
	}
	return next
}

func shouldProbeTipFirst(limit int64, measuredBytes int, measuredSpan int, remaining int) bool {
	if limit <= 0 || measuredBytes <= 0 || measuredSpan <= 0 || remaining <= measuredSpan {
		return false
	}
	estimated := (int64(measuredBytes) * int64(remaining)) / int64(measuredSpan)
	return estimated <= (limit*9)/10
}

func shouldSelectTipWithoutProbe(limit int64, measuredBytes int, measuredSpan int, remaining int) bool {
	if limit <= 0 || measuredBytes <= 0 || measuredSpan <= 0 || remaining <= 0 {
		return false
	}
	if remaining <= measuredSpan {
		return int64(measuredBytes) <= limit/2
	}
	estimated := (int64(measuredBytes) * int64(remaining)) / int64(measuredSpan)
	return estimated <= limit/2
}

func fetchPackForProbe(ctx context.Context, p Params, ref planner.DesiredRef, want, have plumbing.Hash, limit int64) ([]byte, bool, error) {
	desired := singleGP(ref.SourceRef, ref.TargetRef, want)
	haves := planner.SingleHaveMap(have)
	packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, desired, haves)
	if err != nil {
		return nil, false, err
	}
	defer packReader.Close()
	data, err := io.ReadAll(gitproto.LimitPackReader(packReader, limit))
	if err == nil {
		return data, false, nil
	}
	if strings.Contains(err.Error(), "source pack exceeded max-pack-bytes limit") {
		return nil, true, nil
	}
	return nil, false, err
}

func packReaderForCheckpoint(
	ctx context.Context,
	p Params,
	batch plannedBatch,
	checkpoint plumbing.Hash,
	current plumbing.Hash,
	batchLimit int64,
) (io.ReadCloser, error) {
	if data, ok := batch.prefetchedPacks[checkpoint]; ok && len(data) > 0 {
		p.log("bootstrap batch reusing prefetched probe pack",
			"branch", batch.Plan.TargetRef.String(),
			"checkpoint", planner.ShortHash(checkpoint),
			"bytes", len(data))
		return io.NopCloser(bytes.NewReader(data)), nil
	}

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
	if p.BatchMaxPack > 0 || p.SourceConn == nil || p.SourceConn.Endpoint == nil {
		return 0, false
	}
	if p.SourceService == nil || !p.SourceService.SupportsBootstrapBatch() {
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

func autoBatchMaxPackBytes(p Params, err error) (int64, bool) {
	if p.BatchMaxPack > 0 || !isTargetBodyLimitError(err) {
		return 0, false
	}
	if p.SourceService == nil || !p.SourceService.SupportsBootstrapBatch() {
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
