package syncer

import (
	"bytes"
	"errors"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"
)

// buildPack encodes n blobs into a packfile and returns the bytes.
func buildPack(t *testing.T, n int) []byte {
	t.Helper()
	store := memory.NewStorage()
	hashes := make([]plumbing.Hash, 0, n)
	for i := range n {
		obj := store.NewEncodedObject()
		obj.SetType(plumbing.BlobObject)
		w, err := obj.Writer()
		if err != nil {
			t.Fatalf("blob writer: %v", err)
		}
		// Distinct content so each blob is its own object.
		if _, err := w.Write([]byte{byte(i), byte(i >> 8), byte(i >> 16)}); err != nil {
			t.Fatalf("blob write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("blob close: %v", err)
		}
		h, err := store.SetEncodedObject(obj)
		if err != nil {
			t.Fatalf("blob set: %v", err)
		}
		hashes = append(hashes, h)
	}
	var buf bytes.Buffer
	enc := packfile.NewEncoder(&buf, store, false)
	if _, err := enc.Encode(hashes, 10); err != nil {
		t.Fatalf("encode pack: %v", err)
	}
	return buf.Bytes()
}

// The limit must stop the stream, not report an overrun after the objects are
// already resident. Decoding a pack of 50 objects into a store bounded at 10
// must fail, and the store must hold no more than the limit.
func TestBoundedStorerStopsMidStream(t *testing.T) {
	const limit = 10
	pack := buildPack(t, 50)

	inner := memory.NewStorage()
	bounded := newBoundedStorer(inner, limit)

	err := packfile.UpdateObjectStorage(bounded, bytes.NewReader(pack))
	if err == nil {
		t.Fatal("expected the object limit to fail the decode, got nil")
	}
	if !errors.Is(err, ErrObjectLimit) {
		t.Errorf("error %v does not satisfy errors.Is(err, ErrObjectLimit)", err)
	}
	var limitErr *ObjectLimitError
	if !errors.As(err, &limitErr) {
		t.Errorf("error %v is not an *ObjectLimitError", err)
	} else if limitErr.Limit != limit {
		t.Errorf("reported limit %d, want %d", limitErr.Limit, limit)
	}

	// The point of moving the check earlier: memory stays bounded.
	stored := 0
	iter, err := inner.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		t.Fatalf("iter objects: %v", err)
	}
	if err := iter.ForEach(func(plumbing.EncodedObject) error {
		stored++
		return nil
	}); err != nil {
		t.Fatalf("count objects: %v", err)
	}
	if stored > limit {
		t.Errorf("store admitted %d objects, limit was %d", stored, limit)
	}
}

// A pack that fits must decode unchanged, so the guard cannot be satisfied by
// rejecting everything.
func TestBoundedStorerAdmitsPackWithinLimit(t *testing.T) {
	pack := buildPack(t, 5)
	inner := memory.NewStorage()
	bounded := newBoundedStorer(inner, 100)

	if err := packfile.UpdateObjectStorage(bounded, bytes.NewReader(pack)); err != nil {
		t.Fatalf("decode within limit: %v", err)
	}
	for _, typ := range []plumbing.ObjectType{plumbing.BlobObject} {
		iter, err := inner.IterEncodedObjects(typ)
		if err != nil {
			t.Fatalf("iter: %v", err)
		}
		n := 0
		if err := iter.ForEach(func(plumbing.EncodedObject) error { n++; return nil }); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 5 {
			t.Errorf("stored %d blobs, want 5", n)
		}
	}
}

// Zero or negative means unlimited, and must not pay for a wrapper.
func TestNewBoundedStorerUnlimitedReturnsInput(t *testing.T) {
	inner := memory.NewStorage()
	for _, limit := range []int{0, -1} {
		if got := newBoundedStorer(inner, limit); got != inner {
			t.Errorf("limit %d: expected the input store back unwrapped, got %T", limit, got)
		}
	}
}

// Reads must pass through untouched — planning and the push path use the same
// store the fetch wrote to.
func TestBoundedStorerPassesReadsThrough(t *testing.T) {
	inner := memory.NewStorage()
	bounded := newBoundedStorer(inner, 100)

	commit := &object.Commit{Message: "hello"}
	obj := bounded.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		t.Fatalf("encode commit: %v", err)
	}
	h, err := bounded.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("set commit: %v", err)
	}
	if _, err := bounded.EncodedObject(plumbing.CommitObject, h); err != nil {
		t.Errorf("read back through the wrapper: %v", err)
	}
	if _, err := inner.EncodedObject(plumbing.CommitObject, h); err != nil {
		t.Errorf("object did not reach the underlying store: %v", err)
	}
}
