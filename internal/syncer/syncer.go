// Package syncer provides the top-level orchestration for git-sync.
// It delegates to internal/gitproto for protocol, internal/planner for
// planning, and internal/auth for credentials.
package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage/memory"

	"github.com/soph/git-sync/internal/auth"
	"github.com/soph/git-sync/internal/convert"
	"github.com/soph/git-sync/internal/gitproto"
	"github.com/soph/git-sync/internal/planner"
	bstrap "github.com/soph/git-sync/internal/strategy/bootstrap"
	"github.com/soph/git-sync/internal/strategy/incremental"
	"github.com/soph/git-sync/internal/strategy/materialized"
	repstrat "github.com/soph/git-sync/internal/strategy/replicate"
	"github.com/soph/git-sync/internal/validation"
)

const (
	protocolModeAuto = validation.ProtocolAuto
	protocolModeV1   = validation.ProtocolV1
	protocolModeV2   = validation.ProtocolV2
	modeSync         = "sync"
	modeReplicate    = "replicate"
)

const DefaultMaterializedMaxObjects = materialized.DefaultMaxMaterializedObjects

// Endpoint holds the connection configuration for a remote.
type Endpoint struct {
	URL           string
	Username      string
	Token         string
	BearerToken   string
	SkipTLSVerify bool
}

// RefMapping is a user-specified source:target ref mapping.
type RefMapping = validation.RefMapping

// Config holds all configuration for a sync operation.
type Config struct {
	Source                 Endpoint
	Target                 Endpoint
	HTTPClient             *http.Client
	Branches               []string
	Mappings               []RefMapping
	IncludeTags            bool
	DryRun                 bool
	Verbose                bool
	ShowStats              bool
	MeasureMemory          bool
	Mode                   string
	Force                  bool
	Prune                  bool
	MaxPackBytes           int64
	BatchMaxPackBytes      int64
	MaterializedMaxObjects int
	ProtocolMode           string
}

// Re-export types from planner for CLI compatibility.
type (
	RefKind    = planner.RefKind
	Action     = planner.Action
	BranchPlan = planner.BranchPlan
)

const (
	RefKindBranch = planner.RefKindBranch
	RefKindTag    = planner.RefKindTag
	ActionCreate  = planner.ActionCreate
	ActionUpdate  = planner.ActionUpdate
	ActionDelete  = planner.ActionDelete
	ActionSkip    = planner.ActionSkip
	ActionBlock   = planner.ActionBlock
)

type RefInfo struct {
	Name string        `json:"name"`
	Hash plumbing.Hash `json:"hash"`
}

func (r RefInfo) MarshalJSON() ([]byte, error) {
	type ri struct {
		Name string `json:"name"`
		Hash string `json:"hash"`
	}
	return json.Marshal(ri{Name: r.Name, Hash: r.Hash.String()})
}

// Result holds the outcome of a sync or bootstrap operation.
type Result struct {
	Plans              []BranchPlan `json:"plans"`
	Pushed             int          `json:"pushed"`
	Skipped            int          `json:"skipped"`
	Blocked            int          `json:"blocked"`
	Deleted            int          `json:"deleted"`
	DryRun             bool         `json:"dry_run"`
	OperationMode      string       `json:"operation_mode"`
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

func (r Result) Lines() []string {
	lines := make([]string, 0, len(r.Plans)+8)
	for _, plan := range r.Plans {
		lines = append(lines, planner.FormatPlanLine(plan))
	}
	summary := fmt.Sprintf(
		"summary: pushed=%d deleted=%d skipped=%d blocked=%d mode=%s protocol=%s relay=%t relay-mode=%s relay-reason=%s batching=%t batch-count=%d planned-batches=%d",
		r.Pushed, r.Deleted, r.Skipped, r.Blocked, r.OperationMode, r.Protocol, r.Relay, r.RelayMode, r.RelayReason, r.Batching, r.BatchCount, r.PlannedBatchCount,
	)
	if r.DryRun {
		summary += " dry-run=true"
	}
	lines = append(lines, summary)
	lines = append(lines, statsLines(r.Stats)...)
	lines = append(lines, measurementLine(r.Measurement)...)
	if r.BootstrapSuggested {
		lines = append(lines, "hint: target refs are absent; bootstrap can seed them without local object storage")
	}
	if r.Batching && len(r.TempRefs) > 0 {
		lines = append(lines, fmt.Sprintf("batching: temp-refs=%s", strings.Join(r.TempRefs, ",")))
	}
	return lines
}

// ProbeResult holds the outcome of a probe operation.
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
	lines = append(lines, statsLines(r.Stats)...)
	lines = append(lines, measurementLine(r.Measurement)...)
	return lines
}

