package gitproto

import (
	"context"
	"io"
	"net/url"
)

// Conn represents a connection to a Git remote transport such as Smart HTTP
// or SSH.
type Conn interface {
	RequestInfoRefs(ctx context.Context, service string, gitProtocol string) ([]byte, error)
	PostRPCStreamBody(ctx context.Context, service string, body io.Reader, v2 bool, phase string) (io.ReadCloser, error)
	Endpoint() *url.URL
	ProgressWriter() io.Writer
	SetProgressWriter(w io.Writer)
	Close() error
}
