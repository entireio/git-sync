package gitproto

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing/transport"
)

// A host or username beginning with "-" must be refused, not handed to ssh as
// an operand: ssh would parse "-oProxyCommand=..." as an option and execute an
// arbitrary local command (git's CVE-2017-1000117 class).
func TestSSHInvocationArgsRejectsOptionLikeDestination(t *testing.T) {
	cases := []struct {
		name string
		ep   *url.URL
	}{
		{"host", &url.URL{Scheme: "ssh", Host: "-oProxyCommand=touch /tmp/pwned", Path: "/repo.git"}},
		{"username", &url.URL{Scheme: "ssh", User: url.User("-oProxyCommand=x"), Host: "example.com", Path: "/repo.git"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := sshInvocationArgs(tc.ep, "git-upload-pack", ""); err == nil {
				t.Fatalf("expected option-like %s to be rejected, got nil error", tc.name)
			}
		})
	}
}

// "--" must precede the destination so ssh stops parsing options at it.
func TestSSHInvocationArgsTerminatesOptionsBeforeDestination(t *testing.T) {
	ep := &url.URL{Scheme: "ssh", User: url.User("alice"), Host: "example.com:2222", Path: "/repo.git"}
	args, err := sshInvocationArgs(ep, "git-upload-pack", "")
	if err != nil {
		t.Fatalf("sshInvocationArgs: %v", err)
	}
	destIdx := -1
	for i, a := range args {
		if a == "alice@example.com" {
			destIdx = i
			break
		}
	}
	if destIdx <= 0 {
		t.Fatalf("destination not found in args: %v", args)
	}
	if args[destIdx-1] != "--" {
		t.Fatalf("expected \"--\" immediately before destination, got %v", args)
	}
}

func TestNewSSHConnRequiresBinary(t *testing.T) {
	orig := SSHLookPath
	t.Cleanup(func() { SSHLookPath = orig })
	SSHLookPath = func(string) (string, error) {
		return "", errors.New("not found")
	}

	ep, err := transport.ParseURL("ssh://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	_, err = NewSSHConn(ep, "source")
	if err == nil || !strings.Contains(err.Error(), "locate ssh binary") {
		t.Fatalf("NewSSHConn error = %v, want locate ssh binary failure", err)
	}
}

func TestSSHConnRequestInfoRefsHonorsUserConfigAndProtocolV2(t *testing.T) {
	env := newSSHShimEnv(t)
	conn := newSSHTestConn(t, "ssh://example.com/repo.git", env.script)

	body, err := conn.RequestInfoRefs(t.Context(), "git-upload-pack", GitProtocolV2)
	if err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}
	if string(body) != "response-1" {
		t.Fatalf("RequestInfoRefs body = %q, want %q", body, "response-1")
	}

	logLine := env.logLines(t)[0]
	if got, want := logLine, "example.com\tgit-upload-pack '/repo.git'"; got != want {
		t.Fatalf("ssh invocation = %q, want %q", got, want)
	}
	if got, want := env.optLines(t)[0], "BatchMode=yes SendEnv=GIT_PROTOCOL\tversion=2"; got != want {
		t.Fatalf("ssh options and GIT_PROTOCOL = %q, want %q", got, want)
	}
}

// v2 rides the ssh env; v1 doesn't.
func TestSSHRPCProtocolEnv(t *testing.T) {
	t.Setenv("GIT_PROTOCOL", "version=2")
	env := newSSHShimEnvWith(t, "0000")
	conn := newSSHTestConn(t, "ssh://example.com/repo.git", env.script)

	for _, v2 := range []bool{true, false} {
		reader, err := conn.PostRPCStreamBody(t.Context(), "git-upload-pack", strings.NewReader("body"), v2, "fetch")
		if err != nil {
			t.Fatalf("PostRPCStreamBody(v2=%v): %v", v2, err)
		}
		if _, err := io.ReadAll(reader); err != nil {
			t.Fatalf("read response: %v", err)
		}
		if err := reader.Close(); err != nil {
			t.Fatalf("close response: %v", err)
		}
	}
	if _, err := conn.RequestInfoRefs(t.Context(), "git-upload-pack", ""); err != nil {
		t.Fatalf("RequestInfoRefs v1: %v", err)
	}

	want := []string{
		"BatchMode=yes SendEnv=GIT_PROTOCOL\tversion=2",
		"BatchMode=yes\t",
		"BatchMode=yes\t",
	}
	if got := env.optLines(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ssh options and GIT_PROTOCOL = %q, want %q", got, want)
	}
	for i, line := range env.logLines(t) {
		if got, want := line, "example.com\tgit-upload-pack '/repo.git'"; got != want {
			t.Fatalf("ssh invocation %d = %q, want %q", i+1, got, want)
		}
	}
}

