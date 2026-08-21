package syncer

import (
	"errors"
	"fmt"
)

// The empty-desired-set outcomes. Replicate reaches this family whenever
// planning produces no desired refs, which happens for two unrelated reasons
// that the wire signal alone cannot tell apart: the source has no refs, or the
// source has refs and the requested scope excluded all of them. Collapsing
// them into one error (as this package did before) forces every caller to
// guess, and the two demand opposite handling — one may be a converged state,
// the other never is.
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

	// ErrSourceEmptyUnverified means planning found no desired refs AND the
	// source advertised no refs, but nothing asserted that the source is
	// actually empty, so the emptiness cannot be trusted. Absence of refs in
	// a response is not the same claim as a repository reporting it has no
	// commits: a blank body behind a valid header, a server-side ref-listing
	// or hide-pattern regression, or a narrowed ref-prefix all produce the
	// same silence. So does ref-name validation dropping every advertised
	// name (gitproto.PartitionRefNames) — a repository full of refs whose
	// names git would reject arrives here indistinguishable from an empty
	// one, which is exactly why the unborn assertion and not the ref count
	// is what decides. Callers must treat this as "unknown", never as
	// "converged".
	ErrSourceEmptyUnverified = errors.New("source advertised no refs but did not confirm it is empty")

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
// Emptiness is established from positive evidence, never inferred from a
// response carrying no refs (see ErrSourceEmptyUnverified). Three conditions
// must all hold before this returns success:
//
//  1. The caller opted in (AllowEmptySource). Off by default so no existing
//     caller's contract changes.
//  2. The request was unscoped (AllRefs). Under a narrower scope an empty
//     desired set says nothing about the repository as a whole, and the
//     target's refs — which are never scope-filtered — are not ours to judge
//     against a partial view of the source.
//  3. The source asserted an unborn HEAD (RefService.SourceUnborn), i.e. the
//     server itself reported that the repository has no commits.
//
// With all three satisfied, the source is known to hold nothing; whether that
// is a converged state then depends on the target, which the session has
// already listed. An empty target means the two match with nothing to apply,
// and the zero-plan success carries SourceEmpty so the caller can tell this
// apart from an ordinary no-op sync. A populated target means real divergence
// and returns ErrSourceEmptyTargetPopulated.
//
// A caller that has not opted in (or asked for a narrower scope) falls back to
// the historical error text and sees byte-identical behavior to before this
// existed. An opted-in caller talking to a source that cannot make the
// assertion gets ErrSourceEmptyUnverified instead — same refusal to conclude
// anything, but named, because for such a caller the missing assertion is
// itself worth knowing about.
func (s *syncSession) resolveEmptyDesiredSet() (Result, error) {
	// The opt-in gate comes FIRST so that "off" is structurally identical to
	// the behavior that predates this function — one error, one message — and
	// not merely identical in the cases anyone thought to check. A caller that
	// has not opted in cannot receive a sentinel it has never heard of, which
	// is what makes the new errors safe to introduce: only code that asked for
	// the distinction has to know how to classify it.
	if !s.cfg.AllowEmptySource || !s.cfg.AllRefs {
		return Result{}, errors.New("no source refs matched")
	}
	if len(s.sourceRefMap) > 0 {
		return Result{}, ErrNoRefsSelected
	}
	if s.sourceService == nil || !s.sourceService.SourceUnborn {
		return Result{}, ErrSourceEmptyUnverified
	}
	if len(s.target.refMap) > 0 {
		return Result{}, fmt.Errorf("%w (%d)", ErrSourceEmptyTargetPopulated, len(s.target.refMap))
	}
	return Result{
		Plans:         []BranchPlan{},
		OperationMode: modeReplicate,
		Protocol:      s.sourceService.Protocol,
		SourceEmpty:   true,
		Stats:         s.stats.snapshot(),
		Measurement:   s.measurementDone(),
	}, nil
}
