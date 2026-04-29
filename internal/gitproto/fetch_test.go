package gitproto

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/pktline"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage/memory"
	"github.com/stretchr/testify/require"
)

const refsHeadsMain = "refs/heads/main"

func TestCapabilities(t *testing.T) {
	// v2 protocol
	v2Caps := &V2Capabilities{
		Caps: map[string]string{
			"fetch":   "shallow",
			"ls-refs": "",
			"agent":   "git/test",
		},
	}
	rs := &RefService{Protocol: "v2", V2Caps: v2Caps}
	got := rs.Capabilities()
	if len(got) != 3 {
		t.Fatalf("v2 Capabilities() returned %d items, want 3", len(got))
	}
	// Should be sorted.
	if got[0] != "agent=git/test" {
		t.Errorf("v2 Capabilities()[0] = %q, want %q", got[0], "agent=git/test")
	}

	// v1 protocol
	adv := packp.NewAdvRefs()
	require.NoError(t, adv.Capabilities.Set(capability.OFSDelta))
	rs = &RefService{Protocol: "v1", V1Adv: adv}
	got = rs.Capabilities()
	if len(got) == 0 {
		t.Fatal("v1 Capabilities() returned empty list")
	}

	// unknown protocol
	rs = &RefService{Protocol: "v99"}
	got = rs.Capabilities()
	if got != nil {
		t.Errorf("unknown protocol Capabilities() = %v, want nil", got)
	}
}

func TestFetchFeatures(t *testing.T) {
	v2Caps := &V2Capabilities{
		Caps: map[string]string{
			"fetch": "shallow filter include-tag",
		},
	}
	rs := &RefService{Protocol: "v2", V2Caps: v2Caps}
	features := rs.FetchFeatures()
	if !features.Filter || !features.IncludeTag {
		t.Fatalf("FetchFeatures() = %+v, want filter and include-tag enabled", features)
	}

	rs = &RefService{Protocol: "v1"}
	features = rs.FetchFeatures()
	if features.Filter || features.IncludeTag {
		t.Fatalf("FetchFeatures() for v1 = %+v, want zero value", features)
	}
}

func TestSupportsBootstrapBatch(t *testing.T) {
	if (&RefService{Protocol: "v1"}).SupportsBootstrapBatch() {
		t.Fatal("v1 service should not support bootstrap batching")
	}
	if (&RefService{
		Protocol: "v2",
		V2Caps:   &V2Capabilities{Caps: map[string]string{"fetch": "shallow"}},
	}).SupportsBootstrapBatch() {
		t.Fatal("v2 service without filter should not support bootstrap batching")
	}
	if !(&RefService{
		Protocol: "v2",
		V2Caps:   &V2Capabilities{Caps: map[string]string{"fetch": "filter"}},
	}).SupportsBootstrapBatch() {
		t.Fatal("v2 service with filter should support bootstrap batching")
	}
}

func TestBuildSidebandReader(t *testing.T) {
	data := "hello world"
	reader := bytes.NewBufferString(data)

	// No sideband support -- should return the original reader.
	caps := capability.NewList()
	got := buildSidebandReader(caps, reader, nil)
	if got != reader {
		t.Error("expected original reader when no sideband capability")
	}

	// With Sideband64k -- should return a demuxer (different reader).
	caps = capability.NewList()
	require.NoError(t, caps.Set(capability.Sideband64k))
	got = buildSidebandReader(caps, reader, nil)
	if got == reader {
		t.Error("expected wrapped reader when Sideband64k is set")
	}

	// With Sideband (not 64k) -- should return a demuxer.
	caps = capability.NewList()
	require.NoError(t, caps.Set(capability.Sideband))
	got = buildSidebandReader(caps, reader, nil)
	if got == reader {
		t.Error("expected wrapped reader when Sideband is set")
	}
}

func TestBuildSidebandReaderWithProgress(t *testing.T) {
	reader := bytes.NewBufferString("test")
	caps := capability.NewList()
	require.NoError(t, caps.Set(capability.Sideband64k))
	var progress sideband.Progress = io.Discard
	got := buildSidebandReader(caps, reader, progress)
	if got == reader {
		t.Error("expected wrapped reader when sideband capability is set")
	}
}

