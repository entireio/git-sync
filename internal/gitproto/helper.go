package gitproto

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
)

// RemoteHelperLookPath resolves the git remote-helper binary for a URL scheme,
// following git's `git-remote-<scheme>` naming convention. Replaceable in
// tests; production wiring just calls exec.LookPath.
var RemoteHelperLookPath = func(scheme string) (string, bool) {
	path, err := exec.LookPath("git-remote-" + scheme)
	if err != nil {
		return "", false
	}
	return path, true
}

// LookupRemoteHelper reports whether a git remote helper is installed for the
// given URL scheme (e.g. "entire" → git-remote-entire). Schemes git-sync
// speaks natively (http/https/ssh) should never be routed here — git ships
// git-remote-http(s), and diverting to it would bypass the optimized native
// transport.
func LookupRemoteHelper(scheme string) (string, bool) {
	if scheme == "" {
		return "", false
	}
	return RemoteHelperLookPath(scheme)
}

// HelperConn speaks the git remote-helper protocol (gitremote-helpers(7)) to a
// git-remote-<scheme> binary, bridging git-sync's smart-transport Conn onto the
// helper's stateless-connect capability. It lets git-sync drive schemes it has
// no native transport for (e.g. entire://) by delegating auth and the actual
// network I/O to the helper, while still running the wire protocol itself.
//
// It requires the helper to support stateless-connect (the modern v2 bridge)
// for both upload-pack and receive-pack. It probes the helper's advertised
// capabilities first and fails with a clear error if stateless-connect is
// absent; it does not fall back to the legacy `connect` capability. A
// per-service "fallback" reply is also treated as an error. git-remote-entire
// satisfies this.
//
// Each Conn operation spawns its own helper process and tears it down when the
// operation completes. This is deliberate, not lazy: the helper services
// exactly one stateless-connect session per process (its protocol loop returns
// once the session ends), and its receive-pack path reads the pushed pack until
// stdin EOF — both of which make "fresh process per RPC" the simplest correct
// model, and it naturally supports git-sync's batched (multi-request) pushes.
// The cost is one extra info/refs request per RPC, negligible beside pack
// transfer.
//
// Like the SSH transport, the helper bridge has no per-request byte accounting,
// so --stats omits helper-side throughput.
type HelperConn struct {
	HelperPath  string
	EndpointURL *url.URL
	// RawURL is the user-typed URL (e.g. entire://host/path), passed to the
	// helper verbatim — the helper, not git-sync, owns scheme interpretation.
	RawURL      string
	Label       string
	progressOut io.Writer
}

// NewHelperConn builds a remote-helper-backed connection. helperPath is the
// resolved git-remote-<scheme> binary; rawURL is the URL as the user typed it.
func NewHelperConn(helperPath string, ep *url.URL, rawURL, label string) *HelperConn {
	return &HelperConn{HelperPath: helperPath, EndpointURL: ep, RawURL: rawURL, Label: label}
}

func (c *HelperConn) Endpoint() *url.URL { return c.EndpointURL }

func (c *HelperConn) ProgressWriter() io.Writer { return c.progressOut }

func (c *HelperConn) SetProgressWriter(w io.Writer) { c.progressOut = w }

// Close is a no-op: HelperConn owns no long-lived process. Each RPC manages its
// own helper process lifetime.
func (c *HelperConn) Close() error { return nil }

// RequestInfoRefs spawns the helper, opens a stateless-connect session for the
// service, and returns the advertisement the helper emits (the v2 capability
// advertisement for upload-pack, or the v0 ref advertisement for receive-pack).
// The helper strips the smart-HTTP "# service=" banner itself, so the bytes
// returned match what the SSH transport produces — bannerless and terminated by
// a flush, which both DecodeV2Capabilities and decodeV1AdvRefs already accept.
func (c *HelperConn) RequestInfoRefs(ctx context.Context, service string, gitProtocol string) ([]byte, error) {
	_ = gitProtocol // the helper negotiates v2 with the remote itself
	proc, err := c.dial(ctx, service)
	if err != nil {
		return nil, err
	}
	adv, readErr := readAdvertisement(proc.out)
	// errors.Join drops nils, so this covers the readErr-only, finishErr-only,
	// and both-set cases in one branch.
	if err := errors.Join(readErr, proc.finish()); err != nil {
		return nil, fmt.Errorf("%s advertisement: %w", service, err)
	}
	return adv, nil
}

