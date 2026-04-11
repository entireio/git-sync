package gitproto

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"

	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	transporthttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// StatsPhaseHeader is the HTTP header used to annotate requests with the
// current git-sync stats phase for round-trip tracking.
const StatsPhaseHeader = "X-Git-Sync-Stats-Phase"

// Conn represents a connection to a remote Git HTTP endpoint.
type Conn struct {
	Label     string
	Endpoint  *transport.Endpoint
	Transport transport.Transport
	HTTP      *http.Client
	Auth      transport.AuthMethod
}

// NewConn creates a new connection to the given endpoint.
func NewConn(ep *transport.Endpoint, label string, auth transport.AuthMethod, rt http.RoundTripper) *Conn {
	httpClient := &http.Client{Transport: rt}
	return &Conn{
		Label:     label,
		Endpoint:  ep,
		Transport: transporthttp.NewClient(httpClient),
		HTTP:      httpClient,
		Auth:      auth,
	}
}

// NewHTTPTransport creates an http.Transport with optional TLS skip.
func NewHTTPTransport(skipTLS bool) http.RoundTripper {
	if !skipTLS {
		return http.DefaultTransport
	}
	if cloned, ok := http.DefaultTransport.(*http.Transport); ok {
		tc := cloned.Clone()
		if tc.TLSClientConfig == nil {
			tc.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		tc.TLSClientConfig.InsecureSkipVerify = true
		return tc
	}
	return http.DefaultTransport
}

// RequestInfoRefs fetches /info/refs for the given service.
func RequestInfoRefs(ctx context.Context, conn *Conn, service, gitProtocol string) ([]byte, error) {
	url := fmt.Sprintf("%s/info/refs?service=%s", conn.Endpoint.String(), service)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", capability.DefaultAgent())
	req.Header.Set(StatsPhaseHeader, service+" info-refs")
	if gitProtocol != "" {
		req.Header.Set("Git-Protocol", gitProtocol)
	}
	ApplyAuth(req, conn.Auth)

	res, err := conn.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if err := transporthttp.NewErr(res); err != nil {
		return nil, err
	}
	// Bound the read to prevent unbounded memory allocation (issue #9).
	const maxInfoRefsSize = 64 * 1024 * 1024 // 64 MiB
	lr := io.LimitReader(res.Body, maxInfoRefsSize+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxInfoRefsSize {
		return nil, fmt.Errorf("info/refs response exceeds %d byte limit", maxInfoRefsSize)
	}
	return data, nil
}

// PostRPC sends a buffered POST to the given service and returns the full response body.
// Responses are bounded to prevent unbounded memory allocation (issue #9).
func PostRPC(ctx context.Context, conn *Conn, service string, body []byte, v2 bool, phase string) ([]byte, error) {
	reader, err := PostRPCStream(ctx, conn, service, body, v2, phase)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	const maxRPCResponse = 128 * 1024 * 1024 // 128 MiB
	lr := io.LimitReader(reader, maxRPCResponse+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxRPCResponse {
		return nil, fmt.Errorf("RPC response for %s exceeds %d byte limit", service, maxRPCResponse)
	}
	return data, nil
}

// PostRPCStream sends a POST to the given service and returns the response body
// as a streaming reader. Caller must close the returned ReadCloser.
func PostRPCStream(ctx context.Context, conn *Conn, service string, body []byte, v2 bool, phase string) (io.ReadCloser, error) {
	url := fmt.Sprintf("%s/%s", conn.Endpoint.String(), service)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", fmt.Sprintf("application/x-%s-request", service))
	req.Header.Set("Accept", fmt.Sprintf("application/x-%s-result", service))
	req.Header.Set("User-Agent", capability.DefaultAgent())
	req.Header.Set(StatsPhaseHeader, phase)
	if v2 {
		req.Header.Set("Git-Protocol", "version=2")
	}
	ApplyAuth(req, conn.Auth)

	res, err := conn.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if err := transporthttp.NewErr(res); err != nil {
		_ = res.Body.Close()
		return nil, err
	}
	return res.Body, nil
}

// ApplyAuth applies the given auth method to an HTTP request.
func ApplyAuth(req *http.Request, auth transport.AuthMethod) {
	switch a := auth.(type) {
	case *transporthttp.BasicAuth:
		a.SetAuth(req)
	case *transporthttp.TokenAuth:
		a.SetAuth(req)
	}
}