func TestProgressWriter(t *testing.T) {
	w := progressWriter(false)
	if w != nil {
		t.Error("progressWriter(false) should return nil")
	}
	w = progressWriter(true)
	if w == nil {
		t.Error("progressWriter(true) should return non-nil writer")
	}
}

func TestWrappedRCClose(t *testing.T) {
	// wrappedRC should close the underlying closer.
	called := false
	rc := &wrappedRC{
		Closer: closerFunc(func() error {
			called = true
			return nil
		}),
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if !called {
		t.Error("underlying closer was not called")
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

type blockingPacketBody struct {
	ctx         context.Context
	startedRead chan<- struct{}
	first       []byte
	stage       int
	closed      bool
}

func (b *blockingPacketBody) Read(p []byte) (int, error) {
	switch b.stage {
	case 0:
		b.stage = 1
		return copy(p, b.first), nil
	default:
		select {
		case b.startedRead <- struct{}{}:
		default:
		}
		<-b.ctx.Done()
		return 0, b.ctx.Err()
	}
}

func (b *blockingPacketBody) Close() error {
	b.closed = true
	return nil
}

type interruptedBody struct {
	data   []byte
	err    error
	offset int
	closed bool
}

func (b *interruptedBody) Read(p []byte) (int, error) {
	if b.offset < len(b.data) {
		n := copy(p, b.data[b.offset:])
		b.offset += n
		return n, nil
	}
	return 0, b.err
}

func (b *interruptedBody) Close() error {
	b.closed = true
	return nil
}

func TestDecodeV2LSRefs(t *testing.T) {
	// Build a valid ls-refs response:
	// Each line: "<hash> <refname>\n"
	wire := "" +
		FormatPktLine("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/heads/main\n") +
		FormatPktLine("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/heads/dev\n") +
		"0000" // flush

	refs, head, err := decodeV2LSRefs(bytes.NewReader([]byte(wire)))
	if err != nil {
		t.Fatalf("decodeV2LSRefs: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	if refs[0].Name().String() != refsHeadsMain {
		t.Errorf("refs[0].Name() = %q, want %q", refs[0].Name(), refsHeadsMain)
	}
	if refs[0].Hash().String() != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("refs[0].Hash() = %q", refs[0].Hash())
	}
	if refs[1].Name().String() != "refs/heads/dev" {
		t.Errorf("refs[1].Name() = %q, want %q", refs[1].Name(), "refs/heads/dev")
	}
	if head != "" {
		t.Errorf("head target = %q, want empty (no HEAD advertised)", head)
	}
}

func TestDecodeV2LSRefsHeadSymref(t *testing.T) {
	wire := "" +
		FormatPktLine("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa HEAD symref-target:refs/heads/main\n") +
		FormatPktLine("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/heads/main\n") +
		"0000"
	refs, head, err := decodeV2LSRefs(bytes.NewReader([]byte(wire)))
	if err != nil {
		t.Fatalf("decodeV2LSRefs: %v", err)
	}
	// HEAD is consumed for its symref target and not emitted as a ref.
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref (HEAD filtered), got %d", len(refs))
	}
	if refs[0].Name().String() != refsHeadsMain {
		t.Errorf("refs[0].Name() = %q, want refs/heads/main", refs[0].Name())
	}
	if head.String() != refsHeadsMain {
		t.Errorf("head target = %q, want refs/heads/main", head)
	}
}

func TestDecodeV2LSRefsMalformed(t *testing.T) {
	// Line with only one field (no refname).
	wire := FormatPktLine("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n") + "0000"
	_, _, err := decodeV2LSRefs(bytes.NewReader([]byte(wire)))
	if err == nil {
		t.Fatal("expected error for malformed ls-refs line, got nil")
	}
}

func TestDecodeV2LSRefsEmpty(t *testing.T) {
	// Empty response (just flush).
	wire := "0000"
	refs, _, err := decodeV2LSRefs(bytes.NewReader([]byte(wire)))
	if err != nil {
		t.Fatalf("decodeV2LSRefs: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("expected 0 refs, got %d", len(refs))
	}
}

func TestBufReader(t *testing.T) {
	input := bytes.NewBufferString("test data")
	pr := NewPacketReader(input)
	br := pr.BufReader()
	if br == nil {
		t.Fatal("BufReader() returned nil")
	}
}

func TestFetchToStoreUnsupportedProtocol(t *testing.T) {
	rs := &RefService{Protocol: "v99"}
	err := rs.FetchToStore(t.Context(), nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for unsupported protocol")
	}
}

func TestFetchPackUnsupportedProtocol(t *testing.T) {
	rs := &RefService{Protocol: "v99"}
	_, err := rs.FetchPack(t.Context(), nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for unsupported protocol")
	}
}

func TestFetchCommitGraphRequiresV2(t *testing.T) {
	rs := &RefService{Protocol: "v1"}
	err := rs.FetchCommitGraph(t.Context(), nil, nil, DesiredRef{}, nil)
	if err == nil {
		t.Fatal("expected error for non-v2 protocol")
	}
}

func TestFetchCommitGraphRequiresFilter(t *testing.T) {
	caps := &V2Capabilities{
		Caps: map[string]string{
			"fetch": "shallow",
		},
	}
	rs := &RefService{Protocol: "v2", V2Caps: caps}
	err := rs.FetchCommitGraph(t.Context(), nil, nil, DesiredRef{}, nil)
	if err == nil {
		t.Fatal("expected error when filter not supported")
	}
}

func TestFetchPackV1ContextCanceled(t *testing.T) {
	started := make(chan struct{}, 1)
	ep, err := transport.ParseURL("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	conn := NewConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))

	adv := packp.NewAdvRefs()
	require.NoError(t, adv.Capabilities.Set(capability.Sideband64k))
	desired := map[plumbing.ReferenceName]DesiredRef{
		plumbing.NewBranchReferenceName("main"): {
			SourceRef:  plumbing.NewBranchReferenceName("main"),
			TargetRef:  plumbing.NewBranchReferenceName("main"),
			SourceHash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := fetchPackV1(ctx, conn, adv, desired, nil, false)
		done <- err
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
		t.Fatal("fetchPackV1 did not return after cancellation")
	}
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestFetchPackV2ContextCanceled(t *testing.T) {
	started := make(chan struct{}, 1)
	ep, err := transport.ParseURL("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	conn := NewConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))

	caps := &V2Capabilities{
		Caps: map[string]string{
			"fetch": "",
		},
	}
	desired := map[plumbing.ReferenceName]DesiredRef{
		plumbing.NewBranchReferenceName("main"): {
			SourceRef:  plumbing.NewBranchReferenceName("main"),
			TargetRef:  plumbing.NewBranchReferenceName("main"),
			SourceHash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := fetchPackV2(ctx, conn, caps, desired, nil, false)
		done <- err
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
		t.Fatal("fetchPackV2 did not return after cancellation")
	}
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestFetchToStoreV2ContextCanceled(t *testing.T) {
	started := make(chan struct{}, 1)
	ep, err := transport.ParseURL("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	conn := NewConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))

	caps := &V2Capabilities{
		Caps: map[string]string{
			"fetch": "",
		},
	}
	desired := map[plumbing.ReferenceName]DesiredRef{
		plumbing.NewBranchReferenceName("main"): {
			SourceRef:  plumbing.NewBranchReferenceName("main"),
			TargetRef:  plumbing.NewBranchReferenceName("main"),
			SourceHash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- fetchToStoreV2(ctx, memory.NewStorage(), conn, caps, desired, nil, false)
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
		t.Fatal("fetchToStoreV2 did not return after cancellation")
	}
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestFetchToStoreV2ClosesBodyOnDecodeError(t *testing.T) {
	ep, err := transport.ParseURL("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	body := &trackingReadCloser{ReadCloser: io.NopCloser(bytes.NewBufferString(FormatPktLine("bogus\n") + "0000"))}
	conn := NewConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Request:    req,
			Body:       body,
		}, nil
	}))

	caps := &V2Capabilities{
		Caps: map[string]string{
			"fetch": "",
		},
	}
	desired := map[plumbing.ReferenceName]DesiredRef{
		plumbing.NewBranchReferenceName("main"): {
			SourceRef:  plumbing.NewBranchReferenceName("main"),
			TargetRef:  plumbing.NewBranchReferenceName("main"),
			SourceHash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
	}

	err = fetchToStoreV2(context.Background(), memory.NewStorage(), conn, caps, desired, nil, false)
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !body.closed {
		t.Fatal("expected response body to be closed on decode error")
	}
}

func TestFetchToStoreV2ContextCanceledMidStream(t *testing.T) {
	startedRead := make(chan struct{}, 1)
	ep, err := transport.ParseURL("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	conn := NewConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body := &blockingPacketBody{
			ctx:         req.Context(),
			startedRead: startedRead,
			first:       []byte(FormatPktLine("packfile\n")),
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Request:    req,
			Body:       body,
		}, nil
	}))

	caps := &V2Capabilities{
		Caps: map[string]string{
			"fetch": "",
		},
	}
	desired := map[plumbing.ReferenceName]DesiredRef{
		plumbing.NewBranchReferenceName("main"): {
			SourceRef:  plumbing.NewBranchReferenceName("main"),
			TargetRef:  plumbing.NewBranchReferenceName("main"),
			SourceHash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- fetchToStoreV2(ctx, memory.NewStorage(), conn, caps, desired, nil, false)
	}()

	select {
	case <-startedRead:
	case <-time.After(2 * time.Second):
		t.Fatal("response body was not consumed before timeout")
	}
	cancel()

	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fetchToStoreV2 did not return after cancellation")
	}
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestFetchPackV1ClosesBodyOnDecodeError(t *testing.T) {
	ep, err := transport.ParseURL("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	body := &trackingReadCloser{ReadCloser: io.NopCloser(bytes.NewBufferString("not-a-valid-server-response"))}
	conn := NewConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Request:    req,
			Body:       body,
		}, nil
	}))

	adv := packp.NewAdvRefs()
	desired := map[plumbing.ReferenceName]DesiredRef{
		plumbing.NewBranchReferenceName("main"): {
			SourceRef:  plumbing.NewBranchReferenceName("main"),
			TargetRef:  plumbing.NewBranchReferenceName("main"),
			SourceHash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
	}

	_, err = fetchPackV1(context.Background(), conn, adv, desired, nil, false)
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !body.closed {
		t.Fatal("expected response body to be closed on decode error")
	}
}

func TestFetchPackV1ReturnedReaderClosesBodyOnInterruption(t *testing.T) {
	ep, err := transport.ParseURL("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	body := &interruptedBody{
		data: []byte("0008NAK\nPACK"),
		err:  io.ErrUnexpectedEOF,
	}
	conn := NewConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Request:    req,
			Body:       body,
		}, nil
	}))

	adv := packp.NewAdvRefs()
	desired := map[plumbing.ReferenceName]DesiredRef{
		plumbing.NewBranchReferenceName("main"): {
			SourceRef:  plumbing.NewBranchReferenceName("main"),
			TargetRef:  plumbing.NewBranchReferenceName("main"),
			SourceHash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
	}

	rc, err := fetchPackV1(context.Background(), conn, adv, desired, nil, false)
	if err != nil {
		t.Fatalf("fetchPackV1: %v", err)
	}
	data, err := io.ReadAll(rc)
	if len(data) == 0 {
		t.Fatal("expected partial pack data before interruption")
	}
	if err == nil {
		t.Fatal("expected interrupted read error")
	}
	if closeErr := rc.Close(); closeErr != nil {
		t.Fatalf("close returned reader: %v", closeErr)
	}
	if !body.closed {
		t.Fatal("expected returned reader close to close underlying body")
	}
}

func TestFetchPackV1ReturnedReaderErrorsOnMalformedMidStreamPacket(t *testing.T) {
	ep, err := transport.ParseURL("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	body := &trackingReadCloser{ReadCloser: io.NopCloser(bytes.NewBufferString("0008NAK\nzzzz"))}
	conn := NewConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Request:    req,
			Body:       body,
		}, nil
	}))

	adv := packp.NewAdvRefs()
	require.NoError(t, adv.Capabilities.Set(capability.Sideband64k))
	desired := map[plumbing.ReferenceName]DesiredRef{
		plumbing.NewBranchReferenceName("main"): {
			SourceRef:  plumbing.NewBranchReferenceName("main"),
			TargetRef:  plumbing.NewBranchReferenceName("main"),
			SourceHash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
	}

	rc, err := fetchPackV1(context.Background(), conn, adv, desired, nil, false)
	if err != nil {
		t.Fatalf("fetchPackV1: %v", err)
	}
	_, err = io.ReadAll(rc)
	if err == nil {
		t.Fatal("expected malformed mid-stream sideband error")
	}
	if closeErr := rc.Close(); closeErr != nil {
		t.Fatalf("close returned reader: %v", closeErr)
	}
	if !body.closed {
		t.Fatal("expected returned reader close to close underlying body")
	}
}

func TestFetchPackV1DrainsSecondNAK(t *testing.T) {
	// go-git's upload-pack server emits two NAKs when haves were sent but none
	// were reachable from the wants. ServerResponse.Decode only consumes the
	// first; the second must be drained before sideband demux or the demuxer
	// misreads "NAK\n" as a sideband frame with channel 'N'.
	// This simulates that double-NAK followed by a sideband-wrapped PACK header
	// (channel 0x01 + "PACK" magic).
	payload := []byte("0008NAK\n0008NAK\n0009\x01PACK")
	ep, err := transport.ParseURL("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	body := &trackingReadCloser{ReadCloser: io.NopCloser(bytes.NewReader(payload))}
	conn := NewConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Request:    req,
			Body:       body,
		}, nil
	}))

	adv := packp.NewAdvRefs()
	require.NoError(t, adv.Capabilities.Set(capability.Sideband64k))
	desired := map[plumbing.ReferenceName]DesiredRef{
		plumbing.NewBranchReferenceName("main"): {
			SourceRef:  plumbing.NewBranchReferenceName("main"),
			TargetRef:  plumbing.NewBranchReferenceName("main"),
			SourceHash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
	}

	rc, err := fetchPackV1(context.Background(), conn, adv, desired, nil, false)
	if err != nil {
		t.Fatalf("fetchPackV1 should tolerate double-NAK preamble, got: %v", err)
	}
	got, err := io.ReadAll(rc)
	// Reader will error (or hit EOF) after the truncated sideband frame; what
	// we care about is that the demuxed payload started with PACK, proving the
	// second NAK was drained rather than fed into the demuxer.
	if !bytes.HasPrefix(got, []byte("PACK")) {
		t.Fatalf("expected demuxed pack bytes to start with PACK, got %q (err=%v)", got, err)
	}
	if closeErr := rc.Close(); closeErr != nil {
		t.Fatalf("close returned reader: %v", closeErr)
	}
	if !body.closed {
		t.Fatal("expected returned reader close to close underlying body")
	}
}

