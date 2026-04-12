package planner

import (
	"fmt"
	"slices"
	"testing"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v6/storage/memory"
)

func TestSelectBranches(t *testing.T) {
	source := map[string]plumbing.Hash{
		"main": plumbing.NewHash("1111111111111111111111111111111111111111"),
		"dev":  plumbing.NewHash("2222222222222222222222222222222222222222"),
	}
	got := SelectBranches(source, []string{"dev", "missing"})
	if len(got) != 1 || got["dev"] != source["dev"] {
		t.Fatalf("unexpected branch selection: %#v", got)
	}
}

func TestPlanRefSkip(t *testing.T) {
	hash := plumbing.NewHash("1111111111111111111111111111111111111111")
	plan, err := PlanRef(nil, DesiredRef{
		Kind: RefKindBranch, Label: "main",
		SourceRef: plumbing.NewBranchReferenceName("main"),
		TargetRef: plumbing.NewBranchReferenceName("main"),
		SourceHash: hash,
	}, hash, false)
	if err != nil {
		t.Fatalf("PlanRef error: %v", err)
	}
	if plan.Action != ActionSkip {
		t.Fatalf("expected skip, got %s", plan.Action)
	}
}

func TestPlanRefFastForwardAndBlock(t *testing.T) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	root := seedCommit(t, repo, nil)
	next := seedCommit(t, repo, []plumbing.Hash{root})
	side := seedCommit(t, repo, []plumbing.Hash{root})

	ffPlan, err := PlanRef(repo.Storer, DesiredRef{
		Kind: RefKindBranch, Label: "main",
		SourceRef: plumbing.NewBranchReferenceName("main"),
		TargetRef: plumbing.NewBranchReferenceName("main"),
		SourceHash: next,
	}, root, false)
	if err != nil {
		t.Fatalf("PlanRef fast-forward: %v", err)
	}
	if ffPlan.Action != ActionUpdate {
		t.Fatalf("expected update, got %s", ffPlan.Action)
	}

	blockPlan, err := PlanRef(repo.Storer, DesiredRef{
		Kind: RefKindBranch, Label: "main",
		SourceRef: plumbing.NewBranchReferenceName("main"),
		TargetRef: plumbing.NewBranchReferenceName("main"),
		SourceHash: side,
	}, next, false)
	if err != nil {
		t.Fatalf("PlanRef block: %v", err)
	}
	if blockPlan.Action != ActionBlock {
		t.Fatalf("expected block, got %s", blockPlan.Action)
	}
}

func TestValidateMappingsRejectsDuplicateTargets(t *testing.T) {
	_, err := ValidateMappings([]RefMapping{
		{Source: "main", Target: "stable"},
		{Source: "release", Target: "stable"},
	})
	if err == nil {
		t.Fatalf("expected error for duplicate target")
	}
}

func TestValidateMappingsRejectsCrossKind(t *testing.T) {
	_, err := ValidateMappings([]RefMapping{
		{Source: "refs/heads/main", Target: "refs/tags/main"},
	})
	if err == nil {
		t.Fatalf("expected error for cross-kind mapping")
	}
}

func TestValidateMappingsRejectsMixedQualification(t *testing.T) {
	_, err := ValidateMappings([]RefMapping{
		{Source: "refs/heads/main", Target: "stable"},
	})
	if err == nil {
		t.Fatalf("expected error for mixed qualification")
	}
}

func TestSampledCheckpointCandidates(t *testing.T) {
	candidates := SampledCheckpointCandidates(10, 100, 20)
	if len(candidates) == 0 {
		t.Fatalf("expected sampled candidates")
	}
	if candidates[0] != 29 {
		t.Fatalf("expected highest candidate first, got %v", candidates)
	}
	if !slices.Contains(candidates, 29) {
		t.Fatalf("expected projected candidate near previous span, got %v", candidates)
	}
	if !slices.Contains(candidates, 10) {
		t.Fatalf("expected lower bound candidate, got %v", candidates)
	}
}

