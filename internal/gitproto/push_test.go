package gitproto

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v6/plumbing/transport"
)

func TestOpenV2PackStreamCloseClosesBody(t *testing.T) {
	body := &trackingReadCloser{
		ReadCloser: io.NopCloser(bytes.NewBufferString(
			FormatPktLine("packfile\n"),
		)),
	}

	rc, err := openV2PackStream(body)
	if err != nil {
		t.Fatalf("openV2PackStream: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close pack stream: %v", err)
	}
	if !body.closed {
		t.Fatal("expected underlying body to be closed")
	}
}

// fakeReceivePackServer returns an httptest.Server that responds to
// git-receive-pack POST requests. If reportErr is non-empty, the
// report-status will indicate failure.
func fakeReceivePackServer(t *testing.T, reportErr string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Consume the request body.
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()

		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)

		if reportErr != "" {
			// Write a minimal report-status with an error.
			report := packp.NewReportStatus()
			report.UnpackStatus = reportErr
			_ = report.Encode(w)
		}
		// If no reportErr, write nothing -- PushPack will not try to
		// decode report-status when the capability is not negotiated.
	}))
}

func connForServer(t *testing.T, srv *httptest.Server) *Conn {
	t.Helper()
	ep, err := transport.NewEndpoint(srv.URL + "/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	return NewConn(ep, "test", nil, srv.Client().Transport)
}

func TestPushPackClosesPackOnSuccess(t *testing.T) {
	srv := fakeReceivePackServer(t, "")
	defer srv.Close()

	pack := &trackingReadCloser{ReadCloser: io.NopCloser(bytes.NewBufferString("PACK"))}
	conn := connForServer(t, srv)
	adv := packp.NewAdvRefs()
	adv.Capabilities = capability.NewList()

	err := PushPack(context.Background(), conn, adv, []PushCommand{{
		Name: "refs/heads/main",
		New:  plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	}}, pack, false)
	if err != nil {
		t.Fatalf("PushPack returned error: %v", err)
	}
	if !pack.closed {
		t.Fatal("expected pack to be closed on success")
	}
}

func TestPushPackClosesPackOnReceivePackError(t *testing.T) {
	// Server that returns HTTP 500 so the POST fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		http.Error(w, "receive-pack failed", http.StatusInternalServerError)
	}))
	defer srv.Close()

	pack := &trackingReadCloser{ReadCloser: io.NopCloser(bytes.NewBufferString("PACK"))}
	conn := connForServer(t, srv)
	adv := packp.NewAdvRefs()
	adv.Capabilities = capability.NewList()

	err := PushPack(context.Background(), conn, adv, []PushCommand{{
		Name: "refs/heads/main",
		New:  plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	}}, pack, false)
	if err == nil {
		t.Fatal("expected PushPack to return an error")
	}
	if !pack.closed {
		t.Fatal("expected pack to be closed on error")
	}
}

func TestBuildUpdateRequest(t *testing.T) {
	adv := packp.NewAdvRefs()
	_ = adv.Capabilities.Set(capability.ReportStatus)
	_ = adv.Capabilities.Set(capability.DeleteRefs)
	_ = adv.Capabilities.Set(capability.Sideband64k)

	req, hasDelete, hasUpdates, err := buildUpdateRequest(adv, []PushCommand{
		{Name: "refs/heads/main", New: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
		{Name: "refs/heads/old", Old: plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), Delete: true},
	}, false)
	if err != nil {
		t.Fatalf("buildUpdateRequest: %v", err)
	}
	if !hasDelete {
		t.Error("expected hasDelete = true")
	}
	if !hasUpdates {
		t.Error("expected hasUpdates = true")
	}
	if len(req.Commands) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(req.Commands))
	}
	if !req.Capabilities.Supports(capability.ReportStatus) {
		t.Error("expected report-status capability")
	}
}

func TestBuildUpdateRequestDeleteWithoutCapability(t *testing.T) {
	adv := packp.NewAdvRefs()
	// No delete-refs capability.

	_, _, _, err := buildUpdateRequest(adv, []PushCommand{
		{Name: "refs/heads/old", Old: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), Delete: true},
	}, false)
	if err == nil {
		t.Fatal("expected error when target does not support delete-refs")
	}
}

func TestPushPackRejectsDeletes(t *testing.T) {
	pack := &trackingReadCloser{ReadCloser: io.NopCloser(bytes.NewBufferString("PACK"))}
	// PushPack should reject delete commands before even trying to connect.
	adv := packp.NewAdvRefs()
	adv.Capabilities = capability.NewList()
	// Use a nil-transport conn -- we should never reach the network.
	ep, _ := transport.NewEndpoint("https://example.com/repo.git")
	conn := &Conn{Endpoint: ep, HTTP: &http.Client{}}

	err := PushPack(context.Background(), conn, adv, []PushCommand{
		{Name: "refs/heads/old", Delete: true},
	}, pack, false)
	if err == nil {
		t.Fatal("expected error for delete in pack push")
	}
}

type trackingReadCloser struct {
	io.ReadCloser
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	if r.ReadCloser != nil {
		return r.ReadCloser.Close()
	}
	return nil
}
