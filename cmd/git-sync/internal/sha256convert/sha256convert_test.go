package sha256convert

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"
	gogitstorer "github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/storage/filesystem"

	"entire.io/entire/git-sync/internal/planner"
)

// TestTranslator builds a small SHA1 source repo with blobs, trees, commits,
// and an annotated tag — including signed commit/tag — then runs the
// translator and asserts both the bookkeeping counts and the on-disk
// invariant: every loose object's filename equals sha256(headered content).
// That invariant is the one go-git v6 alpha 3 gets wrong via its
// SetEncodedObject path; verifying it directly prevents regressing back
// onto the broken loose-object writer.
func TestTranslator(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src.git")
	dstDir := filepath.Join(root, "dst.git")

	srcRepo, err := git.PlainInit(srcDir, true)
	if err != nil {
		t.Fatalf("init SHA1 source: %v", err)
	}
	dstRepo, err := git.PlainInit(dstDir, true, git.WithObjectFormat(formatcfg.SHA256))
	if err != nil {
		t.Fatalf("init SHA256 target: %v", err)
	}

	blobHash := writeBlob(t, srcRepo.Storer, []byte("hello world\n"))
	treeHash := writeTree(t, srcRepo.Storer, []object.TreeEntry{
		{Name: "README", Mode: filemode.Regular, Hash: blobHash},
	})

	sig := object.Signature{Name: "Test", Email: "test@example.com", When: time.Unix(1700000000, 0).UTC()}
	commit1 := &object.Commit{
		Author:    sig,
		Committer: sig,
		Message:   "initial\n",
		TreeHash:  treeHash,
		Signature: "-----BEGIN PGP SIGNATURE-----\nfake sig data\n-----END PGP SIGNATURE-----",
	}
	c1Hash := writeObject(t, srcRepo.Storer, commit1.Encode)

	commit2 := &object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      "second\n",
		TreeHash:     treeHash,
		ParentHashes: []plumbing.Hash{c1Hash},
	}
	c2Hash := writeObject(t, srcRepo.Storer, commit2.Encode)

	tag := &object.Tag{
		Name:       "v1",
		Tagger:     sig,
		Message:    "annotated tag\n",
		TargetType: plumbing.CommitObject,
		Target:     c2Hash,
		Signature:  "-----BEGIN PGP SIGNATURE-----\nfake tag sig\n-----END PGP SIGNATURE-----",
	}
	tagHash := writeObject(t, srcRepo.Storer, tag.Encode)

	reachable, err := discoverReachable(t.Context(), srcRepo.Storer, []plumbing.Hash{tagHash}, nil)
	if err != nil {
		t.Fatalf("discoverReachable: %v", err)
	}
	tr, err := newTranslator(t.Context(), srcRepo.Storer, dstRepo.Storer, false, reachable)
	if err != nil {
		t.Fatalf("newTranslator: %v", err)
	}
	newTagHash, err := tr.translate(tagHash)
	if err != nil {
		t.Fatalf("translate tag: %v", err)
	}

	wantCounts := Counts{Blobs: 1, Trees: 1, Commits: 2, Tags: 1}
	if got := tr.snapshotCounts(); got != wantCounts {
		t.Errorf("counts: got %+v, want %+v", got, wantCounts)
	}
	if tr.signaturesStripped != 2 {
		t.Errorf("signatures stripped: got %d, want 2 (commit + tag)", tr.signaturesStripped)
	}

	// Idempotency: translating the same hash again must reuse the mapping
	// without writing more objects or bumping counters.
	startBlobs := tr.blobs.Load()
	if _, err := tr.translate(tagHash); err != nil {
		t.Fatalf("re-translate tag: %v", err)
	}
	if tr.blobs.Load() != startBlobs {
		t.Errorf("re-translate increased blob count; memoization broken")
	}

	// Every translated hash must point at a loose object whose filename
	// equals sha256(headered content). This is the precise invariant the
	// go-git bug violates — keep it as a test.
	objectsDir := filepath.Join(dstDir, "objects")
	verified := 0
	for _, h := range tr.mapping {
		assertLooseObjectHashMatches(t, objectsDir, h)
		verified++
	}
	if verified == 0 {
		t.Fatal("no objects in mapping; nothing was verified")
	}

	// The translated tag must decode under the SHA256 target and point at
	// a SHA256 commit whose tree resolves to a SHA256 tree.
	tagObj, err := object.GetTag(dstRepo.Storer, newTagHash)
	if err != nil {
		t.Fatalf("read translated tag: %v", err)
	}
	if tagObj.Signature != "" {
		t.Errorf("translated tag still carries a signature: %q", tagObj.Signature)
	}
	if tagObj.Target != tr.mapping[c2Hash] {
		t.Errorf("translated tag target: got %s, want %s", tagObj.Target, tr.mapping[c2Hash])
	}

	commit, err := object.GetCommit(dstRepo.Storer, tagObj.Target)
	if err != nil {
		t.Fatalf("read translated commit: %v", err)
	}
	if commit.Signature != "" {
		t.Errorf("translated commit still carries a signature: %q", commit.Signature)
	}
	if len(commit.ParentHashes) != 1 || commit.ParentHashes[0] != tr.mapping[c1Hash] {
		t.Errorf("translated commit parents: got %v, want [%s]", commit.ParentHashes, tr.mapping[c1Hash])
	}
	if commit.TreeHash != tr.mapping[treeHash] {
		t.Errorf("translated commit tree: got %s, want %s", commit.TreeHash, tr.mapping[treeHash])
	}
}

// TestTranslator_RewritesMessageHashes confirms that SHA1 hash references
// in commit and tag messages — both full 40-char and short forms — are
// rewritten to the corresponding SHA256 when those SHA1s are translated
// objects in the same conversion, and that ambiguous/unknown short
// prefixes are left alone.
func TestTranslator_RewritesMessageHashes(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src.git")
	dstDir := filepath.Join(root, "dst.git")

	srcRepo, err := git.PlainInit(srcDir, true)
	if err != nil {
		t.Fatalf("init SHA1 source: %v", err)
	}
	dstRepo, err := git.PlainInit(dstDir, true, git.WithObjectFormat(formatcfg.SHA256))
	if err != nil {
		t.Fatalf("init SHA256 target: %v", err)
	}

	blobHash := writeBlob(t, srcRepo.Storer, []byte("x\n"))
	treeHash := writeTree(t, srcRepo.Storer, []object.TreeEntry{
		{Name: "f", Mode: filemode.Regular, Hash: blobHash},
	})
	sig := object.Signature{Name: "Test", Email: "t@example.com", When: time.Unix(1700000000, 0).UTC()}
	parent := &object.Commit{Author: sig, Committer: sig, Message: "first\n", TreeHash: treeHash}
	parentSHA1 := writeObject(t, srcRepo.Storer, parent.Encode)

	// Child commit's message references the parent by full hash, by 7-char
	// short prefix, and includes an unrelated 7-char hex string that should
	// not match anything in the mapping.
	parentHex := parentSHA1.String()
	childMsg := fmt.Sprintf(
		"reverts %s\nsee short %s for context\nunrelated hex 1234567 follows\n",
		parentHex, parentHex[:7])
	child := &object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      childMsg,
		TreeHash:     treeHash,
		ParentHashes: []plumbing.Hash{parentSHA1},
	}
	childSHA1 := writeObject(t, srcRepo.Storer, child.Encode)

	reachable, err := discoverReachable(t.Context(), srcRepo.Storer, []plumbing.Hash{childSHA1}, nil)
	if err != nil {
		t.Fatalf("discoverReachable: %v", err)
	}
	tr, err := newTranslator(t.Context(), srcRepo.Storer, dstRepo.Storer, true, reachable)
	if err != nil {
		t.Fatalf("newTranslator: %v", err)
	}
	if _, err := tr.translate(childSHA1); err != nil {
		t.Fatalf("translate child: %v", err)
	}

	// 2 references should have been rewritten (full + short). The unrelated
	// 7-char hex string is not in the mapping, so it stays.
	if tr.messageRewrites != 2 {
		t.Errorf("message rewrites: got %d, want 2", tr.messageRewrites)
	}

	childNew := tr.mapping[childSHA1]
	parentNew := tr.mapping[parentSHA1]
	gotChild, err := object.GetCommit(dstRepo.Storer, childNew)
	if err != nil {
		t.Fatalf("read translated child: %v", err)
	}
	if !strings.Contains(gotChild.Message, parentNew.String()) {
		t.Errorf("child message missing full SHA256 of parent:\n%s", gotChild.Message)
	}
	if strings.Contains(gotChild.Message, parentHex) {
		t.Errorf("child message still contains original parent SHA1:\n%s", gotChild.Message)
	}
	if !strings.Contains(gotChild.Message, "1234567") {
		t.Errorf("unrelated short hex was wrongly substituted:\n%s", gotChild.Message)
	}
}

