package translate

import (
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"
)

func TestHashNotes_RoundTripAndExtend(t *testing.T) {
	t.Parallel()

	store := memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA1))
	sig := object.Signature{Name: "git-sync", Email: "git-sync@example.com", When: time.Unix(0, 0).UTC()}
	sha1A := plumbing.NewHash(strings.Repeat("a1", 20))
	sha1B := plumbing.NewHash(strings.Repeat("b1", 20))
	sha256A := plumbing.NewHash(strings.Repeat("a2", 32))
	sha256B := plumbing.NewHash(strings.Repeat("b2", 32))

	first, written, err := WriteHashNotes(store, plumbing.ZeroHash, nil, []Pair{{Source: sha256A, Target: sha1A}}, sig, "notes\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 3 || written[2] != first {
		t.Fatalf("written objects: got %v, want blob, tree, commit %s", written, first)
	}
	values, entries, err := ReadHashNotes(store, first)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[plumbing.Hash]plumbing.Hash{sha1A: sha256A}; !maps.Equal(values, want) {
		t.Fatalf("first notes: got %v, want %v", values, want)
	}

	second, _, err := WriteHashNotes(store, first, entries, []Pair{{Source: sha256B, Target: sha1B}}, sig, "notes\n")
	if err != nil {
		t.Fatal(err)
	}
	values, _, err = ReadHashNotes(store, second)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[plumbing.Hash]plumbing.Hash{sha1A: sha256A, sha1B: sha256B}; !maps.Equal(values, want) {
		t.Fatalf("extended notes: got %v, want %v", values, want)
	}
	c, err := object.GetCommit(store, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ParentHashes) != 1 || c.ParentHashes[0] != first {
		t.Errorf("notes commit parents: got %v, want [%s]", c.ParentHashes, first)
	}
}

func TestReadHashNotes_Fanout(t *testing.T) {
	t.Parallel()

	store := memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA1))
	annotated := plumbing.NewHash(strings.Repeat("c3", 20))
	noted := plumbing.NewHash(strings.Repeat("d4", 32))
	blob := writeRaw(t, store, plumbing.BlobObject, noted.String()+"\n")
	sub := writeTreeEntries(t, store, []treeEntry{{"100644", annotated.String()[2:], blob}})
	root := writeTreeEntries(t, store, []treeEntry{{"40000", annotated.String()[:2], sub}})
	commit := writeRaw(t, store, plumbing.CommitObject, "tree "+root.String()+"\n"+
		"author A <a@example.com> 0 +0000\ncommitter A <a@example.com> 0 +0000\n\nnotes\n")

	values, _, err := ReadHashNotes(store, commit)
	if err != nil {
		t.Fatal(err)
	}
	if got := values[annotated]; got != noted {
		t.Fatalf("fanout note for %s: got %s, want %s", annotated, got, noted)
	}
}

func TestReadHashNotes_TreeTip(t *testing.T) {
	t.Parallel()

	store := memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA1))
	sig := object.Signature{Name: "git-sync", Email: "git-sync@example.com", When: time.Unix(0, 0).UTC()}
	sha1A := plumbing.NewHash(strings.Repeat("a1", 20))
	sha256A := plumbing.NewHash(strings.Repeat("a2", 32))
	commit, _, err := WriteHashNotes(store, plumbing.ZeroHash, nil, []Pair{{Source: sha256A, Target: sha1A}}, sig, "notes\n")
	if err != nil {
		t.Fatal(err)
	}
	c, err := object.GetCommit(store, commit)
	if err != nil {
		t.Fatal(err)
	}

	values, entries, err := ReadHashNotes(store, c.TreeHash)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[plumbing.Hash]plumbing.Hash{sha1A: sha256A}; !maps.Equal(values, want) || len(entries) != 1 {
		t.Fatalf("notes read from a tree: got %v and %d entries, want %v", values, len(entries), want)
	}
}
