package syncer

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

const testCheckpointPrefix = "refs/entire/checkpoints/"

// writeObject encodes obj into repo's store and returns its hash.
func writeObject(t *testing.T, repo *git.Repository, obj interface {
	Encode(o plumbing.EncodedObject) error
}) plumbing.Hash {
	t.Helper()
	encoded := repo.Storer.NewEncodedObject()
	if err := obj.Encode(encoded); err != nil {
		t.Fatalf("encode object: %v", err)
	}
	h, err := repo.Storer.SetEncodedObject(encoded)
	if err != nil {
		t.Fatalf("store object: %v", err)
	}
	return h
}

// commitCheckpoint points ref at a new commit holding one metadata file whose
// content is unique to the call. With a zero parent it starts a history of
// its own, the shape of a per-session checkpoint ref.
func commitCheckpoint(t *testing.T, repo *git.Repository, ref plumbing.ReferenceName, parent plumbing.Hash) {
	t.Helper()
	blob := repo.Storer.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	w, err := blob.Writer()
	if err != nil {
		t.Fatalf("blob writer: %v", err)
	}
	if _, err := fmt.Fprintf(w, `{"ref":%q,"parent":%q}`, ref, parent); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close blob: %v", err)
	}
	blobHash, err := repo.Storer.SetEncodedObject(blob)
	if err != nil {
		t.Fatalf("store blob: %v", err)
	}
	treeHash := writeObject(t, repo, &object.Tree{Entries: []object.TreeEntry{
		{Name: "metadata.json", Mode: filemode.Regular, Hash: blobHash},
	}})
	commit := &object.Commit{
		Author:    objectSignature,
		Committer: objectSignature,
		Message:   "checkpoint " + ref.String(),
		TreeHash:  treeHash,
	}
	if !parent.IsZero() {
		commit.ParentHashes = []plumbing.Hash{parent}
	}
	h := writeObject(t, repo, commit)
	if err := repo.Storer.SetReference(plumbing.NewHashReference(ref, h)); err != nil {
		t.Fatalf("set %s: %v", ref, err)
	}
}

// haveRecorder collects the have lines of each v2 fetch the source serves,
// one list per fetch, in the order the fetches arrived.
type haveRecorder struct {
	mu      sync.Mutex
	fetches [][]plumbing.Hash
}

func (r *haveRecorder) attach(s *smartHTTPRepoServer) {
	s.uploadPackV2FetchRaw = func(_ http.ResponseWriter, req v2TestCommandRequest, _ []byte) bool {
		haves := []plumbing.Hash{}
		for _, arg := range req.Args {
			if h, ok := strings.CutPrefix(arg, "have "); ok {
				haves = append(haves, plumbing.NewHash(strings.TrimSpace(h)))
			}
		}
		r.mu.Lock()
		r.fetches = append(r.fetches, sortedHashes(haves...))
		r.mu.Unlock()
		return false // let the server answer the fetch as usual
	}
}

func (r *haveRecorder) take() [][]plumbing.Hash {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.fetches
	r.fetches = nil
	return out
}

func sortedHashes(hs ...plumbing.Hash) []plumbing.Hash {
	out := slices.Clone(hs)
	slices.SortFunc(out, func(a, b plumbing.Hash) int { return strings.Compare(a.String(), b.String()) })
	return out
}

// assertSameRefs fails unless target holds exactly the refs source does
// (HEAD aside), at the same hashes, with every commit, tree and blob
// reachable from them readable.
func assertSameRefs(t *testing.T, source, target *git.Repository) {
	t.Helper()
	collect := func(repo *git.Repository) map[plumbing.ReferenceName]plumbing.Hash {
		out := map[plumbing.ReferenceName]plumbing.Hash{}
		iter, err := repo.Storer.IterReferences()
		if err != nil {
			t.Fatalf("iterate refs: %v", err)
		}
		if err := iter.ForEach(func(ref *plumbing.Reference) error {
			if ref.Type() == plumbing.HashReference && ref.Name() != plumbing.HEAD {
				out[ref.Name()] = ref.Hash()
			}
			return nil
		}); err != nil {
			t.Fatalf("walk refs: %v", err)
		}
		return out
	}
	src, dst := collect(source), collect(target)
	for name, h := range src {
		if dst[name] != h {
			t.Errorf("target %s = %s, want %s", name, dst[name], h)
			continue
		}
		assertClosureReadable(t, target, h)
	}
	for name := range dst {
		if _, ok := src[name]; !ok {
			t.Errorf("target has %s, which the source does not", name)
		}
	}
}