// TestTranslator_RewritesCrossBranchReferences is the test that proves the
// discovery-plus-topological-DFS design fixes the cross-branch limitation
// the older inline-only rewriter had. Two unrelated branches share no
// ancestry. Branch A has a single commit cA. Branch B has commit cB whose
// message references cA by both full and abbreviated SHA1. We translate B
// first, *then* A — the order under which the older code would have left
// cB's message un-rewritten because cA was not yet in the mapping when cB
// was encoded. With message-reference edges in the DFS, translating cB
// pulls cA in via t.translate, so the mapping is populated and the
// rewrite succeeds.
func TestTranslator_RewritesCrossBranchReferences(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src.git")
	dstDir := filepath.Join(root, "dst.git")
	srcRepo := initSHA1(t, srcDir)
	dstRepo := initSHA256(t, dstDir)

	blobA := writeBlob(t, srcRepo.Storer, []byte("a\n"))
	treeA := writeTree(t, srcRepo.Storer, []object.TreeEntry{
		{Name: "a", Mode: filemode.Regular, Hash: blobA},
	})
	blobB := writeBlob(t, srcRepo.Storer, []byte("b\n"))
	treeB := writeTree(t, srcRepo.Storer, []object.TreeEntry{
		{Name: "b", Mode: filemode.Regular, Hash: blobB},
	})

	sig := object.Signature{Name: "Test", Email: "t@example.com", When: time.Unix(1700000000, 0).UTC()}
	cA := writeObject(t, srcRepo.Storer, (&object.Commit{
		Author: sig, Committer: sig, Message: "branch A tip\n", TreeHash: treeA,
	}).Encode)
	// cB has no parent in common with cA — they are siblings under
	// no ancestor, exactly the case where ancestor-only inline
	// rewriting would have failed.
	cAHex := cA.String()
	cB := writeObject(t, srcRepo.Storer, (&object.Commit{
		Author:    sig,
		Committer: sig,
		Message: fmt.Sprintf("branch B tip\n\nCherry-picked from %s\nsee short %s\n",
			cAHex, cAHex[:8]),
		TreeHash: treeB,
	}).Encode)

	// Discovery must see both branches so the reachable set covers cA
	// before cB is encoded.
	reachable, err := discoverReachable(t.Context(), srcRepo.Storer, []plumbing.Hash{cB, cA}, nil)
	if err != nil {
		t.Fatalf("discoverReachable: %v", err)
	}
	tr := mustTranslator(t, srcRepo.Storer, dstRepo.Storer, true, reachable)
	// Translate B first — the order that would have left the rewrite
	// stranded under the old design.
	if _, err := tr.translate(cB); err != nil {
		t.Fatalf("translate cB: %v", err)
	}
	if _, err := tr.translate(cA); err != nil {
		t.Fatalf("translate cA: %v", err)
	}

	if tr.messageRewrites != 2 {
		t.Errorf("expected 2 rewrites (full + short SHA1 of cA), got %d", tr.messageRewrites)
	}
	cBNew := tr.mapping[cB]
	cANew := tr.mapping[cA]
	if cBNew.IsZero() || cANew.IsZero() {
		t.Fatalf("missing mapping entries: cB=%s cA=%s", cBNew, cANew)
	}
	gotB, err := object.GetCommit(dstRepo.Storer, cBNew)
	if err != nil {
		t.Fatalf("read cB: %v", err)
	}
	if !strings.Contains(gotB.Message, cANew.String()) {
		t.Errorf("cB's message missing cA's SHA256:\n%s", gotB.Message)
	}
	if strings.Contains(gotB.Message, cAHex) {
		t.Errorf("cB's message still contains cA's original SHA1:\n%s", gotB.Message)
	}
}

// TestTranslator_SkipMessageRewrite confirms that with rewriteMessages
// false, the translator leaves message content (including SHA1 hashes)
// untouched.
func TestTranslator_SkipMessageRewrite(t *testing.T) {
	root := t.TempDir()
	srcRepo := initSHA1(t, filepath.Join(root, "src.git"))
	dstRepo := initSHA256(t, filepath.Join(root, "dst.git"))

	blob := writeBlob(t, srcRepo.Storer, []byte("x\n"))
	tree := writeTree(t, srcRepo.Storer, []object.TreeEntry{{Name: "f", Mode: filemode.Regular, Hash: blob}})
	sig := object.Signature{Name: "Test", Email: "t@example.com", When: time.Unix(1, 0).UTC()}
	parent := writeObject(t, srcRepo.Storer, (&object.Commit{Author: sig, Committer: sig, Message: "p\n", TreeHash: tree}).Encode)
	parentHex := parent.String()

	child := &object.Commit{
		Author: sig, Committer: sig, TreeHash: tree, ParentHashes: []plumbing.Hash{parent},
		Message: "reverts " + parentHex + "\n",
	}
	childSHA1 := writeObject(t, srcRepo.Storer, child.Encode)

	reachable, err := discoverReachable(t.Context(), srcRepo.Storer, []plumbing.Hash{childSHA1}, nil)
	if err != nil {
		t.Fatalf("discoverReachable: %v", err)
	}
	tr := mustTranslator(t, srcRepo.Storer, dstRepo.Storer, false, reachable)
	if _, err := tr.translate(childSHA1); err != nil {
		t.Fatalf("translate: %v", err)
	}
	if tr.messageRewrites != 0 {
		t.Errorf("expected no rewrites when disabled; got %d", tr.messageRewrites)
	}
	got, err := object.GetCommit(dstRepo.Storer, tr.mapping[childSHA1])
	if err != nil {
		t.Fatalf("read translated child: %v", err)
	}
	if !strings.Contains(got.Message, parentHex) {
		t.Errorf("rewrite-disabled run still mutated the message: %q", got.Message)
	}
}

// TestTranslator_WriteOriginNotes builds a small history and verifies that
// the notes tree contains one entry per translated commit and that each
// entry resolves to a blob whose content is the commit's original SHA1.
func TestTranslator_WriteOriginNotes(t *testing.T) {
	root := t.TempDir()
	srcRepo := initSHA1(t, filepath.Join(root, "src.git"))
	dstRepo := initSHA256(t, filepath.Join(root, "dst.git"))

	blob := writeBlob(t, srcRepo.Storer, []byte("hi\n"))
	tree := writeTree(t, srcRepo.Storer, []object.TreeEntry{{Name: "f", Mode: filemode.Regular, Hash: blob}})
	sig := object.Signature{Name: "Test", Email: "t@example.com", When: time.Unix(1700000000, 0).UTC()}
	c1 := writeObject(t, srcRepo.Storer, (&object.Commit{Author: sig, Committer: sig, Message: "c1\n", TreeHash: tree}).Encode)
	c2 := writeObject(t, srcRepo.Storer, (&object.Commit{Author: sig, Committer: sig, Message: "c2\n", TreeHash: tree, ParentHashes: []plumbing.Hash{c1}}).Encode)

	reachable, err := discoverReachable(t.Context(), srcRepo.Storer, []plumbing.Hash{c2}, nil)
	if err != nil {
		t.Fatalf("discoverReachable: %v", err)
	}
	tr := mustTranslator(t, srcRepo.Storer, dstRepo.Storer, false, reachable)
	if _, err := tr.translate(c2); err != nil {
		t.Fatalf("translate: %v", err)
	}

	refName, err := tr.writeOriginNotes(originNotesRef)
	if err != nil {
		t.Fatalf("writeOriginNotes: %v", err)
	}
	if refName != originNotesRef {
		t.Errorf("ref name: got %q, want %q", refName, originNotesRef)
	}
	notesCommit, err := object.GetCommit(dstRepo.Storer, tr.lastNotesCommit)
	if err != nil {
		t.Fatalf("read notes commit: %v", err)
	}
	notesTree, err := notesCommit.Tree()
	if err != nil {
		t.Fatalf("read notes tree: %v", err)
	}
	if len(notesTree.Entries) != 2 {
		t.Fatalf("notes entries: got %d, want 2", len(notesTree.Entries))
	}
	for _, mapped := range []plumbing.Hash{tr.mapping[c1], tr.mapping[c2]} {
		entry, err := notesTree.FindEntry(mapped.String())
		if err != nil {
			t.Fatalf("no notes entry for %s: %v", mapped, err)
		}
		blob, err := object.GetBlob(dstRepo.Storer, entry.Hash)
		if err != nil {
			t.Fatalf("read note blob: %v", err)
		}
		reader, err := blob.Reader()
		if err != nil {
			t.Fatalf("open note blob: %v", err)
		}
		buf, err := io.ReadAll(reader)
		if err != nil {
			_ = reader.Close()
			t.Fatalf("read note blob: %v", err)
		}
		_ = reader.Close()
		got := strings.TrimSpace(string(buf))
		var origSHA1 plumbing.Hash
		for s, n := range tr.mapping {
			if n == mapped {
				origSHA1 = s
				break
			}
		}
		if got != origSHA1.String() {
			t.Errorf("note for %s: got %q, want %q", mapped, got, origSHA1.String())
		}
	}
}

