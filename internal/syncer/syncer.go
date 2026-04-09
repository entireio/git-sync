package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	transporthttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/go-git/go-git/v5/utils/ioutil"
)

const (
	sourceRemoteName = "source"
	protocolModeAuto = "auto"
	protocolModeV1   = "v1"
	protocolModeV2   = "v2"
)

type Endpoint struct {
	URL         string
	Username    string
	Token       string
	BearerToken string
}

type RefMapping struct {
	Source string
	Target string
}

type Config struct {
	Source       Endpoint
	Target       Endpoint
	Branches     []string
	Mappings     []RefMapping
	IncludeTags  bool
	DryRun       bool
	Verbose      bool
	ShowStats    bool
	Force        bool
	Prune        bool
	MaxPackBytes int64
	ProtocolMode string
}

type RefKind string

const (
	RefKindBranch RefKind = "branch"
	RefKindTag    RefKind = "tag"
)

type BranchPlan struct {
	Branch     string                 `json:"branch"`
	SourceRef  plumbing.ReferenceName `json:"source_ref"`
	TargetRef  plumbing.ReferenceName `json:"target_ref"`
	SourceHash plumbing.Hash          `json:"source_hash"`
	TargetHash plumbing.Hash          `json:"target_hash"`
	Kind       RefKind                `json:"kind"`
	Action     Action                 `json:"action"`
	Reason     string                 `json:"reason"`
}

type Action string

const (
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionDelete Action = "delete"
	ActionSkip   Action = "skip"
	ActionBlock  Action = "block"
)

type Result struct {
	Plans    []BranchPlan `json:"plans"`
	Pushed   int          `json:"pushed"`
	Skipped  int          `json:"skipped"`
	Blocked  int          `json:"blocked"`
	Deleted  int          `json:"deleted"`
	DryRun   bool         `json:"dry_run"`
	Stats    Stats        `json:"stats"`
	Protocol string       `json:"protocol"`
}

type ProbeResult struct {
	SourceURL     string    `json:"source_url"`
	TargetURL     string    `json:"target_url,omitempty"`
	RequestedMode string    `json:"requested_mode"`
	Protocol      string    `json:"protocol"`
	RefPrefixes   []string  `json:"ref_prefixes"`
	Capabilities  []string  `json:"source_capabilities"`
	TargetCaps    []string  `json:"target_capabilities,omitempty"`
	Refs          []RefInfo `json:"refs"`
	Stats         Stats     `json:"stats"`
}

type RefInfo struct {
	Name string        `json:"name"`
	Hash plumbing.Hash `json:"hash"`
}

type FetchResult struct {
	SourceURL      string          `json:"source_url"`
	RequestedMode  string          `json:"requested_mode"`
	Protocol       string          `json:"protocol"`
	Wants          []RefInfo       `json:"wants"`
	Haves          []plumbing.Hash `json:"haves"`
	FetchedObjects int             `json:"fetched_objects"`
	Stats          Stats           `json:"stats"`
}

type Stats struct {
	Enabled bool                     `json:"enabled"`
	Items   map[string]*ServiceStats `json:"items"`
}

type ServiceStats struct {
	Name          string `json:"name"`
	Requests      int    `json:"requests"`
	RequestBytes  int64  `json:"request_bytes"`
	ResponseBytes int64  `json:"response_bytes"`
	Wants         int    `json:"wants"`
	Haves         int    `json:"haves"`
	Commands      int    `json:"commands"`
}

func (p BranchPlan) MarshalJSON() ([]byte, error) {
	type branchPlanJSON struct {
		Branch     string  `json:"branch"`
		SourceRef  string  `json:"source_ref"`
		TargetRef  string  `json:"target_ref"`
		SourceHash string  `json:"source_hash"`
		TargetHash string  `json:"target_hash"`
		Kind       RefKind `json:"kind"`
		Action     Action  `json:"action"`
		Reason     string  `json:"reason"`
	}
	return json.Marshal(branchPlanJSON{
		Branch:     p.Branch,
		SourceRef:  p.SourceRef.String(),
		TargetRef:  p.TargetRef.String(),
		SourceHash: p.SourceHash.String(),
		TargetHash: p.TargetHash.String(),
		Kind:       p.Kind,
		Action:     p.Action,
		Reason:     p.Reason,
	})
}

func (r RefInfo) MarshalJSON() ([]byte, error) {
	type refInfoJSON struct {
		Name string `json:"name"`
		Hash string `json:"hash"`
	}
	return json.Marshal(refInfoJSON{
		Name: r.Name,
		Hash: r.Hash.String(),
	})
}

func (r FetchResult) MarshalJSON() ([]byte, error) {
	type fetchResultJSON struct {
		SourceURL      string    `json:"source_url"`
		RequestedMode  string    `json:"requested_mode"`
		Protocol       string    `json:"protocol"`
		Wants          []RefInfo `json:"wants"`
		Haves          []string  `json:"haves"`
		FetchedObjects int       `json:"fetched_objects"`
		Stats          Stats     `json:"stats"`
	}
	haves := make([]string, 0, len(r.Haves))
	for _, hash := range r.Haves {
		haves = append(haves, hash.String())
	}
	return json.Marshal(fetchResultJSON{
		SourceURL:      r.SourceURL,
		RequestedMode:  r.RequestedMode,
		Protocol:       r.Protocol,
		Wants:          r.Wants,
		Haves:          haves,
		FetchedObjects: r.FetchedObjects,
		Stats:          r.Stats,
	})
}

func (r Result) Lines() []string {
	lines := make([]string, 0, len(r.Plans)+8)
	for _, plan := range r.Plans {
		label := plan.Branch
		if plan.TargetRef != "" {
			label = plan.TargetRef.String()
		}
		line := fmt.Sprintf("%s %s", strings.ToUpper(string(plan.Action)), label)
		if plan.Reason != "" {
			line += " - " + plan.Reason
		}
		lines = append(lines, line)
	}

	summary := fmt.Sprintf(
		"summary: pushed=%d deleted=%d skipped=%d blocked=%d protocol=%s",
		r.Pushed, r.Deleted, r.Skipped, r.Blocked, r.Protocol,
	)
	if r.DryRun {
		summary += " dry-run=true"
	}
	lines = append(lines, summary)

	if r.Stats.Enabled {
		keys := make([]string, 0, len(r.Stats.Items))
		for key := range r.Stats.Items {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			item := r.Stats.Items[key]
			lines = append(lines, fmt.Sprintf(
				"stats: %s requests=%d request-bytes=%d response-bytes=%d wants=%d haves=%d commands=%d",
				item.Name, item.Requests, item.RequestBytes, item.ResponseBytes, item.Wants, item.Haves, item.Commands,
			))
		}
	}

	return lines
}