// PostRPCStreamBody spawns the helper, opens a stateless-connect session, sends
// the request body, and returns the response stream. The helper always emits
// ack + advertisement before reading the request, so the advertisement is
// consumed and discarded first. For v2 the helper frames each response with a
// trailing response-end (0002) packet, which is stripped so the consumer sees
// exactly the byte stream it would over HTTP; for v0 (receive-pack) the response
// runs to EOF.
func (c *HelperConn) PostRPCStreamBody(ctx context.Context, service string, body io.Reader, v2 bool, phase string) (io.ReadCloser, error) {
	_ = phase // helper transport has no per-RPC byte-stats tagging (like SSH)
	proc, err := c.dial(ctx, service)
	if err != nil {
		return nil, err
	}
	if _, err := readAdvertisement(proc.out); err != nil {
		return nil, fmt.Errorf("%s advertisement: %w", service, errors.Join(err, proc.cleanup()))
	}

	copyErr := make(chan error, 1)
	go func() {
		_, err := io.Copy(proc.stdin, body)
		// Closing stdin terminates the request: it bounds the v2
		// flush-terminated read and, crucially, signals end-of-pack to the
		// receive-pack handler, which copies the pack until EOF.
		closeErr := proc.stdin.Close()
		if err != nil {
			copyErr <- err
			return
		}
		copyErr <- closeErr
	}()

	var resp io.Reader = proc.out
	if v2 {
		resp = newResponseEndReader(proc.out)
	}
	return &helperRPCStream{ctx: ctx, resp: resp, proc: proc, copyErr: copyErr}, nil
}

// dial starts the helper process and opens a stateless-connect session for the
// service, consuming the helper's single-line acknowledgement. On success the
// helper is positioned to emit its advertisement.
func (c *HelperConn) dial(ctx context.Context, service string) (*helperProcess, error) {
	// git invokes a remote helper as `git-remote-<scheme> <remote> <url>`.
	// Passing the user-typed URL for both arguments matches git's handling of
	// an anonymous (URL-only) remote, which is what git-sync always has.
	cmd := exec.CommandContext(ctx, c.HelperPath, c.RawURL, c.RawURL)
	// GIT_PROTOCOL=version=2 makes the helper advertise stateless-connect; we
	// drive stateless-connect directly regardless, but this keeps the helper's
	// own negotiation aligned with how we use it.
	cmd.Env = append(os.Environ(), "GIT_PROTOCOL="+GitProtocolV2)
	stderr := &sshCommandError{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open helper stdout: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open helper stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start remote helper %s: %w", c.HelperPath, stderr.wrap(err))
	}
	proc := &helperProcess{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		out:    bufio.NewReaderSize(stdout, 65536),
		stderr: stderr,
	}
	// Probe capabilities first so an incompatible helper produces a clear error
	// instead of a cryptic mid-protocol failure. This is a local exchange — no
	// network round trip happens until stateless-connect below.
	caps, err := proc.readCapabilities()
	if err != nil {
		return nil, fmt.Errorf("probe remote helper %s: %w", c.HelperPath, errors.Join(err, proc.cleanup()))
	}
	if !slices.Contains(caps, "stateless-connect") {
		unsupported := fmt.Errorf("remote helper %s does not support stateless-connect (advertises: %s); git-sync needs a stateless-connect-capable helper such as git-remote-entire",
			c.HelperPath, strings.Join(caps, " "))
		return nil, errors.Join(unsupported, proc.cleanup())
	}
	if _, err := io.WriteString(stdin, "stateless-connect "+service+"\n"); err != nil {
		return nil, fmt.Errorf("request stateless-connect %s: %w", service, errors.Join(err, proc.cleanup()))
	}
	if err := proc.readAck(); err != nil {
		return nil, fmt.Errorf("stateless-connect %s: %w", service, errors.Join(err, proc.cleanup()))
	}
	return proc, nil
}

// helperProcess wraps a single helper invocation and its pipes.
type helperProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	out    *bufio.Reader
	stderr *sshCommandError

	stdinOnce sync.Once
}

// readCapabilities issues the helper's `capabilities` command and returns the
// advertised capability names (the token before any argument on each line),
// reading until the blank terminator line.
func (p *helperProcess) readCapabilities() ([]string, error) {
	if _, err := io.WriteString(p.stdin, "capabilities\n"); err != nil {
		return nil, fmt.Errorf("request capabilities: %w", err)
	}
	var caps []string
	for {
		line, err := p.out.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read capabilities: %w", p.stderr.wrap(err))
		}
		if line = strings.TrimRight(line, "\r\n"); line == "" {
			return caps, nil
		}
		name, _, _ := strings.Cut(line, " ")
		caps = append(caps, name)
	}
}

// readAck consumes the helper's stateless-connect response line: an empty line
// means the connection is established, "fallback" means it can't proxy this
// service, and anything else is an error (with captured stderr).
func (p *helperProcess) readAck() error {
	line, err := p.out.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read helper response: %w", p.stderr.wrap(err))
	}
	switch strings.TrimRight(line, "\r\n") {
	case "":
		return nil
	case "fallback":
		return errors.New("remote helper cannot proxy this service (fallback)")
	default:
		return fmt.Errorf("unexpected helper response %q: %s", strings.TrimRight(line, "\r\n"), p.stderr.String())
	}
}