// TestTranslator_WriteMappingFile checks the sidecar TSV format: header
// line, sorted by SHA1, one entry per translated object.
func TestTranslator_WriteMappingFile(t *testing.T) {
	root := t.TempDir()
	srcRepo := initSHA1(t, filepath.Join(root, "src.git"))
	dstRepo := initSHA256(t, filepath.Join(root, "dst.git"))

	blob := writeBlob(t, srcRepo.Storer, []byte("hi\n"))
	tree := writeTree(t, srcRepo.Storer, []object.TreeEntry{{Name: "f", Mode: filemode.Regular, Hash: blob}})
	sig := object.Signature{Name: "Test", Email: "t@example.com", When: time.Unix(1700000000, 0).UTC()}
	commit := writeObject(t, srcRepo.Storer, (&object.Commit{Author: sig, Committer: sig, Message: "c\n", TreeHash: tree}).Encode)

	reachable, err := discoverReachable(t.Context(), srcRepo.Storer, []plumbing.Hash{commit}, nil)
	if err != nil {
		t.Fatalf("discoverReachable: %v", err)
	}
	tr := mustTranslator(t, srcRepo.Storer, dstRepo.Storer, false, reachable)
	if _, err := tr.translate(commit); err != nil {
		t.Fatalf("translate: %v", err)
	}

	path := filepath.Join(root, "mapping.tsv")
	if err := tr.writeMappingFile(path); err != nil {
		t.Fatalf("writeMappingFile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read mapping: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if !strings.HasPrefix(lines[0], "#") {
		t.Errorf("first line should be a header comment, got %q", lines[0])
	}
	data := lines[1:]
	if len(data) != len(tr.mapping) {
		t.Errorf("mapping line count: got %d, want %d", len(data), len(tr.mapping))
	}
	// Sorted by SHA1.
	for i := 1; i < len(data); i++ {
		prev := strings.Split(data[i-1], "\t")[0]
		cur := strings.Split(data[i], "\t")[0]
		if prev >= cur {
			t.Errorf("mapping not sorted: %q >= %q", prev, cur)
		}
	}
	// Every translated hash present.
	mapped := map[string]string{}
	for _, line := range data {
		parts := strings.Split(line, "\t")
		if len(parts) != 2 {
			t.Errorf("malformed line %q", line)
			continue
		}
		mapped[parts[0]] = parts[1]
	}
	for old, newH := range tr.mapping {
		if mapped[old.String()] != newH.String() {
			t.Errorf("missing or wrong mapping for %s: got %q, want %s", old, mapped[old.String()], newH)
		}
	}
}

// TestTranslator_AmbiguousMessageRefWarning verifies that when an
// abbreviated SHA1 prefix in a commit message matches more than one
// in-scope commit, the prefix is left unrewritten and recorded in
// t.ambiguousMessageRefs so the caller can surface a warning.
//
// We can't easily force a real SHA1 prefix collision in a test, so
// we install two synthetic entries in the reachable map after the
// translator is constructed and then run rewriteHashesInMessage
// directly. This exercises the same code path the production
// pipeline takes.
func TestTranslator_AmbiguousMessageRefWarning(t *testing.T) {
	root := t.TempDir()
	srcRepo := initSHA1(t, filepath.Join(root, "src.git"))
	dstRepo := initSHA256(t, filepath.Join(root, "dst.git"))
	tr := mustTranslator(t, srcRepo.Storer, dstRepo.Storer, true, nil)

	// Two real-looking SHA1 hashes that share the prefix "deadbee".
	one := plumbing.NewHash("deadbee100000000000000000000000000000001")
	two := plumbing.NewHash("deadbee200000000000000000000000000000002")
	tr.reachable[one] = plumbing.CommitObject
	tr.reachable[two] = plumbing.CommitObject

	out, count, err := tr.rewriteMessageRefs("see commit deadbee for details\n", "commit", plumbing.ZeroHash)
	if err != nil {
		t.Fatalf("rewriteMessageRefs: %v", err)
	}
	if count != 0 {
		t.Errorf("ambiguous prefix should not be rewritten; got count=%d", count)
	}
	if !strings.Contains(out, "deadbee") {
		t.Errorf("ambiguous prefix should be left in message; got %q", out)
	}
	if _, recorded := tr.ambiguousMessageRefs["deadbee"]; !recorded {
		t.Errorf("expected %q to be recorded in ambiguousMessageRefs, got %v",
			"deadbee", tr.ambiguousMessageRefs)
	}
}

// TestTranslator_UnresolvableSubmodule confirms that a tree entry with
// Submodule mode pointing at a commit not in the source repo is
// rejected during discovery (fail-fast), before any object is written
// to the target.
func TestTranslator_UnresolvableSubmodule(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src.git")

	srcRepo, err := git.PlainInit(srcDir, true)
	if err != nil {
		t.Fatalf("init SHA1 source: %v", err)
	}

	blobHash := writeBlob(t, srcRepo.Storer, []byte("contents\n"))
	// External-looking SHA1 — not in source.
	external := plumbing.NewHash("0123456789abcdef0123456789abcdef01234567")
	treeHash := writeTree(t, srcRepo.Storer, []object.TreeEntry{
		{Name: "file", Mode: filemode.Regular, Hash: blobHash},
		{Name: "sub", Mode: filemode.Submodule, Hash: external},
	})

	_, err = discoverReachable(t.Context(), srcRepo.Storer, []plumbing.Hash{treeHash}, nil)
	if err == nil {
		t.Fatal("expected discoverReachable to fail on unresolvable submodule, got nil")
	}
	if !strings.Contains(err.Error(), "submodule") {
		t.Errorf("error should mention submodule; got: %v", err)
	}
}

// TestTranslator_VendoredSubmoduleStillRefused locks in the rule that
// even a submodule whose commit happens to live in the source store is
// rejected. The earlier "vendored" carve-out rewrote such gitlinks to
// SHA256, but .gitmodules still points at an upstream SHA1 repo, so
// `git submodule update` would fail in clones of the converted repo.
func TestTranslator_VendoredSubmoduleStillRefused(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src.git")

	srcRepo, err := git.PlainInit(srcDir, true)
	if err != nil {
		t.Fatalf("init SHA1 source: %v", err)
	}

	// Create a commit that lives in this source store, then point a
	// tree's submodule gitlink at it. discoverReachable used to recurse
	// into that commit ("vendored") and translate the gitlink; now it
	// refuses regardless.
	blobHash := writeBlob(t, srcRepo.Storer, []byte("inner\n"))
	innerTree := writeTree(t, srcRepo.Storer, []object.TreeEntry{
		{Name: "f", Mode: filemode.Regular, Hash: blobHash},
	})
	sig := object.Signature{Name: "Test", Email: "t@example.com", When: time.Unix(1700000000, 0).UTC()}
	innerCommit := &object.Commit{Author: sig, Committer: sig, Message: "inner\n", TreeHash: innerTree}
	innerSHA1 := writeObject(t, srcRepo.Storer, innerCommit.Encode)

	outerTree := writeTree(t, srcRepo.Storer, []object.TreeEntry{
		{Name: "sub", Mode: filemode.Submodule, Hash: innerSHA1},
	})

	_, err = discoverReachable(t.Context(), srcRepo.Storer, []plumbing.Hash{outerTree}, nil)
	if err == nil {
		t.Fatal("expected discoverReachable to refuse vendored submodule, got nil")
	}
	if !strings.Contains(err.Error(), "submodule") {
		t.Errorf("error should mention submodule; got: %v", err)
	}
}

// --- helpers ---

// initSHA1 and initSHA256 are t.Fatalf-wrapping `git.PlainInit` shortcuts
// used to keep test bodies focused on the translator logic rather than
// error-handling boilerplate.
func initSHA1(t *testing.T, path string) *git.Repository {
	t.Helper()
	r, err := git.PlainInit(path, true)
	if err != nil {
		t.Fatalf("init SHA1 source at %s: %v", path, err)
	}
	return r
}

func initSHA256(t *testing.T, path string) *git.Repository {
	t.Helper()
	r, err := git.PlainInit(path, true, git.WithObjectFormat(formatcfg.SHA256))
	if err != nil {
		t.Fatalf("init SHA256 target at %s: %v", path, err)
	}
	return r
}

func mustTranslator(t *testing.T, src, dst gogitstorer.Storer, rewrite bool, reachable map[plumbing.Hash]plumbing.ObjectType) *translator {
	t.Helper()
	tr, err := newTranslator(t.Context(), src, dst, rewrite, reachable)
	if err != nil {
		t.Fatalf("newTranslator: %v", err)
	}
	return tr
}

func writeBlob(t *testing.T, storer interface {
	NewEncodedObject() plumbing.EncodedObject
	SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error)
}, content []byte) plumbing.Hash {
	t.Helper()
	obj := storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(content)))
	w, err := obj.Writer()
	if err != nil {
		t.Fatalf("blob writer: %v", err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatalf("blob write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("blob close: %v", err)
	}
	h, err := storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	return h
}

func writeTree(t *testing.T, storer interface {
	NewEncodedObject() plumbing.EncodedObject
	SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error)
}, entries []object.TreeEntry) plumbing.Hash {
	t.Helper()
	tree := &object.Tree{Entries: entries}
	// object.Tree.Encode requires the slice to be sorted by name; tests
	// pre-sort their entries, but be safe.
	return writeObject(t, storer, tree.Encode)
}