func TestSampledCheckpointUnderLimit(t *testing.T) {
	chain := make([]plumbing.Hash, 40)
	for i := range chain {
		chain[i] = plumbing.NewHash(fmt.Sprintf("%040x", i+1))
	}
	var probes []int
	best, err := SampledCheckpointUnderLimit(chain, 4, 8, func(idx int) (bool, error) {
		probes = append(probes, idx)
		return idx > 19, nil
	})
	if err != nil {
		t.Fatalf("SampledCheckpointUnderLimit: %v", err)
	}
	if best < 12 || best > 19 {
		t.Fatalf("expected a reasonable sampled checkpoint, got %d", best)
	}
	if len(probes) > 6 {
		t.Fatalf("expected fixed small probe count, got %d probes: %v", len(probes), probes)
	}
}

func TestBuildDesiredRefsWithMappings(t *testing.T) {
	hash1 := plumbing.NewHash("1111111111111111111111111111111111111111")
	hash2 := plumbing.NewHash("2222222222222222222222222222222222222222")

	sourceRefs := map[plumbing.ReferenceName]plumbing.Hash{
		plumbing.NewBranchReferenceName("main"):    hash1,
		plumbing.NewBranchReferenceName("develop"): hash2,
	}

	tests := []struct {
		name        string
		mappings    []RefMapping
		wantTargets []plumbing.ReferenceName
		wantErr     bool
	}{
		{
			name: "simple rename mapping",
			mappings: []RefMapping{
				{Source: "main", Target: "stable"},
			},
			wantTargets: []plumbing.ReferenceName{
				plumbing.NewBranchReferenceName("stable"),
			},
		},
		{
			name: "multiple mappings",
			mappings: []RefMapping{
				{Source: "main", Target: "prod"},
				{Source: "develop", Target: "staging"},
			},
			wantTargets: []plumbing.ReferenceName{
				plumbing.NewBranchReferenceName("prod"),
				plumbing.NewBranchReferenceName("staging"),
			},
		},
		{
			name: "missing source ref errors",
			mappings: []RefMapping{
				{Source: "nonexistent", Target: "target"},
			},
			wantErr: true,
		},
		{
			name: "duplicate target errors",
			mappings: []RefMapping{
				{Source: "main", Target: "same"},
				{Source: "develop", Target: "same"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			desired, managed, err := BuildDesiredRefs(sourceRefs, PlanConfig{
				Mappings: tt.mappings,
			})
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(desired) != len(tt.wantTargets) {
				t.Fatalf("expected %d desired refs, got %d", len(tt.wantTargets), len(desired))
			}
			for _, target := range tt.wantTargets {
				if _, ok := desired[target]; !ok {
					t.Errorf("expected target ref %s in desired map", target)
				}
				if _, ok := managed[target]; !ok {
					t.Errorf("expected target ref %s in managed map", target)
				}
			}
		})
	}
}

func TestBuildDesiredRefsAllBranches(t *testing.T) {
	hash1 := plumbing.NewHash("1111111111111111111111111111111111111111")
	hash2 := plumbing.NewHash("2222222222222222222222222222222222222222")
	tagHash := plumbing.NewHash("3333333333333333333333333333333333333333")

	sourceRefs := map[plumbing.ReferenceName]plumbing.Hash{
		plumbing.NewBranchReferenceName("main"):    hash1,
		plumbing.NewBranchReferenceName("develop"): hash2,
		plumbing.NewTagReferenceName("v1.0"):       tagHash,
	}

	tests := []struct {
		name            string
		branches        []string
		includeTags     bool
		wantBranchCount int
		wantTagCount    int
	}{
		{
			name:            "no filter returns all branches",
			wantBranchCount: 2,
			wantTagCount:    0,
		},
		{
			name:            "filter to single branch",
			branches:        []string{"main"},
			wantBranchCount: 1,
			wantTagCount:    0,
		},
		{
			name:            "include tags adds tag refs",
			includeTags:     true,
			wantBranchCount: 2,
			wantTagCount:    1,
		},
		{
			name:            "branch filter plus tags",
			branches:        []string{"main"},
			includeTags:     true,
			wantBranchCount: 1,
			wantTagCount:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			desired, _, err := BuildDesiredRefs(sourceRefs, PlanConfig{
				Branches:    tt.branches,
				IncludeTags: tt.includeTags,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			branchCount, tagCount := 0, 0
			for _, d := range desired {
				switch d.Kind {
				case RefKindBranch:
					branchCount++
				case RefKindTag:
					tagCount++
				}
			}
			if branchCount != tt.wantBranchCount {
				t.Errorf("expected %d branches, got %d", tt.wantBranchCount, branchCount)
			}
			if tagCount != tt.wantTagCount {
				t.Errorf("expected %d tags, got %d", tt.wantTagCount, tagCount)
			}
		})
	}
}

func TestBuildPlansDelete(t *testing.T) {
	hash1 := plumbing.NewHash("1111111111111111111111111111111111111111")
	hash2 := plumbing.NewHash("2222222222222222222222222222222222222222")

	mainRef := plumbing.NewBranchReferenceName("main")
	oldRef := plumbing.NewBranchReferenceName("old-branch")

	desired := map[plumbing.ReferenceName]DesiredRef{
		mainRef: {
			Kind:       RefKindBranch,
			Label:      "main",
			SourceRef:  mainRef,
			TargetRef:  mainRef,
			SourceHash: hash1,
		},
	}
	managed := map[plumbing.ReferenceName]ManagedTarget{
		mainRef: {Kind: RefKindBranch, Label: "main"},
	}
	targetRefs := map[plumbing.ReferenceName]plumbing.Hash{
		mainRef: hash1,
		oldRef:  hash2,
	}

	plans, err := BuildPlans(nil, desired, targetRefs, managed, PlanConfig{
		Prune: true,
	})
	if err != nil {
		t.Fatalf("BuildPlans error: %v", err)
	}

	var deletePlan *BranchPlan
	for i, p := range plans {
		if p.Action == ActionDelete {
			deletePlan = &plans[i]
			break
		}
	}
	if deletePlan == nil {
		t.Fatal("expected a delete plan for old-branch")
	}
	if deletePlan.TargetRef != oldRef {
		t.Fatalf("expected delete for %s, got %s", oldRef, deletePlan.TargetRef)
	}
	if deletePlan.Kind != RefKindBranch {
		t.Fatalf("expected branch kind, got %s", deletePlan.Kind)
	}
}

func TestBuildPlansTagBlock(t *testing.T) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	hash1 := seedCommit(t, repo, nil)
	hash2 := seedCommit(t, repo, nil)

	tagRef := plumbing.NewTagReferenceName("v1.0")

	desired := map[plumbing.ReferenceName]DesiredRef{
		tagRef: {
			Kind:       RefKindTag,
			Label:      "v1.0",
			SourceRef:  tagRef,
			TargetRef:  tagRef,
			SourceHash: hash2,
		},
	}
	managed := map[plumbing.ReferenceName]ManagedTarget{
		tagRef: {Kind: RefKindTag, Label: "v1.0"},
	}
	targetRefs := map[plumbing.ReferenceName]plumbing.Hash{
		tagRef: hash1,
	}

	plans, err := BuildPlans(repo.Storer, desired, targetRefs, managed, PlanConfig{
		Force: false,
	})
	if err != nil {
		t.Fatalf("BuildPlans error: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected 1 plan, got %d", len(plans))
	}
	if plans[0].Action != ActionBlock {
		t.Fatalf("expected block action for tag without force, got %s", plans[0].Action)
	}
}

func TestBuildPlansTagForce(t *testing.T) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	hash1 := seedCommit(t, repo, nil)
	hash2 := seedCommit(t, repo, nil)

	tagRef := plumbing.NewTagReferenceName("v1.0")

	desired := map[plumbing.ReferenceName]DesiredRef{
		tagRef: {
			Kind:       RefKindTag,
			Label:      "v1.0",
			SourceRef:  tagRef,
			TargetRef:  tagRef,
			SourceHash: hash2,
		},
	}
	managed := map[plumbing.ReferenceName]ManagedTarget{
		tagRef: {Kind: RefKindTag, Label: "v1.0"},
	}
	targetRefs := map[plumbing.ReferenceName]plumbing.Hash{
		tagRef: hash1,
	}

	plans, err := BuildPlans(repo.Storer, desired, targetRefs, managed, PlanConfig{
		Force: true,
	})
	if err != nil {
		t.Fatalf("BuildPlans error: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected 1 plan, got %d", len(plans))
	}
	if plans[0].Action != ActionUpdate {
		t.Fatalf("expected update action for tag with force, got %s", plans[0].Action)
	}
}