func TestFetchPackV2ClosesBodyOnDecodeError(t *testing.T) {
	ep, err := transport.ParseURL("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	body := &trackingReadCloser{ReadCloser: io.NopCloser(bytes.NewBufferString("0000"))}
	conn := NewConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Request:    req,
			Body:       body,
		}, nil
	}))

	caps := &V2Capabilities{
		Caps: map[string]string{
			"fetch": "",
		},
	}
	desired := map[plumbing.ReferenceName]DesiredRef{
		plumbing.NewBranchReferenceName("main"): {
			SourceRef:  plumbing.NewBranchReferenceName("main"),
			TargetRef:  plumbing.NewBranchReferenceName("main"),
			SourceHash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
	}

	_, err = fetchPackV2(context.Background(), conn, caps, desired, nil, false)
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !body.closed {
		t.Fatal("expected response body to be closed on decode error")
	}
}

func TestStoreV2FetchPackReturnsRemoteError(t *testing.T) {
	var wire bytes.Buffer
	if _, err := pktline.WriteString(&wire, "ERR upload-pack: not our ref"); err != nil {
		t.Fatalf("write remote error: %v", err)
	}

	err := storeV2FetchPack(memory.NewStorage(), &wire, false)
	if err == nil {
		t.Fatal("expected remote error")
	}
	if got, want := err.Error(), "remote: upload-pack: not our ref"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestOpenV2PackStreamReturnsRemoteError(t *testing.T) {
	var wire bytes.Buffer
	if _, err := pktline.WriteString(&wire, "ERR upload-pack: not our ref"); err != nil {
		t.Fatalf("write remote error: %v", err)
	}

	_, err := openV2PackStream(io.NopCloser(&wire), false)
	if err == nil {
		t.Fatal("expected remote error")
	}
	if got, want := err.Error(), "remote: upload-pack: not our ref"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestFetchPackV2ReturnedReaderClosesBodyOnInterruption(t *testing.T) {
	ep, err := transport.ParseURL("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	var wire bytes.Buffer
	if _, err := pktline.WriteString(&wire, "packfile\n"); err != nil {
		t.Fatalf("write packfile header: %v", err)
	}
	if _, err := pktline.Write(&wire, append([]byte{1}, []byte("PACK")...)); err != nil {
		t.Fatalf("write sideband packet: %v", err)
	}
	body := &interruptedBody{
		data: wire.Bytes(),
		err:  io.ErrUnexpectedEOF,
	}
	conn := NewConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Request:    req,
			Body:       body,
		}, nil
	}))

	caps := &V2Capabilities{
		Caps: map[string]string{
			"fetch": "",
		},
	}
	desired := map[plumbing.ReferenceName]DesiredRef{
		plumbing.NewBranchReferenceName("main"): {
			SourceRef:  plumbing.NewBranchReferenceName("main"),
			TargetRef:  plumbing.NewBranchReferenceName("main"),
			SourceHash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
	}

	rc, err := fetchPackV2(context.Background(), conn, caps, desired, nil, false)
	if err != nil {
		t.Fatalf("fetchPackV2: %v", err)
	}
	data, err := io.ReadAll(rc)
	if len(data) == 0 {
		t.Fatal("expected partial pack data before interruption")
	}
	if err == nil {
		t.Fatal("expected interrupted read error")
	}
	if closeErr := rc.Close(); closeErr != nil {
		t.Fatalf("close returned reader: %v", closeErr)
	}
	if !body.closed {
		t.Fatal("expected returned reader close to close underlying body")
	}
}

