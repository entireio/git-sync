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

	"entire.io/entire/git-sync/internal/auth"
	"entire.io/entire/git-sync/internal/convert"
	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
	bstrap "entire.io/entire/git-sync/internal/strategy/bootstrap"
	"entire.io/entire/git-sync/internal/strategy/incremental"
	"entire.io/entire/git-sync/internal/strategy/materialized"
	repstrat "entire.io/entire/git-sync/internal/strategy/replicate"
	"entire.io/entire/git-sync/internal/validation"
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

	// FollowInfoRefsRedirect, when true, rewrites the effective host of
	// this endpoint to the final URL of /info/refs after HTTP redirects,
	// so follow-up upload-pack / receive-pack POSTs land on the
	// redirected node. Plumbed into gitproto.Conn.FollowInfoRefsRedirect.
	FollowInfoRefsRedirect bool
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
	TargetMaxPackBytes     int64
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
	b, err := json.Marshal(ri{Name: r.Name, Hash: r.Hash.String()})
	if err != nil {
		return nil, fmt.Errorf("marshal JSON: %w", err)
	}
	return b, nil
}

// Result holds the outcome of a sync or bootstrap operation.
type Result struct {
	Plans              []BranchPlan `json:"plans"`
	Pushed             int          `json:"pushed"`
	Skipped            int          `json:"skipped"`
	Blocked            int          `json:"blocked"`
	Deleted            int          `json:"deleted"`
	DryRun             bool         `json:"dryRun"`
	OperationMode      string       `json:"operationMode"`
	Relay              bool         `json:"relay"`
	RelayMode          string       `json:"relayMode"`
	RelayReason        string       `json:"relayReason"`
	Batching           bool         `json:"batching"`
	BatchCount         int          `json:"batchCount"`
	PlannedBatchCount  int          `json:"plannedBatchCount"`
	TempRefs           []string     `json:"tempRefs"`
	BootstrapSuggested bool         `json:"bootstrapSuggested"`
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
		lines = append(lines, "batching: temp-refs="+strings.Join(r.TempRefs, ","))
	}
	return lines
}

// ProbeResult holds the outcome of a probe operation.
type ProbeResult struct {
	SourceURL     string      `json:"sourceUrl"`
	TargetURL     string      `json:"targetUrl,omitempty"`
	RequestedMode string      `json:"requestedMode"`
	Protocol      string      `json:"protocol"`
	RefPrefixes   []string    `json:"refPrefixes"`
	Capabilities  []string    `json:"sourceCapabilities"`
	TargetCaps    []string    `json:"targetCapabilities,omitempty"`
	Refs          []RefInfo   `json:"refs"`
	Stats         Stats       `json:"stats"`
	Measurement   Measurement `json:"measurement"`
}

