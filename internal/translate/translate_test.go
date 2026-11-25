package translate

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/storage/memory"
)

type treeEntry struct {
	mode string
	name string
	hash plumbing.Hash
}

func TestTranslate_MatchesNativeObjects(t *testing.T) {
	t.Parallel()

	src := memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA256))
	want := memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA1))
	srcTip := buildHistory(t, src)
	wantTip := buildHistory(t, want)

	dst := memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA1))
	tr := New(src, dst, formatcfg.SHA256, formatcfg.SHA1, nil)
	got, err := tr.Translate(srcTip)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantTip {
		t.Fatalf("translated tip: got %s, want %s", got, wantTip)
	}
	if n := len(tr.Commits()); n != 2 {
		t.Errorf("translated commits: got %d, want 2", n)
	}
	if err := dst.HasEncodedObject(wantTip); err != nil {
		t.Errorf("translated tip missing from dst: %v", err)
	}
}

func TestTranslate_StopsAtSeed(t *testing.T) {
	t.Parallel()

	src := memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA256))
	want := memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA1))
	absentParent := plumbing.NewHash("aa" + fmt.Sprintf("%062d", 1))
	seededParent := plumbing.NewHash("bb" + fmt.Sprintf("%038d", 1))

	srcTip := writeCommit(t, src, writeTree(t, src), absentParent, "child")
	wantTip := writeCommit(t, want, writeTree(t, want), seededParent, "child")

	tr := New(src, memory.NewStorage(), formatcfg.SHA256, formatcfg.SHA1, map[plumbing.Hash]plumbing.Hash{absentParent: seededParent})
	got, err := tr.Translate(srcTip)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantTip {
		t.Fatalf("translated tip: got %s, want %s", got, wantTip)
	}
	commits := tr.Commits()
	if len(commits) != 1 || commits[0] != (Pair{Source: srcTip, Target: wantTip}) {
		t.Errorf("commits: got %v, want only the child", commits)
	}
	if used := tr.SeedsUsed(); len(used) != 1 || used[0] != (Pair{Source: absentParent, Target: seededParent}) {
		t.Errorf("seeds used: got %v, want only the parent", used)
	}
}

func TestTranslate_KeepsCommitSignatures(t *testing.T) {
	t.Parallel()

	src := memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA256))
	want := memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA1))
	signatures := "gpgsig -----BEGIN SSH SIGNATURE-----\n" +
		" sha1\n" +
		" -----END SSH SIGNATURE-----\n" +
		"gpgsig-sha256 -----BEGIN SSH SIGNATURE-----\n" +
		" \n" +
		" sha256\n" +
		" -----END SSH SIGNATURE-----\n"
	commit := func(store *memory.Storage) plumbing.Hash {
		return writeRaw(t, store, plumbing.CommitObject, "tree "+writeTree(t, store).String()+"\n"+
			"author A <a@example.com> 1700000000 +0000\n"+
			"committer A <a@example.com> 1700000000 +0000\n"+
			signatures+"\nsigned\n")
	}

	got, err := New(src, memory.NewStorage(), formatcfg.SHA256, formatcfg.SHA1, nil).Translate(commit(src))
	if err != nil {
		t.Fatal(err)
	}
	if wantHash := commit(want); got != wantHash {
		t.Fatalf("translated commit: got %s, want %s", got, wantHash)
	}
}

