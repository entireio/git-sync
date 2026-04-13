package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	billy "github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/memfs"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v6/plumbing/transport"
	transporthttp "github.com/go-git/go-git/v6/plumbing/transport/http"
	"github.com/go-git/go-git/v6/storage/memory"
	"github.com/soph/git-sync/pkg/gitsync/unstable"
)

const testBranch = "master"

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

	if got := decoded["source_url"]; got != "https://example.com/source.git" {
		t.Fatalf("unexpected source_url: %#v", got)
	}
	if got := decoded["protocol"]; got != "v2" {
		t.Fatalf("unexpected protocol: %#v", got)
	}
	if got := decoded["fetched_objects"]; got != float64(42) {
		t.Fatalf("unexpected fetched_objects: %#v", got)
	}
	measurement, ok := decoded["measurement"].(map[string]any)
	if !ok || measurement["elapsed_millis"] != float64(12) {
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
	if result["dry_run"] != true {
		t.Fatalf("expected dry_run=true, got %#v", result["dry_run"])
	}
	if result["operation_mode"] != "sync" {
		t.Fatalf("expected operation_mode=sync, got %#v", result["operation_mode"])
	}
	if result["bootstrap_suggested"] != true {
		t.Fatalf("expected bootstrap_suggested=true, got %#v", result["bootstrap_suggested"])
	}
	if result["relay_reason"] != "empty-target-managed-refs" {
		t.Fatalf("expected relay_reason for bootstrap suggestion, got %#v", result["relay_reason"])
	}
	plans, ok := result["plans"].([]any)
	if !ok || len(plans) == 0 {
		t.Fatalf("expected plan entries, got %#v", result["plans"])
	}
	plan0, ok := plans[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected first plan entry: %#v", plans[0])
	}
	if plan0["source_hash"] == nil || plan0["target_hash"] == nil {
		t.Fatalf("expected string hash fields in plan entry, got %#v", plan0)
	}
	if _, ok := plan0["source_hash"].(string); !ok {
		t.Fatalf("expected source_hash string, got %#v", plan0["source_hash"])
	}
	if _, ok := plan0["target_hash"].(string); !ok {
		t.Fatalf("expected target_hash string, got %#v", plan0["target_hash"])
	}
	if _, ok := plan0["source_ref"].(string); !ok {
		t.Fatalf("expected source_ref string, got %#v", plan0["source_ref"])
	}
	if _, ok := plan0["target_ref"].(string); !ok {
		t.Fatalf("expected target_ref string, got %#v", plan0["target_ref"])
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
			"--mode", "replicate",
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
	if result["operation_mode"] != "replicate" {
		t.Fatalf("expected operation_mode=replicate, got %#v", result["operation_mode"])
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
	service := transport.Service(r.URL.Query().Get("service"))
	if service != transport.UploadPackService && service != transport.ReceivePackService {
		http.Error(w, "missing service", http.StatusBadRequest)
		return
	}

	var buf bytes.Buffer
	if err := transport.AdvertiseReferences(r.Context(), s.repo.Storer, &buf, service, false); err != nil {
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
	ar := packp.NewAdvRefs()
	if err := ar.Decode(bytes.NewReader(data)); err == nil {
		mutate(ar.Capabilities)
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
	ar = packp.NewAdvRefs()
	if err := ar.Decode(rd); err != nil {
		return nil, err
	}
	mutate(ar.Capabilities)
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

	err := transport.UploadPack(r.Context(), s.repo.Storer, r.Body, wc, &transport.UploadPackOptions{
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

	var buf bytes.Buffer
	wc := nopWriteCloser{&buf}

	err := transport.ReceivePack(r.Context(), s.repo.Storer, r.Body, wc, &transport.ReceivePackOptions{
		StatelessRPC: true,
	})

	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	if buf.Len() > 0 {
		_, _ = w.Write(buf.Bytes())
	}
	if err != nil {
		return
	}
}

// nopWriteCloser wraps an io.Writer to satisfy io.WriteCloser.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func TestMain(m *testing.M) {
	customHTTP := transporthttp.NewTransport(&transporthttp.TransportOptions{
		Client: &http.Client{},
	})
	transport.Register("http", customHTTP)
	transport.Register("https", customHTTP)

	os.Exit(m.Run())
}
