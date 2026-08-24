package syncer

import (
	"errors"
	"fmt"

	"entire.io/entire/git-sync/internal/planner"
)

// The empty-desired-set outcomes. Replicate reaches this family whenever it
// finds nothing to act on — either the source advertised no refs at all, or
// planning produced no desired refs — which happens for unrelated reasons the
// wire signal alone cannot tell apart: the source has no refs, the source has
// refs the requested scope excluded, or the source has refs this reader was
// never shown. Collapsing them into one error (as this package did before)
// forces every caller to guess, and they demand opposite handling — one may be
// a converged state, the others never are.
var (
	// ErrNoRefsSelected means the source DOES advertise refs, but the
	// requested exclusions (ExcludeRefPrefixes, ExcludeRefs) subtracted all of
	// them. Nothing is wrong with the source; the request asked for refs it
	// does not have. Benign and expected for some sources — e.g. a GitHub
	// repository whose only refs live under refs/pull/*, which mirror callers
	// deliberately exclude.
	//
	// Exclusions are the only scoping mechanism that can reach it. Branch
	// selection cannot: this family requires AllRefs, and normalizeAllRefs
	// clears cfg.Branches whenever AllRefs is set. Nor can a mapping, whose
	// absent source ref fails inside BuildDesiredRefs first.
	//
	// Its message deliberately avoids the historical "no source refs matched"
	// text. A caller that substring-matches that phrase (mirror-pipeline does,
	// to stay correct across a vendor bump) must not be able to read one of
	// these as the other, and keeping the texts disjoint means the order the
	// checks run in is not load-bearing.
	ErrNoRefsSelected = errors.New("source has refs but none matched the requested scope")

	// ErrSourceEmptyUnverified means planning found no desired refs and
	// nothing established that the source is actually empty, so emptiness
	// cannot be concluded. This is the default outcome, and deliberately the
	// wide one: an advertisement carrying no refs is not a claim that a
	// repository has none.
	//
	// The cases that land here are all real and all indistinguishable from an
	// empty repository by ref count alone:
	//
	//   - the caller supplied no authoritative assertion (SourceAssertedEmpty);
	//   - the source did not report an unborn HEAD, so HEAD's target exists
	//     and some ref is therefore being withheld;
	//   - ref-name validation dropped every advertised name
	//     (gitproto.PartitionRefNames), so a repository full of refs git would
	//     reject arrives looking empty;
	//   - a blank body behind a valid header, a server-side ref-listing or
	//     hide-pattern regression, or a narrowed ref-prefix.
	//
	// Callers must treat this as "unknown", never as "converged".
	ErrSourceEmptyUnverified = errors.New("source advertised no refs but its emptiness could not be verified")

	// ErrTargetEmptyUnverified means the source was verified empty but the
	// TARGET's emptiness could not be established, so whether the two are
	// converged is unknown. Distinct from ErrSourceEmptyTargetPopulated,
	// which is a target KNOWN to hold refs: this is a target that advertised
	// none and could not confirm it.
	//
	// The same asymmetry applies as on the source side, and it bites harder:
	// receive.hideRefs omits matching refs from receive-pack's advertisement,
	// so a populated target can advertise nothing but the capabilities^{}
	// sentinel. And because receive.hideRefs and uploadpack.hideRefs are
	// separate settings, such a ref is still served to fetchers — a target
	// wrongly judged empty is one whose readers see refs the source does not
	// have, which is precisely the divergence a convergence claim must never
	// paper over.
	ErrTargetEmptyUnverified = errors.New("target advertised no refs but its emptiness could not be verified")

	// ErrSourceEmptyTargetPopulated means the source is VERIFIED empty while
	// the target still holds refs. The two have genuinely diverged: whatever
	// the target serves does not exist on the source. Replicate refuses
	// rather than converging, because converging means deleting every ref on
	// the target and the states that produce this signature — a primary
	// restored from backup, a data-plane wipe, an out-of-band emptying —
	// are exactly the ones where the target may hold the only surviving copy.
	// The refusal is deliberately its own error so a caller can surface this
	// divergence instead of filing it under "nothing to do".
	ErrSourceEmptyTargetPopulated = errors.New("source is empty but the target still has refs")
)

