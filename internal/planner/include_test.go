package planner

import (
	"sort"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

func hash(c string) plumbing.Hash {
	return plumbing.NewHash(strings.Repeat(c, 40))
}

// scopedSourceRefs spans every namespace the include filter has to separate:
// two refs inside the included prefix (one of them reserved by an exclusion),
// and a branch, a tag and an other-kind ref outside it.
func scopedSourceRefs() map[plumbing.ReferenceName]plumbing.Hash {
	return map[plumbing.ReferenceName]plumbing.Hash{
		"refs/heads/entire/native/one":    hash("1"),
		"refs/heads/entire/native/anchor": hash("2"),
		"refs/heads/main":                 hash("3"),
		"refs/tags/v1":                    hash("4"),
		"refs/notes/commits":              hash("5"),
	}
}

const nativePrefix = "refs/heads/entire/native/"

func refNames(refs map[plumbing.ReferenceName]DesiredRef) []string {
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name.String())
	}
	sort.Strings(names)
	return names
}

func assertNames(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", what, got, want)
		}
	}
}

// The fetch set: under include prefixes, discovery mirrors the named namespace
// and nothing else, in every kind — a tag or a note outside the prefix is as
// out of scope as a branch is.
func TestBuildDesiredRefsIncludeRefPrefixes(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		cfg  PlanConfig
		want []string
	}{
		"include narrows every kind": {
			PlanConfig{AllRefs: true, IncludeRefPrefixes: []string{nativePrefix}},
			[]string{"refs/heads/entire/native/anchor", "refs/heads/entire/native/one"},
		},
		// Exclusions compose with the include rather than being replaced by
		// it: the caller reserves one name inside the namespace it mirrors.
		"exclusion inside an included prefix": {
			PlanConfig{AllRefs: true, IncludeRefPrefixes: []string{nativePrefix}, ExcludeRefs: []string{nativePrefix + "anchor"}},
			[]string{"refs/heads/entire/native/one"},
		},
		// Explicit mappings are the caller naming a ref outright, so they
		// bypass the include filter exactly as they bypass exclusions.
		"mapping outside the include prefix is exempt": {
			PlanConfig{
				AllRefs:            true,
				IncludeRefPrefixes: []string{nativePrefix},
				Mappings:           []RefMapping{{Source: "refs/heads/main", Target: "refs/heads/main"}},
			},
			// The mapping pass replaces branch discovery, but the tag and
			// other-kind pass still runs and is still filtered.
			[]string{"refs/heads/main"},
		},
		"no include prefixes narrows nothing": {
			PlanConfig{AllRefs: true},
			[]string{
				"refs/heads/entire/native/anchor", "refs/heads/entire/native/one",
				"refs/heads/main", "refs/notes/commits", "refs/tags/v1",
			},
		},
		// Fail closed: a prefix list that names nothing usable puts no ref in
		// scope, so the run pushes and prunes nothing rather than everything.
		"blank prefix puts nothing in scope": {
			PlanConfig{AllRefs: true, IncludeRefPrefixes: []string{" "}},
			nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			desired, managed, err := BuildDesiredRefs(scopedSourceRefs(), tc.cfg)
			if err != nil {
				t.Fatalf("BuildDesiredRefs: %v", err)
			}
			assertNames(t, "desired", refNames(desired), tc.want)
			if len(managed) != len(desired) {
				t.Fatalf("managed = %d entries, want %d", len(managed), len(desired))
			}
		})
	}
}

// The prune set: a target ref outside the include prefixes is not this
// request's to delete, however stale it looks. The two halves are planned from
// the same predicate, so a ref the run would not push is one it cannot delete.
func TestBuildReplicationPlansIncludeRefPrefixesPruneSet(t *testing.T) {
	t.Parallel()
	cfg := PlanConfig{
		AllRefs:            true,
		Prune:              true,
		IncludeRefPrefixes: []string{nativePrefix},
		ExcludeRefs:        []string{nativePrefix + "anchor"},
	}
	desired, managed, err := BuildDesiredRefs(scopedSourceRefs(), cfg)
	if err != nil {
		t.Fatalf("BuildDesiredRefs: %v", err)
	}
	targetRefs := map[plumbing.ReferenceName]plumbing.Hash{
		// In scope and still on the source: updated, not pruned.
		nativePrefix + "one": hash("6"),
		// In scope and gone from the source: the one deletion.
		nativePrefix + "gone": hash("7"),
		// In scope but reserved by the exclusion: left alone.
		nativePrefix + "anchor": hash("8"),
		// Outside the include prefixes, in all three kinds.
		"refs/heads/main":    hash("9"),
		"refs/tags/v1":       hash("a"),
		"refs/notes/commits": hash("b"),
	}
	plans, err := BuildReplicationPlans(desired, targetRefs, managed, cfg)
	if err != nil {
		t.Fatalf("BuildReplicationPlans: %v", err)
	}
	var deleted, updated []string
	for _, p := range plans {
		switch p.Action {
		case ActionDelete:
			deleted = append(deleted, p.TargetRef.String())
		case ActionUpdate:
			updated = append(updated, p.TargetRef.String())
		case ActionCreate, ActionSkip, ActionBlock, ActionWarn:
		}
	}
	sort.Strings(deleted)
	sort.Strings(updated)
	assertNames(t, "deleted", deleted, []string{nativePrefix + "gone"})
	assertNames(t, "updated", updated, []string{nativePrefix + "one"})
}

