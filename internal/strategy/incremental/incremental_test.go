package incremental

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/soph/git-sync/internal/convert"
	"github.com/soph/git-sync/internal/gitproto"
	"github.com/soph/git-sync/internal/planner"
)

func TestPlansToPushPlans(t *testing.T) {
	hash1 := plumbing.NewHash("1111111111111111111111111111111111111111")
	hash2 := plumbing.NewHash("2222222222222222222222222222222222222222")
	hash3 := plumbing.NewHash("3333333333333333333333333333333333333333")

	tests := []struct {
		name   string
		plans  []planner.BranchPlan
		expect []gitproto.PushPlan
	}{
		{
			name: "create plan",
			plans: []planner.BranchPlan{
				{
					TargetRef:  plumbing.NewBranchReferenceName("main"),
					TargetHash: plumbing.ZeroHash,
					SourceHash: hash1,
					Action:     planner.ActionCreate,
				},
			},
			expect: []gitproto.PushPlan{
				{
					TargetRef:  plumbing.NewBranchReferenceName("main"),
					TargetHash: plumbing.ZeroHash,
					SourceHash: hash1,
					Delete:     false,
				},
			},
		},
		{
			name: "update plan",
			plans: []planner.BranchPlan{
				{
					TargetRef:  plumbing.NewBranchReferenceName("main"),
					TargetHash: hash1,
					SourceHash: hash2,
					Action:     planner.ActionUpdate,
				},
			},
			expect: []gitproto.PushPlan{
				{
					TargetRef:  plumbing.NewBranchReferenceName("main"),
					TargetHash: hash1,
					SourceHash: hash2,
					Delete:     false,
				},
			},
		},
		{
			name: "delete plan",
			plans: []planner.BranchPlan{
				{
					TargetRef:  plumbing.NewBranchReferenceName("old"),
					TargetHash: hash3,
					SourceHash: plumbing.ZeroHash,
					Action:     planner.ActionDelete,
				},
			},
			expect: []gitproto.PushPlan{
				{
					TargetRef:  plumbing.NewBranchReferenceName("old"),
					TargetHash: hash3,
					SourceHash: plumbing.ZeroHash,
					Delete:     true,
				},
			},
		},
		{
			name:   "empty input",
			plans:  []planner.BranchPlan{},
			expect: []gitproto.PushPlan{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convert.PlansToPushPlans(tt.plans)
			if len(got) != len(tt.expect) {
				t.Fatalf("expected %d push plans, got %d", len(tt.expect), len(got))
			}
			for i := range tt.expect {
				if got[i].TargetRef != tt.expect[i].TargetRef {
					t.Errorf("[%d] TargetRef = %s, want %s", i, got[i].TargetRef, tt.expect[i].TargetRef)
				}
				if got[i].TargetHash != tt.expect[i].TargetHash {
					t.Errorf("[%d] TargetHash = %s, want %s", i, got[i].TargetHash, tt.expect[i].TargetHash)
				}
				if got[i].SourceHash != tt.expect[i].SourceHash {
					t.Errorf("[%d] SourceHash = %s, want %s", i, got[i].SourceHash, tt.expect[i].SourceHash)
				}
				if got[i].Delete != tt.expect[i].Delete {
					t.Errorf("[%d] Delete = %v, want %v", i, got[i].Delete, tt.expect[i].Delete)
				}
			}
		})
	}
}

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
	pushPack func(context.Context, []gitproto.PushCommand, io.ReadCloser) error
}

func (f fakeTargetPusher) PushPack(ctx context.Context, cmds []gitproto.PushCommand, pack io.ReadCloser) error {
	return f.pushPack(ctx, cmds, pack)
}

