package pushreconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entirehq/git-sync/internal/gitproto"
	"github.com/entirehq/git-sync/internal/planner"
)

type stubLister struct {
	refs map[plumbing.ReferenceName]plumbing.Hash
	err  error
	n    int
}

func (s *stubLister) ListRefs(_ context.Context) (map[plumbing.ReferenceName]plumbing.Hash, error) {
	s.n++
	return s.refs, s.err
}

func TestCheckReturnsFalseForNonReportError(t *testing.T) {
	lister := &stubLister{}
	if Check(context.Background(), errors.New("network"), nil, lister) {
		t.Fatal("expected Check to return false for plain error")
	}
	if lister.n != 0 {
		t.Errorf("expected ListRefs not to be called, got %d", lister.n)
	}
}

func TestCheckReturnsFalseForUnpackFailure(t *testing.T) {
	lister := &stubLister{}
	err := &gitproto.PushReportError{UnpackStatus: "unpacker error"}
	if Check(context.Background(), err, nil, lister) {
		t.Fatal("expected Check to return false for unpack failure")
	}
	if lister.n != 0 {
		t.Errorf("expected ListRefs not to be called, got %d", lister.n)
	}
}

func TestCheckReturnsTrueWhenTargetMatchesSourceForUpdate(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	source := plumbing.NewHash("2222222222222222222222222222222222222222")
	lister := &stubLister{refs: map[plumbing.ReferenceName]plumbing.Hash{mainRef: source}}

	plans := []planner.BranchPlan{
		{TargetRef: mainRef, SourceHash: source, Action: planner.ActionUpdate},
	}
	pushErr := &gitproto.PushReportError{
		Failures: []gitproto.PushRefFailure{{Ref: mainRef, Status: "remote ref has changed"}},
	}

	if !Check(context.Background(), pushErr, plans, lister) {
		t.Fatal("expected Check to return true when target already matches source")
	}
}

func TestCheckReturnsFalseWhenTargetDivergesFromSource(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	source := plumbing.NewHash("2222222222222222222222222222222222222222")
	divergent := plumbing.NewHash("4444444444444444444444444444444444444444")
	lister := &stubLister{refs: map[plumbing.ReferenceName]plumbing.Hash{mainRef: divergent}}

	plans := []planner.BranchPlan{
		{TargetRef: mainRef, SourceHash: source, Action: planner.ActionUpdate},
	}
	pushErr := &gitproto.PushReportError{
		Failures: []gitproto.PushRefFailure{{Ref: mainRef, Status: "remote ref has changed"}},
	}

	if Check(context.Background(), pushErr, plans, lister) {
		t.Fatal("expected Check to return false when target has diverged")
	}
}

func TestCheckReturnsTrueWhenDeleteTargetAlreadyMissing(t *testing.T) {
	oldRef := plumbing.NewBranchReferenceName("old")
	lister := &stubLister{refs: map[plumbing.ReferenceName]plumbing.Hash{}}

	plans := []planner.BranchPlan{
		{TargetRef: oldRef, Action: planner.ActionDelete},
	}
	pushErr := &gitproto.PushReportError{
		Failures: []gitproto.PushRefFailure{{Ref: oldRef, Status: "remote ref has changed"}},
	}

	if !Check(context.Background(), pushErr, plans, lister) {
		t.Fatal("expected Check to return true when delete target is already absent")
	}
}

func TestCheckReturnsFalseWhenDeleteTargetStillPresent(t *testing.T) {
	oldRef := plumbing.NewBranchReferenceName("old")
	stillThere := plumbing.NewHash("1111111111111111111111111111111111111111")
	lister := &stubLister{refs: map[plumbing.ReferenceName]plumbing.Hash{oldRef: stillThere}}

	plans := []planner.BranchPlan{
		{TargetRef: oldRef, Action: planner.ActionDelete},
	}
	pushErr := &gitproto.PushReportError{
		Failures: []gitproto.PushRefFailure{{Ref: oldRef, Status: "remote ref has changed"}},
	}

	if Check(context.Background(), pushErr, plans, lister) {
		t.Fatal("expected Check to return false when delete target is still present")
	}
}

func TestCheckReturnsFalseWhenListerErrors(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	source := plumbing.NewHash("2222222222222222222222222222222222222222")
	lister := &stubLister{err: errors.New("connection reset")}

	plans := []planner.BranchPlan{
		{TargetRef: mainRef, SourceHash: source, Action: planner.ActionUpdate},
	}
	pushErr := &gitproto.PushReportError{
		Failures: []gitproto.PushRefFailure{{Ref: mainRef, Status: "remote ref has changed"}},
	}

	if Check(context.Background(), pushErr, plans, lister) {
		t.Fatal("expected Check to return false when lister fails")
	}
}

func TestCheckReturnsFalseForUnrecognizedStatusEvenWhenTargetMatches(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	source := plumbing.NewHash("2222222222222222222222222222222222222222")
	lister := &stubLister{refs: map[plumbing.ReferenceName]plumbing.Hash{mainRef: source}}

	plans := []planner.BranchPlan{
		{TargetRef: mainRef, SourceHash: source, Action: planner.ActionUpdate},
	}
	pushErr := &gitproto.PushReportError{
		Failures: []gitproto.PushRefFailure{{Ref: mainRef, Status: "pre-receive hook declined"}},
	}

	if Check(context.Background(), pushErr, plans, lister) {
		t.Fatal("expected Check to return false for non-CAS status even when target matches; hook/policy rejections must not be swallowed")
	}
}

func TestCheckReturnsFalseWhenFailureRefNotInPlans(t *testing.T) {
	mainRef := plumbing.NewBranchReferenceName("main")
	unknown := plumbing.NewBranchReferenceName("unknown")
	source := plumbing.NewHash("2222222222222222222222222222222222222222")
	lister := &stubLister{refs: map[plumbing.ReferenceName]plumbing.Hash{unknown: source}}

	plans := []planner.BranchPlan{
		{TargetRef: mainRef, SourceHash: source, Action: planner.ActionUpdate},
	}
	pushErr := &gitproto.PushReportError{
		Failures: []gitproto.PushRefFailure{{Ref: unknown, Status: "already exists"}},
	}

	if Check(context.Background(), pushErr, plans, lister) {
		t.Fatal("expected Check to return false when failed ref is not in plans")
	}
}
