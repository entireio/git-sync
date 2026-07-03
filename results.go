package gitsync

import (
	"github.com/go-git/go-git/v6/plumbing"

	"entire.io/entire/git-sync/internal/planner"
	"entire.io/entire/git-sync/internal/syncer"
)

// RefKind classifies a ref in results.
type RefKind string

const (
	RefKindBranch = RefKind(planner.RefKindBranch)
	RefKindTag    = RefKind(planner.RefKindTag)
	RefKindOther  = RefKind(planner.RefKindOther)
)

// Action is the planned or executed per-ref action.
type Action string

const (
	ActionCreate = Action(planner.ActionCreate)
	ActionUpdate = Action(planner.ActionUpdate)
	ActionDelete = Action(planner.ActionDelete)
	ActionSkip   = Action(planner.ActionSkip)
	ActionBlock  = Action(planner.ActionBlock)
	ActionWarn   = Action(planner.ActionWarn)
)

// RefResult describes the outcome for a single ref.
type RefResult struct {
	Branch     string  `json:"branch"`
	SourceRef  string  `json:"sourceRef"`
	TargetRef  string  `json:"targetRef"`
	SourceHash string  `json:"sourceHash"`
	TargetHash string  `json:"targetHash"`
	Kind       RefKind `json:"kind"`
	Action     Action  `json:"action"`
	Reason     string  `json:"reason"`
}

// RefPlan is a planned ref action; it has the same shape as RefResult.
type RefPlan = RefResult

// RefInfo is a ref name and its hash as reported by a probe.
type RefInfo struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

// ServiceStats aggregates request counters for one git service.
type ServiceStats struct {
	Name          string `json:"name"`
	Requests      int    `json:"requests"`
	RequestBytes  int64  `json:"requestBytes"`
	ResponseBytes int64  `json:"responseBytes"`
	Wants         int    `json:"wants"`
	Haves         int    `json:"haves"`
	Commands      int    `json:"commands"`
}

// SideBytes reports transfer volume and timing for one side of a sync.
type SideBytes struct {
	Label       string `json:"label"`
	Bytes       int64  `json:"bytes"`
	Display     string `json:"display,omitempty"`
	ActiveNanos int64  `json:"activeNanos,omitempty"`
	IdleNanos   int64  `json:"idleNanos,omitempty"`
}

// Stats aggregates transfer statistics when CollectStats was requested.
type Stats struct {
	Enabled      bool                     `json:"enabled"`
	Items        map[string]*ServiceStats `json:"items"`
	Sides        []SideBytes              `json:"sides,omitempty"`
	ElapsedNanos int64                    `json:"elapsedNanos,omitempty"`
}

// Measurement reports process resource usage when measurement was enabled.
type Measurement struct {
	Enabled            bool   `json:"enabled"`
	ElapsedMillis      int64  `json:"elapsedMillis"`
	PeakAllocBytes     uint64 `json:"peakAllocBytes"`
	PeakHeapInuseBytes uint64 `json:"peakHeapInuseBytes"`
	TotalAllocBytes    uint64 `json:"totalAllocBytes"`
	GCCount            uint32 `json:"gcCount"`
}

// ProbeResult is the outcome of a Probe.
type ProbeResult struct {
	SourceURL     string      `json:"sourceUrl"`
	TargetURL     string      `json:"targetUrl,omitempty"`
	RequestedMode string      `json:"requestedMode"`
	Protocol      string      `json:"protocol"`
	RefPrefixes   []string    `json:"refPrefixes"`
	Capabilities  []string    `json:"sourceCapabilities"`
	TargetCaps    []string    `json:"targetCapabilities,omitempty"`
	Refs          []RefInfo   `json:"refs"`
	SourceHEAD    string      `json:"sourceHead,omitempty"`
	Stats         Stats       `json:"stats"`
	Measurement   Measurement `json:"measurement"`
}

// SyncCounts tallies per-ref outcomes of a sync.
type SyncCounts struct {
	Applied int `json:"applied"`
	Skipped int `json:"skipped"`
	Blocked int `json:"blocked"`
	Deleted int `json:"deleted"`
	Warned  int `json:"warned"`
}

