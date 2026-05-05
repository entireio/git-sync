package gitproto

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os/exec"
	"strings"
)

// SSHConn implements Conn over an SSH connection by executing git-upload-pack
// or git-receive-pack on the remote host via a local ssh binary.
type SSHConn struct {
	Label       string
	EndpointURL *url.URL

	// ProgressOut is the destination for verbose sideband progress
	// messages ("Enumerating objects: ...", "Resolving deltas: ..."
	// streamed by upload-pack and receive-pack). Nil falls back to
	// os.Stderr.
	ProgressOut io.Writer

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	errBuf bytes.Buffer
}

func (conn *SSHConn) Endpoint() *url.URL {
	return conn.EndpointURL
}

func (conn *SSHConn) ProgressWriter() io.Writer {
	return conn.ProgressOut
}

func (conn *SSHConn) SetProgressWriter(w io.Writer) {
	conn.ProgressOut = w
}

func NewSSHConn(ep *url.URL, label string) *SSHConn {
	return &SSHConn{EndpointURL: ep, Label: label}
}

func (conn *SSHConn) RequestInfoRefs(ctx context.Context, service string, gitProtocol string) ([]byte, error) {
	if conn.cmd != nil {
		return nil, errors.New("SSH session already started")
	}

	destination := conn.EndpointURL.Host
	if conn.EndpointURL.User != nil {
		destination = conn.EndpointURL.User.Username() + "@" + destination
	} else {
		destination = "git@" + destination
	}
	path := strings.TrimPrefix(conn.EndpointURL.Path, "/")

	// Use BatchMode=yes to prevent silent hangs on interactive prompts
	conn.cmd = exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", destination, service, path)
	if gitProtocol != "" {
		conn.cmd.Env = append(conn.cmd.Environ(), "GIT_PROTOCOL="+gitProtocol)
	}

	conn.errBuf.Reset()
	conn.cmd.Stderr = &conn.errBuf

	var err error
	conn.stdin, err = conn.cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh stdin pipe: %w", err)
	}
	conn.stdout, err = conn.cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh stdout pipe: %w", err)
	}

	if err := conn.cmd.Start(); err != nil {
		return nil, fmt.Errorf("ssh start: %w", err)
	}

	// In the Git SSH protocol, the remote service immediately writes its
	// reference advertisement to stdout upon execution. We read and buffer
	// this discovery payload until the first flush packet, re-encoding it
	// into raw pkt-line format. This provides the SSH equivalent of a Smart
	// HTTP /info/refs response (without the HTTP-specific service header).
	var buf bytes.Buffer
	packetReader := NewPacketReader(conn.stdout)
	for {
		kind, payload, err := packetReader.ReadPacket()
		if err != nil {
			msg := strings.TrimSpace(conn.errBuf.String())
			if msg != "" {
				return nil, fmt.Errorf("ssh connection failed: %s", msg)
			}
			return nil, fmt.Errorf("read discovery packet: %w", err)
		}
		// Re-encode into pkt-line
		if kind == PacketFlush {
			buf.WriteString("0000")
			break
		}
		fmt.Fprintf(&buf, "%04x%s", len(payload)+4, payload)
	}

	return buf.Bytes(), nil
}

func (conn *SSHConn) PostRPCStreamBody(_ context.Context, _ string, body io.Reader, _ bool, _ string) (io.ReadCloser, error) {
	if conn.cmd == nil {
		return nil, errors.New("SSH session not started; call RequestInfoRefs first")
	}
	// Write the request body to stdin in a separate goroutine so we can
	// start reading the response from stdout.
	go func() {
		defer conn.stdin.Close()
		if _, err := io.Copy(conn.stdin, body); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
			log.Printf("ssh: failed to copy request body to %s: %v", conn.Label, err)
		}
	}()
	return conn.stdout, nil
}

func (conn *SSHConn) Close() error {
	if conn.cmd == nil {
		return nil
	}

	if conn.stdin != nil {
		_ = conn.stdin.Close()
	}

	err := conn.cmd.Wait()
	conn.cmd = nil
	if err != nil {
		return fmt.Errorf("wait for SSH command exit: %w", err)
	}
	return nil
}