// Auto mode negotiates v2 or falls back.
func TestSSHAutoNegotiation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := newSSHTestRepo(t)
	for _, tc := range []struct {
		name   string
		accept bool
		want   string
	}{
		{"server accepts GIT_PROTOCOL", true, "v2"},
		{"server ignores GIT_PROTOCOL", false, "v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := newSSHLocalServer(t, tc.accept)
			conn := newSSHTestConn(t, "ssh://example.com"+repo, script)

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			refs, svc, err := ListSourceRefs(ctx, conn, "auto", []string{"refs/heads/"})
			if err != nil {
				t.Fatalf("ListSourceRefs: %v", err)
			}
			if svc.Protocol != tc.want {
				t.Fatalf("negotiated protocol = %q, want %q", svc.Protocol, tc.want)
			}
			if svc.HeadTarget != "refs/heads/main" {
				t.Fatalf("HEAD target = %q, want refs/heads/main", svc.HeadTarget)
			}
			found := false
			for _, ref := range refs {
				found = found || ref.Name() == "refs/heads/main"
			}
			if !found {
				t.Fatalf("refs/heads/main missing from %v", refs)
			}
		})
	}
}

// newSSHTestRepo makes a one-commit repository.
func newSSHTestRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", dir},
		{"-C", dir, "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (output %q)", args, err, out)
		}
	}
	return dir
}

// newSSHLocalServer fakes ssh and sshd.
func newSSHLocalServer(t *testing.T, accept bool) string {
	t.Helper()
	// Like sshd: forward only if accepted.
	forward := ""
	if !accept {
		forward = "sendenv=\"\""
	}
	script := filepath.Join(t.TempDir(), "ssh-local.sh")
	content := strings.Join([]string{
		"#!/bin/sh",
		"sendenv=\"\"",
		"while [ \"$1\" = \"-o\" ]; do",
		"  if [ \"$2\" = \"SendEnv=GIT_PROTOCOL\" ]; then sendenv=1; fi",
		"  shift 2",
		"done",
		"if [ \"$1\" = \"--\" ]; then shift; fi",
		forward,
		"if [ -z \"$sendenv\" ]; then unset GIT_PROTOCOL; fi",
		// Reject env-prefixed commands, like GitHub.
		"case \"$2\" in git-upload-pack\\ *) ;; *) echo \"Invalid command: $2\" >&2; exit 1;; esac",
		"PATH=\"$(git --exec-path):$PATH\"",
		"exec /bin/sh -c \"$2\"",
	}, "\n")
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write ssh shim: %v", err)
	}
	return script
}

func TestSSHConnRequestInfoRefsSupportsSCPStyleAndPort(t *testing.T) {
	env := newSSHShimEnv(t)

	scpConn := newSSHTestConn(t, "git@example.com:repo.git", env.script)
	if _, err := scpConn.RequestInfoRefs(t.Context(), "git-upload-pack", ""); err != nil {
		t.Fatalf("scp RequestInfoRefs: %v", err)
	}

	portConn := newSSHTestConn(t, "ssh://alice@example.com:2222/repo.git", env.script)
	if _, err := portConn.RequestInfoRefs(t.Context(), "git-upload-pack", ""); err != nil {
		t.Fatalf("port RequestInfoRefs: %v", err)
	}

	lines := env.logLines(t)
	if got, want := lines[0], "git@example.com\tgit-upload-pack 'repo.git'"; got != want {
		t.Fatalf("scp invocation = %q, want %q", got, want)
	}
	if got, want := lines[1], "-p 2222 alice@example.com\tgit-upload-pack '/repo.git'"; got != want {
		t.Fatalf("port invocation = %q, want %q", got, want)
	}
}

