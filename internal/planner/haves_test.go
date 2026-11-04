package planner

import (
	"maps"
	"slices"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

const checkpointPrefix = "refs/entire/checkpoints/"

func haveTargetRefs() map[plumbing.ReferenceName]plumbing.Hash {
	return map[plumbing.ReferenceName]plumbing.Hash{
		"refs/heads/main":                    hash("1"),
		"refs/tags/v1":                       hash("2"),
		"refs/entire/checkpoints/aa/one":     hash("3"),
		"refs/entire/checkpoints/bb/two":     hash("4"),
		"refs/entire/checkpoints/cc/three":   hash("5"),
		"refs/entire/checkpointsish/sibling": hash("6"),
	}
}

func haveNames(haves map[plumbing.ReferenceName]plumbing.Hash) []string {
	names := make([]string, 0, len(haves))
	for name := range haves {
		names = append(names, name.String())
	}
	slices.Sort(names)
	return names
}

func TestFetchHavesWithoutPrefixesReturnsTargetRefs(t *testing.T) {
	target := haveTargetRefs()
	got := FetchHaves(target, []BranchPlan{{TargetRef: "refs/heads/main", Action: ActionUpdate}}, nil)
	if !maps.Equal(got, target) {
		t.Fatalf("FetchHaves without prefixes = %v, want the target refs unchanged", got)
	}
}

// A branch update shares nothing with the checkpoint histories, so none of
// their tips is offered. Everything outside the prefix stays, including a
// sibling namespace the prefix's trailing slash keeps out.
func TestFetchHavesDropsUnchangedIndependentRefs(t *testing.T) {
	plans := []BranchPlan{{TargetRef: "refs/heads/main", Action: ActionUpdate}}
	got := FetchHaves(haveTargetRefs(), plans, []string{checkpointPrefix})
	assertNames(t, "haves", haveNames(got), []string{
		"refs/entire/checkpointsish/sibling",
		"refs/heads/main",
		"refs/tags/v1",
	})
}

// An updated checkpoint ref keeps its own old value: its new history builds
// on it. Its unchanged neighbours still go.
func TestFetchHavesKeepsUpdatedIndependentRef(t *testing.T) {
	plans := []BranchPlan{{TargetRef: "refs/entire/checkpoints/bb/two", Action: ActionUpdate}}
	got := FetchHaves(haveTargetRefs(), plans, []string{checkpointPrefix})
	assertNames(t, "haves", haveNames(got), []string{
		"refs/entire/checkpoints/bb/two",
		"refs/entire/checkpointsish/sibling",
		"refs/heads/main",
		"refs/tags/v1",
	})
	if got["refs/entire/checkpoints/bb/two"] != hash("4") {
		t.Fatalf("updated ref offered %s, want its target value %s", got["refs/entire/checkpoints/bb/two"], hash("4"))
	}
}

// Only an update keeps an independent ref. A delete's tip is about to go and
// a skip is unchanged, so neither helps the pack.
func TestFetchHavesIgnoresNonUpdatePlans(t *testing.T) {
	plans := []BranchPlan{
		{TargetRef: "refs/entire/checkpoints/aa/one", Action: ActionDelete},
		{TargetRef: "refs/entire/checkpoints/bb/two", Action: ActionSkip},
		{TargetRef: "refs/entire/checkpoints/new", Action: ActionCreate},
	}
	got := FetchHaves(haveTargetRefs(), plans, []string{checkpointPrefix})
	assertNames(t, "haves", haveNames(got), []string{
		"refs/entire/checkpointsish/sibling",
		"refs/heads/main",
		"refs/tags/v1",
	})
}

func TestFetchHavesLeavesInputUntouched(t *testing.T) {
	target := haveTargetRefs()
	FetchHaves(target, nil, []string{checkpointPrefix})
	if !maps.Equal(target, haveTargetRefs()) {
		t.Fatalf("FetchHaves modified its input: %v", target)
	}
}
