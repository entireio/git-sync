package planner

import (
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

// TargetScope decides whether a target ref is the request's responsibility, and
// the divergence check in internal/syncer reads it to decide whether a target
// ref contradicts a claim that two repositories agree. Its semantics were wrong
// in three consecutive commits, each time because the answer was derived from
// one half of the question — so the contract is pinned here, next to the code,
// rather than only indirectly through the syncer.
func TestTargetScopeManages(t *testing.T) {
	t.Parallel()

	mapping := []RefMapping{{Source: "refs/heads/main", Target: "refs/heads/main"}}

	cases := map[string]struct {
		cfg  PlanConfig
		ref  plumbing.ReferenceName
		want bool
	}{
		// A mapping target is pushed by BuildDesiredRefs' mapping pass, so it
		// is in scope even though prune declines every branch under mappings.
		"mapping target":             {PlanConfig{AllRefs: true, Mappings: mapping}, "refs/heads/main", true},
		"mapping target, short form": {PlanConfig{AllRefs: true, Mappings: []RefMapping{{Source: "main", Target: "trunk"}}}, "refs/heads/trunk", true},
		// Exclusions apply to auto-discovery, not to explicit mappings —
		// matching the mapping pass in BuildDesiredRefs.
		"mapping target, excluded": {PlanConfig{AllRefs: true, Mappings: mapping, ExcludeRefPrefixes: []string{"refs/heads/"}}, "refs/heads/main", true},
		// An unmapped branch is neither pushed (that pass is in the else) nor
		// pruned once mappings are set.
		"unmapped branch with mappings": {PlanConfig{AllRefs: true, Mappings: mapping}, "refs/heads/other", false},
		"branch without mappings":       {PlanConfig{AllRefs: true}, "refs/heads/other", true},
		// The tag and other-kind pass sits OUTSIDE the mapping/branch branch,
		// so both are still mirrored under mappings even though prune skips them.
		"tag under AllRefs":               {PlanConfig{AllRefs: true, Mappings: mapping}, "refs/tags/v1", true},
		"tag under IncludeTags":           {PlanConfig{IncludeTags: true}, "refs/tags/v1", true},
		"tag without either":              {PlanConfig{}, "refs/tags/v1", false},
		"other kind under AllRefs":        {PlanConfig{AllRefs: true, Mappings: mapping}, "refs/notes/commits", true},
		"other kind without AllRefs":      {PlanConfig{IncludeTags: true}, "refs/notes/commits", false},
		"excluded by prefix":              {PlanConfig{AllRefs: true, ExcludeRefPrefixes: []string{"refs/pull/"}}, "refs/pull/1/head", false},
		"excluded by exact name":          {PlanConfig{AllRefs: true, ExcludeRefs: []string{"refs/heads/entire"}}, "refs/heads/entire", false},
		"exact exclusion spares children": {PlanConfig{AllRefs: true, ExcludeRefs: []string{"refs/heads/entire"}}, "refs/heads/entire/foo", true},
		// Prune being off must not narrow the answer: the question is whose ref
		// this is, not what this run would do to it.
		"prune off, still in scope": {PlanConfig{AllRefs: true, Prune: false}, "refs/heads/other", true},
		// AllRefs clears a Branches filter during normalization; a scope built
		// from an un-normalized config must not read the stale filter.
		"unnormalized branches filter": {PlanConfig{AllRefs: true, Branches: []string{"main"}}, "refs/heads/other", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			scope, err := NewTargetScope(tc.cfg)
			if err != nil {
				t.Fatalf("NewTargetScope: %v", err)
			}
			if got := scope.Manages(tc.ref); got != tc.want {
				t.Errorf("Manages(%s) = %t, want %t", tc.ref, got, tc.want)
			}
		})
	}
}

// PruneTarget answers the narrower question — whether prune could select an
// ALREADY-UNMANAGED ref — and reading it as whole-scope is what broke twice.
// The difference is pinned so the distinction cannot quietly erode.
func TestPruneTargetIsNarrowerThanScope(t *testing.T) {
	t.Parallel()

	cfg := PlanConfig{AllRefs: true, Mappings: []RefMapping{{Source: "refs/heads/main", Target: "refs/heads/main"}}}
	scope, err := NewTargetScope(cfg)
	if err != nil {
		t.Fatalf("NewTargetScope: %v", err)
	}

	for _, ref := range []plumbing.ReferenceName{"refs/heads/main", "refs/notes/commits"} {
		if _, prunable := PruneTarget(ref, cfg); prunable {
			t.Errorf("PruneTarget(%s) = true; prune declines these under mappings", ref)
		}
		if !scope.Manages(ref) {
			t.Errorf("Manages(%s) = false; the request still pushes it", ref)
		}
	}
}

// NewTargetScope resolves mapping names the same way BuildDesiredRefs does, so
// an invalid mapping must fail here rather than silently yielding a scope that
// disagrees with the planner.
func TestNewTargetScopeRejectsInvalidMappings(t *testing.T) {
	t.Parallel()

	_, err := NewTargetScope(PlanConfig{
		Mappings: []RefMapping{
			{Source: "main", Target: "stable"},
			{Source: "release", Target: "stable"},
		},
	})
	if err == nil {
		t.Fatal("expected duplicate target refs to be rejected")
	}
}
