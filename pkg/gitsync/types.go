package gitsync

import (
	"context"

	"github.com/entirehq/git-sync/internal/gitproto"
	"github.com/entirehq/git-sync/pkg/gitsync/internalbridge"
)

// HTTPError is returned (wrapped) when a Git Smart-HTTP request receives a
// non-2xx response. Consumers can use errors.As(err, &gitsync.HTTPError{}) to
// inspect StatusCode directly instead of parsing the formatted error string.
type HTTPError = gitproto.HTTPError

// ProtocolMode controls source-side protocol negotiation.
type ProtocolMode string

const (
	ProtocolAuto ProtocolMode = "auto"
	ProtocolV1   ProtocolMode = "v1"
	ProtocolV2   ProtocolMode = "v2"
)

// OperationMode controls high-level sync semantics.
type OperationMode string

const (
	ModeSync      OperationMode = "sync"
	ModeReplicate OperationMode = "replicate"
)

// Endpoint identifies a remote Git endpoint.
type Endpoint struct {
	URL string `json:"url"`
}

// EndpointAuth carries explicit per-request auth and TLS settings.
// It is resolved through an AuthProvider rather than embedded in Endpoint so
// endpoint identity does not also become the public auth-precedence boundary.
type EndpointAuth struct {
	Username      string `json:"username"`
	Token         string `json:"token"`
	BearerToken   string `json:"bearerToken"`
	SkipTLSVerify bool   `json:"skipTlsVerify"`
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
	Source EndpointAuth `json:"source"`
	Target EndpointAuth `json:"target"`
}

// AuthFor implements AuthProvider.
func (p StaticAuthProvider) AuthFor(_ context.Context, _ Endpoint, role EndpointRole) (EndpointAuth, error) { //nolint:unparam // implements AuthProvider interface
	if role == TargetRole {
		return p.Target, nil
	}
	return p.Source, nil
}

// RefMapping is an explicit source-to-target ref mapping.
type RefMapping struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// RefScope constrains which refs a request manages.
type RefScope struct {
	Branches []string     `json:"branches"`
	Mappings []RefMapping `json:"mappings"`
}

// SyncPolicy controls high-level sync behavior.
type SyncPolicy struct {
	Mode        OperationMode `json:"mode"`
	IncludeTags bool          `json:"includeTags"`
	Force       bool          `json:"force"`
	Prune       bool          `json:"prune"`
	Protocol    ProtocolMode  `json:"protocol"`
}

// ProbeRequest inspects source refs and optional target capabilities.
type ProbeRequest struct {
	Source       Endpoint     `json:"source"`
	Target       *Endpoint    `json:"target"`
	IncludeTags  bool         `json:"includeTags"`
	Protocol     ProtocolMode `json:"protocol"`
	CollectStats bool         `json:"collectStats"`
}

// PlanRequest computes ref actions without pushing.
type PlanRequest struct {
	Source       Endpoint   `json:"source"`
	Target       Endpoint   `json:"target"`
	Scope        RefScope   `json:"scope"`
	Policy       SyncPolicy `json:"policy"`
	CollectStats bool       `json:"collectStats"`
}

// SyncRequest executes a sync between two remotes.
type SyncRequest struct {
	Source       Endpoint   `json:"source"`
	Target       Endpoint   `json:"target"`
	Scope        RefScope   `json:"scope"`
	Policy       SyncPolicy `json:"policy"`
	CollectStats bool       `json:"collectStats"`
}

// ListRefsRequest fetches the ref advertisement from a single endpoint.
// Target selects which transport service to advertise over: true uses
// git-receive-pack (target side, what a push would see), false uses
// git-upload-pack (source side, what a fetch would see).
type ListRefsRequest struct {
	Endpoint Endpoint `json:"endpoint"`
	Target   bool     `json:"target"`
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
