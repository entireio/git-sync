package gitproto

import (
	"bytes"
	"container/list"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

// DefaultCommitParentsCacheBytes is the default sliding-cache size for
// ExtractCommitParents. Sized to comfortably cover delta locality for
// tree:0-filtered packs generated with git's default window/depth
// (typically tens of objects of working set).
const DefaultCommitParentsCacheBytes = 64 * 1024 * 1024

// ExtractCommitParents reads a Git packfile from r and returns a map
// of commit hash → parent hashes for every commit object encountered.
// Non-commit objects (tags, etc.) and the raw bytes of commits are
// discarded after parent extraction, so peak memory is dominated by
// the result map plus a small bounded delta-resolution cache.
//
// Intended for tree:0-filtered packs where the only object kinds are
// commits and (occasionally) annotated tags. Input is the raw packfile
// stream (e.g. demuxed sideband from a v2 fetch response).
func ExtractCommitParents(r io.Reader) (map[plumbing.Hash][]plumbing.Hash, error) {
	return ExtractCommitParentsWithCache(r, DefaultCommitParentsCacheBytes)
}

// ExtractCommitParentsWithCache is like ExtractCommitParents but lets
// the caller cap the delta-resolution cache size. On cache miss the
// underlying pack parser returns ErrObjectNotFound; raise the limit
// and retry, or fall back to the in-memory store path.
func ExtractCommitParentsWithCache(r io.Reader, maxCacheBytes int) (map[plumbing.Hash][]plumbing.Hash, error) {
	if maxCacheBytes <= 0 {
		maxCacheBytes = DefaultCommitParentsCacheBytes
	}

	// go-git's pack parser forces lowMemoryMode = false when the input
	// is not seekable (parser.go:83-85), which causes the scanner to
	// buffer every inflated object in memory until Parse returns
	// (scanner.go:427) — undoing the savings the custom storer aims
	// for. We spill the pack to a temp file so the parser sees a
	// seekable source, enables low-memory mode, and releases object
	// content as it flows through the storer. Already-seekable inputs
	// (e.g. tests using bytes.Reader) skip the spill.
	seekable, ok := r.(io.ReadSeeker)
	if !ok {
		tmp, err := os.CreateTemp("", "gitsync-cgparse-*.pack")
		if err != nil {
			return nil, fmt.Errorf("create temp pack: %w", err)
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()

		if _, err := io.Copy(tmp, r); err != nil {
			return nil, fmt.Errorf("spill pack to temp: %w", err)
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek temp pack: %w", err)
		}
		seekable = tmp
	}

	store := newCommitParentsStorer(maxCacheBytes)
	p := packfile.NewParser(seekable, packfile.WithStorage(store))
	if _, err := p.Parse(); err != nil {
		return nil, fmt.Errorf("parse packfile: %w", err)
	}
	return store.parents, nil
}

// commitParentsStorer is a minimal storer.EncodedObjectStorer that
// captures commit parent metadata as objects flow through the parser,
// and serves recent objects from a bounded LRU cache when the parser
// needs to resolve a delta base.
//
// It implements packfile.LowMemoryCapable to tell the parser it can
// release its inflated-content buffers as soon as objects are handed
// off here — we only retain a small working set in the cache.
type commitParentsStorer struct {
	parents map[plumbing.Hash][]plumbing.Hash
	cache   *deltaCache
	hasher  *plumbing.ObjectHasher
}

func newCommitParentsStorer(maxBytes int) *commitParentsStorer {
	return &commitParentsStorer{
		parents: make(map[plumbing.Hash][]plumbing.Hash),
		cache:   newDeltaCache(maxBytes),
		hasher:  plumbing.FromObjectFormat(""),
	}
}

// LowMemoryMode tells the parser we don't need it to keep object
// content in its internal cache; we handle delta-base retrieval via
// EncodedObject below.
func (s *commitParentsStorer) LowMemoryMode() bool { return true }

// RawObjectWriter returns a sink for one inflated object. The writer's
// Close captures commit parents and caches the bytes for potential
// future delta lookups.
func (s *commitParentsStorer) RawObjectWriter(typ plumbing.ObjectType, sz int64) (io.WriteCloser, error) {
	return &commitParentsWriter{
		s:   s,
		typ: typ,
		buf: bytes.NewBuffer(make([]byte, 0, sz)),
	}, nil
}

type commitParentsWriter struct {
	s   *commitParentsStorer
	typ plumbing.ObjectType
	buf *bytes.Buffer
}

func (w *commitParentsWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if err != nil {
		return n, fmt.Errorf("buffer object bytes: %w", err)
	}
	return n, nil
}

func (w *commitParentsWriter) Close() error {
	content := w.buf.Bytes()
	hash, err := w.s.hasher.Compute(w.typ, content)
	if err != nil {
		return fmt.Errorf("hash %s object (%d bytes): %w", w.typ, len(content), err)
	}
	if w.typ == plumbing.CommitObject {
		w.s.parents[hash] = parseCommitParents(content)
	}
	// Detach the byte slice from the buffer so the buffer can be GC'd
	// independently. The cache may keep this entry until evicted.
	w.s.cache.put(hash, w.typ, content)
	return nil
}

// EncodedObject is queried by the parser when resolving deltas in
// low-memory mode. Serves from the bounded LRU cache. The interface
// contract calls for matching both hash *and* type, except when the
// caller passes AnyObject as a wildcard.
func (s *commitParentsStorer) EncodedObject(typ plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) {
	entry, ok := s.cache.get(hash)
	if !ok {
		return nil, plumbing.ErrObjectNotFound
	}
	if typ != plumbing.AnyObject && entry.typ != typ {
		return nil, plumbing.ErrObjectNotFound
	}
	obj := plumbing.NewMemoryObject(s.hasher)
	obj.SetType(entry.typ)
	obj.SetSize(int64(len(entry.content)))
	w, err := obj.Writer()
	if err != nil {
		return nil, fmt.Errorf("memory-object writer: %w", err)
	}
	if _, err := w.Write(entry.content); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("memory-object write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("memory-object close: %w", err)
	}
	return obj, nil
}

// --- Unused interface methods ------------------------------------------------
//
// EncodedObjectStorer requires more than the two methods the parser
// actually exercises in low-memory mode. We stub the rest with explicit
// errors so any future caller that depends on them fails loudly rather
// than getting a misleading empty result.

func (s *commitParentsStorer) NewEncodedObject() plumbing.EncodedObject {
	return plumbing.NewMemoryObject(s.hasher)
}

func (s *commitParentsStorer) SetEncodedObject(plumbing.EncodedObject) (plumbing.Hash, error) {
	return plumbing.ZeroHash, errors.New("commitParentsStorer: SetEncodedObject not supported")
}

func (s *commitParentsStorer) IterEncodedObjects(plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	return nil, errors.New("commitParentsStorer: IterEncodedObjects not supported")
}

func (s *commitParentsStorer) HasEncodedObject(plumbing.Hash) error {
	return errors.New("commitParentsStorer: HasEncodedObject not supported")
}

func (s *commitParentsStorer) EncodedObjectSize(plumbing.Hash) (int64, error) {
	return 0, errors.New("commitParentsStorer: EncodedObjectSize not supported")
}

func (s *commitParentsStorer) AddAlternate(string) error {
	return errors.New("commitParentsStorer: AddAlternate not supported")
}

// --- Delta cache -------------------------------------------------------------

type deltaCacheEntry struct {
	hash    plumbing.Hash
	typ     plumbing.ObjectType
	content []byte
	elem    *list.Element
}

type deltaCache struct {
	maxBytes int
	curBytes int
	byHash   map[plumbing.Hash]*deltaCacheEntry
	lru      *list.List // front = most recently used
}

func newDeltaCache(maxBytes int) *deltaCache {
	return &deltaCache{
		maxBytes: maxBytes,
		byHash:   make(map[plumbing.Hash]*deltaCacheEntry),
		lru:      list.New(),
	}
}

func (c *deltaCache) put(hash plumbing.Hash, typ plumbing.ObjectType, content []byte) {
	if existing, ok := c.byHash[hash]; ok {
		c.curBytes -= len(existing.content)
		c.lru.Remove(existing.elem)
		delete(c.byHash, hash)
	}
	entry := &deltaCacheEntry{hash: hash, typ: typ, content: content}
	entry.elem = c.lru.PushFront(entry)
	c.byHash[hash] = entry
	c.curBytes += len(content)
	for c.curBytes > c.maxBytes {
		back := c.lru.Back()
		if back == nil {
			return
		}
		oldest, ok := back.Value.(*deltaCacheEntry)
		if !ok {
			// container/list values are interface{}; only put() ever
			// inserts here and only with *deltaCacheEntry, so a
			// mismatch would indicate corruption. Bail rather than
			// loop forever (curBytes would never decrease).
			panic("deltaCache: corrupt LRU entry")
		}
		c.curBytes -= len(oldest.content)
		c.lru.Remove(back)
		delete(c.byHash, oldest.hash)
	}
}

func (c *deltaCache) get(hash plumbing.Hash) (*deltaCacheEntry, bool) {
	entry, ok := c.byHash[hash]
	if !ok {
		return nil, false
	}
	c.lru.MoveToFront(entry.elem)
	return entry, true
}

// --- Commit-header parsing ---------------------------------------------------

// parseCommitParents extracts parent hashes from the canonical
// position of a commit object: immediately after the single "tree"
// header, in an uninterrupted run, before any other header. Anything
// outside that run — "parent" lines that appear before "tree", after
// the first non-parent header, or in malformed shape — is ignored.
//
// This mirrors git's own parser: a malformed commit can claim extra
// "parent" lines outside the canonical position, but git treats only
// the canonical run as real parents. Matching git here keeps our
// reachability computation consistent with what a canonical reader
// will see in the same bytes.
//
// Returns nil if the first header isn't "tree <40-hex>", or if a
// parent line is malformed (wrong length / non-hex hash). In both
// cases the planner sees an empty parent set and stops walking
// rather than guessing.
func parseCommitParents(content []byte) []plumbing.Hash {
	const (
		treePrefix   = "tree "
		parentPrefix = "parent "
		parentLen    = len(parentPrefix) + 40
	)

	line, rest := nextLine(content)
	if !bytes.HasPrefix(line, []byte(treePrefix)) {
		return nil
	}

	var parents []plumbing.Hash
	for {
		line, rest = nextLine(rest)
		if !bytes.HasPrefix(line, []byte(parentPrefix)) {
			return parents
		}
		if len(line) != parentLen {
			return parents
		}
		h := plumbing.NewHash(string(line[len(parentPrefix):]))
		if h.IsZero() {
			return parents
		}
		parents = append(parents, h)
	}
}

// nextLine splits off the first '\n'-terminated line from content,
// returning the line (without the newline) and the remainder. If
// content has no newline, returns it whole with a nil remainder.
func nextLine(content []byte) (line, rest []byte) {
	nl := bytes.IndexByte(content, '\n')
	if nl < 0 {
		return content, nil
	}
	return content[:nl], content[nl+1:]
}
