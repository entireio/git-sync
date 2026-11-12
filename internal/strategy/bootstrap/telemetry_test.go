package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"

	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
)

// telemetryHarness is the fan-out bootstrap of newGroupHarness — trunk "main"
// plus three one-commit forks — with one more branch, "old", whose tip is in
// the trunk's history and so is subsumed. It records progress events, and
// failGraph makes the Nth commit-graph fetch (1-based) fail with a deadline
// after spending a little time, the way a timed-out attempt does.
type telemetryHarness struct {
	*groupHarness

	oldRef    plumbing.ReferenceName
	oldTip    plumbing.Hash
	events    []ProgressEvent
	failGraph int
	graphs    int
}

const telemetryFetchDelay = 2 * time.Millisecond

func newTelemetryHarness(t *testing.T) *telemetryHarness {
	t.Helper()
	h := &telemetryHarness{groupHarness: newGroupHarness(t, 3), oldRef: plumbing.NewBranchReferenceName("old")}
	h.oldTip = h.parents[h.trunkTip][0]
	h.params.DesiredRefs[h.oldRef] = planner.DesiredRef{
		SourceRef: h.oldRef, TargetRef: h.oldRef, SourceHash: h.oldTip, Kind: planner.RefKindBranch, Label: "old",
	}
	h.params.SourceRefCount = 42
	h.params.OnProgress = func(e ProgressEvent) { h.events = append(h.events, e) }
	h.params.SourceService = fakeBootstrapSource{
		fetchCommitParents: func(_ context.Context, _ gitproto.Conn, wants []plumbing.Hash, haves []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
			h.graphs++
			if h.graphs == h.failGraph {
				time.Sleep(telemetryFetchDelay)
				return nil, fmt.Errorf("read commit graph: %w", context.DeadlineExceeded)
			}
			// What a real fetch sends: the wants' history minus the haves'.
			sent := reachableParents(h.parents, wants...)
			for hash := range reachableParents(h.parents, haves...) {
				delete(sent, hash)
			}
			return sent, nil
		},
		fetchPack: func(context.Context, gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
			return packWithObjects(1), nil
		},
	}
	return h
}

func (h *telemetryHarness) created(ref plumbing.ReferenceName, hash plumbing.Hash) ProgressEvent {
	return ProgressEvent{Kind: ProgressRefCreated, Ref: ref, Hash: hash}
}

func (h *telemetryHarness) tipOf(ref plumbing.ReferenceName) plumbing.Hash {
	return h.params.DesiredRefs[ref].SourceHash
}

func assertPlanning(t *testing.T, got PlanningStats, want PlanningStats, minDuration time.Duration) {
	t.Helper()
	if got.SourceRefs != want.SourceRefs || got.BranchesTotal != want.BranchesTotal ||
		got.BranchesPlanned != want.BranchesPlanned || got.BranchesSubsumed != want.BranchesSubsumed {
		t.Errorf("Planning = %+v, want counts %+v", got, want)
	}
	if got.PlanningDuration <= 0 || got.PlanningDuration < minDuration {
		t.Errorf("PlanningDuration = %s, want > 0 and >= %s", got.PlanningDuration, minDuration)
	}
}

func TestExecuteBatchedReportsPlanningStats(t *testing.T) {
	t.Parallel()
	h := newTelemetryHarness(t)

	result, err := Execute(context.Background(), h.params, "empty target")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Five branches: the trunk and three forks get checkpoints, "old" is
	// subsumed by the trunk.
	assertPlanning(t, result.Planning, PlanningStats{SourceRefs: 42, BranchesTotal: 5, BranchesPlanned: 4, BranchesSubsumed: 1}, 0)
}

// A deadline inside the trunk's commit-graph fetch, the first thing a large
// repository does, must still report the branch count and the time burnt:
// that is the attempt that otherwise looks like 30 minutes of nothing.
func TestExecuteBatchedPlanningStatsSurviveTrunkDeadline(t *testing.T) {
	t.Parallel()
	h := newTelemetryHarness(t)
	h.failGraph = 1

	result, err := Execute(context.Background(), h.params, "empty target")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Execute error = %v, want the deadline", err)
	}
	assertPlanning(t, result.Planning, PlanningStats{SourceRefs: 42, BranchesTotal: 5}, telemetryFetchDelay)
	if len(h.events) != 0 {
		t.Errorf("progress events = %v, want none before anything landed", h.events)
	}
}

