package syncer

import (
	"fmt"
	"slices"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

func TestSelectBranches(t *testing.T) {
	source := map[string]plumbing.Hash{
		"main": plumbing.NewHash("1111111111111111111111111111111111111111"),
		"dev":  plumbing.NewHash("2222222222222222222222222222222222222222"),
	}

	got := selectBranches(source, []string{"dev", "missing"})
	if len(got) != 1 || got["dev"] != source["dev"] {
		t.Fatalf("unexpected branch selection: %#v", got)
	}
}

func TestPlanBranchSkip(t *testing.T) {
	hash := plumbing.NewHash("1111111111111111111111111111111111111111")
	plan, err := planBranch(nil, "main", hash, hash)
	if err != nil {
		t.Fatalf("planBranch returned error: %v", err)
	}
	if plan.Action != ActionSkip {
		t.Fatalf("expected skip, got %s", plan.Action)
	}
}

func TestPlanBranchCreate(t *testing.T) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	sourceHash := seedCommit(t, repo, nil)

	plan := BranchPlan{
		Branch:     "main",
		SourceHash: sourceHash,
		Action:     ActionCreate,
	}

	if plan.Action != ActionCreate {
		t.Fatalf("expected create")
	}
}

func TestPlanBranchFastForwardAndBlock(t *testing.T) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}

	root := seedCommit(t, repo, nil)
	next := seedCommit(t, repo, []plumbing.Hash{root})
	side := seedCommit(t, repo, []plumbing.Hash{root})

	ffPlan, err := planBranch(repo, "main", next, root)
	if err != nil {
		t.Fatalf("planBranch fast-forward: %v", err)
	}
	if ffPlan.Action != ActionUpdate {
		t.Fatalf("expected update, got %s", ffPlan.Action)
	}

	blockPlan, err := planBranch(repo, "main", side, next)
	if err != nil {
		t.Fatalf("planBranch block: %v", err)
	}
	if blockPlan.Action != ActionBlock {
		t.Fatalf("expected block, got %s", blockPlan.Action)
	}
}

func seedCommit(t *testing.T, repo *git.Repository, parents []plumbing.Hash) plumbing.Hash {
	t.Helper()

	now := time.Now().UTC()

	obj := repo.Storer.NewEncodedObject()
	commit := &object.Commit{
		Author: object.Signature{
			Name:  "test",
			Email: "test@example.com",
			When:  now,
		},
		Committer: object.Signature{
			Name:  "test",
			Email: "test@example.com",
			When:  now,
		},
		Message:      fmt.Sprintf("test-%d-%d", len(parents), now.UnixNano()),
		TreeHash:     plumbing.ZeroHash,
		ParentHashes: parents,
	}

	if err := commit.Encode(obj); err != nil {
		t.Fatalf("encode commit: %v", err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("store commit: %v", err)
	}
	return hash
}

func TestCoarseCheckpointCandidates(t *testing.T) {
	candidates := coarseCheckpointCandidates(10, 100, 20)
	if len(candidates) == 0 {
		t.Fatalf("expected coarse candidates")
	}
	if candidates[0] != 100 {
		t.Fatalf("expected highest candidate first, got %v", candidates)
	}
	if !slices.Contains(candidates, 29) {
		t.Fatalf("expected projected candidate near previous span, got %v", candidates)
	}
}

func TestLargestCheckpointUnderLimitByProbe(t *testing.T) {
	chain := make([]plumbing.Hash, 40)
	for i := range chain {
		chain[i] = plumbing.NewHash(fmt.Sprintf("%040x", i+1))
	}

	var probes []int
	best, err := largestCheckpointUnderLimitByProbe(chain, 4, 8, func(idx int) (bool, error) {
		probes = append(probes, idx)
		return idx > 19, nil
	})
	if err != nil {
		t.Fatalf("largestCheckpointUnderLimitByProbe: %v", err)
	}
	if best != 19 {
		t.Fatalf("expected best checkpoint index 19, got %d", best)
	}
	if len(probes) > 10 {
		t.Fatalf("expected narrowed probe count, got %d probes: %v", len(probes), probes)
	}
}