func TestBootstrapResumeIndex(t *testing.T) {
	checkpoints := []plumbing.Hash{
		plumbing.NewHash("1111111111111111111111111111111111111111"),
		plumbing.NewHash("2222222222222222222222222222222222222222"),
		plumbing.NewHash("3333333333333333333333333333333333333333"),
	}

	tests := []struct {
		name       string
		resumeHash plumbing.Hash
		wantIdx    int
		wantErr    bool
	}{
		{
			name:       "zero hash starts at beginning",
			resumeHash: plumbing.ZeroHash,
			wantIdx:    0,
		},
		{
			name:       "match first checkpoint resumes at index 1",
			resumeHash: plumbing.NewHash("1111111111111111111111111111111111111111"),
			wantIdx:    1,
		},
		{
			name:       "match second checkpoint resumes at index 2",
			resumeHash: plumbing.NewHash("2222222222222222222222222222222222222222"),
			wantIdx:    2,
		},
		{
			name:       "match last checkpoint resumes past end",
			resumeHash: plumbing.NewHash("3333333333333333333333333333333333333333"),
			wantIdx:    3,
		},
		{
			name:       "mismatch hash returns error",
			resumeHash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx, err := BootstrapResumeIndex(checkpoints, tt.resumeHash)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if idx != tt.wantIdx {
				t.Fatalf("expected resume index %d, got %d", tt.wantIdx, idx)
			}
		})
	}
}

