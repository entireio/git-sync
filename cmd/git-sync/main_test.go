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

	billy "github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	transportclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	transporthttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	transportserver "github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/soph/git-sync/internal/syncer"
)

const testBranch = "master"

func TestMarshalOutput_JSONShape(t *testing.T) {
	data, err := marshalOutput(syncer.FetchResult{
		SourceURL:      "https://example.com/source.git",
		RequestedMode:  "auto",
		Protocol:       "v2",
		Wants:          []syncer.RefInfo{{Name: "refs/heads/main", Hash: plumbing.NewHash("1111111111111111111111111111111111111111")}},
		Haves:          []plumbing.Hash{plumbing.NewHash("2222222222222222222222222222222222222222")},
		FetchedObjects: 42,
		Measurement: syncer.Measurement{
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

	targetRepo, err := git.Init(memory.NewStorage(), nil)
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
	if result["bootstrap_suggested"] != true {
		t.Fatalf("expected bootstrap_suggested=true, got %#v", result["bootstrap_suggested"])
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
	if service != transport.UploadPackServiceName && service != transport.ReceivePackServiceName {
		http.Error(w, "missing service", http.StatusBadRequest)
		return
	}

	session, err := s.newSession(service)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var ar *packp.AdvRefs
	switch service {
	case transport.UploadPackServiceName:
		ar, err = session.(transport.UploadPackSession).AdvertisedReferencesContext(r.Context())
	case transport.ReceivePackServiceName:
		ar, err = session.(transport.ReceivePackSession).AdvertisedReferencesContext(r.Context())
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
}

func (s *smartHTTPRepoServer) handleUploadPack(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	session, err := s.newSession(transport.UploadPackServiceName)
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

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.t.Fatalf("write upload-pack response: %v", err)
	}
}

func (s *smartHTTPRepoServer) handleReceivePack(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.receivePacks++
	s.mu.Unlock()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	session, err := s.newSession(transport.ReceivePackServiceName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	req := packp.NewReferenceUpdateRequest()
	if err := req.Decode(bytes.NewReader(body)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	report, err := session.(transport.ReceivePackSession).ReceivePack(r.Context(), req)
	if report != nil {
		var buf bytes.Buffer
		if encErr := report.Encode(&buf); encErr == nil {
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			_, _ = w.Write(buf.Bytes())
		}
	}
	if err != nil {
		return
	}
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
	case transport.UploadPackServiceName:
		return srv.NewUploadPackSession(endpoint, nil)
	case transport.ReceivePackServiceName:
		return srv.NewReceivePackSession(endpoint, nil)
	default:
		return nil, fmt.Errorf("unknown service %q", service)
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
