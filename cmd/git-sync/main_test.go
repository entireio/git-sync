package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"entire.io/entire/git-sync/internal/auth"
	"entire.io/entire/git-sync/internal/syncertest"
	"entire.io/entire/git-sync/unstable"
	billy "github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/memfs"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/pktline"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage/memory"
)

// TestMain isolates the package's tests from the developer's local
// credential helper. EnsureAuthForService probes /git-receive-pack with a
// flush-packet POST unconditionally (required to discover cross-host
// auth challenges and auth-on-POST-only gates), so without stubbing the
// helper, `git credential fill` could find stored credentials for
// 127.0.0.1 (e.g. cached from an earlier test run) and attach them,
// changing the wire shape of the push the test under inspection.
//
// The probe itself still happens — receive-pack POST counts include it —
// but the stub guarantees no credentials are attached and the probe
// returns without further side effects on the helper.
//
// Tests that need to exercise helper behaviour explicitly should
// restore auth.GitCredentialCommand in their own setup.
func TestMain(m *testing.M) {
	auth.GitCredentialCommand = func(_ context.Context, _ auth.CredentialOp, _ string) ([]byte, error) {
		return nil, errors.New("no helper configured (test default)")
	}
	os.Exit(m.Run())
}

const testBranch = "master"
const modeReplicate = "replicate"

func TestMarshalOutput_JSONShape(t *testing.T) {
	data, err := marshalOutput(unstable.FetchResult{
		SourceURL:      "https://example.com/source.git",
		RequestedMode:  "auto",
		Protocol:       "v2",
		Wants:          []unstable.RefInfo{{Name: "refs/heads/main", Hash: plumbing.NewHash("1111111111111111111111111111111111111111")}},
		Haves:          []plumbing.Hash{plumbing.NewHash("2222222222222222222222222222222222222222")},
		FetchedObjects: 42,
		Measurement: unstable.Measurement{
			Enabled:            true,
			ElapsedMillis:      12,
			PeakAllocBytes:     100,
			PeakHeapInuseBytes: 200,
			TotalAllocBytes:    300,
			GCCount:            1,
		},
	})
	if err != nil {
		t.Fatalf("marshalOutput returned error: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal marshaled output: %v", err)
	}

	if got := decoded["sourceUrl"]; got != "https://example.com/source.git" {
		t.Fatalf("unexpected sourceUrl: %#v", got)
	}
	if got := decoded["protocol"]; got != "v2" {
		t.Fatalf("unexpected protocol: %#v", got)
	}
	if got := decoded["fetchedObjects"]; got != float64(42) {
		t.Fatalf("unexpected fetchedObjects: %#v", got)
	}
	measurement, ok := decoded["measurement"].(map[string]any)
	if !ok || measurement["elapsedMillis"] != float64(12) {
		t.Fatalf("unexpected measurement: %#v", decoded["measurement"])
	}
	wants, ok := decoded["wants"].([]any)
	if !ok || len(wants) != 1 {
		t.Fatalf("unexpected wants: %#v", decoded["wants"])
	}
	want0, ok := wants[0].(map[string]any)
	if !ok || want0["hash"] != "1111111111111111111111111111111111111111" {
		t.Fatalf("unexpected want hash: %#v", wants[0])
	}
	haves, ok := decoded["haves"].([]any)
	if !ok || len(haves) != 1 || haves[0] != "2222222222222222222222222222222222222222" {
		t.Fatalf("unexpected haves: %#v", decoded["haves"])
	}
}

