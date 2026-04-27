package gitproto

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v6/plumbing/transport"
	transporthttp "github.com/go-git/go-git/v6/plumbing/transport/http"
)

const maxHTTPErrorBody = 64 * 1024

// httpError checks an HTTP response status and returns an error for non-2xx responses.
func httpError(res *http.Response) error {
	if res.StatusCode >= http.StatusOK && res.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	var reason string
	if res.Body != nil {
		limited := io.LimitReader(res.Body, maxHTTPErrorBody+1)
		data, err := io.ReadAll(limited)
		if err == nil && len(data) > 0 {
			if len(data) > maxHTTPErrorBody {
				data = append(data[:maxHTTPErrorBody], []byte("...")...)
			}
			reason = string(data)
		}
	}
	return fmt.Errorf("http %d: %s %s", res.StatusCode, res.Request.URL.Redacted(), reason)
}

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

	// AfterInfoRefs, when set, is called with the /info/refs response
	// after RequestInfoRefs has read and bounded the body. The presented
	// res.Body is a fresh reader over the buffered advertisement, so the
	// hook can read it (or not) without affecting RequestInfoRefs's
	// return value. A non-nil return rewrites Endpoint.Scheme and
	// Endpoint.Host to those of the returned URL, so subsequent PostRPC*
	// calls target the chosen host. Endpoint.Path is never modified.
	// Returning nil leaves the endpoint unchanged.
	//
	// Use this for discovery-aware redirection: callers can read response
	// headers, follow the post-redirect URL, or implement protocol-specific
	// replica advertisement and pick a host. See
	// pkg/gitsync.FollowRedirectHook for the vanilla-git case (pin to
	// whatever http.Client redirected to).
	AfterInfoRefs func(*http.Response) *url.URL
}

// NewConn creates a new connection to the given endpoint.
func NewConn(ep *transport.Endpoint, label string, auth transport.AuthMethod, rt http.RoundTripper) *Conn {
	httpClient := &http.Client{Transport: rt}
	return NewConnWithHTTPClient(ep, label, auth, httpClient)
}

// NewConnWithHTTPClient creates a new connection using the provided HTTP client.
// Passing nil falls back to a default client and is intended only for direct
// callers outside git-sync's normal instrumented session setup.
func NewConnWithHTTPClient(ep *transport.Endpoint, label string, auth transport.AuthMethod, httpClient *http.Client) *Conn {
	if httpClient == nil {
		httpClient = &http.Client{Transport: http.DefaultTransport}
	}
	return &Conn{
		Label:     label,
		Endpoint:  ep,
		Transport: transporthttp.NewTransport(&transporthttp.TransportOptions{Client: httpClient}),
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
func RequestInfoRefs(ctx context.Context, conn *Conn, service transport.Service, gitProtocol string) ([]byte, error) {
	svc := service.String()
	url := fmt.Sprintf("%s/info/refs?service=%s", conn.Endpoint.String(), svc)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create info-refs request: %w", err)
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", capability.DefaultAgent())
	req.Header.Set(StatsPhaseHeader, svc+" info-refs")
	if gitProtocol != "" {
		req.Header.Set("Git-Protocol", gitProtocol)
	}
	ApplyAuth(req, conn.Auth)

	res, err := conn.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request info-refs: %w", err)
	}
	defer res.Body.Close()
	if err := httpError(res); err != nil {
		return nil, err
	}
	// Bound the read to prevent unbounded memory allocation (issue #9).
	const maxInfoRefsSize = 64 * 1024 * 1024 // 64 MiB
	lr := io.LimitReader(res.Body, maxInfoRefsSize+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("read info-refs response: %w", err)
	}
	if int64(len(data)) > maxInfoRefsSize {
		return nil, fmt.Errorf("info/refs response exceeds %d byte limit", maxInfoRefsSize)
	}
	if conn.AfterInfoRefs != nil {
		res.Body = io.NopCloser(bytes.NewReader(data))
		if pin := conn.AfterInfoRefs(res); pin != nil {
			if pin.Scheme != "" {
				conn.Endpoint.Scheme = pin.Scheme
			}
			if pin.Host != "" {
				conn.Endpoint.Host = pin.Host
			}
		}
	}
	return data, nil
}

// PostRPC sends a buffered POST to the given service and returns the full response body.
// Responses are bounded to prevent unbounded memory allocation (issue #9).
func PostRPC(ctx context.Context, conn *Conn, service transport.Service, body []byte, v2 bool, phase string) ([]byte, error) {
	reader, err := PostRPCStream(ctx, conn, service, body, v2, phase)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	const maxRPCResponse = 128 * 1024 * 1024 // 128 MiB
	lr := io.LimitReader(reader, maxRPCResponse+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("read RPC response: %w", err)
	}
	if int64(len(data)) > maxRPCResponse {
		return nil, fmt.Errorf("RPC response for %s exceeds %d byte limit", service, maxRPCResponse)
	}
	return data, nil
}

// PostRPCStream sends a POST to the given service and returns the response body
// as a streaming reader. Caller must close the returned ReadCloser.
func PostRPCStream(ctx context.Context, conn *Conn, service transport.Service, body []byte, v2 bool, phase string) (io.ReadCloser, error) {
	return PostRPCStreamBody(ctx, conn, service, bytes.NewReader(body), v2, phase)
}

// PostRPCStreamBody sends a POST to the given service using a streaming request body.
// Caller must close the returned ReadCloser.
func PostRPCStreamBody(ctx context.Context, conn *Conn, service transport.Service, body io.Reader, v2 bool, phase string) (io.ReadCloser, error) {
	svc := service.String()
	url := fmt.Sprintf("%s/%s", conn.Endpoint.String(), svc)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("create RPC request: %w", err)
	}
	req.Header.Set("Content-Type", fmt.Sprintf("application/x-%s-request", svc))
	req.Header.Set("Accept", fmt.Sprintf("application/x-%s-result", svc))
	req.Header.Set("User-Agent", capability.DefaultAgent())
	req.Header.Set(StatsPhaseHeader, phase)
	if v2 {
		req.Header.Set("Git-Protocol", "version=2")
	}
	ApplyAuth(req, conn.Auth)

	res, err := conn.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post RPC: %w", err)
	}
	if err := httpError(res); err != nil {
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
