package gitsync

import (
	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/syncer"
)

// ErrTargetRefMoved is returned (wrapped) by Sync and Replicate when a push was
// rejected because the target ref changed concurrently between this run's plan
// and its push — a benign, retryable compare-and-swap / lease miss, not a real
// failure. Test for it with errors.Is(err, gitsync.ErrTargetRefMoved).
//
// On the default push path the concrete error in the chain is a
// *RefRejectedError (reachable with errors.As). One case does NOT carry that
// concrete type: a BestEffort run with ForceWithLease escalates a lease miss
// through a plain wrapped error that still satisfies errors.Is(ErrTargetRefMoved)
// but is not a *RefRejectedError. So prefer errors.Is when you only need the
// cause, and treat a successful errors.As(*RefRejectedError) as best-effort.
//
// This is the supported way to distinguish a racing concurrent push from a
// genuine push failure; prefer it over inspecting the error message text, which
// is free-form and server-specific.
var ErrTargetRefMoved = gitproto.ErrTargetRefMoved

// RefRejectedError is a single per-ref "ng" status returned by the target's
// receive-pack report-status, reachable with errors.As. Ref is the rejected ref
// and Reason is the raw server reason text. Rejections that are concurrent
// target-ref moves also satisfy errors.Is(err, ErrTargetRefMoved).
type RefRejectedError = gitproto.RefRejectedError

// ErrNoRefsSelected is returned (wrapped) by Replicate when the source
// advertises refs but the requested scope — branch selection, ref mappings,
// exclude prefixes — matched none of them. The source is healthy; the request
// asked for refs it does not have. Benign for some sources by design: a GitHub
// repository whose only refs are under refs/pull/* selects nothing once that
// namespace is excluded. Test for it with errors.Is.
//
// It is deliberately distinct from the empty-source errors below, which mean
// the source has no refs AT ALL. Before these existed both cases shared one
// message and callers could not tell "nothing to mirror" from "nothing
// matched".
var ErrNoRefsSelected = syncer.ErrNoRefsSelected

// ErrSourceEmptyUnverified is returned (wrapped) by Replicate under
// SyncPolicy.AllowEmptySource when the source advertised no refs but never
// confirmed that it is empty — no protocol v2 unborn-HEAD assertion. The
// response's silence has several possible causes besides an empty repository
// (a blank body behind a valid header, a server-side ref-listing or
// hide-pattern regression), so the state is reported as unknown rather than
// converged. Test for it with errors.Is.
var ErrSourceEmptyUnverified = syncer.ErrSourceEmptyUnverified

// ErrSourceEmptyTargetPopulated is returned (wrapped) by Replicate under
// SyncPolicy.AllowEmptySource when the source is confirmed empty while the
// target still holds refs — a real divergence, since nothing the target serves
// exists on the source. Replicate refuses instead of converging: converging
// means deleting every ref on the target, and the states that produce this
// signature (a source restored from backup, a wipe, an out-of-band emptying)
// are the ones where the target may hold the only surviving copy. Test for it
// with errors.Is, and surface it as divergence rather than as "nothing to do".
var ErrSourceEmptyTargetPopulated = syncer.ErrSourceEmptyTargetPopulated