// A deadline in the remaining branches' shared fetch: the trunk is planned
// (and landed), "old" was classified as subsumed before the fetch, and the
// duration adds the failed fetch to the trunk's planning.
func TestExecuteBatchedPlanningStatsSurviveRemainingBranchesDeadline(t *testing.T) {
	t.Parallel()
	h := newTelemetryHarness(t)
	h.failGraph = 2

	result, err := Execute(context.Background(), h.params, "empty target")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Execute error = %v, want the deadline", err)
	}
	assertPlanning(t, result.Planning, PlanningStats{SourceRefs: 42, BranchesTotal: 5, BranchesPlanned: 1, BranchesSubsumed: 1}, telemetryFetchDelay)
	// The trunk landed before the deadline, and said so.
	want := []ProgressEvent{
		{Kind: ProgressTempRefAdvanced, Ref: planner.BootstrapTempRef(h.trunkRef), Hash: h.trunkTip},
		h.created(h.trunkRef, h.trunkTip),
	}
	if !slices.Equal(h.events, want) {
		t.Errorf("progress events = %v, want %v", h.events, want)
	}
}

// A failure while deriving one branch's checkpoints from the shared graph
// leaves the count at the branches planned before it. checkpointsFromParents
// does no network I/O, so the failure here is a malformed graph rather than a
// deadline; the accounting is the same.
func TestExecuteBatchedPlanningStatsCountBranchesBeforePerBranchFailure(t *testing.T) {
	t.Parallel()
	h := newTelemetryHarness(t)
	last := h.forkRefs[len(h.forkRefs)-1]
	lastTip := h.tipOf(last)
	inner, ok := h.params.SourceService.(fakeBootstrapSource)
	if !ok {
		t.Fatalf("SourceService is %T, want fakeBootstrapSource", h.params.SourceService)
	}
	h.params.SourceService = fakeBootstrapSource{
		fetchPack: inner.fetchPack,
		fetchCommitParents: func(ctx context.Context, conn gitproto.Conn, wants, haves []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
			parents, err := inner.fetchCommitParents(ctx, conn, wants, haves)
			if _, ok := parents[lastTip]; ok && len(wants) > 1 {
				parents[lastTip] = []plumbing.Hash{lastTip} // a cycle the walk rejects
			}
			return parents, err
		},
	}

	result, err := Execute(context.Background(), h.params, "empty target")
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("Execute error = %v, want the cycle to fail planning", err)
	}
	// Trunk, then the two forks ordered before the broken one.
	assertPlanning(t, result.Planning, PlanningStats{SourceRefs: 42, BranchesTotal: 5, BranchesPlanned: 3, BranchesSubsumed: 1}, 0)
}

// With no trunk but refs already on the target, every branch is planned in
// the deferred phase against those refs, and a branch they already reach is
// counted as subsumed.
func TestExecuteBatchedPlanningStatsWithoutTrunkAgainstTargetRefs(t *testing.T) {
	t.Parallel()
	h := newTelemetryHarness(t)
	h.params.SourceHeadTarget = ""
	h.params.TargetRefs = map[plumbing.ReferenceName]plumbing.Hash{plumbing.NewBranchReferenceName("existing"): h.oldTip}

	result, err := Execute(context.Background(), h.params, "replicate-pack-rejected")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	assertPlanning(t, result.Planning, PlanningStats{SourceRefs: 42, BranchesTotal: 5, BranchesPlanned: 4, BranchesSubsumed: 1}, 0)
}

// With neither a trunk nor target refs each branch is planned in turn, and a
// failure partway reports the ones before it.
func TestExecuteBatchedPlanningStatsSerialPlanning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		failGraph int
		planned   int
	}{
		{name: "complete", planned: 5},
		{name: "deadline on third branch", failGraph: 3, planned: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newTelemetryHarness(t)
			h.params.SourceHeadTarget = ""
			h.failGraph = tc.failGraph

			result, err := Execute(context.Background(), h.params, "empty target")
			switch {
			case tc.failGraph == 0 && err != nil:
				t.Fatalf("Execute: %v", err)
			case tc.failGraph > 0 && !errors.Is(err, context.DeadlineExceeded):
				t.Fatalf("Execute error = %v, want the deadline", err)
			}
			assertPlanning(t, result.Planning, PlanningStats{SourceRefs: 42, BranchesTotal: 5, BranchesPlanned: tc.planned}, 0)
		})
	}
}

