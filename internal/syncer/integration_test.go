package syncer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	billy "github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/plumbing/transport"
	transportclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	transporthttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	transportserver "github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/soph/git-sync/internal/auth"
	"github.com/soph/git-sync/internal/gitproto"
	"github.com/soph/git-sync/internal/planner"
)

const testBranch = "master"

func TestRun_IntegrationInitialSyncToEmptyTarget(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 6)

	targetRepo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServer(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	result, err := Run(context.Background(), Config{
		Source: Endpoint{URL: sourceServer.RepoURL()},
		Target: Endpoint{URL: targetServer.RepoURL()},
	})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}
	if result.Pushed != 1 || result.Blocked != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if !result.Relay {
		t.Fatalf("expected sync to auto-switch to relay bootstrap on empty target")
	}
	if result.RelayReason != "empty-target-managed-refs" {
		t.Fatalf("expected bootstrap relay reason, got %+v", result)
	}

	assertHeadsMatch(t, sourceRepo, targetRepo, testBranch)

	if sourceServer.BytesOut(serviceUploadPack, metricPack) == 0 {
		t.Fatalf("expected source upload-pack response bytes")
	}
	if targetServer.Count(serviceReceivePack, metricPack) != 1 {
		t.Fatalf("expected one receive-pack POST, got %d", targetServer.Count(serviceReceivePack, metricPack))
	}
	if targetServer.BytesIn(serviceReceivePack, metricPack) == 0 {
		t.Fatalf("expected receive-pack request bytes")
	}
	if targetServer.Count(serviceUploadPack, metricPack) != 0 {
		t.Fatalf("expected no target upload-pack POSTs, got %d", targetServer.Count(serviceUploadPack, metricPack))
	}
}

func TestRun_IntegrationInitialSyncAutoFallsBackToBatchedBootstrapOnTargetBodyLimit(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeLargeCommits(t, sourceRepo, sourceFS, 20, 200_000)

	targetRepo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	targetServer.receivePackBodyLimit = 1_000_000
	defer sourceServer.Close()
	defer targetServer.Close()

	result, err := Run(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		Target:       Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode: protocolModeAuto,
	})
	if err != nil {
		t.Fatalf("initial sync with auto-batch fallback failed: %v", err)
	}
	if result.Pushed != 1 || result.Blocked != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if !result.Relay || result.RelayMode != "bootstrap-batch" || !result.Batching {
		t.Fatalf("expected batched relay fallback result, got %+v", result)
	}
	if result.BatchCount < 2 {
		t.Fatalf("expected multiple batches after size-limit fallback, got %+v", result)
	}

	assertHeadsMatch(t, sourceRepo, targetRepo, testBranch)

	if targetServer.Count(serviceReceivePack, metricPack) < 2 {
		t.Fatalf("expected fallback to retry after initial rejected push, got %d receive-pack POSTs", targetServer.Count(serviceReceivePack, metricPack))
	}
}

func TestRun_IntegrationPlanSuggestsBootstrapOnEmptyTarget(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 2)

	targetRepo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	result, err := Run(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		Target:       Endpoint{URL: targetServer.RepoURL()},
		DryRun:       true,
		ProtocolMode: protocolModeAuto,
	})
	if err != nil {
		t.Fatalf("plan failed: %v", err)
	}
	if !result.DryRun || !result.BootstrapSuggested {
		t.Fatalf("expected bootstrap suggestion, got %+v", result)
	}
	if result.RelayReason != "empty-target-managed-refs" {
		t.Fatalf("expected bootstrap suggestion reason, got %+v", result)
	}
	if result.Relay {
		t.Fatalf("dry-run plan should not execute relay")
	}
}

func TestBootstrap_IntegrationInitialSyncToEmptyTarget(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 4)

	targetRepo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	result, err := Bootstrap(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		Target:       Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode: protocolModeAuto,
		ShowStats:    true,
	})
	if err != nil {
		t.Fatalf("bootstrap failed: %v", err)
	}
	if result.Pushed != 1 || result.Blocked != 0 || len(result.Plans) != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Plans[0].Action != ActionCreate {
		t.Fatalf("expected create plan, got %+v", result.Plans[0])
	}

	assertHeadsMatch(t, sourceRepo, targetRepo, testBranch)

	if sourceServer.BytesOut(serviceUploadPack, metricPack) == 0 {
		t.Fatalf("expected source upload-pack response bytes")
	}
	if targetServer.Count(serviceReceivePack, metricPack) != 1 {
		t.Fatalf("expected one receive-pack POST, got %d", targetServer.Count(serviceReceivePack, metricPack))
	}
}

func TestBootstrap_IntegrationFailsWhenTargetRefExists(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 2)

	targetRepo, targetFS := newSourceRepo(t)
	makeCommits(t, targetRepo, targetFS, 1)

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	_, err := Bootstrap(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		Target:       Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode: protocolModeAuto,
	})
	if err == nil {
		t.Fatalf("expected bootstrap failure when target ref exists")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected existing-ref error, got %v", err)
	}
	if targetServer.Count(serviceReceivePack, metricPack) != 0 {
		t.Fatalf("expected no receive-pack POSTs, got %d", targetServer.Count(serviceReceivePack, metricPack))
	}
}

