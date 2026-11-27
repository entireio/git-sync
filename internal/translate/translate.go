// Package translate converts git objects between object formats, SHA1 and
// SHA256, as git's object-file-convert.c does.
//
// Only embedded object IDs change: tree entries, a commit's tree, parent and
// mergetag lines, and a tag's object line. Every other byte is kept,
// including both signature headers on a commit (gpgsig signs the SHA1 form
// and gpgsig-sha256 the SHA256 form). A tag keeps the signature over its own
// format in its body and the other in a header, so converting a tag moves
// each signature to the place the other format keeps it. A commit header git
// does not know fails the conversion, because it might embed an object ID.
//
// Converting an object and converting the result back yields the original
// bytes, so the two hashes identify one object, as with git's
// extensions.compatObjectFormat.
package translate

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/go-git/go-git/v6/plumbing"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

// ErrGitlink is returned for a submodule gitlink whose commit has no known
// translation. The commit lives in another repository, so its other-format
// hash cannot be computed here.
var ErrGitlink = errors.New("submodule gitlink has no known translation")

// MapFunc returns the target-format ID of a source-format object. gitlink
// is set for a tree entry naming a submodule commit, which is not in the
// repository being converted.
type MapFunc func(oid plumbing.Hash, gitlink bool) (plumbing.Hash, error)

// Pair is a source object and the object it was translated into.
type Pair struct {
	Source plumbing.Hash
	Target plumbing.Hash
}

// Translator translates objects read from src into dst. It is not safe for
// concurrent use.
type Translator struct {
	src      storer.EncodedObjectStorer
	dst      storer.EncodedObjectStorer
	from, to formatcfg.ObjectFormat

	// known maps every source hash translated so far, plus the seed, to its
	// target hash.
	known map[plumbing.Hash]plumbing.Hash
	// commits lists the commits this translator encoded, in the order they
	// were encoded. Seeded commits are not in it.
	commits []Pair
	// seed is the translations New was given.
	seed map[plumbing.Hash]plumbing.Hash
	// seedsUsed lists the seeded objects a translated object references,
	// gitlinks aside, each once.
	seedsUsed []Pair
	usedSeed  map[plumbing.Hash]struct{}
}

// New returns a Translator from src, holding objects in format from, into
// dst, holding objects in format to. seed holds translations known in
// advance: commits recorded by an earlier conversion, or submodule commits.
// Translation stops at a seeded hash and uses its target hash without
// reading the object.
func New(src, dst storer.EncodedObjectStorer, from, to formatcfg.ObjectFormat, seed map[plumbing.Hash]plumbing.Hash) *Translator {
	known := make(map[plumbing.Hash]plumbing.Hash, len(seed))
	for k, v := range seed {
		known[k] = v
	}
	return &Translator{src: src, dst: dst, from: from, to: to, known: known, seed: seed, usedSeed: make(map[plumbing.Hash]struct{})}
}

// Lookup returns the translation of h if it is seeded or already translated.
func (t *Translator) Lookup(h plumbing.Hash) (plumbing.Hash, bool) {
	out, ok := t.known[h]
	return out, ok
}

// Commits returns the commits this translator encoded, excluding seeded ones.
func (t *Translator) Commits() []Pair {
	return append([]Pair(nil), t.commits...)
}

// SeedsUsed returns the seeded objects that translated objects reference,
// or that Translate was asked for. The translated objects are complete only
// where these exist. Submodule commits a gitlink names are left out, since
// they live in another repository.
func (t *Translator) SeedsUsed() []Pair {
	return append([]Pair(nil), t.seedsUsed...)
}

// Translate translates h and everything it references, returning the hash of
// the translated object in dst.
func (t *Translator) Translate(h plumbing.Hash) (plumbing.Hash, error) {
	if out, ok := t.known[h]; ok {
		if _, seeded := t.seed[h]; seeded {
			if _, seen := t.usedSeed[h]; !seen {
				t.usedSeed[h] = struct{}{}
				t.seedsUsed = append(t.seedsUsed, Pair{Source: h, Target: out})
			}
		}
		return out, nil
	}
	obj, err := t.src.EncodedObject(plumbing.AnyObject, h)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("load %s: %w", h, err)
	}
	raw, err := readAll(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read %s: %w", h, err)
	}
	content, err := ConvertObject(obj.Type(), raw, t.from, t.to, t.mapOID)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("%s %s: %w", obj.Type(), h, err)
	}
	out, err := writeObject(t.dst, obj.Type(), content)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store translation of %s: %w", h, err)
	}
	t.known[h] = out
	if obj.Type() == plumbing.CommitObject {
		t.commits = append(t.commits, Pair{Source: h, Target: out})
	}
	return out, nil
}

