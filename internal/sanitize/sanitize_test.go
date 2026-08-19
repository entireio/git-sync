package sanitize

import (
	"bytes"
	"strings"
	"testing"
)

func TestTextStripsTerminalControlSequences(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text untouched", "Enumerating objects: 42, done.", "Enumerating objects: 42, done."},
		{"tab and newline survive", "a\tb\nc", "a\tb\nc"},
		// CR is dropped here but kept by Writer: a one-shot message has no need
		// to move the cursor, and a bare CR rewrites the line on its own.
		{"CR dropped", "rejected\rok refs/heads/main", "rejectedok refs/heads/main"},
		{"CRLF becomes LF", "line one\r\nline two", "line one\nline two"},
		{"escape removed", "before\x1b[2Kafter", "before[2Kafter"},
		{"OSC introducer removed", "x\x1b]0;titley", "x]0;titley"},
		{"NUL removed", "a\x00b", "ab"},
		{"bell removed", "ding\x07", "ding"},
		{"backspace removed", "abc\x08", "abc"},
		{"DEL removed", "abc\x7f", "abc"},
		{"vertical tab and form feed removed", "a\x0bb\x0cc", "abc"},
		{"empty input", "", ""},
		{"only control characters", "\x1b\x00\x07", ""},
		// UTF-8 must survive intact: continuation bytes are all >= 0x80, so a
		// byte-oriented filter must not touch them.
		{"multibyte utf-8 preserved", "héllo — 世界", "héllo — 世界"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Text(tt.in); got != tt.want {
				t.Errorf("Text(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// The spoofing case the filter exists for: a rejection reason that clears the
// line and rewrites it to look like success. Stripping ESC alone is not enough,
// because a bare CR repositions the cursor to the start of the line by itself.
func TestTextDefeatsLineRewriteSpoofing(t *testing.T) {
	hostile := "rejected\x1b[2K\rok refs/heads/main"
	got := Text(hostile)
	if strings.ContainsAny(got, "\x1b\r") {
		t.Errorf("cursor-moving characters survived: %q", got)
	}
	if !strings.HasPrefix(got, "rejected") {
		t.Errorf("the real message must still lead: %q", got)
	}
}

// Writer keeps CR because git's in-place progress depends on it, and the line
// prefix bounds what overwriting a row can achieve. This is the deliberate
// asymmetry with Text.
func TestWriterKeepsCarriageReturnForProgress(t *testing.T) {
	var buf bytes.Buffer
	w := Writer(&buf)
	if _, err := w.Write([]byte("Resolving deltas:  50%\r")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, want := buf.String(), "Resolving deltas:  50%\r"; got != want {
		t.Errorf("Writer dropped the progress CR: got %q, want %q", got, want)
	}
}

func TestWriterFiltersStreamedChunks(t *testing.T) {
	var buf bytes.Buffer
	w := Writer(&buf)

	// Split so an escape sequence straddles two writes, the way sideband
	// frames arrive.
	chunks := []string{"Resolving deltas:  10%\r", "\x1b[2K", "Resolving deltas: 100%\n"}
	for _, c := range chunks {
		n, err := w.Write([]byte(c))
		if err != nil {
			t.Fatalf("write %q: %v", c, err)
		}
		// A short write would make callers like io.Copy report an error even
		// though the filtering is intentional.
		if n != len(c) {
			t.Errorf("Write(%q) = %d, want %d", c, n, len(c))
		}
	}
	got := buf.String()
	if strings.Contains(got, "\x1b") {
		t.Errorf("escape survived streaming: %q", got)
	}
	want := "Resolving deltas:  10%\r[2KResolving deltas: 100%\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWriterAllControlCharactersIsNotAShortWrite(t *testing.T) {
	var buf bytes.Buffer
	w := Writer(&buf)
	in := []byte("\x1b\x00\x07")
	n, err := w.Write(in)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(in) {
		t.Errorf("Write() = %d, want %d", n, len(in))
	}
	if buf.Len() != 0 {
		t.Errorf("expected nothing written, got %q", buf.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

var errWrite = &writeError{}

type writeError struct{}

func (*writeError) Error() string { return "write failed" }

func TestWriterForwardsUnderlyingError(t *testing.T) {
	w := Writer(failingWriter{})
	if _, err := w.Write([]byte("text")); err == nil {
		t.Error("expected the underlying write error to surface")
	}
}
