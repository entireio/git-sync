package gitproto

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"
	"sync"
)

// SSHLookPath is replaceable in tests.
var SSHLookPath = exec.LookPath

// SSHConn represents a Git transport over the local ssh binary.
type SSHConn struct {
	Label       string
	EndpointURL *url.URL
	sshPath     string
	progressOut io.Writer
}

// NewSSHConn creates a new SSH transport connection backed by the local ssh
// binary.
func NewSSHConn(ep *url.URL, label string) (*SSHConn, error) {
	sshPath, err := SSHLookPath("ssh")
	if err != nil {
		return nil, fmt.Errorf("locate ssh binary: %w", err)
	}
	normalizeEndpointPath(ep)
	return &SSHConn{
		Label:       label,
		EndpointURL: ep,
		sshPath:     sshPath,
	}, nil
}

func (c *SSHConn) Endpoint() *url.URL { return c.EndpointURL }

func (c *SSHConn) ProgressWriter() io.Writer { return c.progressOut }

func (c *SSHConn) SetProgressWriter(w io.Writer) { c.progressOut = w }

func (c *SSHConn) Close() error { return nil }

func (c *SSHConn) RequestInfoRefs(ctx context.Context, service string, gitProtocol string) ([]byte, error) {
	cmd, stderr, err := c.startRPC(ctx, service, gitProtocol)
	if err != nil {
		return nil, err
	}
	return requestInfoRefsWithCommand(ctx, service, cmd, stderr)
}

func requestInfoRefsWithCommand(ctx context.Context, service string, cmd *sshCommand, stderr *sshCommandError) ([]byte, error) {
	if err := cmd.Stdin.Close(); err != nil {
		return nil, fmt.Errorf("close ssh stdin for %s: %w", service, errors.Join(err, cleanupSSHCommand(cmd)))
	}
	data, readErr := io.ReadAll(cmd.Stdout)
	waitErr := cmd.wait()
	if ctx.Err() != nil {
		return nil, errors.Join(ctx.Err(), readErr, stderr.wrap(waitErr))
	}
	if readErr != nil {
		return nil, fmt.Errorf("%s info-refs: %w", service, readErr)
	}
	if len(data) > 0 {
		return data, nil
	}
	if waitErr != nil {
		return nil, fmt.Errorf("%s info-refs: %w", service, stderr.wrap(waitErr))
	}
	return data, nil
}

func (c *SSHConn) PostRPCStreamBody(ctx context.Context, service string, body io.Reader, v2 bool, phase string) (io.ReadCloser, error) {
	_ = phase // phase labels are HTTP-only today; SSH transport has no per-RPC stats tagging
	gitProtocol := ""
	if v2 {
		gitProtocol = GitProtocolV2
	}
	cmd, stderr, err := c.startRPC(ctx, service, gitProtocol)
	if err != nil {
		return nil, err
	}
	copyErr := make(chan error, 1)
	go func() {
		_, err := io.Copy(cmd.Stdin, body)
		closeErr := cmd.Stdin.Close()
		if err != nil {
			copyErr <- err
			return
		}
		copyErr <- closeErr
	}()
	stdout, err := discardSSHAdvertisement(cmd.Stdout)
	if err != nil {
		_ = cmd.Stdout.Close()
		waitErr := cmd.wait()
		return nil, fmt.Errorf("%s advertisement: %w", service, errors.Join(stderr.wrap(err), waitErr))
	}
	return &sshRPCStream{
		ctx:     ctx,
		stdout:  stdout,
		wait:    cmd.wait,
		copyErr: copyErr,
		stderr:  stderr,
	}, nil
}

func (c *SSHConn) startRPC(ctx context.Context, service string, gitProtocol string) (*sshCommand, *sshCommandError, error) {
	args, err := sshInvocationArgs(c.EndpointURL, service, gitProtocol)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, c.sshPath, args...)
	stderr := &sshCommandError{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("open ssh stdout for %s: %w", service, err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("open ssh stdin for %s: %w", service, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start ssh for %s: %w", service, stderr.wrap(err))
	}
	return &sshCommand{Cmd: cmd, Stdin: stdin, Stdout: stdout}, stderr, nil
}

func sshInvocationArgs(ep *url.URL, service string, gitProtocol string) ([]string, error) {
	destination, err := sshDestination(ep)
	if err != nil {
		return nil, err
	}
	remoteCommand, err := sshRemoteCommand(ep, service, gitProtocol)
	if err != nil {
		return nil, err
	}
	args := []string{"-o", "BatchMode=yes"}
	if port := ep.Port(); port != "" {
		if err := rejectOptionLike("SSH port", port); err != nil {
			return nil, err
		}
		args = append(args, "-p", port)
	}
	// The "--" terminates ssh option parsing so the destination can never be
	// consumed as a flag; rejectOptionLike below is the portable primary guard
	// (older clients ignore unknown operands but all support "--").
	args = append(args, "--", destination, remoteCommand)
	return args, nil
}

func sshDestination(ep *url.URL) (string, error) {
	if ep == nil || ep.Hostname() == "" {
		return "", errors.New("missing SSH host")
	}
	host := ep.Hostname()
	if err := rejectOptionLike("SSH host", host); err != nil {
		return "", err
	}
	if ep.User != nil && ep.User.Username() != "" {
		user := ep.User.Username()
		if err := rejectOptionLike("SSH username", user); err != nil {
			return "", err
		}
		return user + "@" + host, nil
	}
	return host, nil
}

// rejectOptionLike refuses a destination component that begins with "-". Such
// a value would be parsed by ssh as an option rather than an operand — e.g. a
// host of "-oProxyCommand=..." turns into arbitrary local command execution
// (the class of git's CVE-2017-1000117) — and is never a legitimate host,
// username, or port.
func rejectOptionLike(what, value string) error {
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("refusing %s %q: must not begin with '-'", what, value)
	}
	return nil
}