func writeObject(t *testing.T, storer interface {
	NewEncodedObject() plumbing.EncodedObject
	SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error)
}, encode func(plumbing.EncodedObject) error) plumbing.Hash {
	t.Helper()
	obj := storer.NewEncodedObject()
	if err := encode(obj); err != nil {
		t.Fatalf("encode: %v", err)
	}
	h, err := storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return h
}

// assertLooseObjectHashMatches reads the on-disk loose object for h, zlib-
// decompresses it, and confirms sha256(decompressed bytes) == h. The
// decompressed bytes include the "<type> <size>\x00" header, which is what
// git hashes — so this is a direct check on the loose writer's correctness.
func assertLooseObjectHashMatches(t *testing.T, objectsDir string, h plumbing.Hash) {
	t.Helper()
	hex := h.String()
	if len(hex) != 64 {
		t.Errorf("hash %s is not 64 hex chars (sha256)", hex)
		return
	}
	path := filepath.Join(objectsDir, hex[:2], hex[2:])
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("zlib %s: %v", path, err)
	}
	defer zr.Close()
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompress %s: %v", path, err)
	}
	sum := sha256.Sum256(plain)
	got := makeHex(sum[:])
	if got != hex {
		t.Errorf("loose object %s: sha256(content) = %s; filename and content disagree", hex, got)
	}
}

func makeHex(b []byte) string {
	return hex.EncodeToString(b)
}

// --- Integration test (gated) ---

const gitHTTPBackendEnv = "GITSYNC_E2E_SHA256_HTTP_BACKEND"

// TestRun_GitHTTPBackend exercises the full convert-sha256 pipeline against
// a local git http-backend serving a real SHA1 source repo. Gated like the
// other end-to-end git-http-backend tests to keep the default test runs
// hermetic (no external binaries required).
func TestRun_GitHTTPBackend(t *testing.T) {
	if os.Getenv(gitHTTPBackendEnv) == "" {
		t.Skipf("set %s=1 to run the convert-sha256 git-http-backend integration test", gitHTTPBackendEnv)
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git binary not available: %v", err)
	}

	root := t.TempDir()
	srcBare := filepath.Join(root, "source.git")
	worktree := filepath.Join(root, "work")
	dstDir := filepath.Join(root, "target.git")

	mustGit(t, root, "init", "--bare", srcBare)
	mustGit(t, root, "init", "-b", "main", worktree)
	mustGit(t, worktree, "config", "user.name", "convert-sha256 test")
	mustGit(t, worktree, "config", "user.email", "test@example.com")
	mustWrite(t, filepath.Join(worktree, "README"), "hello\n")
	mustGit(t, worktree, "add", "README")
	mustGit(t, worktree, "commit", "-m", "initial")
	// Capture the first commit's SHA1 so the second commit's message can
	// reference it (both full and abbreviated). The conversion should
	// rewrite both to the new SHA256 hash.
	firstSHA1 := strings.TrimSpace(mustGitOutput(t, worktree, "rev-parse", "HEAD"))
	mustWrite(t, filepath.Join(worktree, "second.txt"), "world\n")
	mustGit(t, worktree, "add", "second.txt")
	mustGit(t, worktree, "commit", "-m",
		fmt.Sprintf("second\n\nreverts %s\nsee short %s", firstSHA1, firstSHA1[:7]))
	mustGit(t, worktree, "tag", "-a", "v1", "-m", "first tag")
	mustGit(t, worktree, "remote", "add", "origin", srcBare)
	mustGit(t, worktree, "push", "origin", "HEAD:refs/heads/main")
	mustGit(t, worktree, "push", "origin", "v1")

	srv := newCGIBackend(t, gitBin, root)
	defer srv.Close()

	mappingPath := filepath.Join(root, "mapping.tsv")
	res, err := Run(context.Background(), Request{
		SourceURL:   srv.URL + "/source.git",
		TargetDir:   dstDir,
		MappingFile: mappingPath,
		Check:       true,
		Out:         io.Discard,
	})
	if err != nil {
		t.Fatalf("convert-sha256 run: %v", err)
	}
	if res.Counts.Commits < 2 {
		t.Errorf("expected at least 2 commits converted, got %+v", res.Counts)
	}
	if res.Counts.Tags != 1 {
		t.Errorf("expected 1 tag converted, got %d", res.Counts.Tags)
	}
	if res.RefsConverted < 2 {
		t.Errorf("expected at least 2 refs (main + v1), got %d", res.RefsConverted)
	}

	// The converted repo must be self-consistent under SHA256.
	fsckOut, err := exec.CommandContext(t.Context(), gitBin, "-C", dstDir, "fsck", "--full").CombinedOutput()
	if err != nil {
		t.Fatalf("git fsck failed: %v\n%s", err, fsckOut)
	}
	if strings.Contains(string(fsckOut), "error") || strings.Contains(string(fsckOut), "bad sha") {
		t.Fatalf("git fsck reported errors:\n%s", fsckOut)
	}

	// Sanity: extensions.objectformat is set, and git can walk the history.
	format := mustGitOutput(t, dstDir, "config", "extensions.objectformat")
	if strings.TrimSpace(format) != "sha256" {
		t.Errorf("extensions.objectformat: got %q, want %q", strings.TrimSpace(format), "sha256")
	}
	log := mustGitOutput(t, dstDir, "log", "--oneline", "refs/heads/main")
	if !strings.Contains(log, "initial") || !strings.Contains(log, "second") {
		t.Errorf("git log missing expected commit subjects:\n%s", log)
	}
	tagShow := mustGitOutput(t, dstDir, "cat-file", "-p", "refs/tags/v1")
	if !strings.Contains(tagShow, "first tag") {
		t.Errorf("annotated tag did not round-trip:\n%s", tagShow)
	}

	// Message rewriting: the second commit's body referenced firstSHA1
	// twice (full + 7-char short). Both should now be SHA256 hashes.
	if res.MessageRewrites != 2 {
		t.Errorf("message rewrites: got %d, want 2", res.MessageRewrites)
	}
	secondMsg := mustGitOutput(t, dstDir, "log", "-1", "--format=%B", "refs/heads/main")
	if strings.Contains(secondMsg, firstSHA1) {
		t.Errorf("second commit message still contains the original SHA1:\n%s", secondMsg)
	}

	// Origin notes: the ref exists, and the head commit's note resolves
	// to the original SHA1 it was rewritten from.
	if res.OriginNotesRef != "refs/notes/sha1-origin" {
		t.Errorf("OriginNotesRef: got %q, want refs/notes/sha1-origin", res.OriginNotesRef)
	}
	headSHA256 := strings.TrimSpace(mustGitOutput(t, dstDir, "rev-parse", "refs/heads/main"))
	note := strings.TrimSpace(mustGitOutput(t, dstDir, "notes", "--ref=sha1-origin", "show", headSHA256))
	// The note for the second (head) commit holds its pre-conversion SHA1.
	headSHA1 := strings.TrimSpace(mustGitOutput(t, srcBare, "rev-parse", "refs/heads/main"))
	if note != headSHA1 {
		t.Errorf("origin note for head: got %q, want %q", note, headSHA1)
	}

	// Mapping file: present, sorted, has at least one entry per
	// translated commit/tree/blob/tag.
	if res.MappingFile != mappingPath {
		t.Errorf("MappingFile: got %q, want %q", res.MappingFile, mappingPath)
	}
	mapping, err := os.ReadFile(mappingPath)
	if err != nil {
		t.Fatalf("read mapping file: %v", err)
	}
	if !strings.Contains(string(mapping), headSHA1) {
		t.Errorf("mapping file missing head SHA1 %s:\n%s", headSHA1, mapping)
	}

	// --check: every step should pass against a freshly-converted repo,
	// including git fsck --full (available since we already need the
	// git binary to drive the source side of this test).
	if len(res.Checks) == 0 {
		t.Fatal("expected Checks to be populated when --check is enabled")
	}
	for _, c := range res.Checks {
		if !c.OK {
			t.Errorf("check %q failed: %s", c.Name, c.Detail)
		}
	}
	expected := map[string]bool{"config": false, "HEAD": false, "refs": false, "git fsck --full": false}
	for _, c := range res.Checks {
		expected[c.Name] = true
	}
	for name, present := range expected {
		if !present {
			t.Errorf("--check did not run %q step", name)
		}
	}
}