func TestRun_Plan_JSONDoesNotPush(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 3)

	targetRepo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServer(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	output, err := captureStdout(func() error {
		return run(context.Background(), []string{
			"plan",
			"--json",
			"--stats",
			sourceServer.RepoURL(),
			targetServer.RepoURL(),
		})
	})
	if err != nil {
		t.Fatalf("run plan: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode plan json: %v\noutput=%s", err, output)
	}
	if result["dryRun"] != true {
		t.Fatalf("expected dryRun=true, got %#v", result["dryRun"])
	}
	if result["operationMode"] != "sync" {
		t.Fatalf("expected operationMode=sync, got %#v", result["operationMode"])
	}
	if result["bootstrapSuggested"] != true {
		t.Fatalf("expected bootstrapSuggested=true, got %#v", result["bootstrapSuggested"])
	}
	if result["relayReason"] != "empty-target-managed-refs" {
		t.Fatalf("expected relayReason for bootstrap suggestion, got %#v", result["relayReason"])
	}
	plans, ok := result["plans"].([]any)
	if !ok || len(plans) == 0 {
		t.Fatalf("expected plan entries, got %#v", result["plans"])
	}
	plan0, ok := plans[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected first plan entry: %#v", plans[0])
	}
	if plan0["sourceHash"] == nil || plan0["targetHash"] == nil {
		t.Fatalf("expected string hash fields in plan entry, got %#v", plan0)
	}
	if _, ok := plan0["sourceHash"].(string); !ok {
		t.Fatalf("expected sourceHash string, got %#v", plan0["sourceHash"])
	}
	if _, ok := plan0["targetHash"].(string); !ok {
		t.Fatalf("expected targetHash string, got %#v", plan0["targetHash"])
	}
	if _, ok := plan0["sourceRef"].(string); !ok {
		t.Fatalf("expected sourceRef string, got %#v", plan0["sourceRef"])
	}
	if _, ok := plan0["targetRef"].(string); !ok {
		t.Fatalf("expected targetRef string, got %#v", plan0["targetRef"])
	}
	if result["pushed"] != float64(0) {
		t.Fatalf("expected pushed=0, got %#v", result["pushed"])
	}
	if targetServer.Count("git-receive-pack") != 0 {
		t.Fatalf("expected no receive-pack POSTs, got %d", targetServer.Count("git-receive-pack"))
	}
}

func TestRun_Plan_ReplicateMode_JSONShowsReplicate(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 1)

	targetRepo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServer(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	output, err := captureStdout(func() error {
		return run(context.Background(), []string{
			"plan",
			"--mode", modeReplicate,
			"--json",
			sourceServer.RepoURL(),
			targetServer.RepoURL(),
		})
	})
	if err != nil {
		t.Fatalf("run replicate plan: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode plan json: %v\noutput=%s", err, output)
	}
	if result["operationMode"] != modeReplicate {
		t.Fatalf("expected operationMode=replicate, got %#v", result["operationMode"])
	}
}

func TestRun_Replicate_SubcommandExecutesAgainstEmptyTarget(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 1)

	targetRepo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServer(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	output, err := captureStdout(func() error {
		return run(context.Background(), []string{
			modeReplicate,
			"--json",
			sourceServer.RepoURL(),
			targetServer.RepoURL(),
		})
	})
	if err != nil {
		t.Fatalf("run replicate subcommand: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode replicate json: %v\noutput=%s", err, output)
	}
	if result["dryRun"] != false {
		t.Fatalf("expected dryRun=false, got %#v", result["dryRun"])
	}
	if result["operationMode"] != modeReplicate {
		t.Fatalf("expected operationMode=replicate, got %#v", result["operationMode"])
	}
	if result["pushed"] != float64(1) {
		t.Fatalf("expected pushed=1, got %#v", result["pushed"])
	}
	if result["relay"] != true {
		t.Fatalf("expected relay=true, got %#v", result["relay"])
	}
	if result["relayReason"] != "empty-target-managed-refs" {
		t.Fatalf("expected relayReason=empty-target-managed-refs, got %#v", result["relayReason"])
	}

	// Two receive-pack POSTs: the auth-probe (a flush-packet POST that
	// EnsureAuthForService always sends to detect auth-on-POST-only gates and
	// cross-host challenges) plus the real push.
	if got := targetServer.Count("git-receive-pack"); got != 2 {
		t.Fatalf("expected two receive-pack POSTs (auth-probe + real push), got %d", got)
	}
	sourceHead, err := sourceRepo.Reference(plumbing.NewBranchReferenceName(testBranch), true)
	if err != nil {
		t.Fatalf("source head: %v", err)
	}
	targetHead, err := targetRepo.Reference(plumbing.NewBranchReferenceName(testBranch), true)
	if err != nil {
		t.Fatalf("target head: %v", err)
	}
	if sourceHead.Hash() != targetHead.Hash() {
		t.Fatalf("expected target head %s to match source head %s", targetHead.Hash(), sourceHead.Hash())
	}
}