func (r ProbeResult) Lines() []string {
	lines := []string{
		fmt.Sprintf("source: %s", r.SourceURL),
		fmt.Sprintf("requested-protocol: %s", r.RequestedMode),
		fmt.Sprintf("negotiated-protocol: %s", r.Protocol),
	}

	if len(r.RefPrefixes) > 0 {
		lines = append(lines, "ref-prefixes: "+strings.Join(r.RefPrefixes, ", "))
	}
	if len(r.Capabilities) > 0 {
		lines = append(lines, "source-capabilities: "+strings.Join(r.Capabilities, ", "))
	}
	if r.TargetURL != "" {
		lines = append(lines, "target: "+r.TargetURL)
	}
	if len(r.TargetCaps) > 0 {
		lines = append(lines, "target-capabilities: "+strings.Join(r.TargetCaps, ", "))
	}

	lines = append(lines, fmt.Sprintf("refs: %d", len(r.Refs)))
	for _, ref := range r.Refs {
		lines = append(lines, fmt.Sprintf("ref: %s %s", ref.Hash.String(), ref.Name))
	}

	if r.Stats.Enabled {
		keys := make([]string, 0, len(r.Stats.Items))
		for key := range r.Stats.Items {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			item := r.Stats.Items[key]
			lines = append(lines, fmt.Sprintf(
				"stats: %s requests=%d request-bytes=%d response-bytes=%d wants=%d haves=%d commands=%d",
				item.Name, item.Requests, item.RequestBytes, item.ResponseBytes, item.Wants, item.Haves, item.Commands,
			))
		}
	}

	return lines
}

func (r FetchResult) Lines() []string {
	lines := []string{
		fmt.Sprintf("source: %s", r.SourceURL),
		fmt.Sprintf("requested-protocol: %s", r.RequestedMode),
		fmt.Sprintf("negotiated-protocol: %s", r.Protocol),
		fmt.Sprintf("wants: %d", len(r.Wants)),
		fmt.Sprintf("haves: %d", len(r.Haves)),
		fmt.Sprintf("fetched-objects: %d", r.FetchedObjects),
	}
	for _, want := range r.Wants {
		lines = append(lines, fmt.Sprintf("want: %s %s", want.Hash.String(), want.Name))
	}
	for _, have := range r.Haves {
		lines = append(lines, fmt.Sprintf("have: %s", have.String()))
	}
	if r.Stats.Enabled {
		keys := make([]string, 0, len(r.Stats.Items))
		for key := range r.Stats.Items {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			item := r.Stats.Items[key]
			lines = append(lines, fmt.Sprintf(
				"stats: %s requests=%d request-bytes=%d response-bytes=%d wants=%d haves=%d commands=%d",
				item.Name, item.Requests, item.RequestBytes, item.ResponseBytes, item.Wants, item.Haves, item.Commands,
			))
		}
	}
	return lines
}

func Run(ctx context.Context, cfg Config) (Result, error) {
	if cfg.ProtocolMode == "" {
		cfg.ProtocolMode = protocolModeAuto
	}
	if cfg.ProtocolMode != protocolModeAuto && cfg.ProtocolMode != protocolModeV1 && cfg.ProtocolMode != protocolModeV2 {
		return Result{}, fmt.Errorf("unsupported protocol mode %q", cfg.ProtocolMode)
	}

	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		return Result{}, fmt.Errorf("init in-memory repository: %w", err)
	}

	stats := newStats(cfg.ShowStats)
	sourceConn, err := newTransportConn(cfg.Source, "source", stats)
	if err != nil {
		return Result{}, fmt.Errorf("create source transport: %w", err)
	}
	targetConn, err := newTransportConn(cfg.Target, "target", stats)
	if err != nil {
		return Result{}, fmt.Errorf("create target transport: %w", err)
	}

	sourceRefs, sourceService, err := listSourceRefs(ctx, sourceConn, cfg)
	if err != nil {
		return Result{}, fmt.Errorf("list source refs: %w", err)
	}
	targetAdv, err := advertisedRefsV1(ctx, targetConn, transport.ReceivePackServiceName)
	if err != nil {
		return Result{}, fmt.Errorf("list target refs: %w", err)
	}
	targetRefs, err := advertisedReferences(targetAdv)
	if err != nil {
		return Result{}, fmt.Errorf("decode target refs: %w", err)
	}

	sourceRefMap := refHashMap(sourceRefs)
	targetRefMap := refHashMap(targetRefs)

	desiredRefs, managedTargets, err := buildDesiredRefs(sourceRefMap, cfg)
	if err != nil {
		return Result{}, err
	}
	if len(desiredRefs) == 0 {
		return Result{}, fmt.Errorf("no source refs matched")
	}

	if err := sourceService.Fetch(ctx, repo, sourceConn, desiredRefs, targetRefMap); err != nil {
		if !errors.Is(err, git.NoErrAlreadyUpToDate) {
			return Result{}, err
		}
	}

	plans, err := buildPlans(repo, desiredRefs, targetRefMap, managedTargets, cfg)
	if err != nil {
		return Result{}, err
	}

	result := Result{
		Plans:    plans,
		DryRun:   cfg.DryRun,
		Stats:    stats.snapshot(),
		Protocol: sourceService.protocol,
	}

	pushPlans := make([]BranchPlan, 0, len(plans))
	for _, plan := range plans {
		switch plan.Action {
		case ActionCreate, ActionUpdate:
			if cfg.DryRun {
				result.Skipped++
				continue
			}
			pushPlans = append(pushPlans, plan)
		case ActionDelete:
			if cfg.DryRun {
				result.Skipped++
				continue
			}
			pushPlans = append(pushPlans, plan)
		case ActionSkip:
			result.Skipped++
		case ActionBlock:
			result.Blocked++
		}
	}

	if !cfg.DryRun && result.Blocked > 0 {
		return result, fmt.Errorf("blocked %d ref update(s); rerun with --force where appropriate", result.Blocked)
	}

	if !cfg.DryRun && len(pushPlans) > 0 {
		if err := pushToTarget(ctx, repo, targetConn, targetAdv, pushPlans, targetRefMap, cfg.Verbose); err != nil {
			return result, fmt.Errorf("push target refs: %w", err)
		}
	}

	for _, plan := range pushPlans {
		switch plan.Action {
		case ActionCreate, ActionUpdate:
			result.Pushed++
		case ActionDelete:
			result.Deleted++
		}
	}

	result.Stats = stats.snapshot()
	return result, nil
}

