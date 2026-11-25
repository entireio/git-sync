package gitproto

import (
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

func TestParseCommonAcksV1(t *testing.T) {
	t.Parallel()
	have := plumbing.NewHash(strings.Repeat("a", 40))
	missing := plumbing.NewHash(strings.Repeat("b", 40))

	tests := []struct {
		name    string
		resp    string
		want    map[plumbing.Hash]struct{}
		wantErr string
	}{
		{
			name: "common and ready",
			resp: pkt("ACK "+have.String()+" common\n") + pkt("ACK "+missing.String()+" ready\n") + pkt("NAK\n"),
			want: map[plumbing.Hash]struct{}{have: {}},
		},
		{name: "none", resp: pkt("NAK\n"), want: map[plumbing.Hash]struct{}{}},
		{name: "error", resp: pkt("ERR upload-pack: not our ref\n"), wantErr: "not our ref"},
		{name: "truncated", resp: pkt("ACK " + have.String() + " common\n"), wantErr: "pktline"},
	}
	for _, tt := range tests {
		got, err := parseCommonAcksV1(NewPacketReader(strings.NewReader(tt.resp)))
		checkAcks(t, tt.name, got, err, tt.want, tt.wantErr)
	}
}

func TestParseCommonAcksV2(t *testing.T) {
	t.Parallel()
	have := plumbing.NewHash(strings.Repeat("a", 40))

	tests := []struct {
		name    string
		resp    string
		want    map[plumbing.Hash]struct{}
		wantErr string
	}{
		{
			name: "ack",
			resp: pkt("acknowledgments\n") + pkt("ACK "+have.String()+"\n") + "0000",
			want: map[plumbing.Hash]struct{}{have: {}},
		},
		{name: "nak", resp: pkt("acknowledgments\n") + pkt("NAK\n") + "0000", want: map[plumbing.Hash]struct{}{}},
		{name: "ready", resp: pkt("acknowledgments\n") + pkt("ready\n") + "0001", wantErr: "ready"},
		{name: "error", resp: pkt("ERR boom\n"), wantErr: "boom"},
	}
	for _, tt := range tests {
		got, err := parseCommonAcksV2(NewPacketReader(strings.NewReader(tt.resp)))
		checkAcks(t, tt.name, got, err, tt.want, tt.wantErr)
	}
}

// TestHasCommits_StatefulSSH runs upload-pack locally behind an ssh stand-in.
// Stateful v0 refuses a want of a commit no ref points at, which is why
// HasCommits offers the commits as haves instead.
func TestHasCommits_StatefulSSH(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	for _, msg := range []string{"one", "two", "three"} {
		gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", msg)
	}
	old := plumbing.NewHash(strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD~2")))
	missing := plumbing.NewHash(strings.Repeat("7", 40))

	wrapper := filepath.Join(t.TempDir(), "ssh")
	script := "#!/bin/sh\nfor a; do last=$a; done\nexec sh -c \"$last\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := SSHLookPath
	t.Cleanup(func() { SSHLookPath = orig })
	SSHLookPath = func(string) (string, error) { return wrapper, nil }

	for _, mode := range []string{"v1", "v2"} {
		conn, err := NewSSHConn(&url.URL{Scheme: "ssh", Host: "localhost", Path: repo}, "target")
		if err != nil {
			t.Fatal(err)
		}
		refs, service, err := ListSourceRefs(t.Context(), conn, mode, []string{"refs/heads/"})
		if err != nil {
			t.Fatalf("%s: list refs: %v", mode, err)
		}
		tips := make([]plumbing.Hash, 0, len(refs))
		for _, r := range refs {
			tips = append(tips, r.Hash())
		}
		got, err := service.HasCommits(t.Context(), conn, tips, []plumbing.Hash{old, missing})
		checkAcks(t, mode, got, err, map[plumbing.Hash]struct{}{old: {}}, "")
	}
}

func checkAcks(t *testing.T, name string, got map[plumbing.Hash]struct{}, err error, want map[plumbing.Hash]struct{}, wantErr string) {
	t.Helper()
	switch {
	case wantErr != "":
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%s: got %v, want error mentioning %q", name, err, wantErr)
		}
	case err != nil:
		t.Errorf("%s: %v", name, err)
	case !maps.Equal(got, want):
		t.Errorf("%s: got %v, want %v", name, got, want)
	}
}

func pkt(s string) string {
	return fmt.Sprintf("%04x%s", len(s)+4, s)
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com",
		"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
