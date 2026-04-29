package gitproto

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/protocol/packp/capability"
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

// AuthMethod authorizes outbound HTTP requests for a remote. It is satisfied
// by *transporthttp.BasicAuth and *transporthttp.TokenAuth, whose Authorizer
// methods replaced the AuthMethod interface that go-git removed in v6 alpha.2.
type AuthMethod interface {
	Authorizer(req *http.Request) error
}

// Conn represents a connection to a remote Git HTTP endpoint.
type Conn struct {
	Label    string
	Endpoint *url.URL
	HTTP     *http.Client
	Auth     AuthMethod

	// FollowInfoRefsRedirect, when true, rewrites Endpoint.Scheme and
	// Endpoint.Host to the final URL returned by RequestInfoRefs after
	// HTTP redirects. Subsequent PostRPC* calls then target the
	// redirected host directly, matching vanilla git's smart-HTTP
	// behaviour for discovery-aware servers that 307 /info/refs to a
	// hosting replica. Endpoint.Path is never modified — it still
	// contains the repo path. Off by default to preserve behaviour for
	// callers that rely on Endpoint being stable.
	FollowInfoRefsRedirect bool
}

// NewConn creates a new connection to the given endpoint.
func NewConn(ep *url.URL, label string, auth AuthMethod, rt http.RoundTripper) *Conn {
	httpClient := &http.Client{Transport: rt}
	return NewConnWithHTTPClient(ep, label, auth, httpClient)
}

// NewConnWithHTTPClient creates a new connection using the provided HTTP client.
// Passing nil falls back to a default client and is intended only for direct
// callers outside git-sync's normal instrumented session setup.
func NewConnWithHTTPClient(ep *url.URL, label string, auth AuthMethod, httpClient *http.Client) *Conn {
	if httpClient == nil {
		httpClient = &http.Client{Transport: http.DefaultTransport}
	}
	normalizeEndpointPath(ep)
	return &Conn{
		Label:    label,
		Endpoint: ep,
		HTTP:     httpClient,
		Auth:     auth,
	}
}

func normalizeEndpointPath(ep *url.URL) {
	if ep == nil {
		return
	}
	ep.Path = strings.TrimRight(ep.Path, "/")
	ep.RawPath = strings.TrimRight(ep.RawPath, "/")
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
func RequestInfoRefs(ctx context.Context, conn *Conn, service string, gitProtocol string) ([]byte, error) {
	reqURL := fmt.Sprintf("%s/info/refs?service=%s", conn.Endpoint.String(), service)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create info-refs request: %w", err)
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
		return nil, fmt.Errorf("request info-refs: %w", err)
	}
	defer res.Body.Close()
	if err := httpError(res); err != nil {
		return nil, err
	}
	wantContentType := fmt.Sprintf("application/x-%s-advertisement", service)
	gotContentType := res.Header.Get("Content-Type")
	gotMediaType := gotContentType
	if gotContentType != "" {
		if mediaType, _, err := mime.ParseMediaType(gotContentType); err == nil {
			gotMediaType = mediaType
		}
	}
	if gotMediaType != wantContentType {
		return nil, fmt.Errorf("unexpected info/refs content-type %q, want %q", gotContentType, wantContentType)
	}
	if conn.FollowInfoRefsRedirect && res.Request != nil && res.Request.URL != nil {
		final := res.Request.URL
		if final.Host != conn.Endpoint.Host || final.Scheme != conn.Endpoint.Scheme {
			conn.Endpoint.Scheme = final.Scheme
			conn.Endpoint.Host = final.Host
		}
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
		return nil, fmt.Errorf("read RPC response: %w", err)
	}
	if int64(len(data)) > maxRPCResponse {
		return nil, fmt.Errorf("RPC response for %s exceeds %d byte limit", service, maxRPCResponse)
	}
	return data, nil
}

// PostRPCStream sends a POST to the given service and returns the response body
// as a streaming reader. Caller must close the returned ReadCloser.
func PostRPCStream(ctx context.Context, conn *Conn, service string, body []byte, v2 bool, phase string) (io.ReadCloser, error) {
	return PostRPCStreamBody(ctx, conn, service, bytes.NewReader(body), v2, phase)
}

// PostRPCStreamBody sends a POST to the given service using a streaming request body.
// Caller must close the returned ReadCloser.
func PostRPCStreamBody(ctx context.Context, conn *Conn, service string, body io.Reader, v2 bool, phase string) (io.ReadCloser, error) {
	reqURL := fmt.Sprintf("%s/%s", conn.Endpoint.String(), service)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, body)
	if err != nil {
		return nil, fmt.Errorf("create RPC request: %w", err)
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
		return nil, fmt.Errorf("post RPC: %w", err)
	}
	if err := httpError(res); err != nil {
		_ = res.Body.Close()
		return nil, err
	}
	return res.Body, nil
}

// ApplyAuth applies the given auth method to an HTTP request. Errors from
// the Authorizer (e.g. transient signing failures) are surfaced as request
// failures by leaving the Authorization header unset; the upstream server
// will reject with 401 and the caller logs the surrounding context.
func ApplyAuth(req *http.Request, auth AuthMethod) {
	if auth == nil {
		return
	}
	_ = auth.Authorizer(req) //nolint:errcheck // BasicAuth and TokenAuth never error; future authorizers should surface 401s instead
}