func Bootstrap(ctx context.Context, cfg Config) (Result, error) {
	if cfg.ProtocolMode == "" {
		cfg.ProtocolMode = protocolModeAuto
	}
	if cfg.ProtocolMode != protocolModeAuto && cfg.ProtocolMode != protocolModeV1 && cfg.ProtocolMode != protocolModeV2 {
		return Result{}, fmt.Errorf("unsupported protocol mode %q", cfg.ProtocolMode)
	}
	if cfg.Force {
		return Result{}, fmt.Errorf("bootstrap does not support --force")
	}
	if cfg.Prune {
		return Result{}, fmt.Errorf("bootstrap does not support --prune")
	}
	if cfg.DryRun {
		return Result{}, fmt.Errorf("bootstrap does not support dry-run; use plan or sync")
	}

	stats := newStats(cfg.ShowStats)
	sourceConn, err := newTransportConn(cfg.Source, "source", stats)
	if err != nil {
		return Result{}, fmt.Errorf("create source transport: %w", err)
	}
	targetConn, err := newTransportConn(cfg.Target, "target", stats)
	if err != nil {
		return Result{}, fmt.Errorf("create target transport: %w", err)
	}

	sourceRefs, sourceService, err := listSourceRefs(ctx, sourceConn, cfg)
	if err != nil {
		return Result{}, fmt.Errorf("list source refs: %w", err)
	}
	targetAdv, err := advertisedRefsV1(ctx, targetConn, transport.ReceivePackServiceName)
	if err != nil {
		return Result{}, fmt.Errorf("list target refs: %w", err)
	}
	targetRefs, err := advertisedReferences(targetAdv)
	if err != nil {
		return Result{}, fmt.Errorf("decode target refs: %w", err)
	}

	sourceRefMap := refHashMap(sourceRefs)
	targetRefMap := refHashMap(targetRefs)

	desiredRefs, _, err := buildDesiredRefs(sourceRefMap, cfg)
	if err != nil {
		return Result{}, err
	}
	if len(desiredRefs) == 0 {
		return Result{}, fmt.Errorf("no source refs matched")
	}

	plans, err := buildBootstrapPlans(desiredRefs, targetRefMap)
	if err != nil {
		return Result{}, err
	}

	result := Result{
		Plans:    plans,
		Stats:    stats.snapshot(),
		Protocol: sourceService.protocol,
	}

	packReader, err := sourceService.FetchPack(ctx, sourceConn, desiredRefs, nil)
	if err != nil {
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			return result, nil
		}
		return result, fmt.Errorf("fetch source pack: %w", err)
	}
	defer packReader.Close()
	packReader = limitPackReadCloser(packReader, cfg.MaxPackBytes)

	if err := pushPackToTarget(ctx, targetConn, targetAdv, plans, packReader, cfg.Verbose); err != nil {
		return result, fmt.Errorf("push target refs: %w", err)
	}

	result.Pushed = len(plans)
	result.Stats = stats.snapshot()
	return result, nil
}

func Probe(ctx context.Context, cfg Config) (ProbeResult, error) {
	if cfg.ProtocolMode == "" {
		cfg.ProtocolMode = protocolModeAuto
	}
	if cfg.ProtocolMode != protocolModeAuto && cfg.ProtocolMode != protocolModeV1 && cfg.ProtocolMode != protocolModeV2 {
		return ProbeResult{}, fmt.Errorf("unsupported protocol mode %q", cfg.ProtocolMode)
	}
	if cfg.Source.URL == "" {
		return ProbeResult{}, fmt.Errorf("source repository URL is required")
	}

	stats := newStats(cfg.ShowStats)
	sourceConn, err := newTransportConn(cfg.Source, "source", stats)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("create source transport: %w", err)
	}

	refs, service, err := listSourceRefs(ctx, sourceConn, cfg)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("list source refs: %w", err)
	}

	refInfos := make([]RefInfo, 0, len(refs))
	for _, ref := range refs {
		if ref.Type() != plumbing.HashReference {
			continue
		}
		refInfos = append(refInfos, RefInfo{
			Name: ref.Name().String(),
			Hash: ref.Hash(),
		})
	}
	sort.Slice(refInfos, func(i, j int) bool {
		return refInfos[i].Name < refInfos[j].Name
	})

	result := ProbeResult{
		SourceURL:     cfg.Source.URL,
		RequestedMode: cfg.ProtocolMode,
		Protocol:      service.protocol,
		RefPrefixes:   sourceRefPrefixes(cfg),
		Capabilities:  sourceCapabilities(service),
		Refs:          refInfos,
		Stats:         stats.snapshot(),
	}

	if cfg.Target.URL != "" {
		targetConn, err := newTransportConn(cfg.Target, "target", stats)
		if err != nil {
			return ProbeResult{}, fmt.Errorf("create target transport: %w", err)
		}
		targetAdv, err := advertisedRefsV1(ctx, targetConn, transport.ReceivePackServiceName)
		if err != nil {
			return ProbeResult{}, fmt.Errorf("list target refs: %w", err)
		}
		result.TargetURL = cfg.Target.URL
		result.TargetCaps = advCapabilities(targetAdv)
		result.Stats = stats.snapshot()
	}

	return result, nil
}

func Fetch(ctx context.Context, cfg Config, haveRefs []string, haveHashes []plumbing.Hash) (FetchResult, error) {
	if cfg.ProtocolMode == "" {
		cfg.ProtocolMode = protocolModeAuto
	}
	if cfg.ProtocolMode != protocolModeAuto && cfg.ProtocolMode != protocolModeV1 && cfg.ProtocolMode != protocolModeV2 {
		return FetchResult{}, fmt.Errorf("unsupported protocol mode %q", cfg.ProtocolMode)
	}
	if cfg.Source.URL == "" {
		return FetchResult{}, fmt.Errorf("source repository URL is required")
	}

	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		return FetchResult{}, fmt.Errorf("init in-memory repository: %w", err)
	}

	stats := newStats(cfg.ShowStats)
	sourceConn, err := newTransportConn(cfg.Source, "source", stats)
	if err != nil {
		return FetchResult{}, fmt.Errorf("create source transport: %w", err)
	}

	sourceRefs, sourceService, err := listSourceRefs(ctx, sourceConn, cfg)
	if err != nil {
		return FetchResult{}, fmt.Errorf("list source refs: %w", err)
	}
	sourceRefMap := refHashMap(sourceRefs)

	desiredRefs, _, err := buildDesiredRefs(sourceRefMap, cfg)
	if err != nil {
		return FetchResult{}, err
	}
	if len(desiredRefs) == 0 {
		return FetchResult{}, fmt.Errorf("no source refs matched")
	}

	targetRefMap := make(map[plumbing.ReferenceName]plumbing.Hash)
	for _, raw := range haveRefs {
		name := parseHaveRef(raw)
		hash, ok := sourceRefMap[name]
		if !ok {
			return FetchResult{}, fmt.Errorf("have-ref %q not found on source", raw)
		}
		targetRefMap[name] = hash
	}
	for idx, hash := range haveHashes {
		targetRefMap[plumbing.ReferenceName(fmt.Sprintf("refs/haves/%d", idx))] = hash
	}

	if err := sourceService.Fetch(ctx, repo, sourceConn, desiredRefs, targetRefMap); err != nil {
		if !errors.Is(err, git.NoErrAlreadyUpToDate) {
			return FetchResult{}, err
		}
	}

	wants := make([]RefInfo, 0, len(desiredRefs))
	for _, ref := range desiredRefs {
		wants = append(wants, RefInfo{Name: ref.SourceRef.String(), Hash: ref.SourceHash})
	}
	sort.Slice(wants, func(i, j int) bool { return wants[i].Name < wants[j].Name })

	objectCount, err := countObjects(repo.Storer)
	if err != nil {
		return FetchResult{}, fmt.Errorf("count fetched objects: %w", err)
	}

	return FetchResult{
		SourceURL:      cfg.Source.URL,
		RequestedMode:  cfg.ProtocolMode,
		Protocol:       sourceService.protocol,
		Wants:          wants,
		Haves:          sortedUniqueHashes(mapsRefValues(targetRefMap)),
		FetchedObjects: objectCount,
		Stats:          stats.snapshot(),
	}, nil
}

