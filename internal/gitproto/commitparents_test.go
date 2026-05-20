package gitproto

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"
)

// buildSyntheticCommitChain constructs a linear chain of n commits plus
// (when branch=true) a second branch fork from the second commit. The
// commits share a tree to keep the pack small. Returns the raw pack
// bytes and the expected (commit -> parent hashes) map.
func buildSyntheticCommitChain(t *testing.T, n int, branch bool) ([]byte, map[plumbing.Hash][]plumbing.Hash) {
	t.Helper()
	store := memory.NewStorage()

	// One shared tree to keep the pack lean.
	tree := &object.Tree{Entries: []object.TreeEntry{
		{Name: "f", Mode: 0o100644, Hash: writeBlob(t, store, "v")},
	}}
	treeObj := store.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		t.Fatalf("tree encode: %v", err)
	}
	treeHash, err := store.SetEncodedObject(treeObj)
	if err != nil {
		t.Fatalf("tree set: %v", err)
	}

	hashes := []plumbing.Hash{treeHash}
	expected := map[plumbing.Hash][]plumbing.Hash{}

	var prev plumbing.Hash
	for i := range n {
		c := &object.Commit{
			TreeHash:  treeHash,
			Author:    object.Signature{Name: "T", Email: "t@example", When: time.Unix(int64(i), 0)},
			Committer: object.Signature{Name: "T", Email: "t@example", When: time.Unix(int64(i), 0)},
			Message:   "c" + string(rune('0'+i)),
		}
		if !prev.IsZero() {
			c.ParentHashes = []plumbing.Hash{prev}
		}
		obj := store.NewEncodedObject()
		if err := c.Encode(obj); err != nil {
			t.Fatalf("commit encode: %v", err)
		}
		h, err := store.SetEncodedObject(obj)
		if err != nil {
			t.Fatalf("commit set: %v", err)
		}
		hashes = append(hashes, h)
		if prev.IsZero() {
			expected[h] = nil
		} else {
			expected[h] = []plumbing.Hash{prev}
		}
		prev = h
	}

	if branch && n >= 2 {
		// Find the second commit's hash (index 1 of commits, hashes[2])
		fork := hashes[2]
		c := &object.Commit{
			TreeHash:     treeHash,
			Author:       object.Signature{Name: "T", Email: "t@example", When: time.Unix(int64(n+1), 0)},
			Committer:    object.Signature{Name: "T", Email: "t@example", When: time.Unix(int64(n+1), 0)},
			Message:      "branch",
			ParentHashes: []plumbing.Hash{fork},
		}
		obj := store.NewEncodedObject()
		if err := c.Encode(obj); err != nil {
			t.Fatalf("branch encode: %v", err)
		}
		h, err := store.SetEncodedObject(obj)
		if err != nil {
			t.Fatalf("branch set: %v", err)
		}
		hashes = append(hashes, h)
		expected[h] = []plumbing.Hash{fork}
	}

	var buf bytes.Buffer
	enc := packfile.NewEncoder(&buf, store, false)
	if _, err := enc.Encode(hashes, 10); err != nil {
		t.Fatalf("encode pack: %v", err)
	}
	return buf.Bytes(), expected
}

func writeBlob(t *testing.T, store *memory.Storage, content string) plumbing.Hash {
	t.Helper()
	obj := store.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		t.Fatalf("blob writer: %v", err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatalf("blob write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("blob close: %v", err)
	}
	obj.SetSize(int64(len(content)))
	h, err := store.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("blob set: %v", err)
	}
	return h
}

func TestExtractCommitParents_LinearChain(t *testing.T) {
	t.Parallel()
	pack, want := buildSyntheticCommitChain(t, 5, false)
	got, err := ExtractCommitParents(io.NopCloser(bytes.NewReader(pack)))
	if err != nil {
		t.Fatalf("ExtractCommitParents: %v", err)
	}
	assertParentsEqual(t, got, want)
}

func TestExtractCommitParents_Branching(t *testing.T) {
	t.Parallel()
	pack, want := buildSyntheticCommitChain(t, 5, true)
	got, err := ExtractCommitParents(io.NopCloser(bytes.NewReader(pack)))
	if err != nil {
		t.Fatalf("ExtractCommitParents: %v", err)
	}
	assertParentsEqual(t, got, want)
}

// Force delta encoding by building many commits whose bodies differ
// only slightly. With encoder window of 10, the encoder should produce
// OFS deltas between adjacent commits.
func TestExtractCommitParents_WithDeltas(t *testing.T) {
	t.Parallel()
	pack, want := buildSyntheticCommitChain(t, 50, false)
	got, err := ExtractCommitParentsWithCache(io.NopCloser(bytes.NewReader(pack)), 1024*1024)
	if err != nil {
		t.Fatalf("ExtractCommitParents: %v", err)
	}
	assertParentsEqual(t, got, want)
}

// readOnlyReader hides any io.Seeker / io.ReaderAt the underlying
// source might implement so the extractor takes the spill-to-disk
// branch — matching what happens with the demuxed HTTP stream in
// production.
type readOnlyReader struct{ r io.Reader }

func (r readOnlyReader) Read(p []byte) (int, error) { return r.r.Read(p) }

func TestExtractCommitParents_NonSeekableSpillsToDisk(t *testing.T) {
	t.Parallel()
	pack, want := buildSyntheticCommitChain(t, 30, true)
	got, err := ExtractCommitParents(readOnlyReader{r: bytes.NewReader(pack)})
	if err != nil {
		t.Fatalf("ExtractCommitParents: %v", err)
	}
	assertParentsEqual(t, got, want)
}

func assertParentsEqual(t *testing.T, got, want map[plumbing.Hash][]plumbing.Hash) {
	t.Helper()
	commitsGot := 0
	for h := range got {
		// Only count commit-shaped entries (the synthetic pack has tree+blob too,
		// but those don't make it into the parents map).
		_ = h
		commitsGot++
	}
	wantCount := len(want)
	if commitsGot != wantCount {
		t.Fatalf("commit count: got %d, want %d", commitsGot, wantCount)
	}
	for h, wantParents := range want {
		gotParents, ok := got[h]
		if !ok {
			t.Fatalf("commit %s missing from result", h)
		}
		if len(gotParents) != len(wantParents) {
			t.Fatalf("commit %s parents: got %v, want %v", h, gotParents, wantParents)
		}
		for i, p := range wantParents {
			if gotParents[i] != p {
				t.Fatalf("commit %s parent[%d]: got %s, want %s", h, i, gotParents[i], p)
			}
		}
	}
}