// TestRun_GitHTTPBackend_KeepsTargetOnPostConversionFailure guards the
// invariant that a failure in an optional post-conversion step (here, an
// unwritable --write-mapping path) leaves the fully-converted repo on disk
// rather than deleting it. The target is complete and valid once refs and
// HEAD are written; only the mapping side output failed. Regression test for
// the cleanup-target lifecycle: cleanup must be disarmed once the conversion
// completes, so a path typo or a signing misconfig can't silently discard a
// (potentially multi-hour) conversion.
func TestRun_GitHTTPBackend_KeepsTargetOnPostConversionFailure(t *testing.T) {
	if os.Getenv(gitHTTPBackendEnv) == "" {
		t.Skipf("set %s=1 to run the convert-sha256 git-http-backend integration test", gitHTTPBackendEnv)
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git binary not available: %v", err)
	}

	root := t.TempDir()
	srcBare := filepath.Join(root, "source.git")
	worktree := filepath.Join(root, "work")
	dstDir := filepath.Join(root, "target.git")

	mustGit(t, root, "init", "--bare", srcBare)
	mustGit(t, root, "init", "-b", "main", worktree)
	mustGit(t, worktree, "config", "user.name", "convert-sha256 test")
	mustGit(t, worktree, "config", "user.email", "test@example.com")
	mustWrite(t, filepath.Join(worktree, "README"), "hello\n")
	mustGit(t, worktree, "add", "README")
	mustGit(t, worktree, "commit", "-m", "initial")
	mustGit(t, worktree, "remote", "add", "origin", srcBare)
	mustGit(t, worktree, "push", "origin", "HEAD:refs/heads/main")

	srv := newCGIBackend(t, gitBin, root)
	defer srv.Close()

	// Force the mapping-file write to fail: a regular file standing in for a
	// directory component makes os.MkdirAll return ENOTDIR. This fires after
	// the conversion is otherwise complete (refs + HEAD written).
	blocker := filepath.Join(root, "blocker")
	mustWrite(t, blocker, "not a dir\n")
	mappingPath := filepath.Join(blocker, "mapping.tsv")

	_, err = Run(context.Background(), Request{
		SourceURL:   srv.URL + "/source.git",
		TargetDir:   dstDir,
		MappingFile: mappingPath,
		Out:         io.Discard,
	})
	if err == nil {
		t.Fatal("expected an error from the unwritable --write-mapping path, got nil")
	}
	if !strings.Contains(err.Error(), "mapping file") {
		t.Errorf("error should name the failing step; got: %v", err)
	}

	// The fix: the converted repo must still be on disk and self-consistent,
	// not deleted by the post-conversion failure.
	if _, statErr := os.Stat(dstDir); statErr != nil {
		t.Fatalf("converted target was deleted after a post-conversion failure: %v", statErr)
	}
	format := strings.TrimSpace(mustGitOutput(t, dstDir, "config", "extensions.objectformat"))
	if format != "sha256" {
		t.Errorf("extensions.objectformat: got %q, want sha256", format)
	}
	fsckOut, fsckErr := exec.CommandContext(t.Context(), gitBin, "-C", dstDir, "fsck", "--full").CombinedOutput()
	if fsckErr != nil {
		t.Fatalf("git fsck on kept target failed: %v\n%s", fsckErr, fsckOut)
	}
	if strings.Contains(string(fsckOut), "error") || strings.Contains(string(fsckOut), "bad sha") {
		t.Fatalf("git fsck reported errors on kept target:\n%s", fsckOut)
	}
}

