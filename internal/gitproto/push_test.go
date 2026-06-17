package gitproto

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

// captureReceivePackBody starts a server that records the request body it
// receives on the returned channel and replies 200 OK. awaitBody reads the
// next captured body or fails the test if none arrives.
func captureReceivePackBody(t *testing.T) (<-chan []byte, *httptest.Server) {
	t.Helper()
	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Logf("read request body: %v", err)
		}
		_ = r.Body.Close()
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	return bodies, srv
}

func awaitBody(t *testing.T, bodies <-chan []byte) []byte {
	t.Helper()
	select {
	case body := <-bodies:
		return body
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive request")
		return nil
	}
}

func TestEmptyPackTrailerMatchesObjectFormat(t *testing.T) {
	t.Run("sha1 default", func(t *testing.T) {
		adv := &packp.AdvRefs{}
		pack := emptyPack(adv)
		require.Len(t, pack, 12+sha1.Size)
		require.Equal(t, emptyPackHeader, pack[:12])
		sum := sha1.Sum(emptyPackHeader)
		require.Equal(t, sum[:], pack[12:])
		// Golden: git's canonical empty-pack checksum.
		require.Equal(t, "029d08823bd8a8eab510ad6ac75c823cfd3ed31e", hex.EncodeToString(pack[12:]))
	})

	t.Run("sha256 from object-format capability", func(t *testing.T) {
		adv := &packp.AdvRefs{}
		adv.Capabilities.Set(capability.ObjectFormat, "sha256")
		pack := emptyPack(adv)
		require.Len(t, pack, 12+sha256.Size)
		require.Equal(t, emptyPackHeader, pack[:12])
		sum := sha256.Sum256(emptyPackHeader)
		require.Equal(t, sum[:], pack[12:])
	})
}

// TestPushCommandsSendsEmptyPackForCreate guards the interop fix: a ref
// create that moves no new objects must still carry a valid empty pack, so
// receive-pack implementations that read a pack header for every non-delete
// command don't see a truncated body.
func TestPushCommandsSendsEmptyPackForCreate(t *testing.T) {
	bodies, srv := captureReceivePackBody(t)
	defer srv.Close()

	conn := connForServer(t, srv)
	adv := &packp.AdvRefs{}

	err := PushCommands(context.Background(), conn, adv, []PushCommand{{
		Name: "refs/heads/docs-rules",
		New:  plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	}}, false, nil)
	require.NoError(t, err)

	require.True(t, bytes.HasSuffix(awaitBody(t, bodies), emptyPack(adv)),
		"request body should end with a valid empty pack")
}

func TestPushCommandsSendsNoPackForDeleteOnly(t *testing.T) {
	bodies, srv := captureReceivePackBody(t)
	defer srv.Close()

	conn := connForServer(t, srv)
	adv := &packp.AdvRefs{}
	adv.Capabilities.Set(capability.DeleteRefs)

	err := PushCommands(context.Background(), conn, adv, []PushCommand{{
		Name:   "refs/gitsync/bootstrap/heads/docs-rules",
		Old:    plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Delete: true,
	}}, false, nil)
	require.NoError(t, err)

	require.False(t, bytes.Contains(awaitBody(t, bodies), []byte("PACK")),
		"delete-only push must not carry a pack")
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
			// Faithful input: go-git returns CommandStatusErr BY VALUE from
			// report.Error(), so annotateLeaseFailure must match it with a value
			// errors.As target. A *CommandStatusErr input would mask that.
			in := (&packp.CommandStatus{ReferenceName: "refs/heads/main", Status: c.status}).Error()
			out := annotateLeaseFailure(in)
			wrapped := strings.Contains(out.Error(), "moved or differs from session start")
			if wrapped != c.wrap {
				t.Fatalf("wrap=%v want=%v (err=%q)", wrapped, c.wrap, out)
			}
			var inner packp.CommandStatusErr
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

func TestIsConcurrentMove(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   bool
	}{
		{"entire-server CAS rejection", "remote ref has changed", true},
		{"CAS rejection with surrounding detail", "command error on refs/heads/main: remote ref has changed", true},
		{"entire-server create-side CAS rejection", "already exists", true},
		{"create-side CAS with surrounding detail", "command error on refs/heads/PIE-11736: already exists", true},
		{"already exists case-insensitive", "Already Exists", true},
		{"force-with-lease stale info", "stale info", true},
		{"stale info case-insensitive", "Stale Info", true},
		// Deliberately NOT moves: a plain non-fast-forward that wasn't force-pushed
		// is indistinguishable from a race and would mask a real "needs --force".
		{"plain non-fast-forward is ambiguous", "non-fast-forward", false},
		{"fetch first is ambiguous", "fetch first", false},
		{"does not match is ambiguous", "remote ref does not match expected old value", false},
		{"policy rejection", "deny updating a hidden ref", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isConcurrentMove(c.reason); got != c.want {
				t.Fatalf("isConcurrentMove(%q) = %v, want %v", c.reason, got, c.want)
			}
		})
	}
}