func buildBootstrapPlans(
	desired map[plumbing.ReferenceName]desiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) ([]BranchPlan, error) {
	targetNames := make([]plumbing.ReferenceName, 0, len(desired))
	for _, want := range desired {
		targetNames = append(targetNames, want.TargetRef)
	}
	sort.Slice(targetNames, func(i, j int) bool { return targetNames[i] < targetNames[j] })

	plans := make([]BranchPlan, 0, len(targetNames))
	for _, targetRef := range targetNames {
		targetHash := targetRefs[targetRef]
		if !targetHash.IsZero() {
			return nil, fmt.Errorf("target ref %s already exists; use sync for non-bootstrap runs", targetRef)
		}
		want := desired[targetRef]
		plans = append(plans, BranchPlan{
			Branch:     want.Label,
			SourceRef:  want.SourceRef,
			TargetRef:  want.TargetRef,
			SourceHash: want.SourceHash,
			TargetHash: plumbing.ZeroHash,
			Kind:       want.Kind,
			Action:     ActionCreate,
			Reason:     fmt.Sprintf("create %s at %s", want.TargetRef, shortHash(want.SourceHash)),
		})
	}
	return plans, nil
}

func selectBranches(source map[string]plumbing.Hash, requested []string) map[string]plumbing.Hash {
	if len(requested) == 0 {
		return source
	}

	selected := make(map[string]plumbing.Hash, len(requested))
	for _, branch := range requested {
		if hash, ok := source[branch]; ok {
			selected[branch] = hash
		}
	}
	return selected
}

func buildDesiredRefs(sourceRefs map[plumbing.ReferenceName]plumbing.Hash, cfg Config) (map[plumbing.ReferenceName]desiredRef, map[plumbing.ReferenceName]managedTarget, error) {
	desired := make(map[plumbing.ReferenceName]desiredRef)
	managed := make(map[plumbing.ReferenceName]managedTarget)

	addManaged := func(sourceRef, targetRef plumbing.ReferenceName, kind RefKind, hash plumbing.Hash) error {
		if hash.IsZero() {
			return fmt.Errorf("source ref %s not found", sourceRef)
		}
		short := targetRef.Short()
		desired[targetRef] = desiredRef{
			Kind:       kind,
			Label:      short,
			SourceRef:  sourceRef,
			TargetRef:  targetRef,
			SourceHash: hash,
		}
		managed[targetRef] = managedTarget{Kind: kind, Label: short}
		return nil
	}

	if len(cfg.Mappings) > 0 {
		for _, mapping := range cfg.Mappings {
			sourceRef, targetRef, kind, err := normalizeMapping(mapping)
			if err != nil {
				return nil, nil, err
			}
			if err := addManaged(sourceRef, targetRef, kind, sourceRefs[sourceRef]); err != nil {
				return nil, nil, err
			}
		}
	} else {
		branches := branchMapFromRefHashMap(sourceRefs)
		selected := selectBranches(branches, cfg.Branches)
		for branch, hash := range selected {
			refName := plumbing.NewBranchReferenceName(branch)
			if err := addManaged(refName, refName, RefKindBranch, hash); err != nil {
				return nil, nil, err
			}
		}
	}

	if cfg.IncludeTags {
		for refName, hash := range sourceRefs {
			if !refName.IsTag() {
				continue
			}
			if err := addManaged(refName, refName, RefKindTag, hash); err != nil {
				return nil, nil, err
			}
		}
	}

	return desired, managed, nil
}

func normalizeMapping(mapping RefMapping) (plumbing.ReferenceName, plumbing.ReferenceName, RefKind, error) {
	src := strings.TrimSpace(mapping.Source)
	dst := strings.TrimSpace(mapping.Target)
	if src == "" || dst == "" {
		return "", "", "", fmt.Errorf("invalid mapping %q:%q", mapping.Source, mapping.Target)
	}

	if strings.HasPrefix(src, "refs/") || strings.HasPrefix(dst, "refs/") {
		sourceRef := plumbing.ReferenceName(src)
		targetRef := plumbing.ReferenceName(dst)
		kind := refKindFromName(targetRef)
		if kind == "" {
			return "", "", "", fmt.Errorf("unsupported mapped ref kind: %s -> %s", src, dst)
		}
		return sourceRef, targetRef, kind, nil
	}

	return plumbing.NewBranchReferenceName(src), plumbing.NewBranchReferenceName(dst), RefKindBranch, nil
}

func refKindFromName(name plumbing.ReferenceName) RefKind {
	switch {
	case name.IsBranch():
		return RefKindBranch
	case name.IsTag():
		return RefKindTag
	default:
		return ""
	}
}

type desiredRef struct {
	Kind       RefKind
	Label      string
	SourceRef  plumbing.ReferenceName
	TargetRef  plumbing.ReferenceName
	SourceHash plumbing.Hash
}

type managedTarget struct {
	Kind  RefKind
	Label string
}