// TestRun_GitHTTPBackend_Sign verifies the --sign-mode tips path end-to-end. SSH
// signing is used (not GPG) because it can be set up from scratch in the
// test with just ssh-keygen, no agent required.
func TestRun_GitHTTPBackend_Sign(t *testing.T) {
	if os.Getenv(gitHTTPBackendEnv) == "" {
		t.Skipf("set %s=1 to run the convert-sha256 git-http-backend integration test", gitHTTPBackendEnv)
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git binary not available: %v", err)
	}
	sshKeygenBin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skipf("ssh-keygen not available: %v", err)
	}

	root := t.TempDir()
	srcBare := filepath.Join(root, "source.git")
	worktree := filepath.Join(root, "work")
	dstDir := filepath.Join(root, "target.git")

	mustGit(t, root, "init", "--bare", srcBare)
	mustGit(t, root, "init", "-b", "main", worktree)
	mustGit(t, worktree, "config", "user.name", "convert-sha256 test")
	mustGit(t, worktree, "config", "user.email", "test@example.com")
	mustWrite(t, filepath.Join(worktree, "README"), "hello\n")
	mustGit(t, worktree, "add", "README")
	mustGit(t, worktree, "commit", "-m", "initial")
	mustGit(t, worktree, "remote", "add", "origin", srcBare)
	mustGit(t, worktree, "push", "origin", "HEAD:refs/heads/main")

	// Generate an ephemeral ed25519 SSH key for signing.
	keyPath := filepath.Join(root, "signkey")
	keygen := exec.CommandContext(t.Context(), sshKeygenBin, "-q", "-t", "ed25519", "-N", "", "-f", keyPath, "-C", "test@example.com")
	if out, err := keygen.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}

	// Write a global gitconfig that points git at SSH signing using the
	// ephemeral key, and route GIT_CONFIG_GLOBAL at it so signBranchTips'
	// subprocess inherits the config.
	globalCfg := filepath.Join(root, "global.gitconfig")
	if err := os.WriteFile(globalCfg, []byte(fmt.Sprintf(`
[user]
	name = Conversion Test
	email = test@example.com
	signingkey = %s
[gpg]
	format = ssh
`, keyPath)), 0o600); err != nil {
		t.Fatalf("write global gitconfig: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	// Disable any system gitconfig so the test isn't influenced by host
	// signing config.
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	srv := newCGIBackend(t, gitBin, root)
	defer srv.Close()

	res, err := Run(context.Background(), Request{
		SourceURL: srv.URL + "/source.git",
		TargetDir: dstDir,
		SignMode:  SignModeTips,
		Out:       io.Discard,
	})
	if err != nil {
		t.Fatalf("convert-sha256 run: %v", err)
	}

	wantTag := "refs/tags/converted/main"
	if len(res.SignedTags) != 1 || res.SignedTags[0] != wantTag {
		t.Errorf("SignedTags: got %v, want [%s]", res.SignedTags, wantTag)
	}

	// The tag exists in the target and is an annotated, signed tag (the
	// body contains a SSH SIGNATURE block; cat-file -p shows the tag
	// object including the signature).
	tagShow := mustGitOutput(t, dstDir, "cat-file", "-p", wantTag)
	if !strings.Contains(tagShow, "BEGIN SSH SIGNATURE") {
		t.Errorf("expected signed tag to contain an SSH SIGNATURE block:\n%s", tagShow)
	}
	if !strings.Contains(tagShow, "SHA1 → SHA256 conversion attestation") {
		t.Errorf("expected signed tag message to contain attestation text:\n%s", tagShow)
	}

	// Tag's target should be the branch tip (the SHA256 hash of the
	// converted main).
	mainTip := strings.TrimSpace(mustGitOutput(t, dstDir, "rev-parse", "refs/heads/main"))
	tagTarget := strings.TrimSpace(mustGitOutput(t, dstDir, "rev-list", "-n", "1", wantTag))
	if tagTarget != mainTip {
		t.Errorf("signed tag target: got %s, want %s (main tip)", tagTarget, mainTip)
	}
}

// TestRun_GitHTTPBackend_PullRefs verifies that --all-refs excludes
// pull/merge-request namespaces by default — including the foreign commit
// that only a pull ref makes reachable — and that --include-pull-refs opts
// back in.
func TestRun_GitHTTPBackend_PullRefs(t *testing.T) {
	if os.Getenv(gitHTTPBackendEnv) == "" {
		t.Skipf("set %s=1 to run the convert-sha256 git-http-backend integration test", gitHTTPBackendEnv)
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git binary not available: %v", err)
	}

	root := t.TempDir()
	srcBare := filepath.Join(root, "source.git")
	worktree := filepath.Join(root, "work")

	mustGit(t, root, "init", "--bare", srcBare)
	mustGit(t, root, "init", "-b", "main", worktree)
	mustGit(t, worktree, "config", "user.name", "convert-sha256 test")
	mustGit(t, worktree, "config", "user.email", "test@example.com")
	mustWrite(t, filepath.Join(worktree, "README"), "hello\n")
	mustGit(t, worktree, "add", "README")
	mustGit(t, worktree, "commit", "-m", "initial")
	mustGit(t, worktree, "remote", "add", "origin", srcBare)
	mustGit(t, worktree, "push", "origin", "HEAD:refs/heads/main")
	// A server-internal PR ref whose tip is foreign: the commit is on no
	// branch, so only refs/pull/1/head makes it reachable.
	mustWrite(t, filepath.Join(worktree, "evil.txt"), "foreign\n")
	mustGit(t, worktree, "add", "evil.txt")
	mustGit(t, worktree, "commit", "-m", "foreign PR commit")
	mustGit(t, worktree, "push", "origin", "HEAD:refs/pull/1/head")

	srv := newCGIBackend(t, gitBin, root)
	defer srv.Close()

	// Default: --all-refs must drop refs/pull/* and report the count.
	defaultDir := filepath.Join(root, "default.git")
	res, err := Run(context.Background(), Request{
		SourceURL: srv.URL + "/source.git",
		TargetDir: defaultDir,
		AllRefs:   true,
		Out:       io.Discard,
	})
	if err != nil {
		t.Fatalf("convert (default): %v", err)
	}
	if res.SkippedPullRefs != 1 {
		t.Errorf("SkippedPullRefs (default): got %d, want 1", res.SkippedPullRefs)
	}
	if refs := mustGitOutput(t, defaultDir, "for-each-ref", "--format=%(refname)"); strings.Contains(refs, "refs/pull/") {
		t.Errorf("default --all-refs conversion leaked a pull ref:\n%s", refs)
	}

	// Opt-in: --include-pull-refs converts refs/pull/* and the foreign tip.
	inclDir := filepath.Join(root, "incl.git")
	res2, err := Run(context.Background(), Request{
		SourceURL:       srv.URL + "/source.git",
		TargetDir:       inclDir,
		AllRefs:         true,
		IncludePullRefs: true,
		Out:             io.Discard,
	})
	if err != nil {
		t.Fatalf("convert (include): %v", err)
	}
	if res2.SkippedPullRefs != 0 {
		t.Errorf("SkippedPullRefs (--include-pull-refs): got %d, want 0", res2.SkippedPullRefs)
	}
	if refs := mustGitOutput(t, inclDir, "for-each-ref", "--format=%(refname)"); !strings.Contains(refs, "refs/pull/1/head") {
		t.Errorf("--include-pull-refs did not convert the pull ref:\n%s", refs)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func mustGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

type cgiBackend struct {
	*httptest.Server
}

func newCGIBackend(t *testing.T, gitBin, root string) *cgiBackend {
	t.Helper()
	handler := &cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	return &cgiBackend{Server: srv}
}

// Compile-time sanity: confirm the storers the translator expects are still
// the filesystem-backed type that PlainInit returns. If a future go-git
// release changes the concrete storer, the type assertion in newTranslator
// will start failing in this package's tests rather than only at runtime
// against a real repo.
var _ = (*filesystem.Storage)(nil)

func TestProtectedExcludePrefixes(t *testing.T) {
	tests := []struct {
		name     string
		prefixes []string
		want     []string
	}{
		{"nil input", nil, nil},
		{"single benign namespace", []string{"refs/pull/"}, nil},
		{"multiple benign namespaces", []string{"refs/pull/", "refs/notes/", "refs/changes/"}, nil},
		{"whole branches namespace banned", []string{"refs/heads/"}, []string{"refs/heads/"}},
		{"whole tags namespace banned", []string{"refs/tags/"}, []string{"refs/tags/"}},
		{"branch sub-namespace banned", []string{"refs/heads/feature/"}, []string{"refs/heads/feature/"}},
		{"tag sub-namespace banned", []string{"refs/tags/v1/"}, []string{"refs/tags/v1/"}},
		{"refs/ banned because it would drop everything", []string{"refs/"}, []string{"refs/"}},
		{"empty string banned (would drop every ref)", []string{""}, []string{""}},
		{"partial refs/h banned (covers refs/heads/)", []string{"refs/h"}, []string{"refs/h"}},
		{"mixed input reports only the bad ones, in order", []string{"refs/pull/", "refs/heads/", "refs/notes/", "refs/tags/v1.0"}, []string{"refs/heads/", "refs/tags/v1.0"}},
		{"duplicates collapsed", []string{"refs/heads/", "refs/heads/"}, []string{"refs/heads/"}},
		{"trims whitespace before matching", []string{"  refs/heads/  "}, []string{"  refs/heads/  "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := protectedExcludePrefixes(tt.prefixes)
			if len(got) != len(tt.want) {
				t.Fatalf("protectedExcludePrefixes(%v) = %v, want %v", tt.prefixes, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("protectedExcludePrefixes(%v)[%d] = %q, want %q", tt.prefixes, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestRedactSourceURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no credentials", "https://github.com/o/r.git", "https://github.com/o/r.git"},
		{"user and password", "https://user:secret@github.com/o/r.git", "https://github.com/o/r.git"},
		// Token-as-username: url.URL.Redacted() would leave this intact
		// (no password component), so we must strip the whole userinfo.
		{"token as username", "https://ghp_abc123@github.com/o/r.git", "https://github.com/o/r.git"},
		{"x-access-token form", "https://x-access-token:ghp_abc@github.com/o/r.git", "https://github.com/o/r.git"},
		// Unparseable input must not echo whatever credentials it carried.
		{"unparseable", "https://tok@ho%zz/r.git", "<source url redacted>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactSourceURL(tt.in); got != tt.want {
				t.Errorf("redactSourceURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestEffectiveExcludePrefixes(t *testing.T) {
	user := []string{"refs/changes/"}
	tests := []struct {
		name             string
		allRefs          bool
		includePullRefs  bool
		wantPullExcluded bool
	}{
		{"without --all-refs the pull namespaces are not added", false, false, false},
		{"--all-refs excludes pull namespaces by default", true, false, true},
		{"--all-refs --include-pull-refs keeps them in scope", true, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := effectiveExcludePrefixes(user, tt.allRefs, tt.includePullRefs)
			// The user's own prefixes are always preserved.
			if len(got) == 0 || got[0] != "refs/changes/" {
				t.Fatalf("user prefixes not preserved: %v", got)
			}
			hasPull := false
			for _, p := range got {
				if p == "refs/pull/" {
					hasPull = true
				}
			}
			if hasPull != tt.wantPullExcluded {
				t.Errorf("refs/pull/ excluded = %v, want %v (got %v)", hasPull, tt.wantPullExcluded, got)
			}
		})
	}
}

func TestCountForeignPullRefs(t *testing.T) {
	refs := map[plumbing.ReferenceName]plumbing.Hash{
		"refs/heads/main":            plumbing.NewHash("aaaa000000000000000000000000000000000001"),
		"refs/pull/1/head":           plumbing.NewHash("aaaa000000000000000000000000000000000002"),
		"refs/pull/2/head":           plumbing.NewHash("aaaa000000000000000000000000000000000003"),
		"refs/merge-requests/9/head": plumbing.NewHash("aaaa000000000000000000000000000000000004"),
		"refs/pull-requests/5/from":  plumbing.NewHash("aaaa000000000000000000000000000000000005"),
		"refs/tags/v1":               plumbing.NewHash("aaaa000000000000000000000000000000000006"),
	}
	if got, want := countForeignPullRefs(refs), 4; got != want {
		t.Errorf("countForeignPullRefs = %d, want %d", got, want)
	}
}

// TestEnsureEmptyTarget covers the three cases the failure-cleanup logic
// depends on: an absent directory is created (created=true), a
// pre-existing empty directory is accepted without claiming creation
// (created=false), and a non-empty directory is refused.
func TestEnsureEmptyTarget(t *testing.T) {
	t.Run("absent dir is created", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "out")
		created, err := ensureEmptyTarget(path)
		if err != nil {
			t.Fatalf("ensureEmptyTarget: %v", err)
		}
		if !created {
			t.Errorf("created = false, want true for an absent directory")
		}
		if info, statErr := os.Stat(path); statErr != nil || !info.IsDir() {
			t.Errorf("expected %s to be created as a directory; stat err=%v", path, statErr)
		}
	})

	t.Run("pre-existing empty dir is accepted, not claimed as created", func(t *testing.T) {
		path := t.TempDir() // already exists and is empty
		created, err := ensureEmptyTarget(path)
		if err != nil {
			t.Fatalf("ensureEmptyTarget: %v", err)
		}
		if created {
			t.Errorf("created = true, want false for a pre-existing directory")
		}
	})

	t.Run("non-empty dir is refused", func(t *testing.T) {
		path := t.TempDir()
		if err := os.WriteFile(filepath.Join(path, "f"), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed file: %v", err)
		}
		if _, err := ensureEmptyTarget(path); err == nil {
			t.Errorf("expected refusal of a non-empty directory, got nil")
		}
	})
}

// TestCleanupConvertedTarget locks in the invariant the failure path
// claims: a directory the run created is removed outright, but a
// directory the user pre-created is restored to empty — never deleted —
// so a mountpoint or a dir with user-set ownership/ACLs survives a failed
// conversion.
func TestCleanupConvertedTarget(t *testing.T) {
	t.Run("run-created dir is removed outright", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "out")
		if err := os.MkdirAll(filepath.Join(path, "objects"), 0o755); err != nil {
			t.Fatalf("seed: %v", err)
		}
		cleanupConvertedTarget(path, true)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed; stat err=%v", path, err)
		}
	})

	t.Run("pre-existing dir is emptied but kept", func(t *testing.T) {
		path := t.TempDir()
		// Mimic a half-written bare repo: nested dirs plus a top-level file.
		if err := os.MkdirAll(filepath.Join(path, "objects", "pack"), 0o755); err != nil {
			t.Fatalf("seed dirs: %v", err)
		}
		if err := os.WriteFile(filepath.Join(path, "HEAD"), []byte("ref: x\n"), 0o644); err != nil {
			t.Fatalf("seed file: %v", err)
		}
		cleanupConvertedTarget(path, false)
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			t.Fatalf("pre-existing directory must survive cleanup; stat err=%v", err)
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			t.Fatalf("read dir: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("directory should be empty after cleanup, got %d entries", len(entries))
		}
	})
}

// TestPreviewJoin covers the inline-vs-truncate boundary the two Lines()
// summaries share, including the optional suffix that points at the full
// list — the bit that had drifted between the two copy-pasted blocks.
func TestPreviewJoin(t *testing.T) {
	over := []string{"a", "b", "c", "d", "e", "f", "g"} // 7 > previewMax (5)
	tests := []struct {
		name   string
		items  []string
		suffix string
		want   string
	}{
		{"empty", nil, "", ""},
		{"under the cap joins all", []string{"a", "b"}, "", "a, b"},
		{"exactly at the cap joins all", []string{"a", "b", "c", "d", "e"}, "", "a, b, c, d, e"},
		{"over the cap truncates with count", over, "", "a, b, c, d, e, ... (2 more)"},
		{"suffix lands inside the parens", over, "; full list in --json", "a, b, c, d, e, ... (2 more; full list in --json)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := previewJoin(tt.items, tt.suffix); got != tt.want {
				t.Errorf("previewJoin(%v, %q) = %q, want %q", tt.items, tt.suffix, got, tt.want)
			}
		})
	}
}

// The default pull-ref exclusions must never trip the branch/tag
// protection guard — they live outside refs/heads/ and refs/tags/.
func TestForeignPullRefPrefixes_NotProtected(t *testing.T) {
	if bad := protectedExcludePrefixes(foreignPullRefPrefixes); len(bad) != 0 {
		t.Errorf("foreign pull-ref prefixes rejected by protectedExcludePrefixes: %v", bad)
	}
}

func TestRun_RejectsExcludePrefixesThatDropBranchesOrTags(t *testing.T) {
	// We never reach the network here — the validation fires before
	// any I/O — so a non-empty target dir is the only thing the early
	// path needs.
	dst := t.TempDir()
	req := Request{
		SourceURL:          "http://example.invalid/repo.git",
		TargetDir:          filepath.Join(dst, "out"),
		ExcludeRefPrefixes: []string{"refs/pull/", "refs/heads/feature/"},
	}
	_, err := Run(t.Context(), req)
	if err == nil {
		t.Fatalf("Run accepted --exclude-ref-prefix refs/heads/feature/, expected refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, "refs/heads/feature/") {
		t.Fatalf("error did not name the offending prefix: %v", err)
	}
	if !strings.Contains(msg, "exclude-ref-prefix") {
		t.Fatalf("error did not mention the flag: %v", err)
	}
}

func TestCheckSideOutputCollision(t *testing.T) {
	mk := func(name string) planner.DesiredRef {
		ref := plumbing.ReferenceName(name)
		return planner.DesiredRef{SourceRef: ref, TargetRef: ref}
	}
	tests := []struct {
		name             string
		desired          map[plumbing.ReferenceName]planner.DesiredRef
		skipOriginNotes  bool
		sign             bool
		wantErrSubstring string
	}{
		{
			name: "no collisions accepted",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				"refs/heads/main": mk("refs/heads/main"),
				"refs/tags/v1":    mk("refs/tags/v1"),
			},
			wantErrSubstring: "",
		},
		{
			name: "origin-notes collision refused by default",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				"refs/heads/main":        mk("refs/heads/main"),
				"refs/notes/sha1-origin": mk("refs/notes/sha1-origin"),
			},
			wantErrSubstring: "refs/notes/sha1-origin",
		},
		{
			name: "origin-notes collision allowed when --no-origin-notes set",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				"refs/notes/sha1-origin": mk("refs/notes/sha1-origin"),
			},
			skipOriginNotes:  true,
			wantErrSubstring: "",
		},
		{
			name: "converted-tag collision refused only when --sign-mode tips",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				"refs/heads/main":          mk("refs/heads/main"),
				"refs/tags/converted/main": mk("refs/tags/converted/main"),
			},
			sign:             true,
			wantErrSubstring: "refs/tags/converted/main",
		},
		{
			name: "converted-tag without --sign-mode tips passes through",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				"refs/tags/converted/main": mk("refs/tags/converted/main"),
			},
			sign:             false,
			wantErrSubstring: "",
		},
		{
			name: "multiple converted-tag collisions listed in sorted order",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				"refs/tags/converted/zeta":  mk("refs/tags/converted/zeta"),
				"refs/tags/converted/alpha": mk("refs/tags/converted/alpha"),
			},
			sign:             true,
			wantErrSubstring: "refs/tags/converted/alpha, refs/tags/converted/zeta",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkSideOutputCollision(tt.desired, tt.skipOriginNotes, tt.sign)
			switch {
			case tt.wantErrSubstring == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErrSubstring != "" && err == nil:
				t.Fatalf("expected error containing %q, got nil", tt.wantErrSubstring)
			case tt.wantErrSubstring != "" && !strings.Contains(err.Error(), tt.wantErrSubstring):
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErrSubstring)
			}
		})
	}
}