// independentHistoryFixture is a source with one branch and three checkpoint
// refs, each its own history, already replicated to an empty target.
type independentHistoryFixture struct {
	source, target *git.Repository
	sourceFS       billy.Filesystem
	sourceURL      string
	targetURL      string
	haves          *haveRecorder
	checkpoints    []plumbing.ReferenceName
}

func newIndependentHistoryFixture(t *testing.T) *independentHistoryFixture {
	t.Helper()
	sourceRepo, sourceFS := newSourceRepo(t)
	targetRepo, _ := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 2)
	f := &independentHistoryFixture{source: sourceRepo, target: targetRepo, sourceFS: sourceFS, haves: &haveRecorder{}}
	for _, name := range []string{"aa/one", "bb/two", "cc/three"} {
		ref := plumbing.ReferenceName(testCheckpointPrefix + name)
		commitCheckpoint(t, sourceRepo, ref, plumbing.ZeroHash)
		f.checkpoints = append(f.checkpoints, ref)
	}

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	targetServer.receivePackThinCap = true
	t.Cleanup(sourceServer.Close)
	t.Cleanup(targetServer.Close)
	f.haves.attach(sourceServer)
	f.sourceURL, f.targetURL = sourceServer.RepoURL(), targetServer.RepoURL()

	if _, err := f.run(t, modeReplicate, true); err != nil {
		t.Fatalf("seed replicate: %v", err)
	}
	assertSameRefs(t, sourceRepo, targetRepo)
	f.haves.take()
	return f
}

func (f *independentHistoryFixture) run(t *testing.T, mode string, independent bool) (Result, error) {
	t.Helper()
	cfg := Config{
		Source:  Endpoint{URL: f.sourceURL},
		Target:  Endpoint{URL: f.targetURL},
		Mode:    mode,
		AllRefs: true,
		Prune:   mode == modeReplicate,
	}
	if independent {
		cfg.IndependentHistoryRefPrefixes = []string{testCheckpointPrefix}
	}
	return Run(context.Background(), cfg)
}

func (f *independentHistoryFixture) tip(t *testing.T, ref plumbing.ReferenceName) plumbing.Hash {
	t.Helper()
	r, err := f.source.Reference(ref, true)
	if err != nil {
		t.Fatalf("read %s: %v", ref, err)
	}
	return r.Hash()
}

func (f *independentHistoryFixture) checkpointTips(t *testing.T) []plumbing.Hash {
	t.Helper()
	out := make([]plumbing.Hash, 0, len(f.checkpoints))
	for _, ref := range f.checkpoints {
		out = append(out, f.tip(t, ref))
	}
	return out
}

func (f *independentHistoryFixture) advanceBranch(t *testing.T) (before plumbing.Hash) {
	t.Helper()
	branch := plumbing.NewBranchReferenceName(testBranch)
	before = f.tip(t, branch)
	makeCommits(t, f.source, f.sourceFS, 1)
	return before
}

// assertHaves fails unless exactly one fetch ran and it offered want.
func assertHaves(t *testing.T, fetches [][]plumbing.Hash, want ...plumbing.Hash) {
	t.Helper()
	if len(fetches) != 1 {
		t.Fatalf("got %d fetches (haves %v), want exactly one", len(fetches), fetches)
	}
	assertFetchHaves(t, fetches[0], want...)
}