// FetchResult holds the outcome of a fetch operation.
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

func (r FetchResult) MarshalJSON() ([]byte, error) {
	type fr struct {
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
	for _, h := range r.Haves {
		haves = append(haves, h.String())
	}
	return json.Marshal(fr{
		SourceURL: r.SourceURL, RequestedMode: r.RequestedMode,
		Protocol: r.Protocol, Wants: r.Wants, Haves: haves,
		FetchedObjects: r.FetchedObjects, Stats: r.Stats, Measurement: r.Measurement,
	})
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
	for _, w := range r.Wants {
		lines = append(lines, fmt.Sprintf("want: %s %s", w.Hash.String(), w.Name))
	}
	for _, h := range r.Haves {
		lines = append(lines, fmt.Sprintf("have: %s", h.String()))
	}
	lines = append(lines, statsLines(r.Stats)...)
	lines = append(lines, measurementLine(r.Measurement)...)
	return lines
}

func statsLines(s Stats) []string {
	if !s.Enabled {
		return nil
	}
	keys := make([]string, 0, len(s.Items))
	for k := range s.Items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lines []string
	for _, k := range keys {
		item := s.Items[k]
		lines = append(lines, fmt.Sprintf(
			"stats: %s requests=%d request-bytes=%d response-bytes=%d wants=%d haves=%d commands=%d",
			item.Name, item.Requests, item.RequestBytes, item.ResponseBytes, item.Wants, item.Haves, item.Commands,
		))
	}
	return lines
}

func measurementLine(m Measurement) []string {
	if !m.Enabled {
		return nil
	}
	return []string{fmt.Sprintf(
		"measurement: elapsed-ms=%d peak-alloc-bytes=%d peak-heap-inuse-bytes=%d total-alloc-bytes=%d gc-count=%d",
		m.ElapsedMillis, m.PeakAllocBytes, m.PeakHeapInuseBytes, m.TotalAllocBytes, m.GCCount,
	)}
}

// --- Session setup ---

func newConn(raw Endpoint, label string, stats *statsCollector, httpClient *http.Client) (*gitproto.Conn, error) {
	ep, err := transport.NewEndpoint(raw.URL)
	if err != nil {
		return nil, err
	}
	authEp := auth.Endpoint{
		Username:      raw.Username,
		Token:         raw.Token,
		BearerToken:   raw.BearerToken,
		SkipTLSVerify: raw.SkipTLSVerify,
	}
	authMethod, err := auth.Resolve(authEp, ep)
	if err != nil {
		return nil, err
	}
	client := instrumentHTTPClient(httpClient, raw.SkipTLSVerify, label, stats)
	return gitproto.NewConnWithHTTPClient(ep, label, authMethod, client), nil
}

func instrumentHTTPClient(base *http.Client, skipTLS bool, label string, stats *statsCollector) *http.Client {
	if base == nil {
		base = &http.Client{Transport: gitproto.NewHTTPTransport(skipTLS)}
	}
	clone := *base
	baseRT := clone.Transport
	if baseRT == nil {
		baseRT = gitproto.NewHTTPTransport(skipTLS)
	}
	clone.Transport = &countingRoundTripper{base: baseRT, label: label, stats: stats}
	return &clone
}

func planConfig(cfg Config) planner.PlanConfig {
	return planner.PlanConfig{
		Branches:    cfg.Branches,
		Mappings:    cfg.Mappings,
		IncludeTags: cfg.IncludeTags,
		Force:       cfg.Force,
		Prune:       cfg.Prune,
	}
}

// --- Session setup (issue #12) ---

// syncSession holds the shared state for a sync operation, reducing
// setup duplication across Run, Bootstrap, Probe, and Fetch.
type syncSession struct {
	cfg             Config
	stats           *statsCollector
	logger          *slog.Logger
	sourceConn      *gitproto.Conn
	sourceService   *gitproto.RefService
	sourceRefMap    map[plumbing.ReferenceName]plumbing.Hash
	target          *targetSession
	measurementDone func() Measurement
}

type targetSession struct {
	conn     *gitproto.Conn
	adv      *packp.AdvRefs
	refMap   map[plumbing.ReferenceName]plumbing.Hash
	features gitproto.TargetFeatures
	policy   planner.RelayTargetPolicy
	pusher   gitproto.Pusher
}

// newSession performs the shared setup: protocol validation, mapping validation,
// connection creation, and ref discovery.
func newSession(ctx context.Context, cfg Config, needTarget bool) (*syncSession, error) {
	mode, err := validation.NormalizeProtocolMode(cfg.ProtocolMode)
	if err != nil {
		return nil, err
	}
	cfg.ProtocolMode = mode
	switch cfg.Mode {
	case "", modeSync:
		cfg.Mode = modeSync
	case modeReplicate:
	default:
		return nil, fmt.Errorf("unsupported operation mode %q", cfg.Mode)
	}
	if _, err := validation.ValidateMappings(cfg.Mappings); err != nil {
		return nil, err
	}
	if cfg.Mode == modeReplicate && cfg.Force {
		return nil, fmt.Errorf("replicate does not support --force; use sync instead")
	}

	s := &syncSession{
		cfg:             cfg,
		stats:           newStats(cfg.ShowStats),
		measurementDone: startMeasurement(cfg.MeasureMemory),
	}
	if cfg.Verbose {
		s.logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}))
	}

	s.sourceConn, err = newConn(cfg.Source, "source", s.stats, cfg.HTTPClient)
	if err != nil {
		return nil, fmt.Errorf("create source transport: %w", err)
	}

	refPrefixes := planner.RefPrefixes(cfg.Mappings, cfg.IncludeTags)
	sourceRefs, sourceService, err := gitproto.ListSourceRefs(ctx, s.sourceConn, cfg.ProtocolMode, refPrefixes)
	if err != nil {
		return nil, fmt.Errorf("list source refs: %w", err)
	}
	s.sourceService = sourceService
	s.sourceRefMap = gitproto.RefHashMap(sourceRefs)

	if needTarget {
		targetConn, err := newConn(cfg.Target, "target", s.stats, cfg.HTTPClient)
		if err != nil {
			return nil, fmt.Errorf("create target transport: %w", err)
		}
		targetAdv, err := gitproto.AdvertisedRefsV1(ctx, targetConn, transport.ReceivePackService)
		if err != nil {
			return nil, fmt.Errorf("list target refs: %w", err)
		}
		targetRefSlice, err := gitproto.AdvRefsToSlice(targetAdv)
		if err != nil {
			return nil, fmt.Errorf("decode target refs: %w", err)
		}
		targetRefMap := gitproto.RefHashMap(targetRefSlice)
		targetFeatures := gitproto.TargetFeaturesFromAdvRefs(targetAdv)
		s.target = &targetSession{
			conn:     targetConn,
			adv:      targetAdv,
			refMap:   targetRefMap,
			features: targetFeatures,
			policy: planner.RelayTargetPolicy{
				CapabilitiesKnown: targetFeatures.Known,
				NoThin:            targetFeatures.NoThin,
			},
			pusher: gitproto.NewPusher(targetConn, targetAdv, cfg.Verbose),
		}
	}

	return s, nil
}

