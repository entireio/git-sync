package syncer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	"entire.io/entire/git-sync/internal/gitproto"
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

func TestLeaseFailureErrorEscalatesPastBestEffort(t *testing.T) {
	main := plumbing.NewBranchReferenceName("main")
	pull := plumbing.ReferenceName("refs/pull/1/head")
	leased := Config{ForceWithLease: true}

	// Non-lease rejection alone: BestEffort handles it as a warning, no fatal.
	s := &syncSession{cfg: leased, rejections: map[plumbing.ReferenceName]string{
		pull: "deny updating a hidden ref",
	}}
	if err := s.leaseFailureError(); err != nil {
		t.Fatalf("expected nil for non-lease rejection, got %v", err)
	}

	// Lease rejection under --force-with-lease: must escalate, naming the
	// affected ref and the override flag.
	s.rejections[main] = "stale info, exp 1234, got abcd"
	err := s.leaseFailureError()
	if err == nil {
		t.Fatal("expected lease failure to escalate past BestEffort")
	}
	if !strings.Contains(err.Error(), main.String()) {
		t.Errorf("expected affected ref in error, got %q", err)
	}
	if !strings.Contains(err.Error(), "--force-blind") {
		t.Errorf("expected migration hint in error, got %q", err)
	}
}

func TestLeaseFailureErrorOnlyAppliesUnderForceWithLease(t *testing.T) {
	main := plumbing.NewBranchReferenceName("main")
	// The broad markers (non-fast-forward / fetch first) can mean ordinary
	// server policy rejection rather than a lease miss. Those rejections must
	// stay warnable under BestEffort when --force-with-lease isn't set.
	rejections := map[plumbing.ReferenceName]string{
		main: "non-fast-forward",
	}
	for _, cfg := range []Config{{}, {ForceBlind: true}} {
		s := &syncSession{cfg: cfg, rejections: rejections}
		if err := s.leaseFailureError(); err != nil {
			t.Errorf("cfg=%+v: expected nil (no lease contract), got %v", cfg, err)
		}
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
func TestNewHTTPConn_PropagatesFollowInfoRefsRedirect(t *testing.T) {
	stats := newStats(false)

	off, err := newConn(Endpoint{URL: "https://node.example/repo.git"}, "target", stats, nil)
	if err != nil {
		t.Fatalf("new conn (off): %v", err)
	}
	offHTTP, ok := off.(*gitproto.HTTPConn)
	if !ok {
		t.Fatalf("expected *gitproto.HTTPConn, got %T", off)
	}
	if offHTTP.FollowInfoRefsRedirect {
		t.Error("FollowInfoRefsRedirect should default to false")
	}

	on, err := newConn(Endpoint{URL: "https://node.example/repo.git", FollowInfoRefsRedirect: true}, "target", stats, nil)
	if err != nil {
		t.Fatalf("new conn (on): %v", err)
	}
	onHTTP, ok := on.(*gitproto.HTTPConn)
	if !ok {
		t.Fatalf("expected *gitproto.HTTPConn, got %T", on)
	}
	if !onHTTP.FollowInfoRefsRedirect {
		t.Error("FollowInfoRefsRedirect was not propagated from Endpoint to Conn")
	}
}

func TestNewConnBuildsSSHTransport(t *testing.T) {
	orig := gitproto.SSHLookPath
	t.Cleanup(func() { gitproto.SSHLookPath = orig })
	script := filepath.Join(t.TempDir(), "ssh-stub.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write ssh stub: %v", err)
	}
	gitproto.SSHLookPath = func(string) (string, error) {
		return script, nil
	}

	stats := newStats(false)
	tests := []string{
		"ssh://example.com/repo.git",
		"git+ssh://example.com/repo.git",
		"git@example.com:repo.git",
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			conn, err := newConn(Endpoint{URL: raw}, "source", stats, nil)
			if err != nil {
				t.Fatalf("new conn: %v", err)
			}
			if _, ok := conn.(*gitproto.SSHConn); !ok {
				t.Fatalf("expected *gitproto.SSHConn, got %T", conn)
			}
		})
	}
}

func TestSSHStatsWarning(t *testing.T) {
	tests := []struct {
		name   string
		cfg    Config
		source gitproto.Conn
		target gitproto.Conn
		want   bool
	}{
		{
			name:   "no flags",
			cfg:    Config{},
			source: &gitproto.SSHConn{},
		},
		{
			name:   "http only",
			cfg:    Config{Progress: true},
			source: &gitproto.HTTPConn{},
			target: &gitproto.HTTPConn{},
		},
		{
			name:   "progress with ssh source",
			cfg:    Config{Progress: true},
			source: &gitproto.SSHConn{},
			want:   true,
		},
		{
			name:   "show stats with ssh target",
			cfg:    Config{ShowStats: true},
			source: &gitproto.HTTPConn{},
			target: &gitproto.SSHConn{},
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sshStatsWarning(tt.cfg, tt.source, tt.target)
			if (got != "") != tt.want {
				t.Fatalf("sshStatsWarning() = %q, want warning=%t", got, tt.want)
			}
		})
	}
}
