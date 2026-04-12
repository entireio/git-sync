package syncertest

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	billy "github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/memfs"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"
)

// NewMemoryRepo creates an in-memory repository with a memfs-backed worktree.
func NewMemoryRepo(tb testing.TB) (*git.Repository, billy.Filesystem) {
	tb.Helper()

	fs := memfs.New()
	repo, err := git.Init(memory.NewStorage(), git.WithWorkTree(fs))
	if err != nil {
		tb.Fatalf("init repo: %v", err)
	}
	return repo, fs
}

// MakeCommits writes and commits tracked.txt repeatedly on the current branch.
func MakeCommits(tb testing.TB, repo *git.Repository, fs billy.Filesystem, count int) {
	tb.Helper()

	wt, err := repo.Worktree()
	if err != nil {
		tb.Fatalf("open worktree: %v", err)
	}

	for i := 0; i < count; i++ {
		content := strings.Repeat(fmt.Sprintf("line %d %d\n", i, time.Now().UnixNano()), 24)
		file, err := fs.Create("tracked.txt")
		if err != nil {
			tb.Fatalf("create file: %v", err)
		}
		if _, err := io.WriteString(file, content); err != nil {
			tb.Fatalf("write file: %v", err)
		}
		if err := file.Close(); err != nil {
			tb.Fatalf("close file: %v", err)
		}
		if _, err := wt.Add("tracked.txt"); err != nil {
			tb.Fatalf("add file: %v", err)
		}
		if _, err := wt.Commit(fmt.Sprintf("commit %d", i), &git.CommitOptions{
			Author:    &defaultSignature,
			Committer: &defaultSignature,
		}); err != nil {
			tb.Fatalf("commit: %v", err)
		}
	}
}

// MakeBenchmarkCommits preserves the smaller benchmark-specific commit shape so
// benchmark series remain comparable across refactors.
func MakeBenchmarkCommits(tb testing.TB, repo *git.Repository, fs billy.Filesystem, count int) {
	tb.Helper()

	wt, err := repo.Worktree()
	if err != nil {
		tb.Fatalf("open worktree: %v", err)
	}

	for i := 0; i < count; i++ {
		content := fmt.Sprintf("bench line %d %d\n", i, time.Now().UnixNano())
		file, err := fs.Create("tracked.txt")
		if err != nil {
			tb.Fatalf("create file: %v", err)
		}
		if _, err := io.WriteString(file, content); err != nil {
			tb.Fatalf("write file: %v", err)
		}
		if err := file.Close(); err != nil {
			tb.Fatalf("close file: %v", err)
		}
		if _, err := wt.Add("tracked.txt"); err != nil {
			tb.Fatalf("add file: %v", err)
		}
		if _, err := wt.Commit(fmt.Sprintf("bench commit %d", i), &git.CommitOptions{
			Author:    &defaultSignature,
			Committer: &defaultSignature,
		}); err != nil {
			tb.Fatalf("commit: %v", err)
		}
	}
}

// MakeLargeCommits creates large pseudo-random blob commits to exercise pack
// sizing and batching paths.
func MakeLargeCommits(tb testing.TB, repo *git.Repository, fs billy.Filesystem, count int, blobSize int) {
	tb.Helper()

	wt, err := repo.Worktree()
	if err != nil {
		tb.Fatalf("open worktree: %v", err)
	}

	for i := 0; i < count; i++ {
		name := fmt.Sprintf("blob-%d.bin", i)
		file, err := fs.Create(name)
		if err != nil {
			tb.Fatalf("create file: %v", err)
		}
		content := make([]byte, blobSize)
		state := uint32(0x9e3779b9) + uint32(i)*uint32(2654435761)
		for idx := range content {
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			content[idx] = byte(state >> 24)
		}
		if _, err := file.Write(content); err != nil {
			tb.Fatalf("write file: %v", err)
		}
		if err := file.Close(); err != nil {
			tb.Fatalf("close file: %v", err)
		}
		if _, err := wt.Add(name); err != nil {
			tb.Fatalf("add file: %v", err)
		}
		if _, err := wt.Commit(fmt.Sprintf("large commit %d", i), &git.CommitOptions{
			Author:    &defaultSignature,
			Committer: &defaultSignature,
		}); err != nil {
			tb.Fatalf("commit: %v", err)
		}
	}
}

// AssertBranchHeadsMatch verifies that two repos agree on the named branch tip.
func AssertBranchHeadsMatch(tb testing.TB, sourceRepo, targetRepo *git.Repository, branch string) {
	tb.Helper()

	sourceRef, err := sourceRepo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		tb.Fatalf("resolve source ref: %v", err)
	}
	targetRef, err := targetRepo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		tb.Fatalf("resolve target ref: %v", err)
	}
	if sourceRef.Hash() != targetRef.Hash() {
		tb.Fatalf("branch %s mismatch: source=%s target=%s", branch, sourceRef.Hash(), targetRef.Hash())
	}
}

var defaultSignature = object.Signature{
	Name:  "test",
	Email: "test@example.com",
	When:  time.Unix(1, 0).UTC(),
}
