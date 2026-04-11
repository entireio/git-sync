package syncer

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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
	"github.com/zalando/go-keyring"
)

const (
	sourceRemoteName = "source"
	protocolModeAuto = "auto"
	protocolModeV1   = "v1"
	protocolModeV2   = "v2"

	defaultAutoBatchMaxPackBytes = 512 * 1024 * 1024
	entireCLIClientID            = "entire-cli"
)

var bodyLimitPattern = regexp.MustCompile(`body exceeded size limit ([0-9]+)`)

type Endpoint struct {
	URL           string
	Username      string
	Token         string
	BearerToken   string
	SkipTLSVerify bool
}

type RefMapping struct {
	Source string
	Target string
}

type Config struct {
	Source            Endpoint
	Target            Endpoint
	Branches          []string
	Mappings          []RefMapping
	IncludeTags       bool
	DryRun            bool
	Verbose           bool
	ShowStats         bool
	MeasureMemory     bool
	Force             bool
	Prune             bool
	MaxPackBytes      int64
	BatchMaxPackBytes int64
	ProtocolMode      string
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
	Plans              []BranchPlan `json:"plans"`
	Pushed             int          `json:"pushed"`
	Skipped            int          `json:"skipped"`
	Blocked            int          `json:"blocked"`
	Deleted            int          `json:"deleted"`
	DryRun             bool         `json:"dry_run"`
	Relay              bool         `json:"relay"`
	RelayMode          string       `json:"relay_mode"`
	RelayReason        string       `json:"relay_reason"`
	Batching           bool         `json:"batching"`
	BatchCount         int          `json:"batch_count"`
	PlannedBatchCount  int          `json:"planned_batch_count"`
	TempRefs           []string     `json:"temp_refs"`
	BootstrapSuggested bool         `json:"bootstrap_suggested"`
	Stats              Stats        `json:"stats"`
	Measurement        Measurement  `json:"measurement"`
	Protocol           string       `json:"protocol"`
}

type ProbeResult struct {
	SourceURL     string      `json:"source_url"`
	TargetURL     string      `json:"target_url,omitempty"`
	RequestedMode string      `json:"requested_mode"`
	Protocol      string      `json:"protocol"`
	RefPrefixes   []string    `json:"ref_prefixes"`
	Capabilities  []string    `json:"source_capabilities"`
	TargetCaps    []string    `json:"target_capabilities,omitempty"`
	Refs          []RefInfo   `json:"refs"`
	Stats         Stats       `json:"stats"`
	Measurement   Measurement `json:"measurement"`
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
	Measurement    Measurement     `json:"measurement"`
}

type entireAuthHostInfo struct {
	ActiveUser string   `json:"activeUser"`
	Users      []string `json:"users"`
}

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
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

