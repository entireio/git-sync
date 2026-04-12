package gitproto

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
)

func TestRefHashMap(t *testing.T) {
	hashA := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	hashB := plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	refs := []*plumbing.Reference{
		plumbing.NewHashReference("refs/heads/main", hashA),
		plumbing.NewHashReference("refs/heads/dev", hashB),
		plumbing.NewSymbolicReference("HEAD", "refs/heads/main"), // symbolic, should be skipped
	}

	m := RefHashMap(refs)

	if len(m) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(m))
	}
	if got := m["refs/heads/main"]; got != hashA {
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

func TestAdvRefsCaps(t *testing.T) {
	// nil AdvRefs should return nil.
	if got := AdvRefsCaps(nil); got != nil {
		t.Errorf("AdvRefsCaps(nil) = %v, want nil", got)
	}

	// AdvRefs with nil Capabilities should return nil.
	adv := &packp.AdvRefs{}
	adv.Capabilities = nil
	if got := AdvRefsCaps(adv); got != nil {
		t.Errorf("AdvRefsCaps(nil caps) = %v, want nil", got)
	}

	// AdvRefs with populated capabilities.
	adv = packp.NewAdvRefs()
	_ = adv.Capabilities.Set(capability.OFSDelta)
	_ = adv.Capabilities.Add(capability.Agent, "git/test-agent")
	_ = adv.Capabilities.Set(capability.NoProgress)

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
	adv := packp.NewAdvRefs()
	hashA := plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	hashB := plumbing.NewHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	adv.References = map[string]plumbing.Hash{
		"refs/heads/main": hashA,
		"refs/heads/dev":  hashB,
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
	if found["refs/heads/main"] != hashA {
		t.Errorf("refs/heads/main = %s, want %s", found["refs/heads/main"], hashA)
	}
	if found["refs/heads/dev"] != hashB {
		t.Errorf("refs/heads/dev = %s, want %s", found["refs/heads/dev"], hashB)
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

func TestListSourceRefsUnsupportedProtocol(t *testing.T) {
	_, _, err := ListSourceRefs(context.Background(), nil, "v99", nil)
	if err == nil {
		t.Fatal("expected error for unsupported protocol mode")
	}
}