func TestExecuteIncrementalRelayUsesTargetRefsAsHaves(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	oldHash := plumbing.NewHash("1111111111111111111111111111111111111111")
	newHash := plumbing.NewHash("2222222222222222222222222222222222222222")

	var gotDesired map[plumbing.ReferenceName]gitproto.DesiredRef
	var gotHaves map[plumbing.ReferenceName]plumbing.Hash
	var pushed []gitproto.PushCommand

	params := Params{
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
		},
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			mainRef: {
				SourceRef:  mainRef,
				TargetRef:  mainRef,
				SourceHash: newHash,
				Kind:       planner.RefKindBranch,
			},
		},
		TargetRefs: map[plumbing.ReferenceName]plumbing.Hash{mainRef: oldHash},
		PushPlans: []planner.BranchPlan{{
			SourceRef:  mainRef,
			TargetRef:  mainRef,
			SourceHash: newHash,
			TargetHash: oldHash,
			Kind:       planner.RefKindBranch,
			Action:     planner.ActionUpdate,
		}},
		CanRelay: func(force, prune, dryRun bool, plans []planner.BranchPlan) (bool, string) {
			if force || prune || dryRun || len(plans) != 1 {
				t.Fatalf("unexpected relay inputs: force=%v prune=%v dryRun=%v plans=%d", force, prune, dryRun, len(plans))
			}
			return true, "fast-forward"
		},
	}
	result, err := Execute(context.Background(), params, planner.PlanConfig{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.Relay || result.RelayMode != "incremental" || result.RelayReason != "fast-forward" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if gotDesired[mainRef].SourceHash != newHash {
		t.Fatalf("desired source hash = %s, want %s", gotDesired[mainRef].SourceHash, newHash)
	}
	if gotHaves[mainRef] != oldHash {
		t.Fatalf("have hash = %s, want %s", gotHaves[mainRef], oldHash)
	}
	if len(pushed) != 1 || pushed[0].Name != mainRef || pushed[0].New != newHash || pushed[0].Old != oldHash {
		t.Fatalf("unexpected pushed commands: %+v", pushed)
	}
}

func TestExecuteFullTagCreateRelayOmitsHaves(t *testing.T) {
	tagRef := plumbing.NewTagReferenceName("v1.0.0")
	tagHash := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	var gotHaves map[plumbing.ReferenceName]plumbing.Hash

	params := Params{
		SourceService: fakeSourceService{
			fetchPack: func(_ context.Context, _ *gitproto.Conn, _ map[plumbing.ReferenceName]gitproto.DesiredRef, targetRefs map[plumbing.ReferenceName]plumbing.Hash) (io.ReadCloser, error) {
				gotHaves = targetRefs
				return io.NopCloser(bytes.NewReader([]byte("PACK"))), nil
			},
		},
		TargetPusher: fakeTargetPusher{
			pushPack: func(_ context.Context, _ []gitproto.PushCommand, pack io.ReadCloser) error {
				return pack.Close()
			},
		},
		DesiredRefs: map[plumbing.ReferenceName]planner.DesiredRef{
			tagRef: {
				SourceRef:  tagRef,
				TargetRef:  tagRef,
				SourceHash: tagHash,
				Kind:       planner.RefKindTag,
			},
		},
		PushPlans: []planner.BranchPlan{{
			SourceRef:  tagRef,
			TargetRef:  tagRef,
			SourceHash: tagHash,
			Kind:       planner.RefKindTag,
			Action:     planner.ActionCreate,
		}},
		CanRelay: func(bool, bool, bool, []planner.BranchPlan) (bool, string) {
			return false, ""
		},
	}
	result, err := Execute(context.Background(), params, planner.PlanConfig{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.Relay || result.RelayReason == "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if gotHaves != nil {
		t.Fatalf("expected nil haves for full tag create relay, got %v", gotHaves)
	}
}

func TestToGP(t *testing.T) {
	hash1 := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	hash2 := plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	tests := []struct {
		name      string
		desired   map[plumbing.ReferenceName]planner.DesiredRef
		wantIsTag map[plumbing.ReferenceName]bool
	}{
		{
			name: "branch ref sets IsTag false",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				plumbing.NewBranchReferenceName("main"): {
					Kind:       planner.RefKindBranch,
					SourceRef:  plumbing.NewBranchReferenceName("main"),
					TargetRef:  plumbing.NewBranchReferenceName("main"),
					SourceHash: hash1,
				},
			},
			wantIsTag: map[plumbing.ReferenceName]bool{
				plumbing.NewBranchReferenceName("main"): false,
			},
		},
		{
			name: "tag ref sets IsTag true",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				plumbing.NewTagReferenceName("v1.0"): {
					Kind:       planner.RefKindTag,
					SourceRef:  plumbing.NewTagReferenceName("v1.0"),
					TargetRef:  plumbing.NewTagReferenceName("v1.0"),
					SourceHash: hash2,
				},
			},
			wantIsTag: map[plumbing.ReferenceName]bool{
				plumbing.NewTagReferenceName("v1.0"): true,
			},
		},
		{
			name: "mixed branch and tag",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				plumbing.NewBranchReferenceName("dev"): {
					Kind:       planner.RefKindBranch,
					SourceRef:  plumbing.NewBranchReferenceName("dev"),
					TargetRef:  plumbing.NewBranchReferenceName("dev"),
					SourceHash: hash1,
				},
				plumbing.NewTagReferenceName("v2.0"): {
					Kind:       planner.RefKindTag,
					SourceRef:  plumbing.NewTagReferenceName("v2.0"),
					TargetRef:  plumbing.NewTagReferenceName("v2.0"),
					SourceHash: hash2,
				},
			},
			wantIsTag: map[plumbing.ReferenceName]bool{
				plumbing.NewBranchReferenceName("dev"): false,
				plumbing.NewTagReferenceName("v2.0"):   true,
			},
		},
		{
			name:      "empty input",
			desired:   map[plumbing.ReferenceName]planner.DesiredRef{},
			wantIsTag: map[plumbing.ReferenceName]bool{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convert.DesiredRefs(tt.desired)
			if len(got) != len(tt.desired) {
				t.Fatalf("expected %d results, got %d", len(tt.desired), len(got))
			}
			for ref, wantTag := range tt.wantIsTag {
				gp, ok := got[ref]
				if !ok {
					t.Errorf("missing ref %s in output", ref)
					continue
				}
				if gp.IsTag != wantTag {
					t.Errorf("ref %s: IsTag = %v, want %v", ref, gp.IsTag, wantTag)
				}
				src := tt.desired[ref]
				if gp.SourceRef != src.SourceRef {
					t.Errorf("ref %s: SourceRef = %s, want %s", ref, gp.SourceRef, src.SourceRef)
				}
				if gp.TargetRef != src.TargetRef {
					t.Errorf("ref %s: TargetRef = %s, want %s", ref, gp.TargetRef, src.TargetRef)
				}
				if gp.SourceHash != src.SourceHash {
					t.Errorf("ref %s: SourceHash = %s, want %s", ref, gp.SourceHash, src.SourceHash)
				}
			}
		})
	}
}
