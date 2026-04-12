// Package incremental implements the incremental relay strategy for git-sync.
// This fast-path streams a pack from source directly to target when all updates
// are fast-forward branch updates or new tag creates.
package incremental

import (
	"context"
	"fmt"
	"io"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/soph/git-sync/internal/convert"
	"github.com/soph/git-sync/internal/gitproto"
	"github.com/soph/git-sync/internal/planner"
)

// Params holds the inputs for an incremental relay execution.
type Params struct {
	SourceConn    *gitproto.Conn
	SourceService interface {
		FetchPack(context.Context, *gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error)
	}
	TargetPusher interface {
		PushPack(context.Context, []gitproto.PushCommand, io.ReadCloser) error
	}
	DesiredRefs  map[plumbing.ReferenceName]planner.DesiredRef
	TargetRefs   map[plumbing.ReferenceName]plumbing.Hash
	PushPlans    []planner.BranchPlan
	MaxPackBytes int64
	Verbose      bool
	CanRelay     func(bool, bool, bool, []planner.BranchPlan) (bool, string)
	CanTagRelay  func([]planner.BranchPlan) (bool, string)
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
	canRelay := p.CanRelay
	if canRelay == nil {
		return Result{}, fmt.Errorf("incremental strategy requires CanRelay")
	}
	if p.TargetPusher == nil {
		return Result{}, fmt.Errorf("incremental strategy requires TargetPusher")
	}
	if ok, reason := canRelay(cfg.Force, cfg.Prune, false, p.PushPlans); ok {
		desired := convert.DesiredRefs(planner.DesiredSubset(p.DesiredRefs, p.PushPlans))
		packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, desired, p.TargetRefs)
		if err != nil {
			return Result{}, fmt.Errorf("fetch source pack: %w", err)
		}
		packReader = gitproto.LimitPackReader(packReader, p.MaxPackBytes)
		cmds := gitproto.ToPushCommands(convert.PlansToPushPlans(p.PushPlans))
		if err := p.TargetPusher.PushPack(ctx, cmds, packReader); err != nil {
			return Result{}, fmt.Errorf("push target refs: %w", err)
		}
		return Result{Relay: true, RelayMode: "incremental", RelayReason: reason}, nil
	}

	if p.CanTagRelay == nil {
		return Result{}, fmt.Errorf("incremental strategy requires CanTagRelay")
	}
	if ok, reason := p.CanTagRelay(p.PushPlans); ok {
		desired := convert.DesiredRefs(planner.DesiredSubset(p.DesiredRefs, p.PushPlans))
		packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, desired, nil)
		if err != nil {
			return Result{}, fmt.Errorf("fetch source tag pack: %w", err)
		}
		packReader = gitproto.LimitPackReader(packReader, p.MaxPackBytes)
		cmds := gitproto.ToPushCommands(convert.PlansToPushPlans(p.PushPlans))
		if err := p.TargetPusher.PushPack(ctx, cmds, packReader); err != nil {
			return Result{}, fmt.Errorf("push target refs: %w", err)
		}
		return Result{Relay: true, RelayMode: "incremental", RelayReason: reason}, nil
	}

	return Result{}, nil
}
