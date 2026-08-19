package syncer

import (
	"errors"
	"fmt"
	"io"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage"
)

// ErrObjectLimit is reported when a fetch into the in-memory store exceeds the
// materialized object limit. Test for it with errors.Is.
var ErrObjectLimit = errors.New("materialized object limit exceeded")

// ObjectLimitError carries how many objects were admitted before the limit was
// reached. Reachable with errors.As.
type ObjectLimitError struct {
	Limit int
}

func (e *ObjectLimitError) Error() string {
	return fmt.Sprintf("fetch exceeded the materialized object limit of %d objects; "+
		"raise --materialized-max-objects, or use bootstrap for large initial syncs", e.Limit)
}

func (e *ObjectLimitError) Is(target error) bool { return target == ErrObjectLimit }

// boundedStorer admits at most limit objects into the wrapped store, failing the
// write that would exceed it.
//
// The materialized path already had an object limit, but it was checked on the
// object closure *after* the fetch had finished — by which point the objects
// were resident and the process may already have died, so the guard reported an
// overrun it was meant to prevent. Counting here makes the limit bite while the
// pack is still streaming.
//
// RawObjectWriter is the choke point: go-git's pack scanner routes every object
// it decodes through it (parser.go for deltas, scanner.go for everything else),
// and memory storage's implementation only reaches SetEncodedObject via its own
// internal closer, which never passes back through this wrapper. Counting in
// both places would therefore risk double-counting rather than adding coverage.
//
// Reads are untouched, so planning and the push path see the store as they
// always did.
type boundedStorer struct {
	storage.Storer

	limit int
	count int
}

// newBoundedStorer wraps s so that no more than limit objects can be written to
// it. A limit of zero or less means unlimited, and s is returned unchanged so
// nothing pays for the wrapper.
func newBoundedStorer(s storage.Storer, limit int) storage.Storer {
	if limit <= 0 {
		return s
	}
	return &boundedStorer{Storer: s, limit: limit}
}

func (s *boundedStorer) RawObjectWriter(typ plumbing.ObjectType, sz int64) (io.WriteCloser, error) {
	if s.count >= s.limit {
		return nil, &ObjectLimitError{Limit: s.limit}
	}
	s.count++
	w, err := s.Storer.RawObjectWriter(typ, sz)
	if err != nil {
		return nil, fmt.Errorf("raw object writer: %w", err)
	}
	return w, nil
}