// BatchSummary reports batched push progress.
type BatchSummary struct {
	Enabled bool `json:"enabled"`
	Planned int  `json:"planned"`
	Done    int  `json:"done"`
}

// ExecutionSummary describes how a sync was executed.
type ExecutionSummary struct {
	DryRun             bool         `json:"dryRun"`
	Protocol           string       `json:"protocol"`
	OperationMode      string       `json:"operationMode"`
	Relay              bool         `json:"relay"`
	TransferMode       string       `json:"transferMode"`
	Reason             string       `json:"reason"`
	BootstrapSuggested bool         `json:"bootstrapSuggested"`
	SourceHEAD         string       `json:"sourceHead,omitempty"`
	Batch              BatchSummary `json:"batch"`
}

// SyncResult is the outcome of a Sync or Replicate.
type SyncResult struct {
	Refs        []RefResult      `json:"refs"`
	Counts      SyncCounts       `json:"counts"`
	Execution   ExecutionSummary `json:"execution"`
	Stats       Stats            `json:"stats"`
	Measurement Measurement      `json:"measurement"`
}

// PlanResult is the outcome of a Plan; it has the same shape as SyncResult.
type PlanResult = SyncResult

func fromProbeResult(result syncer.ProbeResult) ProbeResult {
	out := ProbeResult{
		SourceURL:     result.SourceURL,
		TargetURL:     result.TargetURL,
		RequestedMode: result.RequestedMode,
		Protocol:      result.Protocol,
		RefPrefixes:   append([]string(nil), result.RefPrefixes...),
		Capabilities:  append([]string(nil), result.Capabilities...),
		TargetCaps:    append([]string(nil), result.TargetCaps...),
		Refs:          make([]RefInfo, 0, len(result.Refs)),
		SourceHEAD:    result.SourceHEAD.String(),
		Stats:         fromStats(result.Stats),
		Measurement:   fromMeasurement(result.Measurement),
	}
	for _, ref := range result.Refs {
		out.Refs = append(out.Refs, RefInfo{Name: ref.Name, Hash: ref.Hash.String()})
	}
	return out
}

func fromSyncResult(result syncer.Result) SyncResult {
	out := SyncResult{
		Refs: make([]RefResult, 0, len(result.Plans)),
		Counts: SyncCounts{
			Applied: result.Pushed,
			Skipped: result.Skipped,
			Blocked: result.Blocked,
			Deleted: result.Deleted,
			Warned:  result.Warned,
		},
		Execution: ExecutionSummary{
			DryRun:             result.DryRun,
			Protocol:           result.Protocol,
			OperationMode:      result.OperationMode,
			Relay:              result.Relay,
			TransferMode:       result.RelayMode,
			Reason:             result.RelayReason,
			BootstrapSuggested: result.BootstrapSuggested,
			SourceHEAD:         result.SourceHEAD.String(),
			Batch: BatchSummary{
				Enabled: result.Batching,
				Planned: result.PlannedBatchCount,
				Done:    result.BatchCount,
			},
		},
		Stats:       fromStats(result.Stats),
		Measurement: fromMeasurement(result.Measurement),
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

func fromStats(stats syncer.Stats) Stats {
	out := Stats{
		Enabled:      stats.Enabled,
		Items:        make(map[string]*ServiceStats, len(stats.Items)),
		ElapsedNanos: stats.ElapsedNanos,
	}
	for key, item := range stats.Items {
		out.Items[key] = &ServiceStats{
			Name:          item.Name,
			Requests:      item.Requests,
			RequestBytes:  item.RequestBytes,
			ResponseBytes: item.ResponseBytes,
			Wants:         item.Wants,
			Haves:         item.Haves,
			Commands:      item.Commands,
		}
	}
	if len(stats.Sides) > 0 {
		out.Sides = make([]SideBytes, 0, len(stats.Sides))
		for _, side := range stats.Sides {
			out.Sides = append(out.Sides, SideBytes{
				Label:       side.Label,
				Bytes:       side.Bytes,
				Display:     side.Display,
				ActiveNanos: side.ActiveNanos,
				IdleNanos:   side.IdleNanos,
			})
		}
	}
	return out
}

func fromMeasurement(m syncer.Measurement) Measurement {
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
