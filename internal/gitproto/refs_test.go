package gitproto

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/pktline"
	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/transport"
)

func TestRefHashMap(t *testing.T) {
	hashA := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	hashB := plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	refs := []*plumbing.Reference{
		plumbing.NewHashReference(refsHeadsMain, hashA),
		plumbing.NewHashReference("refs/heads/dev", hashB),
		plumbing.NewSymbolicReference("HEAD", refsHeadsMain), // symbolic, should be skipped
	}

	m := RefHashMap(refs)

	if len(m) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(m))
	}
	if got := m[refsHeadsMain]; got != hashA {
		t.Errorf("refs/heads/main = %s, want %s", got, hashA)
	}
	if got := m["refs/heads/dev"]; got != hashB {
		t.Errorf("refs/heads/dev = %s, want %s", got, hashB)
	}

	// Empty input.
	m = RefHashMap(nil)
	if len(m) != 0 {
		t.Errorf("RefHashMap(nil) returned %d entries, want 0", len(m))
	}
}

func TestHeadTargetFromAdv(t *testing.T) {
	// nil returns empty.
	if got := headTargetFromAdv(nil); got != "" {
		t.Errorf("headTargetFromAdv(nil) = %q, want empty", got)
	}

	adv := &packp.AdvRefs{}
	adv.Capabilities.Add(capability.SymRef, "HEAD:refs/heads/main")
	if got := headTargetFromAdv(adv); got.String() != refsHeadsMain {
		t.Errorf("headTargetFromAdv = %q, want refs/heads/main", got)
	}

	// Symref pointing at something other than HEAD is ignored.
	adv = &packp.AdvRefs{}
	adv.Capabilities.Add(capability.SymRef, "refs/remotes/origin/HEAD:refs/heads/main")
	if got := headTargetFromAdv(adv); got != "" {
		t.Errorf("headTargetFromAdv ignored non-HEAD symref = %q, want empty", got)
	}
}

func TestAdvRefsCaps(t *testing.T) {
	// nil AdvRefs should return nil.
	if got := AdvRefsCaps(nil); got != nil {
		t.Errorf("AdvRefsCaps(nil) = %v, want nil", got)
	}

	// AdvRefs with empty Capabilities should return nil.
	adv := &packp.AdvRefs{}
	if got := AdvRefsCaps(adv); got != nil {
		t.Errorf("AdvRefsCaps(empty caps) = %v, want nil", got)
	}

	// AdvRefs with populated capabilities.
	adv = &packp.AdvRefs{}
	adv.Capabilities.Set(capability.OFSDelta)
	adv.Capabilities.Add(capability.Agent, "git/test-agent")
	adv.Capabilities.Set(capability.NoProgress)

	items := AdvRefsCaps(adv)
	if len(items) == 0 {
		t.Fatal("expected non-empty capability list")
	}

	// Verify that known capabilities appear in the output.
	found := make(map[string]bool)
	for _, item := range items {
		found[item] = true
	}
	if !found["ofs-delta"] {
		t.Error("expected ofs-delta in capability list")
	}
	if !found["agent=git/test-agent"] {
		t.Errorf("expected agent=git/test-agent in capability list, got items: %v", items)
	}
	if !found["no-progress"] {
		t.Error("expected no-progress in capability list")
	}
}