// A "~" path is quoted along with the rest of the path. The remote side still
// expands it: git-upload-pack resolves the path through enter_repo(), which
// interpolates a leading "~" itself, so quoting does not break tilde paths.
func TestSSHConnRequestInfoRefsQuotesTildePaths(t *testing.T) {
	env := newSSHShimEnv(t)

	conn := newSSHTestConn(t, "git@example.com:~/repo with spaces.git", env.script)
	if _, err := conn.RequestInfoRefs(t.Context(), "git-upload-pack", ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}
	if got, want := env.logLines(t)[0], "git@example.com\tgit-upload-pack '~/repo with spaces.git'"; got != want {
		t.Fatalf("ssh invocation = %q, want %q", got, want)
	}
}

// The remote command string is interpreted by the remote login shell, so shell
// metacharacters in the path must never escape their quotes. A "~" prefix used
// to be exempt from quoting so the remote shell would expand it, which let a
// SCP-style URL smuggle commands past the quoting: "git@host:~a;id/repo.git"
// produced `git-upload-pack ~a;id/'repo.git'`, running `id` on the remote host.
func TestSSHRemoteCommandQuotesShellMetacharacters(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "tilde prefix with command separator",
			raw:  "git@example.com:~a;id/repo.git",
			want: "git-upload-pack '~a;id/repo.git'",
		},
		{
			name: "tilde prefix with backticks",
			raw:  "git@example.com:~`id`/repo.git",
			want: "git-upload-pack '~`id`/repo.git'",
		},
		{
			name: "tilde prefix with command substitution",
			raw:  "git@example.com:~$(id)/repo.git",
			want: "git-upload-pack '~$(id)/repo.git'",
		},
		{
			name: "tilde path with no slash at all",
			raw:  "git@example.com:~a;id",
			want: "git-upload-pack '~a;id'",
		},
		{
			name: "embedded single quote is escaped, not terminated",
			raw:  "git@example.com:~'; id; '/repo.git",
			want: `git-upload-pack '~'"'"'; id; '"'"'/repo.git'`,
		},
		{
			name: "plain tilde path still works",
			raw:  "git@example.com:~/repo.git",
			want: "git-upload-pack '~/repo.git'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep, err := transport.ParseURL(tc.raw)
			if err != nil {
				t.Fatalf("ParseURL(%q): %v", tc.raw, err)
			}
			got, err := sshRemoteCommand(ep, "git-upload-pack")
			if err != nil {
				t.Fatalf("sshRemoteCommand: %v", err)
			}
			if got != tc.want {
				t.Fatalf("sshRemoteCommand = %q, want %q", got, tc.want)
			}
		})
	}
}

// The authority on whether the quoting holds is a shell, not a string
// comparison: sshd hands this command to the remote login shell. Run it against
// a stubbed git-upload-pack and assert the shell passes the path through as one
// literal argument, with no injected command executed.
func TestSSHRemoteCommandSurvivesShellInterpretation(t *testing.T) {
	for _, raw := range []string{
		"git@example.com:~a;id/repo.git",
		"git@example.com:~`id`/repo.git",
		"git@example.com:~$(id)/repo.git",
		"git@example.com:~a;id",
		"git@example.com:~'; id; '/repo.git",
		"git@example.com:~/repo with spaces.git",
		"git@example.com:~/repo.git",
		"git@example.com:normal/repo.git",
	} {
		t.Run(raw, func(t *testing.T) {
			ep, err := transport.ParseURL(raw)
			if err != nil {
				t.Fatalf("ParseURL(%q): %v", raw, err)
			}
			remoteCommand, err := sshRemoteCommand(ep, "git-upload-pack")
			if err != nil {
				t.Fatalf("sshRemoteCommand: %v", err)
			}

			dir := t.TempDir()
			argvFile := filepath.Join(dir, "argv")
			marker := filepath.Join(dir, "injected")
			// Stubs use only shell builtins and redirection so PATH can be
			// restricted to dir alone.
			writeShellStub(t, filepath.Join(dir, "git-upload-pack"), "printf '%s\\n' \"$@\" > "+argvFile)
			writeShellStub(t, filepath.Join(dir, "id"), ": > "+marker)

			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", remoteCommand)
			cmd.Env = []string{"PATH=" + dir}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("run %q: %v (output %q)", remoteCommand, err, out)
			}

			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("injected command ran: %q executed `id`", remoteCommand)
			}
			argv, err := os.ReadFile(argvFile)
			if err != nil {
				t.Fatalf("read recorded argv: %v", err)
			}
			if got, want := string(argv), ep.Path+"\n"; got != want {
				t.Fatalf("remote received argv %q, want the path verbatim %q", got, want)
			}
		})
	}
}

