package syncer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	"entire.io/entire/git-sync/internal/planner"
)

const nativePrefix = "refs/heads/entire/native/"

// The reason this policy exists: the owner deleted its last ref in the
// replicated namespace, so the in-scope desired set is empty while the source
// is plainly populated. That must prune the target's copies — the historical
// error left every replica serving a branch the owner no longer has — and it
// must leave every ref outside the namespace exactly where it is.
func TestRun_IntegrationEmptyScopePrunesNamespaceOnly(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 2)
	targetRepo, _ := newSourceRepo(t)

	sourceServer := newSmartHTTPRepoServer(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	targetServer.receivePackThinCap = true
	defer sourceServer.Close()
	defer targetServer.Close()

	// Seed the target with the source's branch, then give it refs the source
	// does not have: one inside the replicated namespace, one outside it.
	if _, err := Run(context.Background(), Config{
		Source: Endpoint{URL: sourceServer.RepoURL()},
		Target: Endpoint{URL: targetServer.RepoURL()},
	}); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	targetHead, err := targetRepo.Reference(plumbing.NewBranchReferenceName(testBranch), true)
	if err != nil {
		t.Fatalf("target head after seed: %v", err)
	}
	inScope := plumbing.ReferenceName(nativePrefix + "stale")
	outOfScope := plumbing.NewBranchReferenceName("release")
	for _, ref := range []plumbing.ReferenceName{inScope, outOfScope} {
		if err := targetRepo.Storer.SetReference(plumbing.NewHashReference(ref, targetHead.Hash())); err != nil {
			t.Fatalf("seed %s: %v", ref, err)
		}
	}

	result, err := Run(context.Background(), Config{
		Source:             Endpoint{URL: sourceServer.RepoURL()},
		Target:             Endpoint{URL: targetServer.RepoURL()},
		Mode:               modeReplicate,
		AllRefs:            true,
		Prune:              true,
		IncludeRefPrefixes: []string{nativePrefix},
		AllowEmptyScope:    true,
	})
	if err != nil {
		t.Fatalf("scoped replicate: %v", err)
	}
	if result.Deleted != 1 || result.Pushed != 0 {
		t.Fatalf("expected exactly one deletion and no pushes, got %+v", result)
	}
	if _, err := targetRepo.Reference(inScope, true); !errors.Is(err, plumbing.ErrReferenceNotFound) {
		t.Fatalf("expected %s pruned, got err=%v", inScope, err)
	}
	for _, ref := range []plumbing.ReferenceName{outOfScope, plumbing.NewBranchReferenceName(testBranch)} {
		got, err := targetRepo.Reference(ref, true)
		if err != nil {
			t.Fatalf("out-of-scope ref %s removed by a scoped run: %v", ref, err)
		}
		if got.Hash() != targetHead.Hash() {
			t.Fatalf("out-of-scope ref %s changed: %s -> %s", ref, targetHead.Hash(), got.Hash())
		}
	}
}

