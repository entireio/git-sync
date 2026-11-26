package gitproto

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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

func TestRemoteErrorKeptToOneSafeLine(t *testing.T) {
	t.Parallel()
	resp := pkt("ERR \x1b[2Jnot \x07our ref\nfetch ok \u009b31m\n")
	parsers := map[string]func(string) error{
		"acks v1": func(r string) error {
			_, err := parseCommonAcksV1(NewPacketReader(strings.NewReader(r)))
			return err
		},
		"acks v2": func(r string) error {
			_, err := parseCommonAcksV2(NewPacketReader(strings.NewReader(r)))
			return err
		},
		"fetch pack": func(r string) error {
			return consumeV2FetchPack(strings.NewReader(r), false, io.Discard, func(io.Reader) error { return nil })
		},
		"pack stream": func(r string) error {
			_, err := openV2PackStream(io.NopCloser(strings.NewReader(r)), false, io.Discard)
			return err
		},
	}
	for name, parse := range parsers {
		err := parse(resp)
		if err == nil || err.Error() != "remote: [2Jnot our ref fetch ok 31m" {
			t.Errorf("%s: got %q, want the remote text on one line without control characters", name, err)
		}
	}
}

// TestHasCommits_LocalGit runs upload-pack locally, statefully behind an ssh
// stand-in and statelessly behind git http-backend. Stateful v0 refuses a
// want of a commit no ref points at, which is why HasCommits offers the
// commits as haves instead. old and mid are on one line of history, and
// upload-pack acknowledges only the first of them it reads.
func TestHasCommits_LocalGit(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git binary not available: %v", err)
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	gitIn(t, root, "init", "-q", "-b", "main", repo)
	gitIn(t, repo, "config", "uploadpack.allowReachableSHA1InWant", "false")
	gitIn(t, repo, "config", "uploadpack.allowAnySHA1InWant", "false")
	for _, msg := range []string{"one", "two", "three"} {
		gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", msg)
	}
	old := plumbing.NewHash(strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD~2")))
	mid := plumbing.NewHash(strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD~1")))
	missing := plumbing.NewHash(strings.Repeat("7", 40))

	wrapper := filepath.Join(t.TempDir(), "ssh")
	script := "#!/bin/sh\nfor a; do last=$a; done\nexec sh -c \"$last\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := SSHLookPath
	t.Cleanup(func() { SSHLookPath = orig })
	SSHLookPath = func(string) (string, error) { return wrapper, nil }

	srv := httptest.NewServer(&cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1"},
	})
	t.Cleanup(srv.Close)
	httpURL, err := url.Parse(srv.URL + "/repo")
	if err != nil {
		t.Fatal(err)
	}

	dial := map[string]func() (Conn, error){
		"ssh": func() (Conn, error) {
			return NewSSHConn(&url.URL{Scheme: "ssh", Host: "localhost", Path: repo}, "target")
		},
		"http": func() (Conn, error) {
			return NewHTTPConnWithClient(httpURL, "target", nil, srv.Client()), nil
		},
	}
	for _, transport := range []string{"ssh", "http"} {
		for _, mode := range []string{"v1", "v2"} {
			name := transport + " " + mode
			conn, err := dial[transport]()
			if err != nil {
				t.Fatal(err)
			}
			refs, service, err := ListSourceRefs(t.Context(), conn, mode, []string{"refs/heads/"})
			if err != nil {
				t.Fatalf("%s: list refs: %v", name, err)
			}
			tips := make([]plumbing.Hash, 0, len(refs))
			for _, r := range refs {
				tips = append(tips, r.Hash())
			}
			got, err := service.HasCommits(t.Context(), conn, tips, []plumbing.Hash{old, mid, missing})
			checkAcks(t, name, got, err, map[plumbing.Hash]struct{}{old: {}, mid: {}}, "")
		}
	}
}

// TestHasCommits_ChainRequests offers every commit of a chain. Protocol v2
// leaves out a have whose child it read earlier in the same request, so the
// first request misses about half of them.
func TestHasCommits_ChainRequests(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git binary not available: %v", err)
	}
	t.Setenv("GIT_AUTHOR_DATE", "1700000000 +0000")
	t.Setenv("GIT_COMMITTER_DATE", "1700000000 +0000")
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	gitIn(t, root, "init", "-q", "-b", "main", repo)
	want := make(map[plumbing.Hash]struct{})
	offered := []plumbing.Hash{plumbing.NewHash(strings.Repeat("7", 40))}
	for i := range 20 {
		gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", strconv.Itoa(i))
		h := plumbing.NewHash(strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD")))
		want[h] = struct{}{}
		offered = append(offered, h)
	}

	var posts atomic.Int32
	backend := &cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL + "/repo")
	if err != nil {
		t.Fatal(err)
	}
	conn := NewHTTPConnWithClient(u, "target", nil, srv.Client())
	refs, service, err := ListSourceRefs(t.Context(), conn, "v2", []string{"refs/heads/"})
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	before := posts.Load()
	got, err := service.HasCommits(t.Context(), conn, []plumbing.Hash{refs[0].Hash()}, offered)
	checkAcks(t, "chain", got, err, want, "")
	if n := posts.Load() - before; n > 5 {
		t.Errorf("HasCommits made %d requests for a chain of %d commits, want at most 5", n, len(want))
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