func (r ProbeResult) Lines() []string {
	lines := []string{
		"source: " + r.SourceURL,
		"requested-protocol: " + r.RequestedMode,
		"negotiated-protocol: " + r.Protocol,
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
	SourceURL      string          `json:"sourceUrl"`
	RequestedMode  string          `json:"requestedMode"`
	Protocol       string          `json:"protocol"`
	Wants          []RefInfo       `json:"wants"`
	Haves          []plumbing.Hash `json:"haves"`
	FetchedObjects int             `json:"fetchedObjects"`
	Stats          Stats           `json:"stats"`
	Measurement    Measurement     `json:"measurement"`
}

func (r FetchResult) MarshalJSON() ([]byte, error) {
	type fr struct {
		SourceURL      string      `json:"sourceUrl"`
		RequestedMode  string      `json:"requestedMode"`
		Protocol       string      `json:"protocol"`
		Wants          []RefInfo   `json:"wants"`
		Haves          []string    `json:"haves"`
		FetchedObjects int         `json:"fetchedObjects"`
		Stats          Stats       `json:"stats"`
		Measurement    Measurement `json:"measurement"`
	}
	haves := make([]string, 0, len(r.Haves))
	for _, h := range r.Haves {
		haves = append(haves, h.String())
	}
	b, err := json.Marshal(fr{
		SourceURL: r.SourceURL, RequestedMode: r.RequestedMode,
		Protocol: r.Protocol, Wants: r.Wants, Haves: haves,
		FetchedObjects: r.FetchedObjects, Stats: r.Stats, Measurement: r.Measurement,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal JSON: %w", err)
	}
	return b, nil
}

func (r FetchResult) Lines() []string {
	lines := []string{
		"source: " + r.SourceURL,
		"requested-protocol: " + r.RequestedMode,
		"negotiated-protocol: " + r.Protocol,
		fmt.Sprintf("wants: %d", len(r.Wants)),
		fmt.Sprintf("haves: %d", len(r.Haves)),
		fmt.Sprintf("fetched-objects: %d", r.FetchedObjects),
	}
	for _, w := range r.Wants {
		lines = append(lines, fmt.Sprintf("want: %s %s", w.Hash.String(), w.Name))
	}
	for _, h := range r.Haves {
		lines = append(lines, "have: "+h.String())
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
	ep, err := transport.ParseURL(raw.URL)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint: %w", err)
	}
	authEp := auth.Endpoint{
		Username:      raw.Username,
		Token:         raw.Token,
		BearerToken:   raw.BearerToken,
		SkipTLSVerify: raw.SkipTLSVerify,
	}
	authMethod, err := auth.Resolve(authEp, ep)
	if err != nil {
		return nil, fmt.Errorf("resolve auth: %w", err)
	}
	client := instrumentHTTPClient(httpClient, raw.SkipTLSVerify, label, stats)
	conn := gitproto.NewConnWithHTTPClient(ep, label, authMethod, client)
	conn.FollowInfoRefsRedirect = raw.FollowInfoRefsRedirect
	return conn, nil
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

// needsLocalSourceClosure reports whether the sync must populate the
// in-memory store with the full source closure before running. The fetch
// is required when:
//   - Force or prune is set: incremental relay is disabled, so the
//     materialized fallback will run and needs the closure.
//   - Any desired ref already exists on target at a different hash: a
//     branch fast-forward check will need ancestry data, or a tag retarget
//     will route to materialized.
//
// When all desired refs are either skips (target hash matches source) or
// creates (target hash is zero), incremental relay handles the push without
// the closure — the upfront fetch would just be wasted bandwidth, since
// relay does its own FetchPack on the source.
func needsLocalSourceClosure(
	cfg Config,
	desired map[plumbing.ReferenceName]planner.DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) bool {
	if cfg.Force || cfg.Prune {
		return true
	}
	for targetRef, want := range desired {
		targetHash := targetRefs[targetRef]
		if targetHash.IsZero() {
			continue
		}
		if targetHash == want.SourceHash {
			continue
		}
		return true
	}
	return false
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
		return nil, fmt.Errorf("normalize protocol mode: %w", err)
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
		return nil, fmt.Errorf("validate mappings: %w", err)
	}
	if cfg.Mode == modeReplicate && cfg.Force {
		return nil, errors.New("replicate does not support --force; use sync instead")
	}
	if needTarget {
		if err := validation.ValidateEndpoints(cfg.Source.URL, cfg.Target.URL); err != nil {
			return nil, fmt.Errorf("validate endpoints: %w", err)
		}
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
	sourceService.Verbose = cfg.Verbose
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
		return Result{}, fmt.Errorf("build desired refs: %w", err)
	}
	if len(desiredRefs) == 0 {
		return Result{}, errors.New("no source refs matched")
	}

	// Check for bootstrap opportunity (before allocating in-memory repo)
	if ok, reason := planner.CanBootstrapRelay(s.cfg.Force, s.cfg.Prune, desiredRefs, targetRefMap); ok {
		if s.cfg.DryRun {
			plans, err := planner.BuildBootstrapPlans(desiredRefs, targetRefMap)
			if err != nil {
				return Result{}, fmt.Errorf("build bootstrap plans: %w", err)
			}
			return Result{
				Plans: plans, DryRun: true, RelayReason: reason,
				OperationMode: modeSync, BootstrapSuggested: true, Stats: stats.snapshot(),
				Measurement: measurementDone(), Protocol: sourceService.Protocol,
			}, nil
		}
		return bootstrapWithInputs(ctx, s, desiredRefs, targetRefMap, reason)
	}

	// Normal sync: allocate in-memory repo. The source closure is fetched
	// lazily — only when planning needs ancestry data (FF detection on a
	// divergent branch) or when the materialized fallback ends up running.
	// Pure skip/create plans that take incremental relay never decode
	// source objects locally, so the upfront fetch would be a wasted
	// full-pack round trip.
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		return Result{}, fmt.Errorf("init in-memory repository: %w", err)
	}
	gpDesired := convert.DesiredRefs(desiredRefs)
	closureFetched := false
	fetchClosure := func() error {
		if closureFetched {
			return nil
		}
		if err := sourceService.FetchToStore(ctx, repo.Storer, s.sourceConn, gpDesired, targetRefMap); err != nil {
			if !errors.Is(err, git.NoErrAlreadyUpToDate) {
				return fmt.Errorf("fetch to store: %w", err)
			}
		}
		closureFetched = true
		return nil
	}
	if needsLocalSourceClosure(s.cfg, desiredRefs, targetRefMap) {
		if err := fetchClosure(); err != nil {
			return Result{}, err
		}
	}

	plans, err := planner.BuildPlans(repo.Storer, desiredRefs, targetRefMap, managedTargets, planConfig(s.cfg))
	if err != nil {
		return Result{}, fmt.Errorf("build plans: %w", err)
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
			// Materialized fallback. needsLocalSourceClosure may have skipped
			// the upfront fetch when relay looked eligible from the plan
			// shape alone, but CanIncrementalRelay can still reject (e.g.
			// when target capabilities are unknown). Fetch on demand so
			// materialized doesn't run against an empty store.
			if err := fetchClosure(); err != nil {
				return result, err
			}
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
		case ActionSkip, ActionBlock:
			// not applicable in this context
		}
	}
	result.Stats = stats.snapshot()
	result.Measurement = measurementDone()
	return result, nil
}