// The line the policy must not cross. A source that advertised nothing at all
// is the unverifiable case — a hidden or withheld listing looks identical — so
// an empty in-scope set proves nothing and the target keeps its refs.
func TestRun_IntegrationEmptyScopeRefusesSilentSource(t *testing.T) {
	sourceRepo, _ := newSourceRepo(t)
	targetRepo, targetFS := newSourceRepo(t)
	makeCommits(t, targetRepo, targetFS, 1)

	sourceServer := newSmartHTTPRepoServer(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	targetServer.receivePackThinCap = true
	defer sourceServer.Close()
	defer targetServer.Close()

	targetHead, err := targetRepo.Reference(plumbing.NewBranchReferenceName(testBranch), true)
	if err != nil {
		t.Fatalf("target head: %v", err)
	}
	inScope := plumbing.ReferenceName(nativePrefix + "kept")
	if err := targetRepo.Storer.SetReference(plumbing.NewHashReference(inScope, targetHead.Hash())); err != nil {
		t.Fatalf("seed %s: %v", inScope, err)
	}

	_, err = Run(context.Background(), Config{
		Source:             Endpoint{URL: sourceServer.RepoURL()},
		Target:             Endpoint{URL: targetServer.RepoURL()},
		Mode:               modeReplicate,
		AllRefs:            true,
		Prune:              true,
		IncludeRefPrefixes: []string{nativePrefix},
		AllowEmptyScope:    true,
	})
	if !errors.Is(err, ErrSourceEmptyUnverified) {
		t.Fatalf("expected ErrSourceEmptyUnverified for a source that advertised nothing, got %v", err)
	}
	if _, err := targetRepo.Reference(inScope, true); err != nil {
		t.Fatalf("in-scope target ref deleted on an unverifiable empty source: %v", err)
	}
}

// The two empty-set policies answer the same wire observation differently, so
// a request carrying both is rejected at the edge every entry point shares
// rather than silently resolving to one of them.
func TestValidateEmptySourcePolicyScopeRules(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		cfg  Config
		want string
	}{
		"empty scope without include prefixes": {
			Config{Mode: modeReplicate, AllowEmptyScope: true},
			"AllowEmptyScope requires IncludeRefPrefixes",
		},
		// Only replicate consults the policy; every other mode would carry it
		// in, return the historical error, and keep the refs it was meant to
		// prune — the silent-no-op class this whole family exists to prevent.
		// Both of these leave the policy inert in a way that reads as success:
		// without prune the empty in-scope set plans nothing and the run goes
		// green over the refs it was meant to reap, and without AllRefs the
		// advertisement emptyScopePrunes reads is itself narrowed, so "the
		// source showed us refs" stops meaning what the rule needs it to mean.
		"empty scope without prune": {
			Config{Mode: modeReplicate, AllRefs: true, AllowEmptyScope: true, IncludeRefPrefixes: []string{nativePrefix}},
			"requires Prune",
		},
		"empty scope without all refs": {
			Config{Mode: modeReplicate, Prune: true, AllowEmptyScope: true, IncludeRefPrefixes: []string{nativePrefix}},
			"requires AllRefs",
		},
		"empty scope outside replicate": {
			Config{Mode: modeSync, AllRefs: true, Prune: true, AllowEmptyScope: true, IncludeRefPrefixes: []string{nativePrefix}},
			"applies to replicate only",
		},
		"empty source with include prefixes": {
			Config{Mode: modeReplicate, AllRefs: true, AllowEmptySource: true, IncludeRefPrefixes: []string{nativePrefix}},
			"mutually exclusive",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := validateEmptySourcePolicy(tc.cfg)
			if err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}

	// The combination each rule permits still validates, so neither rule
	// widened into the configurations the policies exist for.
	for name, cfg := range map[string]Config{
		"scoped prune":   {Mode: modeReplicate, AllRefs: true, Prune: true, AllowEmptyScope: true, IncludeRefPrefixes: []string{nativePrefix}},
		"unscoped empty": {Mode: modeReplicate, AllRefs: true, AllowEmptySource: true},
	} {
		if err := validateEmptySourcePolicy(cfg); err != nil {
			t.Errorf("%s: unexpected validation error: %v", name, err)
		}
	}
}

// The resume route reads the target's markers, so it asks the target-side
// scope question too: a marker swept out of scope by an include prefix is a
// bootstrap that can never be resumed, on exactly the large repositories
// batching exists for.
func TestHasBootstrapResumeMarkerUnderIncludeScope(t *testing.T) {
	t.Parallel()
	branch := plumbing.ReferenceName(nativePrefix + "one")
	marker := planner.BootstrapTempRef(branch)
	s := emptySourceSession(
		Config{AllRefs: true, Prune: true, IncludeRefPrefixes: []string{nativePrefix}},
		nil,
		map[plumbing.ReferenceName]plumbing.Hash{marker: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
		unbornSource(),
	)
	desired := map[plumbing.ReferenceName]planner.DesiredRef{branch: {TargetRef: branch}}
	if !s.hasBootstrapResumeMarker(desired) {
		t.Fatal("resume marker undetected under a prefix-scoped request; the bootstrap can never resume")
	}
}

// Mappings bypass the include prefixes in discovery, so a mapped branch
// outside them is still this run's to bootstrap — and its marker is still its
// resume state. Asking the prefixes alone disowned it, which loses the ENT-2054
// route on exactly the branch the caller named explicitly.
func TestHasBootstrapResumeMarkerForMappedBranch(t *testing.T) {
	t.Parallel()
	branch := plumbing.NewBranchReferenceName("main")
	marker := planner.BootstrapTempRef(branch)
	s := emptySourceSession(
		Config{
			AllRefs:            true,
			Prune:              true,
			IncludeRefPrefixes: []string{nativePrefix},
			Mappings:           []RefMapping{{Source: "refs/heads/main", Target: "refs/heads/main"}},
		},
		nil,
		map[plumbing.ReferenceName]plumbing.Hash{marker: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
		unbornSource(),
	)
	desired := map[plumbing.ReferenceName]planner.DesiredRef{branch: {TargetRef: branch}}
	if !s.hasBootstrapResumeMarker(desired) {
		t.Fatal("resume marker for an explicitly mapped branch went undetected; its bootstrap can never resume")
	}
}