func TestBootstrap_IntegrationBranchMapping(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 3)

	targetRepo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	result, err := Bootstrap(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		Target:       Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode: protocolModeAuto,
		Mappings:     []RefMapping{{Source: "master", Target: "stable"}},
	})
	if err != nil {
		t.Fatalf("bootstrap mapping failed: %v", err)
	}
	if result.Pushed != 1 || len(result.Plans) != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Plans[0].TargetRef != plumbing.NewBranchReferenceName("stable") {
		t.Fatalf("expected stable target ref, got %+v", result.Plans[0])
	}

	sourceRef, err := sourceRepo.Reference(plumbing.NewBranchReferenceName("master"), true)
	if err != nil {
		t.Fatalf("resolve source ref: %v", err)
	}
	targetRef, err := targetRepo.Reference(plumbing.NewBranchReferenceName("stable"), true)
	if err != nil {
		t.Fatalf("resolve target ref: %v", err)
	}
	if sourceRef.Hash() != targetRef.Hash() {
		t.Fatalf("mapped target mismatch: source=%s target=%s", sourceRef.Hash(), targetRef.Hash())
	}
}

func TestBootstrap_IntegrationTags(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 2)

	head, err := sourceRepo.Reference(plumbing.NewBranchReferenceName(testBranch), true)
	if err != nil {
		t.Fatalf("source head: %v", err)
	}
	if err := sourceRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName("v1"), head.Hash())); err != nil {
		t.Fatalf("set source tag: %v", err)
	}

	targetRepo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	result, err := Bootstrap(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		Target:       Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode: protocolModeAuto,
		IncludeTags:  true,
	})
	if err != nil {
		t.Fatalf("bootstrap tags failed: %v", err)
	}
	if result.Pushed != 2 || len(result.Plans) != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, err := targetRepo.Reference(plumbing.NewTagReferenceName("v1"), true); err != nil {
		t.Fatalf("expected v1 tag on target: %v", err)
	}
}

func TestBootstrap_IntegrationPackLimit(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 2)

	targetRepo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	_, err = Bootstrap(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		Target:       Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode: protocolModeAuto,
		MaxPackBytes: 32,
	})
	if err == nil {
		t.Fatalf("expected bootstrap failure when pack exceeds threshold")
	}
	if !strings.Contains(err.Error(), "max-pack-bytes") {
		t.Fatalf("expected max-pack-bytes error, got %v", err)
	}
	if _, err := targetRepo.Reference(plumbing.NewBranchReferenceName(testBranch), true); err != plumbing.ErrReferenceNotFound {
		t.Fatalf("expected target branch to remain absent, got %v", err)
	}
}

func TestRun_IntegrationResyncFetchesLessFromSource(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 10)

	targetRepo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServer(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	if _, err := Run(context.Background(), Config{
		Source: Endpoint{URL: sourceServer.RepoURL()},
		Target: Endpoint{URL: targetServer.RepoURL()},
	}); err != nil {
		t.Fatalf("seed sync failed: %v", err)
	}

	fullSourcePackBytes := sourceServer.BytesOut(serviceUploadPack, metricPack)
	if fullSourcePackBytes == 0 {
		t.Fatalf("expected initial source upload-pack bytes")
	}
	if sourceServer.Haves(serviceUploadPack, metricPack) != 0 {
		t.Fatalf("expected no source haves on initial sync, got %d", sourceServer.Haves(serviceUploadPack, metricPack))
	}

	sourceServer.ResetMetrics()
	targetServer.ResetMetrics()

	makeCommits(t, sourceRepo, sourceFS, 1)

	result, err := Run(context.Background(), Config{
		Source: Endpoint{URL: sourceServer.RepoURL()},
		Target: Endpoint{URL: targetServer.RepoURL()},
	})
	if err != nil {
		t.Fatalf("resync failed: %v", err)
	}
	if result.Pushed != 1 || result.Blocked != 0 {
		t.Fatalf("unexpected resync result: %+v", result)
	}

	assertHeadsMatch(t, sourceRepo, targetRepo, testBranch)

	deltaSourcePackBytes := sourceServer.BytesOut(serviceUploadPack, metricPack)
	if deltaSourcePackBytes == 0 {
		t.Fatalf("expected delta source upload-pack bytes")
	}
	if sourceServer.Wants(serviceUploadPack, metricPack) == 0 {
		t.Fatalf("expected source wants on resync")
	}
	if sourceServer.Haves(serviceUploadPack, metricPack) == 0 {
		t.Fatalf("expected source fetch to advertise haves on resync")
	}

	if targetServer.Count(serviceReceivePack, metricPack) != 1 {
		t.Fatalf("expected one receive-pack POST, got %d", targetServer.Count(serviceReceivePack, metricPack))
	}
	if targetServer.Count(serviceUploadPack, metricPack) != 0 {
		t.Fatalf("expected no target upload-pack POSTs, got %d", targetServer.Count(serviceUploadPack, metricPack))
	}
}