func (t *Translator) mapOID(oid plumbing.Hash, gitlink bool) (plumbing.Hash, error) {
	if !gitlink {
		return t.Translate(oid)
	}
	if out, ok := t.known[oid]; ok {
		return out, nil
	}
	return plumbing.ZeroHash, fmt.Errorf("%s: %w", oid, ErrGitlink)
}

// ConvertObject converts the content of one object from format from to
// format to, mapping each embedded object ID with mapOID. Blobs embed no
// IDs and are returned unchanged.
func ConvertObject(typ plumbing.ObjectType, raw []byte, from, to formatcfg.ObjectFormat, mapOID MapFunc) ([]byte, error) {
	switch typ {
	case plumbing.BlobObject:
		return raw, nil
	case plumbing.TreeObject:
		return ConvertTree(raw, from, to, mapOID)
	case plumbing.CommitObject:
		return ConvertCommit(raw, from, to, mapOID)
	case plumbing.TagObject:
		return ConvertTag(raw, from, to, mapOID)
	case plumbing.InvalidObject, plumbing.OFSDeltaObject, plumbing.REFDeltaObject, plumbing.AnyObject:
	}
	return nil, fmt.Errorf("cannot convert object of type %s", typ)
}

// ConvertTree is convert_tree_object: each entry is "<mode> <path>\0" and
// the raw object ID, whose width changes with the format. The target width
// is that of the IDs mapOID returns.
func ConvertTree(raw []byte, from, _ formatcfg.ObjectFormat, mapOID MapFunc) ([]byte, error) {
	fromSize := from.Size()
	out := make([]byte, 0, len(raw)+len(raw)/4)
	for p := raw; len(p) > 0; {
		mode, pathEnd, err := decodeTreeEntry(p, fromSize)
		if err != nil {
			return nil, err
		}
		oid, ok := plumbing.FromBytes(p[pathEnd : pathEnd+fromSize])
		if !ok {
			return nil, errors.New("failed to decode tree entry")
		}
		mapped, err := mapOID(oid, mode == 0o160000)
		if err != nil {
			return nil, fmt.Errorf("failed to map tree entry for %s: %w", oid, err)
		}
		out = append(out, p[:pathEnd]...)
		out = append(out, mapped.Bytes()...)
		p = p[pathEnd+fromSize:]
	}
	return out, nil
}

// decodeTreeEntry is decode_tree_entry_raw: it validates one entry and
// returns its mode and the offset just past the path's NUL terminator.
func decodeTreeEntry(p []byte, hashSize int) (uint32, int, error) {
	if len(p) < hashSize+3 || p[len(p)-(hashSize+1)] != 0 {
		return 0, 0, errors.New("failed to decode tree entry")
	}
	var mode uint32
	i := 0
	for ; i < len(p) && p[i] != ' '; i++ {
		c := p[i]
		if c < '0' || c > '7' {
			return 0, 0, errors.New("failed to decode tree entry: malformed mode")
		}
		mode = mode<<3 + uint32(c-'0')
	}
	path := i + 1
	if i == len(p) {
		return 0, 0, errors.New("failed to decode tree entry")
	}
	nul := bytes.IndexByte(p[path:], 0)
	if nul <= 0 || len(p) < path+nul+1+hashSize {
		return 0, 0, errors.New("failed to decode tree entry")
	}
	return mode, path + nul + 1, nil
}

