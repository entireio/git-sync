package gitproto

import (
	"bytes"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

func refs(names ...string) []*plumbing.Reference {
	out := make([]*plumbing.Reference, 0, len(names))
	h := plumbing.NewHash("0123456789012345678901234567890123456789")
	for _, n := range names {
		out = append(out, plumbing.NewHashReference(plumbing.ReferenceName(n), h))
	}
	return out
}

func TestPartitionRefNamesKeepsLegitimateNames(t *testing.T) {
	in := refs(
		"refs/heads/main",
		"refs/heads/feature/nested-1.2",
		"refs/tags/v1.0.0",
		"refs/notes/commits",
		"refs/pull/42/head",
		"refs/merge-requests/7/head",
	)
	valid, skipped := PartitionRefNames(in)
	if len(skipped) != 0 {
		t.Fatalf("skipped legitimate names: %q", skipped)
	}
	if len(valid) != len(in) {
		t.Fatalf("kept %d refs, want %d", len(valid), len(in))
	}
}

func TestPartitionRefNamesRejectsDangerousNames(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		why  string
	}{
		{
			name: "parent traversal",
			ref:  "refs/heads/../../../../tmp/PWNED",
			why:  "convert-sha256 writes refs to disk; this escapes refs/heads",
		},
		{
			name: "traversal mid-path",
			ref:  "refs/heads/sub/../../../PWNED",
			why:  "lands at the repository root",
		},
		{
			name: "NUL smuggles a receive-pack feature list",
			ref:  "refs/heads/main\x00report-status",
			why:  "receive-pack parses features after the first NUL",
		},
		{
			name: "newline",
			ref:  "refs/heads/main\nrefs/heads/other",
			why:  "line-oriented protocol confusion",
		},
		{
			name: "carriage return",
			ref:  "refs/heads/main\rother",
			why:  "terminal and log confusion",
		},
		{
			name: "escape sequence",
			ref:  "refs/heads/\x1b[2Kmain",
			why:  "ANSI escape reaching a terminal",
		},
		{
			name: "leading dash on a branch",
			ref:  "refs/heads/-oProxyCommand=id",
			why:  "argument-injection shape",
		},
		{
			name: "lock suffix",
			ref:  "refs/heads/main.lock",
			why:  "collides with git's ref lock file",
		},
		{
			name: "reflog selector",
			ref:  "refs/heads/main@{0}",
			why:  "not a ref name git accepts",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			valid, skipped := PartitionRefNames(refs("refs/heads/main", tc.ref))
			if len(skipped) != 1 || skipped[0] != tc.ref {
				t.Fatalf("expected %q to be skipped (%s), got skipped=%q", tc.ref, tc.why, skipped)
			}
			if len(valid) != 1 || valid[0].Name().String() != "refs/heads/main" {
				t.Fatalf("the valid ref alongside it must survive, got %v", valid)
			}
		})
	}
}

func TestPartitionRefNamesEmptyInput(t *testing.T) {
	valid, skipped := PartitionRefNames(nil)
	if len(valid) != 0 || len(skipped) != 0 {
		t.Fatalf("nil input: got valid=%v skipped=%v", valid, skipped)
	}
}

func TestWarnSkippedRefNamesQuotesControlCharacters(t *testing.T) {
	var buf bytes.Buffer
	// A name crafted to clear the line and overwrite earlier output if it were
	// printed raw to a terminal.
	WarnSkippedRefNames(&buf, "source", []string{"refs/heads/\x1b[2K\rspoofed"})
	out := buf.String()
	if strings.ContainsAny(out, "\x1b\r") {
		t.Errorf("raw control characters reached the output: %q", out)
	}
	if !strings.Contains(out, `\x1b`) {
		t.Errorf("expected the escape to be shown quoted, got %q", out)
	}
	if !strings.Contains(out, "source: skipping 1 ref(s)") {
		t.Errorf("missing the summary line: %q", out)
	}
}

func TestWarnSkippedRefNamesTruncatesAndCounts(t *testing.T) {
	names := make([]string, 0, maxWarnedRefNames+5)
	for i := range maxWarnedRefNames + 5 {
		names = append(names, "refs/heads/bad"+string(rune('a'+i))+"\x00")
	}
	var buf bytes.Buffer
	WarnSkippedRefNames(&buf, "target", names)
	out := buf.String()
	if !strings.Contains(out, "skipping 15 ref(s)") {
		t.Errorf("count must cover every skipped ref: %q", out)
	}
	if !strings.Contains(out, "and 5 more") {
		t.Errorf("expected truncation notice: %q", out)
	}
}

func TestWarnSkippedRefNamesNoOpWhenNothingSkipped(t *testing.T) {
	var buf bytes.Buffer
	WarnSkippedRefNames(&buf, "source", nil)
	if buf.Len() != 0 {
		t.Errorf("expected no output, got %q", buf.String())
	}
}