// validateEmptySourcePolicy rejects an empty-source policy that no path would
// consult, at the one point every entry (Run, Bootstrap, Fetch, Probe) shares.
//
// Both conditions were previously accepted, threaded into this package, and
// then silently discarded: the caller set the policy, got the historical
// "no source refs matched", and had no way to learn the policy did nothing.
// Failing at the edge is the same treatment SyncPolicy.Validate already gives
// mode-specific force flags.
func validateEmptySourcePolicy(cfg Config) error {
	if !cfg.AllowEmptySource {
		// The assertions are inputs to this policy alone. Set without it they
		// are inert by design — the opt-in is what makes the outcome
		// reachable — so they are not an error on their own.
		return nil
	}
	if cfg.Mode != modeReplicate {
		return fmt.Errorf("AllowEmptySource applies to replicate only, got mode %q", cfg.Mode)
	}
	// Protocol v1 has no unborn-HEAD signal at all, so the corroboration this
	// policy requires can never be satisfied over it: every run would fail,
	// and with a message that reads as the source withholding refs. Refusing
	// the combination up front names the real cause — the caller's own
	// protocol selection. "auto" is accepted: it negotiates v2 wherever the
	// server supports it, and the SSH fallback to v1 is only discoverable
	// mid-run (see resolveEmptySource).
	if cfg.ProtocolMode == protocolModeV1 {
		return errors.New("AllowEmptySource requires protocol v2 on the source; v1 cannot report an unborn HEAD, so emptiness can never be corroborated")
	}
	// Under a narrower scope the source advertisement is itself narrowed (see
	// planner.RefPrefixes), so an empty one says nothing about the repository
	// as a whole — and the target's refs, which are never scope-filtered, are
	// not ours to judge against a partial view of the source.
	if !cfg.AllRefs {
		return errors.New("AllowEmptySource requires an unscoped request (AllRefs); a narrowed scope cannot establish that a repository is empty")
	}
	return nil
}

// resolveEmptyDesiredSet decides what an empty desired set means when planning
// produced no refs to act on.
//
// It is the post-planning entry point. An empty source ADVERTISEMENT is
// intercepted before planning (see runReplicate), because a mapping whose
// source ref is absent errors inside BuildDesiredRefs and would never let this
// run.
//
// Given that intercept, the delegation below is currently unreachable: getting
// here with an empty advertisement requires the opt-in to be off or the scope
// narrowed, and either returns the historical error one line earlier. It is
// kept rather than replaced with an assertion so the two entry points cannot
// drift apart if that gate is ever loosened — but do not read it as evidence
// that both are live today. Only the pre-planning path reaches convergence.
func (s *syncSession) resolveEmptyDesiredSet() (Result, error) {
	if !s.cfg.AllowEmptySource || !s.cfg.AllRefs {
		return Result{}, errors.New("no source refs matched")
	}
	if len(s.sourceRefMap) > 0 {
		return Result{}, ErrNoRefsSelected
	}
	return s.resolveEmptySource()
}

