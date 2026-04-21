// Package incremental implements the incremental relay strategy for git-sync.
// This fast-path streams a pack from source directly to target when all updates
// are fast-forward branch updates or new tag creates.
package incremental

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entirehq/git-sync/internal/convert"
	"github.com/entirehq/git-sync/internal/gitproto"
	"github.com/entirehq/git-sync/internal/planner"
	"github.com/entirehq/git-sync/internal/strategy/pushreconcile"
)

// Params holds the inputs for an incremental relay execution.
type Params struct {
	SourceConn    *gitproto.Conn
	SourceService interface {
		FetchPack(ctx context.Context, conn *gitproto.Conn, desired map[plumbing.ReferenceName]gitproto.DesiredRef, haves map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error)
	}
	TargetPusher interface {
		PushPack(ctx context.Context, cmds []gitproto.PushCommand, pack io.ReadCloser) error
	}
	TargetLister pushreconcile.Lister
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
		return Result{}, errors.New("incremental strategy requires CanRelay")
	}
	if p.TargetPusher == nil {
		return Result{}, errors.New("incremental strategy requires TargetPusher")
	}
	cmds := convert.PlansToPushCommands(p.PushPlans)
	if ok, reason := canRelay(cfg.Force, cfg.Prune, false, p.PushPlans); ok {
		return p.relay(ctx, cmds, p.TargetRefs, reason, "fetch source pack")
	}

	if p.CanTagRelay == nil {
		return Result{}, errors.New("incremental strategy requires CanTagRelay")
	}
	if ok, reason := p.CanTagRelay(p.PushPlans); ok {
		return p.relay(ctx, cmds, nil, reason, "fetch source tag pack")
	}

	return Result{}, nil
}

// relay fetches a pack from source with the given haves, pushes it to the
// target, and reconciles per-ref CAS failures if the target already matches.
func (p Params) relay(
	ctx context.Context,
	cmds []gitproto.PushCommand,
	haves map[plumbing.ReferenceName]plumbing.Hash,
	reason, fetchErrPrefix string,
) (Result, error) {
	desired := convert.DesiredRefsForPlans(p.DesiredRefs, p.PushPlans)
	packReader, err := p.SourceService.FetchPack(ctx, p.SourceConn, desired, haves)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", fetchErrPrefix, err)
	}
	packReader = gitproto.LimitPackReader(packReader, p.MaxPackBytes)
	packReader = closeOnce(packReader)
	pushErr := p.TargetPusher.PushPack(ctx, cmds, packReader)
	_ = packReader.Close()
	if pushErr != nil {
		if !pushreconcile.Check(ctx, pushErr, p.PushPlans, p.TargetLister) {
			return Result{}, fmt.Errorf("push target refs: %w", pushErr)
		}
		reason = pushreconcile.Reason
	}
	return Result{Relay: true, RelayMode: "incremental", RelayReason: reason}, nil
}

type closeOnceReadCloser struct {
	io.ReadCloser

	once sync.Once
}

func (c *closeOnceReadCloser) Close() error {
	var err error
	c.once.Do(func() {
		err = c.ReadCloser.Close()
	})
	if err != nil {
		return fmt.Errorf("close pack reader: %w", err)
	}
	return nil
}

func closeOnce(rc io.ReadCloser) io.ReadCloser {
	if rc == nil {
		return nil
	}
	if _, ok := rc.(*closeOnceReadCloser); ok {
		return rc
	}
	return &closeOnceReadCloser{ReadCloser: rc}
}
