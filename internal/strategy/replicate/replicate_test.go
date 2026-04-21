package replicate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entirehq/git-sync/internal/gitproto"
	"github.com/entirehq/git-sync/internal/planner"
	"github.com/entirehq/git-sync/internal/strategy/pushreconcile"
)

type fakeSourceService struct {
	fetchPack func(context.Context, *gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error)
}

func (f fakeSourceService) FetchPack(
	ctx context.Context,
	conn *gitproto.Conn,
	desired map[plumbing.ReferenceName]gitproto.DesiredRef,
	targetRefs map[plumbing.ReferenceName]plumbing.Hash,
) (io.ReadCloser, error) {
	return f.fetchPack(ctx, conn, desired, targetRefs)
}

type fakeTargetPusher struct {
	pushPack     func(context.Context, []gitproto.PushCommand, io.ReadCloser) error
	pushCommands func(context.Context, []gitproto.PushCommand) error
	listRefs     func(context.Context) (map[plumbing.ReferenceName]plumbing.Hash, error)
}

func (f fakeTargetPusher) PushPack(ctx context.Context, cmds []gitproto.PushCommand, pack io.ReadCloser) error {
	return f.pushPack(ctx, cmds, pack)
}

func (f fakeTargetPusher) PushCommands(ctx context.Context, cmds []gitproto.PushCommand) error {
	return f.pushCommands(ctx, cmds)
}

func (f fakeTargetPusher) ListRefs(ctx context.Context) (map[plumbing.ReferenceName]plumbing.Hash, error) {
	if f.listRefs == nil {
		return nil, errors.New("fakeTargetPusher.ListRefs not configured")
	}
	return f.listRefs(ctx)
}