func assertFetchHaves(t *testing.T, got []plumbing.Hash, want ...plumbing.Hash) {
	t.Helper()
	if want := sortedHashes(want...); !slices.Equal(got, want) {
		t.Fatalf("haves offered = %v, want %v", got, want)
	}
}

// The case the option exists for: a branch moves while the checkpoint refs
// sit still. Without the option every checkpoint tip rides along as a have.
func TestRun_IntegrationReplicateBranchUpdateOffersNoCheckpointHaves(t *testing.T) {
	f := newIndependentHistoryFixture(t)
	before := f.advanceBranch(t)

	if _, err := f.run(t, modeReplicate, true); err != nil {
		t.Fatalf("replicate: %v", err)
	}
	assertHaves(t, f.haves.take(), before)
	assertSameRefs(t, f.source, f.target)
}

func TestRun_IntegrationReplicateBranchUpdateBaselineOffersCheckpointHaves(t *testing.T) {
	f := newIndependentHistoryFixture(t)
	before := f.advanceBranch(t)

	if _, err := f.run(t, modeReplicate, false); err != nil {
		t.Fatalf("replicate: %v", err)
	}
	assertHaves(t, f.haves.take(), append(f.checkpointTips(t), before)...)
	assertSameRefs(t, f.source, f.target)
}

// A new checkpoint starts its own history, so no existing checkpoint tip can
// shorten its pack. The branch tip is still offered.
func TestRun_IntegrationReplicateNewCheckpointOffersNoCheckpointHaves(t *testing.T) {
	f := newIndependentHistoryFixture(t)
	commitCheckpoint(t, f.source, testCheckpointPrefix+"dd/four", plumbing.ZeroHash)

	if _, err := f.run(t, modeReplicate, true); err != nil {
		t.Fatalf("replicate: %v", err)
	}
	assertHaves(t, f.haves.take(), f.tip(t, plumbing.NewBranchReferenceName(testBranch)))
	assertSameRefs(t, f.source, f.target)
}

// An updated checkpoint keeps its own old value as a have; its neighbours do
// not ride along.
func TestRun_IntegrationReplicateCheckpointUpdateOffersItsOldValue(t *testing.T) {
	f := newIndependentHistoryFixture(t)
	ref := f.checkpoints[1]
	before := f.tip(t, ref)
	commitCheckpoint(t, f.source, ref, before)

	if _, err := f.run(t, modeReplicate, true); err != nil {
		t.Fatalf("replicate: %v", err)
	}
	assertHaves(t, f.haves.take(), f.tip(t, plumbing.NewBranchReferenceName(testBranch)), before)
	assertSameRefs(t, f.source, f.target)
}

// Sync mode's incremental relay builds its haves the same way. A branch update
// in sync mode first fetches the source closure for the fast-forward check,
// before any plan exists, and that fetch keeps the full have list: only the
// relay fetch that follows is narrowed.
func TestRun_IntegrationSyncIncrementalOffersNoCheckpointHaves(t *testing.T) {
	f := newIndependentHistoryFixture(t)
	before := f.advanceBranch(t)

	result, err := f.run(t, modeSync, true)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !result.Relay || result.RelayMode != relayModeIncremental {
		t.Fatalf("expected incremental relay, got %+v", result)
	}
	fetches := f.haves.take()
	if len(fetches) != 2 {
		t.Fatalf("got %d fetches (haves %v), want the closure fetch and the relay fetch", len(fetches), fetches)
	}
	assertFetchHaves(t, fetches[0], append(f.checkpointTips(t), before)...)
	assertFetchHaves(t, fetches[1], before)
	assertSameRefs(t, f.source, f.target)
}