// --- Public API ---

// Run executes a sync or plan operation.
func Run(ctx context.Context, cfg Config) (Result, error) {
	s, err := newSession(ctx, cfg, true)
	if err != nil {
		return Result{}, err
	}
	if s.cfg.Mode == modeReplicate {
		return s.runReplicate(ctx)
	}
	return s.runSync(ctx)
}

func (s *syncSession) runSync(ctx context.Context) (Result, error) {
	measurementDone := s.measurementDone
	stats := s.stats
	sourceService := s.sourceService
	sourceRefMap := s.sourceRefMap
	targetRefMap := s.target.refMap

	desiredRefs, managedTargets, err := planner.BuildDesiredRefs(sourceRefMap, planConfig(s.cfg))
	if err != nil {
		return Result{}, err
	}
	if len(desiredRefs) == 0 {
		return Result{}, fmt.Errorf("no source refs matched")
	}

	// Check for bootstrap opportunity (before allocating in-memory repo)
	if ok, reason := planner.CanBootstrapRelay(s.cfg.Force, s.cfg.Prune, desiredRefs, targetRefMap); ok {
		if s.cfg.DryRun {
			plans, err := planner.BuildBootstrapPlans(desiredRefs, targetRefMap)
			if err != nil {
				return Result{}, err
			}
			return Result{
				Plans: plans, DryRun: true, RelayReason: reason,
				OperationMode: modeSync, BootstrapSuggested: true, Stats: stats.snapshot(),
				Measurement: measurementDone(), Protocol: sourceService.Protocol,
			}, nil
		}
		return bootstrapWithInputs(ctx, s, desiredRefs, targetRefMap, reason)
	}

	// Normal sync: allocate in-memory repo and fetch objects
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		return Result{}, fmt.Errorf("init in-memory repository: %w", err)
	}
	gpDesired := convert.DesiredRefs(desiredRefs)
	if err := sourceService.FetchToStore(ctx, repo.Storer, s.sourceConn, gpDesired, targetRefMap); err != nil {
		if !errors.Is(err, git.NoErrAlreadyUpToDate) {
			return Result{}, err
		}
	}

	plans, err := planner.BuildPlans(repo.Storer, desiredRefs, targetRefMap, managedTargets, planConfig(s.cfg))
	if err != nil {
		return Result{}, err
	}

	result := Result{
		Plans: plans, DryRun: s.cfg.DryRun, OperationMode: modeSync, Protocol: sourceService.Protocol,
		Stats: stats.snapshot(), Measurement: measurementDone(),
	}

	pushPlans := make([]BranchPlan, 0, len(plans))
	for _, plan := range plans {
		switch plan.Action {
		case ActionCreate, ActionUpdate, ActionDelete:
			if s.cfg.DryRun {
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

	if !s.cfg.DryRun && result.Blocked > 0 {
		return result, fmt.Errorf("blocked %d ref update(s); rerun with --force where appropriate", result.Blocked)
	}
	result.RelayReason = planner.RelayFallbackReason(s.cfg.Force, s.cfg.Prune, s.cfg.DryRun, pushPlans, s.target.policy)

	if !s.cfg.DryRun {
		// Try incremental relay first
		incResult, err := s.executeIncremental(ctx, desiredRefs, pushPlans)
		if err != nil {
			return result, err
		}
		if incResult.Relay {
			result.Relay = incResult.Relay
			result.RelayMode = incResult.RelayMode
			result.RelayReason = incResult.RelayReason
		} else if len(pushPlans) > 0 {
			// Materialized fallback
			if err := s.executeMaterialized(ctx, repo.Storer, desiredRefs, pushPlans); err != nil {
				return result, err
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

func (s *syncSession) runReplicate(ctx context.Context) (Result, error) {
	desiredRefs, managedTargets, err := planner.BuildDesiredRefs(s.sourceRefMap, planConfig(s.cfg))
	if err != nil {
		return Result{}, err
	}
	if len(desiredRefs) == 0 {
		return Result{}, fmt.Errorf("no source refs matched")
	}

	if ok, reason := planner.SupportsReplicateRelay(s.target.policy); !ok {
		return Result{OperationMode: modeReplicate}, fmt.Errorf("replicate requires relay-capable target: %s; use sync instead", reason)
	}

	allAbsent := s.replicateCanBootstrap(desiredRefs)
	if allAbsent {
		if s.cfg.DryRun {
			plans, err := planner.BuildBootstrapPlans(desiredRefs, s.target.refMap)
			if err != nil {
				return Result{}, err
			}
			return Result{
				Plans:              plans,
				DryRun:             true,
				OperationMode:      modeReplicate,
				RelayReason:        "empty-target-managed-refs",
				BootstrapSuggested: true,
				Stats:              s.stats.snapshot(),
				Measurement:        s.measurementDone(),
				Protocol:           s.sourceService.Protocol,
			}, nil
		}
		return bootstrapWithInputs(ctx, s, desiredRefs, s.target.refMap, "empty-target-managed-refs")
	}

	plans, err := planner.BuildReplicationPlans(desiredRefs, s.target.refMap, managedTargets, planConfig(s.cfg))
	if err != nil {
		return Result{}, err
	}

	result := Result{
		Plans:         plans,
		DryRun:        s.cfg.DryRun,
		OperationMode: modeReplicate,
		Protocol:      s.sourceService.Protocol,
		Stats:         s.stats.snapshot(),
		Measurement:   s.measurementDone(),
	}

	pushPlans := make([]BranchPlan, 0, len(plans))
	relayPlans := make([]BranchPlan, 0, len(plans))
	for _, plan := range plans {
		switch plan.Action {
		case ActionCreate, ActionUpdate, ActionDelete:
			if s.cfg.DryRun {
				result.Skipped++
				continue
			}
			pushPlans = append(pushPlans, plan)
			if plan.Action != ActionDelete {
				relayPlans = append(relayPlans, plan)
			}
		case ActionSkip:
			result.Skipped++
		case ActionBlock:
			result.Blocked++
		}
	}

	if !s.cfg.DryRun && len(relayPlans) > 0 {
		ok, reason := planner.CanReplicateRelay(relayPlans)
		if !ok {
			return result, fmt.Errorf("replicate requires relay-capable target: %s; use sync instead", reason)
		}
		repResult, err := s.executeReplicate(ctx, desiredRefs, pushPlans)
		if err != nil {
			return result, fmt.Errorf("replicate relay failed: %w; use sync instead", err)
		}
		result.Relay = repResult.Relay
		result.RelayMode = repResult.RelayMode
		result.RelayReason = repResult.RelayReason
	}

	for _, plan := range pushPlans {
		switch plan.Action {
		case ActionCreate, ActionUpdate:
			result.Pushed++
		case ActionDelete:
			result.Deleted++
		}
	}
	result.Stats = s.stats.snapshot()
	result.Measurement = s.measurementDone()
	return result, nil
}

func (s *syncSession) replicateCanBootstrap(desiredRefs map[plumbing.ReferenceName]planner.DesiredRef) bool {
	for targetRef := range desiredRefs {
		if !s.target.refMap[targetRef].IsZero() {
			return false
		}
	}
	if !s.cfg.Prune {
		return true
	}
	for targetRef, hash := range s.target.refMap {
		if hash.IsZero() {
			continue
		}
		if _, ok := desiredRefs[targetRef]; ok {
			continue
		}
		switch {
		case targetRef.IsTag() && s.cfg.IncludeTags:
			return false
		case targetRef.IsBranch() && len(s.cfg.Mappings) == 0 && len(s.cfg.Branches) == 0:
			return false
		}
	}
	return true
}

// Bootstrap seeds an empty target with relay behavior.
func Bootstrap(ctx context.Context, cfg Config) (Result, error) {
	if cfg.Force {
		return Result{}, fmt.Errorf("bootstrap does not support --force")
	}
	if cfg.Prune {
		return Result{}, fmt.Errorf("bootstrap does not support --prune")
	}
	if cfg.DryRun {
		return Result{}, fmt.Errorf("bootstrap does not support dry-run; use plan or sync")
	}

	s, err := newSession(ctx, cfg, true)
	if err != nil {
		return Result{}, err
	}

	desiredRefs, _, err := planner.BuildDesiredRefs(s.sourceRefMap, planConfig(cfg))
	if err != nil {
		return Result{}, err
	}
	if len(desiredRefs) == 0 {
		return Result{}, fmt.Errorf("no source refs matched")
	}

	_, reason := planner.CanBootstrapRelay(cfg.Force, cfg.Prune, desiredRefs, s.target.refMap)
	result, err := bootstrapWithInputs(ctx, s, desiredRefs, s.target.refMap, reason)
	result.Measurement = s.measurementDone()
	return result, err
}

// Probe inspects source and optionally target remotes.
func Probe(ctx context.Context, cfg Config) (ProbeResult, error) {
	if cfg.Source.URL == "" {
		return ProbeResult{}, fmt.Errorf("source repository URL is required")
	}

	s, err := newSession(ctx, cfg, cfg.Target.URL != "")
	if err != nil {
		return ProbeResult{}, err
	}
	return s.newProbeResult(), nil
}

// Fetch exercises source-side fetch negotiation.
func Fetch(ctx context.Context, cfg Config, haveRefs []string, haveHashes []plumbing.Hash) (FetchResult, error) {
	if cfg.Source.URL == "" {
		return FetchResult{}, fmt.Errorf("source repository URL is required")
	}

	s, err := newSession(ctx, cfg, false)
	if err != nil {
		return FetchResult{}, err
	}

	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		return FetchResult{}, fmt.Errorf("init in-memory repository: %w", err)
	}
	desiredRefs, err := s.buildDesiredRefs()
	if err != nil {
		return FetchResult{}, err
	}
	targetRefMap, err := s.buildHaveRefMap(haveRefs, haveHashes)
	if err != nil {
		return FetchResult{}, err
	}

	gpDesired := convert.DesiredRefs(desiredRefs)
	if err := s.sourceService.FetchToStore(ctx, repo.Storer, s.sourceConn, gpDesired, targetRefMap); err != nil {
		if !errors.Is(err, git.NoErrAlreadyUpToDate) {
			return FetchResult{}, err
		}
	}

	objectCount, err := countObjects(repo.Storer)
	if err != nil {
		return FetchResult{}, fmt.Errorf("count fetched objects: %w", err)
	}
	return s.newFetchResult(objectCount, desiredRefs, targetRefMap), nil
}

// --- Bootstrap implementation ---

func bootstrapWithInputs(
	ctx context.Context,
	s *syncSession,
	desiredRefs map[plumbing.ReferenceName]planner.DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
	relayReason string,
) (Result, error) {
	bResult, err := bstrap.Execute(ctx, bstrap.Params{
		SourceConn: s.sourceConn, SourceService: s.sourceService, TargetPusher: s.target.pusher,
		DesiredRefs: desiredRefs, TargetRefs: targetRefs,
		MaxPackBytes: s.cfg.MaxPackBytes, BatchMaxPack: s.cfg.BatchMaxPackBytes,
		Verbose: s.cfg.Verbose, Logger: s.logger,
	}, relayReason)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Plans: bResult.Plans, Pushed: bResult.Pushed, OperationMode: s.cfg.Mode,
		Relay: bResult.Relay, RelayMode: bResult.RelayMode, RelayReason: bResult.RelayReason,
		Batching: bResult.Batching, BatchCount: bResult.BatchCount,
		PlannedBatchCount: bResult.PlannedBatchCount, TempRefs: bResult.TempRefs,
		Stats: s.stats.snapshot(), Measurement: s.measurementDone(), Protocol: s.sourceService.Protocol,
	}, nil
}

func (s *syncSession) executeIncremental(
	ctx context.Context,
	desiredRefs map[plumbing.ReferenceName]planner.DesiredRef,
	pushPlans []planner.BranchPlan,
) (incremental.Result, error) {
	return incremental.Execute(ctx, incremental.Params{
		SourceConn: s.sourceConn, SourceService: s.sourceService, TargetPusher: s.target.pusher,
		DesiredRefs: desiredRefs, TargetRefs: s.target.refMap,
		PushPlans: pushPlans, MaxPackBytes: s.cfg.MaxPackBytes,
		CanRelay: func(force, prune, dryRun bool, plans []planner.BranchPlan) (bool, string) {
			return planner.CanIncrementalRelay(force, prune, dryRun, plans, s.target.policy)
		},
		CanTagRelay: planner.CanFullTagCreateRelay,
	}, planConfig(s.cfg))
}

func (s *syncSession) executeMaterialized(
	ctx context.Context,
	store storer.Storer,
	desiredRefs map[plumbing.ReferenceName]planner.DesiredRef,
	pushPlans []planner.BranchPlan,
) error {
	return materialized.Execute(ctx, materialized.Params{
		Store: store, SourceConn: s.sourceConn, SourceService: s.sourceService, TargetPusher: s.target.pusher,
		DesiredRefs: desiredRefs, TargetRefs: s.target.refMap,
		PushPlans: pushPlans, MaxObjects: s.cfg.MaterializedMaxObjects,
	})
}

func (s *syncSession) executeReplicate(
	ctx context.Context,
	desiredRefs map[plumbing.ReferenceName]planner.DesiredRef,
	pushPlans []planner.BranchPlan,
) (repstrat.Result, error) {
	return repstrat.Execute(ctx, repstrat.Params{
		SourceConn: s.sourceConn, SourceService: s.sourceService, TargetPusher: s.target.pusher,
		DesiredRefs: desiredRefs, TargetRefs: s.target.refMap,
		PushPlans: pushPlans, MaxPackBytes: s.cfg.MaxPackBytes,
	})
}

func (s *syncSession) buildDesiredRefs() (map[plumbing.ReferenceName]planner.DesiredRef, error) {
	desiredRefs, _, err := planner.BuildDesiredRefs(s.sourceRefMap, planConfig(s.cfg))
	if err != nil {
		return nil, err
	}
	if len(desiredRefs) == 0 {
		return nil, fmt.Errorf("no source refs matched")
	}
	return desiredRefs, nil
}

func (s *syncSession) buildHaveRefMap(haveRefs []string, haveHashes []plumbing.Hash) (map[plumbing.ReferenceName]plumbing.Hash, error) {
	targetRefMap := make(map[plumbing.ReferenceName]plumbing.Hash)
	for _, raw := range haveRefs {
		name := validation.ParseHaveRef(raw)
		hash, ok := s.sourceRefMap[name]
		if !ok {
			return nil, fmt.Errorf("have-ref %q not found on source", raw)
		}
		targetRefMap[name] = hash
	}
	for idx, hash := range haveHashes {
		targetRefMap[plumbing.ReferenceName(fmt.Sprintf("refs/haves/%d", idx))] = hash
	}
	return targetRefMap, nil
}

func (s *syncSession) newProbeResult() ProbeResult {
	refInfos := make([]RefInfo, 0, len(s.sourceRefMap))
	for name, hash := range s.sourceRefMap {
		refInfos = append(refInfos, RefInfo{Name: name.String(), Hash: hash})
	}
	sort.Slice(refInfos, func(i, j int) bool { return refInfos[i].Name < refInfos[j].Name })

	result := ProbeResult{
		SourceURL:     s.cfg.Source.URL,
		RequestedMode: s.cfg.ProtocolMode,
		Protocol:      s.sourceService.Protocol,
		RefPrefixes:   planner.RefPrefixes(s.cfg.Mappings, s.cfg.IncludeTags),
		Capabilities:  s.sourceService.Capabilities(),
		Refs:          refInfos,
		Stats:         s.stats.snapshot(),
		Measurement:   s.measurementDone(),
	}
	if s.target != nil {
		result.TargetURL = s.cfg.Target.URL
		result.TargetCaps = gitproto.AdvRefsCaps(s.target.adv)
		result.Stats = s.stats.snapshot()
		result.Measurement = s.measurementDone()
	}
	return result
}

func (s *syncSession) newFetchResult(
	objectCount int,
	desiredRefs map[plumbing.ReferenceName]planner.DesiredRef,
	targetRefMap map[plumbing.ReferenceName]plumbing.Hash,
) FetchResult {
	wants := make([]RefInfo, 0, len(desiredRefs))
	for _, ref := range desiredRefs {
		wants = append(wants, RefInfo{Name: ref.SourceRef.String(), Hash: ref.SourceHash})
	}
	sort.Slice(wants, func(i, j int) bool { return wants[i].Name < wants[j].Name })

	haveValues := make([]plumbing.Hash, 0, len(targetRefMap))
	for _, h := range targetRefMap {
		if !h.IsZero() {
			haveValues = append(haveValues, h)
		}
	}

	return FetchResult{
		SourceURL:      s.cfg.Source.URL,
		RequestedMode:  s.cfg.ProtocolMode,
		Protocol:       s.sourceService.Protocol,
		Wants:          wants,
		Haves:          gitproto.SortedUniqueHashes(haveValues),
		FetchedObjects: objectCount,
		Stats:          s.stats.snapshot(),
		Measurement:    s.measurementDone(),
	}
}

func countObjects(store storer.EncodedObjectStorer) (int, error) {
	iter, err := store.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	count := 0
	err = iter.ForEach(func(_ plumbing.EncodedObject) error {
		count++
		return nil
	})
	return count, err
}