// TestDiscoverReachable_HonorsCtxCancellation confirms discovery
// returns promptly when its context is canceled before it starts,
// matching the per-object check translate() already does.
func TestDiscoverReachable_HonorsCtxCancellation(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src.git")
	srcRepo, err := git.PlainInit(srcDir, true)
	if err != nil {
		t.Fatalf("init source: %v", err)
	}
	blob := writeBlob(t, srcRepo.Storer, []byte("x\n"))
	tree := writeTree(t, srcRepo.Storer, []object.TreeEntry{{Name: "f", Mode: filemode.Regular, Hash: blob}})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = discoverReachable(ctx, srcRepo.Storer, []plumbing.Hash{tree}, nil)
	if err == nil {
		t.Fatal("expected canceled ctx to surface as error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error should wrap context.Canceled; got %v", err)
	}
}

// TestRunChecks_FsckSkippedWhenGitMissing locks in the Skipped flag
// for the fsck check. Callers that gate on Check.OK alone now can't
// tell a real fsck pass from a skip; Skipped resolves the ambiguity.
func TestRunChecks_FsckSkippedWhenGitMissing(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary available; this test exercises the missing-git path via PATH override")
	}
	// Force LookPath("git") to fail by overriding PATH.
	t.Setenv("PATH", "")
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, true, git.WithObjectFormat(formatcfg.SHA256))
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	checks := runChecks(t.Context(), dir, repo, 0, nil, false)
	var fsck Check
	for _, c := range checks {
		if c.Name == "git fsck --full" {
			fsck = c
			break
		}
	}
	if fsck.Name == "" {
		t.Fatalf("fsck check missing from output")
	}
	if !fsck.Skipped {
		t.Errorf("fsck should be Skipped when git is missing, got %+v", fsck)
	}
	if !fsck.OK {
		t.Errorf("Skipped implies OK; got %+v", fsck)
	}
}