// ConvertCommit is convert_commit_object.
func ConvertCommit(raw []byte, from, to formatcfg.ObjectFormat, mapOID MapFunc) ([]byte, error) {
	out := make([]byte, 0, len(raw)+len(raw)/8)
	hexSize := from.HexSize()
	p := raw
	for len(p) > 0 && p[0] != '\n' {
		eol := bytes.IndexByte(p, '\n')
		if eol < 0 {
			return nil, errors.New("bad line in commit")
		}
		line := p[:eol]
		switch {
		case len(line) > 5 && bytes.HasPrefix(line, []byte("tree ")):
			mapped, err := mapHexLine(line, "tree ", hexSize, mapOID)
			if err != nil {
				return nil, err
			}
			out = append(out, "tree "+mapped.String()+"\n"...)
		case len(line) > 7 && bytes.HasPrefix(line, []byte("parent ")):
			mapped, err := mapHexLine(line, "parent ", hexSize, mapOID)
			if err != nil {
				return nil, err
			}
			out = append(out, "parent "+mapped.String()+"\n"...)
		case len(line) > 9 && bytes.HasPrefix(line, []byte("mergetag ")):
			tag := append([]byte(nil), p[len("mergetag "):eol+1]...)
			p = p[eol+1:]
			for len(p) > 0 && p[0] == ' ' {
				next := bytes.IndexByte(p, '\n')
				if next < 0 {
					return nil, errors.New("bad mergetag continuation in commit")
				}
				tag = append(tag, p[1:next+1]...)
				p = p[next+1:]
			}
			converted, err := ConvertTag(tag, from, to, mapOID)
			if err != nil {
				return nil, fmt.Errorf("mergetag: %w", err)
			}
			out = append(out, "mergetag"...)
			out = addLines(out, " ", converted)
			continue
		case bytes.HasPrefix(p, []byte("author ")),
			bytes.HasPrefix(p, []byte("committer ")),
			bytes.HasPrefix(p, []byte("encoding ")),
			bytes.HasPrefix(p, []byte("gpgsig")):
			out = append(out, p[:eol+1]...)
		default:
			key, _, _ := bytes.Cut(line, []byte{' '})
			return nil, fmt.Errorf("unknown commit header %q might embed an object ID", key)
		}
		p = p[eol+1:]
		for len(p) > 0 && p[0] == ' ' {
			next := bytes.IndexByte(p, '\n')
			if next < 0 {
				return nil, errors.New("bad continuation in commit")
			}
			out = append(out, p[:next+1]...)
			p = p[next+1:]
		}
	}
	return append(out, p...), nil
}

func mapHexLine(line []byte, prefix string, hexSize int, mapOID MapFunc) (plumbing.Hash, error) {
	hex := line[len(prefix):]
	name := prefix[:len(prefix)-1]
	if len(hex) != hexSize {
		return plumbing.ZeroHash, fmt.Errorf("bad %s in commit", name)
	}
	oid, ok := plumbing.FromHex(string(hex))
	if !ok {
		return plumbing.ZeroHash, fmt.Errorf("bad %s in commit", name)
	}
	mapped, err := mapOID(oid, false)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("unable to map %s %s in commit object: %w", name, oid, err)
	}
	return mapped, nil
}

// TagObject returns the object a tag in format f points at. Like
// convert_tag_object, it reads only the leading object line and requires
// nothing else of the tag.
func TagObject(raw []byte, f formatcfg.ObjectFormat) (plumbing.Hash, error) {
	entryLen := f.HexSize() + len("object ")
	if entryLen >= len(raw) || !bytes.HasPrefix(raw, []byte("object ")) || raw[entryLen] != '\n' {
		return plumbing.ZeroHash, errors.New("bogus tag object")
	}
	oid, ok := plumbing.FromHex(string(raw[len("object "):entryLen]))
	if !ok {
		return plumbing.ZeroHash, errors.New("bad tag object ID")
	}
	return oid, nil
}

// ConvertTag is convert_tag_object. The body may end in a signature over
// the from format, and the header may carry one over the to format. The
// first becomes a header in the output, and the second becomes its body
// signature.
func ConvertTag(raw []byte, from, to formatcfg.ObjectFormat, mapOID MapFunc) ([]byte, error) {
	oid, err := TagObject(raw, from)
	if err != nil {
		return nil, err
	}
	mapped, err := mapOID(oid, false)
	if err != nil {
		return nil, fmt.Errorf("unable to map %s in tag object: %w", oid, err)
	}
	rest := raw[len("object ")+from.HexSize()+1:]

	payloadSize := signatureStart(rest)
	ourSig := rest[payloadSize:]
	payload, otherSig := splitHeaderSignature(rest[:payloadSize], to)

	out := make([]byte, 0, len(raw)+len(raw)/8+16)
	out = append(out, "object "+mapped.String()+"\n"...)
	out = append(out, payload...)
	if len(ourSig) > 0 {
		out = addHeaderSignature(out, ourSig, from)
	}
	return append(out, otherSig...), nil
}

// signatureHeader is gpg_sig_headers: the header carrying a signature over
// an object's form in format f.
func signatureHeader(f formatcfg.ObjectFormat) string {
	if f == formatcfg.SHA256 {
		return "gpgsig-sha256"
	}
	return "gpgsig"
}

