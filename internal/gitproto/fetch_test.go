package gitproto

import (
	"bytes"
	"io"
	"testing"

	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/sideband"
)

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
	_ = adv.Capabilities.Set(capability.OFSDelta)
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
	_ = caps.Set(capability.Sideband64k)
	got = buildSidebandReader(caps, reader, nil)
	if got == reader {
		t.Error("expected wrapped reader when Sideband64k is set")
	}

	// With Sideband (not 64k) -- should return a demuxer.
	caps = capability.NewList()
	_ = caps.Set(capability.Sideband)
	got = buildSidebandReader(caps, reader, nil)
	if got == reader {
		t.Error("expected wrapped reader when Sideband is set")
	}
}

func TestBuildSidebandReaderWithProgress(t *testing.T) {
	reader := bytes.NewBufferString("test")
	caps := capability.NewList()
	_ = caps.Set(capability.Sideband64k)
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
		Reader: bytes.NewBufferString("data"),
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

func TestDecodeV2LSRefs(t *testing.T) {
	// Build a valid ls-refs response:
	// Each line: "<hash> <refname>\n"
	wire := "" +
		FormatPktLine("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/heads/main\n") +
		FormatPktLine("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/heads/dev\n") +
		"0000" // flush

	refs, err := decodeV2LSRefs(bytes.NewReader([]byte(wire)))
	if err != nil {
		t.Fatalf("decodeV2LSRefs: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	if refs[0].Name().String() != "refs/heads/main" {
		t.Errorf("refs[0].Name() = %q, want %q", refs[0].Name(), "refs/heads/main")
	}
	if refs[0].Hash().String() != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("refs[0].Hash() = %q", refs[0].Hash())
	}
	if refs[1].Name().String() != "refs/heads/dev" {
		t.Errorf("refs[1].Name() = %q, want %q", refs[1].Name(), "refs/heads/dev")
	}
}

func TestDecodeV2LSRefsMalformed(t *testing.T) {
	// Line with only one field (no refname).
	wire := FormatPktLine("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n") + "0000"
	_, err := decodeV2LSRefs(bytes.NewReader([]byte(wire)))
	if err == nil {
		t.Fatal("expected error for malformed ls-refs line, got nil")
	}
}

func TestDecodeV2LSRefsEmpty(t *testing.T) {
	// Empty response (just flush).
	wire := "0000"
	refs, err := decodeV2LSRefs(bytes.NewReader([]byte(wire)))
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
	err := rs.FetchToStore(nil, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for unsupported protocol")
	}
}

func TestFetchPackUnsupportedProtocol(t *testing.T) {
	rs := &RefService{Protocol: "v99"}
	_, err := rs.FetchPack(nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for unsupported protocol")
	}
}

func TestFetchCommitGraphRequiresV2(t *testing.T) {
	rs := &RefService{Protocol: "v1"}
	err := rs.FetchCommitGraph(nil, nil, nil, DesiredRef{})
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
	err := rs.FetchCommitGraph(nil, nil, nil, DesiredRef{})
	if err == nil {
		t.Fatal("expected error when filter not supported")
	}
}