func TestRun_IntegrationBranchMappingAndStats(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 3)

	targetRepo, _ := newSourceRepo(t)

	sourceServer := newSmartHTTPRepoServer(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	result, err := Run(context.Background(), Config{
		Source:    Endpoint{URL: sourceServer.RepoURL()},
		Target:    Endpoint{URL: targetServer.RepoURL()},
		Mappings:  []RefMapping{{Source: "master", Target: "stable"}},
		ShowStats: true,
	})
	if err != nil {
		t.Fatalf("mapped sync failed: %v", err)
	}

	sourceRef, err := sourceRepo.Reference(plumbing.NewBranchReferenceName("master"), true)
	if err != nil {
		t.Fatalf("resolve source ref: %v", err)
	}
	targetRef, err := targetRepo.Reference(plumbing.NewBranchReferenceName("stable"), true)
	if err != nil {
		t.Fatalf("resolve target ref: %v", err)
	}
	if sourceRef.Hash() != targetRef.Hash() {
		t.Fatalf("mapped target mismatch: source=%s target=%s", sourceRef.Hash(), targetRef.Hash())
	}
	if !result.Stats.Enabled || len(result.Stats.Items) == 0 {
		t.Fatalf("expected stats to be populated")
	}
}

func TestRun_IntegrationDryRunPlansWithoutPush(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 3)

	targetRepo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	result, err := Run(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		Target:       Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode: protocolModeAuto,
		DryRun:       true,
		ShowStats:    true,
	})
	if err != nil {
		t.Fatalf("dry-run plan failed: %v", err)
	}
	if !result.DryRun {
		t.Fatalf("expected dry-run result")
	}
	if result.Pushed != 0 {
		t.Fatalf("expected no pushed refs, got %+v", result)
	}
	if len(result.Plans) == 0 {
		t.Fatalf("expected at least one plan")
	}
	if targetServer.Count(serviceReceivePack, metricPack) != 0 {
		t.Fatalf("expected no receive-pack POSTs during dry-run, got %d", targetServer.Count(serviceReceivePack, metricPack))
	}
	if _, err := targetRepo.Reference(plumbing.NewBranchReferenceName(testBranch), true); err != plumbing.ErrReferenceNotFound {
		t.Fatalf("expected target branch to remain absent, got %v", err)
	}
}

func TestRun_IntegrationUsesGitCredentialHelperFallback(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 2)

	targetRepo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	const username = "oauth2"
	const password = "helper-secret"

	sourceServer := newAuthenticatedSmartHTTPRepoServer(t, sourceRepo, username, password)
	targetServer := newAuthenticatedSmartHTTPRepoServer(t, targetRepo, username, password)
	defer sourceServer.Close()
	defer targetServer.Close()

	originalFill := auth.GitCredentialFillCommand
	t.Cleanup(func() {
		auth.GitCredentialFillCommand = originalFill
	})
	auth.GitCredentialFillCommand = func(ctx context.Context, input string) ([]byte, error) {
		if !strings.Contains(input, "protocol=http\n") {
			t.Fatalf("expected protocol in credential input, got %q", input)
		}
		if !strings.Contains(input, "host=") {
			t.Fatalf("expected host in credential input, got %q", input)
		}
		if !strings.Contains(input, "path=repo.git\n") {
			t.Fatalf("expected repo path in credential input, got %q", input)
		}
		return []byte("username=" + username + "\npassword=" + password + "\n\n"), nil
	}

	result, err := Run(context.Background(), Config{
		Source: Endpoint{URL: sourceServer.RepoURL()},
		Target: Endpoint{URL: targetServer.RepoURL()},
	})
	if err != nil {
		t.Fatalf("sync with credential helper failed: %v", err)
	}
	if result.Pushed != 1 || result.Blocked != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}

	assertHeadsMatch(t, sourceRepo, targetRepo, testBranch)
}

func TestRun_IntegrationProtocolV2Source(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 4)

	targetRepo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	result, err := Run(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		Target:       Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode: protocolModeV2,
	})
	if err != nil {
		t.Fatalf("initial v2 sync failed: %v", err)
	}
	if result.Protocol != protocolModeV2 {
		t.Fatalf("expected protocol v2, got %s", result.Protocol)
	}

	assertHeadsMatch(t, sourceRepo, targetRepo, testBranch)

	sourceServer.ResetMetrics()
	makeCommits(t, sourceRepo, sourceFS, 1)

	result, err = Run(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		Target:       Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode: protocolModeV2,
	})
	if err != nil {
		t.Fatalf("resync v2 failed: %v", err)
	}
	if result.Protocol != protocolModeV2 {
		t.Fatalf("expected protocol v2 on resync, got %s", result.Protocol)
	}
	if sourceServer.Haves(serviceUploadPack, metricPack) == 0 {
		t.Fatalf("expected protocol v2 fetch to advertise haves on resync")
	}
}

