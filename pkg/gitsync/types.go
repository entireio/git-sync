package gitsync

import (
	"context"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/soph/git-sync/internal/planner"
	"github.com/soph/git-sync/internal/syncer"
	"github.com/soph/git-sync/internal/validation"
)

// ProtocolMode controls source-side protocol negotiation.
type ProtocolMode string

const (
	ProtocolAuto ProtocolMode = validation.ProtocolAuto
	ProtocolV1   ProtocolMode = validation.ProtocolV1
	ProtocolV2   ProtocolMode = validation.ProtocolV2
)

// Endpoint identifies a remote Git endpoint.
type Endpoint struct {
	URL string
}

// EndpointAuth carries explicit per-request auth and TLS settings.
// It is resolved through an AuthProvider rather than embedded in Endpoint so
// endpoint identity does not also become the public auth-precedence boundary.
type EndpointAuth struct {
	Username      string
	Token         string
	BearerToken   string
	SkipTLSVerify bool
}

// EndpointRole identifies whether auth is being resolved for the source or target.
type EndpointRole string

const (
	SourceRole EndpointRole = "source"
	TargetRole EndpointRole = "target"
)

// AuthProvider resolves auth for a request endpoint.
type AuthProvider interface {
	AuthFor(ctx context.Context, endpoint Endpoint, role EndpointRole) (EndpointAuth, error)
}

// StaticAuthProvider returns fixed source and target auth values.
type StaticAuthProvider struct {
	Source EndpointAuth
	Target EndpointAuth
}

// AuthFor implements AuthProvider.
func (p StaticAuthProvider) AuthFor(_ context.Context, _ Endpoint, role EndpointRole) (EndpointAuth, error) {
	if role == TargetRole {
		return p.Target, nil
	}
	return p.Source, nil
}

// RefMapping is an explicit source-to-target ref mapping.
type RefMapping = validation.RefMapping

// RefScope constrains which refs a request manages.
type RefScope struct {
	Branches []string
	Mappings []RefMapping
}

// SyncPolicy controls high-level sync behavior.
type SyncPolicy struct {
	IncludeTags bool
	Force       bool
	Prune       bool
	Protocol    ProtocolMode
}

// ProbeRequest inspects source refs and optional target capabilities.
type ProbeRequest struct {
	Source       Endpoint
	Target       *Endpoint
	IncludeTags  bool
	Protocol     ProtocolMode
	CollectStats bool
}

// PlanRequest computes ref actions without pushing.
type PlanRequest struct {
	Source       Endpoint
	Target       Endpoint
	Scope        RefScope
	Policy       SyncPolicy
	CollectStats bool
}

// SyncRequest executes a sync between two remotes.
type SyncRequest struct {
	Source       Endpoint
	Target       Endpoint
	Scope        RefScope
	Policy       SyncPolicy
	CollectStats bool
}

// RefKind distinguishes branch refs from tag refs.
type RefKind string

const (
	RefKindBranch RefKind = RefKind(planner.RefKindBranch)
	RefKindTag    RefKind = RefKind(planner.RefKindTag)
)

// Action describes the planned or executed operation on a ref.
type Action string

const (
	ActionCreate Action = Action(planner.ActionCreate)
	ActionUpdate Action = Action(planner.ActionUpdate)
	ActionDelete Action = Action(planner.ActionDelete)
	ActionSkip   Action = Action(planner.ActionSkip)
	ActionBlock  Action = Action(planner.ActionBlock)
)

// RefPlan describes the outcome for a single ref.
type RefResult struct {
	Branch     string  `json:"branch"`
	SourceRef  string  `json:"source_ref"`
	TargetRef  string  `json:"target_ref"`
	SourceHash string  `json:"source_hash"`
	TargetHash string  `json:"target_hash"`
	Kind       RefKind `json:"kind"`
	Action     Action  `json:"action"`
	Reason     string  `json:"reason"`
}

// RefPlan is kept as an alias for early adopters while the stable result
// surface converges on the more explicit RefResult name.
type RefPlan = RefResult

// RefInfo identifies a named ref.
type RefInfo struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

// ServiceStats tracks transfer statistics for a single service.
type ServiceStats struct {
	Name          string `json:"name"`
	Requests      int    `json:"requests"`
	RequestBytes  int64  `json:"request_bytes"`
	ResponseBytes int64  `json:"response_bytes"`
	Wants         int    `json:"wants"`
	Haves         int    `json:"haves"`
	Commands      int    `json:"commands"`
}

// Stats summarizes transfer metrics.
type Stats struct {
	Enabled bool                     `json:"enabled"`
	Items   map[string]*ServiceStats `json:"items"`
}

// Measurement summarizes elapsed time and Go heap usage.
type Measurement struct {
	Enabled            bool   `json:"enabled"`
	ElapsedMillis      int64  `json:"elapsed_millis"`
	PeakAllocBytes     uint64 `json:"peak_alloc_bytes"`
	PeakHeapInuseBytes uint64 `json:"peak_heap_inuse_bytes"`
	TotalAllocBytes    uint64 `json:"total_alloc_bytes"`
	GCCount            uint32 `json:"gc_count"`
}

