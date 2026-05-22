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
	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage/memory"
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
	if got := progressSink(false, "anything: ", nil); got != nil {
		t.Fatalf("progressSink(false) = %T, want nil", got)
	}
	if got := progressSink(true, "source: ", nil); got == nil {
		t.Fatal("progressSink(true) returned nil, want non-nil writer")
	}
}

func TestOpenV2PackStreamCloseClosesBody(t *testing.T) {
	body := &trackingReadCloser{
		ReadCloser: io.NopCloser(bytes.NewBufferString(
			FormatPktLine("packfile\n"),
		)),
	}

	rc, err := openV2PackStream(body, false, nil)
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
			report := &packp.ReportStatus{}
			report.UnpackStatus = reportErr
			if err := report.Encode(w); err != nil {
				t.Logf("encode report: %v", err)
			}
		}
		// If no reportErr, write nothing -- PushPack will not try to
		// decode report-status when the capability is not negotiated.
	}))
}

func connForServer(t *testing.T, srv *httptest.Server) *HTTPConn {
	t.Helper()
	ep, err := transport.ParseURL(srv.URL + "/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	return NewHTTPConn(ep, "test", nil, srv.Client().Transport)
}

func TestPushPackClosesPackOnSuccess(t *testing.T) {
	srv := fakeReceivePackServer(t, "")
	defer srv.Close()

	pack := &trackingReadCloser{ReadCloser: io.NopCloser(bytes.NewBufferString("PACK"))}
	conn := connForServer(t, srv)
	adv := &packp.AdvRefs{}

	err := PushPack(context.Background(), conn, adv, []PushCommand{{
		Name: "refs/heads/main",
		New:  plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	}}, pack, false, nil)
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
	adv := &packp.AdvRefs{}

	err := PushPack(context.Background(), conn, adv, []PushCommand{{
		Name: "refs/heads/main",
		New:  plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	}}, pack, false, nil)
	if err == nil {
		t.Fatal("expected PushPack to return an error")
	}
	if !pack.closed {
		t.Fatal("expected pack to be closed on error")
	}
}

func TestPushPackClosesPackOnContextCanceled(t *testing.T) {
	started := make(chan struct{}, 1)
	ep, err := transport.ParseURL("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	conn := NewHTTPConn(ep, "target", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))

	pack := &trackingReadCloser{ReadCloser: io.NopCloser(bytes.NewBufferString("PACK"))}
	adv := &packp.AdvRefs{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- PushPack(ctx, conn, adv, []PushCommand{{
			Name: "refs/heads/main",
			New:  plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		}}, pack, false, nil)
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

// TestPushPackStartsHTTPBeforePackFullyRead asserts that PushPack — the
// relay path — keeps streaming source pack bytes through to the target
// with chunked encoding. Materialized push (PushObjects) gets the same
// property via precomputed delta selection; see
// TestPushObjectsStreamsBody.
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
	adv := &packp.AdvRefs{}

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
		}}, pack, false, nil)
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

// TestPushObjectsStreamsBody asserts that PushObjects sends a chunked
// receive-pack request — the streaming property is what avoids the
// mid-stream stall (delta selection runs before the body opens, so
// pack bytes flow continuously once writing starts). A request with
// no Transfer-Encoding: chunked would mean we've regressed to
// buffering the whole pack before sending.
func TestPushObjectsStreamsBody(t *testing.T) {
	type observation struct {
		transferEncoding []string
		contentLength    int64
		bodyLen          int64
	}
	observed := make(chan observation, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			t.Logf("drain request body: %v", err)
		}
		_ = r.Body.Close()
		observed <- observation{
			transferEncoding: r.TransferEncoding,
			contentLength:    r.ContentLength,
			bodyLen:          n,
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	conn := connForServer(t, srv)
	adv := &packp.AdvRefs{}
	adv.Capabilities.Set(capability.OFSDelta)

	err := PushObjects(context.Background(), conn, adv, []PushCommand{{
		Name: "refs/heads/main",
		New:  plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	}}, memory.NewStorage(), nil, false, nil)
	if err != nil {
		t.Fatalf("PushObjects: %v", err)
	}

	var obs observation
	select {
	case obs = <-observed:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive request")
	}

	chunked := false
	for _, te := range obs.transferEncoding {
		if te == "chunked" {
			chunked = true
			break
		}
	}
	if !chunked {
		t.Errorf("Transfer-Encoding = %v, want to include \"chunked\"", obs.transferEncoding)
	}
	if obs.contentLength != -1 {
		t.Errorf("Content-Length = %d, want -1 (unknown for chunked)", obs.contentLength)
	}
	if obs.bodyLen <= 0 {
		t.Errorf("bodyLen = %d, want > 0", obs.bodyLen)
	}
}

func TestBuildUpdateRequest(t *testing.T) {
	adv := &packp.AdvRefs{}
	adv.Capabilities.Set(capability.ReportStatus)
	adv.Capabilities.Set(capability.DeleteRefs)
	adv.Capabilities.Set(capability.Sideband64k)

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
	adv := &packp.AdvRefs{}
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
	adv := &packp.AdvRefs{}
	// Use a nil-transport conn -- we should never reach the network.
	ep, err := transport.ParseURL("https://example.com/repo.git")
	require.NoError(t, err)
	conn := &HTTPConn{EndpointURL: ep, HTTP: &http.Client{}}

	err = PushPack(context.Background(), conn, adv, []PushCommand{
		{Name: "refs/heads/old", Delete: true},
	}, pack, false, nil)
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

func TestAnnotateLeaseFailureWrapsStaleInfo(t *testing.T) {
	cases := []struct {
		name   string
		status string
		wrap   bool
	}{
		{name: "stale info", status: "stale info", wrap: true},
		{name: "stale info with detail", status: "stale info, exp 1234, got abcd", wrap: true},
		{name: "fetch first", status: "fetch first", wrap: true},
		{name: "non-fast-forward", status: "non-fast-forward", wrap: true},
		{name: "does not match expected old", status: "remote ref does not match expected old value", wrap: true},
		{name: "unrelated reason passes through", status: "deny updating a hidden ref", wrap: false},
		{name: "case-insensitive match", status: "Stale Info", wrap: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := &packp.CommandStatusErr{ReferenceName: "refs/heads/main", Status: c.status}
			out := annotateLeaseFailure(in)
			wrapped := strings.Contains(out.Error(), "moved or differs from session start")
			if wrapped != c.wrap {
				t.Fatalf("wrap=%v want=%v (err=%q)", wrapped, c.wrap, out)
			}
			var inner *packp.CommandStatusErr
			if !errors.As(out, &inner) || inner.Status != c.status {
				t.Fatalf("annotateLeaseFailure must preserve the underlying CommandStatusErr; got %#v", out)
			}
		})
	}
}

func TestAnnotateLeaseFailurePassesNonCommandStatusErrors(t *testing.T) {
	err := errors.New("network blew up")
	if got := annotateLeaseFailure(err); !errors.Is(got, err) {
		t.Fatalf("unrelated error should pass through unchanged, got %v", got)
	}
}
