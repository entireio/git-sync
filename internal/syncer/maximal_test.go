package syncer

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"entire.io/entire/git-sync/internal/auth"
	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// maximalFixture is a source whose branch master has two older branches
// behind it, already mirrored to a target. Every target tip but master's is
// an ancestor of master's.
type maximalFixture struct {
	source       *git.Repository
	sourceFS     billy.Filesystem
	target       *git.Repository
	targetServer *smartHTTPRepoServer
	sourceURL    string
	haves        *haveRecorder
	older        []plumbing.ReferenceName
	assume       bool
	// configure, when set, adjusts every run's Config, the seed run's too.
	configure func(*Config)
}

func newMaximalFixture(t *testing.T, advertise bool, setup ...func(*maximalFixture)) *maximalFixture {
	t.Helper()
	sourceRepo, sourceFS := newSourceRepo(t)
	targetRepo, _ := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 3)
	f := &maximalFixture{source: sourceRepo, sourceFS: sourceFS, target: targetRepo, haves: &haveRecorder{}}

	commit, err := sourceRepo.CommitObject(f.tip(t, plumbing.NewBranchReferenceName(testBranch)))
	if err != nil {
		t.Fatalf("read master: %v", err)
	}
	for _, name := range []string{"older-a", "older-b"} {
		if commit, err = commit.Parent(0); err != nil {
			t.Fatalf("walk master: %v", err)
		}
		ref := plumbing.NewBranchReferenceName(name)
		if err := sourceRepo.Storer.SetReference(plumbing.NewHashReference(ref, commit.Hash)); err != nil {
			t.Fatalf("set %s: %v", ref, err)
		}
		f.older = append(f.older, ref)
	}

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	f.targetServer = newSmartHTTPRepoServerV2(t, targetRepo)
	f.targetServer.receivePackThinCap = true
	f.targetServer.lsRefsMaximal = advertise
	t.Cleanup(sourceServer.Close)
	t.Cleanup(f.targetServer.Close)
	f.haves.attach(sourceServer)
	f.sourceURL = sourceServer.RepoURL()
	for _, fn := range setup {
		fn(f)
	}

	if _, err := f.run(t, modeReplicate, ""); err != nil {
		t.Fatalf("seed replicate: %v", err)
	}
	assertSameRefs(t, sourceRepo, targetRepo)
	f.haves.take()
	return f
}

func (f *maximalFixture) run(t *testing.T, mode, protocol string) (Result, error) {
	t.Helper()
	cfg := Config{
		Source:       Endpoint{URL: f.sourceURL},
		Target:       Endpoint{URL: f.targetServer.RepoURL()},
		Mode:         mode,
		ProtocolMode: protocol,
		Branches:     []string{testBranch, "older-a", "older-b"},

		AssumeTargetMaximal: f.assume,
	}
	if f.configure != nil {
		f.configure(&cfg)
	}
	return Run(context.Background(), cfg)
}

func (f *maximalFixture) targetTip(t *testing.T, ref plumbing.ReferenceName) plumbing.Hash {
	t.Helper()
	r, err := f.target.Reference(ref, true)
	if err != nil {
		t.Fatalf("read target %s: %v", ref, err)
	}
	return r.Hash()
}

func (f *maximalFixture) assertMasterMirrored(t *testing.T) {
	t.Helper()
	master := plumbing.NewBranchReferenceName(testBranch)
	want := f.tip(t, master)
	if got := f.targetTip(t, master); got != want {
		t.Fatalf("target master = %s, want %s", got, want)
	}
	assertClosureReadable(t, f.target, want)
}

func (f *maximalFixture) tip(t *testing.T, ref plumbing.ReferenceName) plumbing.Hash {
	t.Helper()
	r, err := f.source.Reference(ref, true)
	if err != nil {
		t.Fatalf("read %s: %v", ref, err)
	}
	return r.Hash()
}

// advance moves master on the source and returns its old tip.
func (f *maximalFixture) advance(t *testing.T) plumbing.Hash {
	t.Helper()
	before := f.tip(t, plumbing.NewBranchReferenceName(testBranch))
	makeCommits(t, f.source, f.sourceFS, 1)
	return before
}

func (f *maximalFixture) olderTips(t *testing.T) []plumbing.Hash {
	t.Helper()
	out := make([]plumbing.Hash, 0, len(f.older))
	for _, ref := range f.older {
		out = append(out, f.tip(t, ref))
	}
	return out
}

func assertEveryFetchHaves(t *testing.T, fetches [][]plumbing.Hash, want ...plumbing.Hash) {
	t.Helper()
	if len(fetches) == 0 {
		t.Fatal("no fetch reached the source")
	}
	for _, got := range fetches {
		assertFetchHaves(t, got, want...)
	}
}

