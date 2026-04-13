// Package replicate implements relay-only source-authoritative replication.
package replicate

import (
	"context"
	"fmt"
	"io"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/soph/git-sync/internal/convert"
	"github.com/soph/git-sync/internal/gitproto"
	"github.com/soph/git-sync/internal/planner"
)

// Params holds the inputs for a replication relay execution.
type Params struct {
	SourceConn    *gitproto.Conn
	SourceService interface {
		FetchPack(context.Context, *gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error)
	}
	TargetPusher interface {
		PushPack(context.Context, []gitproto.PushCommand, io.ReadCloser) error
		PushCommands(context.Context, []gitproto.PushCommand) error
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
		return Result{}, fmt.Errorf("replicate strategy requires TargetPusher")
	}

	updatePlans := make([]planner.BranchPlan, 0, len(p.PushPlans))
	deletePlans := make([]planner.BranchPlan, 0, len(p.PushPlans))
	for _, plan := range p.PushPlans {
		switch plan.Action {
		case planner.ActionCreate, planner.ActionUpdate:
			updatePlans = append(updatePlans, plan)
		case planner.ActionDelete:
			deletePlans = append(deletePlans, plan)
		default:
			return Result{}, fmt.Errorf("replicate strategy does not support %s actions", plan.Action)
		}
	}

	if len(updatePlans) > 0 {
		desired := convert.DesiredRefsForPlans(p.DesiredRefs, updatePlans)
		packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, desired, p.TargetRefs)
		if err != nil {
			return Result{}, fmt.Errorf("fetch source pack: %w", err)
		}
		packReader = gitproto.LimitPackReader(packReader, p.MaxPackBytes)
		packReader = closeOnce(packReader)
		if err := p.TargetPusher.PushPack(ctx, convert.PlansToPushCommands(updatePlans), packReader); err != nil {
			_ = packReader.Close()
			return Result{}, fmt.Errorf("push target refs: %w", err)
		}
		_ = packReader.Close()
	}

	if len(deletePlans) > 0 {
		if err := p.TargetPusher.PushCommands(ctx, convert.PlansToPushCommands(deletePlans)); err != nil {
			return Result{}, fmt.Errorf("delete target refs: %w", err)
		}
	}

	return Result{Relay: true, RelayMode: "replicate", RelayReason: "replicate-overwrite-relay"}, nil
}

type closeOnceReadCloser struct {
	io.ReadCloser
	closed bool
}

func (c *closeOnceReadCloser) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	return c.ReadCloser.Close()
}

func closeOnce(rc io.ReadCloser) io.ReadCloser {
	if rc == nil {
		return nil
	}
	if _, ok := rc.(*closeOnceReadCloser); ok {
		return rc
	}
	// PushPack and the caller both close the relay reader; wrap it so retries and
	// error cleanup do not surface spurious double-close failures.
	return &closeOnceReadCloser{ReadCloser: rc}
}
