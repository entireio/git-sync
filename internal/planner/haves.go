package planner

import (
	"github.com/go-git/go-git/v6/plumbing"
)

// FetchHaves returns the target refs whose tips a relay fetch offers the
// source as haves. The source leaves out every object reachable from a have,
// so the default is every target tip.
//
// A ref under one of independentPrefixes carries its own history, sharing
// nothing with any other ref, so an unchanged one cannot shorten a pack the
// run needs and is left out. The one such tip that can help is the old value
// of a ref the run updates: the new history usually builds on it. With no
// prefixes the input map is returned as is.
func FetchHaves(
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
	plans []BranchPlan,
	independentPrefixes []string,
) map[plumbing.ReferenceName]plumbing.Hash {
	if len(independentPrefixes) == 0 {
		return targetRefs
	}
	updated := make(map[plumbing.ReferenceName]struct{}, len(plans))
	for _, plan := range plans {
		if plan.Action == ActionUpdate {
			updated[plan.TargetRef] = struct{}{}
		}
	}
	haves := make(map[plumbing.ReferenceName]plumbing.Hash, len(targetRefs))
	for name, hash := range targetRefs {
		if hasAnyPrefix(name, independentPrefixes) {
			if _, ok := updated[name]; !ok {
				continue
			}
		}
		haves[name] = hash
	}
	return haves
}