func writeShellStub(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write stub %s: %v", path, err)
	}
}

func TestSSHConnPostRPCStreamBodyCanBeCalledRepeatedly(t *testing.T) {
	env := newSSHShimEnvWith(t, "0000")
	conn := newSSHTestConn(t, "ssh://example.com/repo.git", env.script)

	reader1, err := conn.PostRPCStreamBody(t.Context(), "git-upload-pack", strings.NewReader("first-body"), false, "fetch one")
	if err != nil {
		t.Fatalf("PostRPCStreamBody first: %v", err)
	}
	data1, err := io.ReadAll(reader1)
	if err != nil {
		t.Fatalf("read first response: %v", err)
	}
	if err := reader1.Close(); err != nil {
		t.Fatalf("close first response: %v", err)
	}

	reader2, err := conn.PostRPCStreamBody(t.Context(), "git-upload-pack", strings.NewReader("second-body"), false, "fetch two")
	if err != nil {
		t.Fatalf("PostRPCStreamBody second: %v", err)
	}
	data2, err := io.ReadAll(reader2)
	if err != nil {
		t.Fatalf("read second response: %v", err)
	}
	if err := reader2.Close(); err != nil {
		t.Fatalf("close second response: %v", err)
	}

	if string(data1) != "response-1" || string(data2) != "response-2" {
		t.Fatalf("responses = %q / %q, want response-1 / response-2", data1, data2)
	}
	if got, want := env.body(t, 1), "first-body"; got != want {
		t.Fatalf("first request body = %q, want %q", got, want)
	}
	if got, want := env.body(t, 2), "second-body"; got != want {
		t.Fatalf("second request body = %q, want %q", got, want)
	}
	if got := len(env.logLines(t)); got != 2 {
		t.Fatalf("ssh invocation count = %d, want 2", got)
	}
}

// GitHub drops requests sent mid-advertisement.
func TestSSHConnPostRPCStreamBodyWaitsForAdvertisement(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "strict-ssh.sh")
	bodyFile := filepath.Join(dir, "body")
	// The marker appears just before the closing flush-pkt, so a body read
	// that finds it missing came before the advertisement was read.
	marker := filepath.Join(dir, "advertised")
	content := strings.Join([]string{
		"#!/bin/sh",
		"printf '0009hello'",
		"sleep 0.2",
		": >" + shellQuote(marker),
		"printf '0000'",
		"cat >" + shellQuote(bodyFile),
		"printf 'response'",
	}, "\n")
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	conn := newSSHTestConn(t, "ssh://example.com/repo.git", script)

	body := &markerCheckedReader{t: t, marker: marker, Reader: strings.NewReader("request")}
	reader, err := conn.PostRPCStreamBody(t.Context(), "git-upload-pack", body, false, "fetch")
	if err != nil {
		t.Fatalf("PostRPCStreamBody: %v", err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close response: %v", err)
	}
	if string(data) != "response" {
		t.Fatalf("response = %q, want %q", data, "response")
	}
	sent, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(sent) != "request" {
		t.Fatalf("request body = %q, want %q", sent, "request")
	}
}

// markerCheckedReader fails the test if it is read before marker exists.
type markerCheckedReader struct {
	io.Reader

	t       *testing.T
	marker  string
	checked bool
}