func buildPlans(
	repo *git.Repository,
	desired map[plumbing.ReferenceName]desiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
	managed map[plumbing.ReferenceName]managedTarget,
	cfg Config,
) ([]BranchPlan, error) {
	if cfg.Prune {
		for targetRef := range targetRefs {
			if _, ok := managed[targetRef]; ok {
				continue
			}
			switch {
			case targetRef.IsTag() && cfg.IncludeTags:
				managed[targetRef] = managedTarget{Kind: RefKindTag, Label: targetRef.Short()}
			case targetRef.IsBranch() && len(cfg.Mappings) == 0 && len(cfg.Branches) == 0:
				managed[targetRef] = managedTarget{Kind: RefKindBranch, Label: targetRef.Short()}
			}
		}
	}

	targetNames := make([]plumbing.ReferenceName, 0, len(managed))
	for name := range managed {
		targetNames = append(targetNames, name)
	}
	sort.Slice(targetNames, func(i, j int) bool { return targetNames[i] < targetNames[j] })

	plans := make([]BranchPlan, 0, len(targetNames)+8)
	for _, targetRef := range targetNames {
		info := managed[targetRef]
		want, existsInDesired := desired[targetRef]
		targetHash, existsOnTarget := targetRefs[targetRef]

		if !existsInDesired {
			if cfg.Prune && existsOnTarget {
				plans = append(plans, BranchPlan{
					Branch:     info.Label,
					TargetRef:  targetRef,
					TargetHash: targetHash,
					Kind:       info.Kind,
					Action:     ActionDelete,
					Reason:     fmt.Sprintf("%s -> <deleted>", shortHash(targetHash)),
				})
			}
			continue
		}

		if !existsOnTarget {
			plans = append(plans, BranchPlan{
				Branch:     want.Label,
				SourceRef:  want.SourceRef,
				TargetRef:  want.TargetRef,
				SourceHash: want.SourceHash,
				Kind:       want.Kind,
				Action:     ActionCreate,
				Reason:     fmt.Sprintf("%s -> <new>", shortHash(want.SourceHash)),
			})
			continue
		}

		plan, err := planRef(repo, want, targetHash, cfg.Force)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}

	sort.Slice(plans, func(i, j int) bool {
		return plans[i].TargetRef.String() < plans[j].TargetRef.String()
	})
	return plans, nil
}

func planRef(repo *git.Repository, want desiredRef, targetHash plumbing.Hash, force bool) (BranchPlan, error) {
	plan := BranchPlan{
		Branch:     want.Label,
		SourceRef:  want.SourceRef,
		TargetRef:  want.TargetRef,
		SourceHash: want.SourceHash,
		TargetHash: targetHash,
		Kind:       want.Kind,
	}

	if want.SourceHash == targetHash {
		plan.Action = ActionSkip
		plan.Reason = fmt.Sprintf("%s already current", shortHash(want.SourceHash))
		return plan, nil
	}

	if want.Kind == RefKindTag {
		if force {
			plan.Action = ActionUpdate
			plan.Reason = fmt.Sprintf("%s -> %s (force tag update)", shortHash(targetHash), shortHash(want.SourceHash))
			return plan, nil
		}
		plan.Action = ActionBlock
		plan.Reason = fmt.Sprintf("%s differs from %s; use --force to retarget tag", shortHash(targetHash), shortHash(want.SourceHash))
		return plan, nil
	}

	sourceCommit, err := repo.CommitObject(want.SourceHash)
	if err != nil {
		return plan, fmt.Errorf("load source commit for %s: %w", want.TargetRef, err)
	}

	isFF, err := reachesCommitHash(repo.Storer, sourceCommit, targetHash)
	if err != nil {
		return plan, fmt.Errorf("check fast-forward for %s: %w", want.TargetRef, err)
	}
	if isFF {
		plan.Action = ActionUpdate
		plan.Reason = fmt.Sprintf("%s -> %s", shortHash(targetHash), shortHash(want.SourceHash))
		return plan, nil
	}

	if force {
		plan.Action = ActionUpdate
		plan.Reason = fmt.Sprintf("%s -> %s (force)", shortHash(targetHash), shortHash(want.SourceHash))
		return plan, nil
	}

	plan.Action = ActionBlock
	plan.Reason = fmt.Sprintf("%s is not an ancestor of %s", shortHash(targetHash), shortHash(want.SourceHash))
	return plan, nil
}

func planBranch(repo *git.Repository, branch string, sourceHash, targetHash plumbing.Hash) (BranchPlan, error) {
	return planRef(repo, desiredRef{
		Kind:       RefKindBranch,
		Label:      branch,
		SourceRef:  plumbing.NewBranchReferenceName(branch),
		TargetRef:  plumbing.NewBranchReferenceName(branch),
		SourceHash: sourceHash,
	}, targetHash, false)
}

func fetchSourceRefsWithHavesV1(
	ctx context.Context,
	repo *git.Repository,
	conn *transportConn,
	sourceAdv *packp.AdvRefs,
	desired map[plumbing.ReferenceName]desiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) error {
	session, err := conn.transport.NewUploadPackSession(conn.endpoint, conn.authMethod())
	if err != nil {
		return fmt.Errorf("open source upload-pack session: %w", err)
	}
	defer session.Close()

	req := packp.NewUploadPackRequestFromCapabilities(sourceAdv.Capabilities)
	for _, ref := range desired {
		req.Wants = append(req.Wants, ref.SourceHash)
	}
	req.Wants = sortedUniqueHashes(req.Wants)
	req.Haves = sortedUniqueHashes(mapsRefValues(targetRefs))
	if len(req.Wants) == 0 {
		return git.NoErrAlreadyUpToDate
	}
	if sourceAdv.Capabilities.Supports(capability.NoProgress) {
		_ = req.Capabilities.Set(capability.NoProgress)
	}
	conn.stats.addWantsHaves("source upload-pack", len(req.Wants), len(req.Haves))

	reader, err := session.UploadPack(ctx, req)
	if err != nil {
		if errors.Is(err, transport.ErrEmptyUploadPackRequest) {
			return git.NoErrAlreadyUpToDate
		}
		return fmt.Errorf("source upload-pack: %w", err)
	}
	defer ioutil.CheckClose(reader, &err)

	if err := packfile.UpdateObjectStorage(repo.Storer, buildSidebandIfSupported(req.Capabilities, reader, nil)); err != nil {
		return fmt.Errorf("store source packfile: %w", err)
	}

	for _, ref := range desired {
		localRef := plumbing.ReferenceName(localBranchRef(sourceRemoteName, ref.TargetRef.Short()))
		if ref.Kind == RefKindTag {
			localRef = plumbing.ReferenceName("refs/remotes/" + sourceRemoteName + "/tags/" + ref.TargetRef.Short())
		}
		if err := repo.Storer.SetReference(plumbing.NewHashReference(localRef, ref.SourceHash)); err != nil {
			return fmt.Errorf("set local source ref %s: %w", ref.SourceRef, err)
		}
	}
	return nil
}