func TestAdvRefsToSlice(t *testing.T) {
	adv := &packp.AdvRefs{}
	hashA := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	hashB := plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	adv.References = []*plumbing.Reference{
		plumbing.NewHashReference(refsHeadsMain, hashA),
		plumbing.NewHashReference("refs/heads/dev", hashB),
	}

	refs, err := AdvRefsToSlice(adv)
	if err != nil {
		t.Fatalf("AdvRefsToSlice: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}

	found := make(map[plumbing.ReferenceName]plumbing.Hash)
	for _, ref := range refs {
		found[ref.Name()] = ref.Hash()
	}
	if found[refsHeadsMain] != hashA {
		t.Errorf("refs/heads/main = %s, want %s", found[refsHeadsMain], hashA)
	}
	if found["refs/heads/dev"] != hashB {
		t.Errorf("refs/heads/dev = %s, want %s", found["refs/heads/dev"], hashB)
	}
}

// Regression: v1 advertisements include peeled "^{}" entries for annotated tags
// to expose the commit the tag points at. They are wire-protocol metadata, not
// real refs — receive-pack rejects them with "invalid reference name" when the
// planner schedules a delete for one. AdvRefsToSlice must drop them.
func TestAdvRefsToSliceDropsPeeledTagEntries(t *testing.T) {
	adv := &packp.AdvRefs{}
	tagHash := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	commitHash := plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	adv.References = []*plumbing.Reference{
		plumbing.NewHashReference("refs/tags/v1", tagHash),
		plumbing.NewHashReference("refs/tags/v1^{}", commitHash),
	}

	refs, err := AdvRefsToSlice(adv)
	if err != nil {
		t.Fatalf("AdvRefsToSlice: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref after peeled drop, got %d: %v", len(refs), refs)
	}
	if refs[0].Name() != "refs/tags/v1" {
		t.Errorf("got %s, want refs/tags/v1", refs[0].Name())
	}
}

func TestDecodeV1AdvRefs(t *testing.T) {
	// Empty data should return ErrEmptyRemoteRepository.
	_, err := decodeV1AdvRefs(nil)
	if err == nil {
		t.Fatal("expected error for nil data, got nil")
	}

	// Empty bytes should also error.
	_, err = decodeV1AdvRefs([]byte{})
	if err == nil {
		t.Fatal("expected error for empty data, got nil")
	}
}

func TestDecodeV1AdvRefsSmartEmptyAdvertisement(t *testing.T) {
	var body strings.Builder
	if _, err := pktline.Writef(&body, "# service=%s\n", transport.ReceivePackService); err != nil {
		t.Fatalf("write smart service line: %v", err)
	}
	if err := pktline.WriteFlush(&body); err != nil {
		t.Fatalf("write smart flush: %v", err)
	}

	_, err := decodeV1AdvRefs([]byte(body.String()))
	if !errors.Is(err, transport.ErrEmptyRemoteRepository) {
		t.Fatalf("expected empty remote repository, got %v", err)
	}
}

func TestDecodeV1AdvRefsMalformedIncludesPreview(t *testing.T) {
	_, err := decodeV1AdvRefs([]byte("# service=git-receive-pack"))
	if err == nil {
		t.Fatal("expected malformed decode error")
	}
	if !strings.Contains(err.Error(), `body-prefix="# service=git-receive-pack"`) {
		t.Fatalf("expected body preview in error, got %v", err)
	}
}

func TestListSourceRefsUnsupportedProtocol(t *testing.T) {
	_, _, err := ListSourceRefs(context.Background(), nil, "v99", nil)
	if err == nil {
		t.Fatal("expected error for unsupported protocol mode")
	}
}

func TestListSourceRefsAutoFallsBackToV1AfterSSHV2ProbeError(t *testing.T) {
	t.Parallel()

	conn := &stubConn{
		reqInfoRefs: func(_ context.Context, _ string, gitProtocol string) ([]byte, error) {
			if gitProtocol == GitProtocolV2 {
				return nil, errors.New("ssh server rejected v2 probe")
			}

			var body strings.Builder
			if _, err := pktline.Writef(&body, "# service=%s\n", transport.UploadPackService); err != nil {
				t.Fatalf("write smart service line: %v", err)
			}
			if err := pktline.WriteFlush(&body); err != nil {
				t.Fatalf("write smart flush: %v", err)
			}
			if _, err := pktline.Writef(&body, "%s HEAD\x00%s\n", strings.Repeat("a", 40), capability.SymRef+"=HEAD:refs/heads/main"); err != nil {
				t.Fatalf("write advertised head: %v", err)
			}
			if _, err := pktline.Writef(&body, "%s refs/heads/main\n", strings.Repeat("a", 40)); err != nil {
				t.Fatalf("write advertised ref: %v", err)
			}
			if err := pktline.WriteFlush(&body); err != nil {
				t.Fatalf("write trailing flush: %v", err)
			}
			return []byte(body.String()), nil
		},
	}

	refs, svc, err := ListSourceRefs(t.Context(), conn, "auto", nil)
	if err != nil {
		t.Fatalf("ListSourceRefs(auto) error = %v", err)
	}
	if svc.Protocol != "v1" {
		t.Fatalf("protocol = %q, want v1", svc.Protocol)
	}
	if got := svc.HeadTarget.String(); got != refsHeadsMain {
		t.Fatalf("head target = %q, want refs/heads/main", got)
	}
	foundMain := false
	for _, ref := range refs {
		if ref.Name().String() == refsHeadsMain {
			foundMain = true
			break
		}
	}
	if !foundMain {
		t.Fatalf("refs = %#v, want refs/heads/main to be advertised", refs)
	}
}

func TestListSourceRefsAutoJoinsErrorsWhenV1FallbackAlsoFails(t *testing.T) {
	t.Parallel()

	v2Err := errors.New("ssh v2 probe failed")
	v1Err := errors.New("ssh v1 fallback failed")
	conn := &stubConn{
		reqInfoRefs: func(_ context.Context, _ string, gitProtocol string) ([]byte, error) {
			if gitProtocol == GitProtocolV2 {
				return nil, v2Err
			}
			return nil, v1Err
		},
	}

	_, _, err := ListSourceRefs(t.Context(), conn, "auto", nil)
	if err == nil {
		t.Fatal("expected error when both v2 probe and v1 fallback fail")
	}
	if !errors.Is(err, v2Err) {
		t.Errorf("err does not wrap v2 error: %v", err)
	}
	if !errors.Is(err, v1Err) {
		t.Errorf("err does not wrap v1 error: %v", err)
	}
}

func TestListSourceRefsAutoDoesNotFallBackForNonSSH(t *testing.T) {
	t.Parallel()

	v2Err := errors.New("https v2 probe failed")
	v1Called := false
	conn := &stubConn{
		reqInfoRefs: func(_ context.Context, _ string, gitProtocol string) ([]byte, error) {
			if gitProtocol == GitProtocolV2 {
				return nil, v2Err
			}
			v1Called = true
			return nil, errors.New("unexpected v1 call")
		},
		endpoint: &url.URL{Scheme: "https", Host: "github.com"},
	}

	_, _, err := ListSourceRefs(t.Context(), conn, "auto", nil)
	if err == nil {
		t.Fatal("expected error from v2 probe")
	}
	if !errors.Is(err, v2Err) {
		t.Errorf("err does not wrap v2 error: %v", err)
	}
	if v1Called {
		t.Error("v1 fallback should not be attempted for non-SSH schemes")
	}
}

type stubConn struct {
	reqInfoRefs func(ctx context.Context, service string, gitProtocol string) ([]byte, error)
	endpoint    *url.URL
}

func (s *stubConn) RequestInfoRefs(ctx context.Context, service string, gitProtocol string) ([]byte, error) {
	return s.reqInfoRefs(ctx, service, gitProtocol)
}

func (s *stubConn) PostRPCStreamBody(context.Context, string, io.Reader, bool, string) (io.ReadCloser, error) {
	return nil, errors.New("unexpected PostRPCStreamBody call")
}

func (s *stubConn) Endpoint() *url.URL {
	if s.endpoint != nil {
		return s.endpoint
	}
	return &url.URL{Scheme: "ssh", Host: "github.com"}
}

func (s *stubConn) ProgressWriter() io.Writer { return nil }

func (s *stubConn) SetProgressWriter(io.Writer) {}

func (s *stubConn) Close() error { return nil }