func (r *markerCheckedReader) Read(p []byte) (int, error) {
	if !r.checked {
		r.checked = true
		if _, err := os.Stat(r.marker); err != nil {
			r.t.Errorf("request body read before the advertisement finished: %v", err)
		}
	}
	return r.Reader.Read(p)
}

func TestSSHConnRequestInfoRefsHonorsContext(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ssh-sleep.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	conn := newSSHTestConn(t, "ssh://example.com/repo.git", script)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	_, err := conn.RequestInfoRefs(ctx, "git-upload-pack", "")
	if err == nil {
		t.Fatal("RequestInfoRefs returned nil error on canceled context")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("RequestInfoRefs error = %v, want context deadline exceeded", err)
	}
}

func TestSSHConnPostRPCStreamBodyHonorsContext(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ssh-read-sleep.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '0000'\ncat >/dev/null\nsleep 5\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	conn := newSSHTestConn(t, "ssh://example.com/repo.git", script)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	// The deadline can expire before the shim advertises (a new script's
	// first exec is slow on macOS), and then PostRPCStreamBody reports it.
	reader, err := conn.PostRPCStreamBody(ctx, "git-upload-pack", strings.NewReader("body"), false, "fetch")
	if errors.Is(err, context.DeadlineExceeded) {
		return
	}
	if err != nil {
		t.Fatalf("PostRPCStreamBody: %v", err)
	}
	if _, err := io.ReadAll(reader); err != nil && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("ReadAll error = %v", err)
	}
	if err := reader.Close(); err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("reader.Close error = %v, want context deadline exceeded", err)
	}
}

func TestSSHConnPostRPCStreamBodyAdvertisementHonorsContext(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "ssh-partial-advert.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '0009hel'\nsleep 5\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	conn := newSSHTestConn(t, "ssh://example.com/repo.git", script)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	_, err := conn.PostRPCStreamBody(ctx, "git-upload-pack", strings.NewReader("body"), false, "fetch")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("PostRPCStreamBody error = %v, want context deadline exceeded", err)
	}
}

func TestSSHRPCStreamCloseReportsContextOverCopyError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	copyErr := make(chan error, 1)
	copyErr <- errors.New("write |1: broken pipe")
	stream := &sshRPCStream{
		ctx:     ctx,
		stdout:  closeReaderFunc(func() error { return nil }),
		wait:    func() error { return errors.New("signal: killed") },
		copyErr: copyErr,
		stderr:  &sshCommandError{},
	}
	if err := stream.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close error = %v, want context canceled", err)
	}
}

func TestRequestInfoRefsCleansUpWhenStdinCloseFails(t *testing.T) {
	t.Parallel()

	stdoutClosed := false
	waitCalled := false
	_, err := requestInfoRefsWithCommand(
		t.Context(),
		"git-upload-pack",
		&sshCommand{
			Stdin:  closeWriterFunc(func() error { return errors.New("close failed") }),
			Stdout: closeReaderFunc(func() error { stdoutClosed = true; return nil }),
			waitFn: func() error { waitCalled = true; return nil },
		},
		&sshCommandError{},
	)
	if err == nil || !strings.Contains(err.Error(), "close ssh stdin for git-upload-pack") {
		t.Fatalf("requestInfoRefsWithCommand error = %v", err)
	}
	if !stdoutClosed {
		t.Fatal("stdout was not closed on stdin-close failure")
	}
	if !waitCalled {
		t.Fatal("wait was not called on stdin-close failure")
	}
}

type sshShimEnv struct {
	script     string
	logFile    string
	optsFile   string
	bodyPrefix string
}

type closeWriterFunc func() error

func (f closeWriterFunc) Write(p []byte) (int, error) { return len(p), nil }
func (f closeWriterFunc) Close() error                { return f() }

type closeReaderFunc func() error

func (f closeReaderFunc) Read([]byte) (int, error) { return 0, io.EOF }
func (f closeReaderFunc) Close() error             { return f() }

func newSSHShimEnv(t *testing.T) sshShimEnv {
	t.Helper()
	return newSSHShimEnvWith(t, "")
}

