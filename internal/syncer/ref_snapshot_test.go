package syncer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/syncertest"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
)

// Interleave complete syncs at the discovery boundaries without timing or sleeps.
// The target fixture enforces the expected-old checks used by receive-pack.
func TestRun_ConcurrentSyncRefSnapshots(t *testing.T) {
	const (
		beforeTarget = "before_target"
		afterTarget  = "after_target"
		afterSource  = "after_source"
	)
	for _, tc := range []struct {
		name         string
		boundary     string
		bestEffort   bool
		mode         string
		emptyTarget  bool
		materialized bool
	}{
		{name: "newer_sync_before_target_read", boundary: beforeTarget},
		{name: "newer_sync_after_source_read", boundary: afterSource},
		{name: "same_update_after_target_read", boundary: afterTarget},
		{name: "same_update_best_effort", boundary: afterTarget, bestEffort: true},
		{name: "same_update_sync", boundary: afterTarget, mode: modeSync, bestEffort: true},
		{name: "same_update_materialized", boundary: afterTarget, mode: modeSync, materialized: true},
		{name: "same_create_bootstrap", boundary: afterTarget, emptyTarget: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, sourceFS := newSourceRepo(t)
			target, _ := newSourceRepo(t)
			makeCommits(t, source, sourceFS, 2)
			sourceServer := newSmartHTTPRepoServer(t, source)
			targetServer := newSmartHTTPRepoServer(t, target)
			t.Cleanup(sourceServer.Close)
			t.Cleanup(targetServer.Close)
			targetServer.receivePackThinCap = !tc.materialized
			targetServer.receivePackNoThin = tc.materialized
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
				BestEffort:   tc.bestEffort,
				ProtocolMode: protocolModeV1,
			}
			if tc.mode != "" {
				cfg.Mode = tc.mode
				cfg.ForceWithLease = tc.bestEffort
			}
			if !tc.emptyTarget {
				if _, err := Run(t.Context(), cfg); err != nil {
					t.Fatalf("initial sync: %v", err)
				}
			}
			if tc.boundary == afterSource {
				makeCommits(t, source, sourceFS, 1)
			}
			// Prepare both snapshots on the test goroutine. The transport hook
			// only changes refs and returns errors; it never calls testing.Fatal.
			main := plumbing.NewBranchReferenceName(testBranch)
			oldHead, err := source.Reference(main, true)
			if err != nil {
				t.Fatal(err)
			}
			makeCommits(t, source, sourceFS, 1)
			newHead, err := source.Reference(main, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := source.Storer.SetReference(oldHead); err != nil {
				t.Fatal(err)
			}
			feature := plumbing.NewBranchReferenceName("new-branch")
			var mu sync.Mutex
			triggered := false
			var newerErr error
			runNewerSync := func() error {
				mu.Lock()
				defer mu.Unlock()
				if triggered {
					return newerErr
				}
				triggered = true
				for _, ref := range []plumbing.ReferenceName{main, feature} {
					if err := source.Storer.SetReference(plumbing.NewHashReference(ref, newHead.Hash())); err != nil {
						newerErr = err
						return err
					}
				}
				_, newerErr = Run(t.Context(), cfg)
				return newerErr
			}

			sourceURL, err := url.Parse(cfg.Source.URL)
			if err != nil {
				t.Fatal(err)
			}
			targetURL, err := url.Parse(cfg.Target.URL)
			if err != nil {
				t.Fatal(err)
			}
			isDiscovery := func(req *http.Request, endpoint *url.URL, service string) bool {
				return req.Method == http.MethodGet && req.URL.Scheme == endpoint.Scheme &&
					req.URL.Host == endpoint.Host && req.URL.Path == endpoint.Path+"/info/refs" &&
					req.URL.Query().Get("service") == service
			}
			older := cfg
			older.HTTPClient = &http.Client{Transport: syncertest.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				targetRead := isDiscovery(req, targetURL, "git-receive-pack")
				sourceRead := isDiscovery(req, sourceURL, "git-upload-pack")
				if tc.boundary == beforeTarget && targetRead {
					if err := runNewerSync(); err != nil {
						return nil, fmt.Errorf("interleaved sync: %w", err)
					}
				}
				resp, err := http.DefaultTransport.RoundTrip(req)
				if err != nil {
					return resp, err
				}
				if (tc.boundary == afterSource && sourceRead) || (tc.boundary == afterTarget && targetRead) {
					// Preserve this advertisement while the newer sync finishes.
					body, readErr := io.ReadAll(resp.Body)
					closeErr := resp.Body.Close()
					if err := errors.Join(readErr, closeErr); err != nil {
						return nil, err
					}
					if err := runNewerSync(); err != nil {
						return nil, fmt.Errorf("interleaved sync: %w", err)
					}
					resp.Body = io.NopCloser(bytes.NewReader(body))
				}
				return resp, nil
			})}
			result, err := Run(t.Context(), older)
			mu.Lock()
			fired, hookErr := triggered, newerErr
			mu.Unlock()
			if !fired || hookErr != nil {
				t.Fatalf("concurrent sync triggered=%v, error=%v", fired, hookErr)
			}
			assertHeadsMatch(t, source, target, testBranch)
			assertHeadsMatch(t, source, target, feature.Short())
			if tc.boundary == afterSource {
				if !errors.Is(err, gitproto.ErrTargetRefMoved) {
					t.Fatalf("expected ErrTargetRefMoved, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("older sync: %v", err)
			}
			if result.Pushed != 0 || result.Deleted != 0 || result.Warned != 0 || result.Skipped != 2 {
				t.Fatalf("expected two already-current refs and no writes or warnings, got %+v", result)
			}
		})
	}
}
