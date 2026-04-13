package syncer

import (
	"errors"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage/memory"
)

func TestLimitedStorerSetEncodedObjectStopsAtLimit(t *testing.T) {
	base := memory.NewStorage()
	store := newLimitedStorer(base, 1)

	if _, err := store.SetEncodedObject(testBlob("one")); err != nil {
		t.Fatalf("first object should fit within limit: %v", err)
	}
	err := func() error {
		_, err := store.SetEncodedObject(testBlob("two"))
		return err
	}()
	var limitErr materializedFetchLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("expected materialized fetch limit error, got %v", err)
	}
	if limitErr.limit != 1 {
		t.Fatalf("unexpected limit error payload: %+v", limitErr)
	}
}

func TestEffectiveMaterializedMaxObjects(t *testing.T) {
	if got := effectiveMaterializedMaxObjects(123); got != 123 {
		t.Fatalf("effectiveMaterializedMaxObjects(123) = %d, want 123", got)
	}
	if got := effectiveMaterializedMaxObjects(0); got != DefaultMaterializedMaxObjects {
		t.Fatalf("effectiveMaterializedMaxObjects(0) = %d, want %d", got, DefaultMaterializedMaxObjects)
	}
}

func testBlob(content string) plumbing.EncodedObject {
	obj := &plumbing.MemoryObject{}
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(content)))
	if _, err := obj.Write([]byte(content)); err != nil {
		panic(err)
	}
	return obj
}
