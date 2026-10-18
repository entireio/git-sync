package syncer

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
)

// Interleave complete syncs at the discovery boundaries without timing or sleeps.
// The target fixture enforces the expected-old checks used by receive-pack.
func TestRun_ConcurrentSyncRefSnapshots(t *testing.T) {
	for _, afterSource := range []bool{false, true} {
		name := "newer_sync_before_target_read"
		if afterSource {
			name = "newer_sync_after_source_read"
		}
		t.Run(name, func(t *testing.T) {
			source, sourceFS := newSourceRepo(t)
			target, _ := newSourceRepo(t)
			makeCommits(t, source, sourceFS, 2)
			sourceServer := newSmartHTTPRepoServer(t, source)
			targetServer := newSmartHTTPRepoServer(t, target)
			t.Cleanup(sourceServer.Close)
			t.Cleanup(targetServer.Close)
			targetServer.receivePackThinCap = true
			targetServer.receivePackUnpackForHook = true
			targetServer.receivePackHook = func(req *packp.UpdateRequests, _ bool) *packp.ReportStatus {
				return applyCommandsExcept(target.Storer, req, "", "")
			}
			cfg := Config{
				Source:       Endpoint{URL: sourceServer.RepoURL()},
				Target:       Endpoint{URL: targetServer.RepoURL()},
				Mode:         modeReplicate,
				AllRefs:      true,
				Prune:        true,
				ProtocolMode: protocolModeV1,
			}
			if _, err := Run(t.Context(), cfg); err != nil {
				t.Fatalf("initial sync: %v", err)
			}
			if afterSource {
				// Give the older run a non-noop update to attempt after the
				// newer run finishes. Its expected-old hash must stay stale.
				makeCommits(t, source, sourceFS, 1)
			}

			feature := plumbing.NewBranchReferenceName("new-branch")
			triggered := false
			runNewerSync := func() {
				triggered = true
				makeCommits(t, source, sourceFS, 1)
				head, err := source.Reference(plumbing.NewBranchReferenceName(testBranch), true)
				if err != nil {
					t.Fatal(err)
				}
				if err := source.Storer.SetReference(plumbing.NewHashReference(feature, head.Hash())); err != nil {
					t.Fatal(err)
				}
				if _, err := Run(t.Context(), cfg); err != nil {
					t.Fatalf("newer sync: %v", err)
				}
				assertHeadsMatch(t, source, target, testBranch)
			}

			older := cfg
			older.HTTPClient = &http.Client{Transport: snapshotTransport(func(req *http.Request) (*http.Response, error) {
				if !triggered && !afterSource && req.Method == http.MethodGet && strings.HasPrefix(req.URL.String(), cfg.Target.URL) {
					runNewerSync()
				}
				resp, err := http.DefaultTransport.RoundTrip(req)
				if err != nil {
					return resp, err
				}
				if !triggered && afterSource && req.Method == http.MethodGet && strings.HasPrefix(req.URL.String(), cfg.Source.URL) {
					// Keep the older source advertisement while the newer
					// sync advances both repositories.
					body, readErr := io.ReadAll(resp.Body)
					closeErr := resp.Body.Close()
					if readErr != nil {
						return nil, readErr
					}
					if closeErr != nil {
						return nil, closeErr
					}
					resp.Body = io.NopCloser(bytes.NewReader(body))
					runNewerSync()
				}
				return resp, nil
			})}
			result, err := Run(t.Context(), older)
			if !triggered {
				t.Fatal("concurrent sync was not triggered")
			}
			// Check both the branch tip and prune: an older source snapshot
			// must neither rewind the main branch nor delete the new branch.
			assertHeadsMatch(t, source, target, testBranch)
			assertHeadsMatch(t, source, target, feature.Short())
			if afterSource {
				if err == nil || !strings.Contains(err.Error(), "stale info") {
					t.Fatalf("expected stale ref rejection, got %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("older sync: %v", err)
				}
				if result.Pushed != 0 || result.Deleted != 0 {
					t.Fatalf("expected no changes after newer sync, got %+v", result)
				}
			}
		})
	}
}

type snapshotTransport func(*http.Request) (*http.Response, error)

func (f snapshotTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