func sshRemoteCommand(ep *url.URL, service string, gitProtocol string) (string, error) {
	if ep == nil || ep.Path == "" {
		return "", errors.New("missing SSH repository path")
	}
	path := shellQuotePath(ep.Path)
	if gitProtocol != "" {
		return gitProtocolEnv(gitProtocol) + " " + service + " " + path, nil
	}
	return service + " " + path, nil
}

func gitProtocolEnv(gitProtocol string) string {
	return "GIT_PROTOCOL=" + shellQuote(gitProtocol)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func shellQuotePath(path string) string {
	if !strings.HasPrefix(path, "~") {
		return shellQuote(path)
	}
	slash := strings.IndexByte(path, '/')
	if slash < 0 {
		return path
	}
	return path[:slash+1] + shellQuote(path[slash+1:])
}

type sshCommand struct {
	Cmd    *exec.Cmd
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	waitFn func() error
}

func (c *sshCommand) wait() error {
	if c.waitFn != nil {
		return c.waitFn()
	}
	if err := c.Cmd.Wait(); err != nil {
		return fmt.Errorf("wait for ssh command: %w", err)
	}
	return nil
}

func cleanupSSHCommand(cmd *sshCommand) error {
	if cmd == nil {
		return nil
	}
	var errs []error
	if cmd.Stdout != nil {
		if err := cmd.Stdout.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close ssh stdout: %w", err))
		}
	}
	if err := cmd.wait(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func discardSSHAdvertisement(stdout io.ReadCloser) (io.ReadCloser, error) {
	buffered := bufio.NewReader(stdout)
	header, err := buffered.Peek(4)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return &bufferedReadCloser{Reader: buffered, Closer: stdout}, nil
		}
		return nil, fmt.Errorf("peek SSH advertisement: %w", err)
	}
	if !looksLikePktlineHeader(header) {
		return &bufferedReadCloser{Reader: buffered, Closer: stdout}, nil
	}
	reader := NewPacketReader(buffered)
	for {
		kind, _, err := reader.ReadPacket()
		if err != nil {
			return nil, err
		}
		if kind == PacketFlush {
			break
		}
	}
	return &bufferedReadCloser{Reader: reader.BufReader(), Closer: stdout}, nil
}

func looksLikePktlineHeader(header []byte) bool {
	if len(header) != 4 {
		return false
	}
	var fixed [4]byte
	copy(fixed[:], header)
	switch string(fixed[:]) {
	case "0000", "0001", "0002":
		return true
	}
	_, err := parseHexLength(fixed)
	return err == nil
}

type sshRPCStream struct {
	ctx     context.Context
	stdout  io.ReadCloser
	wait    func() error
	copyErr <-chan error
	stderr  *sshCommandError

	waitOnce sync.Once
	waitErr  error
}

type bufferedReadCloser struct {
	*bufio.Reader
	io.Closer
}

func (s *sshRPCStream) Read(p []byte) (int, error) {
	n, err := s.stdout.Read(p)
	return n, err //nolint:wrapcheck // io.Reader contract requires forwarding EOF and stream errors as-is
}

func (s *sshRPCStream) Close() error {
	closeErr := s.stdout.Close()
	s.waitOnce.Do(func() {
		copyErr := <-s.copyErr
		waitErr := s.wait()
		if copyErr != nil {
			s.waitErr = copyErr
			if waitErr != nil {
				s.waitErr = errors.Join(copyErr, s.stderr.wrap(waitErr))
			}
			return
		}
		if waitErr != nil {
			s.waitErr = s.stderr.wrap(waitErr)
		}
		if s.ctx != nil && s.ctx.Err() != nil {
			s.waitErr = errors.Join(s.ctx.Err(), s.waitErr)
		}
	})
	return errors.Join(closeErr, s.waitErr)
}

type sshCommandError struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (e *sshCommandError) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.buf.Write(p) //nolint:wrapcheck // io.Writer implementation forwards buffer write errors verbatim
}

func (e *sshCommandError) String() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.TrimSpace(e.buf.String())
}

func (e *sshCommandError) wrap(err error) error {
	if err == nil {
		return nil
	}
	if msg := e.String(); msg != "" {
		return fmt.Errorf("%w: %s", err, msg)
	}
	return err
}
