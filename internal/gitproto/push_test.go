package gitproto

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/stretchr/testify/require"
)

func TestPrefixedLineWriter(t *testing.T) {
	tests := []struct {
		name   string
		writes []string
		want   string
	}{
		{
			name:   "single line with newline",
			writes: []string{"counting objects: 42\n"},
			want:   "target: counting objects: 42\n",
		},
		{
			name:   "carriage returns are line terminators for in-place updates",
			writes: []string{"resolving deltas: 10%\rresolving deltas: 50%\rresolving deltas: 100%\n"},
			want:   "target: resolving deltas: 10%\rtarget: resolving deltas: 50%\rtarget: resolving deltas: 100%\n",
		},
		{
			name:   "split across multiple writes",
			writes: []string{"count", "ing ", "objects: 100\nresolving "},
			want:   "target: counting objects: 100\ntarget: resolving ",
		},
		{
			name:   "no trailing prefix when stream ends mid-line",
			writes: []string{"partial progress"},
			want:   "target: partial progress",
		},
		{
			name:   "empty write is a noop",
			writes: []string{"", "visible\n"},
			want:   "target: visible\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			pw := &prefixedLineWriter{w: &buf, prefix: "target: ", atLineStart: true}
			for _, chunk := range tc.writes {
				n, err := pw.Write([]byte(chunk))
				if err != nil {
					t.Fatalf("Write(%q): %v", chunk, err)
				}
				if n != len(chunk) {
					t.Fatalf("Write(%q) consumed %d, want %d", chunk, n, len(chunk))
				}
			}
			if got := buf.String(); got != tc.want {
				t.Fatalf("output = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProgressSinkNilWhenNotVerbose(t *testing.T) {
	if got := progressSink(false, "anything: "); got != nil {
		t.Fatalf("progressSink(false) = %T, want nil", got)
	}
	if got := progressSink(true, "source: "); got == nil {
		t.Fatal("progressSink(true) returned nil, want non-nil writer")
	}
}

func TestOpenV2PackStreamCloseClosesBody(t *testing.T) {
	body := &trackingReadCloser{
		ReadCloser: io.NopCloser(bytes.NewBufferString(
			FormatPktLine("packfile\n"),
		)),
	}

	rc, err := openV2PackStream(body, false)
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
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Logf("drain request body: %v", err)
		}
		_ = r.Body.Close()

		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)

		if reportErr != "" {
			// Write a minimal report-status with an error.
			report := packp.NewReportStatus()
			report.UnpackStatus = reportErr
			if err := report.Encode(w); err != nil {
				t.Logf("encode report: %v", err)
			}
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
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Logf("drain request body: %v", err)
		}
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

func TestPushPackClosesPackOnContextCanceled(t *testing.T) {
	started := make(chan struct{}, 1)
	ep, err := transport.NewEndpoint("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	conn := NewConn(ep, "target", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))

	pack := &trackingReadCloser{ReadCloser: io.NopCloser(bytes.NewBufferString("PACK"))}
	adv := packp.NewAdvRefs()
	adv.Capabilities = capability.NewList()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- PushPack(ctx, conn, adv, []PushCommand{{
			Name: "refs/heads/main",
			New:  plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		}}, pack, false)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach server before timeout")
	}
	cancel()

	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PushPack did not return after cancellation")
	}
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if !pack.closed {
		t.Fatal("expected pack to be closed on cancellation")
	}
}

func TestPushPackStartsHTTPBeforePackFullyRead(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Logf("drain request body: %v", err)
		}
		_ = r.Body.Close()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	conn := connForServer(t, srv)
	adv := packp.NewAdvRefs()
	adv.Capabilities = capability.NewList()

	pack := &gatedReadCloser{
		first:   []byte("PACK"),
		second:  strings.Repeat("x", 1024),
		release: release,
	}

	done := make(chan error, 1)
	go func() {
		done <- PushPack(context.Background(), conn, adv, []PushCommand{{
			Name: "refs/heads/main",
			New:  plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		}}, pack, false)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start before full pack was released")
	}

	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("PushPack returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PushPack did not complete after releasing pack")
	}
}

