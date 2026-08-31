package gitsync

import (
	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/strategy/bootstrap"
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

// ErrNoRefsSelected is returned (wrapped) by Replicate under
// SyncPolicy.AllowEmptySource when the source advertises refs but
// RefScope.ExcludeRefPrefixes / ExcludeRefs subtracted all of them. The source
// is healthy; the request asked for refs it does not have. Benign for some
// sources by design: a GitHub repository whose only refs are under refs/pull/*
// selects nothing once that namespace is excluded. Test for it with errors.Is.
//
// Exclusions are the only scoping mechanism that reaches it, and the opt-in is
// required — like every sentinel in this family, a caller that has not asked
// for the distinction keeps receiving the historical error. Branch selection
// cannot produce it, because AllowEmptySource requires RefScope.AllRefs and
// that clears any branch filter; nor can a ref mapping, because a mapping whose
// source ref is absent fails earlier, while the source is being planned.
//
// It is deliberately distinct from the empty-source errors below, which cover
// a source that ADVERTISED no refs — a weaker statement on purpose, since
// whether such a repository really holds none is not something a client can
// determine. Before these existed both cases shared one message and callers
// could not tell "nothing was advertised" from "nothing matched".
var ErrNoRefsSelected = syncer.ErrNoRefsSelected

// ErrSourceEmptyUnverified is returned (wrapped) by Replicate under
// SyncPolicy.AllowEmptySource when the source advertised no refs but its
// emptiness could not be established. It covers every way the evidence can
// fall short, not one of them:
//
//   - SyncPolicy.SourceAssertedEmpty was not supplied, so there is no
//     authoritative claim to act on;
//   - the source did not report an unborn HEAD, meaning HEAD's target exists
//     and a ref is therefore being withheld;
//   - ref-name validation dropped every advertised name, so a repository full
//     of refs git would reject arrives looking empty;
//   - a blank body behind a valid header, a server-side ref-listing or
//     hide-pattern regression, or a narrowed ref-prefix.
//
// Treat it as "unknown", never as "converged". Test for it with errors.Is.
var ErrSourceEmptyUnverified = syncer.ErrSourceEmptyUnverified

// ErrTargetEmptyUnverified is returned (wrapped) by Replicate under
// SyncPolicy.AllowEmptySource when the source was verified empty but the
// TARGET's emptiness could not be established — no SyncPolicy.TargetAssertedEmpty,
// or a target ref name dropped as invalid. Distinct from
// ErrSourceEmptyTargetPopulated, which is a target KNOWN to hold refs.
//
// An empty receive-pack advertisement proves no more than an empty ls-refs
// one: receive.hideRefs omits matching refs from it. And because
// receive.hideRefs and uploadpack.hideRefs are separate settings, a ref hidden
// from the push side is still served to fetchers — so a target wrongly judged
// empty is one whose readers see refs the source does not have. Test for it
// with errors.Is.
var ErrTargetEmptyUnverified = syncer.ErrTargetEmptyUnverified

// ErrSourceEmptyTargetPopulated is returned (wrapped) by Replicate under
// SyncPolicy.AllowEmptySource when the source is confirmed empty while the
// target still holds refs — a real divergence, since nothing the target serves
// exists on the source. Replicate refuses instead of converging: converging
// means deleting every ref on the target, and the states that produce this
// signature (a source restored from backup, a wipe, an out-of-band emptying)
// are the ones where the target may hold the only surviving copy. Test for it
// with errors.Is, and surface it as divergence rather than as "nothing to do".
var ErrSourceEmptyTargetPopulated = syncer.ErrSourceEmptyTargetPopulated

// ErrCheckpointExceedsTargetLimit is returned (wrapped) by Sync, Replicate and
// Bootstrap when a batched bootstrap reached a single commit whose pack the
// target itself refused. Checkpoint subdivision splits between commits, so at
// one commit per gap there is nothing left to split and every retry re-sends
// the identical pack. Test for it with
// errors.Is(err, gitsync.ErrCheckpointExceedsTargetLimit).
//
// Treat it as permanent: redelivering wastes a full source fetch per attempt
// to reproduce the same failure. The remedies are a larger receive-pack limit
// on the target, or splitting the commit's objects across packs — neither of
// which a retry can discover.
//
// Deliberately NOT returned when git-sync's own smaller batching budget
// stopped the upload: those runs are retried against the target's announced
// limit first, so this sentinel always carries a verdict from the target
// rather than from a threshold git-sync chose.
var ErrCheckpointExceedsTargetLimit = bootstrap.ErrCheckpointExceedsTargetLimit
