package gitproto

import (
	"bytes"
	"io"
	"reflect"
	"strings"
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

// TestParseCommitParents_CanonicalPositionOnly locks in that we
// extract parents only from the canonical position (immediately
// after "tree", in an uninterrupted run). A malformed commit that
// puts "parent" lines elsewhere should not influence the result —
// this matches git's own parser and prevents reachability divergence
// between us and any canonical reader of the same bytes.
func TestParseCommitParents_CanonicalPositionOnly(t *testing.T) {
	t.Parallel()
	var (
		tree = strings.Repeat("a", 40)
		h1   = strings.Repeat("1", 40)
		h2   = strings.Repeat("2", 40)
		h3   = strings.Repeat("3", 40)
	)

	cases := []struct {
		name string
		body string
		want []plumbing.Hash
	}{
		{
			name: "root commit (no parents)",
			body: "tree " + tree + "\nauthor X <x@e> 0 +0000\ncommitter X <x@e> 0 +0000\n\nmsg\n",
			want: nil,
		},
		{
			name: "single parent",
			body: "tree " + tree + "\nparent " + h1 + "\nauthor X <x@e> 0 +0000\n\nmsg\n",
			want: []plumbing.Hash{plumbing.NewHash(h1)},
		},
		{
			name: "merge: two parents",
			body: "tree " + tree + "\nparent " + h1 + "\nparent " + h2 + "\nauthor X <x@e> 0 +0000\n\nmsg\n",
			want: []plumbing.Hash{plumbing.NewHash(h1), plumbing.NewHash(h2)},
		},
		{
			name: "parent before tree is ignored (object malformed: tree must be first)",
			body: "parent " + h1 + "\ntree " + tree + "\nparent " + h2 + "\nauthor X <x@e> 0 +0000\n\nmsg\n",
			want: nil,
		},
		{
			name: "missing tree returns nil",
			body: "parent " + h1 + "\nauthor X <x@e> 0 +0000\n\nmsg\n",
			want: nil,
		},
		{
			name: "parent after author is ignored",
			body: "tree " + tree + "\nparent " + h1 + "\nauthor X <x@e> 0 +0000\nparent " + h2 + "\n\nmsg\n",
			want: []plumbing.Hash{plumbing.NewHash(h1)},
		},
		{
			name: "parent run interrupted by another header is truncated",
			body: "tree " + tree + "\nparent " + h1 + "\nencoding UTF-8\nparent " + h2 + "\nauthor X <x@e> 0 +0000\n\nmsg\n",
			want: []plumbing.Hash{plumbing.NewHash(h1)},
		},
		{
			name: "malformed parent line (too short) stops the run",
			body: "tree " + tree + "\nparent " + h1 + "\nparent short\nparent " + h2 + "\nauthor X <x@e> 0 +0000\n\nmsg\n",
			want: []plumbing.Hash{plumbing.NewHash(h1)},
		},
		{
			name: "non-hex parent hash stops the run",
			body: "tree " + tree + "\nparent " + h1 + "\nparent " + strings.Repeat("g", 40) + "\nparent " + h2 + "\nauthor X <x@e> 0 +0000\n\nmsg\n",
			want: []plumbing.Hash{plumbing.NewHash(h1)},
		},
		{
			name: "three parents in canonical run",
			body: "tree " + tree + "\nparent " + h1 + "\nparent " + h2 + "\nparent " + h3 + "\nauthor X <x@e> 0 +0000\n\nmsg\n",
			want: []plumbing.Hash{plumbing.NewHash(h1), plumbing.NewHash(h2), plumbing.NewHash(h3)},
		},
		{
			name: "empty body returns nil",
			body: "",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseCommitParents([]byte(tc.body))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseCommitParents:\n got=%v\nwant=%v", got, tc.want)
			}
		})
	}
}

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

// craftPackWithObjectHeader builds a minimal packfile whose single object
// header carries the given type and declared size. The payload is a valid but
// empty zlib stream, so the declared size is a claim the pack never backs up —
// exactly the shape a hostile source would send.
func craftPackWithObjectHeader(typ byte, declaredSize uint64) []byte {
	var b bytes.Buffer
	b.WriteString("PACK")
	b.Write([]byte{0, 0, 0, 2}) // version 2
	b.Write([]byte{0, 0, 0, 1}) // one object

	// Pack object header: first byte holds the continuation bit, the 3-bit
	// type, and the low 4 bits of the size; each following byte adds 7 more
	// significant bits.
	first := (typ&7)<<4 | byte(declaredSize&0x0f)
	size := declaredSize >> 4
	var hdr []byte
	for size > 0 {
		hdr = append(hdr, first|0x80)
		first = byte(size & 0x7f)
		size >>= 7
	}
	b.Write(append(hdr, first))

	b.Write([]byte{0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01}) // empty zlib stream
	b.Write(make([]byte, 20))                                       // trailer
	return b.Bytes()
}

// A packfile header may declare any size the varint encoding can hold. Before
// this was bounded, the declared size was passed straight to make(), so a
// 48-byte pack declaring a 128 TiB object triggered `fatal error: out of
// memory` — a runtime fatal, not a panic, so an embedding process could not
// contain it with recover(). The parse must fail cheaply instead.
func TestExtractCommitParentsRejectsOversizedDeclaredObject(t *testing.T) {
	const commitType = 1
	for _, declared := range []uint64{
		maxDeclaredObjectSize + 1,
		1 << 36, // 64 GiB — allocated 65,536 MiB before the fix
		1 << 47, // 128 TiB — killed the process before the fix
	} {
		pack := craftPackWithObjectHeader(commitType, declared)
		if len(pack) > 64 {
			t.Fatalf("expected a tiny pack, got %d bytes", len(pack))
		}
		_, err := ExtractCommitParents(bytes.NewReader(pack))
		if err == nil {
			t.Fatalf("declared size %d: expected an error, got nil", declared)
		}
		if !strings.Contains(err.Error(), "over the") {
			t.Errorf("declared size %d: error %q does not mention the size limit", declared, err)
		}
	}
}

// A legitimate object under the ceiling must still parse, so the bound cannot
// be satisfied by rejecting everything.
func TestExtractCommitParentsAcceptsNormalObjectSizes(t *testing.T) {
	storer := newCommitParentsStorer(DefaultCommitParentsCacheBytes)
	for _, sz := range []int64{0, 1, maxObjectPrealloc, maxObjectPrealloc + 1, maxDeclaredObjectSize} {
		w, err := storer.RawObjectWriter(plumbing.CommitObject, sz)
		if err != nil {
			t.Errorf("RawObjectWriter(commit, %d) = %v, want no error", sz, err)
			continue
		}
		if err := w.Close(); err != nil {
			t.Errorf("Close() for size %d: %v", sz, err)
		}
	}
	if _, err := storer.RawObjectWriter(plumbing.CommitObject, -1); err == nil {
		t.Error("RawObjectWriter(commit, -1) = nil error, want a rejection")
	}
}