// ProbeResult holds structured probe output suitable for workers.
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

// SyncCounts summarizes per-run ref outcomes.
type SyncCounts struct {
	Applied int `json:"applied"`
	Skipped int `json:"skipped"`
	Blocked int `json:"blocked"`
	Deleted int `json:"deleted"`
}

// BatchSummary describes batching behavior when it was used.
type BatchSummary struct {
	Enabled bool `json:"enabled"`
	Planned int  `json:"planned"`
	Done    int  `json:"done"`
}

// ExecutionSummary describes how a sync was carried out.
type ExecutionSummary struct {
	DryRun             bool         `json:"dry_run"`
	Protocol           string       `json:"protocol"`
	Relay              bool         `json:"relay"`
	Mode               string       `json:"mode"`
	Reason             string       `json:"reason"`
	BootstrapSuggested bool         `json:"bootstrap_suggested"`
	Batch              BatchSummary `json:"batch"`
}

// SyncResult holds structured sync output suitable for workers.
type SyncResult struct {
	Refs        []RefResult      `json:"refs"`
	Counts      SyncCounts       `json:"counts"`
	Execution   ExecutionSummary `json:"execution"`
	Stats       Stats            `json:"stats"`
	Measurement Measurement      `json:"measurement"`
}

// PlanResult is the dry-run form of SyncResult.
type PlanResult = SyncResult

func fromSyncerProbeResult(result syncer.ProbeResult) ProbeResult {
	out := ProbeResult{
		SourceURL:     result.SourceURL,
		TargetURL:     result.TargetURL,
		RequestedMode: result.RequestedMode,
		Protocol:      result.Protocol,
		RefPrefixes:   append([]string(nil), result.RefPrefixes...),
		Capabilities:  append([]string(nil), result.Capabilities...),
		TargetCaps:    append([]string(nil), result.TargetCaps...),
		Refs:          make([]RefInfo, 0, len(result.Refs)),
		Stats:         fromSyncerStats(result.Stats),
		Measurement:   fromSyncerMeasurement(result.Measurement),
	}
	for _, ref := range result.Refs {
		out.Refs = append(out.Refs, RefInfo{Name: ref.Name, Hash: ref.Hash.String()})
	}
	return out
}

func fromSyncerResult(result syncer.Result) SyncResult {
	out := SyncResult{
		Refs: make([]RefResult, 0, len(result.Plans)),
		Counts: SyncCounts{
			Applied: result.Pushed,
			Skipped: result.Skipped,
			Blocked: result.Blocked,
			Deleted: result.Deleted,
		},
		Execution: ExecutionSummary{
			DryRun:             result.DryRun,
			Protocol:           result.Protocol,
			Relay:              result.Relay,
			Mode:               result.RelayMode,
			Reason:             result.RelayReason,
			BootstrapSuggested: result.BootstrapSuggested,
			Batch: BatchSummary{
				Enabled: result.Batching,
				Planned: result.PlannedBatchCount,
				Done:    result.BatchCount,
			},
		},
		Stats:       fromSyncerStats(result.Stats),
		Measurement: fromSyncerMeasurement(result.Measurement),
	}
	for _, plan := range result.Plans {
		out.Refs = append(out.Refs, RefResult{
			Branch:     plan.Branch,
			SourceRef:  plan.SourceRef.String(),
			TargetRef:  plan.TargetRef.String(),
			SourceHash: hashString(plan.SourceHash),
			TargetHash: hashString(plan.TargetHash),
			Kind:       RefKind(plan.Kind),
			Action:     Action(plan.Action),
			Reason:     plan.Reason,
		})
	}
	return out
}

func fromSyncerStats(stats syncer.Stats) Stats {
	out := Stats{Enabled: stats.Enabled, Items: make(map[string]*ServiceStats, len(stats.Items))}
	for key, item := range stats.Items {
		copyItem := *item
		out.Items[key] = &ServiceStats{
			Name:          copyItem.Name,
			Requests:      copyItem.Requests,
			RequestBytes:  copyItem.RequestBytes,
			ResponseBytes: copyItem.ResponseBytes,
			Wants:         copyItem.Wants,
			Haves:         copyItem.Haves,
			Commands:      copyItem.Commands,
		}
	}
	return out
}

func fromSyncerMeasurement(m syncer.Measurement) Measurement {
	return Measurement{
		Enabled:            m.Enabled,
		ElapsedMillis:      m.ElapsedMillis,
		PeakAllocBytes:     m.PeakAllocBytes,
		PeakHeapInuseBytes: m.PeakHeapInuseBytes,
		TotalAllocBytes:    m.TotalAllocBytes,
		GCCount:            m.GCCount,
	}
}

func hashString(hash plumbing.Hash) string {
	if hash.IsZero() {
		return ""
	}
	return hash.String()
}
