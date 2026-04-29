package gitsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/transport"

	"entire.io/git-sync/internal/syncertest"
)

type errAuthProvider struct{}

func (errAuthProvider) AuthFor(_ context.Context, _ Endpoint, _ EndpointRole) (EndpointAuth, error) {
	return EndpointAuth{}, errors.New("boom")
}

func TestValidateRequests(t *testing.T) {
	if err := (ProbeRequest{}).Validate(); err == nil {
		t.Fatalf("expected probe validation error")
	}
	if err := (PlanRequest{}).Validate(); err == nil {
		t.Fatalf("expected plan validation error")
	}
	if err := (SyncRequest{}).Validate(); err == nil {
		t.Fatalf("expected sync validation error")
	}
	if err := (ProbeRequest{
		Source:   Endpoint{URL: "https://source.example/repo.git"},
		Protocol: "bogus",
	}).Validate(); err == nil {
		t.Fatalf("expected invalid probe protocol validation error")
	}
	if err := (SyncRequest{
		Source: Endpoint{URL: "https://source.example/repo.git"},
		Target: Endpoint{URL: "https://target.example/repo.git"},
		Policy: SyncPolicy{Protocol: "bogus"},
	}).Validate(); err == nil {
		t.Fatalf("expected invalid sync protocol validation error")
	}
	if err := (PlanRequest{
		Source: Endpoint{URL: "https://source.example/repo.git"},
		Target: Endpoint{URL: "https://target.example/repo.git"},
		Scope: RefScope{
			Mappings: []RefMapping{
				{Source: "main", Target: "stable"},
				{Source: "release", Target: "stable"},
			},
		},
	}).Validate(); err == nil {
		t.Fatalf("expected duplicate mapping validation error")
	}
}

func TestClientReturnsAuthProviderErrors(t *testing.T) {
	_, err := New(Options{Auth: errAuthProvider{}}).buildProbeConfig(context.Background(), ProbeRequest{
		Source: Endpoint{URL: "https://source.example/repo.git"},
	})
	if err == nil {
		t.Fatalf("expected auth provider error")
	}
}

func TestClientSyncEndToEndWithLocalRepos(t *testing.T) {
	sourceRepo, sourceFS := syncertest.NewMemoryRepo(t)
	syncertest.MakeCommits(t, sourceRepo, sourceFS, 1)
	targetRepo, _ := syncertest.NewMemoryRepo(t)

	sourceServer := newSmartHTTPRepoServer(t, sourceRepo)
	targetServer := newSmartHTTPRepoServer(t, targetRepo)
	defer sourceServer.Close()
	defer targetServer.Close()

	client := New(Options{})
	result, err := client.Sync(context.Background(), SyncRequest{
		Source: Endpoint{URL: sourceServer.RepoURL()},
		Target: Endpoint{URL: targetServer.RepoURL()},
		Scope:  RefScope{Branches: []string{"master"}},
		Policy: SyncPolicy{Protocol: ProtocolV1},
	})
	if err != nil {
		t.Fatalf("client sync: %v", err)
	}
	if len(result.Refs) != 1 || result.Refs[0].Action != ActionCreate {
		t.Fatalf("unexpected ref results: %+v", result.Refs)
	}
	if result.Counts.Applied != 1 {
		t.Fatalf("applied = %d, want 1", result.Counts.Applied)
	}

	targetRef, err := targetRepo.Reference(plumbing.NewBranchReferenceName("master"), true)
	if err != nil {
		t.Fatalf("resolve target ref: %v", err)
	}
	sourceRef, err := sourceRepo.Reference(plumbing.NewBranchReferenceName("master"), true)
	if err != nil {
		t.Fatalf("resolve source ref: %v", err)
	}
	if targetRef.Hash() != sourceRef.Hash() {
		t.Fatalf("target hash = %s, want %s", targetRef.Hash(), sourceRef.Hash())
	}
}

func TestClientReplicateRejectsUnsupportedMode(t *testing.T) {
	err := (SyncRequest{
		Source: Endpoint{URL: "https://source.example/repo.git"},
		Target: Endpoint{URL: "https://target.example/repo.git"},
		Policy: SyncPolicy{Mode: "bogus"},
	}).Validate()
	if err == nil {
		t.Fatalf("expected invalid operation mode validation error")
	}
}

type smartHTTPRepoServer struct {
	tb       testing.TB
	repo     *git.Repository
	repoPath string
	server   *httptest.Server
}

func newSmartHTTPRepoServer(tb testing.TB, repo *git.Repository) *smartHTTPRepoServer {
	tb.Helper()

	s := &smartHTTPRepoServer{
		tb:       tb,
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
	if service != "git-upload-pack" && service != "git-receive-pack" {
		http.Error(w, "missing service", http.StatusBadRequest)
		return
	}

	var buf bytes.Buffer
	if err := transport.AdvertiseReferences(r.Context(), s.repo.Storer, &buf, transport.Service(service), false); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", service))
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.tb.Fatalf("write advertised refs: %v", err)
	}
}

func (s *smartHTTPRepoServer) handleUploadPack(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	var buf bytes.Buffer
	reader := io.NopCloser(bytes.NewReader(body))
	writer := nopWriteCloser{&buf}
	if err := transport.UploadPack(r.Context(), s.repo.Storer, reader, writer, &transport.UploadPackOptions{
		StatelessRPC: true,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.tb.Fatalf("write upload-pack response: %v", err)
	}
}

func (s *smartHTTPRepoServer) handleReceivePack(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	if !bytes.Contains(body, []byte("PACK")) {
		req := packp.NewUpdateRequests()
		if err := req.Decode(bytes.NewReader(body)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		report := packp.NewReportStatus()
		report.UnpackStatus = "ok"
		for _, cmd := range req.Commands {
			status := "ok"
			if cmd.New.IsZero() {
				if err := s.repo.Storer.RemoveReference(cmd.Name); err != nil {
					status = err.Error()
				}
			} else {
				if err := s.repo.Storer.SetReference(plumbing.NewHashReference(cmd.Name, cmd.New)); err != nil {
					status = err.Error()
				}
			}
			report.CommandStatuses = append(report.CommandStatuses, &packp.CommandStatus{
				ReferenceName: cmd.Name,
				Status:        status,
			})
		}
		s.writeReceivePackReport(w, report)
		return
	}

	var buf bytes.Buffer
	reader := io.NopCloser(bytes.NewReader(body))
	writer := nopWriteCloser{&buf}
	if err := transport.ReceivePack(r.Context(), s.repo.Storer, reader, writer, &transport.ReceivePackOptions{
		StatelessRPC: true,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.tb.Fatalf("write receive-pack response: %v", err)
	}
}

func (s *smartHTTPRepoServer) writeReceivePackReport(w http.ResponseWriter, report *packp.ReportStatus) {
	var buf bytes.Buffer
	if err := report.Encode(nopWriteCloser{&buf}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	if _, err := w.Write(buf.Bytes()); err != nil {
		s.tb.Fatalf("write receive-pack report: %v", err)
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