// TestTranslate_SwapsTagSignatures follows convert_tag_object: the
// signature over the source format moves from the tag body into its
// header, and the one over the target format moves from the header into
// the body.
func TestTranslate_SwapsTagSignatures(t *testing.T) {
	t.Parallel()

	src := memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA256))
	want := memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA1))
	srcCommit := writeCommit(t, src, writeTree(t, src), plumbing.ZeroHash, "c")
	wantCommit := writeCommit(t, want, writeTree(t, want), plumbing.ZeroHash, "c")
	header := "type commit\ntag v1\ntagger A <a@example.com> 1700000000 +0000\n"

	srcTag := writeRaw(t, src, plumbing.TagObject, "object "+srcCommit.String()+"\n"+header+
		"gpgsig -----BEGIN SSH SIGNATURE-----\n"+
		" sha1\n"+
		" -----END SSH SIGNATURE-----\n"+
		"\nrelease\n"+
		"-----BEGIN SSH SIGNATURE-----\nsha256\n-----END SSH SIGNATURE-----\n")
	wantTag := writeRaw(t, want, plumbing.TagObject, "object "+wantCommit.String()+"\n"+header+
		"gpgsig-sha256 -----BEGIN SSH SIGNATURE-----\n"+
		" sha256\n"+
		" -----END SSH SIGNATURE-----\n"+
		"\nrelease\n"+
		"-----BEGIN SSH SIGNATURE-----\nsha1\n-----END SSH SIGNATURE-----\n")

	got, err := New(src, memory.NewStorage(), formatcfg.SHA256, formatcfg.SHA1, nil).Translate(srcTag)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantTag {
		t.Fatalf("translated tag: got %s, want %s", got, wantTag)
	}
}

func TestTranslate_RejectsUnknownCommitHeader(t *testing.T) {
	t.Parallel()

	src := memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA256))
	commit := writeRaw(t, src, plumbing.CommitObject, "tree "+writeTree(t, src).String()+"\n"+
		"author A <a@example.com> 1700000000 +0000\n"+
		"committer A <a@example.com> 1700000000 +0000\n"+
		"x-custom "+strings.Repeat("ab", 32)+"\n"+
		"\nmsg\n")

	_, err := New(src, memory.NewStorage(), formatcfg.SHA256, formatcfg.SHA1, nil).Translate(commit)
	if err == nil || !strings.Contains(err.Error(), "x-custom") {
		t.Fatalf("got %v, want an error naming the unknown header", err)
	}
}

func TestTranslate_GitlinkUsesSeed(t *testing.T) {
	t.Parallel()

	src := memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA256))
	want := memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA1))
	sub := plumbing.NewHash(fmt.Sprintf("%064d", 7))
	subSHA1 := plumbing.NewHash(fmt.Sprintf("%040d", 7))
	tree := writeTreeEntries(t, src, []treeEntry{{"160000", "vendor", sub}})
	wantTree := writeTreeEntries(t, want, []treeEntry{{"160000", "vendor", subSHA1}})

	tr := New(src, memory.NewStorage(), formatcfg.SHA256, formatcfg.SHA1, map[plumbing.Hash]plumbing.Hash{sub: subSHA1})
	got, err := tr.Translate(tree)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantTree {
		t.Fatalf("translated tree: got %s, want %s", got, wantTree)
	}
	if used := tr.SeedsUsed(); len(used) != 0 {
		t.Errorf("seeds used: got %v, want none, as a submodule commit is not in this repository", used)
	}
}

func TestTranslate_RejectsGitlink(t *testing.T) {
	t.Parallel()

	src := memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA256))
	sub := plumbing.NewHash(fmt.Sprintf("%064d", 7))
	tree := writeTreeEntries(t, src, []treeEntry{{"160000", "vendor", sub}})

	_, err := New(src, memory.NewStorage(), formatcfg.SHA256, formatcfg.SHA1, nil).Translate(tree)
	if !errors.Is(err, ErrGitlink) {
		t.Fatalf("got %v, want ErrGitlink", err)
	}
}