func TestFetchPackV2ReturnedReaderErrorsOnMalformedMidStreamPacket(t *testing.T) {
	ep, err := transport.ParseURL("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	body := &trackingReadCloser{ReadCloser: io.NopCloser(bytes.NewBufferString(FormatPktLine("packfile\n") + "zzzz"))}
	conn := NewConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Request:    req,
			Body:       body,
		}, nil
	}))

	caps := &V2Capabilities{
		Caps: map[string]string{
			"fetch": "",
		},
	}
	desired := map[plumbing.ReferenceName]DesiredRef{
		plumbing.NewBranchReferenceName("main"): {
			SourceRef:  plumbing.NewBranchReferenceName("main"),
			TargetRef:  plumbing.NewBranchReferenceName("main"),
			SourceHash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
	}

	rc, err := fetchPackV2(context.Background(), conn, caps, desired, nil, false)
	if err != nil {
		t.Fatalf("fetchPackV2: %v", err)
	}
	_, err = io.ReadAll(rc)
	if err == nil {
		t.Fatal("expected malformed mid-stream sideband error")
	}
	if closeErr := rc.Close(); closeErr != nil {
		t.Fatalf("close returned reader: %v", closeErr)
	}
	if !body.closed {
		t.Fatal("expected returned reader close to close underlying body")
	}
}

