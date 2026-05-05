package gitproto

import (
	"context"
	"io"
	"net/url"
)

// Conn represents a connection to a Git remote transport (e.g. Smart HTTP or SSH).
type Conn interface {
	// RequestInfoRefs performs discovery and returns the advertised refs or v2 capabilities.
	RequestInfoRefs(ctx context.Context, service string, gitProtocol string) ([]byte, error)

	// PostRPCStreamBody sends the RPC request body and returns a reader for the response.
	PostRPCStreamBody(ctx context.Context, service string, body io.Reader, v2 bool, phase string) (io.ReadCloser, error)

	// EndpointUrl returns the URL of the endpoint.
	Endpoint() *url.URL

	// ProgressWriter returns the writer where sideband progress should be sent.
	ProgressWriter() io.Writer

	// SetProgressWriter sets the writer where sideband progress should be sent.
	SetProgressWriter(w io.Writer)

	// Close terminates the connection.
	Close() error
}
