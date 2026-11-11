package syncer

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage/memory"

	"entire.io/entire/git-sync/internal/planner"
)

// Config.OnProgress reaches the bootstrap strategy, and its planning figures
// reach Result, the summary line included.
func TestBootstrapReportsPlanningAndProgress(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeLargeCommits(t, sourceRepo, sourceFS, 3, 200_000)
	targetRepo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}
	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	var events []ProgressEvent
	result, err := Bootstrap(context.Background(), Config{
		Source:             Endpoint{URL: sourceServer.RepoURL()},
		Target:             Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode:       protocolModeAuto,
		TargetMaxPackBytes: 350_000,
		OnProgress:         func(e ProgressEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !result.Batching {
		t.Fatalf("expected a batched bootstrap, got %+v", result)
	}

	branch := plumbing.NewBranchReferenceName(testBranch)
	tip, err := sourceRepo.Reference(branch, true)
	if err != nil {
		t.Fatalf("resolve source tip: %v", err)
	}
	if len(events) < 2 {
		t.Fatalf("progress events = %v, want the checkpoints and the branch create", events)
	}
	advanced := 0
	for _, e := range events[:len(events)-1] {
		if e.Kind != ProgressTempRefAdvanced {
			t.Errorf("event %v before the create, want only temp-ref advances", e)
		}
		advanced++
	}
	if advanced != result.BatchCount {
		t.Errorf("temp-ref advances = %d, want one per landed checkpoint (%d)", advanced, result.BatchCount)
	}
	if last := events[len(events)-1]; last != (ProgressEvent{Kind: ProgressRefCreated, Ref: branch, Hash: tip.Hash()}) {
		t.Errorf("last event = %v, want %s created at %s", last, branch, tip.Hash())
	}

	if result.Planning.SourceRefs < 1 || result.Planning.BranchesTotal != 1 ||
		result.Planning.BranchesPlanned != 1 || result.Planning.BranchesSubsumed != 0 || result.Planning.PlanningNanos <= 0 {
		t.Errorf("Planning = %+v, want one planned branch out of one, with time spent", result.Planning)
	}
	if text := strings.Join(result.Lines(), "\n"); !strings.Contains(text, "planning: source-refs=") ||
		!strings.Contains(text, " branches=1 planned=1 subsumed=0 duration=") {
		t.Errorf("Lines() missing the planning line:\n%s", text)
	}
}

// The attempt this exists for: the deadline lands inside the trunk's
// commit-graph fetch, before anything is pushed. The error path must still
// carry the branch count and the time planning burnt.
func TestBootstrapCarriesPlanningStatsWhenPlanningTimesOut(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeLargeCommits(t, sourceRepo, sourceFS, 3, 200_000)
	targetRepo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}
	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	release := make(chan struct{})
	// Every fetch stalls until the run has given up: the first one is the
	// commit-graph fetch, so planning never finishes.
	sourceServer.uploadPackV2FetchRaw = func(w http.ResponseWriter, _ v2TestCommandRequest, _ []byte) bool {
		<-release
		http.Error(w, "too late", http.StatusServiceUnavailable)
		return true
	}
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()
	defer close(release)

	var events []ProgressEvent
	const budget = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	result, err := Bootstrap(ctx, Config{
		Source:             Endpoint{URL: sourceServer.RepoURL()},
		Target:             Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode:       protocolModeAuto,
		TargetMaxPackBytes: 350_000,
		OnProgress:         func(e ProgressEvent) { events = append(events, e) },
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bootstrap error = %v, want the deadline", err)
	}
	if !result.Batching {
		t.Fatalf("expected the batched route on the error path, got %+v", result)
	}
	if result.Planning.SourceRefs < 1 || result.Planning.BranchesTotal != 1 || result.Planning.BranchesPlanned != 0 {
		t.Errorf("Planning = %+v, want one branch to plan and none planned", result.Planning)
	}
	// Most of the budget went on the stalled fetch; the ref listing before it
	// is a local round trip.
	if got := time.Duration(result.Planning.PlanningNanos); got < budget/2 {
		t.Errorf("planning duration = %s, want the stalled fetch counted (budget %s)", got, budget)
	}
	if len(events) != 0 {
		t.Errorf("progress events = %v, want none: nothing landed", events)
	}
}

// In strict mode (the default) a refused update fails the push, but the push
// is not atomic: a branch create the target applied beside a refused temp ref
// has landed and must still be reported.
func TestRun_BootstrapStrictReportsCreateBesideRefusedTempRef(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeLargeCommits(t, sourceRepo, sourceFS, 4, 5_000)
	branchRef := plumbing.NewBranchReferenceName(testBranch)
	tempRef := planner.BootstrapTempRef(branchRef)

	targetRepo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}
	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	// A target that refuses refs/gitsync/* but allows branches.
	denier := &refCreateDenier{repo: targetRepo, ref: tempRef, reason: "deny refs/gitsync"}
	targetServer.receivePackHook = denier.hook
	targetServer.receivePackUnpackForHook = true

	var events []ProgressEvent
	_, err = Run(context.Background(), Config{
		Source:             Endpoint{URL: sourceServer.RepoURL()},
		Target:             Endpoint{URL: targetServer.RepoURL()},
		Mode:               modeReplicate,
		ProtocolMode:       protocolModeAuto,
		TargetMaxPackBytes: 10_000_000, // batched, with the whole branch in one checkpoint
		OnProgress:         func(e ProgressEvent) { events = append(events, e) },
	})
	if err == nil {
		t.Fatal("expected the refused temp ref to fail the strict run")
	}
	if denier.denialCount() == 0 {
		t.Fatal("test setup: no push carried the temp ref")
	}
	tip, err := targetRepo.Reference(branchRef, true)
	if err != nil {
		t.Fatalf("test setup: expected %s on the target beside the refused temp ref: %v", branchRef, err)
	}
	want := []ProgressEvent{{Kind: ProgressRefCreated, Ref: branchRef, Hash: tip.Hash()}}
	if !slices.Equal(events, want) {
		t.Errorf("progress events = %v, want %v", events, want)
	}
}