// Smoke test: cobra flag parsing through the full sync pipeline.
func TestRun_Sync_AllRefsSmokeTest(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 2)

	notesRef := plumbing.ReferenceName("refs/notes/commits")
	head := syncertest.SetRefAtBranch(t, sourceRepo, notesRef, testBranch)

	targetRepo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServer(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	output, err := captureStdout(func() error {
		return run(context.Background(), []string{
			"sync",
			"--all-refs",
			"--json",
			sourceServer.RepoURL(),
			targetServer.RepoURL(),
		})
	})
	if err != nil {
		t.Fatalf("run sync --all-refs: %v\noutput=%s", err, output)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode sync json: %v\noutput=%s", err, output)
	}

	plans, ok := result["plans"].([]any)
	if !ok || len(plans) < 2 {
		t.Fatalf("expected at least 2 plans (branch + notes), got %#v", result["plans"])
	}
	var foundNotesRef bool
	for _, raw := range plans {
		plan, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if plan["targetRef"] == "refs/notes/commits" {
			if plan["kind"] != "other" {
				t.Errorf("expected notes ref kind=other, got %#v", plan["kind"])
			}
			if plan["action"] != "create" {
				t.Errorf("expected notes ref action=create, got %#v", plan["action"])
			}
			foundNotesRef = true
		}
	}
	if !foundNotesRef {
		t.Fatalf("refs/notes/commits not in plans output: %s", output)
	}

	gotNotes, err := targetRepo.Reference(notesRef, true)
	if err != nil {
		t.Fatalf("expected refs/notes/commits on target: %v", err)
	}
	if gotNotes.Hash() != head {
		t.Fatalf("target notes hash = %s, want %s", gotNotes.Hash(), head)
	}
}

// probe --exclude-ref-prefix must filter the returned ref list — otherwise
// the CLI knob is wired but ineffective for previewing scoped state.
func TestRun_Probe_ExcludeRefPrefixFiltersReturnedRefs(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 1)

	notesRef := plumbing.ReferenceName("refs/notes/commits")
	syncertest.SetRefAtBranch(t, sourceRepo, notesRef, testBranch)
	pullRef := plumbing.ReferenceName("refs/pull/1/head")
	syncertest.SetRefAtBranch(t, sourceRepo, pullRef, testBranch)

	sourceServer := newSmartHTTPRepoServer(t, sourceRepo)
	defer sourceServer.Close()

	output, err := captureStdout(func() error {
		return run(context.Background(), []string{
			"probe",
			"--all-refs",
			"--exclude-ref-prefix", "refs/pull/",
			"--json",
			sourceServer.RepoURL(),
		})
	})
	if err != nil {
		t.Fatalf("run probe: %v\noutput=%s", err, output)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode probe json: %v\noutput=%s", err, output)
	}
	refs, ok := result["refs"].([]any)
	if !ok {
		t.Fatalf("expected refs array, got %#v", result["refs"])
	}
	var sawNotes bool
	for _, raw := range refs {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, ok := entry["name"].(string)
		if !ok {
			continue
		}
		if name == string(pullRef) {
			t.Fatalf("expected %s excluded from probe output, but it appeared", pullRef)
		}
		if name == string(notesRef) {
			sawNotes = true
		}
	}
	if !sawNotes {
		t.Fatalf("expected %s in probe output", notesRef)
	}
}

// CLI smoke test for --exclude-ref-prefix under --all-refs: refs/pull/* on
// the source is trimmed, refs/notes/commits is kept.
func TestRun_Sync_ExcludeRefPrefixTrimsPullRefs(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 1)

	notesRef := plumbing.ReferenceName("refs/notes/commits")
	syncertest.SetRefAtBranch(t, sourceRepo, notesRef, testBranch)
	pullRef := plumbing.ReferenceName("refs/pull/1/head")
	syncertest.SetRefAtBranch(t, sourceRepo, pullRef, testBranch)

	targetRepo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServer(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	err = run(context.Background(), []string{
		"sync",
		"--all-refs",
		"--exclude-ref-prefix", "refs/pull/",
		"--json",
		sourceServer.RepoURL(),
		targetServer.RepoURL(),
	})
	if err != nil {
		t.Fatalf("run sync --all-refs --exclude-ref-prefix: %v", err)
	}

	if _, err := targetRepo.Reference(notesRef, true); err != nil {
		t.Errorf("expected refs/notes/commits on target, got err=%v", err)
	}
	if _, err := targetRepo.Reference(pullRef, true); err == nil {
		t.Errorf("expected refs/pull/1/head NOT on target")
	}
}