// The case the feature exists for: the target marks only master, so the fetch
// offers master's old tip alone. The older branches are its ancestors.
func TestRun_IntegrationMaximalTipsShrinkHaves(t *testing.T) {
	for _, mode := range []string{modeSync, modeReplicate} {
		t.Run(mode, func(t *testing.T) {
			f := newMaximalFixture(t, true)
			before := f.advance(t)
			if _, err := f.run(t, mode, ""); err != nil {
				t.Fatalf("%s: %v", mode, err)
			}
			assertEveryFetchHaves(t, f.haves.take(), before)
			assertSameRefs(t, f.source, f.target)
			if args := f.targetServer.LastLSRefsArgs(); !slices.Contains(args, gitproto.MaximalFeature) {
				t.Errorf("target ls-refs args = %v, want %s", args, gitproto.MaximalFeature)
			}
		})
	}
}

// A target that does not advertise the feature never sees the argument, and
// every target tip stays a have.
func TestRun_IntegrationMaximalTipsUnadvertisedKeepsEveryTip(t *testing.T) {
	f := newMaximalFixture(t, false)
	before := f.advance(t)
	if _, err := f.run(t, modeReplicate, ""); err != nil {
		t.Fatalf("replicate: %v", err)
	}
	assertEveryFetchHaves(t, f.haves.take(), append(f.olderTips(t), before)...)
	assertSameRefs(t, f.source, f.target)
	if args := f.targetServer.LastLSRefsArgs(); slices.Contains(args, gitproto.MaximalFeature) {
		t.Errorf("target ls-refs args = %v; sent an argument the target did not advertise", args)
	}
}

// Pinning protocol v1 keeps the target listing off v2 entirely.
func TestRun_IntegrationMaximalTipsSkippedUnderV1(t *testing.T) {
	f := newMaximalFixture(t, true)
	f.targetServer.mu.Lock()
	f.targetServer.lsRefsArgs = nil
	f.targetServer.mu.Unlock()
	f.advance(t)
	if _, err := f.run(t, modeReplicate, protocolModeV1); err != nil {
		t.Fatalf("replicate: %v", err)
	}
	assertSameRefs(t, f.source, f.target)
	if args := f.targetServer.LastLSRefsArgs(); args != nil {
		t.Errorf("target ls-refs args = %v, want no ls-refs under protocol v1", args)
	}
}

// mirror-pipeline's case: entiredb accepts the argument before it advertises
// it, so the caller opts in and the haves shrink anyway.
func TestRun_IntegrationMaximalTipsAssumedWithoutAdvertisement(t *testing.T) {
	f := newMaximalFixture(t, false)
	f.assume = true
	before := f.advance(t)
	if _, err := f.run(t, modeReplicate, ""); err != nil {
		t.Fatalf("replicate: %v", err)
	}
	assertEveryFetchHaves(t, f.haves.take(), before)
	assertSameRefs(t, f.source, f.target)
	if args := f.targetServer.LastLSRefsArgs(); !slices.Contains(args, gitproto.MaximalFeature) {
		t.Errorf("target ls-refs args = %v, want %s", args, gitproto.MaximalFeature)
	}
}

// A target that rejects the assumed argument fails only the listing: the run
// offers every tip and completes.
func TestRun_IntegrationMaximalTipsAssumedButRejected(t *testing.T) {
	f := newMaximalFixture(t, false)
	f.assume = true
	f.targetServer.lsRefsRejectMaximal = true
	before := f.advance(t)
	if _, err := f.run(t, modeReplicate, ""); err != nil {
		t.Fatalf("replicate: %v", err)
	}
	assertEveryFetchHaves(t, f.haves.take(), append(f.olderTips(t), before)...)
	assertSameRefs(t, f.source, f.target)
}

// A run that fetches nothing never pays for the listing: a converged run, in
// either mode, and a replicate that only deletes.
func TestRun_IntegrationMaximalTipsNotListedWithoutFetch(t *testing.T) {
	f := newMaximalFixture(t, true)
	for _, mode := range []string{modeSync, modeReplicate} {
		if _, err := f.run(t, mode, ""); err != nil {
			t.Fatalf("converged %s: %v", mode, err)
		}
	}
	if err := f.source.Storer.RemoveReference(f.older[1]); err != nil {
		t.Fatalf("delete %s: %v", f.older[1], err)
	}
	f.configure = func(c *Config) { c.Branches, c.AllRefs, c.Prune = nil, true, true }
	if _, err := f.run(t, modeReplicate, ""); err != nil {
		t.Fatalf("delete-only replicate: %v", err)
	}
	if _, err := f.target.Reference(f.older[1], false); err == nil {
		t.Fatalf("target still has %s after a pruning replicate", f.older[1])
	}
	if n := f.targetServer.MaximalRequests(); n != 0 {
		t.Fatalf("maximal listings = %d, want none when no run fetches", n)
	}
	if fetches := f.haves.take(); len(fetches) != 0 {
		t.Fatalf("source fetches = %v, want none", fetches)
	}
}

