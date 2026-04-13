package gitsync

import (
	"context"
	"github.com/soph/git-sync/pkg/gitsync/internalbridge"
)

// ProtocolMode controls source-side protocol negotiation.
type ProtocolMode string

const (
	ProtocolAuto ProtocolMode = "auto"
	ProtocolV1   ProtocolMode = "v1"
	ProtocolV2   ProtocolMode = "v2"
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
type RefMapping struct {
	Source string
	Target string
}

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

type RefKind = internalbridge.RefKind

const (
	RefKindBranch RefKind = internalbridge.RefKindBranch
	RefKindTag    RefKind = internalbridge.RefKindTag
)

type Action = internalbridge.Action

const (
	ActionCreate Action = internalbridge.ActionCreate
	ActionUpdate Action = internalbridge.ActionUpdate
	ActionDelete Action = internalbridge.ActionDelete
	ActionSkip   Action = internalbridge.ActionSkip
	ActionBlock  Action = internalbridge.ActionBlock
)

type RefResult = internalbridge.RefResult
type RefPlan = internalbridge.RefPlan
type RefInfo = internalbridge.RefInfo
type ServiceStats = internalbridge.ServiceStats
type Stats = internalbridge.Stats
type Measurement = internalbridge.Measurement
type ProbeResult = internalbridge.ProbeResult
type SyncCounts = internalbridge.SyncCounts
type BatchSummary = internalbridge.BatchSummary
type ExecutionSummary = internalbridge.ExecutionSummary
type SyncResult = internalbridge.SyncResult
type PlanResult = internalbridge.PlanResult