func TestExecuteReplicateRelaysUpdatesAndDeletesSeparately(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	oldRef := plumbing.NewBranchReferenceName("old")
	oldHash := plumbing.NewHash("1111111111111111111111111111111111111111")
	newHash := plumbing.NewHash("2222222222222222222222222222222222222222")

	var gotDesired map[plumbing.ReferenceName]gitproto.DesiredRef
	var gotHaves map[plumbing.ReferenceName]plumbing.Hash
	var pushed []gitproto.PushCommand
	var deleted []gitproto.PushCommand

	tp := fakeTargetPusher{
		pushPack: func(_ context.Context, cmds []gitproto.PushCommand, pack io.ReadCloser) error {
			defer pack.Close()
			pushed = append([]gitproto.PushCommand(nil), cmds...)
			return nil
		},
		pushCommands: func(_ context.Context, cmds []gitproto.PushCommand) error {
			deleted = append([]gitproto.PushCommand(nil), cmds...)
			return nil
		},
	}
	result, err := Execute(context.Background(), Params{
		SourceService: fakeSourceService{
			fetchPack: func(_ context.Context, _ *gitproto.Conn, desired map[plumbing.ReferenceName]gitproto.DesiredRef, targetRefs map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				gotDesired = desired
				gotHaves = targetRefs
				return io.NopCloser(bytes.NewReader([]byte("PACK"))), nil
			},
		},
		TargetPusher: tp,
		TargetLister: tp,
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			mainRef: {
				SourceRef:  mainRef,
				TargetRef:  mainRef,
				SourceHash: newHash,
				Kind:       planner.RefKindBranch,
			},
		},
		TargetRefs: map[plumbing.ReferenceName]plumbing.Hash{
			mainRef: oldHash,
			oldRef:  oldHash,
		},
		PushPlans: []planner.BranchPlan{
			{
				SourceRef:  mainRef,
				TargetRef:  mainRef,
				SourceHash: newHash,
				TargetHash: oldHash,
				Kind:       planner.RefKindBranch,
				Action:     planner.ActionUpdate,
			},
			{
				TargetRef:  oldRef,
				TargetHash: oldHash,
				Kind:       planner.RefKindBranch,
				Action:     planner.ActionDelete,
			},
		},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.Relay || result.RelayMode != "replicate" || result.RelayReason != "replicate-overwrite-relay" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(gotDesired) != 1 || gotDesired[mainRef].SourceHash != newHash {
		t.Fatalf("unexpected desired refs: %+v", gotDesired)
	}
	if gotHaves[mainRef] != oldHash {
		t.Fatalf("expected target ref haves, got %+v", gotHaves)
	}
	if len(pushed) != 1 || pushed[0].Name != mainRef || pushed[0].New != newHash {
		t.Fatalf("unexpected pack push commands: %+v", pushed)
	}
	if len(deleted) != 1 || !deleted[0].Delete || deleted[0].Name != oldRef {
		t.Fatalf("unexpected delete commands: %+v", deleted)
	}
}

// TestExecuteReconcilesWhenTargetAlreadyMatchesSource covers the race where a
// concurrent writer (e.g. a sibling mirror-worker pod) has already pushed the
// exact same source state to the target. PushPack fails with per-ref
// "remote ref has changed", ListRefs shows the target now matches source, and
// Execute reports success with replicate-reconciled.
func TestExecuteReconcilesWhenTargetAlreadyMatchesSource(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	featureRef := plumbing.NewBranchReferenceName("feature")
	staleHash := plumbing.NewHash("1111111111111111111111111111111111111111")
	newMainHash := plumbing.NewHash("2222222222222222222222222222222222222222")
	newFeatureHash := plumbing.NewHash("3333333333333333333333333333333333333333")

	var listRefsCalls int

	tp := fakeTargetPusher{
		pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
			defer pack.Close()
			return &gitproto.PushReportError{
				Failures: []gitproto.PushRefFailure{
					{Ref: mainRef, Status: "remote ref has changed"},
					{Ref: featureRef, Status: "remote ref has changed"},
				},
			}
		},
		pushCommands: func(context.Context, []gitproto.PushCommand) error { return nil },
		listRefs: func(context.Context) (map[plumbing.ReferenceName]plumbing.Hash, error) {
			listRefsCalls++
			return map[plumbing.ReferenceName]plumbing.Hash{
				mainRef:    newMainHash,
				featureRef: newFeatureHash,
			}, nil
		},
	}
	result, err := Execute(context.Background(), Params{
		SourceService: fakeSourceService{
			fetchPack: func(context.Context, *gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader([]byte("PACK"))), nil
			},
		},
		TargetPusher: tp,
		TargetLister: tp,
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			mainRef:    {SourceRef: mainRef, TargetRef: mainRef, SourceHash: newMainHash, Kind: planner.RefKindBranch},
			featureRef: {SourceRef: featureRef, TargetRef: featureRef, SourceHash: newFeatureHash, Kind: planner.RefKindBranch},
		},
		TargetRefs: map[plumbing.ReferenceName]plumbing.Hash{
			mainRef:    staleHash,
			featureRef: staleHash,
		},
		PushPlans: []planner.BranchPlan{
			{SourceRef: mainRef, TargetRef: mainRef, SourceHash: newMainHash, TargetHash: staleHash, Kind: planner.RefKindBranch, Action: planner.ActionUpdate},
			{SourceRef: featureRef, TargetRef: featureRef, SourceHash: newFeatureHash, TargetHash: staleHash, Kind: planner.RefKindBranch, Action: planner.ActionUpdate},
		},
	})
	if err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}
	if !result.Relay || result.RelayMode != "replicate" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.RelayReason != pushreconcile.Reason {
		t.Errorf("RelayReason: want %q, got %q", pushreconcile.Reason, result.RelayReason)
	}
	if listRefsCalls != 1 {
		t.Errorf("expected ListRefs to be called once, got %d", listRefsCalls)
	}
}

// TestExecuteBubblesUpWhenTargetDoesNotReconcile covers the case where a
// concurrent writer pushed something DIFFERENT to the ref — target state
// diverges from source, reconciliation fails, and the push error bubbles up.
func TestExecuteBubblesUpWhenTargetDoesNotReconcile(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	staleHash := plumbing.NewHash("1111111111111111111111111111111111111111")
	newHash := plumbing.NewHash("2222222222222222222222222222222222222222")
	divergentHash := plumbing.NewHash("4444444444444444444444444444444444444444")

	tp := fakeTargetPusher{
		pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
			defer pack.Close()
			return &gitproto.PushReportError{
				Failures: []gitproto.PushRefFailure{
					{Ref: mainRef, Status: "remote ref has changed"},
				},
			}
		},
		listRefs: func(context.Context) (map[plumbing.ReferenceName]plumbing.Hash, error) {
			return map[plumbing.ReferenceName]plumbing.Hash{mainRef: divergentHash}, nil
		},
	}
	_, err := Execute(context.Background(), Params{
		SourceService: fakeSourceService{
			fetchPack: func(context.Context, *gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader([]byte("PACK"))), nil
			},
		},
		TargetPusher: tp,
		TargetLister: tp,
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			mainRef: {SourceRef: mainRef, TargetRef: mainRef, SourceHash: newHash, Kind: planner.RefKindBranch},
		},
		TargetRefs: map[plumbing.ReferenceName]plumbing.Hash{mainRef: staleHash},
		PushPlans: []planner.BranchPlan{
			{SourceRef: mainRef, TargetRef: mainRef, SourceHash: newHash, TargetHash: staleHash, Kind: planner.RefKindBranch, Action: planner.ActionUpdate},
		},
	})
	if err == nil {
		t.Fatal("Execute() expected to return error for divergent target; got nil")
	}
	var reportErr *gitproto.PushReportError
	if !errors.As(err, &reportErr) {
		t.Fatalf("Execute() error not a PushReportError; got %T: %v", err, err)
	}
}

