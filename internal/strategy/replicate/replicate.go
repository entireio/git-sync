// Package replicate implements relay-only source-authoritative replication.
package replicate

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/go-git/go-git/v6/plumbing"

	"entire.io/entire/git-sync/internal/convert"
	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
)

// Params holds the inputs for a replication relay execution.
type Params struct {
	SourceConn    gitproto.Conn
	SourceService interface {
		FetchPack(ctx context.Context, conn gitproto.Conn, desired map[plumbing.ReferenceName]gitproto.DesiredRef, haves map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error)
	}
	TargetPusher interface {
		PushPack(ctx context.Context, cmds []gitproto.PushCommand, pack io.ReadCloser) error
		PushCommands(ctx context.Context, cmds []gitproto.PushCommand) error
	}
	DesiredRefs  map[plumbing.ReferenceName]planner.DesiredRef
	TargetRefs   map[plumbing.ReferenceName]plumbing.Hash
	PushPlans    []planner.BranchPlan
	MaxPackBytes int64
}

// Result holds the outcome of a replication relay.
type Result struct {
	Relay       bool
	RelayMode   string
	RelayReason string
}

// Execute runs relay-only replication. Create/update refs are pushed via pack
// relay and deletes are sent afterwards as ref-only commands.
func Execute(ctx context.Context, p Params) (Result, error) {
	if p.TargetPusher == nil {
		return Result{}, errors.New("replicate strategy requires TargetPusher")
	}

	updatePlans := make([]planner.BranchPlan, 0, len(p.PushPlans))
	deletePlans := make([]planner.BranchPlan, 0, len(p.PushPlans))
	for _, plan := range p.PushPlans {
		switch plan.Action {
		case planner.ActionCreate, planner.ActionUpdate:
			updatePlans = append(updatePlans, plan)
		case planner.ActionDelete:
			deletePlans = append(deletePlans, plan)
		case planner.ActionSkip, planner.ActionBlock, planner.ActionWarn:
			// not applicable: replicate runs before any rejection downgrade,
			// and skip/block plans never reach the executor.
		}
	}

	if len(updatePlans) > 0 {
		desired := convert.DesiredRefsForPlans(p.DesiredRefs, updatePlans)
		packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, desired, p.TargetRefs)
		if err != nil {
			return Result{}, fmt.Errorf("fetch source pack: %w", err)
		}
		packReader = gitproto.LimitPackReader(packReader, p.MaxPackBytes)
		packReader = gitproto.CloseOnce(packReader)
		if err := p.TargetPusher.PushPack(ctx, convert.PlansToPushCommands(updatePlans, false), packReader); err != nil {
			_ = packReader.Close()
			return Result{}, fmt.Errorf("push target refs: %w", err)
		}
		_ = packReader.Close()
	}

	if len(deletePlans) > 0 {
		if err := p.TargetPusher.PushCommands(ctx, convert.PlansToPushCommands(deletePlans, false)); err != nil {
			return Result{}, fmt.Errorf("delete target refs: %w", err)
		}
	}

	return Result{Relay: true, RelayMode: "replicate", RelayReason: "replicate-overwrite-relay"}, nil
}