func fetchSourcePackV1(
	ctx context.Context,
	conn *transportConn,
	sourceAdv *packp.AdvRefs,
	desired map[plumbing.ReferenceName]desiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) (io.ReadCloser, error) {
	session, err := conn.transport.NewUploadPackSession(conn.endpoint, conn.authMethod())
	if err != nil {
		return nil, fmt.Errorf("open source upload-pack session: %w", err)
	}

	req := packp.NewUploadPackRequestFromCapabilities(sourceAdv.Capabilities)
	for _, ref := range desired {
		req.Wants = append(req.Wants, ref.SourceHash)
	}
	req.Wants = sortedUniqueHashes(req.Wants)
	req.Haves = sortedUniqueHashes(mapsRefValues(targetRefs))
	if len(req.Wants) == 0 {
		_ = session.Close()
		return nil, git.NoErrAlreadyUpToDate
	}
	if sourceAdv.Capabilities.Supports(capability.NoProgress) {
		_ = req.Capabilities.Set(capability.NoProgress)
	}
	conn.stats.addWantsHaves("source upload-pack", len(req.Wants), len(req.Haves))

	reader, err := session.UploadPack(ctx, req)
	if err != nil {
		_ = session.Close()
		if errors.Is(err, transport.ErrEmptyUploadPackRequest) {
			return nil, git.NoErrAlreadyUpToDate
		}
		return nil, fmt.Errorf("source upload-pack: %w", err)
	}
	return &sessionReadCloser{
		Reader: buildSidebandIfSupported(req.Capabilities, reader, nil),
		closeFn: func() error {
			_ = reader.Close()
			return session.Close()
		},
	}, nil
}

func pushToTarget(
	ctx context.Context,
	repo *git.Repository,
	conn *transportConn,
	targetAdv *packp.AdvRefs,
	plans []BranchPlan,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
	verbose bool,
) error {
	session, err := conn.transport.NewReceivePackSession(conn.endpoint, conn.authMethod())
	if err != nil {
		return fmt.Errorf("open target receive-pack session: %w", err)
	}
	defer session.Close()

	req := packp.NewReferenceUpdateRequestFromCapabilities(targetAdv.Capabilities)
	req.Progress = progressWriter(verbose)
	if targetAdv.Capabilities.Supports(capability.Sideband64k) {
		_ = req.Capabilities.Set(capability.Sideband64k)
	} else if targetAdv.Capabilities.Supports(capability.Sideband) {
		_ = req.Capabilities.Set(capability.Sideband)
	}

	commands := make([]*packp.Command, 0, len(plans))
	objects := make([]plumbing.Hash, 0, len(plans))
	hasDelete := false
	hasUpdates := false
	for _, plan := range plans {
		cmd := &packp.Command{
			Name: plan.TargetRef,
			Old:  targetRefs[plan.TargetRef],
		}
		switch plan.Action {
		case ActionCreate, ActionUpdate:
			cmd.New = plan.SourceHash
			objects = append(objects, plan.SourceHash)
			hasUpdates = true
		case ActionDelete:
			cmd.New = plumbing.ZeroHash
			hasDelete = true
		}
		commands = append(commands, cmd)
	}
	req.Commands = commands
	conn.stats.addCommands("target receive-pack", len(commands))
	if hasDelete {
		if !targetAdv.Capabilities.Supports(capability.DeleteRefs) {
			return fmt.Errorf("target does not support delete-refs")
		}
		_ = req.Capabilities.Set(capability.DeleteRefs)
	}

	hashesToPush, err := objectsToPush(repo.Storer, objects, targetRefs)
	if err != nil {
		return fmt.Errorf("compute objects to push: %w", err)
	}

	report, err := receivePack(ctx, session, repo.Storer, req, hashesToPush, hasUpdates, !targetAdv.Capabilities.Supports(capability.OFSDelta))
	if err != nil {
		return err
	}
	if report != nil {
		if err := report.Error(); err != nil {
			return err
		}
	}
	return nil
}

func pushPackToTarget(
	ctx context.Context,
	conn *transportConn,
	targetAdv *packp.AdvRefs,
	plans []BranchPlan,
	pack io.ReadCloser,
	verbose bool,
) error {
	session, err := conn.transport.NewReceivePackSession(conn.endpoint, conn.authMethod())
	if err != nil {
		return fmt.Errorf("open target receive-pack session: %w", err)
	}
	defer session.Close()

	req := packp.NewReferenceUpdateRequestFromCapabilities(targetAdv.Capabilities)
	req.Progress = progressWriter(verbose)
	if targetAdv.Capabilities.Supports(capability.Sideband64k) {
		_ = req.Capabilities.Set(capability.Sideband64k)
	} else if targetAdv.Capabilities.Supports(capability.Sideband) {
		_ = req.Capabilities.Set(capability.Sideband)
	}

	commands := make([]*packp.Command, 0, len(plans))
	for _, plan := range plans {
		if plan.Action != ActionCreate {
			return fmt.Errorf("bootstrap only supports create actions")
		}
		commands = append(commands, &packp.Command{
			Name: plan.TargetRef,
			Old:  plumbing.ZeroHash,
			New:  plan.SourceHash,
		})
	}
	req.Commands = commands
	conn.stats.addCommands("target receive-pack", len(commands))

	report, err := receivePackStream(ctx, session, req, pack)
	if err != nil {
		return err
	}
	if report != nil {
		if err := report.Error(); err != nil {
			return err
		}
	}
	return nil
}

func receivePack(
	ctx context.Context,
	session transport.ReceivePackSession,
	store storer.Storer,
	req *packp.ReferenceUpdateRequest,
	hashes []plumbing.Hash,
	sendPack bool,
	useRefDeltas bool,
) (*packp.ReportStatus, error) {
	if !sendPack {
		return session.ReceivePack(ctx, req)
	}

	rd, wr := io.Pipe()
	req.Packfile = rd
	done := make(chan error, 1)

	go func() {
		enc := packfile.NewEncoder(wr, store, useRefDeltas)
		if _, err := enc.Encode(hashes, 10); err != nil {
			done <- wr.CloseWithError(err)
			return
		}
		done <- wr.Close()
	}()

	report, err := session.ReceivePack(ctx, req)
	if err != nil {
		_ = rd.Close()
		return nil, err
	}
	if err := <-done; err != nil {
		return nil, err
	}
	return report, nil
}

func receivePackStream(
	ctx context.Context,
	session transport.ReceivePackSession,
	req *packp.ReferenceUpdateRequest,
	pack io.ReadCloser,
) (*packp.ReportStatus, error) {
	req.Packfile = pack
	report, err := session.ReceivePack(ctx, req)
	if err != nil {
		_ = pack.Close()
		return nil, err
	}
	return report, pack.Close()
}