type Measurement struct {
	Enabled            bool   `json:"enabled"`
	ElapsedMillis      int64  `json:"elapsed_millis"`
	PeakAllocBytes     uint64 `json:"peak_alloc_bytes"`
	PeakHeapInuseBytes uint64 `json:"peak_heap_inuse_bytes"`
	TotalAllocBytes    uint64 `json:"total_alloc_bytes"`
	GCCount            uint32 `json:"gc_count"`
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
		SourceURL      string      `json:"source_url"`
		RequestedMode  string      `json:"requested_mode"`
		Protocol       string      `json:"protocol"`
		Wants          []RefInfo   `json:"wants"`
		Haves          []string    `json:"haves"`
		FetchedObjects int         `json:"fetched_objects"`
		Stats          Stats       `json:"stats"`
		Measurement    Measurement `json:"measurement"`
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
		Measurement:    r.Measurement,
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
		"summary: pushed=%d deleted=%d skipped=%d blocked=%d protocol=%s relay=%t relay-mode=%s relay-reason=%s batching=%t batch-count=%d planned-batches=%d",
		r.Pushed, r.Deleted, r.Skipped, r.Blocked, r.Protocol, r.Relay, r.RelayMode, r.RelayReason, r.Batching, r.BatchCount, r.PlannedBatchCount,
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
	if r.Measurement.Enabled {
		lines = append(lines, fmt.Sprintf(
			"measurement: elapsed-ms=%d peak-alloc-bytes=%d peak-heap-inuse-bytes=%d total-alloc-bytes=%d gc-count=%d",
			r.Measurement.ElapsedMillis, r.Measurement.PeakAllocBytes, r.Measurement.PeakHeapInuseBytes, r.Measurement.TotalAllocBytes, r.Measurement.GCCount,
		))
	}
	if r.BootstrapSuggested {
		lines = append(lines, "hint: target refs are absent; bootstrap can seed them without local object storage")
	}

	if r.Batching && len(r.TempRefs) > 0 {
		lines = append(lines, fmt.Sprintf("batching: temp-refs=%s", strings.Join(r.TempRefs, ",")))
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
	if r.Measurement.Enabled {
		lines = append(lines, fmt.Sprintf(
			"measurement: elapsed-ms=%d peak-alloc-bytes=%d peak-heap-inuse-bytes=%d total-alloc-bytes=%d gc-count=%d",
			r.Measurement.ElapsedMillis, r.Measurement.PeakAllocBytes, r.Measurement.PeakHeapInuseBytes, r.Measurement.TotalAllocBytes, r.Measurement.GCCount,
		))
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
	if r.Measurement.Enabled {
		lines = append(lines, fmt.Sprintf(
			"measurement: elapsed-ms=%d peak-alloc-bytes=%d peak-heap-inuse-bytes=%d total-alloc-bytes=%d gc-count=%d",
			r.Measurement.ElapsedMillis, r.Measurement.PeakAllocBytes, r.Measurement.PeakHeapInuseBytes, r.Measurement.TotalAllocBytes, r.Measurement.GCCount,
		))
	}
	return lines
}

func Run(ctx context.Context, cfg Config) (Result, error) {
	measurementDone := startMeasurement(cfg.MeasureMemory)
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
	if ok, reason := canBootstrapRelay(cfg, desiredRefs, targetRefMap); ok {
		if cfg.DryRun {
			plans, err := buildBootstrapPlans(desiredRefs, targetRefMap)
			if err != nil {
				return Result{}, err
			}
			return Result{
				Plans:              plans,
				DryRun:             true,
				Relay:              false,
				RelayMode:          "",
				RelayReason:        reason,
				BootstrapSuggested: true,
				Stats:              stats.snapshot(),
				Measurement:        measurementDone(),
				Protocol:           sourceService.protocol,
			}, nil
		}
		return bootstrapWithInputs(ctx, cfg, stats, sourceConn, targetConn, sourceService, targetAdv, desiredRefs, targetRefMap, reason)
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
		Plans:       plans,
		DryRun:      cfg.DryRun,
		Relay:       false,
		RelayMode:   "",
		RelayReason: "",
		Stats:       stats.snapshot(),
		Measurement: measurementDone(),
		Protocol:    sourceService.protocol,
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
	result.RelayReason = relayFallbackReason(cfg, pushPlans, targetAdv)

	if !cfg.DryRun {
		if ok, reason := canIncrementalRelay(cfg, pushPlans, targetAdv); ok {
			relayPlans := append([]BranchPlan(nil), pushPlans...)
			desiredRelay := desiredSubsetForPlans(desiredRefs, relayPlans)
			packReader, err := sourceService.FetchPack(ctx, sourceConn, desiredRelay, targetRefMap)
			if err != nil {
				return result, fmt.Errorf("fetch source pack: %w", err)
			}
			defer packReader.Close()
			packReader = limitPackReadCloser(packReader, cfg.MaxPackBytes)
			if err := pushPackToTarget(ctx, targetConn, targetAdv, relayPlans, packReader, cfg.Verbose); err != nil {
				return result, fmt.Errorf("push target refs: %w", err)
			}
			result.Relay = true
			result.RelayMode = "incremental"
			result.RelayReason = reason
		} else if ok, reason := canFullTagCreateRelay(pushPlans); ok {
			relayPlans := append([]BranchPlan(nil), pushPlans...)
			desiredRelay := desiredSubsetForPlans(desiredRefs, relayPlans)
			packReader, err := sourceService.FetchPack(ctx, sourceConn, desiredRelay, nil)
			if err != nil {
				return result, fmt.Errorf("fetch source tag pack: %w", err)
			}
			defer packReader.Close()
			packReader = limitPackReadCloser(packReader, cfg.MaxPackBytes)
			if err := pushPackToTarget(ctx, targetConn, targetAdv, relayPlans, packReader, cfg.Verbose); err != nil {
				return result, fmt.Errorf("push target refs: %w", err)
			}
			result.Relay = true
			result.RelayMode = "incremental"
			result.RelayReason = reason
		} else if len(pushPlans) > 0 {
			if err := ensureLocalObjectsForPush(ctx, repo, sourceConn, sourceService, desiredRefs, pushPlans); err != nil {
				return result, fmt.Errorf("prepare local objects for push: %w", err)
			}
			if err := pushToTarget(ctx, repo, targetConn, targetAdv, pushPlans, targetRefMap, cfg.Verbose); err != nil {
				return result, fmt.Errorf("push target refs: %w", err)
			}
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
	result.Measurement = measurementDone()
	return result, nil
}

func Bootstrap(ctx context.Context, cfg Config) (Result, error) {
	measurementDone := startMeasurement(cfg.MeasureMemory)
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
	_, reason := canBootstrapRelay(cfg, desiredRefs, targetRefMap)
	result, err := bootstrapWithInputs(ctx, cfg, stats, sourceConn, targetConn, sourceService, targetAdv, desiredRefs, targetRefMap, reason)
	result.Measurement = measurementDone()
	return result, err
}

func Probe(ctx context.Context, cfg Config) (ProbeResult, error) {
	measurementDone := startMeasurement(cfg.MeasureMemory)
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
		Measurement:   measurementDone(),
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
		result.Measurement = measurementDone()
	}

	return result, nil
}

func Fetch(ctx context.Context, cfg Config, haveRefs []string, haveHashes []plumbing.Hash) (FetchResult, error) {
	measurementDone := startMeasurement(cfg.MeasureMemory)
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
		Measurement:    measurementDone(),
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

func canBootstrapRelay(
	cfg Config,
	desired map[plumbing.ReferenceName]desiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) (bool, string) {
	if cfg.Force || cfg.Prune {
		return false, "bootstrap-disabled-by-force-or-prune"
	}
	if len(desired) == 0 {
		return false, "bootstrap-no-managed-refs"
	}
	for targetRef := range desired {
		if !targetRefs[targetRef].IsZero() {
			return false, "bootstrap-target-ref-exists"
		}
	}
	return true, "empty-target-managed-refs"
}

func canIncrementalRelay(cfg Config, plans []BranchPlan, targetAdv *packp.AdvRefs) (bool, string) {
	if cfg.Force || cfg.Prune || cfg.DryRun {
		return false, "incremental-disabled-by-force-prune-or-dry-run"
	}
	if len(plans) == 0 {
		return false, "incremental-no-plans"
	}
	if targetAdv == nil || targetAdv.Capabilities == nil {
		return false, "incremental-missing-target-capabilities"
	}
	if targetAdv.Capabilities.Supports(capability.Capability("no-thin")) {
		return false, "incremental-target-no-thin"
	}

	for _, plan := range plans {
		switch plan.Kind {
		case RefKindBranch:
			if !plan.SourceRef.IsBranch() || !plan.TargetRef.IsBranch() {
				return false, "incremental-non-branch-mapping"
			}
			if plan.Action != ActionUpdate {
				return false, "incremental-branch-action-not-update"
			}
			if plan.TargetHash.IsZero() {
				return false, "incremental-branch-target-missing"
			}
		case RefKindTag:
			if !plan.SourceRef.IsTag() || !plan.TargetRef.IsTag() {
				return false, "incremental-non-tag-mapping"
			}
			if plan.Action != ActionCreate {
				return false, "incremental-tag-action-not-create"
			}
		default:
			return false, "incremental-unsupported-ref-kind"
		}
	}
	return true, "fast-forward-branch-or-tag-create"
}

func relayFallbackReason(cfg Config, plans []BranchPlan, targetAdv *packp.AdvRefs) string {
	if ok, reason := canIncrementalRelay(cfg, plans, targetAdv); ok {
		return reason
	} else if ok, reason := canFullTagCreateRelay(plans); ok {
		return reason
	} else {
		return reason
	}
}

func canFullTagCreateRelay(plans []BranchPlan) (bool, string) {
	if len(plans) == 0 {
		return false, "incremental-no-plans"
	}
	for _, plan := range plans {
		if plan.Kind != RefKindTag {
			return false, "incremental-tag-relay-non-tag-plan"
		}
		if !plan.SourceRef.IsTag() || !plan.TargetRef.IsTag() {
			return false, "incremental-tag-relay-non-tag-mapping"
		}
		if plan.Action != ActionCreate {
			return false, "incremental-tag-relay-tag-action-not-create"
		}
	}
	return true, "tag-create-full-pack"
}

func desiredSubsetForPlans(
	desired map[plumbing.ReferenceName]desiredRef,
	plans []BranchPlan,
) map[plumbing.ReferenceName]desiredRef {
	out := make(map[plumbing.ReferenceName]desiredRef, len(plans))
	for _, plan := range plans {
		if ref, ok := desired[plan.TargetRef]; ok {
			out[plan.TargetRef] = ref
		}
	}
	return out
}

func ensureLocalObjectsForPush(
	ctx context.Context,
	repo *git.Repository,
	sourceConn *transportConn,
	sourceService *sourceRefService,
	desired map[plumbing.ReferenceName]desiredRef,
	plans []BranchPlan,
) error {
	tagDesired := make(map[plumbing.ReferenceName]desiredRef)
	for _, plan := range plans {
		if plan.Kind != RefKindTag {
			continue
		}
		if desiredRef, ok := desired[plan.TargetRef]; ok {
			tagDesired[plan.TargetRef] = desiredRef
		}
	}
	if len(tagDesired) == 0 {
		return nil
	}
	return sourceService.Fetch(ctx, repo, sourceConn, tagDesired, nil)
}

func bootstrapWithInputs(
	ctx context.Context,
	cfg Config,
	stats *statsCollector,
	sourceConn *transportConn,
	targetConn *transportConn,
	sourceService *sourceRefService,
	targetAdv *packp.AdvRefs,
	desiredRefs map[plumbing.ReferenceName]desiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
	relayReason string,
) (Result, error) {
	plans, err := buildBootstrapPlans(desiredRefs, targetRefs)
	if err != nil {
		return Result{}, err
	}

	result := Result{
		Plans:       plans,
		Relay:       true,
		RelayMode:   "bootstrap",
		RelayReason: relayReason,
		Stats:       stats.snapshot(),
		Protocol:    sourceService.protocol,
	}

	if cfg.BatchMaxPackBytes > 0 {
		return bootstrapBatchedWithInputs(ctx, cfg, stats, sourceConn, targetConn, sourceService, targetAdv, plans, desiredRefs, targetRefs, result)
	}

	progressf(cfg.Verbose, "bootstrap: fetching %d ref(s) from source", len(plans))

	packReader, err := sourceService.FetchPack(ctx, sourceConn, desiredRefs, nil)
	if err != nil {
		if errors.Is(err, git.NoErrAlreadyUpToDate) {
			return result, nil
		}
		return result, fmt.Errorf("fetch source pack: %w", err)
	}
	packReader = limitPackReadCloser(packReader, cfg.MaxPackBytes)

	progressf(cfg.Verbose, "bootstrap: pushing %d ref(s) to target", len(plans))
	pushErr := pushPackToTarget(ctx, targetConn, targetAdv, plans, packReader, cfg.Verbose)
	if closeErr := packReader.Close(); closeErr != nil && pushErr == nil {
		pushErr = closeErr
	}
	if pushErr != nil {
		autoBatchSize, ok := autoBatchMaxPackBytes(cfg, sourceService, pushErr)
		if !ok {
			return result, fmt.Errorf("push target refs: %w", pushErr)
		}
		progressf(
			cfg.Verbose,
			"bootstrap: target rejected single-pack push; retrying with batch-max-pack-bytes=%d",
			autoBatchSize,
		)
		cfg.BatchMaxPackBytes = autoBatchSize
		return bootstrapBatchedWithInputs(ctx, cfg, stats, sourceConn, targetConn, sourceService, targetAdv, plans, desiredRefs, targetRefs, result)
	}

	result.Pushed = len(plans)
	result.Stats = stats.snapshot()
	return result, nil
}

func autoBatchMaxPackBytes(cfg Config, sourceService *sourceRefService, err error) (int64, bool) {
	if cfg.BatchMaxPackBytes > 0 || !isTargetBodyLimitError(err) {
		return 0, false
	}
	if sourceService == nil || sourceService.protocol != protocolModeV2 || !fetchCapabilitySupports(sourceService.v2, "filter") {
		return 0, false
	}

	batchLimit := int64(defaultAutoBatchMaxPackBytes)
	if targetLimit := targetBodyLimit(err); targetLimit > 0 {
		derivedLimit := targetLimit / 2
		if derivedLimit <= 0 {
			derivedLimit = targetLimit
		}
		if derivedLimit < batchLimit {
			batchLimit = derivedLimit
		}
	}
	if cfg.MaxPackBytes > 0 && cfg.MaxPackBytes < batchLimit {
		batchLimit = cfg.MaxPackBytes
	}
	if batchLimit <= 0 {
		return 0, false
	}
	return batchLimit, true
}

func isTargetBodyLimitError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "body exceeded size limit") ||
		(strings.Contains(message, "request body") && strings.Contains(message, "too large")) ||
		(strings.Contains(message, "payload") && strings.Contains(message, "too large")) ||
		strings.Contains(message, "http 413")
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

type bootstrapBatch struct {
	Plan        BranchPlan
	TempRef     plumbing.ReferenceName
	ResumeHash  plumbing.Hash
	Checkpoints []plumbing.Hash
}

func bootstrapBatchedWithInputs(
	ctx context.Context,
	cfg Config,
	stats *statsCollector,
	sourceConn *transportConn,
	targetConn *transportConn,
	sourceService *sourceRefService,
	targetAdv *packp.AdvRefs,
	plans []BranchPlan,
	desiredRefs map[plumbing.ReferenceName]desiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
	result Result,
) (Result, error) {
	if sourceService.protocol != protocolModeV2 {
		return result, fmt.Errorf("bootstrap batching currently requires protocol v2")
	}
	if !fetchCapabilitySupports(sourceService.v2, "filter") {
		return result, fmt.Errorf("bootstrap batching requires source fetch filter support")
	}

	planRefs := make([]desiredRef, 0, len(plans))
	tagPlans := make([]BranchPlan, 0, len(plans))
	tagDesired := make(map[plumbing.ReferenceName]desiredRef)
	for _, plan := range plans {
		if plan.Kind == RefKindTag {
			tagPlans = append(tagPlans, plan)
			if desired, ok := desiredRefs[plan.TargetRef]; ok {
				tagDesired[plan.TargetRef] = desired
			}
			continue
		}
		if !plan.SourceRef.IsBranch() || !plan.TargetRef.IsBranch() {
			return result, fmt.Errorf("bootstrap batching currently supports branch refs and create-only tags")
		}
		planRefs = append(planRefs, desiredRef{
			Kind:       plan.Kind,
			Label:      plan.Branch,
			SourceRef:  plan.SourceRef,
			TargetRef:  plan.TargetRef,
			SourceHash: plan.SourceHash,
		})
	}

	var (
		batches []bootstrapBatch
		err     error
	)
	if len(planRefs) > 0 {
		progressf(cfg.Verbose, "bootstrap-batch: planning checkpoints for %d branch ref(s)", len(planRefs))
		batches, err = planBootstrapBatches(ctx, cfg, sourceConn, sourceService, planRefs, targetRefs)
		if err != nil {
			return result, err
		}
	}

	batchLimit := cfg.BatchMaxPackBytes
	if cfg.MaxPackBytes > 0 && (batchLimit == 0 || cfg.MaxPackBytes < batchLimit) {
		batchLimit = cfg.MaxPackBytes
	}

	for _, batch := range batches {
		result.PlannedBatchCount += len(batch.Checkpoints)
		result.TempRefs = append(result.TempRefs, batch.TempRef.String())
		progressf(
			cfg.Verbose,
			"bootstrap-batch: branch=%s temp-ref=%s planned-batches=%d resume=%s",
			batch.Plan.TargetRef,
			batch.TempRef,
			len(batch.Checkpoints),
			shortHash(batch.ResumeHash),
		)
		current := batch.ResumeHash
		startIdx, err := bootstrapResumeIndex(batch.Checkpoints, batch.ResumeHash)
		if err != nil {
			return result, fmt.Errorf("resume bootstrap batch for %s: %w", batch.Plan.TargetRef, err)
		}
		for idx := startIdx; idx < len(batch.Checkpoints); idx++ {
			checkpoint := batch.Checkpoints[idx]
			progressf(
				cfg.Verbose,
				"bootstrap-batch: branch=%s batch=%d/%d from=%s to=%s",
				batch.Plan.TargetRef,
				idx+1,
				len(batch.Checkpoints),
				shortHash(current),
				shortHash(checkpoint),
			)
			stagePlans := []BranchPlan{
				{
					Branch:     batch.Plan.Branch,
					SourceRef:  batch.Plan.SourceRef,
					TargetRef:  batch.TempRef,
					SourceHash: checkpoint,
					TargetHash: current,
					Kind:       batch.Plan.Kind,
					Action:     actionForTargetHash(current),
					Reason:     fmt.Sprintf("%s -> %s via %s", shortHash(current), shortHash(checkpoint), batch.TempRef),
				},
			}
			if idx == len(batch.Checkpoints)-1 {
				stagePlans = append(stagePlans, BranchPlan{
					Branch:     batch.Plan.Branch,
					SourceRef:  batch.Plan.SourceRef,
					TargetRef:  batch.Plan.TargetRef,
					SourceHash: checkpoint,
					TargetHash: plumbing.ZeroHash,
					Kind:       batch.Plan.Kind,
					Action:     ActionCreate,
					Reason:     fmt.Sprintf("create %s at %s", batch.Plan.TargetRef, shortHash(checkpoint)),
				})
			}

			packReader, err := sourceService.FetchPack(ctx, sourceConn, singleDesiredRef(batch.Plan.SourceRef, batch.TempRef, checkpoint), singleHaveMap(current))
			if err != nil {
				return result, fmt.Errorf("fetch source batch pack for %s: %w", batch.Plan.TargetRef, err)
			}
			packReader = limitPackReadCloser(packReader, batchLimit)
			if err := pushPackToTarget(ctx, targetConn, targetAdv, stagePlans, packReader, cfg.Verbose); err != nil {
				return result, fmt.Errorf("push bootstrap batch for %s: %w", batch.Plan.TargetRef, err)
			}
			progressf(
				cfg.Verbose,
				"bootstrap-batch: branch=%s batch=%d/%d complete",
				batch.Plan.TargetRef,
				idx+1,
				len(batch.Checkpoints),
			)
			current = checkpoint
			result.BatchCount++
		}

		if current.IsZero() {
			return result, fmt.Errorf("bootstrap batching for %s completed with no checkpoint state", batch.Plan.TargetRef)
		}
		if batch.ResumeHash == batch.Plan.SourceHash {
			if err := pushCommandsToTarget(ctx, targetConn, targetAdv, []BranchPlan{{
				Branch:     batch.Plan.Branch,
				SourceRef:  batch.Plan.SourceRef,
				TargetRef:  batch.Plan.TargetRef,
				SourceHash: batch.Plan.SourceHash,
				TargetHash: plumbing.ZeroHash,
				Kind:       batch.Plan.Kind,
				Action:     ActionCreate,
				Reason:     fmt.Sprintf("create %s at %s", batch.Plan.TargetRef, shortHash(batch.Plan.SourceHash)),
			}}, cfg.Verbose); err != nil {
				return result, fmt.Errorf("resume bootstrap cutover for %s: %w", batch.Plan.TargetRef, err)
			}
		}

		deleteTempPlan := BranchPlan{
			Branch:     batch.Plan.Branch,
			TargetRef:  batch.TempRef,
			SourceHash: plumbing.ZeroHash,
			TargetHash: current,
			Kind:       batch.Plan.Kind,
			Action:     ActionDelete,
			Reason:     fmt.Sprintf("delete temp ref %s", batch.TempRef),
		}
		if err := pushCommandsToTarget(ctx, targetConn, targetAdv, []BranchPlan{deleteTempPlan}, cfg.Verbose); err != nil {
			return result, fmt.Errorf("delete bootstrap temp ref for %s: %w", batch.Plan.TargetRef, err)
		}
		progressf(cfg.Verbose, "bootstrap-batch: branch=%s finalized", batch.Plan.TargetRef)
	}

	if len(tagPlans) > 0 {
		progressf(cfg.Verbose, "bootstrap-batch: pushing %d tag(s) after branch batches", len(tagPlans))
		tagTargetRefs := copyRefHashMap(targetRefs)
		for _, batch := range batches {
			tagTargetRefs[batch.Plan.TargetRef] = batch.Plan.SourceHash
		}
		packReader, err := sourceService.FetchPack(ctx, sourceConn, tagDesired, tagTargetRefs)
		if err != nil {
			if !errors.Is(err, git.NoErrAlreadyUpToDate) {
				return result, fmt.Errorf("fetch bootstrap tag pack: %w", err)
			}
		} else {
			defer packReader.Close()
			packReader = limitPackReadCloser(packReader, cfg.MaxPackBytes)
			if err := pushPackToTarget(ctx, targetConn, targetAdv, tagPlans, packReader, cfg.Verbose); err != nil {
				return result, fmt.Errorf("push bootstrap tags: %w", err)
			}
		}
	}

	result.Pushed = len(plans)
	result.Batching = true
	result.RelayMode = "bootstrap-batch"
	result.Stats = stats.snapshot()
	return result, nil
}

func actionForTargetHash(hash plumbing.Hash) Action {
	if hash.IsZero() {
		return ActionCreate
	}
	return ActionUpdate
}

func singleDesiredRef(sourceRef, targetRef plumbing.ReferenceName, hash plumbing.Hash) map[plumbing.ReferenceName]desiredRef {
	return map[plumbing.ReferenceName]desiredRef{
		targetRef: {
			Kind:       RefKindBranch,
			Label:      targetRef.Short(),
			SourceRef:  sourceRef,
			TargetRef:  targetRef,
			SourceHash: hash,
		},
	}
}

func singleHaveMap(hash plumbing.Hash) map[plumbing.ReferenceName]plumbing.Hash {
	if hash.IsZero() {
		return nil
	}
	return map[plumbing.ReferenceName]plumbing.Hash{
		plumbing.ReferenceName("refs/gitsync/have"): hash,
	}
}

func bootstrapTempRef(targetRef plumbing.ReferenceName) plumbing.ReferenceName {
	return plumbing.ReferenceName("refs/gitsync/bootstrap/heads/" + targetRef.Short())
}

func planBootstrapBatches(
	ctx context.Context,
	cfg Config,
	sourceConn *transportConn,
	sourceService *sourceRefService,
	desired []desiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) ([]bootstrapBatch, error) {
	out := make([]bootstrapBatch, 0, len(desired))
	for _, ref := range desired {
		checkpoints, err := planBootstrapBranchCheckpoints(ctx, cfg, sourceConn, sourceService, ref)
		if err != nil {
			return nil, err
		}
		out = append(out, bootstrapBatch{
			Plan: BranchPlan{
				Branch:     ref.Label,
				SourceRef:  ref.SourceRef,
				TargetRef:  ref.TargetRef,
				SourceHash: ref.SourceHash,
				Kind:       ref.Kind,
				Action:     ActionCreate,
			},
			TempRef:     bootstrapTempRef(ref.TargetRef),
			ResumeHash:  targetRefs[bootstrapTempRef(ref.TargetRef)],
			Checkpoints: checkpoints,
		})
	}
	return out, nil
}

func planBootstrapBranchCheckpoints(
	ctx context.Context,
	cfg Config,
	sourceConn *transportConn,
	sourceService *sourceRefService,
	ref desiredRef,
) ([]plumbing.Hash, error) {
	progressf(cfg.Verbose, "bootstrap-batch: fetching commit graph for %s", ref.TargetRef)
	graphRepo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		return nil, fmt.Errorf("init bootstrap planning repository: %w", err)
	}
	if err := fetchSourceCommitGraphV2(ctx, graphRepo, sourceConn, sourceService.v2, ref); err != nil {
		return nil, fmt.Errorf("fetch bootstrap planning graph for %s: %w", ref.TargetRef, err)
	}

	chain, err := firstParentChain(graphRepo, ref.SourceHash)
	if err != nil {
		return nil, fmt.Errorf("walk first-parent chain for %s: %w", ref.TargetRef, err)
	}
	if len(chain) == 0 {
		return nil, fmt.Errorf("empty first-parent chain for %s", ref.TargetRef)
	}

	checkpoints := make([]plumbing.Hash, 0, len(chain))
	prevIdx := -1
	prevHash := plumbing.ZeroHash
	prevSpan := 0
	for prevIdx < len(chain)-1 {
		bestIdx, err := largestCheckpointUnderLimit(ctx, cfg, sourceConn, sourceService, ref, chain, prevIdx, prevHash, prevSpan)
		if err != nil {
			return nil, err
		}
		if bestIdx <= prevIdx {
			return nil, fmt.Errorf("could not find bootstrap checkpoint for %s under batch-max-pack-bytes=%d", ref.TargetRef, cfg.BatchMaxPackBytes)
		}
		prevSpan = bestIdx - prevIdx
		prevIdx = bestIdx
		prevHash = chain[bestIdx]
		checkpoints = append(checkpoints, prevHash)
		progressf(
			cfg.Verbose,
			"bootstrap-batch: branch=%s planned-checkpoint=%s selected=%d chain-len=%d",
			ref.TargetRef,
			shortHash(prevHash),
			len(checkpoints),
			len(chain),
		)
	}
	return checkpoints, nil
}

func largestCheckpointUnderLimit(
	ctx context.Context,
	cfg Config,
	sourceConn *transportConn,
	sourceService *sourceRefService,
	ref desiredRef,
	chain []plumbing.Hash,
	prevIdx int,
	prevHash plumbing.Hash,
	prevSpan int,
) (int, error) {
	return sampledCheckpointUnderLimitByProbe(chain, prevIdx, prevSpan, func(idx int) (bool, error) {
		tooLarge, err := sourcePackExceedsLimit(ctx, sourceConn, sourceService, ref, chain[idx], prevHash, cfg.BatchMaxPackBytes)
		if err != nil {
			return false, fmt.Errorf("measure bootstrap batch for %s at %s: %w", ref.TargetRef, shortHash(chain[idx]), err)
		}
		if tooLarge {
			progressf(cfg.Verbose, "bootstrap-batch: sample %s exceeds limit=%d", shortHash(chain[idx]), cfg.BatchMaxPackBytes)
		} else {
			progressf(cfg.Verbose, "bootstrap-batch: sample %s fits limit=%d", shortHash(chain[idx]), cfg.BatchMaxPackBytes)
		}
		return tooLarge, nil
	})
}

func sampledCheckpointUnderLimitByProbe(
	chain []plumbing.Hash,
	prevIdx int,
	prevSpan int,
	probe func(idx int) (bool, error),
) (int, error) {
	lo := prevIdx + 1
	hi := len(chain) - 1
	if lo > hi {
		return -1, nil
	}

	samples := sampledCheckpointCandidates(lo, hi, prevSpan)
	best := -1
	for _, idx := range samples {
		tooLarge, err := probe(idx)
		if err != nil {
			return -1, err
		}
		if tooLarge {
			continue
		}
		best = idx
		break
	}
	if best != -1 {
		return best, nil
	}

	if prevSpan > 1 {
		shrunk := prevSpan / 2
		if shrunk < 1 {
			shrunk = 1
		}
		idx := lo + shrunk - 1
		if idx > hi {
			idx = hi
		}
		if idx >= lo {
			tooLarge, err := probe(idx)
			if err != nil {
				return -1, err
			}
			if !tooLarge {
				return idx, nil
			}
		}
	}

	tooLarge, err := probe(lo)
	if err != nil {
		return -1, err
	}
	if tooLarge {
		return -1, nil
	}
	return lo, nil
}

func sampledCheckpointCandidates(lo, hi int, prevSpan int) []int {
	if lo > hi {
		return nil
	}

	set := map[int]struct{}{}
	add := func(idx int) {
		if idx < lo {
			idx = lo
		}
		if idx > hi {
			idx = hi
		}
		set[idx] = struct{}{}
	}

	projected := hi
	if prevSpan > 0 {
		projected = lo + prevSpan - 1
	}
	add(projected)

	const sampleCount = 4
	current := projected
	for i := 0; i < sampleCount-1; i++ {
		if current <= lo {
			add(lo)
			continue
		}
		distance := current - lo
		current = lo + distance/2
		add(current)
	}
	add(lo)

	candidates := make([]int, 0, len(set))
	for idx := range set {
		candidates = append(candidates, idx)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(candidates)))
	return candidates
}

func firstParentChain(repo *git.Repository, tip plumbing.Hash) ([]plumbing.Hash, error) {
	commit, err := repo.CommitObject(tip)
	if err != nil {
		return nil, err
	}
	reversed := make([]plumbing.Hash, 0, 128)
	for {
		reversed = append(reversed, commit.Hash)
		if len(commit.ParentHashes) == 0 {
			break
		}
		commit, err = repo.CommitObject(commit.ParentHashes[0])
		if err != nil {
			return nil, err
		}
	}

	chain := make([]plumbing.Hash, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		chain = append(chain, reversed[i])
	}
	return chain, nil
}

func sourcePackExceedsLimit(
	ctx context.Context,
	sourceConn *transportConn,
	sourceService *sourceRefService,
	ref desiredRef,
	want plumbing.Hash,
	have plumbing.Hash,
	limit int64,
) (bool, error) {
	packReader, err := sourceService.FetchPack(ctx, sourceConn, singleDesiredRef(ref.SourceRef, ref.TargetRef, want), singleHaveMap(have))
	if err != nil {
		return false, err
	}
	defer packReader.Close()
	_, err = io.Copy(io.Discard, limitPackReadCloser(packReader, limit))
	if err == nil {
		return false, nil
	}
	if strings.Contains(err.Error(), "source pack exceeded max-pack-bytes limit") {
		return true, nil
	}
	return false, err
}

func bootstrapResumeIndex(checkpoints []plumbing.Hash, resumeHash plumbing.Hash) (int, error) {
	if resumeHash.IsZero() {
		return 0, nil
	}
	for idx, checkpoint := range checkpoints {
		if checkpoint == resumeHash {
			return idx + 1, nil
		}
	}
	return 0, fmt.Errorf("temp ref hash %s does not match any planned checkpoint", resumeHash)
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
	if desiredHasTag(desired) && sourceAdv.Capabilities.Supports(capability.IncludeTag) {
		_ = req.Capabilities.Set(capability.IncludeTag)
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
	if desiredHasTag(desired) && sourceAdv.Capabilities.Supports(capability.IncludeTag) {
		_ = req.Capabilities.Set(capability.IncludeTag)
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

func desiredHasTag(desired map[plumbing.ReferenceName]desiredRef) bool {
	for _, ref := range desired {
		if ref.Kind == RefKindTag {
			return true
		}
	}
	return false
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
		cmd := &packp.Command{
			Name: plan.TargetRef,
			Old:  plan.TargetHash,
		}
		switch plan.Action {
		case ActionCreate, ActionUpdate:
			cmd.New = plan.SourceHash
		default:
			return fmt.Errorf("streamed pack push only supports create and update actions")
		}
		commands = append(commands, cmd)
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

func pushCommandsToTarget(
	ctx context.Context,
	conn *transportConn,
	targetAdv *packp.AdvRefs,
	plans []BranchPlan,
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
	hasDelete := false
	for _, plan := range plans {
		cmd := &packp.Command{
			Name: plan.TargetRef,
			Old:  plan.TargetHash,
		}
		switch plan.Action {
		case ActionCreate, ActionUpdate:
			cmd.New = plan.SourceHash
		case ActionDelete:
			cmd.New = plumbing.ZeroHash
			hasDelete = true
		default:
			return fmt.Errorf("command-only target push does not support %s", plan.Action)
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

	report, err := receivePack(ctx, session, nil, req, nil, false, false)
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

func startMeasurement(enabled bool) func() Measurement {
	if !enabled {
		return func() Measurement { return Measurement{} }
	}

	start := time.Now()
	var startStats runtime.MemStats
	runtime.ReadMemStats(&startStats)

	done := make(chan struct{})
	var (
		mu            sync.Mutex
		peakAlloc     = startStats.Alloc
		peakHeapInuse = startStats.HeapInuse
		result        Measurement
	)

	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				var current runtime.MemStats
				runtime.ReadMemStats(&current)
				mu.Lock()
				if current.Alloc > peakAlloc {
					peakAlloc = current.Alloc
				}
				if current.HeapInuse > peakHeapInuse {
					peakHeapInuse = current.HeapInuse
				}
				mu.Unlock()
			}
		}
	}()

	var once sync.Once
	return func() Measurement {
		once.Do(func() {
			close(done)
			var endStats runtime.MemStats
			runtime.ReadMemStats(&endStats)
			mu.Lock()
			if endStats.Alloc > peakAlloc {
				peakAlloc = endStats.Alloc
			}
			if endStats.HeapInuse > peakHeapInuse {
				peakHeapInuse = endStats.HeapInuse
			}
			result = Measurement{
				Enabled:            true,
				ElapsedMillis:      time.Since(start).Milliseconds(),
				PeakAllocBytes:     peakAlloc,
				PeakHeapInuseBytes: peakHeapInuse,
				TotalAllocBytes:    endStats.TotalAlloc - startStats.TotalAlloc,
				GCCount:            endStats.NumGC - startStats.NumGC,
			}
			mu.Unlock()
		})
		return result
	}
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

	baseTransport := http.DefaultTransport
	if raw.SkipTLSVerify {
		if cloned, ok := http.DefaultTransport.(*http.Transport); ok {
			transportClone := cloned.Clone()
			if transportClone.TLSClientConfig == nil {
				transportClone.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			}
			transportClone.TLSClientConfig.InsecureSkipVerify = true
			baseTransport = transportClone
		}
	}

	httpClient := &http.Client{
		Transport: &countingRoundTripper{
			base:  baseTransport,
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

func copyRefHashMap(input map[plumbing.ReferenceName]plumbing.Hash) map[plumbing.ReferenceName]plumbing.Hash {
	out := make(map[plumbing.ReferenceName]plumbing.Hash, len(input))
	for name, hash := range input {
		out[name] = hash
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

func progressf(verbose bool, format string, args ...interface{}) {
	if !verbose {
		return
	}
	fmt.Fprintf(os.Stderr, "[git-sync] %s\n", fmt.Sprintf(format, args...))
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
	if username, password, ok := lookupEntireDBCredential(raw, ep); ok {
		return &transporthttp.BasicAuth{Username: username, Password: password}, nil
	}
	username, password, ok := lookupGitCredential(ep)
	if !ok {
		return nil, nil
	}
	return &transporthttp.BasicAuth{Username: username, Password: password}, nil
}

func lookupEntireDBCredential(raw Endpoint, ep *transport.Endpoint) (string, string, bool) {
	if ep == nil || ep.Host == "" {
		return "", "", false
	}
	credHost := endpointCredentialHost(ep)
	token, ok := lookupEntireDBToken(credHost, endpointBaseURL(ep), raw.SkipTLSVerify)
	if !ok || token == "" {
		return "", "", false
	}
	username := raw.Username
	if username == "" {
		username = "git"
	}
	return username, token, true
}

func endpointBaseURL(ep *transport.Endpoint) string {
	if ep == nil || ep.Host == "" {
		return ""
	}
	scheme := ep.Protocol
	if scheme == "" {
		scheme = "https"
	}
	host := ep.Host
	if ep.Port > 0 {
		host = fmt.Sprintf("%s:%d", host, ep.Port)
	}
	return scheme + "://" + host
}

func endpointCredentialHost(ep *transport.Endpoint) string {
	if ep == nil {
		return ""
	}
	if ep.Port > 0 {
		return fmt.Sprintf("%s:%d", ep.Host, ep.Port)
	}
	return ep.Host
}

func lookupEntireDBToken(host, baseURL string, skipTLSVerify bool) (string, bool) {
	configDir := os.Getenv("ENTIRE_CONFIG_DIR")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		configDir = filepath.Join(home, ".config", "entire")
	}

	username, ok := loadEntireDBActiveUser(host, configDir)
	if !ok || username == "" {
		return "", false
	}
	token, err := getEntireDBTokenWithRefresh(context.Background(), host, username, baseURL, skipTLSVerify)
	if err != nil {
		return "", false
	}
	if token == "" {
		return "", false
	}
	return token, true
}

func loadEntireDBActiveUser(host, configDir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(configDir, "hosts.json"))
	if err != nil {
		return "", false
	}
	var hosts map[string]*entireAuthHostInfo
	if err := json.Unmarshal(data, &hosts); err != nil {
		return "", false
	}
	info := hosts[host]
	if info == nil || info.ActiveUser == "" {
		return "", false
	}
	return info.ActiveUser, true
}

func getEntireDBTokenWithRefresh(ctx context.Context, host, username, baseURL string, skipTLSVerify bool) (string, error) {
	encodedToken, err := readEntireDBStoredToken(entireCredentialService(host), username)
	if err != nil {
		return "", err
	}
	token, expiresAt := decodeTokenWithExpiration(encodedToken)
	if token == "" {
		return "", nil
	}
	if !tokenExpiredOrExpiring(expiresAt) {
		return token, nil
	}
	refreshed, err := refreshEntireDBAccessToken(ctx, host, username, baseURL, skipTLSVerify)
	if err != nil {
		return token, nil
	}
	return refreshed, nil
}

func decodeTokenWithExpiration(encoded string) (string, time.Time) {
	idx := strings.LastIndex(encoded, "|")
	if idx == -1 {
		return encoded, time.Time{}
	}
	token := encoded[:idx]
	expiresAtUnix, err := strconv.ParseInt(encoded[idx+1:], 10, 64)
	if err != nil {
		return encoded, time.Time{}
	}
	return token, time.Unix(expiresAtUnix, 0)
}

func tokenExpiredOrExpiring(expiresAt time.Time) bool {
	if expiresAt.IsZero() {
		return true
	}
	return time.Now().Add(5 * time.Minute).After(expiresAt)
}

func refreshEntireDBAccessToken(ctx context.Context, host, username, baseURL string, skipTLSVerify bool) (string, error) {
	refreshToken, err := readEntireDBStoredToken(entireCredentialService(host)+":refresh", username)
	if err != nil {
		return "", err
	}
	if refreshToken == "" || baseURL == "" {
		return "", errors.New("missing refresh token or base url")
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", entireCLIClientID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: skipTLSVerify}, //nolint:gosec
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("refresh failed with status %d", resp.StatusCode)
	}

	var tokenResp oauthTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", err
	}
	if tokenResp.AccessToken == "" {
		return "", errors.New("empty access token in refresh response")
	}

	if err := writeEntireDBStoredToken(
		entireCredentialService(host),
		username,
		encodeTokenWithExpiration(tokenResp.AccessToken, tokenResp.ExpiresIn),
	); err != nil {
		return "", err
	}
	if tokenResp.RefreshToken != "" {
		_ = writeEntireDBStoredToken(entireCredentialService(host)+":refresh", username, tokenResp.RefreshToken)
	}
	return tokenResp.AccessToken, nil
}

func encodeTokenWithExpiration(token string, expiresIn int64) string {
	return fmt.Sprintf("%s|%d", token, time.Now().Unix()+expiresIn)
}

func entireCredentialService(host string) string {
	return "entire:" + host
}

func readEntireDBStoredToken(service, username string) (string, error) {
	if os.Getenv("ENTIRE_TOKEN_STORE") == "file" {
		path := os.Getenv("ENTIRE_TOKEN_STORE_PATH")
		if path == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			path = filepath.Join(home, ".config", "entiredb", "tokens.json")
		}
		return readEntireDBFileToken(path, service, username)
	}
	return keyring.Get(service, username)
}

func writeEntireDBStoredToken(service, username, password string) error {
	if os.Getenv("ENTIRE_TOKEN_STORE") == "file" {
		path := os.Getenv("ENTIRE_TOKEN_STORE_PATH")
		if path == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			path = filepath.Join(home, ".config", "entiredb", "tokens.json")
		}
		return writeEntireDBFileToken(path, service, username, password)
	}
	return keyring.Set(service, username, password)
}

func readEntireDBFileToken(path, service, username string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", keyring.ErrNotFound
		}
		return "", err
	}
	var store map[string]map[string]string
	if err := json.Unmarshal(data, &store); err != nil {
		return "", err
	}
	users := store[service]
	if users == nil {
		return "", keyring.ErrNotFound
	}
	password, ok := users[username]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return password, nil
}

func writeEntireDBFileToken(path, service, username, password string) error {
	store := map[string]map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &store); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if store[service] == nil {
		store[service] = map[string]string{}
	}
	store[service][username] = password
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(store)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
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
