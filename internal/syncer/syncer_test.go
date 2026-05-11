package syncer

import (
	"context"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	bstrap "entire.io/entire/git-sync/internal/strategy/bootstrap"
)

func TestApplyRejectionsDowngradesAndCarriesReason(t *testing.T) {
	notes := plumbing.ReferenceName("refs/notes/commits")
	main := plumbing.NewBranchReferenceName("main")
	plans := []BranchPlan{
		{TargetRef: main, Action: ActionUpdate, Reason: "abc -> def"},
		{TargetRef: notes, Action: ActionCreate, Reason: "abc -> <new>"},
	}
	s := &syncSession{rejections: map[plumbing.ReferenceName]string{
		notes: "deny updating a hidden ref",
	}}
	warned := s.applyRejections(plans)
	if warned != 1 {
		t.Fatalf("expected warned=1, got %d", warned)
	}
	if plans[0].Action != ActionUpdate {
		t.Errorf("expected non-rejected ref to keep Action=%s, got %s", ActionUpdate, plans[0].Action)
	}
	if plans[1].Action != ActionWarn {
		t.Errorf("expected rejected ref Action=%s, got %s", ActionWarn, plans[1].Action)
	}
	if !strings.Contains(plans[1].Reason, "deny updating a hidden ref") {
		t.Errorf("expected server reason in plans[1].Reason, got %q", plans[1].Reason)
	}
}

func TestApplyRejectionsEmptyMapIsNoOp(t *testing.T) {
	plans := []BranchPlan{{TargetRef: plumbing.NewBranchReferenceName("main"), Action: ActionUpdate}}
	s := &syncSession{}
	if got := s.applyRejections(plans); got != 0 {
		t.Fatalf("expected warned=0 with no rejections, got %d", got)
	}
	if plans[0].Action != ActionUpdate {
		t.Errorf("expected plans untouched, got Action=%s", plans[0].Action)
	}
}

func TestFinalizeCountsTalliesPushedAndDeleted(t *testing.T) {
	notes := plumbing.ReferenceName("refs/notes/commits")
	main := plumbing.NewBranchReferenceName("main")
	stale := plumbing.NewBranchReferenceName("stale")
	pushPlans := []BranchPlan{
		{TargetRef: main, Action: ActionUpdate},
		{TargetRef: stale, Action: ActionDelete},
		{TargetRef: notes, Action: ActionCreate},
	}
	result := Result{Plans: append([]BranchPlan{}, pushPlans...)}
	s := &syncSession{rejections: map[plumbing.ReferenceName]string{
		notes: "deny updating a hidden ref",
	}}
	s.finalizeCounts(pushPlans, &result)

	if result.Pushed != 1 {
		t.Errorf("expected Pushed=1 (main only; notes downgraded), got %d", result.Pushed)
	}
	if result.Deleted != 1 {
		t.Errorf("expected Deleted=1, got %d", result.Deleted)
	}
	if result.Warned != 1 {
		t.Errorf("expected Warned=1, got %d", result.Warned)
	}
	for _, plan := range result.Plans {
		if plan.TargetRef == notes && plan.Action != ActionWarn {
			t.Errorf("expected result.Plans notes ref Action=%s, got %s", ActionWarn, plan.Action)
		}
	}
}

func TestGitHubOwnerRepo(t *testing.T) {
	stats := newStats(false)
	conn, err := newConn(Endpoint{URL: "https://github.com/torvalds/linux.git"}, "source", stats, nil)
	if err != nil {
		t.Fatalf("new conn: %v", err)
	}
	owner, repo, ok := bstrap.GitHubOwnerRepo(conn)
	if !ok {
		t.Fatalf("expected github owner/repo match")
	}
	if owner != "torvalds" || repo != "linux" {
		t.Fatalf("unexpected owner/repo: %s/%s", owner, repo)
	}
}

func TestGitHubOwnerRepoRejectsNonGitHubSource(t *testing.T) {
	stats := newStats(false)
	conn, err := newConn(Endpoint{URL: "https://gitlab.com/group/project.git"}, "source", stats, nil)
	if err != nil {
		t.Fatalf("new conn: %v", err)
	}
	if _, _, ok := bstrap.GitHubOwnerRepo(conn); ok {
		t.Fatalf("expected non-github source to be rejected")
	}
}

// TestPublicAPIRejectsIdenticalSourceAndTarget covers every entry point that
// touches both endpoints: same URL on source and target must fail before any
// network I/O. Probe with no target and Fetch are intentionally excluded
// because they do not have a target.
func TestPublicAPIRejectsIdenticalSourceAndTarget(t *testing.T) {
	t.Parallel()

	const url = "https://example.com/repo.git"
	cfg := Config{
		Source: Endpoint{URL: url},
		Target: Endpoint{URL: url},
	}

	tests := []struct {
		name string
		call func() error
	}{
		{name: "Run", call: func() error {
			_, err := Run(context.Background(), cfg)
			return err
		}},
		{name: "Bootstrap", call: func() error {
			_, err := Bootstrap(context.Background(), cfg)
			return err
		}},
		{name: "Probe with target", call: func() error {
			_, err := Probe(context.Background(), cfg)
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.call()
			if err == nil {
				t.Fatalf("%s with identical source/target URLs returned nil error", tt.name)
			}
			if !strings.Contains(err.Error(), "source and target must not be the same repository") {
				t.Fatalf("%s error = %v, want same-repository rejection", tt.name, err)
			}
		})
	}
}

// TestProbeWithoutTargetIgnoresEndpointEqualityCheck guards against a regression
// where the source-vs-target check would fire for a probe that never set a
// target — there is nothing to compare against.
func TestProbeWithoutTargetIgnoresEndpointEqualityCheck(t *testing.T) {
	t.Parallel()

	cfg := Config{Source: Endpoint{URL: "https://example.com/repo.git"}}
	_, err := Probe(context.Background(), cfg)
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "source and target must not be the same repository") {
		t.Fatalf("Probe without target tripped same-repository check: %v", err)
	}
}

// TestNewConn_PropagatesFollowInfoRefsRedirect proves the plumbing from
// Endpoint → gitproto.Conn is in place. Without this the flag on
// Endpoint is dead config.
func TestNewConn_PropagatesFollowInfoRefsRedirect(t *testing.T) {
	stats := newStats(false)

	off, err := newConn(Endpoint{URL: "https://node.example/repo.git"}, "target", stats, nil)
	if err != nil {
		t.Fatalf("new conn (off): %v", err)
	}
	if off.FollowInfoRefsRedirect {
		t.Error("FollowInfoRefsRedirect should default to false")
	}

	on, err := newConn(Endpoint{URL: "https://node.example/repo.git", FollowInfoRefsRedirect: true}, "target", stats, nil)
	if err != nil {
		t.Fatalf("new conn (on): %v", err)
	}
	if !on.FollowInfoRefsRedirect {
		t.Error("FollowInfoRefsRedirect was not propagated from Endpoint to Conn")
	}
}
