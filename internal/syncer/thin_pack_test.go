package syncer

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage/memory"
)

// thinPackRecorder records, for each v2 fetch the source serves for a relay,
// whether it asked for a thin pack. Fetches with a filter decode their pack
// locally (FetchCommitParents) and are not recorded.
type thinPackRecorder struct {
	mu      sync.Mutex
	fetches []bool
}

func (r *thinPackRecorder) attach(s *smartHTTPRepoServer) {
	s.uploadPackV2FetchRaw = func(_ http.ResponseWriter, req v2TestCommandRequest, _ []byte) bool {
		if !slices.ContainsFunc(req.Args, func(arg string) bool { return strings.HasPrefix(arg, "filter ") }) {
			r.mu.Lock()
			r.fetches = append(r.fetches, slices.Contains(req.Args, "thin-pack"))
			r.mu.Unlock()
		}
		return false // let the server answer the fetch as usual
	}
}

// The relay asks the source for a thin pack when the target's receive-pack
// accepts one, and for a self-contained pack when it advertises no-thin. The
// go-git test source ignores the request and always sends a self-contained
// pack, so this test checks the request on the wire; the
// GITSYNC_E2E_GIT_HTTP_BACKEND test checks the pack git sends.
func TestRun_IntegrationRelayAsksForAThinPackOnlyWhenTheTargetAcceptsOne(t *testing.T) {
	branch := plumbing.NewBranchReferenceName(testBranch)
	feature := plumbing.NewBranchReferenceName("feature")
	for _, thin := range []bool{true, false} {
		name := "no-thin target"
		if thin {
			name = "target accepts thin packs"
		}
		t.Run(name, func(t *testing.T) {
			sourceRepo, sourceFS := newSourceRepo(t)
			makeCommits(t, sourceRepo, sourceFS, 3)
			targetRepo, err := git.Init(memory.NewStorage())
			if err != nil {
				t.Fatalf("init target repo: %v", err)
			}
			// The target holds the branch; the source adds a new branch with
			// two commits on top of it, so the relay's fetch has haves.
			if err := copyRefsAndObjects(sourceRepo.Storer, targetRepo.Storer, []plumbing.ReferenceName{branch}); err != nil {
				t.Fatalf("copy target baseline: %v", err)
			}
			base, err := sourceRepo.Reference(branch, true)
			if err != nil {
				t.Fatalf("resolve source head: %v", err)
			}
			makeCommits(t, sourceRepo, sourceFS, 2)
			head, err := sourceRepo.Reference(branch, true)
			if err != nil {
				t.Fatalf("resolve source head: %v", err)
			}
			for _, ref := range []*plumbing.Reference{
				plumbing.NewHashReference(feature, head.Hash()),
				plumbing.NewHashReference(branch, base.Hash()),
			} {
				if err := sourceRepo.Storer.SetReference(ref); err != nil {
					t.Fatalf("set %s: %v", ref.Name(), err)
				}
			}

			sourceServer := newSmartHTTPRepoServerV2(t, sourceRepo)
			targetServer := newSmartHTTPRepoServer(t, targetRepo)
			targetServer.receivePackThinCap = thin
			targetServer.receivePackNoThin = !thin
			defer sourceServer.Close()
			defer targetServer.Close()
			recorder := &thinPackRecorder{}
			recorder.attach(sourceServer)

			result, err := Run(context.Background(), Config{
				Source:       Endpoint{URL: sourceServer.RepoURL()},
				Target:       Endpoint{URL: targetServer.RepoURL()},
				Mode:         modeReplicate,
				ProtocolMode: protocolModeV2,
			})
			if err != nil {
				t.Fatalf("replicate: %v", err)
			}
			if !result.Relay {
				t.Fatalf("expected a relay, got %+v", result)
			}
			assertHeadsMatch(t, sourceRepo, targetRepo, "feature")

			recorder.mu.Lock()
			defer recorder.mu.Unlock()
			if len(recorder.fetches) != 1 || recorder.fetches[0] != thin {
				t.Fatalf("relay fetches asking for thin-pack: %v, want [%t]", recorder.fetches, thin)
			}
		})
	}
}