func (s *syncSession) runReplicate(ctx context.Context) (Result, error) {
	desiredRefs, managedTargets, err := planner.BuildDesiredRefs(s.sourceRefMap, planConfig(s.cfg))
	if err != nil {
		return Result{}, fmt.Errorf("build desired refs: %w", err)
	}
	if len(desiredRefs) == 0 {
		return Result{}, errors.New("no source refs matched")
	}

	if ok, reason := planner.SupportsReplicateRelay(s.target.policy); !ok {
		return Result{OperationMode: modeReplicate}, fmt.Errorf("replicate requires relay-capable target: %s; use sync instead", reason)
	}

	allAbsent := s.replicateCanBootstrap(desiredRefs)
	if allAbsent {
		if s.cfg.DryRun {
			plans, err := planner.BuildBootstrapPlans(desiredRefs, s.target.refMap)
			if err != nil {
				return Result{}, fmt.Errorf("build bootstrap plans: %w", err)
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
		return Result{}, fmt.Errorf("build replication plans: %w", err)
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
		case ActionSkip, ActionBlock:
			// not applicable in this context
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
		return Result{}, errors.New("bootstrap does not support --force")
	}
	if cfg.Prune {
		return Result{}, errors.New("bootstrap does not support --prune")
	}
	if cfg.DryRun {
		return Result{}, errors.New("bootstrap does not support dry-run; use plan or sync")
	}

	s, err := newSession(ctx, cfg, true)
	if err != nil {
		return Result{}, err
	}

	desiredRefs, _, err := planner.BuildDesiredRefs(s.sourceRefMap, planConfig(cfg))
	if err != nil {
		return Result{}, fmt.Errorf("build desired refs: %w", err)
	}
	if len(desiredRefs) == 0 {
		return Result{}, errors.New("no source refs matched")
	}

	_, reason := planner.CanBootstrapRelay(cfg.Force, cfg.Prune, desiredRefs, s.target.refMap)
	result, err := bootstrapWithInputs(ctx, s, desiredRefs, s.target.refMap, reason)
	result.Measurement = s.measurementDone()
	return result, err
}

// Probe inspects source and optionally target remotes.
func Probe(ctx context.Context, cfg Config) (ProbeResult, error) {
	if cfg.Source.URL == "" {
		return ProbeResult{}, errors.New("source repository URL is required")
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
		return FetchResult{}, errors.New("source repository URL is required")
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
		return FetchResult{}, fmt.Errorf("build desired refs: %w", err)
	}
	targetRefMap, err := s.buildHaveRefMap(haveRefs, haveHashes)
	if err != nil {
		return FetchResult{}, fmt.Errorf("build have ref map: %w", err)
	}

	gpDesired := convert.DesiredRefs(desiredRefs)
	if err := s.sourceService.FetchToStore(ctx, repo.Storer, s.sourceConn, gpDesired, targetRefMap); err != nil {
		if !errors.Is(err, git.NoErrAlreadyUpToDate) {
			return FetchResult{}, fmt.Errorf("fetch to store: %w", err)
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
		SourceHeadTarget: s.sourceService.HeadTarget,
		MaxPackBytes:     s.cfg.MaxPackBytes, TargetMaxPack: s.cfg.TargetMaxPackBytes,
		Verbose: s.cfg.Verbose, Logger: s.logger,
	}, relayReason)
	if err != nil {
		return Result{}, fmt.Errorf("bootstrap execute: %w", err)
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
	incResult, incErr := incremental.Execute(ctx, incremental.Params{
		SourceConn: s.sourceConn, SourceService: s.sourceService, TargetPusher: s.target.pusher,
		DesiredRefs: desiredRefs, TargetRefs: s.target.refMap,
		PushPlans: pushPlans, MaxPackBytes: s.cfg.MaxPackBytes,
		CanRelay: func(force, prune, dryRun bool, plans []planner.BranchPlan) (bool, string) {
			return planner.CanIncrementalRelay(force, prune, dryRun, plans, s.target.policy)
		},
		CanTagRelay: planner.CanFullTagCreateRelay,
	}, planConfig(s.cfg))
	if incErr != nil {
		return incResult, fmt.Errorf("incremental execute: %w", incErr)
	}
	return incResult, nil
}

func (s *syncSession) executeMaterialized(
	ctx context.Context,
	store storer.Storer,
	desiredRefs map[plumbing.ReferenceName]planner.DesiredRef,
	pushPlans []planner.BranchPlan,
) error {
	if err := materialized.Execute(ctx, materialized.Params{
		Store: store, SourceConn: s.sourceConn, SourceService: s.sourceService, TargetPusher: s.target.pusher,
		DesiredRefs: desiredRefs, TargetRefs: s.target.refMap,
		PushPlans: pushPlans, MaxObjects: s.cfg.MaterializedMaxObjects,
	}); err != nil {
		return fmt.Errorf("materialized execute: %w", err)
	}
	return nil
}

func (s *syncSession) executeReplicate(
	ctx context.Context,
	desiredRefs map[plumbing.ReferenceName]planner.DesiredRef,
	pushPlans []planner.BranchPlan,
) (repstrat.Result, error) {
	repResult, repErr := repstrat.Execute(ctx, repstrat.Params{
		SourceConn: s.sourceConn, SourceService: s.sourceService, TargetPusher: s.target.pusher,
		DesiredRefs: desiredRefs, TargetRefs: s.target.refMap,
		PushPlans: pushPlans, MaxPackBytes: s.cfg.MaxPackBytes,
	})
	if repErr != nil {
		return repResult, fmt.Errorf("replicate execute: %w", repErr)
	}
	return repResult, nil
}

func (s *syncSession) buildDesiredRefs() (map[plumbing.ReferenceName]planner.DesiredRef, error) {
	desiredRefs, _, err := planner.BuildDesiredRefs(s.sourceRefMap, planConfig(s.cfg))
	if err != nil {
		return nil, fmt.Errorf("build desired refs: %w", err)
	}
	if len(desiredRefs) == 0 {
		return nil, errors.New("no source refs matched")
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
		return 0, fmt.Errorf("iterate encoded objects: %w", err)
	}
	defer iter.Close()
	count := 0
	if err := iter.ForEach(func(_ plumbing.EncodedObject) error {
		count++
		return nil
	}); err != nil {
		return 0, fmt.Errorf("count encoded objects: %w", err)
	}
	return count, nil
}