func TestRun_Fetch_AllRefsCoversTagsAndOtherKind(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 1)

	head, err := sourceRepo.Reference(plumbing.NewBranchReferenceName(testBranch), true)
	if err != nil {
		t.Fatalf("resolve source head: %v", err)
	}
	tagRef := plumbing.NewTagReferenceName("v1")
	if err := sourceRepo.Storer.SetReference(plumbing.NewHashReference(tagRef, head.Hash())); err != nil {
		t.Fatalf("set source tag: %v", err)
	}
	notesRef := plumbing.ReferenceName("refs/notes/commits")
	if err := sourceRepo.Storer.SetReference(plumbing.NewHashReference(notesRef, head.Hash())); err != nil {
		t.Fatalf("set source notes ref: %v", err)
	}

	sourceServer := newSmartHTTPRepoServer(t, sourceRepo)
	defer sourceServer.Close()

	output, err := captureStdout(func() error {
		return run(context.Background(), []string{
			"fetch",
			"--all-refs",
			"--json",
			sourceServer.RepoURL(),
		})
	})
	if err != nil {
		t.Fatalf("run fetch --all-refs: %v\noutput=%s", err, output)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode fetch json: %v\noutput=%s", err, output)
	}
	wants, ok := result["wants"].([]any)
	if !ok {
		t.Fatalf("expected wants in result, got %#v", result)
	}
	seen := make(map[string]bool)
	for _, raw := range wants {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, ok := entry["name"].(string); ok && name != "" {
			seen[name] = true
		}
	}
	for _, want := range []string{string(tagRef), string(notesRef)} {
		if !seen[want] {
			t.Errorf("expected %s in fetch wants, got %v", want, seen)
		}
	}
}

// replicate's --all-refs must not bundle BestEffort the way sync's does.
func TestRun_Replicate_AllRefsKeepsStrictFailureOnNg(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 1)

	targetRepo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServer(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	targetServer.receivePackHook = func(req *packp.UpdateRequests) *packp.ReportStatus {
		return syncertest.DenyRefsReport(req, "deny updating a hidden ref")
	}

	err = run(context.Background(), []string{
		modeReplicate,
		"--all-refs",
		"--json",
		sourceServer.RepoURL(),
		targetServer.RepoURL(),
	})
	if err == nil {
		t.Fatal("expected replicate --all-refs to error on per-ref ng")
	}
}

// Mirror of the replicate test: same target rejection, warning + exit 0
// for sync, so the CLI binding really differs by mode.
func TestRun_Sync_AllRefsWarnsOnNg(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 1)

	targetRepo, err := git.Init(memory.NewStorage())
	if err != nil {
		t.Fatalf("init target repo: %v", err)
	}

	sourceServer := newSmartHTTPRepoServer(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	targetServer.receivePackHook = func(req *packp.UpdateRequests) *packp.ReportStatus {
		return syncertest.DenyRefsReport(req, "deny updating a hidden ref")
	}

	output, err := captureStdout(func() error {
		return run(context.Background(), []string{
			"sync",
			"--all-refs",
			"--json",
			sourceServer.RepoURL(),
			targetServer.RepoURL(),
		})
	})
	if err != nil {
		t.Fatalf("expected sync --all-refs to succeed with warning, got: %v\noutput=%s", err, output)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode sync json: %v\noutput=%s", err, output)
	}
	if got, ok := result["warned"].(float64); !ok || got == 0 {
		t.Fatalf("expected warned > 0 in result, got %#v", result["warned"])
	}
}