// advert is printed before reading stdin.
func newSSHShimEnvWith(t *testing.T, advert string) sshShimEnv {
	t.Helper()
	dir := t.TempDir()
	logFile := filepath.Join(dir, "ssh.log")
	optsFile := filepath.Join(dir, "opts.log")
	countFile := filepath.Join(dir, "count")
	bodyPrefix := filepath.Join(dir, "body-")
	script := filepath.Join(dir, "ssh-shim.sh")
	content := strings.Join([]string{
		"#!/bin/sh",
		"count=0",
		"if [ -f " + shellQuote(countFile) + " ]; then count=$(cat " + shellQuote(countFile) + "); fi",
		"count=$((count+1))",
		"printf '%s' \"$count\" >" + shellQuote(countFile),
		"dest=\"\"",
		"remote=\"\"",
		"port=\"\"",
		"opts=\"\"",
		"while [ \"$1\" = \"-o\" ]; do",
		"  opts=\"$opts $2\"",
		"  shift 2",
		"done",
		"printf '%s\\t%s\\n' \"${opts# }\" \"${GIT_PROTOCOL-}\" >>" + shellQuote(optsFile),
		"if [ \"$1\" = \"-p\" ]; then",
		"  port=\"$2\"",
		"  shift 2",
		"fi",
		"if [ \"$1\" = \"--\" ]; then",
		"  shift",
		"fi",
		"if [ -n \"$port\" ]; then",
		"  dest=\"-p $port $1\"",
		"else",
		"  dest=\"$1\"",
		"fi",
		"remote=\"$2\"",
		"printf '%s\\t%s\\n' \"$dest\" \"$remote\" >>" + shellQuote(logFile),
		"printf '%s' " + shellQuote(advert),
		"cat >" + shellQuote(bodyPrefix) + "\"$count\"",
		"printf 'response-%s' \"$count\"",
	}, "\n")
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write ssh shim: %v", err)
	}
	return sshShimEnv{script: script, logFile: logFile, optsFile: optsFile, bodyPrefix: bodyPrefix}
}

func newSSHTestConn(t *testing.T, rawURL, script string) *SSHConn {
	t.Helper()
	orig := SSHLookPath
	t.Cleanup(func() { SSHLookPath = orig })
	SSHLookPath = func(string) (string, error) { return script, nil }

	ep, err := transport.ParseURL(rawURL)
	if err != nil {
		t.Fatalf("parse url %q: %v", rawURL, err)
	}
	conn, err := NewSSHConn(ep, "source")
	if err != nil {
		t.Fatalf("NewSSHConn(%q): %v", rawURL, err)
	}
	return conn
}

func (e sshShimEnv) logLines(t *testing.T) []string {
	t.Helper()
	return readLogLines(t, e.logFile)
}

// optLines lists "-o" values and GIT_PROTOCOL.
func (e sshShimEnv) optLines(t *testing.T) []string {
	t.Helper()
	return readLogLines(t, e.optsFile)
}

func readLogLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func (e sshShimEnv) body(t *testing.T, count int) string {
	t.Helper()
	data, err := os.ReadFile(e.bodyPrefix + strconv.Itoa(count))
	if err != nil {
		t.Fatalf("read body %d: %v", count, err)
	}
	return string(data)
}

// A remote that streams without end must produce the advertisement-limit
// error, not block. Reading stops at the cap, but the ssh process is still
// writing: unless its stdout is closed before Wait, it blocks forever on a
// full pipe and Wait never returns.
func TestSSHConnRequestInfoRefsDoesNotHangOnEndlessAdvertisement(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "endless-ssh.sh")
	// Ignores its arguments and streams zeros until the pipe is closed.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec cat /dev/zero\n"), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	conn := newSSHTestConn(t, "git@example.com:repo.git", script)

	done := make(chan error, 1)
	go func() {
		_, err := conn.RequestInfoRefs(t.Context(), "git-upload-pack", "")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an advertisement-limit error, got nil")
		}
		if !strings.Contains(err.Error(), "byte limit") {
			t.Errorf("error %q does not mention the limit", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RequestInfoRefs did not return: the transport hung after the read limit was reached")
	}
}