// TargetScope answers "is this ref mine" for the divergence check as well as
// for planning, so the include filter has to reach it too — otherwise a
// scoped request reads every out-of-scope target ref as divergence.
func TestTargetScopeIncludeRefPrefixes(t *testing.T) {
	t.Parallel()
	mapping := []RefMapping{{Source: "refs/heads/main", Target: "refs/heads/main"}}
	cases := map[string]struct {
		cfg  PlanConfig
		ref  plumbing.ReferenceName
		want bool
	}{
		"branch inside the prefix":  {PlanConfig{AllRefs: true, IncludeRefPrefixes: []string{nativePrefix}}, nativePrefix + "one", true},
		"branch outside the prefix": {PlanConfig{AllRefs: true, IncludeRefPrefixes: []string{nativePrefix}}, "refs/heads/main", false},
		"tag outside the prefix":    {PlanConfig{AllRefs: true, IncludeRefPrefixes: []string{nativePrefix}}, "refs/tags/v1", false},
		"note outside the prefix":   {PlanConfig{AllRefs: true, IncludeRefPrefixes: []string{nativePrefix}}, "refs/notes/commits", false},
		"exclusion inside the prefix": {
			PlanConfig{AllRefs: true, IncludeRefPrefixes: []string{nativePrefix}, ExcludeRefs: []string{nativePrefix + "anchor"}},
			nativePrefix + "anchor", false,
		},
		"mapping target outside the prefix": {
			PlanConfig{AllRefs: true, IncludeRefPrefixes: []string{nativePrefix}, Mappings: mapping},
			"refs/heads/main", true,
		},
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
			// PruneTarget is the narrower half: a mapping target is managed
			// but never prunable, so it is the one case the two disagree on.
			if _, prunable := PruneTarget(tc.ref, tc.cfg); prunable && !tc.want {
				t.Errorf("PruneTarget(%s) prunable while Manages is false", tc.ref)
			}
		})
	}
}

// git-sync's own refs/gitsync/ scaffolding stays in TARGET prune scope however
// the request is narrowed. Prune is the only cleaner a stale bootstrap marker
// has, so an include prefix that swept the namespace out of scope would strand
// markers on a prefix-scoped mirror forever, pinning the objects they hold and
// starving the ENT-2054 resume route. Source-side discovery skips the
// namespace on its own, so nothing starts mirroring them either way.
func TestIncludeRefPrefixesKeepScaffoldingPrunable(t *testing.T) {
	t.Parallel()
	cfg := PlanConfig{AllRefs: true, Prune: true, IncludeRefPrefixes: []string{nativePrefix}}
	desired, managed, err := BuildDesiredRefs(scopedSourceRefs(), cfg)
	if err != nil {
		t.Fatalf("BuildDesiredRefs: %v", err)
	}
	// Stale: its branch already exists on the target, so the bootstrap that
	// wrote it finished. Live: its branch is still absent, so the marker is
	// the only record of how far that bootstrap got.
	stale := BootstrapTempRef(plumbing.ReferenceName(nativePrefix + "one"))
	live := BootstrapTempRef(plumbing.ReferenceName(nativePrefix + "anchor"))
	targetRefs := map[plumbing.ReferenceName]plumbing.Hash{
		nativePrefix + "one": hash("6"),
		stale:                hash("7"),
		live:                 hash("8"),
		"refs/heads/main":    hash("9"),
	}
	plans, err := BuildReplicationPlans(desired, targetRefs, managed, cfg)
	if err != nil {
		t.Fatalf("BuildReplicationPlans: %v", err)
	}
	var deleted []string
	for _, p := range plans {
		if p.Action == ActionDelete {
			deleted = append(deleted, p.TargetRef.String())
		}
	}
	sort.Strings(deleted)
	assertNames(t, "deleted", deleted, []string{stale.String()})
}

// Two syncs with different scopes can share a target — a github-kind mirror
// and a namespace-kind one on the same placement. A marker belongs to the
// branch it checkpoints, and isLiveBootstrapMarker can only speak for THIS
// run's desired set, so a scoped run that judged every marker on its own
// reckoning would delete the other writer's live resume state and force a full
// re-import of the repository it was halfway through.
func TestIncludeRefPrefixesSpareOtherWritersMarkers(t *testing.T) {
	t.Parallel()
	cfg := PlanConfig{AllRefs: true, Prune: true, IncludeRefPrefixes: []string{nativePrefix}}
	desired, managed, err := BuildDesiredRefs(scopedSourceRefs(), cfg)
	if err != nil {
		t.Fatalf("BuildDesiredRefs: %v", err)
	}
	// refs/heads/main is outside this run's scope, so its marker is another
	// writer's business: absent branch, live marker, and none of it visible in
	// this run's desired set.
	foreign := BootstrapTempRef(plumbing.NewBranchReferenceName("main"))
	mine := BootstrapTempRef(plumbing.ReferenceName(nativePrefix + "one"))
	targetRefs := map[plumbing.ReferenceName]plumbing.Hash{
		nativePrefix + "one": hash("1"),
		mine:                 hash("2"),
		foreign:              hash("3"),
	}
	plans, err := BuildReplicationPlans(desired, targetRefs, managed, cfg)
	if err != nil {
		t.Fatalf("BuildReplicationPlans: %v", err)
	}
	var deleted []string
	for _, p := range plans {
		if p.Action == ActionDelete {
			deleted = append(deleted, p.TargetRef.String())
		}
	}
	sort.Strings(deleted)
	// mine is stale (its branch exists on the target) and still prunable;
	// foreign is not this run's to judge at all.
	assertNames(t, "deleted", deleted, []string{mine.String()})
}