func TestProbe_IntegrationProtocolV2Source(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 2)
	head, err := sourceRepo.Reference(plumbing.NewBranchReferenceName(testBranch), true)
	if err != nil {
		t.Fatalf("source head: %v", err)
	}
	if err := sourceRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName("v1"), head.Hash())); err != nil {
		t.Fatalf("set source tag: %v", err)
	}

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	defer sourceServer.Close()

	result, err := Probe(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		ProtocolMode: protocolModeV2,
		IncludeTags:  true,
		ShowStats:    true,
	})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}

	if result.Protocol != protocolModeV2 {
		t.Fatalf("expected protocol v2, got %s", result.Protocol)
	}
	if len(result.RefPrefixes) != 2 || result.RefPrefixes[0] != "refs/heads/" || result.RefPrefixes[1] != "refs/tags/" {
		t.Fatalf("unexpected ref prefixes: %#v", result.RefPrefixes)
	}
	if len(result.Capabilities) == 0 {
		t.Fatalf("expected capabilities")
	}
	if len(result.Refs) < 2 {
		t.Fatalf("expected refs, got %#v", result.Refs)
	}
	if !result.Stats.Enabled || len(result.Stats.Items) == 0 {
		t.Fatalf("expected stats")
	}
}

func TestProbe_IntegrationTargetCapabilities(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 1)

	targetRepo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	result, err := Probe(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		Target:       Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode: protocolModeAuto,
		ShowStats:    true,
	})
	if err != nil {
		t.Fatalf("probe with target failed: %v", err)
	}
	if result.TargetURL != targetServer.RepoURL() {
		t.Fatalf("unexpected target url %q", result.TargetURL)
	}
	if len(result.TargetCaps) == 0 {
		t.Fatalf("expected target capabilities")
	}
}

func TestFetch_IntegrationProtocolV2Source(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 4)

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	defer sourceServer.Close()

	result, err := Fetch(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		ProtocolMode: protocolModeV2,
		Branches:     []string{testBranch},
		ShowStats:    true,
	}, nil, nil)
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if result.Protocol != protocolModeV2 {
		t.Fatalf("expected protocol v2, got %s", result.Protocol)
	}
	if len(result.Wants) != 1 {
		t.Fatalf("expected one wanted ref, got %#v", result.Wants)
	}
	if result.FetchedObjects == 0 {
		t.Fatalf("expected fetched objects")
	}
	if !result.Stats.Enabled || len(result.Stats.Items) == 0 {
		t.Fatalf("expected stats")
	}

	sourceServer.ResetMetrics()
	result, err = Fetch(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		ProtocolMode: protocolModeV2,
		Branches:     []string{testBranch},
		ShowStats:    true,
	}, []string{testBranch}, nil)
	if err != nil {
		t.Fatalf("fetch with haves failed: %v", err)
	}
	if len(result.Haves) != 1 {
		t.Fatalf("expected one have, got %#v", result.Haves)
	}
	if sourceServer.Haves(serviceUploadPack, metricPack) == 0 {
		t.Fatalf("expected source fetch to advertise haves")
	}
}

func TestRun_IntegrationTagsPruneAndForce(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 2)
	targetRepo, targetFS := newSourceRepo(t)

	sourceServer := newSmartHTTPRepoServer(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	if _, err := Run(context.Background(), Config{
		Source: Endpoint{URL: sourceServer.RepoURL()},
		Target: Endpoint{URL: targetServer.RepoURL()},
	}); err != nil {
		t.Fatalf("seed sync failed: %v", err)
	}

	head, err := sourceRepo.Reference(plumbing.NewBranchReferenceName(testBranch), true)
	if err != nil {
		t.Fatalf("source head: %v", err)
	}
	if err := sourceRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName("v1"), head.Hash())); err != nil {
		t.Fatalf("set source tag: %v", err)
	}
	if err := sourceRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName("old"), head.Hash())); err != nil {
		t.Fatalf("set source old tag: %v", err)
	}

	if err := targetRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName("stale"), head.Hash())); err != nil {
		t.Fatalf("set stale target tag: %v", err)
	}

	if _, err := Run(context.Background(), Config{
		Source:      Endpoint{URL: sourceServer.RepoURL()},
		Target:      Endpoint{URL: targetServer.RepoURL()},
		IncludeTags: true,
		Prune:       true,
	}); err != nil {
		t.Fatalf("tag sync failed: %v", err)
	}

	if _, err := targetRepo.Reference(plumbing.NewTagReferenceName("v1"), true); err != nil {
		t.Fatalf("expected v1 tag on target: %v", err)
	}
	if _, err := targetRepo.Reference(plumbing.NewTagReferenceName("stale"), true); err != plumbing.ErrReferenceNotFound {
		t.Fatalf("expected stale tag to be pruned, got %v", err)
	}

	makeCommits(t, sourceRepo, sourceFS, 1)
	makeCommits(t, targetRepo, targetFS, 1)

	if _, err := Run(context.Background(), Config{
		Source: Endpoint{URL: sourceServer.RepoURL()},
		Target: Endpoint{URL: targetServer.RepoURL()},
	}); err == nil {
		t.Fatalf("expected divergent sync without force to fail")
	}

	if _, err := Run(context.Background(), Config{
		Source: Endpoint{URL: sourceServer.RepoURL()},
		Target: Endpoint{URL: targetServer.RepoURL()},
		Force:  true,
	}); err != nil {
		t.Fatalf("expected forced sync to succeed: %v", err)
	}

	assertHeadsMatch(t, sourceRepo, targetRepo, testBranch)
}

