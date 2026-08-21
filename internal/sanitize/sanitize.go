// Package sanitize strips control characters out of text that came from a
// remote before it reaches a terminal, a log file, or JSON output.
//
// git-sync relays a fair amount of server-authored text: sideband progress
// ("Enumerating objects: …"), HTTP error bodies, and receive-pack rejection
// reasons. All of it is attacker-controlled if the remote is hostile, and all of
// it lands somewhere that interprets escape sequences. An ESC in a rejection
// reason can clear the line and redraw it, so a rejected push can be made to
// read like a successful one; the same bytes in a log file mislead whoever reads
// it later.
//
// The policy differs between the two entry points, because carriage return is
// both necessary and dangerous depending on context.
//
// Writer, used for streamed sideband progress, keeps '\r': git's in-place
// progress updates are built from it, and the "source:"/"target:" prefix each
// line carries bounds what overwriting a row can achieve.
//
// Text, used for one-shot strings — HTTP error bodies, diagnostic headers, ssh
// "remote:" output, receive-pack rejection reasons — drops it. Those strings are
// not prefixed and have no need to move the cursor, and a bare '\r' is enough to
// rewrite the line on its own: "rejected\rok refs/heads/main" reads as a success
// without any escape sequence involved.
//
// Both keep tab and newline, and drop everything else below 0x20 plus DEL. That
// covers what a terminal acts on, since ANSI CSI and OSC both begin with ESC
// (0x1b). The single-byte C1 introducers are not handled: in a UTF-8 stream they
// are not valid standalone bytes, and terminals in UTF-8 mode do not act on
// them.
package sanitize

import (
	"io"
	"strings"
)

func printable(b byte) bool { return b >= 0x20 && b != 0x7f }

// allowedInText keeps only the whitespace a one-shot message needs.
func allowedInText(b byte) bool {
	switch b {
	case '\t', '\n':
		return true
	}
	return printable(b)
}

// allowedInStream additionally keeps '\r' for in-place progress updates.
func allowedInStream(b byte) bool {
	if b == '\r' {
		return true
	}
	return allowedInText(b)
}

// Text returns s with disallowed control characters removed. Text that has none
// is returned unchanged, without copying.
func Text(s string) string {
	clean := true
	for i := range len(s) {
		if !allowedInText(s[i]) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		if allowedInText(s[i]) {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// Writer wraps w so that control characters are filtered as bytes stream
// through, for output that arrives in chunks rather than as one string. Reports
// the number of bytes it was given, not the smaller number written, so callers
// that check n against len(p) do not see a short write.
func Writer(w io.Writer) io.Writer { return &filteringWriter{w: w} }

type filteringWriter struct {
	w   io.Writer
	buf []byte
}

func (f *filteringWriter) Write(p []byte) (int, error) {
	f.buf = f.buf[:0]
	if cap(f.buf) < len(p) {
		f.buf = make([]byte, 0, len(p))
	}
	for _, b := range p {
		if allowedInStream(b) {
			f.buf = append(f.buf, b)
		}
	}
	if len(f.buf) == 0 {
		return len(p), nil
	}
	if _, err := f.w.Write(f.buf); err != nil {
		return 0, err //nolint:wrapcheck // io.Writer wrapper forwards the underlying error verbatim
	}
	return len(p), nil
}