// TestHashPattern_CaseInsensitive locks in the (?i) on hashPattern —
// uppercase or mixed-case SHA1 references in messages must resolve
// against the (lowercase-canonical) reachable set.
func TestHashPattern_CaseInsensitive(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src.git")
	dstDir := filepath.Join(root, "dst.git")

	srcRepo, err := git.PlainInit(srcDir, true)
	if err != nil {
		t.Fatalf("init src: %v", err)
	}
	dstRepo, err := git.PlainInit(dstDir, true, git.WithObjectFormat(formatcfg.SHA256))
	if err != nil {
		t.Fatalf("init dst: %v", err)
	}
	blob := writeBlob(t, srcRepo.Storer, []byte("x\n"))
	tree := writeTree(t, srcRepo.Storer, []object.TreeEntry{{Name: "f", Mode: filemode.Regular, Hash: blob}})
	sig := object.Signature{Name: "T", Email: "t@example.com", When: time.Unix(1700000000, 0).UTC()}
	parent := &object.Commit{Author: sig, Committer: sig, Message: "first\n", TreeHash: tree}
	parentHash := writeObject(t, srcRepo.Storer, parent.Encode)

	// Reference the parent with an UPPERCASE full hash.
	upper := strings.ToUpper(parentHash.String())
	child := &object.Commit{
		Author: sig, Committer: sig,
		Message:      "see " + upper + " for context\n",
		TreeHash:     tree,
		ParentHashes: []plumbing.Hash{parentHash},
	}
	childHash := writeObject(t, srcRepo.Storer, child.Encode)

	reachable, err := discoverReachable(t.Context(), srcRepo.Storer, []plumbing.Hash{childHash}, nil)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	tr, err := newTranslator(t.Context(), srcRepo.Storer, dstRepo.Storer, true, reachable)
	if err != nil {
		t.Fatalf("newTranslator: %v", err)
	}
	newChild, err := tr.translate(childHash)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if tr.messageRewrites != 1 {
		t.Errorf("expected 1 rewrite (case-insensitive match), got %d", tr.messageRewrites)
	}
	c, err := object.GetCommit(dstRepo.Storer, newChild)
	if err != nil {
		t.Fatalf("read translated child: %v", err)
	}
	if strings.Contains(c.Message, upper) {
		t.Errorf("uppercase SHA1 should have been rewritten; message: %q", c.Message)
	}
}

// TestFsckHasError_HandlesLongLinesAndCase covers two fragility
// fixes: lines longer than bufio.Scanner's 64 KiB default must not be
// silently truncated (we use bytes.Split now), and the "error" /
// "fatal" prefix match must be case-insensitive so e.g. older or
// custom git builds emitting "ERROR:" still trip the check.
func TestFsckHasError_HandlesLongLinesAndCase(t *testing.T) {
	t.Run("long line still scanned", func(t *testing.T) {
		// 100 KiB of dangling-blob filler followed by an error line.
		out := append(bytes.Repeat([]byte("a"), 100*1024), []byte("\nerror: bad ref\n")...)
		if !fsckHasError(out) {
			t.Errorf("fsckHasError should detect error line after a long preceding line")
		}
	})
	t.Run("uppercase ERROR matches", func(t *testing.T) {
		if !fsckHasError([]byte("ERROR: corruption\n")) {
			t.Errorf("fsckHasError should match uppercase ERROR")
		}
	})
	t.Run("fatal without colon matches", func(t *testing.T) {
		if !fsckHasError([]byte("Fatal failure in pack\n")) {
			t.Errorf("fsckHasError should match Fatal prefix even without colon")
		}
	})
	t.Run("dangling warnings are not errors", func(t *testing.T) {
		if fsckHasError([]byte("dangling commit abc123\n")) {
			t.Errorf("dangling lines should not trip fsckHasError")
		}
	})
}

// TestResolveMessageRef_Memoizes confirms a second call for the same
// prefix doesn't re-scan reachable. We do not have a counter on the
// scan, so we test the cache by mutating reachable between calls and
// verifying the second call returns the original result. (In real
// usage, reachable is frozen — this is just a behavioral observation
// to lock in cache effectiveness.)
func TestResolveMessageRef_Memoizes(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src.git")
	dstDir := filepath.Join(root, "dst.git")
	srcRepo, err := git.PlainInit(srcDir, true)
	if err != nil {
		t.Fatalf("init src: %v", err)
	}
	dstRepo, err := git.PlainInit(dstDir, true, git.WithObjectFormat(formatcfg.SHA256))
	if err != nil {
		t.Fatalf("init dst: %v", err)
	}
	reachable := map[plumbing.Hash]plumbing.ObjectType{
		plumbing.NewHash("abc1234567890abcdef1234567890abcdef12345"): plumbing.CommitObject,
	}
	tr, err := newTranslator(t.Context(), srcRepo.Storer, dstRepo.Storer, true, reachable)
	if err != nil {
		t.Fatalf("newTranslator: %v", err)
	}

	prefix := "abc12345"
	h1, r1 := tr.resolveMessageRef(prefix)
	// Mutate reachable; if the cache works, the next call must return
	// the same answer as the first.
	for k := range tr.reachable {
		delete(tr.reachable, k)
	}
	h2, r2 := tr.resolveMessageRef(prefix)
	if h1 != h2 || r1 != r2 {
		t.Errorf("resolveMessageRef should return cached value; got first (%s, %v) vs second (%s, %v)", h1, r1, h2, r2)
	}
	if _, cached := tr.resolveCache[strings.ToLower(prefix)]; !cached {
		t.Errorf("resolveCache should contain entry for %q", prefix)
	}
}

// TestRunChecks_TagOnlyConversionSkipsHEAD locks in the rule that a
// tags-only conversion does not fail --check on HEAD. PlainInit leaves
// HEAD pointing at refs/heads/master (which won't exist), and pickHEAD
// returns "" because the desired set has no branches; runChecks must
// detect that and mark HEAD as "skipped" rather than "missing".
func TestRunChecks_TagOnlyConversionSkipsHEAD(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, true, git.WithObjectFormat(formatcfg.SHA256))
	if err != nil {
		t.Fatalf("init SHA256 target: %v", err)
	}

	checks := runChecks(t.Context(), dir, repo, 0, nil, false)
	var head Check
	for _, c := range checks {
		if c.Name == "HEAD" {
			head = c
			break
		}
	}
	if head.Name == "" {
		t.Fatalf("HEAD check missing from runChecks output")
	}
	if !head.OK {
		t.Errorf("HEAD should be OK for tags-only conversion, got %+v", head)
	}
	if !head.Skipped {
		t.Errorf("HEAD should be marked Skipped on tags-only conversion, got %+v", head)
	}
	if !strings.Contains(head.Detail, "tags-only") {
		t.Errorf("HEAD detail should explain the skip reason, got %q", head.Detail)
	}
}

func TestPickHEAD(t *testing.T) {
	branch := func(name string) planner.DesiredRef {
		ref := plumbing.ReferenceName("refs/heads/" + name)
		return planner.DesiredRef{Kind: planner.RefKindBranch, SourceRef: ref, TargetRef: ref}
	}
	tag := func(name string) planner.DesiredRef {
		ref := plumbing.ReferenceName("refs/tags/" + name)
		return planner.DesiredRef{Kind: planner.RefKindTag, SourceRef: ref, TargetRef: ref}
	}
	tests := []struct {
		name       string
		advertised plumbing.ReferenceName
		desired    map[plumbing.ReferenceName]planner.DesiredRef
		want       plumbing.ReferenceName
	}{
		{
			name:       "advertised HEAD wins when present in desired",
			advertised: "refs/heads/develop",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				"refs/heads/main":    branch("main"),
				"refs/heads/develop": branch("develop"),
			},
			want: "refs/heads/develop",
		},
		{
			name:       "advertised HEAD respects ref mapping (target side)",
			advertised: "refs/heads/source-name",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				"refs/heads/source-name": {
					Kind:      planner.RefKindBranch,
					SourceRef: "refs/heads/source-name",
					TargetRef: "refs/heads/target-name",
				},
			},
			want: "refs/heads/target-name",
		},
		{
			name:       "falls back to main when advertised HEAD missing",
			advertised: "",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				"refs/heads/main":   branch("main"),
				"refs/heads/master": branch("master"),
			},
			want: "refs/heads/main",
		},
		{
			name:       "falls back to master when no main",
			advertised: "",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				"refs/heads/master":  branch("master"),
				"refs/heads/feature": branch("feature"),
			},
			want: "refs/heads/master",
		},
		{
			name:       "falls back to first sorted branch when neither main nor master",
			advertised: "",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				"refs/heads/zeta":  branch("zeta"),
				"refs/heads/alpha": branch("alpha"),
				"refs/heads/beta":  branch("beta"),
			},
			want: "refs/heads/alpha",
		},
		{
			name:       "advertised HEAD pointing outside desired falls back to convention",
			advertised: "refs/heads/dropped",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				"refs/heads/main": branch("main"),
			},
			want: "refs/heads/main",
		},
		{
			name:       "tags-only conversion returns empty so HEAD stays at PlainInit default",
			advertised: "",
			desired: map[plumbing.ReferenceName]planner.DesiredRef{
				"refs/tags/v1.0": tag("v1.0"),
			},
			want: "",
		},
		{
			name:       "empty desired returns empty",
			advertised: "",
			desired:    map[plumbing.ReferenceName]planner.DesiredRef{},
			want:       "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pickHEAD(tt.advertised, tt.desired)
			if got != tt.want {
				t.Fatalf("pickHEAD = %q, want %q", got, tt.want)
			}
		})
	}
}
