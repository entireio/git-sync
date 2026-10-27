package bootstrap

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"

	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
)

// fanOutCommitGraph builds a linear trunk plus forks single-commit branches,
// each hanging off the trunk's second commit.
func fanOutCommitGraph(tb testing.TB, trunkLen, forks int) (parents map[plumbing.Hash][]plumbing.Hash, trunkTip plumbing.Hash, forkTips []plumbing.Hash) {
	tb.Helper()
	store := memory.NewStorage()
	trunk := writeLinearCommitChain(tb, store, trunkLen)
	for i := range forks {
		obj := store.NewEncodedObject()
		when := time.Unix(int64(10_000+i), 0).UTC()
		commit := &object.Commit{
			Author:       object.Signature{Name: "test", Email: "test@example.com", When: when},
			Committer:    object.Signature{Name: "test", Email: "test@example.com", When: when},
			Message:      fmt.Sprintf("fork-%d", i),
			TreeHash:     plumbing.ZeroHash,
			ParentHashes: []plumbing.Hash{trunk[1]},
		}
		if err := commit.Encode(obj); err != nil {
			tb.Fatalf("encode fork %d: %v", i, err)
		}
		hash, err := store.SetEncodedObject(obj)
		if err != nil {
			tb.Fatalf("store fork %d: %v", i, err)
		}
		forkTips = append(forkTips, hash)
	}
	return parentsFromCommitChainStore(tb, store), trunk[trunkLen-1], forkTips
}

// packWithObjects is a pack header announcing count objects, enough for the
// size pre-check to estimate from.
func packWithObjects(count uint32) io.ReadCloser {
	header := make([]byte, 12)
	copy(header, "PACK")
	binary.BigEndian.PutUint32(header[4:], 2)
	binary.BigEndian.PutUint32(header[8:], count)
	return io.NopCloser(bytes.NewReader(header))
}

// groupHarness records what a fan-out bootstrap fetched and pushed, in order.
type groupHarness struct {
	params   Params
	parents  map[plumbing.Hash][]plumbing.Hash
	trunkTip plumbing.Hash
	trunkRef plumbing.ReferenceName
	forkRefs []plumbing.ReferenceName
	events   []string
	// graphWants holds the wants of each commit-graph fetch.
	graphWants [][]plumbing.Hash
	// packPushes holds the commands of each PushPack call.
	packPushes [][]gitproto.PushCommand
	// objectsFor answers the pack header object count for a pack fetch.
	objectsFor func(desired map[plumbing.ReferenceName]gitproto.DesiredRef) uint32
}

func newGroupHarness(t *testing.T, forks int) *groupHarness {
	t.Helper()
	parents, trunkTip, forkTips := fanOutCommitGraph(t, 4, forks)
	h := &groupHarness{
		parents:    parents,
		trunkTip:   trunkTip,
		trunkRef:   plumbing.NewBranchReferenceName("main"),
		objectsFor: func(map[plumbing.ReferenceName]gitproto.DesiredRef) uint32 { return 1 },
	}
	desired := map[plumbing.ReferenceName]planner.DesiredRef{
		h.trunkRef: {SourceRef: h.trunkRef, TargetRef: h.trunkRef, SourceHash: trunkTip, Kind: planner.RefKindBranch, Label: "main"},
	}
	for i, tip := range forkTips {
		ref := plumbing.NewBranchReferenceName(fmt.Sprintf("feature-%02d", i))
		h.forkRefs = append(h.forkRefs, ref)
		desired[ref] = planner.DesiredRef{SourceRef: ref, TargetRef: ref, SourceHash: tip, Kind: planner.RefKindBranch, Label: ref.Short()}
	}
	h.params = Params{
		SourceService: fakeBootstrapSource{
			fetchCommitParents: func(_ context.Context, _ gitproto.Conn, wants []plumbing.Hash, _ []plumbing.Hash) (map[plumbing.Hash][]plumbing.Hash, error) {
				h.events = append(h.events, fmt.Sprintf("graph:%d", len(wants)))
				h.graphWants = append(h.graphWants, slices.Clone(wants))
				return reachableParents(parents, wants...), nil
			},
			fetchPack: func(_ context.Context, _ gitproto.Conn, desired map[plumbing.ReferenceName]gitproto.DesiredRef, _ map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return packWithObjects(h.objectsFor(desired)), nil
			},
		},
		TargetPusher: fakeBootstrapPusher{
			pushPack: func(_ context.Context, cmds []gitproto.PushCommand, pack io.ReadCloser) error {
				if _, err := io.Copy(io.Discard, pack); err != nil {
					return err
				}
				_ = pack.Close()
				h.events = append(h.events, fmt.Sprintf("push:%d", len(cmds)))
				h.packPushes = append(h.packPushes, slices.Clone(cmds))
				return nil
			},
		},
		DesiredRefs:      desired,
		TargetRefs:       map[plumbing.ReferenceName]plumbing.Hash{},
		SourceHeadTarget: h.trunkRef,
		TargetMaxPack:    1024 * 1024,
		RefOutcome: func(plumbing.ReferenceName) (gitproto.RefOutcome, string) {
			return gitproto.RefOutcomeApplied, ""
		},
	}
	return h
}

