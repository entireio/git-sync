package syncer

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"

	"entire.io/entire/git-sync/internal/planner"
)

type translatedFixture struct {
	store                    *memory.Storage
	base, next, sibling      plumbing.Hash
	baseTree, nextTree       plumbing.Hash
	shared, sharedBlob, oldA plumbing.Hash
	newA                     plumbing.Hash
}

// newTranslatedFixture builds base, then next and sibling on top of it. next
// changes a.txt and keeps the sub directory, so the only objects next adds
// are itself, its root tree and the new a.txt.
func newTranslatedFixture(t *testing.T) translatedFixture {
	t.Helper()
	f := translatedFixture{store: memory.NewStorage()}
	f.oldA = storeBlob(t, f.store, "a\n")
	f.newA = storeBlob(t, f.store, "a2\n")
	f.sharedBlob = storeBlob(t, f.store, "x\n")
	f.shared = storeTree(t, f.store, object.TreeEntry{Name: "x.txt", Mode: filemode.Regular, Hash: f.sharedBlob})
	f.baseTree = storeTree(t, f.store,
		object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: f.oldA},
		object.TreeEntry{Name: "sub", Mode: filemode.Dir, Hash: f.shared})
	f.nextTree = storeTree(t, f.store,
		object.TreeEntry{Name: "a.txt", Mode: filemode.Regular, Hash: f.newA},
		object.TreeEntry{Name: "sub", Mode: filemode.Dir, Hash: f.shared})
	f.base = storeCommit(t, f.store, f.baseTree, "base")
	f.next = storeCommit(t, f.store, f.nextTree, "next", f.base)
	f.sibling = storeCommit(t, f.store, f.baseTree, "sibling", f.base)
	return f
}

func TestObjectsMissingFromTarget(t *testing.T) {
	t.Parallel()
	f := newTranslatedFixture(t)

	got, err := objectsMissingFromTarget(f.store, formatcfg.SHA1, []plumbing.Hash{f.next}, map[plumbing.Hash]struct{}{f.base: {}})
	if err != nil {
		t.Fatal(err)
	}
	want := []plumbing.Hash{f.next, f.nextTree, f.newA}
	slices.SortFunc(got, func(a, b plumbing.Hash) int { return strings.Compare(a.String(), b.String()) })
	slices.SortFunc(want, func(a, b plumbing.Hash) int { return strings.Compare(a.String(), b.String()) })
	if !slices.Equal(got, want) {
		t.Fatalf("objects to push: got %v, want %v", got, want)
	}
}

func TestObjectsMissingFromTarget_HeaderOnlyTag(t *testing.T) {
	t.Parallel()
	f := newTranslatedFixture(t)
	obj := f.store.NewEncodedObject()
	obj.SetType(plumbing.TagObject)
	w, err := obj.Writer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("object " + f.next.String() + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	tag, err := f.store.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}

	got, err := objectsMissingFromTarget(f.store, formatcfg.SHA1, []plumbing.Hash{tag}, map[plumbing.Hash]struct{}{f.base: {}})
	if err != nil {
		t.Fatal(err)
	}
	want := []plumbing.Hash{tag, f.next, f.nextTree, f.newA}
	slices.SortFunc(got, func(a, b plumbing.Hash) int { return strings.Compare(a.String(), b.String()) })
	slices.SortFunc(want, func(a, b plumbing.Hash) int { return strings.Compare(a.String(), b.String()) })
	if !slices.Equal(got, want) {
		t.Fatalf("objects to push: got %v, want %v", got, want)
	}
}