func TestFirstParentChain(t *testing.T) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}

	// Build a linear chain: root -> mid -> tip
	root := seedCommit(t, repo, nil)
	mid := seedCommit(t, repo, []plumbing.Hash{root})
	tip := seedCommit(t, repo, []plumbing.Hash{mid})

	chain, err := FirstParentChain(repo.Storer, tip)
	if err != nil {
		t.Fatalf("FirstParentChain error: %v", err)
	}

	if len(chain) != 3 {
		t.Fatalf("expected chain of length 3, got %d: %v", len(chain), chain)
	}
	// Chain should be in root-to-tip order
	if chain[0] != root {
		t.Errorf("chain[0] = %s, want root %s", chain[0], root)
	}
	if chain[1] != mid {
		t.Errorf("chain[1] = %s, want mid %s", chain[1], mid)
	}
	if chain[2] != tip {
		t.Errorf("chain[2] = %s, want tip %s", chain[2], tip)
	}
}

func TestValidateMappingsEmpty(t *testing.T) {
	result, err := ValidateMappings(nil)
	if err != nil {
		t.Fatalf("expected nil error for empty mappings, got %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil result for empty mappings, got %v", result)
	}
}

func TestValidateMappingsValidBranch(t *testing.T) {
	normalized, err := ValidateMappings([]RefMapping{
		{Source: "main", Target: "stable"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(normalized) != 1 {
		t.Fatalf("expected 1 normalized mapping, got %d", len(normalized))
	}
	nm := normalized[0]
	if nm.SourceRef != plumbing.NewBranchReferenceName("main") {
		t.Fatalf("expected source ref refs/heads/main, got %s", nm.SourceRef)
	}
	if nm.TargetRef != plumbing.NewBranchReferenceName("stable") {
		t.Fatalf("expected target ref refs/heads/stable, got %s", nm.TargetRef)
	}
	if nm.Kind != RefKindBranch {
		t.Fatalf("expected kind branch, got %s", nm.Kind)
	}
}

func TestValidateMappingsValidFullRef(t *testing.T) {
	normalized, err := ValidateMappings([]RefMapping{
		{Source: "refs/heads/main", Target: "refs/heads/upstream-main"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(normalized) != 1 {
		t.Fatalf("expected 1 normalized mapping, got %d", len(normalized))
	}
	nm := normalized[0]
	if nm.SourceRef != "refs/heads/main" {
		t.Fatalf("expected source ref refs/heads/main, got %s", nm.SourceRef)
	}
	if nm.TargetRef != "refs/heads/upstream-main" {
		t.Fatalf("expected target ref refs/heads/upstream-main, got %s", nm.TargetRef)
	}
	if nm.Kind != RefKindBranch {
		t.Fatalf("expected kind branch, got %s", nm.Kind)
	}
}

func TestBuildDesiredRefsEmptySource(t *testing.T) {
	// Empty source ref map with a branch filter: SelectBranches finds nothing,
	// so the desired map should be empty without error.
	desired, _, err := BuildDesiredRefs(
		map[plumbing.ReferenceName]plumbing.Hash{},
		PlanConfig{Branches: []string{"main"}},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(desired) != 0 {
		t.Fatalf("expected empty desired refs for empty source, got %d", len(desired))
	}
}

func TestBuildDesiredRefsTagForceRetarget(t *testing.T) {
	// A tag that exists on both source and target with different hashes.
	// With force=true, PlanRef should give ActionUpdate.
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	sourceHash := seedCommit(t, repo, nil)
	targetHash := seedCommit(t, repo, nil)

	tagRef := plumbing.NewTagReferenceName("v1.0")
	sourceRefs := map[plumbing.ReferenceName]plumbing.Hash{
		tagRef: sourceHash,
	}

	desired, _, err := BuildDesiredRefs(sourceRefs, PlanConfig{IncludeTags: true})
	if err != nil {
		t.Fatalf("BuildDesiredRefs error: %v", err)
	}

	targetRefs := map[plumbing.ReferenceName]plumbing.Hash{
		tagRef: targetHash,
	}

	plans, err := BuildPlans(repo.Storer, desired, targetRefs, map[plumbing.ReferenceName]ManagedTarget{
		tagRef: {Kind: RefKindTag, Label: "v1.0"},
	}, PlanConfig{IncludeTags: true, Force: true})
	if err != nil {
		t.Fatalf("BuildPlans error: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected 1 plan, got %d", len(plans))
	}
	if plans[0].Action != ActionUpdate {
		t.Fatalf("expected ActionUpdate for force-retarget tag, got %s", plans[0].Action)
	}
}

func TestBuildDesiredRefsDuplicateMappingTarget(t *testing.T) {
	// Two different source refs mapping to the same target via ValidateMappings
	// should be rejected before BuildDesiredRefs even resolves hashes.
	sourceRefs := map[plumbing.ReferenceName]plumbing.Hash{
		plumbing.NewBranchReferenceName("main"):    plumbing.NewHash("1111111111111111111111111111111111111111"),
		plumbing.NewBranchReferenceName("release"): plumbing.NewHash("2222222222222222222222222222222222222222"),
	}

	_, _, err := BuildDesiredRefs(sourceRefs, PlanConfig{
		Mappings: []RefMapping{
			{Source: "main", Target: "stable"},
			{Source: "release", Target: "stable"},
		},
	})
	if err == nil {
		t.Fatalf("expected error for duplicate target ref from two different sources")
	}
}

func TestCanBootstrapRelayAllAbsent(t *testing.T) {
	hash := plumbing.NewHash("1111111111111111111111111111111111111111")
	desired := map[plumbing.ReferenceName]DesiredRef{
		"refs/heads/main": {
			Kind:       RefKindBranch,
			Label:      "main",
			SourceRef:  "refs/heads/main",
			TargetRef:  "refs/heads/main",
			SourceHash: hash,
		},
		"refs/heads/dev": {
			Kind:       RefKindBranch,
			Label:      "dev",
			SourceRef:  "refs/heads/dev",
			TargetRef:  "refs/heads/dev",
			SourceHash: hash,
		},
	}
	targetRefs := map[plumbing.ReferenceName]plumbing.Hash{}

	ok, reason := CanBootstrapRelay(false, false, desired, targetRefs)
	if !ok {
		t.Fatalf("expected CanBootstrapRelay=true when all absent, got reason: %s", reason)
	}
}

func TestCanBootstrapRelayOneExists(t *testing.T) {
	hash := plumbing.NewHash("1111111111111111111111111111111111111111")
	desired := map[plumbing.ReferenceName]DesiredRef{
		"refs/heads/main": {
			Kind:       RefKindBranch,
			Label:      "main",
			SourceRef:  "refs/heads/main",
			TargetRef:  "refs/heads/main",
			SourceHash: hash,
		},
		"refs/heads/dev": {
			Kind:       RefKindBranch,
			Label:      "dev",
			SourceRef:  "refs/heads/dev",
			TargetRef:  "refs/heads/dev",
			SourceHash: hash,
		},
	}
	targetRefs := map[plumbing.ReferenceName]plumbing.Hash{
		"refs/heads/main": plumbing.NewHash("2222222222222222222222222222222222222222"),
	}

	ok, reason := CanBootstrapRelay(false, false, desired, targetRefs)
	if ok {
		t.Fatalf("expected CanBootstrapRelay=false when one target exists")
	}
	if reason != "bootstrap-target-ref-exists" {
		t.Fatalf("unexpected reason: %s", reason)
	}
}

func TestCanIncrementalRelayMixed(t *testing.T) {
	// A mix of branch update + tag update (not create) should return false.
	// CanIncrementalRelay requires tags to have ActionCreate only.
	plans := []BranchPlan{
		{
			Branch:     "main",
			SourceRef:  "refs/heads/main",
			TargetRef:  "refs/heads/main",
			SourceHash: plumbing.NewHash("1111111111111111111111111111111111111111"),
			TargetHash: plumbing.NewHash("2222222222222222222222222222222222222222"),
			Kind:       RefKindBranch,
			Action:     ActionUpdate,
		},
		{
			Branch:     "v1.0",
			SourceRef:  "refs/tags/v1.0",
			TargetRef:  "refs/tags/v1.0",
			SourceHash: plumbing.NewHash("3333333333333333333333333333333333333333"),
			TargetHash: plumbing.NewHash("4444444444444444444444444444444444444444"),
			Kind:       RefKindTag,
			Action:     ActionUpdate, // tag update, not create
		},
	}

	// Build a minimal AdvRefs with capabilities to pass the capability check.
	advRefs := &packp.AdvRefs{}
	advRefs.Capabilities = capability.NewList()

	ok, reason := CanIncrementalRelay(false, false, false, plans, advRefs)
	if ok {
		t.Fatalf("expected CanIncrementalRelay=false for tag with ActionUpdate")
	}
	if reason != "incremental-tag-action-not-create" {
		t.Fatalf("unexpected reason: %s", reason)
	}
}

func TestCanIncrementalRelayRejectsNoThin(t *testing.T) {
	plans := []BranchPlan{{
		Branch:     "main",
		SourceRef:  "refs/heads/main",
		TargetRef:  "refs/heads/main",
		SourceHash: plumbing.NewHash("1111111111111111111111111111111111111111"),
		TargetHash: plumbing.NewHash("2222222222222222222222222222222222222222"),
		Kind:       RefKindBranch,
		Action:     ActionUpdate,
	}}

	advRefs := &packp.AdvRefs{}
	advRefs.Capabilities = capability.NewList()
	_ = advRefs.Capabilities.Set(capability.Capability("no-thin"))

	ok, reason := CanIncrementalRelay(false, false, false, plans, advRefs)
	if ok {
		t.Fatal("expected CanIncrementalRelay=false when target advertises no-thin")
	}
	if reason != "incremental-target-no-thin" {
		t.Fatalf("unexpected reason: %s", reason)
	}
}

func TestCanIncrementalRelayRejectsBranchCreate(t *testing.T) {
	plans := []BranchPlan{{
		Branch:     "main",
		SourceRef:  "refs/heads/main",
		TargetRef:  "refs/heads/main",
		SourceHash: plumbing.NewHash("1111111111111111111111111111111111111111"),
		TargetHash: plumbing.ZeroHash,
		Kind:       RefKindBranch,
		Action:     ActionCreate,
	}}

	advRefs := &packp.AdvRefs{}
	advRefs.Capabilities = capability.NewList()

	ok, reason := CanIncrementalRelay(false, false, false, plans, advRefs)
	if ok {
		t.Fatal("expected CanIncrementalRelay=false for branch create")
	}
	if reason != "incremental-branch-action-not-update" {
		t.Fatalf("unexpected reason: %s", reason)
	}
}

func TestCanFullTagCreateRelay(t *testing.T) {
	plans := []BranchPlan{{
		Branch:     "v1.0",
		SourceRef:  "refs/tags/v1.0",
		TargetRef:  "refs/tags/v1.0",
		SourceHash: plumbing.NewHash("3333333333333333333333333333333333333333"),
		TargetHash: plumbing.ZeroHash,
		Kind:       RefKindTag,
		Action:     ActionCreate,
	}}

	ok, reason := CanFullTagCreateRelay(plans)
	if !ok {
		t.Fatalf("expected CanFullTagCreateRelay=true, got reason=%s", reason)
	}
	if reason != "tag-create-full-pack" {
		t.Fatalf("unexpected reason: %s", reason)
	}
}

func TestRelayFallbackReason(t *testing.T) {
	tagCreate := []BranchPlan{{
		Branch:     "v1.0",
		SourceRef:  "refs/tags/v1.0",
		TargetRef:  "refs/tags/v1.0",
		SourceHash: plumbing.NewHash("3333333333333333333333333333333333333333"),
		TargetHash: plumbing.ZeroHash,
		Kind:       RefKindTag,
		Action:     ActionCreate,
	}}

	advRefs := &packp.AdvRefs{}
	advRefs.Capabilities = capability.NewList()

	if got := RelayFallbackReason(false, false, false, tagCreate, advRefs); got != "fast-forward-branch-or-tag-create" {
		t.Fatalf("expected fast-forward-branch-or-tag-create, got %s", got)
	}

	unsupported := []BranchPlan{{
		Branch:     "main",
		SourceRef:  "refs/heads/main",
		TargetRef:  "refs/tags/main",
		SourceHash: plumbing.NewHash("1111111111111111111111111111111111111111"),
		TargetHash: plumbing.NewHash("2222222222222222222222222222222222222222"),
		Kind:       RefKindBranch,
		Action:     ActionUpdate,
	}}
	if got := RelayFallbackReason(false, false, false, unsupported, advRefs); got != "incremental-tag-relay-non-tag-plan" {
		t.Fatalf("unexpected fallback reason: %s", got)
	}
}

func TestObjectsToPush(t *testing.T) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}
	commitHash := seedCommit(t, repo, nil)

	// ObjectsToPush with the commit and empty target refs should return at
	// least the commit hash itself (plus its tree, etc.).
	hashes, err := ObjectsToPush(repo.Storer, []plumbing.Hash{commitHash}, nil)
	if err != nil {
		t.Fatalf("ObjectsToPush error: %v", err)
	}
	if len(hashes) == 0 {
		t.Fatal("expected at least one object hash, got none")
	}

	found := false
	for _, h := range hashes {
		if h == commitHash {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected commit hash %s in returned objects", commitHash)
	}

	// If the want hash is already in the haves set, it should be excluded.
	targetRefs := map[plumbing.ReferenceName]plumbing.Hash{
		plumbing.NewBranchReferenceName("main"): commitHash,
	}
	hashes2, err := ObjectsToPush(repo.Storer, []plumbing.Hash{commitHash}, targetRefs)
	if err != nil {
		t.Fatalf("ObjectsToPush with haves error: %v", err)
	}
	if hashes2 != nil {
		t.Fatalf("expected nil when all wants are in haves, got %d objects", len(hashes2))
	}
}

func TestObjectsToPushEmpty(t *testing.T) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init repo: %v", err)
	}

	// Empty wants should return nil.
	hashes, err := ObjectsToPush(repo.Storer, nil, nil)
	if err != nil {
		t.Fatalf("ObjectsToPush error: %v", err)
	}
	if hashes != nil {
		t.Fatalf("expected nil for empty wants, got %d objects", len(hashes))
	}

	// Also test with an empty (non-nil) slice.
	hashes, err = ObjectsToPush(repo.Storer, []plumbing.Hash{}, nil)
	if err != nil {
		t.Fatalf("ObjectsToPush error: %v", err)
	}
	if hashes != nil {
		t.Fatalf("expected nil for empty wants slice, got %d objects", len(hashes))
	}
}

func seedCommit(tb testing.TB, repo *git.Repository, parents []plumbing.Hash) plumbing.Hash {
	tb.Helper()
	now := time.Now().UTC()
	obj := repo.Storer.NewEncodedObject()
	commit := &object.Commit{
		Author:       object.Signature{Name: "test", Email: "test@example.com", When: now},
		Committer:    object.Signature{Name: "test", Email: "test@example.com", When: now},
		Message:      fmt.Sprintf("test-%d-%d", len(parents), now.UnixNano()),
		TreeHash:     plumbing.ZeroHash,
		ParentHashes: parents,
	}
	if err := commit.Encode(obj); err != nil {
		tb.Fatalf("encode commit: %v", err)
	}
	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		tb.Fatalf("store commit: %v", err)
	}
	return hash
}