// TestExecuteBubblesUpOnUnpackFailure covers the case where the push fails
// before any per-ref decisions — the pack was rejected wholesale, so there's
// nothing to reconcile; reconciliation must not be attempted.
func TestExecuteBubblesUpOnUnpackFailure(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	staleHash := plumbing.NewHash("1111111111111111111111111111111111111111")
	newHash := plumbing.NewHash("2222222222222222222222222222222222222222")

	var listRefsCalls int

	tp := fakeTargetPusher{
		pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
			defer pack.Close()
			return &gitproto.PushReportError{UnpackStatus: "unpacker error"}
		},
		listRefs: func(context.Context) (map[plumbing.ReferenceName]plumbing.Hash, error) {
			listRefsCalls++
			return map[plumbing.ReferenceName]plumbing.Hash{}, nil
		},
	}
	_, err := Execute(context.Background(), Params{
		SourceService: fakeSourceService{
			fetchPack: func(context.Context, *gitproto.Conn, map[plumbing.ReferenceName]gitproto.DesiredRef, map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader([]byte("PACK"))), nil
			},
		},
		TargetPusher: tp,
		TargetLister: tp,
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			mainRef: {SourceRef: mainRef, TargetRef: mainRef, SourceHash: newHash, Kind: planner.RefKindBranch},
		},
		TargetRefs: map[plumbing.ReferenceName]plumbing.Hash{mainRef: staleHash},
		PushPlans: []planner.BranchPlan{
			{SourceRef: mainRef, TargetRef: mainRef, SourceHash: newHash, TargetHash: staleHash, Kind: planner.RefKindBranch, Action: planner.ActionUpdate},
		},
	})
	if err == nil {
		t.Fatal("Execute() expected to return error for unpack failure; got nil")
	}
	if listRefsCalls != 0 {
		t.Errorf("ListRefs should not be called for unpack failures; got %d calls", listRefsCalls)
	}
}

// TestExecuteReconcilesDeleteWhenTargetAlreadyMissing covers deletes: if
// another writer already deleted the ref, the target no longer has it, so
// reconciliation succeeds.
func TestExecuteReconcilesDeleteWhenTargetAlreadyMissing(t *testing.T) {
	oldRef := plumbing.NewBranchReferenceName("old")
	oldHash := plumbing.NewHash("1111111111111111111111111111111111111111")

	tp := fakeTargetPusher{
		pushCommands: func(context.Context, []gitproto.PushCommand) error {
			return &gitproto.PushReportError{
				Failures: []gitproto.PushRefFailure{{Ref: oldRef, Status: "remote ref has changed"}},
			}
		},
		listRefs: func(context.Context) (map[plumbing.ReferenceName]plumbing.Hash, error) {
			return map[plumbing.ReferenceName]plumbing.Hash{}, nil
		},
	}
	result, err := Execute(context.Background(), Params{
		TargetPusher: tp,
		TargetLister: tp,
		DesiredRefs:  map[plumbing.ReferenceName]planner.DesiredRef{},
		TargetRefs:   map[plumbing.ReferenceName]plumbing.Hash{oldRef: oldHash},
		PushPlans: []planner.BranchPlan{
			{TargetRef: oldRef, TargetHash: oldHash, Kind: planner.RefKindBranch, Action: planner.ActionDelete},
		},
	})
	if err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}
	if result.RelayReason != pushreconcile.Reason {
		t.Errorf("RelayReason: want %q, got %q", pushreconcile.Reason, result.RelayReason)
	}
}
