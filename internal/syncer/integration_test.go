package syncer

import (
	"bytes"
	"context"
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

func (s *smartHTTPRepoServer) handleUploadPack(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

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

func (s *smartHTTPRepoServer) handleReceivePack(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

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
