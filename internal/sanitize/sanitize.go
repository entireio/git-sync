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
// The filter keeps tab, newline and carriage return, because git's own progress
// output relies on them — a '\r'-terminated line is how in-place progress
// updates work. Everything else below 0x20, plus DEL, is dropped. That covers
// the sequences a terminal acts on, since both ANSI CSI and OSC begin with ESC
// (0x1b). The single-byte C1 introducers are not handled: in a UTF-8 stream they
// are not valid standalone bytes, and terminals in UTF-8 mode do not act on
// them.
package sanitize

import (
	"io"
	"strings"
)

func allowed(b byte) bool {
	switch b {
	case '\t', '\n', '\r':
		return true
	}
	return b >= 0x20 && b != 0x7f
}

// Text returns s with disallowed control characters removed. Text that has none
// is returned unchanged, without copying.
func Text(s string) string {
	clean := true
	for i := range len(s) {
		if !allowed(s[i]) {
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
		if allowed(s[i]) {
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
		if allowed(b) {
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
