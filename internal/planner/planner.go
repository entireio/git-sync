package planner

import (
	"errors"
	"fmt"
	"sort"

	"entire.io/entire/git-sync/internal/validation"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

// PlanConfig holds configuration for plan generation.
type PlanConfig struct {
	Branches    []string
	Mappings    []RefMapping
	IncludeTags bool
	Force       bool
	Prune       bool
	// AllRefs broadens the desired set to every refs/* on the source
	// (notes, pulls, replace, custom namespaces) in addition to whatever
	// branches/tags the existing flags select. Mappings can rename refs
	// in any namespace when AllRefs is set; otherwise only refs/heads/
	// and refs/tags/ are accepted.
	AllRefs bool
	// ExcludeRefPrefixes subtracts namespaces from auto-discovery. A ref
	// whose name starts with any of these prefixes is not pulled, pushed,
	// or pruned. Explicit Mappings are not subject to this filter.
	ExcludeRefPrefixes []string
	// ExcludeRefs subtracts exact ref names (whole-name match) from
	// auto-discovery, same effect as ExcludeRefPrefixes but without matching
	// children — so a caller can reserve refs/heads/entire while still
	// mirroring refs/heads/entire/foo. Explicit Mappings are not subject to it.
	ExcludeRefs []string
}

// BuildDesiredRefs constructs the set of desired refs and managed targets from
// source refs and user configuration. All mapping validation happens here,
// before any network activity (issue #2, #3).
func BuildDesiredRefs(
	sourceRefs map[plumbing.ReferenceName]plumbing.Hash,
	cfg PlanConfig,
) (map[plumbing.ReferenceName]DesiredRef, map[plumbing.ReferenceName]ManagedTarget, error) {
	cfg = normalizeAllRefs(cfg)
	desired := make(map[plumbing.ReferenceName]DesiredRef)
	managed := make(map[plumbing.ReferenceName]ManagedTarget)

	addManaged := func(sourceRef, targetRef plumbing.ReferenceName, kind RefKind, hash plumbing.Hash) error {
		if hash.IsZero() {
			return fmt.Errorf("source ref %s not found", sourceRef)
		}
		// Reject duplicate target refs from different sources (issue #2, #3).
		if existing, ok := desired[targetRef]; ok && existing.SourceRef != sourceRef {
			return fmt.Errorf("duplicate target ref %s: mapped from both %s and %s", targetRef, existing.SourceRef, sourceRef)
		}
		short := targetRef.Short()
		desired[targetRef] = DesiredRef{
			Kind:       kind,
			Label:      short,
			SourceRef:  sourceRef,
			TargetRef:  targetRef,
			SourceHash: hash,
		}
		managed[targetRef] = ManagedTarget{Kind: kind, Label: short}
		return nil
	}

	if len(cfg.Mappings) > 0 {
		// Validate all mappings up front (issue #2, #3)
		normalized, err := validation.ValidateMappings(cfg.Mappings, cfg.AllRefs)
		if err != nil {
			return nil, nil, fmt.Errorf("validate ref mappings: %w", err)
		}
		for _, nm := range normalized {
			kind := RefKindFromName(nm.TargetRef)
			if err := addManaged(nm.SourceRef, nm.TargetRef, kind, sourceRefs[nm.SourceRef]); err != nil {
				return nil, nil, err
			}
		}
	} else {
		branches := BranchMapFromRefHashMap(sourceRefs)
		selected := SelectBranches(branches, cfg.Branches)
		for branch, hash := range selected {
			refName := plumbing.NewBranchReferenceName(branch)
			if IsRefExcluded(refName, cfg.ExcludeRefPrefixes, cfg.ExcludeRefs) {
				continue
			}
			if err := addManaged(refName, refName, RefKindBranch, hash); err != nil {
				return nil, nil, err
			}
		}
	}

	// AllRefs implies tag inclusion. Both passes (tag, other-kind) walk
	// sourceRefs once: under AllRefs+IncludeTags this saves a redundant
	// iteration on repos with thousands of refs/changes/* or refs/notes/*.
	wantTags := cfg.IncludeTags || cfg.AllRefs
	if wantTags || cfg.AllRefs {
		for refName, hash := range sourceRefs {
			kind := RefKindFromName(refName)
			switch {
			case kind == RefKindTag && wantTags:
			case kind == RefKindOther && cfg.AllRefs:
			default:
				continue
			}
			if IsRefExcluded(refName, cfg.ExcludeRefPrefixes, cfg.ExcludeRefs) {
				continue
			}
			if _, ok := desired[refName]; ok {
				continue
			}
			if err := addManaged(refName, refName, kind, hash); err != nil {
				return nil, nil, err
			}
		}
	}

	return desired, managed, nil
}

// normalizeAllRefs zeros Branches under AllRefs so the desired-set and
// prune predicates agree on scope.
func normalizeAllRefs(cfg PlanConfig) PlanConfig {
	if cfg.AllRefs {
		cfg.Branches = nil
	}
	return cfg
}

// BuildPlans generates the action plans for each managed ref.
func BuildPlans(
	store storer.EncodedObjectStorer,
	desired map[plumbing.ReferenceName]DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
	managed map[plumbing.ReferenceName]ManagedTarget,
	cfg PlanConfig,
) ([]BranchPlan, error) {
	cfg = normalizeAllRefs(cfg)
	if cfg.Prune {
		// addPruneCandidates mutates the map, so copy first — the caller's
		// managed map must not be modified (matches BuildReplicationPlans).
		managed = copyManagedTargets(managed)
		addPruneCandidates(managed, targetRefs, cfg)
	}

	targetNames := make([]plumbing.ReferenceName, 0, len(managed))
	for name := range managed {
		targetNames = append(targetNames, name)
	}
	sort.Slice(targetNames, func(i, j int) bool { return targetNames[i] < targetNames[j] })

	plans := make([]BranchPlan, 0, len(targetNames))
	for _, targetRef := range targetNames {
		info := managed[targetRef]
		want, existsInDesired := desired[targetRef]
		targetHash, existsOnTarget := targetRefs[targetRef]

		if !existsInDesired {
			if cfg.Prune && existsOnTarget {
				plans = append(plans, BranchPlan{
					Branch:     info.Label,
					TargetRef:  targetRef,
					TargetHash: targetHash,
					Kind:       info.Kind,
					Action:     ActionDelete,
					Reason:     ShortHash(targetHash) + " -> <deleted>",
				})
			}
			continue
		}

		if !existsOnTarget {
			plans = append(plans, BranchPlan{
				Branch:     want.Label,
				SourceRef:  want.SourceRef,
				TargetRef:  want.TargetRef,
				SourceHash: want.SourceHash,
				Kind:       want.Kind,
				Action:     ActionCreate,
				Reason:     ShortHash(want.SourceHash) + " -> <new>",
			})
			continue
		}

		plan, err := PlanRef(store, want, targetHash, cfg.Force)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}

	sort.Slice(plans, func(i, j int) bool {
		return plans[i].TargetRef.String() < plans[j].TargetRef.String()
	})
	return plans, nil
}

// BuildReplicationPlans generates overwrite-oriented plans for replication mode.
// Divergent refs are updated directly rather than blocked.
//
//nolint:unparam // error return kept for API consistency with BuildPlans
func BuildReplicationPlans(
	desired map[plumbing.ReferenceName]DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
	managed map[plumbing.ReferenceName]ManagedTarget,
	cfg PlanConfig,
) ([]BranchPlan, error) {
	cfg = normalizeAllRefs(cfg)
	if cfg.Prune {
		// Copy before the only mutation so the caller's managed map is left
		// untouched (matches BuildPlans).
		managed = copyManagedTargets(managed)
		addPruneCandidates(managed, targetRefs, cfg)
	}

	targetNames := make([]plumbing.ReferenceName, 0, len(managed))
	for name := range managed {
		targetNames = append(targetNames, name)
	}
	sort.Slice(targetNames, func(i, j int) bool { return targetNames[i] < targetNames[j] })

	plans := make([]BranchPlan, 0, len(targetNames))
	for _, targetRef := range targetNames {
		info := managed[targetRef]
		want, existsInDesired := desired[targetRef]
		targetHash, existsOnTarget := targetRefs[targetRef]

		if !existsInDesired {
			if cfg.Prune && existsOnTarget {
				plans = append(plans, BranchPlan{
					Branch:     info.Label,
					TargetRef:  targetRef,
					TargetHash: targetHash,
					Kind:       info.Kind,
					Action:     ActionDelete,
					Reason:     ShortHash(targetHash) + " -> <deleted>",
				})
			}
			continue
		}

		plans = append(plans, PlanReplicationRef(want, targetHash, existsOnTarget))
	}

	sort.Slice(plans, func(i, j int) bool {
		return plans[i].TargetRef.String() < plans[j].TargetRef.String()
	})
	return plans, nil
}

// TargetScope answers whether a target ref is a request's responsibility at
// all. Build it once per request with NewTargetScope; Manages is then O(1).
//
// Two ways a ref qualifies, and both are needed. It may be a ref the request
// explicitly MANAGES — a mapping target, which BuildDesiredRefs adds to the
// managed set — or one prune could select. PruneTarget alone answers only the
// second, and deliberately reports false for every branch once Mappings is set,
// because addPruneCandidates consults it only for refs already known to be
// unmanaged. Reading it as the whole answer silently drops the mapping targets,
// which is the worst direction for anything deciding divergence: a target still
// holding the mapped ref looks like nothing to worry about.
type TargetScope struct {
	cfg    PlanConfig
	mapped map[plumbing.ReferenceName]struct{}
}

// NewTargetScope precomputes the mapping targets a request declares. It
// normalizes cfg and resolves mapping names the same way BuildDesiredRefs
// does, so the two cannot disagree about what a mapping names.
func NewTargetScope(cfg PlanConfig) (TargetScope, error) {
	cfg = normalizeAllRefs(cfg)
	scope := TargetScope{cfg: cfg, mapped: map[plumbing.ReferenceName]struct{}{}}
	if len(cfg.Mappings) == 0 {
		return scope, nil
	}
	normalized, err := validation.ValidateMappings(cfg.Mappings, cfg.AllRefs)
	if err != nil {
		return TargetScope{}, fmt.Errorf("validate ref mappings: %w", err)
	}
	for _, nm := range normalized {
		scope.mapped[nm.TargetRef] = struct{}{}
	}
	return scope, nil
}

// Manages reports whether this target ref is the request's responsibility —
// that is, whether the request would push to it, or whether prune could select
// it were prune enabled.
//
// Deliberately independent of cfg.Prune. A caller asking "is this ref mine"
// while prune is off still needs the same answer: the divergence check reads
// it to decide whether a target ref contradicts a claim of convergence, and
// "the source is empty but the target holds refs" is a genuine disagreement
// whether or not this particular run would have deleted them.
//
// Push scope and prune scope are not the same set, and this is their union.
// Delegating wholly to PruneTarget was wrong twice over, in the direction that
// under-reports: it omits mapping targets, and it omits the refs
// BuildDesiredRefs auto-discovers under Mappings. That pass — tags, and
// other-kind names under AllRefs — sits OUTSIDE the mapping/branch branch, so
// a mapping-scoped AllRefs request still mirrors refs/notes/* and tags even
// though prune leaves them alone.
func (s TargetScope) Manages(targetRef plumbing.ReferenceName) bool {
	// Mapping targets are not subject to exclusions, matching the mapping pass
	// in BuildDesiredRefs, which applies exclusions only to auto-discovery.
	if _, ok := s.mapped[targetRef]; ok {
		return true
	}
	if IsRefExcluded(targetRef, s.cfg.ExcludeRefPrefixes, s.cfg.ExcludeRefs) {
		return false
	}
	// The auto-discovery half: what BuildDesiredRefs would push, regardless of
	// whether prune would also take it.
	switch kind := RefKindFromName(targetRef); {
	case kind == RefKindTag && (s.cfg.IncludeTags || s.cfg.AllRefs):
		return true
	case kind == RefKindOther && s.cfg.AllRefs:
		return true
	}
	// The prune half, which is what additionally covers unmapped branches.
	_, prunable := PruneTarget(targetRef, s.cfg)
	return prunable
}

// PruneTarget reports whether prune could select an ALREADY-UNMANAGED target
// ref for deletion, and with what classification.
//
// It is not the answer to "is this ref in scope" — with Mappings set it reports
// false for every branch, because its only caller has already excluded the
// managed ones. Use TargetScope.Manages for that question.
//
// It normalizes cfg itself rather than assuming a normalized one. Callers
// inside planning have already normalized (this is then a no-op), but the
// planner's own entry points normalize on the way in, so an outside caller
// holding a raw config has no obvious cue that it must. Getting it wrong is
// silent and one-directional: an un-normalized AllRefs config still carrying a
// Branches filter reports a branch as unmanaged when the request would in fact
// prune it.
//
// This is the single answer to "is this target ref ours to act on", and callers
// outside planning need it too: anything reasoning about what the target holds
// must ask the same question the planner will, or it reports refs the request
// has disclaimed. Exclusions are part of the answer, not a separate pre-filter,
// for the same reason.
//
// Note what mappings do here: a mapping-scoped request manages the refs it
// mapped and, for prune purposes, tags — but not unmapped branches and not
// other namespaces, which it neither pushes nor prunes.
func PruneTarget(targetRef plumbing.ReferenceName, cfg PlanConfig) (ManagedTarget, bool) {
	cfg = normalizeAllRefs(cfg)
	if IsRefExcluded(targetRef, cfg.ExcludeRefPrefixes, cfg.ExcludeRefs) {
		return ManagedTarget{}, false
	}
	switch {
	case targetRef.IsTag() && (cfg.IncludeTags || cfg.AllRefs):
		return ManagedTarget{Kind: RefKindTag, Label: targetRef.Short()}, true
	case targetRef.IsBranch() && len(cfg.Mappings) == 0 && len(cfg.Branches) == 0:
		return ManagedTarget{Kind: RefKindBranch, Label: targetRef.Short()}, true
	case cfg.AllRefs && RefKindFromName(targetRef) == RefKindOther && len(cfg.Mappings) == 0:
		return ManagedTarget{Kind: RefKindOther, Label: targetRef.Short()}, true
	}
	return ManagedTarget{}, false
}

// addPruneCandidates registers unmanaged target refs as deletion candidates
// within the user's current scope. cfg is assumed normalized.
func addPruneCandidates(managed map[plumbing.ReferenceName]ManagedTarget, targetRefs map[plumbing.ReferenceName]plumbing.Hash, cfg PlanConfig) {
	for targetRef := range targetRefs {
		if _, ok := managed[targetRef]; ok {
			continue
		}
		if target, prunable := PruneTarget(targetRef, cfg); prunable {
			managed[targetRef] = target
		}
	}
}

func copyManagedTargets(input map[plumbing.ReferenceName]ManagedTarget) map[plumbing.ReferenceName]ManagedTarget {
	out := make(map[plumbing.ReferenceName]ManagedTarget, len(input))
	for k, v := range input {
		out[k] = v
	}
	return out
}

// BuildBootstrapPlans creates plans for an empty-target bootstrap.
func BuildBootstrapPlans(
	desired map[plumbing.ReferenceName]DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) ([]BranchPlan, error) {
	targetNames := make([]plumbing.ReferenceName, 0, len(desired))
	for _, want := range desired {
		targetNames = append(targetNames, want.TargetRef)
	}
	sort.Slice(targetNames, func(i, j int) bool { return targetNames[i] < targetNames[j] })

	plans := make([]BranchPlan, 0, len(targetNames))
	for _, targetRef := range targetNames {
		targetHash := targetRefs[targetRef]
		if !targetHash.IsZero() {
			return nil, fmt.Errorf("target ref %s already exists; use sync for non-bootstrap runs", targetRef)
		}
		want := desired[targetRef]
		plans = append(plans, BranchPlan{
			Branch:     want.Label,
			SourceRef:  want.SourceRef,
			TargetRef:  want.TargetRef,
			SourceHash: want.SourceHash,
			TargetHash: plumbing.ZeroHash,
			Kind:       want.Kind,
			Action:     ActionCreate,
			Reason:     fmt.Sprintf("create %s at %s", want.TargetRef, ShortHash(want.SourceHash)),
		})
	}
	return plans, nil
}

// PlanRef determines the action for a single ref that exists on both source and target.
func PlanRef(store storer.EncodedObjectStorer, want DesiredRef, targetHash plumbing.Hash, force bool) (BranchPlan, error) {
	plan := BranchPlan{
		Branch:     want.Label,
		SourceRef:  want.SourceRef,
		TargetRef:  want.TargetRef,
		SourceHash: want.SourceHash,
		TargetHash: targetHash,
		Kind:       want.Kind,
	}

	if want.SourceHash == targetHash {
		plan.Action = ActionSkip
		plan.Reason = ShortHash(want.SourceHash) + " already current"
		return plan, nil
	}

	// Tags and other-kind refs (notes, pulls, custom namespaces) don't
	// generally form fast-forward chains — a notes append creates a new
	// commit that isn't an ancestor of the previous notes tip. Treat
	// them the same way: require --force to retarget rather than
	// running an ancestry check that would always fail.
	if want.Kind == RefKindTag || want.Kind == RefKindOther {
		if force {
			plan.Action = ActionUpdate
			plan.Reason = ShortHash(targetHash) + " -> " + ShortHash(want.SourceHash) + " (force " + string(want.Kind) + " update)"
			return plan, nil
		}
		plan.Action = ActionBlock
		plan.Reason = ShortHash(targetHash) + " differs from " + ShortHash(want.SourceHash) + "; use --force-with-lease to update " + string(want.Kind) + " ref " + want.TargetRef.String()
		return plan, nil
	}

	isFF, err := ReachesCommit(store, want.SourceHash, targetHash)
	if err != nil {
		if errors.Is(err, ErrAncestryDepthExceeded) {
			// Can't prove fast-forward within depth limit — block with explanation.
			plan.Action = ActionBlock
			plan.Reason = "ancestry check for " + want.TargetRef.String() + " exceeded depth limit; use --force-with-lease if this is a valid fast-forward"
			return plan, nil
		}
		return plan, fmt.Errorf("check fast-forward for %s: %w", want.TargetRef, err)
	}
	if isFF {
		plan.Action = ActionUpdate
		plan.Reason = ShortHash(targetHash) + " -> " + ShortHash(want.SourceHash)
		return plan, nil
	}

	if force {
		plan.Action = ActionUpdate
		plan.Reason = ShortHash(targetHash) + " -> " + ShortHash(want.SourceHash) + " (force)"
		return plan, nil
	}

	plan.Action = ActionBlock
	plan.Reason = ShortHash(targetHash) + " is not an ancestor of " + ShortHash(want.SourceHash)
	return plan, nil
}

// PlanReplicationRef determines the overwrite-oriented action for a single ref.
func PlanReplicationRef(want DesiredRef, targetHash plumbing.Hash, existsOnTarget bool) BranchPlan {
	plan := BranchPlan{
		Branch:     want.Label,
		SourceRef:  want.SourceRef,
		TargetRef:  want.TargetRef,
		SourceHash: want.SourceHash,
		TargetHash: targetHash,
		Kind:       want.Kind,
	}

	if want.SourceHash == targetHash {
		plan.Action = ActionSkip
		plan.Reason = ShortHash(want.SourceHash) + " already current"
		return plan
	}

	if !existsOnTarget || targetHash.IsZero() {
		plan.Action = ActionCreate
		plan.Reason = ShortHash(want.SourceHash) + " -> <new>"
		return plan
	}

	plan.Action = ActionUpdate
	switch want.Kind {
	case RefKindTag:
		plan.Reason = ShortHash(targetHash) + " -> " + ShortHash(want.SourceHash) + " (replicate tag overwrite)"
	case RefKindBranch, RefKindOther:
		plan.Reason = ShortHash(targetHash) + " -> " + ShortHash(want.SourceHash) + " (replicate overwrite)"
	}
	return plan
}

// MaxAncestryDepth is the maximum number of commits to visit during a
// fast-forward ancestry check. This prevents full graph walks on very
// large histories (issue #16). Set high enough to avoid false negatives
// on real repos — even the Linux kernel has ~1.3M commits.
const MaxAncestryDepth = 2_000_000

// ErrAncestryDepthExceeded is returned when ReachesCommit exceeds MaxAncestryDepth.
var ErrAncestryDepthExceeded = errors.New("ancestry check exceeded depth limit")

// ReachesCommit checks whether the commit at startHash has targetHash as an
// ancestor, bounded to MaxAncestryDepth commits to prevent degenerate walks.
func ReachesCommit(store storer.EncodedObjectStorer, startHash, targetHash plumbing.Hash) (bool, error) {
	if startHash == targetHash {
		return true, nil
	}

	start, err := object.GetCommit(store, startHash)
	if err != nil {
		return false, fmt.Errorf("load source commit %s: %w", startHash, err)
	}

	seen := map[plumbing.Hash]struct{}{}
	stack := []*object.Commit{start}

	for len(stack) > 0 {
		if len(seen) >= MaxAncestryDepth {
			return false, ErrAncestryDepthExceeded
		}
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := seen[current.Hash]; ok {
			continue
		}
		seen[current.Hash] = struct{}{}

		for _, parentHash := range current.ParentHashes {
			if parentHash == targetHash {
				return true, nil
			}
			if _, ok := seen[parentHash]; ok {
				continue
			}
			parent, err := object.GetCommit(store, parentHash)
			if err != nil {
				if errors.Is(err, plumbing.ErrObjectNotFound) {
					continue
				}
				return false, fmt.Errorf("load parent commit %s: %w", parentHash, err)
			}
			stack = append(stack, parent)
		}
	}
	return false, nil
}

// ObjectsToPush computes the set of objects that need to be sent to the target.
//
// A fetch with target refs as haves prunes the source pack server-side, so
// objects reachable from a have are intentionally absent from the local
// store. Both top-level wants and transitive references encountered during
// the walk may be missing for this reason — they're treated as implicitly
// have'd by the target and excluded from the push pack. The target's
// receive-pack accepts ref updates referencing such objects because it
// already has them under one of its existing refs.
func ObjectsToPush(store storer.EncodedObjectStorer, wants []plumbing.Hash, targetRefs map[plumbing.ReferenceName]plumbing.Hash) ([]plumbing.Hash, error) {
	haveSet := make(map[plumbing.Hash]struct{})
	for _, h := range targetRefs {
		if !h.IsZero() {
			haveSet[h] = struct{}{}
		}
	}

	filteredWants := make([]plumbing.Hash, 0, len(wants))
	for _, h := range wants {
		if _, ok := haveSet[h]; !ok {
			filteredWants = append(filteredWants, h)
		}
	}
	if len(filteredWants) == 0 {
		return nil, nil
	}

	seen := make(map[plumbing.Hash]bool, len(filteredWants)*4)
	objects := make([]plumbing.Hash, 0, len(filteredWants)*16)
	for _, h := range filteredWants {
		if err := collectObjects(store, h, haveSet, seen, &objects); err != nil {
			return nil, err
		}
	}
	return objects, nil
}

func collectObjects(
	store storer.EncodedObjectStorer,
	hash plumbing.Hash,
	haves map[plumbing.Hash]struct{},
	seen map[plumbing.Hash]bool,
	out *[]plumbing.Hash,
) error {
	if hash.IsZero() {
		return nil
	}
	if _, ok := haves[hash]; ok {
		return nil
	}
	if seen[hash] {
		return nil
	}
	seen[hash] = true

	obj, err := store.EncodedObject(plumbing.AnyObject, hash)
	if err != nil {
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			// Reachable from a target-ref have via the source server's
			// pack-prune; the target already has it, so it stays out
			// of the push.
			return nil
		}
		return fmt.Errorf("load object %s: %w", hash, err)
	}

	switch obj.Type() {
	case plumbing.CommitObject:
		commit, err := object.GetCommit(store, hash)
		if err != nil {
			return fmt.Errorf("load commit %s: %w", hash, err)
		}
		if err := collectObjects(store, commit.TreeHash, haves, seen, out); err != nil {
			return err
		}
		for _, ph := range commit.ParentHashes {
			if err := collectObjects(store, ph, haves, seen, out); err != nil {
				return err
			}
		}
	case plumbing.TreeObject:
		tree, err := object.GetTree(store, hash)
		if err != nil {
			return fmt.Errorf("load tree %s: %w", hash, err)
		}
		for _, entry := range tree.Entries {
			if err := collectObjects(store, entry.Hash, haves, seen, out); err != nil {
				return err
			}
		}
	case plumbing.TagObject:
		tag, err := object.GetTag(store, hash)
		if err != nil {
			return fmt.Errorf("load tag %s: %w", hash, err)
		}
		if err := collectObjects(store, tag.Target, haves, seen, out); err != nil {
			return err
		}
	case plumbing.BlobObject:
		// Blobs are leaf objects — nothing to recurse into.
	case plumbing.InvalidObject, plumbing.OFSDeltaObject, plumbing.REFDeltaObject, plumbing.AnyObject:
		return fmt.Errorf("unsupported object type %s for %s", obj.Type(), hash)
	}

	*out = append(*out, hash)
	return nil
}
