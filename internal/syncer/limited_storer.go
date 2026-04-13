package syncer

import (
	"fmt"
	"io"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

type materializedFetchLimitError struct {
	count int
	limit int
}

func (e materializedFetchLimitError) Error() string {
	return fmt.Sprintf(
		"materialized fetch exceeded object limit after %d objects (limit %d); narrow refs, use an empty target/bootstrap, or raise --materialized-max-objects",
		e.count,
		e.limit,
	)
}

type limitedStorer struct {
	base  storer.Storer
	limit int
	count int
}

func newLimitedStorer(base storer.Storer, limit int) *limitedStorer {
	return &limitedStorer{base: base, limit: limit}
}

func effectiveMaterializedMaxObjects(limit int) int {
	if limit > 0 {
		return limit
	}
	return DefaultMaterializedMaxObjects
}

func (s *limitedStorer) RawObjectWriter(typ plumbing.ObjectType, sz int64) (io.WriteCloser, error) {
	w, err := s.base.RawObjectWriter(typ, sz)
	if err != nil {
		return nil, err
	}
	return &countingWriteCloser{parent: s, inner: w}, nil
}

func (s *limitedStorer) NewEncodedObject() plumbing.EncodedObject {
	return s.base.NewEncodedObject()
}

func (s *limitedStorer) SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	if err := s.bump(); err != nil {
		return plumbing.ZeroHash, err
	}
	return s.base.SetEncodedObject(obj)
}

func (s *limitedStorer) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	return s.base.EncodedObject(t, h)
}

func (s *limitedStorer) IterEncodedObjects(t plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	return s.base.IterEncodedObjects(t)
}

func (s *limitedStorer) HasEncodedObject(h plumbing.Hash) error {
	return s.base.HasEncodedObject(h)
}

func (s *limitedStorer) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	return s.base.EncodedObjectSize(h)
}

func (s *limitedStorer) AddAlternate(remote string) error {
	return s.base.AddAlternate(remote)
}

func (s *limitedStorer) SetReference(ref *plumbing.Reference) error {
	return s.base.SetReference(ref)
}

func (s *limitedStorer) CheckAndSetReference(newRef, old *plumbing.Reference) error {
	return s.base.CheckAndSetReference(newRef, old)
}

func (s *limitedStorer) Reference(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	return s.base.Reference(name)
}

func (s *limitedStorer) IterReferences() (storer.ReferenceIter, error) {
	return s.base.IterReferences()
}

func (s *limitedStorer) RemoveReference(name plumbing.ReferenceName) error {
	return s.base.RemoveReference(name)
}

func (s *limitedStorer) CountLooseRefs() (int, error) {
	return s.base.CountLooseRefs()
}

func (s *limitedStorer) PackRefs() error {
	return s.base.PackRefs()
}

func (s *limitedStorer) bump() error {
	s.count++
	if s.count > s.limit {
		return materializedFetchLimitError{count: s.count, limit: s.limit}
	}
	return nil
}

type countingWriteCloser struct {
	parent *limitedStorer
	inner  io.WriteCloser
}

func (w *countingWriteCloser) Write(p []byte) (int, error) {
	return w.inner.Write(p)
}

func (w *countingWriteCloser) Close() error {
	if err := w.parent.bump(); err != nil {
		_ = w.inner.Close()
		return err
	}
	return w.inner.Close()
}
