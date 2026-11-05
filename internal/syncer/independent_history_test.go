package syncer

import (
	"context"
	"fmt"
	"io"
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
	"github.com/go-git/go-git/v6/storage/memory"
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

// writeBlob stores content as a blob in repo and returns its hash.
func writeBlob(t *testing.T, repo *git.Repository, content string) plumbing.Hash {
	t.Helper()
	blob := repo.Storer.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	w, err := blob.Writer()
	if err != nil {
		t.Fatalf("blob writer: %v", err)
	}
	if _, err := io.WriteString(w, content); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close blob: %v", err)
	}
	h, err := repo.Storer.SetEncodedObject(blob)
	if err != nil {
		t.Fatalf("store blob: %v", err)
	}
	return h
}

// commitCheckpoint points ref at a new commit laid out like a session
// checkpoint: metadata.json beside a 0/ directory holding the transcript,
// both with content unique to the call. With a zero parent it starts a
// history of its own. The nested directory keeps assertClosureReadable
// honest about subtrees.
func commitCheckpoint(t *testing.T, repo *git.Repository, ref plumbing.ReferenceName, parent plumbing.Hash) {
	t.Helper()
	unique := fmt.Sprintf(`{"ref":%q,"parent":%q}`, ref, parent)
	sessionTree := writeObject(t, repo, &object.Tree{Entries: []object.TreeEntry{
		{Name: "full.jsonl", Mode: filemode.Regular, Hash: writeBlob(t, repo, "transcript "+unique)},
	}})
	treeHash := writeObject(t, repo, &object.Tree{Entries: []object.TreeEntry{
		{Name: "0", Mode: filemode.Dir, Hash: sessionTree},
		{Name: "metadata.json", Mode: filemode.Regular, Hash: writeBlob(t, repo, unique)},
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

// assertClosureReadable fails unless every commit, tree and blob reachable
// from each tip is in repo, so a pack that left out an object the target did
// not already hold fails here and not only on a later clone.
func assertClosureReadable(t *testing.T, repo *git.Repository, tips ...plumbing.Hash) {
	t.Helper()
	if err := readClosure(repo, tips...); err != nil {
		t.Fatalf("closure on target: %v", err)
	}
}

// readClosure reads every object reachable from tips and returns the first
// one it cannot read. It walks parents and tree entries itself, with lookups
// that report a missing object as an error, and reads every blob.
func readClosure(repo *git.Repository, tips ...plumbing.Hash) error {
	seen := map[plumbing.Hash]struct{}{}
	var readTree func(h plumbing.Hash, path string) error
	readTree = func(h plumbing.Hash, path string) error {
		if _, ok := seen[h]; ok {
			return nil
		}
		seen[h] = struct{}{}
		tree, err := repo.TreeObject(h)
		if err != nil {
			return fmt.Errorf("tree %s (%s): %w", h, path, err)
		}
		for _, entry := range tree.Entries {
			name := path + "/" + entry.Name
			switch entry.Mode {
			case filemode.Dir:
				if err := readTree(entry.Hash, name); err != nil {
					return err
				}
			case filemode.Submodule:
				// A gitlink names a commit in another repository.
			case filemode.Empty, filemode.Regular, filemode.Deprecated, filemode.Executable, filemode.Symlink:
				if _, ok := seen[entry.Hash]; ok {
					continue
				}
				seen[entry.Hash] = struct{}{}
				blob, err := repo.BlobObject(entry.Hash)
				if err != nil {
					return fmt.Errorf("blob %s (%s): %w", entry.Hash, name, err)
				}
				r, err := blob.Reader()
				if err != nil {
					return fmt.Errorf("open blob %s (%s): %w", entry.Hash, name, err)
				}
				_, err = io.Copy(io.Discard, r)
				_ = r.Close()
				if err != nil {
					return fmt.Errorf("read blob %s (%s): %w", entry.Hash, name, err)
				}
			default:
				return fmt.Errorf("tree %s entry %s: unexpected mode %s", h, name, entry.Mode)
			}
		}
		return nil
	}

	pending := slices.Clone(tips)
	for len(pending) > 0 {
		h := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		commit, err := repo.CommitObject(h)
		if err != nil {
			return fmt.Errorf("commit %s: %w", h, err)
		}
		if err := readTree(commit.TreeHash, ""); err != nil {
			return fmt.Errorf("commit %s: %w", h, err)
		}
		pending = append(pending, commit.ParentHashes...)
	}
	return nil
}

// The closure check is only as good as its traversal. Remove one object of
// each kind from a nested checkpoint history and require readClosure to name
// it, where go-git's file iterator would have let a missing subtree pass.
func TestReadClosureDetectsMissingObjects(t *testing.T) {
	ref := plumbing.ReferenceName(testCheckpointPrefix + "aa/one")
	build := func(t *testing.T) (*git.Repository, *memory.Storage, *object.Commit) {
		t.Helper()
		store := memory.NewStorage()
		repo, err := git.Init(store)
		if err != nil {
			t.Fatalf("init: %v", err)
		}
		commitCheckpoint(t, repo, ref, plumbing.ZeroHash)
		r, err := repo.Reference(ref, true)
		if err != nil {
			t.Fatalf("read %s: %v", ref, err)
		}
		commitCheckpoint(t, repo, ref, r.Hash())
		r, err = repo.Reference(ref, true)
		if err != nil {
			t.Fatalf("read %s: %v", ref, err)
		}
		tip, err := repo.CommitObject(r.Hash())
		if err != nil {
			t.Fatalf("read tip: %v", err)
		}
		if err := readClosure(repo, tip.Hash); err != nil {
			t.Fatalf("complete history reported missing: %v", err)
		}
		return repo, store, tip
	}
	drop := func(store *memory.Storage, h plumbing.Hash) {
		for _, m := range []map[plumbing.Hash]plumbing.EncodedObject{store.Objects, store.Commits, store.Trees, store.Blobs} {
			delete(m, h)
		}
	}
	subtree := func(t *testing.T, repo *git.Repository, c *object.Commit) object.TreeEntry {
		t.Helper()
		tree, err := repo.TreeObject(c.TreeHash)
		if err != nil {
			t.Fatalf("root tree: %v", err)
		}
		for _, e := range tree.Entries {
			if e.Mode == filemode.Dir {
				return e
			}
		}
		t.Fatal("fixture has no subdirectory")
		return object.TreeEntry{}
	}

	t.Run("subtree", func(t *testing.T) {
		repo, store, tip := build(t)
		dir := subtree(t, repo, tip)
		drop(store, dir.Hash)
		if err := readClosure(repo, tip.Hash); err == nil || !strings.Contains(err.Error(), dir.Hash.String()) {
			t.Fatalf("readClosure with subtree %s removed = %v, want an error naming it", dir.Hash, err)
		}
	})
	t.Run("nested blob", func(t *testing.T) {
		repo, store, tip := build(t)
		dir, err := repo.TreeObject(subtree(t, repo, tip).Hash)
		if err != nil {
			t.Fatalf("subtree: %v", err)
		}
		blob := dir.Entries[0].Hash
		drop(store, blob)
		if err := readClosure(repo, tip.Hash); err == nil || !strings.Contains(err.Error(), blob.String()) {
			t.Fatalf("readClosure with blob %s removed = %v, want an error naming it", blob, err)
		}
	})
	t.Run("parent commit", func(t *testing.T) {
		repo, store, tip := build(t)
		parent := tip.ParentHashes[0]
		drop(store, parent)
		if err := readClosure(repo, tip.Hash); err == nil || !strings.Contains(err.Error(), parent.String()) {
			t.Fatalf("readClosure with parent %s removed = %v, want an error naming it", parent, err)
		}
	})
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