func TestPlanTranslatedRef(t *testing.T) {
	t.Parallel()
	f := newTranslatedFixture(t)

	sha1Base := plumbing.NewHash(strings.Repeat("1b", 20))
	sha1Next := plumbing.NewHash(strings.Repeat("1e", 20))
	sha1Sibling := plumbing.NewHash(strings.Repeat("15", 20))
	unknown := plumbing.NewHash(strings.Repeat("ff", 20))
	reverse := map[plumbing.Hash]plumbing.Hash{sha1Base: f.base, sha1Sibling: f.sibling}
	branch := planner.DesiredRef{Kind: planner.RefKindBranch, SourceRef: "refs/heads/main", TargetRef: "refs/heads/main", SourceHash: f.next}
	tag := planner.DesiredRef{Kind: planner.RefKindTag, SourceRef: "refs/tags/v1", TargetRef: "refs/tags/v1", SourceHash: f.next}

	tests := []struct {
		name       string
		want       planner.DesiredRef
		target     plumbing.Hash
		translated plumbing.Hash
		action     planner.Action
	}{
		{"tag already on target", tag, unknown, plumbing.ZeroHash, planner.ActionSkip},
		{"new tag", tag, plumbing.ZeroHash, sha1Next, planner.ActionCreate},
		{"new branch", branch, plumbing.ZeroHash, sha1Next, planner.ActionCreate},
		{"already current", branch, sha1Next, sha1Next, planner.ActionSkip},
		{"fast-forward", branch, sha1Base, sha1Next, planner.ActionUpdate},
		{"target written directly", branch, unknown, sha1Next, planner.ActionBlock},
		{"diverged", branch, sha1Sibling, sha1Next, planner.ActionBlock},
	}
	for _, tt := range tests {
		got := planTranslatedRef(f.store, tt.want, tt.target, tt.translated, reverse)
		if got.Action != tt.action {
			t.Errorf("%s: action %s (%s), want %s", tt.name, got.Action, got.Reason, tt.action)
		}
		if got.SourceHash != tt.want.SourceHash {
			t.Errorf("%s: plan reports source %s, want the source's own hash %s", tt.name, got.SourceHash, tt.want.SourceHash)
		}
	}
}

func TestValidateTranslation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "sync"},
		{name: "replicate", cfg: Config{Mode: modeReplicate}, wantErr: "replicate"},
		{name: "force", cfg: Config{ForceWithLease: true}, wantErr: "force"},
		{name: "prune", cfg: Config{Prune: true}, wantErr: "--prune"},
		{name: "all refs", cfg: Config{AllRefs: true}, wantErr: "--all-refs"},
	}
	for _, tt := range tests {
		err := validateTranslation(tt.cfg)
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tt.name, err)
		case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
			t.Errorf("%s: got %v, want error mentioning %q", tt.name, err, tt.wantErr)
		}
	}
}

func storeBlob(t *testing.T, store *memory.Storage, content string) plumbing.Hash {
	t.Helper()
	obj := store.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
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

func storeTree(t *testing.T, store *memory.Storage, entries ...object.TreeEntry) plumbing.Hash {
	t.Helper()
	obj := store.NewEncodedObject()
	if err := (&object.Tree{Entries: entries}).Encode(obj); err != nil {
		t.Fatal(err)
	}
	h, err := store.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func storeCommit(t *testing.T, store *memory.Storage, tree plumbing.Hash, msg string, parents ...plumbing.Hash) plumbing.Hash {
	t.Helper()
	sig := object.Signature{Name: "A", Email: "a@example.com", When: time.Unix(1700000000, 0).UTC()}
	obj := store.NewEncodedObject()
	c := &object.Commit{Author: sig, Committer: sig, Message: msg, TreeHash: tree, ParentHashes: parents}
	if err := c.Encode(obj); err != nil {
		t.Fatal(err)
	}
	h, err := store.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestNotesParent(t *testing.T) {
	t.Parallel()
	f := newTranslatedFixture(t)

	tests := []struct {
		name string
		tip  plumbing.Hash
		want plumbing.Hash
	}{
		{"no notes", plumbing.ZeroHash, plumbing.ZeroHash},
		{"commit", f.base, f.base},
		{"tree", f.baseTree, plumbing.ZeroHash},
	}
	for _, tt := range tests {
		got, err := notesParent(f.store, tt.tip)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%s: got %s, want %s", tt.name, got, tt.want)
		}
	}
}
