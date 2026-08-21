package gitproto

import (
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/go-git/go-git/v6/plumbing"
)

// PartitionRefNames splits refs into those whose names are safe to act on and
// the names that are not.
//
// Ref names arrive from a remote's advertisement, so they are untrusted input,
// and git-sync acts on them in two ways that make a malformed name dangerous:
//
//   - convert-sha256 writes them to disk. go-git resolves a name through the
//     repository filesystem, so "refs/heads/../../config" lands outside
//     refs/heads — clamped at the repository root by go-billy, but still able to
//     overwrite config, HEAD, or packed-refs.
//   - Ref-update commands embed them in the receive-pack request. receive-pack
//     reads a feature list from everything after the first NUL on a command
//     line, so a name containing NUL lets the source inject capabilities into
//     the push git-sync sends to the target.
//
// Validation is plumbing.ReferenceName.Validate, which implements git's
// check_refname_format: it rejects "..", NUL, CR, LF and other control
// characters, DEL, space, "~^:?*[", backslash, "@{", a ".lock" suffix, a
// leading dot, empty path components, and a leading dash on a branch or tag.
// Deferring to it rather than hand-rolling a check keeps git-sync's notion of a
// valid ref identical to git's, so nothing git considers legitimate is skipped.
//
// Skipping rather than failing follows the same model as per-ref push
// rejections under BestEffort: one malformed ref upstream should not stop a
// mirror. Callers report what was skipped via WarnSkippedRefNames.
func PartitionRefNames(refs []*plumbing.Reference) (valid []*plumbing.Reference, skipped []string) {
	valid = refs[:0]
	for _, ref := range refs {
		if err := ref.Name().Validate(); err != nil {
			skipped = append(skipped, ref.Name().String())
			continue
		}
		valid = append(valid, ref)
	}
	return valid, skipped
}

// maxWarnedRefNames bounds how many skipped names are named individually. A
// remote could advertise thousands; the count still reflects all of them.
const maxWarnedRefNames = 10

// WarnSkippedRefNames reports refs dropped by PartitionRefNames. Nil w falls
// back to stderr; an empty list is a no-op.
//
// Names are printed with %q. They are remote-controlled, and this text goes to
// a terminal and into logs, so quoting is what keeps an embedded escape
// sequence from being interpreted rather than shown.
func WarnSkippedRefNames(w io.Writer, label string, skipped []string) {
	if len(skipped) == 0 {
		return
	}
	if w == nil {
		w = os.Stderr
	}
	names := make([]string, len(skipped))
	copy(names, skipped)
	sort.Strings(names)

	shown := names
	if len(shown) > maxWarnedRefNames {
		shown = shown[:maxWarnedRefNames]
	}
	fmt.Fprintf(w, "%s: skipping %d ref(s) with names git would reject:", label, len(names))
	for _, name := range shown {
		fmt.Fprintf(w, " %q", name)
	}
	if len(names) > len(shown) {
		fmt.Fprintf(w, " and %d more", len(names)-len(shown))
	}
	fmt.Fprintln(w)
}