func objectsToPush(store storer.Storer, wants []plumbing.Hash, targetRefs map[plumbing.ReferenceName]plumbing.Hash) ([]plumbing.Hash, error) {
	targetHaves := sortedUniqueHashes(mapsRefValues(targetRefs))
	if len(wants) == 0 {
		return nil, nil
	}

	haveSet := make(map[plumbing.Hash]struct{}, len(targetHaves))
	for _, hash := range targetHaves {
		haveSet[hash] = struct{}{}
	}

	filteredWants := make([]plumbing.Hash, 0, len(wants))
	for _, hash := range sortedUniqueHashes(wants) {
		if _, ok := haveSet[hash]; ok {
			continue
		}
		filteredWants = append(filteredWants, hash)
	}
	if len(filteredWants) == 0 {
		return nil, nil
	}

	seen := make(map[plumbing.Hash]bool, len(filteredWants)*4)
	objects := make([]plumbing.Hash, 0, len(filteredWants)*16)
	for _, hash := range filteredWants {
		if err := collectPushObjects(store, hash, haveSet, seen, &objects); err != nil {
			return nil, err
		}
	}
	return objects, nil
}

func collectPushObjects(
	store storer.EncodedObjectStorer,
	hash plumbing.Hash,
	externalHaves map[plumbing.Hash]struct{},
	seen map[plumbing.Hash]bool,
	out *[]plumbing.Hash,
) error {
	if hash.IsZero() {
		return nil
	}
	if _, ok := externalHaves[hash]; ok {
		return nil
	}
	if seen[hash] {
		return nil
	}
	seen[hash] = true

	obj, err := store.EncodedObject(plumbing.AnyObject, hash)
	if err != nil {
		return fmt.Errorf("load object %s: %w", hash, err)
	}

	switch obj.Type() {
	case plumbing.CommitObject:
		commit, err := object.GetCommit(store, hash)
		if err != nil {
			return fmt.Errorf("load commit %s: %w", hash, err)
		}
		if err := collectPushObjects(store, commit.TreeHash, externalHaves, seen, out); err != nil {
			return err
		}
		for _, parentHash := range commit.ParentHashes {
			if err := collectPushObjects(store, parentHash, externalHaves, seen, out); err != nil {
				return err
			}
		}
	case plumbing.TreeObject:
		tree, err := object.GetTree(store, hash)
		if err != nil {
			return fmt.Errorf("load tree %s: %w", hash, err)
		}
		for _, entry := range tree.Entries {
			if err := collectPushObjects(store, entry.Hash, externalHaves, seen, out); err != nil {
				return err
			}
		}
	case plumbing.TagObject:
		tag, err := object.GetTag(store, hash)
		if err != nil {
			return fmt.Errorf("load tag %s: %w", hash, err)
		}
		if err := collectPushObjects(store, tag.Target, externalHaves, seen, out); err != nil {
			return err
		}
	case plumbing.BlobObject:
	default:
		return fmt.Errorf("unsupported object type %s for %s", obj.Type(), hash)
	}

	*out = append(*out, hash)
	return nil
}

type sessionReadCloser struct {
	io.Reader
	closeFn func() error
}

func (r *sessionReadCloser) Close() error {
	if r.closeFn == nil {
		return nil
	}
	return r.closeFn()
}

func limitPackReadCloser(r io.ReadCloser, maxBytes int64) io.ReadCloser {
	if maxBytes <= 0 {
		return r
	}
	return &packLimitReadCloser{
		ReadCloser: r,
		maxBytes:   maxBytes,
	}
}

type packLimitReadCloser struct {
	io.ReadCloser
	maxBytes int64
	read     int64
}

func (r *packLimitReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.read += int64(n)
	if r.read > r.maxBytes {
		return n, fmt.Errorf("source pack exceeded max-pack-bytes limit (%d)", r.maxBytes)
	}
	return n, err
}

func branchMapFromRefHashMap(refs map[plumbing.ReferenceName]plumbing.Hash) map[string]plumbing.Hash {
	branches := make(map[string]plumbing.Hash)
	for name, hash := range refs {
		if name.IsBranch() {
			branches[name.Short()] = hash
		}
	}
	return branches
}

func refHashMap(refs []*plumbing.Reference) map[plumbing.ReferenceName]plumbing.Hash {
	out := make(map[plumbing.ReferenceName]plumbing.Hash)
	for _, ref := range refs {
		if ref.Type() == plumbing.HashReference {
			out[ref.Name()] = ref.Hash()
		}
	}
	return out
}

func advertisedReferences(ar *packp.AdvRefs) ([]*plumbing.Reference, error) {
	refs, err := ar.AllReferences()
	if err != nil {
		return nil, err
	}

	iter, err := refs.IterReferences()
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var out []*plumbing.Reference
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		out = append(out, ref)
		return nil
	})
	return out, err
}

type transportConn struct {
	label     string
	endpoint  *transport.Endpoint
	transport transport.Transport
	http      *http.Client
	raw       Endpoint
	auth      transport.AuthMethod
	stats     *statsCollector
}

func newTransportConn(raw Endpoint, label string, stats *statsCollector) (*transportConn, error) {
	ep, err := transport.NewEndpoint(raw.URL)
	if err != nil {
		return nil, err
	}
	auth, err := resolveAuthMethod(raw, ep)
	if err != nil {
		return nil, err
	}

	httpClient := &http.Client{
		Transport: &countingRoundTripper{
			base:  http.DefaultTransport,
			label: label,
			stats: stats,
		},
	}

	return &transportConn{
		label:     label,
		endpoint:  ep,
		transport: transporthttp.NewClient(httpClient),
		http:      httpClient,
		raw:       raw,
		auth:      auth,
		stats:     stats,
	}, nil
}

func (c *transportConn) authMethod() transport.AuthMethod {
	return c.auth
}

func applyAuth(req *http.Request, authMethod transport.AuthMethod) {
	switch auth := authMethod.(type) {
	case *transporthttp.BasicAuth:
		auth.SetAuth(req)
	case *transporthttp.TokenAuth:
		auth.SetAuth(req)
	}
}

func reachesCommitHash(store storer.EncodedObjectStorer, start *object.Commit, target plumbing.Hash) (bool, error) {
	if start.Hash == target {
		return true, nil
	}

	seen := map[plumbing.Hash]bool{}
	stack := []*object.Commit{start}

	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[current.Hash] {
			continue
		}
		seen[current.Hash] = true

		for _, parentHash := range current.ParentHashes {
			if parentHash == target {
				return true, nil
			}
			if seen[parentHash] {
				continue
			}
			parent, err := object.GetCommit(store, parentHash)
			if err != nil {
				if errors.Is(err, plumbing.ErrObjectNotFound) {
					continue
				}
				return false, err
			}
			stack = append(stack, parent)
		}
	}

	return false, nil
}

func mapsRefValues(input map[plumbing.ReferenceName]plumbing.Hash) []plumbing.Hash {
	out := make([]plumbing.Hash, 0, len(input))
	for _, hash := range input {
		if !hash.IsZero() {
			out = append(out, hash)
		}
	}
	return out
}