func TestRun_Replicate_SubcommandRejectsForce(t *testing.T) {
	err := run(context.Background(), []string{
		modeReplicate,
		"--force-with-lease",
		"http://127.0.0.1:1/source.git",
		"http://127.0.0.1:1/target.git",
	})
	if err == nil {
		t.Fatal("expected replicate --force-with-lease to be rejected")
	}
	if !strings.Contains(err.Error(), "replicate does not support force flags") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRun_LegacyForceFlagRejected(t *testing.T) {
	err := run(context.Background(), []string{
		"sync",
		"--force",
		"http://127.0.0.1:1/source.git",
		"http://127.0.0.1:1/target.git",
	})
	if err == nil {
		t.Fatal("expected --force to be rejected")
	}
	if !strings.Contains(err.Error(), "--force has been removed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRun_ForceWithLeaseAndBlindAreMutuallyExclusive(t *testing.T) {
	err := run(context.Background(), []string{
		"sync",
		"--force-with-lease",
		"--force-blind",
		"http://127.0.0.1:1/source.git",
		"http://127.0.0.1:1/target.git",
	})
	if err == nil {
		t.Fatal("expected --force-with-lease and --force-blind together to be rejected")
	}
	if !strings.Contains(err.Error(), "force-with-lease") || !strings.Contains(err.Error(), "force-blind") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRun_Fetch_SourceFollowInfoRefsRedirectFlag(t *testing.T) {
	sourceRepo, sourceFS := newSourceRepo(t)
	makeCommits(t, sourceRepo, sourceFS, 1)

	sourceServer := newSmartHTTPRepoServer(t, sourceRepo)
	defer sourceServer.Close()

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == sourceServer.repoPath+"/info/refs":
			http.Redirect(w, r, sourceServer.server.URL+r.URL.Path+"?"+r.URL.RawQuery, http.StatusTemporaryRedirect)
		case r.Method == http.MethodPost:
			http.Error(w, "entry domain rejects packs", http.StatusMethodNotAllowed)
		default:
			http.NotFound(w, r)
		}
	}))
	defer entry.Close()

	output, err := captureStdout(func() error {
		return run(context.Background(), []string{
			"fetch",
			"--source-follow-info-refs-redirect",
			"--branch", testBranch,
			"--json",
			entry.URL + sourceServer.repoPath,
		})
	})
	if err != nil {
		t.Fatalf("run fetch: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("decode fetch json: %v\noutput=%s", err, output)
	}
	if got, ok := result["fetchedObjects"].(float64); !ok || got == 0 {
		t.Fatalf("expected fetched objects from redirected source, got %#v", result["fetchedObjects"])
	}
}

func captureStdout(fn func() error) (string, error) {
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	runErr := fn()
	_ = w.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return "", err
	}
	_ = r.Close()
	return strings.TrimSpace(buf.String()), runErr
}