func TestTagObject(t *testing.T) {
	t.Parallel()

	target := plumbing.NewHash(strings.Repeat("ab", 32))
	tests := []struct {
		name string
		raw  string
		want plumbing.Hash
	}{
		{"header only", "object " + target.String() + "\n", target},
		{"full", "object " + target.String() + "\ntype commit\ntag v1\n\nmsg\n", target},
		{"no newline", "object " + target.String(), plumbing.ZeroHash},
		{"short id", "object " + strings.Repeat("ab", 20) + "\n", plumbing.ZeroHash},
		{"not hex", "object " + strings.Repeat("zz", 32) + "\n", plumbing.ZeroHash},
		{"no object line", "type commit\n", plumbing.ZeroHash},
	}
	for _, tt := range tests {
		got, err := TagObject([]byte(tt.raw), formatcfg.SHA256)
		if tt.want.IsZero() {
			if err == nil {
				t.Errorf("%s: got %s, want an error", tt.name, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("%s: got %s, %v, want %s", tt.name, got, err, tt.want)
		}
	}
}

func TestAddHeaderSignature_StopsAtNUL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, buf, sig, want string
	}{
		{"plain", "object x\ntag v\n\nmsg\n", "a\nb\n", "object x\ntag v\ngpgsig a\n b\n\nmsg\n"},
		{"NUL in signature", "object x\n\nmsg\n", "a\nb\x00c\nd\n", "object x\ngpgsig a\n b\nmsg\n"},
		{"NUL in header", "object x\ntag v\x00\n\nmsg\n", "a\n", "object x\ntag v\x00\n\nmsg\ngpgsig a\n"},
	}
	for _, tt := range tests {
		got := addHeaderSignature([]byte(tt.buf), []byte(tt.sig), formatcfg.SHA1)
		if string(got) != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestSignatureStart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		body string
		want int
	}{
		{"\nplain\n", len("\nplain\n")},
		{"\nmsg\n-----BEGIN PGP SIGNATURE-----\nx\n", len("\nmsg\n")},
		{"\nquoted -----BEGIN PGP SIGNATURE-----\n", len("\nquoted -----BEGIN PGP SIGNATURE-----\n")},
		{"\n-----BEGIN SSH SIGNATURE-----\na\n-----BEGIN PGP SIGNATURE-----\nb\n", len("\n-----BEGIN SSH SIGNATURE-----\na\n")},
	}
	for _, tt := range tests {
		if got := signatureStart([]byte(tt.body)); got != tt.want {
			t.Errorf("signatureStart(%q) = %d, want %d", tt.body, got, tt.want)
		}
	}
}

// buildHistory writes the same two-commit history into store using the
// store's own object format, so a SHA256 and a SHA1 store end up with
// objects that differ only in their embedded hashes.
func buildHistory(t *testing.T, store *memory.Storage) plumbing.Hash {
	t.Helper()
	readme := writeRaw(t, store, plumbing.BlobObject, "hello\n")
	script := writeRaw(t, store, plumbing.BlobObject, "#!/bin/sh\necho hi\n")
	link := writeRaw(t, store, plumbing.BlobObject, "README")
	sub := writeTreeEntries(t, store, []treeEntry{{"100644", "a.txt", readme}})
	root1 := writeTreeEntries(t, store, []treeEntry{{"100644", "README", readme}})
	root2 := writeTreeEntries(t, store, []treeEntry{
		{"100644", "README", readme},
		{"120000", "link", link},
		{"100755", "run.sh", script},
		{"40000", "sub", sub},
	})
	first := writeCommit(t, store, root1, plumbing.ZeroHash, "initial")
	return writeCommit(t, store, root2, first, "second")
}

func writeTree(t *testing.T, store *memory.Storage) plumbing.Hash {
	t.Helper()
	blob := writeRaw(t, store, plumbing.BlobObject, "x\n")
	return writeTreeEntries(t, store, []treeEntry{{"100644", "x", blob}})
}

func writeTreeEntries(t *testing.T, store *memory.Storage, entries []treeEntry) plumbing.Hash {
	t.Helper()
	var raw []byte
	for _, e := range entries {
		raw = append(raw, e.mode+" "+e.name+"\x00"...)
		raw = append(raw, e.hash.Bytes()...)
	}
	return writeRaw(t, store, plumbing.TreeObject, string(raw))
}

func writeCommit(t *testing.T, store *memory.Storage, tree, parent plumbing.Hash, msg string) plumbing.Hash {
	t.Helper()
	raw := "tree " + tree.String() + "\n"
	if !parent.IsZero() {
		raw += "parent " + parent.String() + "\n"
	}
	raw += "author A <a@example.com> 1700000000 +0000\n" +
		"committer A <a@example.com> 1700000000 +0000\n" +
		"encoding UTF-8\n" +
		"\n" + msg + "\n"
	return writeRaw(t, store, plumbing.CommitObject, raw)
}

func writeRaw(t *testing.T, store *memory.Storage, typ plumbing.ObjectType, content string) plumbing.Hash {
	t.Helper()
	obj := store.NewEncodedObject()
	obj.SetType(typ)
	obj.SetSize(int64(len(content)))
	w, err := obj.Writer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	h, err := store.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
