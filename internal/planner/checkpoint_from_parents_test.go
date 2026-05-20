package planner

import (
	"reflect"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage/memory"
)

// buildParentsMapFromRepo extracts a (commit -> parents) map from an
// in-memory repo for cross-checking the parents-map walkers against
// the storer-based walkers on the same graph.
func buildParentsMapFromRepo(t *testing.T, repo *git.Repository, tips ...plumbing.Hash) map[plumbing.Hash][]plumbing.Hash {
	t.Helper()
	parents := map[plumbing.Hash][]plumbing.Hash{}
	visit := func(h plumbing.Hash) error {
		if _, ok := parents[h]; ok {
			return nil
		}
		commit, err := repo.CommitObject(h)
		if err != nil {
			return err
		}
		parents[h] = append([]plumbing.Hash(nil), commit.ParentHashes...)
		return nil
	}
	queue := append([]plumbing.Hash(nil), tips...)
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		if _, ok := parents[h]; ok {
			continue
		}
		if err := visit(h); err != nil {
			t.Fatalf("visit %s: %v", h, err)
		}
		queue = append(queue, parents[h]...)
	}
	return parents
}

func TestFirstParentChainFromParents_MatchesStorerWalker(t *testing.T) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	root := seedCommit(t, repo, nil)
	mid := seedCommit(t, repo, []plumbing.Hash{root})
	div := seedCommit(t, repo, []plumbing.Hash{mid})
	tip := seedCommit(t, repo, []plumbing.Hash{div})

	parents := buildParentsMapFromRepo(t, repo, tip)

	stopAt := map[plumbing.Hash]struct{}{root: {}, mid: {}}
	wantChain, err := FirstParentChainStoppingAt(repo.Storer, tip, stopAt)
	if err != nil {
		t.Fatalf("storer walker: %v", err)
	}
	gotChain, err := FirstParentChainFromParents(parents, tip, stopAt)
	if err != nil {
		t.Fatalf("parents walker: %v", err)
	}
	if !reflect.DeepEqual(gotChain, wantChain) {
		t.Fatalf("chains differ:\n got=%v\nwant=%v", gotChain, wantChain)
	}
}

func TestFirstParentChainFromParents_TipInStopSet(t *testing.T) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	root := seedCommit(t, repo, nil)
	tip := seedCommit(t, repo, []plumbing.Hash{root})

	parents := buildParentsMapFromRepo(t, repo, tip)
	got, err := FirstParentChainFromParents(parents, tip, map[plumbing.Hash]struct{}{tip: {}})
	if err != nil {
		t.Fatalf("parents walker: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty chain when tip is in stop set, got %v", got)
	}
}

func TestFirstParentChainFromParents_MissingTipErrors(t *testing.T) {
	parents := map[plumbing.Hash][]plumbing.Hash{
		plumbing.NewHash("1111111111111111111111111111111111111111"): nil,
	}
	missing := plumbing.NewHash("2222222222222222222222222222222222222222")
	_, err := FirstParentChainFromParents(parents, missing, nil)
	if err == nil {
		t.Fatal("expected error for missing tip, got nil")
	}
	if !strings.Contains(err.Error(), "not found in parents map") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTopoChainFromParents_MatchesStorerWalker(t *testing.T) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	// Build a graph with a merge:
	//   root → a → m ← b
	//          \↗   ↘
	root := seedCommit(t, repo, nil)
	a := seedCommit(t, repo, []plumbing.Hash{root})
	b := seedCommit(t, repo, []plumbing.Hash{a})
	c := seedCommit(t, repo, []plumbing.Hash{a})
	merge := seedCommit(t, repo, []plumbing.Hash{b, c})

	parents := buildParentsMapFromRepo(t, repo, merge)

	stopAt := map[plumbing.Hash]struct{}{root: {}}
	wantChain, err := TopoChainStoppingAt(repo.Storer, merge, stopAt)
	if err != nil {
		t.Fatalf("storer walker: %v", err)
	}
	gotChain, err := TopoChainFromParents(parents, merge, stopAt)
	if err != nil {
		t.Fatalf("parents walker: %v", err)
	}
	if !reflect.DeepEqual(gotChain, wantChain) {
		t.Fatalf("chains differ:\n got=%v\nwant=%v", gotChain, wantChain)
	}
}