// pushedCreate reports whether some PushPack created ref directly, not
// through a temp ref.
func (h *groupHarness) pushedCreate(ref plumbing.ReferenceName) bool {
	for _, cmds := range h.packPushes {
		for _, cmd := range cmds {
			if cmd.Name == ref && cmd.Old.IsZero() && !cmd.New.IsZero() {
				return true
			}
		}
	}
	return false
}

func (h *groupHarness) pushedTempRef(ref plumbing.ReferenceName) bool {
	temp := planner.BootstrapTempRef(ref)
	for _, cmds := range h.packPushes {
		for _, cmd := range cmds {
			if cmd.Name == temp {
				return true
			}
		}
	}
	return false
}

// The trunk lands before any other branch is planned, so a run that times out
// while planning the rest still leaves the trunk on the target (ENT-3617).
func TestExecuteBatchedPushesTrunkBeforePlanningOtherBranches(t *testing.T) {
	t.Parallel()
	h := newGroupHarness(t, 3)

	if _, err := Execute(context.Background(), h.params, "empty target"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Trunk graph, trunk checkpoint push, one graph fetch for all three forks,
	// one group push creating all three.
	want := []string{"graph:1", "push:2", "graph:3", "push:3"}
	if !slices.Equal(h.events, want) {
		t.Fatalf("events = %v, want %v", h.events, want)
	}
}

// Thousands of short-lived branches must not cost a fetch and a push each:
// small branches share one commit-graph fetch and one pack.
func TestExecuteBatchedGroupsSmallBranchesInOnePush(t *testing.T) {
	t.Parallel()
	h := newGroupHarness(t, 5)

	result, err := Execute(context.Background(), h.params, "empty target")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(h.graphWants) != 2 || len(h.graphWants[1]) != 5 {
		t.Fatalf("commit-graph fetches = %v, want the trunk and then all 5 forks together", h.graphWants)
	}
	if len(h.packPushes) != 2 {
		t.Fatalf("PushPack called %d times, want 2 (trunk, then one group)", len(h.packPushes))
	}
	for _, ref := range h.forkRefs {
		if !h.pushedCreate(ref) {
			t.Errorf("%s was not created directly by the group push: %v", ref, h.packPushes)
		}
		if h.pushedTempRef(ref) {
			t.Errorf("%s used a temp ref; a grouped branch needs none", ref)
		}
	}
	if result.Pushed != 6 {
		t.Errorf("Pushed = %d, want 6", result.Pushed)
	}
}

// A group whose pack estimate is over budget splits in half until each part
// fits, and each part still creates its refs directly.
func TestExecuteBatchedSplitsOversizedGroup(t *testing.T) {
	t.Parallel()
	h := newGroupHarness(t, 4)
	h.objectsFor = func(desired map[plumbing.ReferenceName]gitproto.DesiredRef) uint32 {
		if len(desired) > 1 {
			return 1_000_000 // ~715 MiB at 750 B/object, over the 1 MiB budget
		}
		return 1
	}

	if _, err := Execute(context.Background(), h.params, "empty target"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, ref := range h.forkRefs {
		if !h.pushedCreate(ref) {
			t.Errorf("%s was not created after the group split: %v", ref, h.packPushes)
		}
		if h.pushedTempRef(ref) {
			t.Errorf("%s fell back to a temp ref though it fits alone", ref)
		}
	}
	// Trunk plus one push per fork: the 4-group split to 2+2, then to singles.
	if len(h.packPushes) != 5 {
		t.Errorf("PushPack called %d times, want 5", len(h.packPushes))
	}
}

// A branch too big even on its own leaves the group and runs on its own
// checkpoints, through its temp ref, where its chain can be subdivided.
func TestExecuteBatchedOversizedSingleBranchRunsStandalone(t *testing.T) {
	t.Parallel()
	h := newGroupHarness(t, 3)
	big := h.forkRefs[1]
	h.objectsFor = func(desired map[plumbing.ReferenceName]gitproto.DesiredRef) uint32 {
		if _, ok := desired[big]; ok {
			return 1_000_000
		}
		return 1
	}

	if _, err := Execute(context.Background(), h.params, "empty target"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !h.pushedTempRef(big) {
		t.Errorf("%s did not run on its own checkpoints: %v", big, h.packPushes)
	}
	for _, ref := range h.forkRefs {
		if ref == big {
			continue
		}
		if !h.pushedCreate(ref) {
			t.Errorf("%s was not created by a group push: %v", ref, h.packPushes)
		}
	}
}

// A branch with a resume marker keeps its own batch, so the marker is resumed
// from and then cleaned up rather than stranded.
func TestExecuteBatchedBranchWithResumeMarkerRunsStandalone(t *testing.T) {
	t.Parallel()
	h := newGroupHarness(t, 2)
	resumed := h.forkRefs[0]
	resumedTip := h.params.DesiredRefs[resumed].SourceHash
	h.params.TargetRefs[planner.BootstrapTempRef(resumed)] = resumedTip
	var commandPushes [][]gitproto.PushCommand
	pusher, ok := h.params.TargetPusher.(fakeBootstrapPusher)
	if !ok {
		t.Fatalf("harness pusher is %T", h.params.TargetPusher)
	}
	pusher.pushCommands = func(_ context.Context, cmds []gitproto.PushCommand) error {
		commandPushes = append(commandPushes, slices.Clone(cmds))
		return nil
	}
	h.params.TargetPusher = pusher

	if _, err := Execute(context.Background(), h.params, "empty target"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, cmds := range h.packPushes {
		for _, cmd := range cmds {
			if cmd.Name == resumed {
				t.Errorf("%s with a resume marker rode a pack push %v; its marker already holds the tip", resumed, cmds)
			}
		}
	}
	var created, markerDeleted bool
	for _, cmds := range commandPushes {
		for _, cmd := range cmds {
			if cmd.Name == resumed && cmd.New == resumedTip {
				created = true
			}
			if cmd.Name == planner.BootstrapTempRef(resumed) && cmd.Delete {
				markerDeleted = true
			}
		}
	}
	if !created || !markerDeleted {
		t.Errorf("resume cutover for %s: created=%v marker deleted=%v; commands %v", resumed, created, markerDeleted, commandPushes)
	}
	if !h.pushedCreate(h.forkRefs[1]) {
		t.Errorf("%s was not created: %v", h.forkRefs[1], h.packPushes)
	}
}

// deferredForks is what planBatches would leave for after the trunk: every
// fork, planned against the trunk's history.
func (h *groupHarness) deferredForks() deferredBranches {
	stopAt := map[plumbing.Hash]struct{}{}
	for hash := range reachableParents(h.parents, h.trunkTip) {
		stopAt[hash] = struct{}{}
	}
	refs := make([]planner.DesiredRef, len(h.forkRefs))
	for i, ref := range h.forkRefs {
		refs[i] = h.params.DesiredRefs[ref]
	}
	return deferredBranches{refs: refs, haves: []plumbing.Hash{h.trunkTip}, stopAt: stopAt}
}

// Groups are sized with the run's current budget. When the trunk's pushes have
// already lowered it below TargetMaxPack, groups must shrink with it.
func TestPlanAfterTrunkSizesGroupsWithCurrentBudget(t *testing.T) {
	t.Parallel()
	h := newGroupHarness(t, 4)

	countGroups := func(budget int64) int {
		batches, err := planAfterTrunk(context.Background(), h.params, h.deferredForks(), budget)
		if err != nil {
			t.Fatalf("planAfterTrunk: %v", err)
		}
		groups := 0
		for _, batch := range batches {
			if len(batch.group) > 0 {
				groups++
			}
		}
		return groups
	}
	if got := countGroups(h.params.TargetMaxPack); got != 1 {
		t.Errorf("groups at TargetMaxPack = %d, want 1", got)
	}
	// Two one-commit forks per group.
	if got := countGroups(2 * estimatedBytesPerCommit); got != 2 {
		t.Errorf("groups at a lowered budget = %d, want 2", got)
	}
}

// A rejected group push teaches the run the target's limit, as a rejected
// checkpoint push does, so later pushes start under it instead of
// rediscovering it.
func TestPushBranchGroupsLearnsTargetLimit(t *testing.T) {
	t.Parallel()
	h := newGroupHarness(t, 4)
	const announced = 200_000
	pushes := 0
	pusher, ok := h.params.TargetPusher.(fakeBootstrapPusher)
	if !ok {
		t.Fatalf("harness pusher is %T", h.params.TargetPusher)
	}
	pusher.pushPack = func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
		_ = pack.Close()
		pushes++
		if pushes == 1 {
			return fmt.Errorf("target receive-pack: http 413: body exceeded size limit %d", announced)
		}
		return nil
	}
	h.params.TargetPusher = pusher

	batches, err := planAfterTrunk(context.Background(), h.params, h.deferredForks(), h.params.TargetMaxPack)
	if err != nil {
		t.Fatalf("planAfterTrunk: %v", err)
	}
	if len(batches) != 1 || len(batches[0].group) != 4 {
		t.Fatalf("expected one group of 4, got %+v", batches)
	}
	budget := h.params.TargetMaxPack
	fromObservation := false
	bytesPerObject := int64(estimatedBytesPerObject)
	announcedLimit := int64(0)
	standalone, err := pushBranchGroups(context.Background(), h.params, batches[0].group, groupPushState{
		completedRefs:   map[plumbing.ReferenceName]plumbing.Hash{},
		budget:          &budget,
		fromObservation: &fromObservation,
		bytesPerObject:  &bytesPerObject,
		announced:       &announcedLimit,
	}, &Result{})
	if err != nil {
		t.Fatalf("pushBranchGroups: %v", err)
	}
	if len(standalone) != 0 {
		t.Errorf("standalone = %d branches, want 0", len(standalone))
	}
	if announcedLimit != announced {
		t.Errorf("announced limit = %d, want %d", announcedLimit, announced)
	}
	if budget != announced {
		t.Errorf("budget = %d, want the announced %d", budget, announced)
	}
	if pushes < 3 {
		t.Errorf("pushes = %d, want the rejected group and then its parts", pushes)
	}
}

func TestSplitGroup(t *testing.T) {
	t.Parallel()
	members := make([]plannedBatch, 5)
	for _, tc := range []struct{ parts, want int }{{0, 2}, {2, 2}, {3, 3}, {9, 5}} {
		got := splitGroup(members, tc.parts)
		if len(got) != tc.want {
			t.Errorf("splitGroup(5, %d) = %d parts, want %d", tc.parts, len(got), tc.want)
		}
		total := 0
		for _, part := range got {
			if len(part) == 0 {
				t.Errorf("splitGroup(5, %d) produced an empty part", tc.parts)
			}
			total += len(part)
		}
		if total != len(members) {
			t.Errorf("splitGroup(5, %d) covers %d members, want %d", tc.parts, total, len(members))
		}
	}
}
