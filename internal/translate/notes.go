package translate

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

// NoteEntry is one note in a git notes tree: the annotated object and the
// blob holding its note.
type NoteEntry struct {
	Object plumbing.Hash
	Blob   plumbing.Hash
}

// ReadHashNotes reads a notes ref whose notes each hold one object hash, as
// convert-sha256's refs/notes/sha1-origin does. It returns the annotated
// object to noted hash mapping and the tree entries it came from, so a
// caller can extend the notes without reading every blob again.
//
// tip is the commit the notes ref points at, or the tree, which git also
// accepts. Both flat trees and fanout trees (annotated hashes split across
// nested directories) are read.
func ReadHashNotes(store storer.EncodedObjectStorer, tip plumbing.Hash) (map[plumbing.Hash]plumbing.Hash, []NoteEntry, error) {
	obj, err := store.EncodedObject(plumbing.AnyObject, tip)
	if err != nil {
		return nil, nil, fmt.Errorf("load notes %s: %w", tip, err)
	}
	treeHash := tip
	if obj.Type() == plumbing.CommitObject {
		c, err := object.DecodeCommit(store, obj)
		if err != nil {
			return nil, nil, fmt.Errorf("decode notes commit %s: %w", tip, err)
		}
		treeHash = c.TreeHash
	} else if obj.Type() != plumbing.TreeObject {
		return nil, nil, fmt.Errorf("notes %s is a %s, not a commit or tree", tip, obj.Type())
	}
	tree, err := object.GetTree(store, treeHash)
	if err != nil {
		return nil, nil, fmt.Errorf("load notes tree %s: %w", treeHash, err)
	}
	values := make(map[plumbing.Hash]plumbing.Hash)
	var entries []NoteEntry
	if err := readNotesTree(store, tree, "", values, &entries); err != nil {
		return nil, nil, err
	}
	return values, entries, nil
}

func readNotesTree(store storer.EncodedObjectStorer, tree *object.Tree, prefix string, values map[plumbing.Hash]plumbing.Hash, entries *[]NoteEntry) error {
	for _, e := range tree.Entries {
		name := prefix + e.Name
		if e.Mode == filemode.Dir {
			sub, err := object.GetTree(store, e.Hash)
			if err != nil {
				return fmt.Errorf("load notes subtree %s: %w", name, err)
			}
			if err := readNotesTree(store, sub, name, values, entries); err != nil {
				return err
			}
			continue
		}
		annotated, ok := plumbing.FromHex(name)
		if !ok {
			// Not a note: git notes trees may carry other files.
			continue
		}
		blob, err := store.EncodedObject(plumbing.BlobObject, e.Hash)
		if err != nil {
			return fmt.Errorf("load note for %s: %w", name, err)
		}
		content, err := readAll(blob)
		if err != nil {
			return fmt.Errorf("read note for %s: %w", name, err)
		}
		fields := bytes.Fields(content)
		if len(fields) == 0 {
			return fmt.Errorf("note for %s is empty", name)
		}
		noted, ok := plumbing.FromHex(string(fields[0]))
		if !ok {
			return fmt.Errorf("note for %s does not hold an object hash", name)
		}
		values[annotated] = noted
		*entries = append(*entries, NoteEntry{Object: annotated, Blob: e.Hash})
	}
	return nil
}

// WriteHashNotes writes a notes commit holding existing plus one note per
// pair in add, keyed by Pair.Target and holding Pair.Source. The tree is
// flat. parent is the previous notes commit, or zero for the first one. It
// returns the new commit and every object it wrote, which are the objects a
// push of the notes ref must send.
func WriteHashNotes(store storer.EncodedObjectStorer, parent plumbing.Hash, existing []NoteEntry, add []Pair, sig object.Signature, message string) (plumbing.Hash, []plumbing.Hash, error) {
	if len(add) == 0 {
		return plumbing.ZeroHash, nil, errors.New("no notes to add")
	}
	byName := make(map[string]plumbing.Hash, len(existing)+len(add))
	for _, e := range existing {
		byName[e.Object.String()] = e.Blob
	}
	written := make([]plumbing.Hash, 0, len(add)+2)
	for _, p := range add {
		blob, err := writeObject(store, plumbing.BlobObject, []byte(p.Source.String()+"\n"))
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("store note for %s: %w", p.Target, err)
		}
		byName[p.Target.String()] = blob
		written = append(written, blob)
	}

	tree := &object.Tree{Entries: make([]object.TreeEntry, 0, len(byName))}
	for name, blob := range byName {
		tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: blob})
	}
	sort.Slice(tree.Entries, func(i, j int) bool { return tree.Entries[i].Name < tree.Entries[j].Name })
	treeHash, err := encodeObject(store, tree.Encode)
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("store notes tree: %w", err)
	}
	written = append(written, treeHash)

	commit := &object.Commit{Author: sig, Committer: sig, Message: message, TreeHash: treeHash}
	if !parent.IsZero() {
		commit.ParentHashes = []plumbing.Hash{parent}
	}
	commitHash, err := encodeObject(store, commit.Encode)
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("store notes commit: %w", err)
	}
	written = append(written, commitHash)
	return commitHash, written, nil
}

func encodeObject(store storer.EncodedObjectStorer, encode func(plumbing.EncodedObject) error) (plumbing.Hash, error) {
	obj := store.NewEncodedObject()
	if err := encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	h, err := store.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("set object: %w", err)
	}
	return h, nil
}