func TestRun_IntegrationAddAnnotatedTagAfterInitialBranchSync(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 2)

	targetRepo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	if _, err := Run(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		Target:       Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode: protocolModeV2,
	}); err != nil {
		t.Fatalf("initial branch sync failed: %v", err)
	}

	head, err := sourceRepo.Reference(plumbing.NewBranchReferenceName(testBranch), true)
	if err != nil {
		t.Fatalf("source head: %v", err)
	}
	if _, err := sourceRepo.CreateTag("annotated-v1", head.Hash(), &git.CreateTagOptions{
		Tagger:  &objectSignature,
		Message: "annotated release",
	}); err != nil {
		t.Fatalf("create annotated tag: %v", err)
	}

	result, err := Run(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		Target:       Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode: protocolModeV2,
		IncludeTags:  true,
	})
	if err != nil {
		t.Fatalf("annotated tag follow-up sync failed: %v", err)
	}
	if result.Pushed == 0 {
		t.Fatalf("expected follow-up sync to push annotated tag, got %+v", result)
	}
	if _, err := targetRepo.Reference(plumbing.NewTagReferenceName("annotated-v1"), true); err != nil {
		t.Fatalf("expected annotated tag on target: %v", err)
	}
}

func TestRun_IntegrationAddHistoricalAnnotatedTagAfterInitialBranchSync_NoThinTarget(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 4)

	targetRepo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	targetServer.receivePackNoThin = true
	defer sourceServer.Close()
	defer targetServer.Close()

	if _, err := Run(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		Target:       Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode: protocolModeV2,
	}); err != nil {
		t.Fatalf("initial branch sync failed: %v", err)
	}

	head, err := sourceRepo.Reference(plumbing.NewBranchReferenceName(testBranch), true)
	if err != nil {
		t.Fatalf("source head: %v", err)
	}
	chain, err := planner.FirstParentChain(sourceRepo.Storer, head.Hash())
	if err != nil {
		t.Fatalf("build commit chain: %v", err)
	}
	if len(chain) < 2 {
		t.Fatalf("expected historical commit chain, got %d", len(chain))
	}
	if _, err := sourceRepo.CreateTag("annotated-old", chain[1], &git.CreateTagOptions{
		Tagger:  &objectSignature,
		Message: "historical release",
	}); err != nil {
		t.Fatalf("create historical annotated tag: %v", err)
	}

	result, err := Run(context.Background(), Config{
		Source:       Endpoint{URL: sourceServer.RepoURL()},
		Target:       Endpoint{URL: targetServer.RepoURL()},
		ProtocolMode: protocolModeV2,
		IncludeTags:  true,
	})
	if err != nil {
		t.Fatalf("historical annotated tag follow-up sync failed: %v", err)
	}
	if result.Pushed == 0 {
		t.Fatalf("expected follow-up sync to push historical annotated tag, got %+v", result)
	}
	if _, err := targetRepo.Reference(plumbing.NewTagReferenceName("annotated-old"), true); err != nil {
		t.Fatalf("expected historical annotated tag on target: %v", err)
	}
}

func newSourceRepo(t *testing.T) (*git.Repository, billy.Filesystem) {
	t.Helper()

	fs := memfs.New()
	repo, err := git.Init(memory.NewStorage(), fs)
	if err != nil {
		t.Fatalf("init source repo: %v", err)
	}

	return repo, fs
}

func makeCommits(t *testing.T, repo *git.Repository, fs billy.Filesystem, count int) {
	t.Helper()

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("open worktree: %v", err)
	}

	for i := 0; i < count; i++ {
		content := strings.Repeat(fmt.Sprintf("line %d %d\n", i, time.Now().UnixNano()), 24)
		file, err := fs.Create("tracked.txt")
		if err != nil {
			t.Fatalf("create file: %v", err)
		}
		if _, err := io.WriteString(file, content); err != nil {
			t.Fatalf("write file: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close file: %v", err)
		}

		if _, err := wt.Add("tracked.txt"); err != nil {
			t.Fatalf("add file: %v", err)
		}

		_, err = wt.Commit(fmt.Sprintf("commit %d", i), &git.CommitOptions{
			Author:    &objectSignature,
			Committer: &objectSignature,
		})
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
}