// The listing asks only for the mirrored namespace, so a target-only ref
// outside it is unlisted and stays a have.
func TestRun_IntegrationMaximalTipsListOnlyMirroredNamespaces(t *testing.T) {
	f := newMaximalFixture(t, true)
	pull := plumbing.ReferenceName("refs/pull/1/head")
	olderA := f.tip(t, f.older[0])
	if err := f.target.Storer.SetReference(plumbing.NewHashReference(pull, olderA)); err != nil {
		t.Fatalf("set %s: %v", pull, err)
	}
	before := f.advance(t)
	if _, err := f.run(t, modeSync, ""); err != nil {
		t.Fatalf("sync: %v", err)
	}
	assertEveryFetchHaves(t, f.haves.take(), before, olderA)
	f.assertMasterMirrored(t)
	args := f.targetServer.LastLSRefsArgs()
	if !slices.Contains(args, gitproto.MaximalFeature) {
		t.Fatalf("target ls-refs args = %v, want %s", args, gitproto.MaximalFeature)
	}
	for _, arg := range args {
		if prefix, ok := strings.CutPrefix(arg, "ref-prefix "); ok && prefix != "refs/heads/" {
			t.Errorf("listing asked for %q, outside the mirrored refs/heads/", prefix)
		}
	}
}

// A marked target tip the source lacks, here a commit only the target has on
// top of master, leaves master unmarked. The source ignores the unknown tip,
// so master's old tip must still go out as a have, or the fetch would resend
// master's whole history. The older branches stay dropped: master covers them.
func TestRun_IntegrationMaximalTipsKeepDefaultBranchUnderUnknownTip(t *testing.T) {
	for _, mode := range []string{modeSync, modeReplicate} {
		t.Run(mode, func(t *testing.T) {
			f := newMaximalFixture(t, true)
			targetOnly := plumbing.NewBranchReferenceName("target-only")
			commitCheckpoint(t, f.target, targetOnly, f.tip(t, plumbing.NewBranchReferenceName(testBranch)))
			before := f.advance(t)
			if _, err := f.run(t, mode, ""); err != nil {
				t.Fatalf("%s: %v", mode, err)
			}
			assertEveryFetchHaves(t, f.haves.take(), f.targetTip(t, targetOnly), before)
			f.assertMasterMirrored(t)
		})
	}
}

func TestKeepHaveDefaultBranch(t *testing.T) {
	head := plumbing.NewBranchReferenceName("main")
	mapped := plumbing.NewBranchReferenceName("upstream-main")
	other := plumbing.NewBranchReferenceName("feature")
	checkpoint := plumbing.ReferenceName("refs/entire/checkpoints/a")
	desired := map[plumbing.ReferenceName]planner.DesiredRef{
		mapped: {SourceRef: head, TargetRef: mapped},
		other:  {SourceRef: other, TargetRef: other},
	}
	s := &syncSession{
		cfg:           Config{IndependentHistoryRefPrefixes: []string{"refs/entire/checkpoints/"}},
		sourceService: &gitproto.RefService{HeadTarget: head},
	}
	keep := s.keepHave(desired)
	for name, want := range map[plumbing.ReferenceName]bool{head: true, mapped: true, other: false, checkpoint: true} {
		if got := keep(name); got != want {
			t.Errorf("keep(%s) = %v, want %v", name, got, want)
		}
	}

	s.sourceService.HeadTarget = ""
	if keep := s.keepHave(desired); keep(mapped) || keep(head) {
		t.Error("without a source HEAD, no ref is kept as the default branch")
	}
}

// An updated independent-history ref whose old value another ref in its
// namespace reaches is unmarked, and its old value is still the have that
// shortens the update. The reduction leaves the namespace to FetchHaves,
// which keeps that old value and drops the unchanged tip.
func TestRun_IntegrationMaximalTipsKeepIndependentHistoryRefs(t *testing.T) {
	// Validation and the planner accept a padded prefix, so this must too.
	for name, prefix := range map[string]string{"exact": testCheckpointPrefix, "padded": " " + testCheckpointPrefix + " "} {
		t.Run(name, func(t *testing.T) { testMaximalTipsKeepIndependentHistoryRefs(t, prefix) })
	}
}

