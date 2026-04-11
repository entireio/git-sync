// Package incremental implements the incremental relay strategy for git-sync.
// This fast-path streams a pack from source directly to target when all updates
// are fast-forward branch updates or new tag creates.
package incremental

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"

	"github.com/soph/git-sync/internal/gitproto"
	"github.com/soph/git-sync/internal/planner"
)

// Params holds the inputs for an incremental relay execution.
type Params struct {
	SourceConn    *gitproto.Conn
	TargetConn    *gitproto.Conn
	SourceService *gitproto.RefService
	TargetAdv     *packp.AdvRefs
	DesiredRefs   map[plumbing.ReferenceName]planner.DesiredRef
	TargetRefs    map[plumbing.ReferenceName]plumbing.Hash
	PushPlans     []planner.BranchPlan
	MaxPackBytes  int64
	Verbose       bool
}

// Result holds the outcome of an incremental relay.
type Result struct {
	Relay       bool
	RelayMode   string
	RelayReason string
}

// Execute attempts the incremental relay strategy. Returns (result, nil) on
// success, or (zero, nil) if the strategy is not applicable. Errors indicate
// a relay was attempted but failed.
func Execute(ctx context.Context, p Params, cfg planner.PlanConfig) (Result, error) {
	if ok, reason := planner.CanIncrementalRelay(cfg.Force, cfg.Prune, false, p.PushPlans, p.TargetAdv); ok {
		desired := toGP(planner.DesiredSubset(p.DesiredRefs, p.PushPlans))
		packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, desired, p.TargetRefs)
		if err != nil {
			return Result{}, fmt.Errorf("fetch source pack: %w", err)
		}
		packReader = gitproto.LimitPackReader(packReader, p.MaxPackBytes)
		cmds := gitproto.ToPushCommands(plansToPushPlans(p.PushPlans))
		if err := gitproto.PushPack(ctx, p.TargetConn, p.TargetAdv, cmds, packReader, p.Verbose); err != nil {
			return Result{}, fmt.Errorf("push target refs: %w", err)
		}
		return Result{Relay: true, RelayMode: "incremental", RelayReason: reason}, nil
	}

	if ok, reason := planner.CanFullTagCreateRelay(p.PushPlans); ok {
		desired := toGP(planner.DesiredSubset(p.DesiredRefs, p.PushPlans))
		packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, desired, nil)
		if err != nil {
			return Result{}, fmt.Errorf("fetch source tag pack: %w", err)
		}
		packReader = gitproto.LimitPackReader(packReader, p.MaxPackBytes)
		cmds := gitproto.ToPushCommands(plansToPushPlans(p.PushPlans))
		if err := gitproto.PushPack(ctx, p.TargetConn, p.TargetAdv, cmds, packReader, p.Verbose); err != nil {
			return Result{}, fmt.Errorf("push target refs: %w", err)
		}
		return Result{Relay: true, RelayMode: "incremental", RelayReason: reason}, nil
	}

	return Result{}, nil
}

func toGP(desired map[plumbing.ReferenceName]planner.DesiredRef) map[plumbing.ReferenceName]gitproto.DesiredRef {
	out := make(map[plumbing.ReferenceName]gitproto.DesiredRef, len(desired))
	for k, v := range desired {
		out[k] = gitproto.DesiredRef{
			SourceRef: v.SourceRef, TargetRef: v.TargetRef,
			SourceHash: v.SourceHash, IsTag: v.Kind == planner.RefKindTag,
		}
	}
	return out
}

func plansToPushPlans(plans []planner.BranchPlan) []gitproto.PushPlan {
	out := make([]gitproto.PushPlan, len(plans))
	for i, p := range plans {
		out[i] = gitproto.PushPlan{
			TargetRef: p.TargetRef, TargetHash: p.TargetHash, SourceHash: p.SourceHash,
			Delete: p.Action == planner.ActionDelete,
		}
	}
	return out
}
