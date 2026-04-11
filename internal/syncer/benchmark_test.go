package syncer

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	billy "github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"
)

func BenchmarkRunBootstrapEmptyTarget(b *testing.B) {
	b.ReportAllocs()

	sourceRepo, sourceFS := newBenchRepo(b)
	makeBenchCommits(b, sourceRepo, sourceFS, 8)
	sourceServer := newSmartHTTPRepoServerV2(b, sourceRepo)
	defer sourceServer.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		targetRepo, err := git.Init(memory.NewStorage(), nil)
		if err != nil {
			b.Fatalf("init target repo: %v", err)
		}
		targetServer := newSmartHTTPRepoServer(b, targetRepo)
		b.StartTimer()

		_, err = Run(context.Background(), Config{
			Source:       Endpoint{URL: sourceServer.RepoURL()},
			Target:       Endpoint{URL: targetServer.RepoURL()},
			ProtocolMode: protocolModeAuto,
		})
		b.StopTimer()
		targetServer.Close()
		if err != nil {
			b.Fatalf("bootstrap benchmark run failed: %v", err)
		}
		b.StartTimer()
	}
}

func BenchmarkRunIncrementalRelay(b *testing.B) {
	b.ReportAllocs()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		sourceRepo, sourceFS := newBenchRepo(b)
		makeBenchCommits(b, sourceRepo, sourceFS, 2)

		targetRepo, err := git.Init(memory.NewStorage(), nil)
		if err != nil {
			b.Fatalf("init target repo: %v", err)
		}
		if err := copyRefsAndObjects(sourceRepo.Storer, targetRepo.Storer, []plumbing.ReferenceName{plumbing.NewBranchReferenceName(testBranch)}); err != nil {
			b.Fatalf("copy target baseline: %v", err)
		}

		makeBenchCommits(b, sourceRepo, sourceFS, 1)

		sourceServer := newSmartHTTPRepoServerV2(b, sourceRepo)
		targetServer := newSmartHTTPRepoServer(b, targetRepo)
		b.StartTimer()

		_, err = Run(context.Background(), Config{
			Source:       Endpoint{URL: sourceServer.RepoURL()},
			Target:       Endpoint{URL: targetServer.RepoURL()},
			ProtocolMode: protocolModeAuto,
		})
		b.StopTimer()
		sourceServer.Close()
		targetServer.Close()
		if err != nil {
			b.Fatalf("incremental benchmark run failed: %v", err)
		}
		b.StartTimer()
	}
}

func BenchmarkRunMaterializedFallback(b *testing.B) {
	b.ReportAllocs()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		sourceRepo, sourceFS := newBenchRepo(b)
		makeBenchCommits(b, sourceRepo, sourceFS, 3)

		targetRepo, err := git.Init(memory.NewStorage(), nil)
		if err != nil {
			b.Fatalf("init target repo: %v", err)
		}
		if err := copyRefsAndObjects(sourceRepo.Storer, targetRepo.Storer, []plumbing.ReferenceName{plumbing.NewBranchReferenceName(testBranch)}); err != nil {
			b.Fatalf("copy target baseline: %v", err)
		}

		sourceHead, err := sourceRepo.Reference(plumbing.NewBranchReferenceName(testBranch), true)
		if err != nil {
			b.Fatalf("resolve source head: %v", err)
		}
		if err := sourceRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("release"), sourceHead.Hash())); err != nil {
			b.Fatalf("set source release branch: %v", err)
		}

		sourceServer := newSmartHTTPRepoServerV2(b, sourceRepo)
		targetServer := newSmartHTTPRepoServer(b, targetRepo)
		b.StartTimer()

		_, err = Run(context.Background(), Config{
			Source:       Endpoint{URL: sourceServer.RepoURL()},
			Target:       Endpoint{URL: targetServer.RepoURL()},
			ProtocolMode: protocolModeAuto,
		})
		b.StopTimer()
		sourceServer.Close()
		targetServer.Close()
		if err != nil {
			b.Fatalf("materialized benchmark run failed: %v", err)
		}
		b.StartTimer()
	}
}

func newBenchRepo(b *testing.B) (*git.Repository, billy.Filesystem) {
	b.Helper()
	fs := memfs.New()
	repo, err := git.Init(memory.NewStorage(), fs)
	if err != nil {
		b.Fatalf("init repo: %v", err)
	}
	return repo, fs
}

func makeBenchCommits(b *testing.B, repo *git.Repository, fs billy.Filesystem, count int) {
	b.Helper()
	wt, err := repo.Worktree()
	if err != nil {
		b.Fatalf("open worktree: %v", err)
	}

	for i := 0; i < count; i++ {
		content := fmt.Sprintf("bench line %d %d\n", i, time.Now().UnixNano())
		file, err := fs.Create("tracked.txt")
		if err != nil {
			b.Fatalf("create file: %v", err)
		}
		if _, err := io.WriteString(file, content); err != nil {
			b.Fatalf("write file: %v", err)
		}
		if err := file.Close(); err != nil {
			b.Fatalf("close file: %v", err)
		}
		if _, err := wt.Add("tracked.txt"); err != nil {
			b.Fatalf("add file: %v", err)
		}
		if _, err := wt.Commit(fmt.Sprintf("bench commit %d", i), &git.CommitOptions{
			Author:    &objectSignature,
			Committer: &objectSignature,
		}); err != nil {
			b.Fatalf("commit: %v", err)
		}
	}
}

func copyRefsAndObjects(src, dst storer.Storer, refs []plumbing.ReferenceName) error {
	iter, err := src.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		return err
	}
	defer iter.Close()

	if err := iter.ForEach(func(obj plumbing.EncodedObject) error {
		newObj := dst.NewEncodedObject()
		newObj.SetType(obj.Type())
		newObj.SetSize(obj.Size())

		r, err := obj.Reader()
		if err != nil {
			return err
		}
		defer r.Close()

		w, err := newObj.Writer()
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, r); err != nil {
			_ = w.Close()
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		_, err = dst.SetEncodedObject(newObj)
		return err
	}); err != nil {
		return err
	}

	for _, refName := range refs {
		ref, err := src.Reference(refName)
		if err != nil {
			return err
		}
		if err := dst.SetReference(plumbing.NewHashReference(refName, ref.Hash())); err != nil {
			return err
		}
	}
	return nil
}