func testMaximalTipsKeepIndependentHistoryRefs(t *testing.T, prefix string) {
	cpA := plumbing.ReferenceName(testCheckpointPrefix + "a")
	cpB := plumbing.ReferenceName(testCheckpointPrefix + "b")
	f := newMaximalFixture(t, true, func(f *maximalFixture) {
		commitCheckpoint(t, f.source, cpA, plumbing.ZeroHash)
		commitCheckpoint(t, f.source, cpB, f.tip(t, cpA))
		f.configure = func(c *Config) {
			c.Branches = nil
			c.AllRefs = true
			c.IndependentHistoryRefPrefixes = []string{prefix}
		}
	})
	oldA := f.tip(t, cpA)
	commitCheckpoint(t, f.source, cpA, oldA)
	if _, err := f.run(t, modeReplicate, ""); err != nil {
		t.Fatalf("replicate: %v", err)
	}
	master := f.tip(t, plumbing.NewBranchReferenceName(testBranch))
	assertEveryFetchHaves(t, f.haves.take(), master, oldA)
	assertSameRefs(t, f.source, f.target)
	if n := f.targetServer.MaximalRequests(); n != 1 {
		t.Fatalf("maximal listings = %d, want 1", n)
	}
}

// With FollowInfoRefsRedirect, upload-pack discovery may land on a read host
// that refuses pushes. The listing runs on its own connection, so the push
// stays on the host the run was given, whether or not the target supports
// the feature.
func TestRun_IntegrationMaximalProbeLeavesPushOnTarget(t *testing.T) {
	for _, advertise := range []bool{true, false} {
		t.Run(map[bool]string{true: "advertised", false: "unadvertised"}[advertise], func(t *testing.T) {
			f := newMaximalFixture(t, advertise)
			reader := newSmartHTTPRepoServerV2(t, f.target)
			reader.lsRefsMaximal = advertise
			t.Cleanup(reader.Close)
			var readerPushes atomic.Int32
			reader.receivePackRaw = func(w http.ResponseWriter, _ *http.Request) bool {
				readerPushes.Add(1)
				http.Error(w, "read-only replica", http.StatusMethodNotAllowed)
				return true
			}
			f.targetServer.uploadPackRedirect = reader.server.URL
			f.configure = func(c *Config) { c.Target.FollowInfoRefsRedirect = true }

			before := f.advance(t)
			if _, err := f.run(t, modeReplicate, ""); err != nil {
				t.Fatalf("replicate: %v", err)
			}
			if n := readerPushes.Load(); n != 0 {
				t.Fatalf("read host received %d pushes", n)
			}
			assertSameRefs(t, f.source, f.target)
			if advertise {
				if n := reader.MaximalRequests(); n != 1 {
					t.Fatalf("read host maximal listings = %d, want 1", n)
				}
				assertEveryFetchHaves(t, f.haves.take(), before)
			}
		})
	}
}

// The authenticated case of the redirect above: the probe authenticates to
// the target through the credential helper, then discovery redirects it to an
// unrelated read host. Neither the probe nor the push may carry the target's
// credentials there.
func TestRun_IntegrationMaximalProbeKeepsHelperCredentialsOffReadHost(t *testing.T) {
	const user, pass = "writer", "writer-secret"
	f := newMaximalFixture(t, true)
	f.targetServer.username, f.targetServer.password = user, pass

	reader := newSmartHTTPRepoServerV2(t, f.target)
	reader.lsRefsMaximal = true
	t.Cleanup(reader.Close)
	var leaked atomic.Int32
	next := reader.server.Config.Handler
	reader.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); ok && u == user && p == pass {
			leaked.Add(1)
		}
		next.ServeHTTP(w, r)
	})
	// A different host name for the same loopback server, so the redirect
	// crosses sites rather than only ports.
	readerURL := strings.Replace(reader.server.URL, "127.0.0.1", "localhost", 1)
	f.targetServer.uploadPackRedirect = readerURL
	f.configure = func(c *Config) { c.Target.FollowInfoRefsRedirect = true }

	originalCred := auth.GitCredentialCommand
	t.Cleanup(func() { auth.GitCredentialCommand = originalCred })
	auth.GitCredentialCommand = func(_ context.Context, op auth.CredentialOp, input string) ([]byte, error) {
		if op != auth.CredentialOpFill {
			return nil, nil
		}
		// The helper holds credentials for the target only.
		if !strings.Contains(input, "host=127.0.0.1") {
			return []byte("\n"), nil
		}
		return []byte("username=" + user + "\npassword=" + pass + "\n\n"), nil
	}

	f.advance(t)
	if _, err := f.run(t, modeReplicate, ""); err != nil {
		t.Fatalf("replicate: %v", err)
	}
	assertSameRefs(t, f.source, f.target)
	if n := reader.MaximalRequests(); n != 1 {
		t.Fatalf("read host maximal listings = %d, want 1", n)
	}
	if n := leaked.Load(); n != 0 {
		t.Fatalf("read host received the target's credentials on %d requests", n)
	}
}
