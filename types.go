package gitsync

import (
	"context"
	"errors"
)

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

	// FollowInfoRefsRedirect, when true, rewrites this endpoint's
	// effective host to the final URL returned by /info/refs after
	// HTTP redirects. Subsequent git RPCs (git-upload-pack,
	// git-receive-pack) then target the redirected host directly.
	// Matches vanilla git's smart-HTTP behaviour for discovery-aware
	// servers that 307 /info/refs to a hosting replica.
	FollowInfoRefsRedirect bool `json:"followInfoRefsRedirect,omitempty"`
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

// RefScope constrains which refs a request manages. AllRefs broadens scope
// to every refs/* on the source (branches, tags, notes, pulls, custom
// namespaces) and implies SyncPolicy.IncludeTags. ExcludeRefPrefixes
// subtracts namespaces from auto-discovery (useful for trimming
// refs/pull/* when mirroring open-source GitHub repos under AllRefs);
// explicit Mappings are not subject to it.
type RefScope struct {
	Branches           []string     `json:"branches"`
	Mappings           []RefMapping `json:"mappings"`
	AllRefs            bool         `json:"allRefs,omitempty"`
	ExcludeRefPrefixes []string     `json:"excludeRefPrefixes,omitempty"`
	// ExcludeRefs subtracts exact ref names from auto-discovery: matched
	// whole (not by prefix), so a caller can reserve a directory-anchor name
	// like refs/heads/entire without also excluding its children
	// (refs/heads/entire/foo). Not applied to explicit Mappings.
	ExcludeRefs []string `json:"excludeRefs,omitempty"`
}

// SyncPolicy controls high-level sync behavior. BestEffort downgrades per-ref
// receive-pack rejections to warnings; pack-level failures remain fatal.
//
// ForceWithLease and ForceBlind both allow non-fast-forward branch updates and
// tag retargets; they differ in how the push command's expected-old value is
// set. ForceWithLease sends the target tip captured at session start, so
// receive-pack rejects updates where the target moved during the run (the
// "lease"). ForceBlind sends a zero expected-old, telling receive-pack to
// overwrite regardless of the current target value — matching `git push
// --force` semantics. The two are mutually exclusive.
type SyncPolicy struct {
	Mode           OperationMode `json:"mode"`
	IncludeTags    bool          `json:"includeTags"`
	ForceWithLease bool          `json:"forceWithLease,omitempty"`
	ForceBlind     bool          `json:"forceBlind,omitempty"`
	Prune          bool          `json:"prune"`
	BestEffort     bool          `json:"bestEffort,omitempty"`
	Protocol       ProtocolMode  `json:"protocol"`
	// SourceAssertedEmpty and TargetAssertedEmpty are your authoritative
	// statements that the source and target repositories hold no refs at all
	// — from a repository-state query on each side, not from a ref listing.
	//
	// git-sync cannot determine either and will not guess. Ref hiding is
	// invisible to the client by design, on both legs and independently:
	// uploadpack.hideRefs can reduce a populated source's ls-refs response to
	// an unborn-HEAD line alone, and receive.hideRefs can reduce a populated
	// target's receive-pack advertisement to the bare capabilities^{}
	// sentinel. Neither is distinguishable from a genuinely empty repository.
	// The target case is the sharper one: receive.hideRefs and
	// uploadpack.hideRefs are separate settings, so a ref hidden from the push
	// side is still served to fetchers — a target wrongly judged empty is one
	// whose readers see refs the source does not have.
	//
	// Supply these only from a source of truth that sees past hiding. Without
	// them Replicate reports ErrSourceEmptyUnverified or
	// ErrTargetEmptyUnverified and never claims convergence.
	//
	// They are necessary inputs, never sufficient ones: Replicate
	// independently requires an empty advertisement on each side, an unborn
	// HEAD on the source, no ref names dropped as invalid on either, and no
	// visible target ref, before it will report convergence. Those checks can
	// only refuse — they never turn an absent assertion into a success.
	SourceAssertedEmpty bool `json:"sourceAssertedEmpty,omitempty"`
	TargetAssertedEmpty bool `json:"targetAssertedEmpty,omitempty"`

	// AllowEmptySource opts into treating a verified-empty source as an
	// outcome instead of an error. Replicate only. When source and target are
	// both verified empty (see the asserted-empty fields above) Replicate
	// succeeds with zero plans and ExecutionSummary.SourceEmpty set; when the
	// target holds refs the two have diverged and it fails with
	// ErrSourceEmptyTargetPopulated rather than deleting them; when either
	// side's emptiness cannot be established it fails with the matching
	// unverified error.
	//
	// Off by default: leave it unset and an empty source errors exactly as it
	// always has.
	AllowEmptySource bool `json:"allowEmptySource,omitempty"`
}

// Validate enforces SyncPolicy invariants at the request edge.
func (p SyncPolicy) Validate() error {
	if p.ForceWithLease && p.ForceBlind {
		return errors.New("ForceWithLease and ForceBlind are mutually exclusive")
	}
	if p.Mode == ModeReplicate && (p.ForceWithLease || p.ForceBlind) {
		return errors.New("replicate does not support force flags; use sync instead")
	}
	return nil
}

// ProbeRequest inspects source refs and optional target capabilities.
type ProbeRequest struct {
	Source             Endpoint     `json:"source"`
	Target             *Endpoint    `json:"target"`
	IncludeTags        bool         `json:"includeTags"`
	AllRefs            bool         `json:"allRefs,omitempty"`
	ExcludeRefPrefixes []string     `json:"excludeRefPrefixes,omitempty"`
	Protocol           ProtocolMode `json:"protocol"`
	CollectStats       bool         `json:"collectStats"`
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