// resolveEmptySource is the only place that may conclude "the source is empty
// and we are converged".
//
// Emptiness is NOT established here. Git offers no way to prove it: ref hiding
// is designed to be invisible to the client, so a hidden ref and an absent one
// are the same observation, and an unborn HEAD says only that HEAD's target
// does not exist (a repository holding refs/heads/other with HEAD pointed at a
// never-created refs/heads/main reports unborn, and hiding that branch reduces
// its whole advertisement to the unborn line — verified against git 2.53). The
// assertion therefore has to come from the server's own repository state,
// which the caller supplies via Config.SourceAssertedEmpty; what this function
// does is refuse to act on that assertion unless everything git CAN observe
// agrees with it.
//
// Six conditions must all hold before this returns success:
//
//  1. The caller opted in (AllowEmptySource). Checked FIRST so that "off" is
//     structurally identical to the behavior that predates this function — one
//     error, one message — and not merely identical in the cases someone
//     thought to test. A caller that has not opted in cannot receive a
//     sentinel it has never heard of, which is what makes these errors safe to
//     introduce.
//  2. The request was unscoped (AllRefs). Under a narrower scope an empty
//     desired set says nothing about the repository as a whole, and the
//     target's refs — which are never scope-filtered — are not ours to judge
//     against a partial view of the source.
//  3. The caller asserted emptiness (SourceAssertedEmpty) from a source of
//     truth that sees past hiding.
//  4. Every git-side observation corroborates it: nothing advertised, HEAD
//     reported unborn, and no advertised name dropped as invalid. Each of
//     these can only ever REFUSE — none can promote an absent assertion into a
//     success — so the git signal is a consistency check on the caller's
//     claim, never a substitute for it.
//  5. The target advertises no refs THIS REQUEST WOULD MANAGE — anything
//     visible there is real, since hiding can conceal refs but never invent
//     them, so a visible ref means divergence. Refs the request excludes are
//     not ours to judge: the run would neither push nor prune them, and a
//     mirror that excludes refs/pull/* while its target holds only refs/pull/*
//     is converged over everything it manages. Counting them would make such a
//     mirror permanently unconvergeable over a ref it never touches.
//  6. The target's emptiness is asserted too (TargetAssertedEmpty) and
//     corroborated the same way. An empty receive-pack advertisement proves no
//     more than an empty ls-refs one: receive.hideRefs omits matching refs
//     from it, and since that is a separate setting from uploadpack.hideRefs,
//     a target wrongly judged empty may still be serving those refs to its
//     readers.
//
// Anything unmet fails closed: ErrSourceEmptyUnverified for the source half,
// ErrTargetEmptyUnverified for the target half, ErrSourceEmptyTargetPopulated
// for a target known to hold refs. Only condition 5 reports divergence; every
// other failure is "unknown".
func (s *syncSession) resolveEmptySource() (Result, error) {
	if !s.cfg.AllowEmptySource || !s.cfg.AllRefs {
		return Result{}, errors.New("no source refs matched")
	}
	if !s.cfg.SourceAssertedEmpty {
		return Result{}, fmt.Errorf("%w: no authoritative assertion from the source", ErrSourceEmptyUnverified)
	}
	if s.sourceService == nil {
		return Result{}, fmt.Errorf("%w: the source ref listing is unavailable", ErrSourceEmptyUnverified)
	}
	// Distinguish "the source COULD NOT report unborn" from "the source DID NOT
	// report it". Only the second says anything about the repository; the first
	// is a property of the protocol, and reporting it as the second tells the
	// operator their server is withholding refs — implying a hideRefs
	// misconfiguration or a compromised source — when nothing is wrong with it.
	//
	// A v1 request is refused before any I/O (see validateEmptySourcePolicy);
	// this catches what validation cannot see: an "auto" SSH source whose v2
	// probe failed and fell back to v1 mid-run.
	if reason, cannot := s.sourceCannotReportUnborn(); cannot {
		return Result{}, fmt.Errorf("%w: %s, so emptiness cannot be corroborated over this connection", ErrSourceEmptyUnverified, reason)
	}
	if !s.sourceService.HeadUnborn {
		// HEAD's target exists while nothing was advertised, so a ref is being
		// withheld — the caller's assertion disagrees with the wire and loses.
		return Result{}, fmt.Errorf("%w: source asserted empty but did not report an unborn HEAD", ErrSourceEmptyUnverified)
	}
	if n := s.sourceService.SkippedRefCount; n > 0 {
		return Result{}, fmt.Errorf("%w: source asserted empty but %d advertised ref name(s) were dropped as invalid", ErrSourceEmptyUnverified, n)
	}
	// Every remaining condition is about the target, and this function is
	// billed as the one place that may conclude convergence — so a session
	// built without a target (Fetch, a target-less Probe) must be refused
	// rather than dereferenced. runReplicate always has one; a future reuse
	// might not.
	if s.target == nil {
		return Result{}, fmt.Errorf("%w: no target was listed to compare against", ErrTargetEmptyUnverified)
	}
	// A target that advertises refs this request manages is populated, full
	// stop — hiding can only ever conceal refs, never invent them, so anything
	// visible here is real and this is divergence.
	inScope, err := s.targetRefsInScope()
	if err != nil {
		return Result{}, err
	}
	if inScope > 0 {
		return Result{}, fmt.Errorf("%w (%d in scope)", ErrSourceEmptyTargetPopulated, inScope)
	}
	// An EMPTY target advertisement proves nothing on its own, for the same
	// reason the source's did not, so it needs the same authoritative
	// assertion and the same corroboration.
	if !s.cfg.TargetAssertedEmpty {
		return Result{}, fmt.Errorf("%w: no authoritative assertion from the target", ErrTargetEmptyUnverified)
	}
	if n := s.target.skippedRefCount; n > 0 {
		return Result{}, fmt.Errorf("%w: target asserted empty but %d advertised ref name(s) were dropped as invalid", ErrTargetEmptyUnverified, n)
	}
	return Result{
		Plans:         []BranchPlan{},
		DryRun:        s.cfg.DryRun,
		OperationMode: modeReplicate,
		Protocol:      s.sourceService.Protocol,
		Converged:     true,
		// Replicate refuses a non-relay target outright, so every successful
		// replicate reports a relay; a consumer reading Relay=false with an
		// empty RelayMode would file this under "materialized fallback" or
		// "malformed". The reason names why nothing moved. SourceHEAD stays
		// empty on purpose: an unborn HEAD has no existing target branch, and
		// consumers read a non-empty SourceHEAD as one that exists.
		Relay:       true,
		RelayMode:   modeReplicate,
		RelayReason: "source-empty-converged",
		Stats:       s.stats.snapshot(),
		Measurement: s.measurementDone(),
	}, nil
}

