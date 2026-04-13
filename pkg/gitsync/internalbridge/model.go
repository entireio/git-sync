package internalbridge

import (
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/soph/git-sync/internal/planner"
	"github.com/soph/git-sync/internal/syncer"
)

type RefKind string

const (
	RefKindBranch RefKind = RefKind(planner.RefKindBranch)
	RefKindTag    RefKind = RefKind(planner.RefKindTag)
)

type Action string

const (
	ActionCreate Action = Action(planner.ActionCreate)
	ActionUpdate Action = Action(planner.ActionUpdate)
	ActionDelete Action = Action(planner.ActionDelete)
	ActionSkip   Action = Action(planner.ActionSkip)
	ActionBlock  Action = Action(planner.ActionBlock)
)

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

type RefPlan = RefResult

type RefInfo struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
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

type Stats struct {
	Enabled bool                     `json:"enabled"`
	Items   map[string]*ServiceStats `json:"items"`
}

type Measurement struct {
	Enabled            bool   `json:"enabled"`
	ElapsedMillis      int64  `json:"elapsed_millis"`
	PeakAllocBytes     uint64 `json:"peak_alloc_bytes"`
	PeakHeapInuseBytes uint64 `json:"peak_heap_inuse_bytes"`
	TotalAllocBytes    uint64 `json:"total_alloc_bytes"`
	GCCount            uint32 `json:"gc_count"`
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

type SyncCounts struct {
	Applied int `json:"applied"`
	Skipped int `json:"skipped"`
	Blocked int `json:"blocked"`
	Deleted int `json:"deleted"`
}

type BatchSummary struct {
	Enabled bool `json:"enabled"`
	Planned int  `json:"planned"`
	Done    int  `json:"done"`
}

type ExecutionSummary struct {
	DryRun             bool         `json:"dry_run"`
	Protocol           string       `json:"protocol"`
	Relay              bool         `json:"relay"`
	Mode               string       `json:"mode"`
	Reason             string       `json:"reason"`
	BootstrapSuggested bool         `json:"bootstrap_suggested"`
	Batch              BatchSummary `json:"batch"`
}

type SyncResult struct {
	Refs        []RefResult      `json:"refs"`
	Counts      SyncCounts       `json:"counts"`
	Execution   ExecutionSummary `json:"execution"`
	Stats       Stats            `json:"stats"`
	Measurement Measurement      `json:"measurement"`
}

type PlanResult = SyncResult

func FromProbeResult(result syncer.ProbeResult) ProbeResult {
	out := ProbeResult{
		SourceURL:     result.SourceURL,
		TargetURL:     result.TargetURL,
		RequestedMode: result.RequestedMode,
		Protocol:      result.Protocol,
		RefPrefixes:   append([]string(nil), result.RefPrefixes...),
		Capabilities:  append([]string(nil), result.Capabilities...),
		TargetCaps:    append([]string(nil), result.TargetCaps...),
		Refs:          make([]RefInfo, 0, len(result.Refs)),
		Stats:         FromStats(result.Stats),
		Measurement:   FromMeasurement(result.Measurement),
	}
	for _, ref := range result.Refs {
		out.Refs = append(out.Refs, RefInfo{Name: ref.Name, Hash: ref.Hash.String()})
	}
	return out
}

func FromSyncResult(result syncer.Result) SyncResult {
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
		Stats:       FromStats(result.Stats),
		Measurement: FromMeasurement(result.Measurement),
	}
	for _, plan := range result.Plans {
		out.Refs = append(out.Refs, RefResult{
			Branch:     plan.Branch,
			SourceRef:  plan.SourceRef.String(),
			TargetRef:  plan.TargetRef.String(),
			SourceHash: HashString(plan.SourceHash),
			TargetHash: HashString(plan.TargetHash),
			Kind:       RefKind(plan.Kind),
			Action:     Action(plan.Action),
			Reason:     plan.Reason,
		})
	}
	return out
}

func FromStats(stats syncer.Stats) Stats {
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

func FromMeasurement(m syncer.Measurement) Measurement {
	return Measurement{
		Enabled:            m.Enabled,
		ElapsedMillis:      m.ElapsedMillis,
		PeakAllocBytes:     m.PeakAllocBytes,
		PeakHeapInuseBytes: m.PeakHeapInuseBytes,
		TotalAllocBytes:    m.TotalAllocBytes,
		GCCount:            m.GCCount,
	}
}

func HashString(hash plumbing.Hash) string {
	if hash.IsZero() {
		return ""
	}
	return hash.String()
}