func newSourceRepo(t *testing.T) (*git.Repository, billy.Filesystem) {
	t.Helper()

	fs := memfs.New()
	repo, err := git.Init(memory.NewStorage(), git.WithWorkTree(fs))
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

	for i := range count {
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

var objectSignature = signature()

func signature() object.Signature {
	return object.Signature{
		Name:  "test",
		Email: "test@example.com",
		When:  time.Unix(1, 0).UTC(),
	}
}

type smartHTTPRepoServer struct {
	t        *testing.T
	server   *httptest.Server
	repo     *git.Repository
	repoPath string

	// receivePackHook synthesizes the receive-pack response when set,
	// bypassing the embedded ReceivePack handler. Used to simulate
	// per-ref ng statuses from hostile targets.
	receivePackHook func(*packp.UpdateRequests) *packp.ReportStatus

	mu           sync.Mutex
	receivePacks int
	thinCapable  bool
}

func newSmartHTTPRepoServer(t *testing.T, repo *git.Repository) *smartHTTPRepoServer {
	t.Helper()

	s := &smartHTTPRepoServer{
		t:           t,
		repo:        repo,
		repoPath:    "/repo.git",
		thinCapable: true,
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

func (s *smartHTTPRepoServer) Close() {
	s.server.Close()
}

func (s *smartHTTPRepoServer) RepoURL() string {
	return s.server.URL + s.repoPath
}

func (s *smartHTTPRepoServer) Count(service string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if service == "git-receive-pack" {
		return s.receivePacks
	}
	return 0
}

func (s *smartHTTPRepoServer) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == s.repoPath+"/info/refs":
		s.handleInfoRefs(w, r)
	case r.Method == http.MethodPost && r.URL.Path == s.repoPath+"/git-upload-pack":
		s.handleUploadPack(w, r)
	case r.Method == http.MethodPost && r.URL.Path == s.repoPath+"/git-receive-pack":
		s.handleReceivePack(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *smartHTTPRepoServer) handleInfoRefs(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	if service != transport.UploadPackService && service != transport.ReceivePackService {
		http.Error(w, "missing service", http.StatusBadRequest)
		return
	}

	var buf bytes.Buffer
	if err := transport.AdvertiseRefs(r.Context(), s.repo.Storer, &buf, service, false); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if service == transport.ReceivePackService && s.thinCapable {
		rewritten, err := rewriteReceivePackAdvertisement(buf.Bytes(), func(caps *capability.List) {
			caps.Delete(capability.Capability("no-thin"))
		})
		if err != nil {
			s.t.Fatalf("rewrite receive-pack advertisement: %v", err)
		}
		buf.Reset()
		_, _ = buf.Write(rewritten)
	}

	w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", service))
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.t.Fatalf("write advertised refs: %v", err)
	}
}

func rewriteReceivePackAdvertisement(data []byte, mutate func(*capability.List)) ([]byte, error) {
	ar := &packp.AdvRefs{}
	if err := ar.Decode(bytes.NewReader(data)); err == nil {
		mutate(&ar.Capabilities)
		var buf bytes.Buffer
		if err := ar.Encode(&buf); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	rd := bytes.NewReader(data)
	var smart packp.SmartReply
	if err := smart.Decode(rd); err != nil {
		return nil, err
	}
	ar = &packp.AdvRefs{}
	if err := ar.Decode(rd); err != nil {
		return nil, err
	}
	mutate(&ar.Capabilities)
	var buf bytes.Buffer
	if err := smart.Encode(&buf); err != nil {
		return nil, err
	}
	if err := ar.Encode(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *smartHTTPRepoServer) handleUploadPack(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	wc := nopWriteCloser{&buf}

	err := transport.UploadPack(r.Context(), s.repo.Storer, r.Body, wc, &transport.UploadPackRequest{
		StatelessRPC: true,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.t.Fatalf("write upload-pack response: %v", err)
	}
}

func (s *smartHTTPRepoServer) handleReceivePack(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.receivePacks++
	s.mu.Unlock()

	if s.receivePackHook != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req := &packp.UpdateRequests{}
		if err := req.Decode(bytes.NewReader(body)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		report := s.receivePackHook(req)
		// Wrap the report in sideband framing when negotiated, mirroring
		// what transport.ReceivePack writes; the client's demuxer otherwise
		// fails on raw report-status pkt-lines.
		var buf bytes.Buffer
		var writer io.Writer = &buf
		useSideband := false
		// Mirrors the syncer test server's sideband-wrap: no-progress
		// turns off the wrapping even if a sideband cap is advertised.
		if !req.Capabilities.Supports(capability.NoProgress) {
			switch {
			case req.Capabilities.Supports(capability.Sideband64k):
				writer = sideband.NewMuxer(sideband.Sideband64k, &buf)
				useSideband = true
			case req.Capabilities.Supports(capability.Sideband):
				writer = sideband.NewMuxer(sideband.Sideband, &buf)
				useSideband = true
			}
		}
		if err := report.Encode(nopWriteCloser{writer}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if useSideband {
			_ = pktline.WriteFlush(&buf)
		}
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		if _, err := w.Write(buf.Bytes()); err != nil {
			s.t.Fatalf("write receive-pack hook response: %v", err)
		}
		return
	}

	var buf bytes.Buffer
	wc := nopWriteCloser{&buf}

	err := transport.ReceivePack(r.Context(), s.repo.Storer, r.Body, wc, &transport.ReceivePackRequest{
		StatelessRPC: true,
	})

	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	if buf.Len() > 0 {
		if _, err := w.Write(buf.Bytes()); err != nil {
			s.t.Fatalf("write receive-pack response: %v", err)
		}
	}
	if err != nil {
		return
	}
}

// nopWriteCloser wraps an io.Writer to satisfy io.WriteCloser.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