// A delete is not a fetch: dropping a checkpoint never asks the source for a
// pack, and the target loses exactly that ref.
func TestRun_IntegrationReplicatePrunesCheckpointWithIndependentPrefix(t *testing.T) {
	f := newIndependentHistoryFixture(t)
	if err := f.source.Storer.RemoveReference(f.checkpoints[0]); err != nil {
		t.Fatalf("remove %s: %v", f.checkpoints[0], err)
	}

	result, err := f.run(t, modeReplicate, true)
	if err != nil {
		t.Fatalf("replicate: %v", err)
	}
	if result.Deleted != 1 {
		t.Fatalf("expected one delete, got %+v", result)
	}
	if fetches := f.haves.take(); len(fetches) != 0 {
		t.Fatalf("a prune-only run fetched from the source: %v", fetches)
	}
	assertSameRefs(t, f.source, f.target)
}

// assertClosureReadable walks every commit reachable from each tip and reads
// its tree and every blob, so a pack that left out an object the target did
// not already hold fails here and not only on a later clone.
func assertClosureReadable(t *testing.T, repo *git.Repository, tips ...plumbing.Hash) {
	t.Helper()
	for _, tip := range tips {
		commit, err := repo.CommitObject(tip)
		if err != nil {
			t.Fatalf("read tip %s: %v", tip, err)
		}
		iter := object.NewCommitPreorderIter(commit, nil, nil)
		if err := iter.ForEach(func(c *object.Commit) error {
			tree, err := c.Tree()
			if err != nil {
				return fmt.Errorf("commit %s tree: %w", c.Hash, err)
			}
			return tree.Files().ForEach(func(f *object.File) error {
				if _, err := f.Contents(); err != nil {
					return fmt.Errorf("commit %s blob %s (%s): %w", c.Hash, f.Hash, f.Name, err)
				}
				return nil
			})
		}); err != nil {
			t.Fatalf("closure of %s on target: %v", tip, err)
		}
	}
}

// newSharedHistoryFixture is newIndependentHistoryFixture plus a ref under
// the prefix that builds on the branch, so the prefix is misconfigured: the
// histories it names are not independent. Already replicated to the target.
func newSharedHistoryFixture(t *testing.T) (f *independentHistoryFixture, shared plumbing.ReferenceName) {
	t.Helper()
	f = newIndependentHistoryFixture(t)
	shared = testCheckpointPrefix + "shared/base"
	commitCheckpoint(t, f.source, shared, f.tip(t, plumbing.NewBranchReferenceName(testBranch)))
	if _, err := f.run(t, modeReplicate, true); err != nil {
		t.Fatalf("seed shared ref: %v", err)
	}
	assertSameRefs(t, f.source, f.target)
	f.haves.take()
	return f, shared
}

// The option is a claim about history. When the claim is false, a new ref
// under the prefix that builds on an existing one no longer gets that ref's
// tip as a have, so the source resends objects the target already holds. The
// result must still be a complete, readable copy.
func TestRun_IntegrationReplicateSharedHistoryUnderPrefixStaysComplete(t *testing.T) {
	f, shared := newSharedHistoryFixture(t)
	base := f.tip(t, shared)
	next := plumbing.ReferenceName(testCheckpointPrefix + "shared/next")
	commitCheckpoint(t, f.source, next, base)

	if _, err := f.run(t, modeReplicate, true); err != nil {
		t.Fatalf("replicate: %v", err)
	}
	assertHaves(t, f.haves.take(), f.tip(t, plumbing.NewBranchReferenceName(testBranch)))
	assertSameRefs(t, f.source, f.target)
}

// A branch that moves onto a commit the target holds only under the prefix
// gets the same treatment: that commit is resent, and the branch's whole
// history is readable on the target.
func TestRun_IntegrationReplicateBranchOntoPrefixedHistoryStaysComplete(t *testing.T) {
	f, shared := newSharedHistoryFixture(t)
	branch := plumbing.NewBranchReferenceName(testBranch)
	before := f.tip(t, branch)
	commitCheckpoint(t, f.source, branch, f.tip(t, shared))

	if _, err := f.run(t, modeReplicate, true); err != nil {
		t.Fatalf("replicate: %v", err)
	}
	assertHaves(t, f.haves.take(), before)
	assertSameRefs(t, f.source, f.target)
}