// One-shot plans no checkpoints: it reports the advertisement and the branch
// count, and nothing that would read as a planning phase.
func TestExecuteOneShotReportsBranchCountOnly(t *testing.T) {
	t.Parallel()
	mainRef := plumbing.NewBranchReferenceName("main")
	devRef := plumbing.NewBranchReferenceName("dev")
	tagRef := plumbing.NewTagReferenceName("v1")
	hash := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	var events []ProgressEvent
	result, err := Execute(context.Background(), Params{
		SourceService: fakeBootstrapSource{
			fetchPack: func(context.Context, gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader([]byte("PACK"))), nil
			},
		},
		TargetPusher: fakeBootstrapPusher{pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
			return pack.Close()
		}},
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			mainRef: {SourceRef: mainRef, TargetRef: mainRef, SourceHash: hash, Kind: planner.RefKindBranch},
			devRef:  {SourceRef: devRef, TargetRef: devRef, SourceHash: hash, Kind: planner.RefKindBranch},
			tagRef:  {SourceRef: tagRef, TargetRef: tagRef, SourceHash: hash, Kind: planner.RefKindTag},
		},
		TargetRefs:       map[plumbing.ReferenceName]plumbing.Hash{},
		SourceHeadTarget: mainRef,
		SourceRefCount:   9,
		// dev's create is refused: the one pack landed, but not that ref.
		RefOutcome: func(name plumbing.ReferenceName) (gitproto.RefOutcome, string) {
			if name == devRef {
				return gitproto.RefOutcomeRefused, "protected"
			}
			return gitproto.RefOutcomeApplied, ""
		},
		OnProgress: func(e ProgressEvent) { events = append(events, e) },
	}, "empty target")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if want := (PlanningStats{SourceRefs: 9, BranchesTotal: 2}); result.Planning != want {
		t.Errorf("Planning = %+v, want %+v", result.Planning, want)
	}
	// Push order: the HEAD branch hoisted first, then the rest by name.
	want := []ProgressEvent{
		{Kind: ProgressRefCreated, Ref: mainRef, Hash: hash},
		{Kind: ProgressRefCreated, Ref: tagRef, Hash: hash},
	}
	if !slices.Equal(events, want) {
		t.Errorf("progress events = %v, want %v", events, want)
	}
}

