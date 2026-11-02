package bootstrap

import (
	"time"

	"github.com/go-git/go-git/v6/plumbing"
)

// PlanningStats reports what checkpoint planning did for one bootstrap run.
//
// It is populated incrementally, and Execute returns it on every error path,
// so a run cut off by its deadline still reports how far planning got. That
// partial number is the point of it: a branch-heavy repository whose attempts
// all time out looks identical from the outside — size lookup, a long
// silence, a timeout — unless the attempt says how many branches it planned
// before it was stopped.
//
// Only batched bootstrap plans checkpoints. A one-shot bootstrap sets
// SourceRefs and BranchesTotal and leaves the rest zero; when its push is
// refused and the run switches to batches, the batched figures replace them.
type PlanningStats struct {
	// SourceRefs is the number of refs in the source advertisement the
	// caller passed as Params.SourceRefCount (see there for exactly what it
	// counts). 0 when unknown.
	SourceRefs int
	// BranchesTotal is the number of branch refs bootstrap had to create: the
	// trunk plus every other branch. Tags and other-kind refs ride the tail
	// phase without planning and are not counted. Set before the first
	// commit-graph fetch, so a run that fails there still reports it.
	BranchesTotal int
	// BranchesPlanned is how many of those have checkpoints so far: the trunk
	// once its chain is walked, each other branch once its checkpoints are
	// derived from the shared commit graph. Once planning finishes,
	// BranchesPlanned + BranchesSubsumed == BranchesTotal. Batched only.
	BranchesPlanned int
	// BranchesSubsumed is how many need no pack: their tip is already in the
	// trunk's history, or already reachable from the refs on the target.
	// Each becomes a single ref create. Batched only.
	BranchesSubsumed int
	// PlanningDuration is the wall time spent planning: the trunk's planning
	// and the remaining branches' planning, each including its commit-graph
	// fetch. It is added after each phase returns, whether it failed or not,
	// so a deadline mid-fetch still shows the time it burnt. Pushes between
	// the two phases are not included. Batched only.
	PlanningDuration time.Duration
}

// ProgressKind names what a ProgressEvent reports landing on the target.
type ProgressKind string

const (
	// ProgressTempRefAdvanced reports a checkpoint push that moved a
	// refs/gitsync/bootstrap/... resume marker forward.
	ProgressTempRefAdvanced ProgressKind = "temp-ref-advanced"
	// ProgressRefCreated reports a branch, tag or other ref created on the
	// target.
	ProgressRefCreated ProgressKind = "ref-created"
)

// ProgressEvent is one update the target took. See Params.OnProgress.
type ProgressEvent struct {
	Kind ProgressKind
	// Ref is the target ref the event is about: the temp ref for
	// ProgressTempRefAdvanced, the created ref otherwise.
	Ref plumbing.ReferenceName
	// Hash is the commit Ref now points at.
	Hash plumbing.Hash
}

// progress reports one update the target took. Nil-safe.
func (p Params) progress(kind ProgressKind, ref plumbing.ReferenceName, hash plumbing.Hash) {
	if p.OnProgress != nil {
		p.OnProgress(ProgressEvent{Kind: kind, Ref: ref, Hash: hash})
	}
}

// progressCreated reports ref as created when the push that just returned
// confirms it. It is the same rule the run applies before it counts a create
// as delivered: a refusal, or a target that said nothing about the ref, is not
// progress.
func (p Params) progressCreated(ref plumbing.ReferenceName, hash plumbing.Hash) {
	if p.OnProgress == nil {
		return
	}
	if confirmed, _ := p.createConfirmed(ref); confirmed {
		p.progress(ProgressRefCreated, ref, hash)
	}
}
