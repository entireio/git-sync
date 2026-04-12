package syncer

import (
	"context"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/storage/memory"
	"github.com/soph/git-sync/internal/syncertest"
	"io"
	"testing"
)

func BenchmarkRunBootstrapEmptyTarget(b *testing.B) {
	b.ReportAllocs()

	sourceRepo, sourceFS := syncertest.NewMemoryRepo(b)
	syncertest.MakeBenchmarkCommits(b, sourceRepo, sourceFS, 8)
	sourceServer := newSmartHTTPRepoServerV2(b, sourceRepo)
	defer sourceServer.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		targetRepo, err := git.Init(memory.NewStorage())
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
		sourceRepo, sourceFS := syncertest.NewMemoryRepo(b)
		syncertest.MakeBenchmarkCommits(b, sourceRepo, sourceFS, 2)

		targetRepo, err := git.Init(memory.NewStorage())
		if err != nil {
			b.Fatalf("init target repo: %v", err)
		}
		if err := copyRefsAndObjects(sourceRepo.Storer, targetRepo.Storer, []plumbing.ReferenceName{plumbing.NewBranchReferenceName(testBranch)}); err != nil {
			b.Fatalf("copy target baseline: %v", err)
		}

		syncertest.MakeBenchmarkCommits(b, sourceRepo, sourceFS, 1)

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
		sourceRepo, sourceFS := syncertest.NewMemoryRepo(b)
		syncertest.MakeBenchmarkCommits(b, sourceRepo, sourceFS, 3)

		targetRepo, err := git.Init(memory.NewStorage())
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