func (p *helperProcess) closeStdin() error {
	var err error
	p.stdinOnce.Do(func() { err = p.stdin.Close() })
	if err != nil {
		return fmt.Errorf("close helper stdin: %w", err)
	}
	return nil
}

func (p *helperProcess) wait() error {
	if err := p.cmd.Wait(); err != nil {
		return p.stderr.wrap(fmt.Errorf("remote helper: %w", err))
	}
	return nil
}

// finish closes the request side and waits for the helper to exit cleanly,
// draining any trailing output so cmd.Wait doesn't race the stdout pipe. Used
// by RequestInfoRefs, which needs only the advertisement.
func (p *helperProcess) finish() error {
	closeErr := p.closeStdin()
	_, _ = io.Copy(io.Discard, p.out) //nolint:errcheck // best-effort drain before Wait; exit status is authoritative
	return errors.Join(closeErr, p.wait())
}

// cleanup force-tears-down the process on an error path.
func (p *helperProcess) cleanup() error {
	_ = p.closeStdin() //nolint:errcheck // force-teardown path; wait status is the reported error
	_ = p.stdout.Close()
	return p.wait()
}

// readAdvertisement reads raw pkt-lines up to and including the first flush
// (0000) and returns them verbatim. This is the helper's service advertisement;
// the trailing flush is preserved because both advertisement decoders consume
// it as the section terminator.
func readAdvertisement(br *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	var scratch []byte
	for {
		kind, frame, err := readRawPktLine(br, scratch)
		if err != nil {
			return nil, fmt.Errorf("read advertisement pkt-line: %w", err)
		}
		buf.Write(frame)
		scratch = frame // reuse the (possibly grown) backing buffer next iteration
		if kind == PacketFlush {
			return buf.Bytes(), nil
		}
	}
}

// responseEndReader re-emits a helper's framed v2 response verbatim, returning
// io.EOF when it reaches the stateless-connect response-end (0002) packet. The
// 0002 is consumed but never forwarded, so the downstream protocol parser sees
// exactly the same bytes it would from an HTTP response body (which ends at the
// connection's EOF instead). The frame buffer is reused across packets, so
// relaying a multi-GB pack costs no per-packet allocation.
type responseEndReader struct {
	src     *bufio.Reader
	buf     []byte // reused backing for the current frame
	pending []byte // unread bytes of the current frame (sub-slice of buf)
	done    bool
}

func newResponseEndReader(src *bufio.Reader) *responseEndReader {
	return &responseEndReader{src: src}
}

func (r *responseEndReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.done {
			return 0, io.EOF
		}
		if err := r.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *responseEndReader) fill() error {
	kind, frame, err := readRawPktLine(r.src, r.buf)
	if err != nil {
		return fmt.Errorf("read response pkt-line: %w", err)
	}
	if kind == PacketResponseEnd {
		r.done = true
		return io.EOF
	}
	r.buf = frame // retain the grown backing for the next fill
	r.pending = frame
	return nil
}

// helperRPCStream is the io.ReadCloser returned for a helper RPC response. It
// forwards reads from the (optionally response-end-bounded) helper stdout and,
// on Close, tears the process down: closing both pipes (which unblocks the body
// writer if the consumer bailed mid-stream), then joining the body-copy error,
// the process exit status, and any captured stderr.
type helperRPCStream struct {
	ctx     context.Context
	resp    io.Reader
	proc    *helperProcess
	copyErr <-chan error

	closeOnce sync.Once
	closeErr  error
}

func (s *helperRPCStream) Read(p []byte) (int, error) {
	n, err := s.resp.Read(p)
	return n, err //nolint:wrapcheck // io.Reader contract requires forwarding EOF and stream errors as-is
}

func (s *helperRPCStream) Close() error {
	s.closeOnce.Do(func() {
		_ = s.proc.closeStdin() //nolint:errcheck // unblocks the body writer; copy/wait errors below are authoritative
		closeOut := s.proc.stdout.Close()
		copyErr := <-s.copyErr
		waitErr := s.proc.wait()

		s.closeErr = errors.Join(copyErr, waitErr)
		if s.closeErr == nil && closeOut != nil && !errors.Is(closeOut, os.ErrClosed) {
			s.closeErr = closeOut
		}
		if s.ctx != nil && s.ctx.Err() != nil {
			s.closeErr = errors.Join(s.ctx.Err(), s.closeErr)
		}
	})
	return s.closeErr
}
