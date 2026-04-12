package planner

import (
	"fmt"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage/memory"
)

func BenchmarkBuildDesiredRefs(b *testing.B) {
	sourceRefs := make(map[plumbing.ReferenceName]plumbing.Hash, 100)
	for i := 0; i < 100; i++ {
		name := plumbing.NewBranchReferenceName(fmt.Sprintf("branch-%03d", i))
		sourceRefs[name] = plumbing.NewHash(fmt.Sprintf("%040x", i+1))
	}
	cfg := PlanConfig{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, err := BuildDesiredRefs(sourceRefs, cfg)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBuildPlans(b *testing.B) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		b.Fatalf("init repo: %v", err)
	}

	// Build a two-commit chain so we can test fast-forward detection.
	root := seedCommit(b, repo, nil)
	tip := seedCommit(b, repo, []plumbing.Hash{root})

	desired := make(map[plumbing.ReferenceName]DesiredRef, 100)
	targetRefs := make(map[plumbing.ReferenceName]plumbing.Hash, 100)
	managed := make(map[plumbing.ReferenceName]ManagedTarget, 100)

	for i := 0; i < 100; i++ {
		ref := plumbing.NewBranchReferenceName(fmt.Sprintf("branch-%03d", i))
		short := ref.Short()

		switch {
		case i < 50:
			// skip: source == target
			desired[ref] = DesiredRef{
				Kind: RefKindBranch, Label: short,
				SourceRef: ref, TargetRef: ref,
				SourceHash: root,
			}
			targetRefs[ref] = root
		case i < 75:
			// create: exists in desired, absent from target
			desired[ref] = DesiredRef{
				Kind: RefKindBranch, Label: short,
				SourceRef: ref, TargetRef: ref,
				SourceHash: tip,
			}
			// no targetRefs entry -> ActionCreate
		default:
			// update (fast-forward): target at root, source at tip
			desired[ref] = DesiredRef{
				Kind: RefKindBranch, Label: short,
				SourceRef: ref, TargetRef: ref,
				SourceHash: tip,
			}
			targetRefs[ref] = root
		}
		managed[ref] = ManagedTarget{Kind: RefKindBranch, Label: short}
	}

	cfg := PlanConfig{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Copy managed map each iteration because BuildPlans can mutate it
		// when Prune is set (not the case here, but copy for safety).
		mgdCopy := make(map[plumbing.ReferenceName]ManagedTarget, len(managed))
		for k, v := range managed {
			mgdCopy[k] = v
		}
		_, err := BuildPlans(repo.Storer, desired, targetRefs, mgdCopy, cfg)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSampledCheckpointCandidates(b *testing.B) {
	// Simulate a chain of 10000 commits; prevSpan of 500.
	lo := 100
	hi := 9999
	prevSpan := 500

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		candidates := SampledCheckpointCandidates(lo, hi, prevSpan)
		if len(candidates) == 0 {
			b.Fatal("expected candidates")
		}
	}
}

func BenchmarkReachesCommit(b *testing.B) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		b.Fatalf("init repo: %v", err)
	}

	// Build a 100-commit linear chain.
	hashes := make([]plumbing.Hash, 100)
	hashes[0] = seedCommit(b, repo, nil)
	for i := 1; i < 100; i++ {
		hashes[i] = seedCommit(b, repo, []plumbing.Hash{hashes[i-1]})
	}

	tip := hashes[99]
	root := hashes[0]

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ok, err := ReachesCommit(repo.Storer, tip, root)
		if err != nil {
			b.Fatal(err)
		}
		if !ok {
			b.Fatal("expected tip to reach root")
		}
	}
}

// seedCommit is defined in planner_test.go with a testing.TB parameter,
// so it is usable from both tests and benchmarks.