// One event per landed checkpoint and per created ref, in the order they
// landed: the trunk's checkpoint and cutover, the subsumed branch, the group.
func TestExecuteBatchedReportsProgress(t *testing.T) {
	t.Parallel()
	h := newTelemetryHarness(t)

	if _, err := Execute(context.Background(), h.params, "empty target"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := []ProgressEvent{
		{Kind: ProgressTempRefAdvanced, Ref: planner.BootstrapTempRef(h.trunkRef), Hash: h.trunkTip},
		h.created(h.trunkRef, h.trunkTip),
		h.created(h.oldRef, h.oldTip),
	}
	for _, ref := range h.forkRefs {
		want = append(want, h.created(ref, h.tipOf(ref)))
	}
	if !slices.Equal(h.events, want) {
		t.Errorf("progress events:\n got %v\nwant %v", h.events, want)
	}
}

// A branch planned as several checkpoints reports each one as it lands, so a
// long branch shows progress between its first push and its cutover.
func TestExecuteBatchedReportsEachCheckpoint(t *testing.T) {
	t.Parallel()
	h := newTelemetryHarness(t)
	// Four trunk commits at the 64 KiB-per-commit estimate: two batches.
	h.params.TargetMaxPack = 2 * estimatedBytesPerCommit

	if _, err := Execute(context.Background(), h.params, "empty target"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	temp := planner.BootstrapTempRef(h.trunkRef)
	var advanced []plumbing.Hash
	for _, e := range h.events {
		if e.Kind == ProgressTempRefAdvanced {
			if e.Ref != temp {
				t.Errorf("temp-ref event for %s, want only %s", e.Ref, temp)
			}
			advanced = append(advanced, e.Hash)
		}
	}
	if len(advanced) != 2 || advanced[1] != h.trunkTip {
		t.Fatalf("temp-ref advances = %v, want 2 ending at the trunk tip %s", advanced, h.trunkTip)
	}
}

func TestExecuteBatchedWithoutProgressCallback(t *testing.T) {
	t.Parallel()
	h := newTelemetryHarness(t)
	h.params.OnProgress = nil

	if _, err := Execute(context.Background(), h.params, "empty target"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// A create the target refused is not progress; nor is a temp ref it refused,
// though a branch created in the same push still is. A refused branch create
// that the closing ref listing then finds on the target, at the hash this run
// pushed, is.
func TestExecuteBatchedProgressSkipsRefusedUpdates(t *testing.T) {
	t.Parallel()
	mainRef := plumbing.NewBranchReferenceName("main")

	t.Run("refused branch create", func(t *testing.T) {
		t.Parallel()
		h := newBatchedRefusalHarness(t, map[plumbing.ReferenceName]string{mainRef: "deny creating a protected branch"})
		var events []ProgressEvent
		h.params.OnProgress = func(e ProgressEvent) { events = append(events, e) }
		if _, err := Execute(context.Background(), h.params, "empty target"); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		want := []ProgressEvent{
			{Kind: ProgressTempRefAdvanced, Ref: h.trunkTempRef, Hash: h.trunkTip},
			{Kind: ProgressRefCreated, Ref: h.forkRef, Hash: h.params.DesiredRefs[h.forkRef].SourceHash},
		}
		if !slices.Equal(events, want) {
			t.Errorf("progress events = %v, want %v", events, want)
		}
	})

	t.Run("refused create found by listing", func(t *testing.T) {
		t.Parallel()
		h := newBatchedRefusalHarness(t, map[plumbing.ReferenceName]string{mainRef: "ref exists"})
		h.refsNow[mainRef] = h.trunkTip
		var events []ProgressEvent
		h.params.OnProgress = func(e ProgressEvent) { events = append(events, e) }
		if _, err := Execute(context.Background(), h.params, "empty target"); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		want := []ProgressEvent{
			{Kind: ProgressTempRefAdvanced, Ref: h.trunkTempRef, Hash: h.trunkTip},
			{Kind: ProgressRefCreated, Ref: h.forkRef, Hash: h.params.DesiredRefs[h.forkRef].SourceHash},
			{Kind: ProgressRefCreated, Ref: mainRef, Hash: h.trunkTip},
		}
		if !slices.Equal(events, want) {
			t.Errorf("progress events = %v, want %v", events, want)
		}
	})

	t.Run("refused temp ref", func(t *testing.T) {
		t.Parallel()
		h := newBatchedRefusalHarness(t, map[plumbing.ReferenceName]string{
			planner.BootstrapTempRef(mainRef): "deny refs/gitsync",
		})
		var events []ProgressEvent
		h.params.OnProgress = func(e ProgressEvent) { events = append(events, e) }
		if _, err := Execute(context.Background(), h.params, "empty target"); err == nil {
			t.Fatal("Execute succeeded, want the refused temp ref to stop the run")
		}
		// The trunk's only checkpoint push carries the temp ref and the
		// branch create. The run stops on the refused temp ref, which is
		// not progress, but the branch the target created is.
		want := []ProgressEvent{{Kind: ProgressRefCreated, Ref: mainRef, Hash: h.trunkTip}}
		if !slices.Equal(events, want) {
			t.Errorf("progress events = %v, want %v", events, want)
		}
	})

	t.Run("refused subsumed create", func(t *testing.T) {
		t.Parallel()
		h := newSubsumedRefusalHarness(t, map[plumbing.ReferenceName]string{
			plumbing.NewBranchReferenceName("release"): "protected",
		})
		var events []ProgressEvent
		h.params.OnProgress = func(e ProgressEvent) { events = append(events, e) }
		if _, err := Execute(context.Background(), h.params, "empty target"); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		for _, e := range events {
			if e.Ref == h.forkRef {
				t.Errorf("refused subsumed create reported as progress: %v", e)
			}
		}
	})
}

// Tags ride the tail phase; each confirmed create is reported after the
// branches.
func TestExecuteBatchedReportsTailCreates(t *testing.T) {
	t.Parallel()
	h := newTelemetryHarness(t)
	tagRef := plumbing.NewTagReferenceName("v1")
	h.params.DesiredRefs[tagRef] = planner.DesiredRef{SourceRef: tagRef, TargetRef: tagRef, SourceHash: h.trunkTip, Kind: planner.RefKindTag}

	result, err := Execute(context.Background(), h.params, "empty target")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := h.events[len(h.events)-1]; got != h.created(tagRef, h.trunkTip) {
		t.Errorf("last progress event = %v, want the tag create", got)
	}
	if result.Planning.BranchesTotal != 5 {
		t.Errorf("BranchesTotal = %d, want 5: a tag is not a planned branch", result.Planning.BranchesTotal)
	}
}

// failingPusher makes every pack push fail with an error batching cannot fix,
// the way a later ref-update request fails after an earlier one landed.
func failingPusher(t *testing.T, h *telemetryHarness) {
	t.Helper()
	pusher, ok := h.params.TargetPusher.(fakeBootstrapPusher)
	if !ok {
		t.Fatalf("TargetPusher is %T, want fakeBootstrapPusher", h.params.TargetPusher)
	}
	pusher.pushPack = func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
		_ = pack.Close()
		return errors.New("later ref-update request: connection reset")
	}
	h.params.TargetPusher = pusher
}

// appliedOnly reports ref as applied and says nothing about any other ref.
func appliedOnly(ref plumbing.ReferenceName) func(plumbing.ReferenceName) (gitproto.RefOutcome, string) {
	return func(name plumbing.ReferenceName) (gitproto.RefOutcome, string) {
		if name == ref {
			return gitproto.RefOutcomeApplied, ""
		}
		return gitproto.RefOutcomeUnknown, ""
	}
}

func TestExecuteOneShotReportsCreatesLandedBeforePushFailed(t *testing.T) {
	h := newTelemetryHarness(t)
	h.params.TargetMaxPack = 0
	failingPusher(t, h)
	h.params.RefOutcome = appliedOnly(h.trunkRef)

	if _, err := Execute(context.Background(), h.params, "empty target"); err == nil {
		t.Fatal("expected the push error")
	}
	want := []ProgressEvent{h.created(h.trunkRef, h.trunkTip)}
	if !slices.Equal(h.events, want) {
		t.Fatalf("events = %v, want %v", h.events, want)
	}
}

func TestBranchGroupReportsMembersLandedBeforePushFailed(t *testing.T) {
	h := newTelemetryHarness(t)
	failingPusher(t, h)
	applied := h.forkRefs[0]
	h.params.RefOutcome = appliedOnly(applied)

	batches, err := planAfterTrunk(context.Background(), h.params, h.deferredForks(), h.params.TargetMaxPack, &PlanningStats{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pushBranchGroups(context.Background(), h.params, batches[0].group, newTestGroupState(h.params.TargetMaxPack), &Result{})
	if err == nil {
		t.Fatal("expected the push error")
	}
	want := []ProgressEvent{h.created(applied, h.tipOf(applied))}
	if !slices.Equal(h.events, want) {
		t.Fatalf("events = %v, want %v", h.events, want)
	}
}

func TestCreatedOnceReportsEachRefCreateOnce(t *testing.T) {
	var got []ProgressEvent
	report := createdOnce(func(ev ProgressEvent) { got = append(got, ev) })
	main := plumbing.NewBranchReferenceName("main")
	temp := plumbing.ReferenceName("refs/gitsync/bootstrap/heads/main")
	created := ProgressEvent{Kind: ProgressRefCreated, Ref: main, Hash: plumbing.NewHash("01")}
	advanced := ProgressEvent{Kind: ProgressTempRefAdvanced, Ref: temp, Hash: plumbing.NewHash("02")}

	report(advanced)
	report(created)
	report(advanced)
	report(created)

	want := []ProgressEvent{advanced, created, advanced}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if createdOnce(nil) != nil {
		t.Fatal("createdOnce(nil) must stay nil")
	}
}

func TestExecuteBatchedReportsTailCreatesLandedBeforePushFailed(t *testing.T) {
	h := newTelemetryHarness(t)
	tag := plumbing.NewTagReferenceName("v1")
	other := plumbing.NewTagReferenceName("v2")
	h.params.DesiredRefs = map[plumbing.ReferenceName]planner.DesiredRef{
		tag:   {SourceRef: tag, TargetRef: tag, SourceHash: h.trunkTip, Kind: planner.RefKindTag},
		other: {SourceRef: other, TargetRef: other, SourceHash: h.trunkTip, Kind: planner.RefKindTag},
	}
	failingPusher(t, h)
	h.params.RefOutcome = appliedOnly(tag)

	if _, err := Execute(context.Background(), h.params, "empty target"); err == nil {
		t.Fatal("expected the push error")
	}
	want := []ProgressEvent{h.created(tag, h.trunkTip)}
	if !slices.Equal(h.events, want) {
		t.Fatalf("events = %v, want %v", h.events, want)
	}
}

func TestExecuteBatchedReportsBranchCreateLandedBeforeCheckpointPushFailed(t *testing.T) {
	h := newTelemetryHarness(t)
	failingPusher(t, h)
	// The final checkpoint's push carries the temp ref and the branch create.
	// Only the create is progress; the run does not advance on a failed push.
	h.params.RefOutcome = func(name plumbing.ReferenceName) (gitproto.RefOutcome, string) {
		if name == h.trunkRef || name == planner.BootstrapTempRef(h.trunkRef) {
			return gitproto.RefOutcomeApplied, ""
		}
		return gitproto.RefOutcomeUnknown, ""
	}

	if _, err := Execute(context.Background(), h.params, "empty target"); err == nil {
		t.Fatal("expected the push error")
	}
	want := []ProgressEvent{h.created(h.trunkRef, h.trunkTip)}
	if !slices.Equal(h.events, want) {
		t.Fatalf("events = %v, want %v", h.events, want)
	}
}