func makeLargeCommits(t *testing.T, repo *git.Repository, fs billy.Filesystem, count int, blobSize int) {
	t.Helper()

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("open worktree: %v", err)
	}

	for i := 0; i < count; i++ {
		name := fmt.Sprintf("blob-%d.bin", i)
		file, err := fs.Create(name)
		if err != nil {
			t.Fatalf("create file: %v", err)
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
			t.Fatalf("write file: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close file: %v", err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("add file: %v", err)
		}

		_, err = wt.Commit(fmt.Sprintf("large commit %d", i), &git.CommitOptions{
			Author:    &objectSignature,
			Committer: &objectSignature,
		})
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
}

var objectSignature = signature()

func signature() object.Signature {
	return object.Signature{
		Name:  "test",
		Email: "test@example.com",
		When:  time.Unix(1, 0).UTC(),
	}
}

func assertHeadsMatch(t *testing.T, sourceRepo, targetRepo *git.Repository, branch string) {
	t.Helper()

	sourceRef, err := sourceRepo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		t.Fatalf("resolve source ref: %v", err)
	}
	targetRef, err := targetRepo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		t.Fatalf("resolve target ref: %v", err)
	}
	if sourceRef.Hash() != targetRef.Hash() {
		t.Fatalf("branch %s mismatch: source=%s target=%s", branch, sourceRef.Hash(), targetRef.Hash())
	}
}

type metricKind string

const (
	serviceUploadPack  = transport.UploadPackServiceName
	serviceReceivePack = transport.ReceivePackServiceName

	metricInfoRefs metricKind = "info_refs"
	metricPack     metricKind = "pack"
)

type exchangeMetric struct {
	service string
	kind    metricKind
	in      int64
	out     int64
	wants   int
	haves   int
}

type smartHTTPRepoServer struct {
	t        *testing.T
	server   *httptest.Server
	repo     *git.Repository
	repoPath string
	v2       bool
	username string
	password string

	receivePackBodyLimit int64
	receivePackNoThin    bool

	mu      sync.Mutex
	metrics []exchangeMetric
}

func newSmartHTTPRepoServer(t *testing.T, repo *git.Repository) *smartHTTPRepoServer {
	t.Helper()

	s := &smartHTTPRepoServer{
		t:        t,
		repo:     repo,
		repoPath: "/repo.git",
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func newSmartHTTPRepoServerV2(t *testing.T, repo *git.Repository) *smartHTTPRepoServer {
	t.Helper()

	s := newSmartHTTPRepoServer(t, repo)
	s.v2 = true
	return s
}

func newAuthenticatedSmartHTTPRepoServer(t *testing.T, repo *git.Repository, username, password string) *smartHTTPRepoServer {
	t.Helper()

	s := newSmartHTTPRepoServer(t, repo)
	s.username = username
	s.password = password
	return s
}

func (s *smartHTTPRepoServer) Close() {
	s.server.Close()
}

func (s *smartHTTPRepoServer) RepoURL() string {
	return s.server.URL + s.repoPath
}

func (s *smartHTTPRepoServer) ResetMetrics() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics = nil
}

func (s *smartHTTPRepoServer) Count(service string, kind metricKind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, metric := range s.metrics {
		if metric.service == service && metric.kind == kind {
			count++
		}
	}
	return count
}

func (s *smartHTTPRepoServer) BytesIn(service string, kind metricKind) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, metric := range s.metrics {
		if metric.service == service && metric.kind == kind {
			total += metric.in
		}
	}
	return total
}

func (s *smartHTTPRepoServer) BytesOut(service string, kind metricKind) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, metric := range s.metrics {
		if metric.service == service && metric.kind == kind {
			total += metric.out
		}
	}
	return total
}

func (s *smartHTTPRepoServer) Wants(service string, kind metricKind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, metric := range s.metrics {
		if metric.service == service && metric.kind == kind {
			total += metric.wants
		}
	}
	return total
}

func (s *smartHTTPRepoServer) Haves(service string, kind metricKind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, metric := range s.metrics {
		if metric.service == service && metric.kind == kind {
			total += metric.haves
		}
	}
	return total
}

