package syncer

import (
	"errors"
	"fmt"
)

// The empty-desired-set outcomes. Replicate reaches this family whenever
// planning produces no desired refs, which happens for unrelated reasons the
// wire signal alone cannot tell apart: the source has no refs, the source has
// refs the requested scope excluded, or the source has refs this reader was
// never shown. Collapsing them into one error (as this package did before)
// forces every caller to guess, and they demand opposite handling — one may be
// a converged state, the others never are.
var (
	// ErrNoRefsSelected means the source DOES advertise refs, but the
	// requested scope (branch selection, mappings, exclude prefixes) matched
	// none of them. Nothing is wrong with the source; the request asked for
	// refs it does not have. Benign and expected for some sources — e.g. a
	// GitHub repository whose only refs live under refs/pull/*, which mirror
	// callers deliberately exclude.
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

// resolveEmptyDesiredSet decides what an empty desired set means, and is the
// only place that may conclude "the source is empty and we are converged".
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
// Five conditions must all hold before this returns success:
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
//  5. The target has no refs either, which is what makes the state converged
//     rather than divergent.
//
// Anything unmet fails closed, to ErrSourceEmptyUnverified or (for a populated
// target) ErrSourceEmptyTargetPopulated. Only condition 5 distinguishes
// "converged" from "diverged"; every other failure is "unknown".
func (s *syncSession) resolveEmptyDesiredSet() (Result, error) {
	if !s.cfg.AllowEmptySource || !s.cfg.AllRefs {
		return Result{}, errors.New("no source refs matched")
	}
	if len(s.sourceRefMap) > 0 {
		return Result{}, ErrNoRefsSelected
	}
	if !s.cfg.SourceAssertedEmpty {
		return Result{}, fmt.Errorf("%w: no authoritative assertion from the source", ErrSourceEmptyUnverified)
	}
	if s.sourceService == nil || !s.sourceService.HeadUnborn {
		// HEAD's target exists while nothing was advertised, so a ref is being
		// withheld — the caller's assertion disagrees with the wire and loses.
		return Result{}, fmt.Errorf("%w: source asserted empty but did not report an unborn HEAD", ErrSourceEmptyUnverified)
	}
	if n := len(s.sourceService.SkippedRefNames); n > 0 {
		return Result{}, fmt.Errorf("%w: source asserted empty but %d advertised ref name(s) were dropped as invalid", ErrSourceEmptyUnverified, n)
	}
	if len(s.target.refMap) > 0 {
		return Result{}, fmt.Errorf("%w (%d)", ErrSourceEmptyTargetPopulated, len(s.target.refMap))
	}
	return Result{
		Plans:         []BranchPlan{},
		DryRun:        s.cfg.DryRun,
		OperationMode: modeReplicate,
		Protocol:      s.sourceService.Protocol,
		SourceEmpty:   true,
		Stats:         s.stats.snapshot(),
		Measurement:   s.measurementDone(),
	}, nil
}
