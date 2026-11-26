package syncer

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"

	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
	"entire.io/entire/git-sync/internal/translate"
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

	got, err := objectsMissingFromTarget(f.store, formatcfg.SHA1, []plumbing.Hash{f.next}, map[plumbing.Hash]struct{}{f.base: {}}, treeKnown)
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

// TestObjectsMissingFromTarget_TreeNotOnTarget has a boundary commit whose
// tree may not translate to the tree the target holds, as with a repository
// an older convert-sha256 converted. The trees next shares with it are
// pushed. Blobs translate unchanged, so they are not.
func TestObjectsMissingFromTarget_TreeNotOnTarget(t *testing.T) {
	t.Parallel()
	f := newTranslatedFixture(t)

	got, err := objectsMissingFromTarget(f.store, formatcfg.SHA1, []plumbing.Hash{f.next}, map[plumbing.Hash]struct{}{f.base: {}},
		func(plumbing.Hash) (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	want := []plumbing.Hash{f.next, f.nextTree, f.newA, f.shared}
	slices.SortFunc(got, func(a, b plumbing.Hash) int { return strings.Compare(a.String(), b.String()) })
	slices.SortFunc(want, func(a, b plumbing.Hash) int { return strings.Compare(a.String(), b.String()) })
	if !slices.Equal(got, want) {
		t.Fatalf("objects to push: got %v, want %v", got, want)
	}
}

func TestRoundTrips(t *testing.T) {
	t.Parallel()
	f := newTranslatedFixture(t)
	exact, err := translate.New(f.store, memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA256)),
		formatcfg.SHA1, formatcfg.SHA256, nil).Translate(f.next)
	if err != nil {
		t.Fatal(err)
	}
	other := plumbing.NewHash(strings.Repeat("ab", 32))

	tests := []struct {
		name string
		seed map[plumbing.Hash]plumbing.Hash
		want bool
	}{
		{name: "exact", seed: map[plumbing.Hash]plumbing.Hash{f.next: exact}, want: true},
		{name: "normalized elsewhere", seed: map[plumbing.Hash]plumbing.Hash{f.next: other}, want: false},
		{name: "unknown", want: false},
	}
	for _, tt := range tests {
		tr := translate.New(f.store, memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA256)),
			formatcfg.SHA1, formatcfg.SHA256, tt.seed)
		got, err := roundTrips(f.store, formatcfg.SHA1, formatcfg.SHA256, tr, f.next)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
		if used := tr.SeedsUsed(); len(used) != 0 {
			t.Errorf("%s: checking reported seeds as used: %v", tt.name, used)
		}
	}
}

func TestDropConvertedTags(t *testing.T) {
	t.Parallel()
	desired := map[plumbing.ReferenceName]planner.DesiredRef{
		"refs/heads/main":                {SourceRef: "refs/heads/main", TargetRef: "refs/heads/main"},
		"refs/tags/v1":                   {SourceRef: "refs/tags/v1", TargetRef: "refs/tags/v1"},
		"refs/tags/converted/main":       {SourceRef: "refs/tags/converted/main", TargetRef: "refs/tags/converted/main"},
		"refs/tags/attestation":          {SourceRef: "refs/tags/converted/main", TargetRef: "refs/tags/attestation"},
		"refs/tags/converted/from-other": {SourceRef: "refs/tags/v2", TargetRef: "refs/tags/converted/from-other"},
	}
	dropConvertedTags(desired)
	got := slices.Sorted(maps.Keys(desired))
	want := []plumbing.ReferenceName{"refs/heads/main", "refs/tags/v1"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func treeKnown(plumbing.Hash) (bool, error) { return true, nil }

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

	got, err := objectsMissingFromTarget(f.store, formatcfg.SHA1, []plumbing.Hash{tag}, map[plumbing.Hash]struct{}{f.base: {}}, treeKnown)
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
		{"retranslated tag matches target", tag, sha1Next, sha1Next, planner.ActionSkip},
		{"retranslated tag differs from target", tag, unknown, sha1Next, planner.ActionSkip},
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
		{name: "best effort", cfg: Config{BestEffort: true}, wantErr: "best-effort"},
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

func TestValidateTranslationFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		atomic       bool
		translating  bool
		targetAtomic bool
		retranslate  bool
		wantErr      string
	}{
		{name: "off", translating: true},
		{name: "off without translation"},
		{name: "on", atomic: true, translating: true, targetAtomic: true},
		{name: "without translation", atomic: true, targetAtomic: true, wantErr: "--atomic"},
		{name: "target lacks atomic", atomic: true, translating: true, wantErr: "atomic"},
		{name: "retranslate tags", retranslate: true, translating: true},
		{name: "retranslate tags without translation", retranslate: true, wantErr: "--retranslate-tags"},
	}
	for _, tt := range tests {
		err := validateTranslationFlags(Config{Atomic: tt.atomic, RetranslateTags: tt.retranslate}, tt.translating, tt.targetAtomic)
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tt.name, err)
		case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
			t.Errorf("%s: got %v, want error mentioning %q", tt.name, err, tt.wantErr)
		}
	}
	if err := validateTranslationFlags(Config{Atomic: true}, true, false); !errors.Is(err, gitproto.ErrAtomicPushUnsupported) {
		t.Errorf("target lacks atomic: got %v, want ErrAtomicPushUnsupported", err)
	}
}

func TestReachableCommits(t *testing.T) {
	t.Parallel()
	f := newTranslatedFixture(t)

	tests := []struct {
		name  string
		roots []plumbing.Hash
		stop  []plumbing.Hash
		want  []plumbing.Hash
	}{
		{"everything", []plumbing.Hash{f.next}, nil, []plumbing.Hash{f.next, f.base}},
		{"stops at known commits", []plumbing.Hash{f.next, f.sibling}, []plumbing.Hash{f.base}, []plumbing.Hash{f.next, f.sibling}},
		{"root already known", []plumbing.Hash{f.base}, []plumbing.Hash{f.base}, nil},
	}
	for _, tt := range tests {
		stop := make(map[plumbing.Hash]plumbing.Hash)
		for _, h := range tt.stop {
			stop[h] = plumbing.ZeroHash
		}
		got, err := reachableCommits(f.store, formatcfg.SHA1, tt.roots, stop)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		want := make(map[plumbing.Hash]struct{})
		for _, h := range tt.want {
			want[h] = struct{}{}
		}
		if !maps.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", tt.name, got, want)
		}
	}
}