// splitHeaderSignature is parse_buffer_signed_by_header. It returns buf
// without its signature headers, and the signature carried by the header
// for format f with the continuation indent removed.
func splitHeaderSignature(buf []byte, f formatcfg.ObjectFormat) (payload, signature []byte) {
	header := signatureHeader(f)
	inSignature, otherSignature := false, false
	for line := buf; len(line) > 0; {
		next := len(line)
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			next = i + 1
		}
		var sig []byte
		isSig := false
		switch {
		case inSignature && line[0] == ' ':
			sig, isSig = line[1:next], true
		case bytes.HasPrefix(line, []byte(header)) && len(line) > len(header) && line[len(header)] == ' ':
			sig, isSig = line[len(header)+1:next], true
			otherSignature = false
		case bytes.HasPrefix(line, []byte("gpgsig")):
			otherSignature = true
		case otherSignature && line[0] != ' ':
			otherSignature = false
		}
		if isSig {
			signature = append(signature, sig...)
			inSignature = true
		} else {
			if line[0] == '\n' {
				next = len(line)
			}
			if !otherSignature {
				payload = append(payload, line[:next]...)
			}
			inSignature = false
		}
		line = line[next:]
	}
	return payload, signature
}

// addHeaderSignature is add_header_signature: it inserts sig as a header
// for format f at the end of buf's header. Git treats both as C strings, so
// sig ends at its first NUL, and the end of the header is searched for only
// before buf's first NUL.
func addHeaderSignature(buf, sig []byte, f formatcfg.ObjectFormat) []byte {
	sig, _, _ = bytes.Cut(sig, []byte{0})
	if len(sig) == 0 {
		return buf
	}
	header, _, _ := bytes.Cut(buf, []byte{0})
	pos := len(buf)
	if eoh := bytes.Index(header, []byte("\n\n")); eoh >= 0 {
		pos = eoh + 1
	}
	lines := []byte(signatureHeader(f))
	for _, line := range bytes.SplitAfter(sig, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		lines = append(lines, ' ')
		lines = append(lines, line...)
	}
	out := make([]byte, 0, len(buf)+len(lines))
	out = append(out, buf[:pos]...)
	out = append(out, lines...)
	return append(out, buf[pos:]...)
}

// addLines is strbuf_add_lines: it appends each line of buf after prefix
// and ends the result with a newline.
func addLines(out []byte, prefix string, buf []byte) []byte {
	for len(buf) > 0 {
		next := len(buf)
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			next = i + 1
		}
		out = append(out, prefix...)
		out = append(out, buf[:next]...)
		buf = buf[next:]
	}
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return out
}

var signatureMarkers = [][]byte{
	[]byte("-----BEGIN PGP SIGNATURE-----"),
	[]byte("-----BEGIN PGP MESSAGE-----"),
	[]byte("-----BEGIN SIGNED MESSAGE-----"),
	[]byte("-----BEGIN SSH SIGNATURE-----"),
}

// SignatureStart is parse_signed_buffer: the offset of the last line in buf
// that opens a signature, or len(buf) when there is none. A tag's body
// signature starts there.
func SignatureStart(buf []byte) int { return signatureStart(buf) }

func signatureStart(buf []byte) int {
	match := len(buf)
	for off := 0; off < len(buf); {
		line := buf[off:]
		for _, m := range signatureMarkers {
			if bytes.HasPrefix(line, m) {
				match = off
				break
			}
		}
		nl := bytes.IndexByte(line, '\n')
		if nl < 0 {
			break
		}
		off += nl + 1
	}
	return match
}

func writeObject(dst storer.EncodedObjectStorer, typ plumbing.ObjectType, content []byte) (plumbing.Hash, error) {
	obj := dst.NewEncodedObject()
	obj.SetType(typ)
	obj.SetSize(int64(len(content)))
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("open writer: %w", err)
	}
	if _, err := w.Write(content); err != nil {
		_ = w.Close()
		return plumbing.ZeroHash, fmt.Errorf("write: %w", err)
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("close writer: %w", err)
	}
	h, err := dst.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("set object: %w", err)
	}
	return h, nil
}

func readAll(obj plumbing.EncodedObject) ([]byte, error) {
	r, err := obj.Reader()
	if err != nil {
		return nil, fmt.Errorf("open reader: %w", err)
	}
	b, err := io.ReadAll(r)
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	return b, err
}