func (s *smartHTTPRepoServer) handle(w http.ResponseWriter, r *http.Request) {
	if s.username != "" || s.password != "" {
		username, password, ok := r.BasicAuth()
		if !ok || username != s.username || password != s.password {
			w.Header().Set("WWW-Authenticate", `Basic realm="git-sync-test"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == s.repoPath+"/info/refs":
		s.handleInfoRefs(w, r)
	case r.Method == http.MethodPost && r.URL.Path == s.repoPath+"/"+serviceUploadPack:
		s.handleUploadPack(w, r)
	case r.Method == http.MethodPost && r.URL.Path == s.repoPath+"/"+serviceReceivePack:
		s.handleReceivePack(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *smartHTTPRepoServer) handleInfoRefs(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	if service != serviceUploadPack && service != serviceReceivePack {
		http.Error(w, "missing service", http.StatusBadRequest)
		return
	}
	if s.v2 && service == serviceUploadPack && strings.Contains(r.Header.Get("Git-Protocol"), "version=2") {
		s.handleInfoRefsV2(w, r)
		return
	}

	session, err := s.newSession(service)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var ar *packp.AdvRefs
	switch service {
	case serviceUploadPack:
		ar, err = session.(transport.UploadPackSession).AdvertisedReferencesContext(r.Context())
	case serviceReceivePack:
		ar, err = session.(transport.ReceivePackSession).AdvertisedReferencesContext(r.Context())
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if service == serviceReceivePack && s.receivePackNoThin {
		_ = ar.Capabilities.Set(capability.Capability("no-thin"))
	}

	ar.Prefix = [][]byte{
		[]byte("# service=" + service),
		pktline.Flush,
	}

	var buf bytes.Buffer
	if err := ar.Encode(&buf); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", service))
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.t.Fatalf("write advertised refs: %v", err)
	}

	s.recordMetric(service, metricInfoRefs, 0, int64(buf.Len()), 0, 0)
}

func (s *smartHTTPRepoServer) handleInfoRefsV2(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	enc := pktline.NewEncoder(&buf)
	lines := []string{
		"version 2\n",
		"ls-refs=unborn\n",
		"fetch=thin-pack filter\n",
		"agent=test-server\n",
	}
	for _, line := range lines {
		if err := enc.EncodeString(line); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := enc.Flush(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", serviceUploadPack))
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.t.Fatalf("write v2 advertised refs: %v", err)
	}

	s.recordMetric(serviceUploadPack, metricInfoRefs, 0, int64(buf.Len()), 0, 0)
}

func (s *smartHTTPRepoServer) handleUploadPack(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()
	if s.v2 && strings.Contains(r.Header.Get("Git-Protocol"), "version=2") {
		s.handleUploadPackV2(w, r, body)
		return
	}

	session, err := s.newSession(serviceUploadPack)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	req := packp.NewUploadPackRequest()
	if err := req.Decode(bytes.NewReader(body)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp, err := session.(transport.UploadPackSession).UploadPack(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := resp.Encode(&buf); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-result", serviceUploadPack))
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.t.Fatalf("write upload-pack response: %v", err)
	}

	s.recordMetric(serviceUploadPack, metricPack, int64(len(body)), int64(buf.Len()), len(req.Wants), strings.Count(string(body), "have "))
}

func (s *smartHTTPRepoServer) handleUploadPackV2(w http.ResponseWriter, r *http.Request, body []byte) {
	req, err := decodeV2TestCommandRequest(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	switch req.Command {
	case "ls-refs":
		s.handleUploadPackV2LSRefs(w, req, body)
	case "fetch":
		s.handleUploadPackV2Fetch(w, req, body)
	default:
		http.Error(w, "unsupported v2 command", http.StatusBadRequest)
	}
}

func (s *smartHTTPRepoServer) handleUploadPackV2LSRefs(w http.ResponseWriter, req v2TestCommandRequest, body []byte) {
	prefixes := make([]string, 0, len(req.Args))
	for _, arg := range req.Args {
		if strings.HasPrefix(arg, "ref-prefix ") {
			prefixes = append(prefixes, strings.TrimPrefix(arg, "ref-prefix "))
		}
	}

	refs, err := s.refsMatchingPrefixes(prefixes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	enc := pktline.NewEncoder(&buf)
	for _, ref := range refs {
		if err := enc.EncodeString(ref.Hash().String() + " " + ref.Name().String() + "\n"); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := enc.Flush(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-result", serviceUploadPack))
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.t.Fatalf("write v2 ls-refs response: %v", err)
	}

	s.recordMetric(serviceUploadPack, metricPack, int64(len(body)), int64(buf.Len()), 0, 0)
}

func (s *smartHTTPRepoServer) handleUploadPackV2Fetch(w http.ResponseWriter, req v2TestCommandRequest, body []byte) {
	var wants []plumbing.Hash
	var haves []plumbing.Hash
	for _, arg := range req.Args {
		switch {
		case strings.HasPrefix(arg, "want "):
			wants = append(wants, plumbing.NewHash(strings.TrimPrefix(arg, "want ")))
		case strings.HasPrefix(arg, "have "):
			haves = append(haves, plumbing.NewHash(strings.TrimPrefix(arg, "have ")))
		}
	}

	hashes, err := revlist.Objects(s.repo.Storer, wants, haves)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var pack bytes.Buffer
	enc := packfile.NewEncoder(&pack, s.repo.Storer, false)
	if _, err := enc.Encode(hashes, 10); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	pkt := pktline.NewEncoder(&buf)
	if err := pkt.EncodeString("packfile\n"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for offset := 0; offset < pack.Len(); offset += 65515 {
		end := offset + 65515
		if end > pack.Len() {
			end = pack.Len()
		}
		payload := append([]byte{1}, pack.Bytes()[offset:end]...)
		if err := pkt.Encode(payload); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := pkt.Flush(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-result", serviceUploadPack))
	if _, err := w.Write(buf.Bytes()); err != nil {
		if isConnectionCloseError(err) {
			return
		}
		s.t.Fatalf("write v2 fetch response: %v", err)
	}

	s.recordMetric(serviceUploadPack, metricPack, int64(len(body)), int64(buf.Len()), len(wants), len(haves))
}

func (s *smartHTTPRepoServer) handleReceivePack(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	if s.receivePackBodyLimit > 0 && int64(len(body)) > s.receivePackBodyLimit {
		report := packp.NewReportStatus()
		report.UnpackStatus = fmt.Sprintf("push rejected: body exceeded size limit %d (trace_id=00000000000000000000000000000000)", s.receivePackBodyLimit)

		var buf bytes.Buffer
		if err := report.Encode(&buf); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-result", serviceReceivePack))
		if _, err := w.Write(buf.Bytes()); err != nil {
			s.t.Fatalf("write receive-pack rejection: %v", err)
		}
		s.recordMetric(serviceReceivePack, metricPack, int64(len(body)), int64(buf.Len()), 0, 0)
		return
	}

	session, err := s.newSession(serviceReceivePack)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	req := packp.NewReferenceUpdateRequest()
	if err := req.Decode(bytes.NewReader(body)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if !bytes.Contains(body, []byte("PACK")) {
		report := packp.NewReportStatus()
		report.UnpackStatus = "ok"
		for _, cmd := range req.Commands {
			report.CommandStatuses = append(report.CommandStatuses, &packp.CommandStatus{
				ReferenceName: cmd.Name,
				Status:        "ok",
			})
			if cmd.New.IsZero() {
				if err := s.repo.Storer.RemoveReference(cmd.Name); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				continue
			}
			if err := s.repo.Storer.SetReference(plumbing.NewHashReference(cmd.Name, cmd.New)); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		var buf bytes.Buffer
		if err := report.Encode(&buf); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-result", serviceReceivePack))
		if _, err := w.Write(buf.Bytes()); err != nil {
			s.t.Fatalf("write receive-pack command response: %v", err)
		}
		s.recordMetric(serviceReceivePack, metricPack, int64(len(body)), int64(buf.Len()), 0, 0)
		return
	}

	report, err := session.(transport.ReceivePackSession).ReceivePack(r.Context(), req)

	var buf bytes.Buffer
	if report != nil {
		if err := report.Encode(&buf); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-result", serviceReceivePack))
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.t.Fatalf("write receive-pack response: %v", err)
	}
	if err != nil {
		return
	}

	s.recordMetric(serviceReceivePack, metricPack, int64(len(body)), int64(buf.Len()), 0, 0)
}

func (s *smartHTTPRepoServer) newSession(service string) (interface{}, error) {
	loader := transportserver.MapLoader{}

	endpoint, err := transport.NewEndpoint(s.RepoURL())
	if err != nil {
		return nil, err
	}
	loader[endpoint.String()] = s.repo.Storer

	srv := transportserver.NewServer(loader)
	switch service {
	case serviceUploadPack:
		return srv.NewUploadPackSession(endpoint, nil)
	case serviceReceivePack:
		return srv.NewReceivePackSession(endpoint, nil)
	default:
		return nil, fmt.Errorf("unknown service %q", service)
	}
}

func (s *smartHTTPRepoServer) recordMetric(service string, kind metricKind, in, out int64, wants, haves int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics = append(s.metrics, exchangeMetric{
		service: service,
		kind:    kind,
		in:      in,
		out:     out,
		wants:   wants,
		haves:   haves,
	})
}

func isConnectionCloseError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset by peer")
}

func (s *smartHTTPRepoServer) refsMatchingPrefixes(prefixes []string) ([]*plumbing.Reference, error) {
	iter, err := s.repo.Storer.IterReferences()
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var refs []*plumbing.Reference
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference {
			return nil
		}
		if len(prefixes) > 0 {
			matched := false
			for _, prefix := range prefixes {
				if strings.HasPrefix(ref.Name().String(), prefix) {
					matched = true
					break
				}
			}
			if !matched {
				return nil
			}
		}
		refs = append(refs, ref)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(refs, func(i, j int) bool {
		return refs[i].Name().String() < refs[j].Name().String()
	})
	return refs, nil
}

type v2TestCommandRequest struct {
	Command string
	Args    []string
}

func decodeV2TestCommandRequest(body []byte) (v2TestCommandRequest, error) {
	reader := gitproto.NewPacketReader(bytes.NewReader(body))
	req := v2TestCommandRequest{}
	inArgs := false

	for {
		kind, payload, err := reader.ReadPacket()
		if err != nil {
			return req, err
		}
		switch kind {
		case gitproto.PacketFlush:
			return req, nil
		case gitproto.PacketDelim:
			inArgs = true
		case gitproto.PacketData:
			line := strings.TrimSuffix(string(payload), "\n")
			if strings.HasPrefix(line, "command=") {
				req.Command = strings.TrimPrefix(line, "command=")
				continue
			}
			if inArgs {
				req.Args = append(req.Args, line)
			}
		default:
			return req, fmt.Errorf("unexpected packet type %v", kind)
		}
	}
}

func TestMain(m *testing.M) {
	originalHTTP := transportclient.Protocols["http"]
	originalHTTPS := transportclient.Protocols["https"]

	customHTTP := transporthttp.NewClient(&http.Client{})
	transportclient.InstallProtocol("http", customHTTP)
	transportclient.InstallProtocol("https", customHTTP)

	code := m.Run()

	transportclient.InstallProtocol("http", originalHTTP)
	transportclient.InstallProtocol("https", originalHTTPS)

	os.Exit(code)
}