func TestFetchToStoreV2ClosesBodyOnMalformedMidStreamPacket(t *testing.T) {
	ep, err := transport.ParseURL("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	body := &trackingReadCloser{ReadCloser: io.NopCloser(bytes.NewBufferString(FormatPktLine("packfile\n") + "zzzz"))}
	conn := NewConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Request:    req,
			Body:       body,
		}, nil
	}))

	caps := &V2Capabilities{
		Caps: map[string]string{
			"fetch": "",
		},
	}
	desired := map[plumbing.ReferenceName]DesiredRef{
		plumbing.NewBranchReferenceName("main"): {
			SourceRef:  plumbing.NewBranchReferenceName("main"),
			TargetRef:  plumbing.NewBranchReferenceName("main"),
			SourceHash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
	}

	err = fetchToStoreV2(context.Background(), memory.NewStorage(), conn, caps, desired, nil, false)
	if err == nil {
		t.Fatal("expected malformed mid-stream sideband error")
	}
	if !body.closed {
		t.Fatal("expected response body to be closed on malformed mid-stream packet")
	}
}

func TestBuildV1UploadPackBodyEmptyWantSet(t *testing.T) {
	adv := packp.NewAdvRefs()
	_, _, err := buildV1UploadPackBody(adv, nil, nil, false, false)
	if !errors.Is(err, git.NoErrAlreadyUpToDate) {
		t.Fatalf("expected NoErrAlreadyUpToDate, got %v", err)
	}
}
