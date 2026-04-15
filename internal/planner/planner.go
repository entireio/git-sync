package planner

import (
	"errors"
	"fmt"
	"sort"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/entirehq/git-sync/internal/validation"
)

// PlanConfig holds configuration for plan generation.
type PlanConfig struct {
	Branches    []string
	Mappings    []RefMapping
	IncludeTags bool
	Force       bool
	Prune       bool
}

// BuildDesiredRefs constructs the set of desired refs and managed targets from
// source refs and user configuration. All mapping validation happens here,
// before any network activity (issue #2, #3).
func BuildDesiredRefs(
	sourceRefs map[plumbing.ReferenceName]plumbing.Hash,
	cfg PlanConfig,
) (map[plumbing.ReferenceName]DesiredRef, map[plumbing.ReferenceName]ManagedTarget, error) {
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
		normalized, err := validation.ValidateMappings(cfg.Mappings)
		if err != nil {
			return nil, nil, err
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
			if err := addManaged(refName, refName, RefKindBranch, hash); err != nil {
				return nil, nil, err
			}
		}
	}

	if cfg.IncludeTags {
		for refName, hash := range sourceRefs {
			if !refName.IsTag() {
				continue
			}
			if err := addManaged(refName, refName, RefKindTag, hash); err != nil {
				return nil, nil, err
			}
		}
	}

	return desired, managed, nil
}

// BuildPlans generates the action plans for each managed ref.
func BuildPlans(
	store storer.EncodedObjectStorer,
	desired map[plumbing.ReferenceName]DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
	managed map[plumbing.ReferenceName]ManagedTarget,
	cfg PlanConfig,
) ([]BranchPlan, error) {
	if cfg.Prune {
		for targetRef := range targetRefs {
			if _, ok := managed[targetRef]; ok {
				continue
			}
			switch {
			case targetRef.IsTag() && cfg.IncludeTags:
				managed[targetRef] = ManagedTarget{Kind: RefKindTag, Label: targetRef.Short()}
			case targetRef.IsBranch() && len(cfg.Mappings) == 0 && len(cfg.Branches) == 0:
				managed[targetRef] = ManagedTarget{Kind: RefKindBranch, Label: targetRef.Short()}
			}
		}
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
					Reason:     fmt.Sprintf("%s -> <deleted>", ShortHash(targetHash)),
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
				Reason:     fmt.Sprintf("%s -> <new>", ShortHash(want.SourceHash)),
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
func BuildReplicationPlans(
	desired map[plumbing.ReferenceName]DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
	managed map[plumbing.ReferenceName]ManagedTarget,
	cfg PlanConfig,
) ([]BranchPlan, error) {
	managed = copyManagedTargets(managed)
	if cfg.Prune {
		for targetRef := range targetRefs {
			if _, ok := managed[targetRef]; ok {
				continue
			}
			switch {
			case targetRef.IsTag() && cfg.IncludeTags:
				managed[targetRef] = ManagedTarget{Kind: RefKindTag, Label: targetRef.Short()}
			case targetRef.IsBranch() && len(cfg.Mappings) == 0 && len(cfg.Branches) == 0:
				managed[targetRef] = ManagedTarget{Kind: RefKindBranch, Label: targetRef.Short()}
			}
		}
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
					Reason:     fmt.Sprintf("%s -> <deleted>", ShortHash(targetHash)),
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
		plan.Reason = fmt.Sprintf("%s already current", ShortHash(want.SourceHash))
		return plan, nil
	}

	if want.Kind == RefKindTag {
		if force {
			plan.Action = ActionUpdate
			plan.Reason = fmt.Sprintf("%s -> %s (force tag update)", ShortHash(targetHash), ShortHash(want.SourceHash))
			return plan, nil
		}
		plan.Action = ActionBlock
		plan.Reason = fmt.Sprintf("%s differs from %s; use --force to retarget tag", ShortHash(targetHash), ShortHash(want.SourceHash))
		return plan, nil
	}

	isFF, err := ReachesCommit(store, want.SourceHash, targetHash)
	if err != nil {
		if errors.Is(err, ErrAncestryDepthExceeded) {
			// Can't prove fast-forward within depth limit — block with explanation.
			plan.Action = ActionBlock
			plan.Reason = fmt.Sprintf("ancestry check for %s exceeded depth limit; use --force if this is a valid fast-forward", want.TargetRef)
			return plan, nil
		}
		return plan, fmt.Errorf("check fast-forward for %s: %w", want.TargetRef, err)
	}
	if isFF {
		plan.Action = ActionUpdate
		plan.Reason = fmt.Sprintf("%s -> %s", ShortHash(targetHash), ShortHash(want.SourceHash))
		return plan, nil
	}

	if force {
		plan.Action = ActionUpdate
		plan.Reason = fmt.Sprintf("%s -> %s (force)", ShortHash(targetHash), ShortHash(want.SourceHash))
		return plan, nil
	}

	plan.Action = ActionBlock
	plan.Reason = fmt.Sprintf("%s is not an ancestor of %s", ShortHash(targetHash), ShortHash(want.SourceHash))
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
		plan.Reason = fmt.Sprintf("%s already current", ShortHash(want.SourceHash))
		return plan
	}

	if !existsOnTarget || targetHash.IsZero() {
		plan.Action = ActionCreate
		plan.Reason = fmt.Sprintf("%s -> <new>", ShortHash(want.SourceHash))
		return plan
	}

	plan.Action = ActionUpdate
	switch want.Kind {
	case RefKindTag:
		plan.Reason = fmt.Sprintf("%s -> %s (replicate tag overwrite)", ShortHash(targetHash), ShortHash(want.SourceHash))
	default:
		plan.Reason = fmt.Sprintf("%s -> %s (replicate overwrite)", ShortHash(targetHash), ShortHash(want.SourceHash))
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
				return false, err
			}
			stack = append(stack, parent)
		}
	}
	return false, nil
}

// ObjectsToPush computes the set of objects that need to be sent to the target.
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
	default:
		return fmt.Errorf("unsupported object type %s for %s", obj.Type(), hash)
	}

	*out = append(*out, hash)
	return nil
}
