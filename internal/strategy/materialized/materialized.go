// Package materialized implements the materialized fallback push strategy.
// This path fetches objects into local memory, then encodes and pushes them
// to the target. Used when relay is not safe.
package materialized

import (
	"context"
	"fmt"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/storer"

	"github.com/soph/git-sync/internal/convert"
	"github.com/soph/git-sync/internal/gitproto"
	"github.com/soph/git-sync/internal/planner"
)

// Params holds the inputs for a materialized push.
type Params struct {
	Store         storer.Storer
	SourceConn    *gitproto.Conn
	SourceService interface {
		FetchToStore(context.Context, storer.Storer, *gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) error
	}
	TargetPusher interface {
		PushObjects(context.Context, []gitproto.PushCommand, storer.Storer, []plumbing.Hash) error
	}
	DesiredRefs map[plumbing.ReferenceName]planner.DesiredRef
	TargetRefs  map[plumbing.ReferenceName]plumbing.Hash
	PushPlans   []planner.BranchPlan
	MaxObjects  int
}

// DefaultMaxMaterializedObjects is the default safety limit for the materialized fallback path.
// Beyond this count, the in-memory object store would consume excessive memory.
// Fail early rather than OOM (issue #15).
const DefaultMaxMaterializedObjects = 500_000

type executor struct {
	ctx    context.Context
	params Params
}

// Execute runs the materialized fallback: ensures tag objects are local,
// computes the object closure, and pushes to the target.
func Execute(ctx context.Context, p Params) error {
	if len(p.PushPlans) == 0 {
		return nil
	}
	return (&executor{ctx: ctx, params: p}).run()
}

func (e *executor) run() error {
	if err := e.ensureTagObjects(); err != nil {
		return fmt.Errorf("prepare local objects for push: %w", err)
	}
	hashes, err := e.collectObjectClosure()
	if err != nil {
		return fmt.Errorf("compute objects to push: %w", err)
	}
	if err := e.enforceObjectLimit(hashes); err != nil {
		return err
	}
	return e.push(hashes)
}

func (e *executor) ensureTagObjects() error {
	tagDesired := make(map[plumbing.ReferenceName]gitproto.DesiredRef)
	for _, plan := range e.params.PushPlans {
		if plan.Kind != planner.RefKindTag {
			continue
		}
		if d, ok := e.params.DesiredRefs[plan.TargetRef]; ok {
			tagDesired[plan.TargetRef] = gitproto.DesiredRef{
				SourceRef: d.SourceRef, TargetRef: d.TargetRef,
				SourceHash: d.SourceHash, IsTag: true,
			}
		}
	}
	if len(tagDesired) == 0 {
		return nil
	}
	err := e.params.SourceService.FetchToStore(e.ctx, e.params.Store, e.params.SourceConn, tagDesired, nil)
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return err
	}
	return nil
}

func (e *executor) collectObjectClosure() ([]plumbing.Hash, error) {
	objects := make([]plumbing.Hash, 0, len(e.params.PushPlans))
	for _, plan := range e.params.PushPlans {
		if plan.Action == planner.ActionCreate || plan.Action == planner.ActionUpdate {
			objects = append(objects, plan.SourceHash)
		}
	}
	return planner.ObjectsToPush(e.params.Store, objects, e.params.TargetRefs)
}

func (e *executor) enforceObjectLimit(hashes []plumbing.Hash) error {
	maxObjects := effectiveMaxObjects(e.params.MaxObjects)
	if len(hashes) <= maxObjects {
		return nil
	}
	return fmt.Errorf(
		"materialized push requires %d objects (limit %d); use bootstrap for large initial syncs",
		len(hashes), maxObjects,
	)
}

func (e *executor) push(hashes []plumbing.Hash) error {
	cmds := convert.PlansToPushCommands(e.params.PushPlans)
	if e.params.TargetPusher == nil {
		return fmt.Errorf("materialized strategy requires TargetPusher")
	}
	if err := e.params.TargetPusher.PushObjects(e.ctx, cmds, e.params.Store, hashes); err != nil {
		return fmt.Errorf("push target refs: %w", err)
	}
	return nil
}

func effectiveMaxObjects(limit int) int {
	if limit > 0 {
		return limit
	}
	return DefaultMaxMaterializedObjects
}