func sortedUniqueHashes(input []plumbing.Hash) []plumbing.Hash {
	seen := make(map[plumbing.Hash]bool, len(input))
	out := make([]plumbing.Hash, 0, len(input))
	for _, hash := range input {
		if seen[hash] {
			continue
		}
		seen[hash] = true
		out = append(out, hash)
	}
	plumbing.HashesSort(out)
	return out
}

func countObjects(store storer.EncodedObjectStorer) (int, error) {
	iter, err := store.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		return 0, err
	}
	defer iter.Close()

	count := 0
	err = iter.ForEach(func(obj plumbing.EncodedObject) error {
		count++
		return nil
	})
	return count, err
}

func localBranchRef(remoteName, branch string) string {
	return plumbing.NewRemoteReferenceName(remoteName, branch).String()
}

func shortHash(hash plumbing.Hash) string {
	if hash.IsZero() {
		return "<zero>"
	}
	value := hash.String()
	if len(value) > 8 {
		return value[:8]
	}
	return value
}

func parseHaveRef(raw string) plumbing.ReferenceName {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "refs/") {
		return plumbing.ReferenceName(raw)
	}
	return plumbing.NewBranchReferenceName(raw)
}

func progressWriter(verbose bool) io.Writer {
	if !verbose {
		return nil
	}
	return os.Stderr
}

func (e Endpoint) authMethod() transport.AuthMethod {
	if e.BearerToken != "" {
		return &transporthttp.TokenAuth{Token: e.BearerToken}
	}
	if e.Token != "" {
		username := e.Username
		if username == "" {
			username = "git"
		}
		return &transporthttp.BasicAuth{Username: username, Password: e.Token}
	}
	return nil
}

var gitCredentialFillCommand = func(ctx context.Context, input string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "credential", "fill")
	cmd.Stdin = strings.NewReader(input)
	return cmd.Output()
}

func resolveAuthMethod(raw Endpoint, ep *transport.Endpoint) (transport.AuthMethod, error) {
	if auth := raw.authMethod(); auth != nil {
		return auth, nil
	}
	if ep == nil {
		return nil, nil
	}
	if ep.Protocol != "http" && ep.Protocol != "https" {
		return nil, nil
	}
	username, password, ok := lookupGitCredential(ep)
	if !ok {
		return nil, nil
	}
	return &transporthttp.BasicAuth{Username: username, Password: password}, nil
}

func lookupGitCredential(ep *transport.Endpoint) (string, string, bool) {
	input := credentialFillInput(ep)
	if input == "" {
		return "", "", false
	}
	output, err := gitCredentialFillCommand(context.Background(), input)
	if err != nil {
		return "", "", false
	}
	values := parseCredentialFillOutput(output)
	password := values["password"]
	if password == "" {
		return "", "", false
	}
	username := values["username"]
	if username == "" {
		if ep.User != "" {
			username = ep.User
		} else {
			username = "git"
		}
	}
	return username, password, true
}

func credentialFillInput(ep *transport.Endpoint) string {
	if ep == nil || ep.Host == "" {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("protocol=")
	builder.WriteString(ep.Protocol)
	builder.WriteString("\n")
	builder.WriteString("host=")
	builder.WriteString(ep.Host)
	builder.WriteString("\n")
	if path := strings.TrimPrefix(ep.Path, "/"); path != "" {
		builder.WriteString("path=")
		builder.WriteString(path)
		builder.WriteString("\n")
	}
	if ep.User != "" {
		builder.WriteString("username=")
		builder.WriteString(ep.User)
		builder.WriteString("\n")
	}
	builder.WriteString("\n")
	return builder.String()
}

func parseCredentialFillOutput(output []byte) map[string]string {
	values := map[string]string{}
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		key, value, ok := bytes.Cut(line, []byte{'='})
		if !ok {
			continue
		}
		values[string(key)] = string(value)
	}
	return values
}

func buildSidebandIfSupported(l *capability.List, reader io.Reader, p sideband.Progress) io.Reader {
	var t sideband.Type
	switch {
	case l.Supports(capability.Sideband):
		t = sideband.Sideband
	case l.Supports(capability.Sideband64k):
		t = sideband.Sideband64k
	default:
		return reader
	}

	d := sideband.NewDemuxer(t, reader)
	d.Progress = p
	return d
}

type statsCollector struct {
	enabled bool
	items   map[string]*ServiceStats
}

func newStats(enabled bool) *statsCollector {
	return &statsCollector{enabled: enabled, items: map[string]*ServiceStats{}}
}

func (s *statsCollector) ensure(name string) *ServiceStats {
	item, ok := s.items[name]
	if !ok {
		item = &ServiceStats{Name: name}
		s.items[name] = item
	}
	return item
}

func (s *statsCollector) addWantsHaves(name string, wants, haves int) {
	if !s.enabled {
		return
	}
	item := s.ensure(name)
	item.Wants += wants
	item.Haves += haves
}

func (s *statsCollector) addCommands(name string, commands int) {
	if !s.enabled {
		return
	}
	item := s.ensure(name)
	item.Commands += commands
}

func (s *statsCollector) recordRoundTrip(name string, requestBytes, responseBytes int64) {
	if !s.enabled {
		return
	}
	item := s.ensure(name)
	item.Requests++
	item.RequestBytes += requestBytes
	item.ResponseBytes += responseBytes
}

func (s *statsCollector) snapshot() Stats {
	out := Stats{Enabled: s.enabled, Items: map[string]*ServiceStats{}}
	for key, item := range s.items {
		copyItem := *item
		out.Items[key] = &copyItem
	}
	return out
}

type countingRoundTripper struct {
	base  http.RoundTripper
	label string
	stats *statsCollector
}

func (rt *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := rt.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	serviceName := req.Header.Get(statsPhaseHdr)
	if serviceName == "" {
		serviceName = req.URL.Query().Get("service")
		if serviceName == "" {
			serviceName = strings.TrimPrefix(req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:], "/")
		}
	}
	name := strings.TrimSpace(rt.label + " " + serviceName)
	requestBytes := req.ContentLength
	if requestBytes < 0 {
		requestBytes = 0
	}

	res.Body = &countingReadCloser{
		ReadCloser: res.Body,
		onClose: func(n int64) {
			rt.stats.recordRoundTrip(name, requestBytes, n)
		},
	}
	return res, nil
}

type countingReadCloser struct {
	io.ReadCloser
	n       int64
	onClose func(int64)
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReadCloser) Close() error {
	err := c.ReadCloser.Close()
	if c.onClose != nil {
		c.onClose(c.n)
		c.onClose = nil
	}
	return err
}