func TestAsRefRejectedErrorClassifiesAndPreserves(t *testing.T) {
	cases := []struct {
		name   string
		status string
		moved  bool
	}{
		{"concurrent move (remote ref has changed)", "remote ref has changed", true},
		{"concurrent create (already exists)", "already exists", true},
		{"lease miss (stale info)", "stale info", true},
		{"ambiguous non-fast-forward is not a move", "non-fast-forward", false},
		{"policy rejection is not a move", "deny updating a hidden ref", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Faithful input: go-git's report.Error() returns CommandStatusErr BY
			// VALUE, not a pointer — the errors.As targets in annotateLeaseFailure /
			// asRefRejectedError must match the value.
			cs := (&packp.CommandStatus{ReferenceName: "refs/heads/main", Status: c.status}).Error()
			// Mirror the production chain: lease annotation, typed wrap, then the
			// same outer wrapping report-status / syncer / client apply with %w.
			wrapped := fmt.Errorf("sync: %w", fmt.Errorf("report-status: %w", asRefRejectedError(annotateLeaseFailure(cs))))

			var rej *RefRejectedError
			if !errors.As(wrapped, &rej) {
				t.Fatalf("errors.As must reach *RefRejectedError through wrapping; got %v", wrapped)
			}
			if rej.Ref != "refs/heads/main" || rej.Reason != c.status {
				t.Fatalf("Ref/Reason = %q/%q, want refs/heads/main and %q", rej.Ref, rej.Reason, c.status)
			}
			if got := errors.Is(wrapped, ErrTargetRefMoved); got != c.moved {
				t.Fatalf("errors.Is(ErrTargetRefMoved) = %v, want %v (status=%q)", got, c.moved, c.status)
			}
			// Backward compatibility: the underlying go-git error and the raw
			// reason substring must survive the typed wrap unchanged.
			var cse packp.CommandStatusErr
			if !errors.As(wrapped, &cse) || cse.Status != c.status {
				t.Fatalf("underlying packp.CommandStatusErr must be preserved; got %#v", wrapped)
			}
			if !strings.Contains(wrapped.Error(), c.status) {
				t.Fatalf("message must still contain the raw reason %q; got %q", c.status, wrapped.Error())
			}
		})
	}
}

func TestRefRejectedErrorZeroValueErrorDoesNotPanic(t *testing.T) {
	// An embedder may construct one from the exported fields alone (e.g. when
	// testing their own errors.As-based handling). Error() must not nil-panic on
	// the absent unexported wrapped err.
	e := &RefRejectedError{Ref: "refs/heads/main", Reason: "remote ref has changed"}
	got := e.Error()
	if !strings.Contains(got, "refs/heads/main") || !strings.Contains(got, "remote ref has changed") {
		t.Fatalf("zero-value Error() = %q, want it to include the ref and reason", got)
	}
	// moved defaults to false, so an externally-built value is never a move.
	if errors.Is(e, ErrTargetRefMoved) {
		t.Fatal("externally-constructed RefRejectedError must not satisfy ErrTargetRefMoved")
	}
}

func TestAsRefRejectedErrorPassesNonCommandStatusErrors(t *testing.T) {
	err := errors.New("decode report-status: short read")
	got := asRefRejectedError(err)
	if !errors.Is(got, err) {
		t.Fatalf("non-CommandStatusErr should pass through unchanged, got %v", got)
	}
	var rej *RefRejectedError
	if errors.As(got, &rej) {
		t.Fatalf("non-CommandStatusErr must not be wrapped as *RefRejectedError")
	}
}

// TestAsRefRejectedError_RealReportStatusPath drives the EXACT error go-git
// hands sendReceivePack — a value-typed packp.CommandStatusErr produced by
// ReportStatus.Error() — through the production wrap (annotateLeaseFailure →
// asRefRejectedError → "report-status: %w"). The hand-built table tests above
// can construct the input however they like; this one pins the classification
// to go-git's real return type. If a pointer-vs-value regression ever creeps
// back into the errors.As targets, asRefRejectedError stops matching, errors.Is
// goes false, and this fails — which is exactly the bug it guards against.
func TestAsRefRejectedError_RealReportStatusPath(t *testing.T) {
	rs := &packp.ReportStatus{
		UnpackStatus: "ok",
		CommandStatuses: []*packp.CommandStatus{
			{ReferenceName: "refs/heads/main", Status: "remote ref has changed"},
		},
	}
	reportErr := rs.Error() // value packp.CommandStatusErr, exactly as in sendReceivePack
	if reportErr == nil {
		t.Fatal("expected a rejection error from report-status")
	}

	wrapped := fmt.Errorf("report-status: %w", asRefRejectedError(annotateLeaseFailure(reportErr)))

	if !errors.Is(wrapped, ErrTargetRefMoved) {
		t.Fatalf("a real go-git report-status CAS rejection must satisfy errors.Is(ErrTargetRefMoved); got %v (underlying %T)", wrapped, reportErr)
	}
	var rej *RefRejectedError
	if !errors.As(wrapped, &rej) || rej.Reason != "remote ref has changed" {
		t.Fatalf("must classify as *RefRejectedError carrying the raw reason; got %#v", wrapped)
	}
}

// TestAsRefRejectedError_ToleratesPointerCommandStatusErr guards the value/
// pointer robustness in commandStatusErr. go-git returns CommandStatusErr by
// VALUE today (covered by the real-report-status test above); this pins the
// other form so that if go-git ever hands the error over as a *CommandStatusErr,
// classification keeps working instead of silently regressing to "every
// rejection unclassified".
func TestAsRefRejectedError_ToleratesPointerCommandStatusErr(t *testing.T) {
	ptr := &packp.CommandStatusErr{ReferenceName: "refs/heads/main", Status: "remote ref has changed"}
	wrapped := fmt.Errorf("report-status: %w", asRefRejectedError(annotateLeaseFailure(ptr)))

	if !errors.Is(wrapped, ErrTargetRefMoved) {
		t.Fatalf("a *CommandStatusErr rejection must also classify as ErrTargetRefMoved; got %v", wrapped)
	}
	var rej *RefRejectedError
	if !errors.As(wrapped, &rej) || rej.Reason != "remote ref has changed" {
		t.Fatalf("must classify the pointer form as *RefRejectedError; got %#v", wrapped)
	}
}
