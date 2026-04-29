package replicate

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	"entire.io/git-sync/internal/gitproto"
	"entire.io/git-sync/internal/planner"
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
}

func (f fakeTargetPusher) PushPack(ctx context.Context, cmds []gitproto.PushCommand, pack io.ReadCloser) error {
	return f.pushPack(ctx, cmds, pack)
}

func (f fakeTargetPusher) PushCommands(ctx context.Context, cmds []gitproto.PushCommand) error {
	return f.pushCommands(ctx, cmds)
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

	result, err := Execute(context.Background(), Params{
		SourceService: fakeSourceService{
			fetchPack: func(_ context.Context, _ *gitproto.Conn, desired map[plumbing.ReferenceName]gitproto.DesiredRef, targetRefs map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				gotDesired = desired
				gotHaves = targetRefs
				return io.NopCloser(bytes.NewReader([]byte("PACK"))), nil
			},
		},
		TargetPusher: fakeTargetPusher{
			pushPack: func(_ context.Context, cmds []gitproto.PushCommand, pack io.ReadCloser) error {
				defer pack.Close()
				pushed = append([]gitproto.PushCommand(nil), cmds...)
				return nil
			},
			pushCommands: func(_ context.Context, cmds []gitproto.PushCommand) error {
				deleted = append([]gitproto.PushCommand(nil), cmds...)
				return nil
			},
		},
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
