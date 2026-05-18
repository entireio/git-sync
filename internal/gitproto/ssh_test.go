package gitproto

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing/transport"
)

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
	if got, want := logLine, "example.com\tGIT_PROTOCOL='version=2' git-upload-pack '/repo.git'"; got != want {
		t.Fatalf("ssh invocation = %q, want %q", got, want)
	}
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

func TestSSHConnRequestInfoRefsPreservesTildePaths(t *testing.T) {
	env := newSSHShimEnv(t)

	conn := newSSHTestConn(t, "git@example.com:~/repo with spaces.git", env.script)
	if _, err := conn.RequestInfoRefs(t.Context(), "git-upload-pack", ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}
	if got, want := env.logLines(t)[0], "git@example.com\tgit-upload-pack ~/'repo with spaces.git'"; got != want {
		t.Fatalf("ssh invocation = %q, want %q", got, want)
	}
}

func TestSSHConnPostRPCStreamBodyCanBeCalledRepeatedly(t *testing.T) {
	env := newSSHShimEnv(t)
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
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nsleep 5\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	conn := newSSHTestConn(t, "ssh://example.com/repo.git", script)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	reader, err := conn.PostRPCStreamBody(ctx, "git-upload-pack", strings.NewReader("body"), false, "fetch")
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
	dir := t.TempDir()
	logFile := filepath.Join(dir, "ssh.log")
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
		"if [ \"$1\" = \"-o\" ]; then",
		"  shift 2",
		"fi",
		"if [ \"$1\" = \"-p\" ]; then",
		"  port=\"$2\"",
		"  shift 2",
		"  dest=\"-p $port $1\"",
		"else",
		"  dest=\"$1\"",
		"fi",
		"remote=\"$2\"",
		"printf '%s\\t%s\\n' \"$dest\" \"$remote\" >>" + shellQuote(logFile),
		"cat >" + shellQuote(bodyPrefix) + "\"$count\"",
		"printf 'response-%s' \"$count\"",
	}, "\n")
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write ssh shim: %v", err)
	}
	return sshShimEnv{script: script, logFile: logFile, bodyPrefix: bodyPrefix}
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
	data, err := os.ReadFile(e.logFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	trimmed := strings.TrimSpace(string(data))
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