// sourceCannotReportUnborn reports whether this connection is structurally
// incapable of carrying an unborn-HEAD signal, and why.
//
// HeadUnborn being false means only "not reported", which covers two very
// different situations. Over a connection that CAN report it, false is evidence
// against the caller's assertion — HEAD's target exists, so a ref is hidden.
// Over one that cannot, false is no evidence at all, and treating it as the
// former blames the server for the client's protocol.
func (s *syncSession) sourceCannotReportUnborn() (string, bool) {
	if s.sourceService.Protocol == protocolModeV1 {
		return "the source negotiated protocol v1, which has no unborn-HEAD signal", true
	}
	// Nil caps means a caller assembled this session without an advertisement
	// (tests do); only an advertisement that is present and lacks the feature
	// is evidence the server cannot report it.
	if caps := s.sourceService.V2Caps; caps != nil && !caps.LSRefsSupports("unborn") {
		return "the source does not advertise ls-refs=unborn", true
	}
	return "", false
}

// targetRefsInScope counts the target refs this request is responsible for.
//
// It must ask exactly the question the planner asks, so it delegates to
// planner.TargetScope rather than re-deriving the answer. Both directions of
// getting this wrong are real, and they fail oppositely: too narrow a scope
// leaves a mirror permanently diverged over a ref it would never touch, while
// too wide a scope — or, worse, a predicate that silently omits the mapping
// targets — converges over a target that still holds refs the source does not.
//
// The zero-hash skip stays here because it is about the value rather than the
// name: a zero hash is a deletion sentinel, not a ref that exists.
func (s *syncSession) targetRefsInScope() (int, error) {
	scope, err := planner.NewTargetScope(planConfig(s.cfg))
	if err != nil {
		// Unreachable in practice: newSession validates mappings before any
		// session exists. Surfaced rather than swallowed, because guessing a
		// scope here would mean guessing at divergence.
		return 0, fmt.Errorf("resolve target scope: %w", err)
	}
	n := 0
	for name, hash := range s.target.refMap {
		if hash.IsZero() {
			continue
		}
		if !scope.Manages(name) {
			continue
		}
		n++
	}
	return n, nil
}