func TestBuildUpdateRequest(t *testing.T) {
	adv := packp.NewAdvRefs()
	require.NoError(t, adv.Capabilities.Set(capability.ReportStatus))
	require.NoError(t, adv.Capabilities.Set(capability.DeleteRefs))
	require.NoError(t, adv.Capabilities.Set(capability.Sideband64k))

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

func TestPushPackReturnsPushReportErrorForPerRefFailures(t *testing.T) {
	refA := plumbing.ReferenceName("refs/heads/main")
	refB := plumbing.ReferenceName("refs/heads/feature")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Logf("drain request body: %v", err)
		}
		_ = r.Body.Close()

		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)

		report := packp.NewReportStatus()
		report.UnpackStatus = "ok"
		report.CommandStatuses = []*packp.CommandStatus{
			{ReferenceName: refA, Status: "remote ref has changed"},
			{ReferenceName: refB, Status: "already exists"},
		}
		if err := report.Encode(w); err != nil {
			t.Logf("encode report: %v", err)
		}
	}))
	defer srv.Close()

	pack := &trackingReadCloser{ReadCloser: io.NopCloser(bytes.NewBufferString("PACK"))}
	conn := connForServer(t, srv)
	adv := packp.NewAdvRefs()
	adv.Capabilities = capability.NewList()
	require.NoError(t, adv.Capabilities.Set(capability.ReportStatus))

	err := PushPack(context.Background(), conn, adv, []PushCommand{
		{Name: refA, New: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
		{Name: refB, New: plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")},
	}, pack, false)
	if err == nil {
		t.Fatal("expected PushPack to return an error")
	}

	var prErr *PushReportError
	if !errors.As(err, &prErr) {
		t.Fatalf("expected *PushReportError, got %T: %v", err, err)
	}
	if prErr.UnpackStatus != "" {
		t.Errorf("unexpected UnpackStatus %q; expected empty for per-ref failures", prErr.UnpackStatus)
	}
	if len(prErr.Failures) != 2 {
		t.Fatalf("expected 2 failures, got %d: %+v", len(prErr.Failures), prErr.Failures)
	}
	got := map[plumbing.ReferenceName]string{}
	for _, f := range prErr.Failures {
		got[f.Ref] = f.Status
	}
	if got[refA] != "remote ref has changed" {
		t.Errorf("ref %s status: want %q, got %q", refA, "remote ref has changed", got[refA])
	}
	if got[refB] != "already exists" {
		t.Errorf("ref %s status: want %q, got %q", refB, "already exists", got[refB])
	}
}

func TestBuildReportErrorTreatsEmptyUnpackStatusAsFatal(t *testing.T) {
	// A malformed / degraded receive-pack response with an empty unpack
	// status must be treated as failure (matches go-git's ReportStatus.Error
	// semantics), not silently passed through.
	report := &packp.ReportStatus{UnpackStatus: ""}
	got := buildReportError(report)
	if got == nil {
		t.Fatal("expected non-nil PushReportError for empty unpack status; empty is not 'ok'")
	}
	if got.UnpackStatus != "" {
		t.Errorf("UnpackStatus: want empty (propagated), got %q", got.UnpackStatus)
	}
}

func TestPushPackRejectsDeletes(t *testing.T) {
	pack := &trackingReadCloser{ReadCloser: io.NopCloser(bytes.NewBufferString("PACK"))}
	// PushPack should reject delete commands before even trying to connect.
	adv := packp.NewAdvRefs()
	adv.Capabilities = capability.NewList()
	// Use a nil-transport conn -- we should never reach the network.
	ep, err := transport.NewEndpoint("https://example.com/repo.git")
	require.NoError(t, err)
	conn := &Conn{Endpoint: ep, HTTP: &http.Client{}}

	err = PushPack(context.Background(), conn, adv, []PushCommand{
		{Name: "refs/heads/old", Delete: true},
	}, pack, false)
	if err == nil {
		t.Fatal("expected error for delete in pack push")
	}
	if !pack.closed {
		t.Fatal("expected pack to be closed when delete commands are rejected")
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

type gatedReadCloser struct {
	first   []byte
	second  string
	release <-chan struct{}
	stage   int
	closed  bool
}

func (r *gatedReadCloser) Read(p []byte) (int, error) {
	switch r.stage {
	case 0:
		r.stage = 1
		return copy(p, r.first), nil
	case 1:
		<-r.release
		r.stage = 2
		return copy(p, r.second), nil
	default:
		return 0, io.EOF
	}
}

func (r *gatedReadCloser) Close() error {
	r.closed = true
	return nil
}
