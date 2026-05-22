package gitproto

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
)

const maxHTTPErrorBody = 64 * 1024

// diagnosticHeaders carry trace/correlation IDs that operators of upstream
// services use to look up the failing request server-side. Surfaced in
// httpError so a 500 with an opaque body (e.g. "Internal Server Error") still
// gives the user something actionable to share when reporting the failure.
var diagnosticHeaders = []string{
	"Cf-Ray",
	"X-Request-Id",
	"Request-Id",
	"X-Trace-Id",
	"X-Amz-Request-Id",
	"X-Github-Request-Id",
	"Server",
	"Content-Type",
}

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
			reason = strings.TrimSpace(string(data))
		}
	}
	var diag []string
	for _, h := range diagnosticHeaders {
		if v := res.Header.Get(h); v != "" {
			diag = append(diag, h+"="+v)
		}
	}
	if len(diag) > 0 {
		return fmt.Errorf("http %d: %s [%s] %s", res.StatusCode, res.Request.URL.Redacted(), strings.Join(diag, ", "), reason)
	}
	return fmt.Errorf("http %d: %s %s", res.StatusCode, res.Request.URL.Redacted(), reason)
}

// StatsPhaseHeader is the HTTP header used to annotate requests with the
// current git-sync stats phase for round-trip tracking.
const StatsPhaseHeader = "X-Git-Sync-Stats-Phase"

// HTTPTraceEnv enables verbose httptrace logging to stderr when set to any
// non-empty value other than "0" or "false". Diagnoses connection-pool
// behavior against hosts that close idle keep-alive connections more
// aggressively than Go's transport assumes (CDN edges, some hosted git
// providers) — a stale pooled connection surfaces as "use of closed network
// connection" on the next POST. Off by default; zero overhead unless set.
const HTTPTraceEnv = "GITSYNC_HTTP_TRACE"

func httpTraceEnabled() bool {
	v := os.Getenv(HTTPTraceEnv)
	if v == "" {
		return false
	}
	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// withHTTPTrace returns ctx with a ClientTrace that logs connection lifecycle
// events for one request to stderr. label is prepended to every line so
// concurrent or interleaved requests stay readable. Returns ctx unchanged
// when GITSYNC_HTTP_TRACE is not enabled.
func withHTTPTrace(ctx context.Context, label string) context.Context {
	if !httpTraceEnabled() {
		return ctx
	}
	trace := &httptrace.ClientTrace{
		GetConn: func(hostPort string) {
			fmt.Fprintf(os.Stderr, "[httptrace] %s GetConn %s\n", label, hostPort)
		},
		GotConn: func(info httptrace.GotConnInfo) {
			fmt.Fprintf(os.Stderr,
				"[httptrace] %s GotConn reused=%v wasIdle=%v idle=%s local=%s remote=%s\n",
				label, info.Reused, info.WasIdle, info.IdleTime,
				info.Conn.LocalAddr(), info.Conn.RemoteAddr())
		},
		PutIdleConn: func(err error) {
			if err != nil {
				fmt.Fprintf(os.Stderr, "[httptrace] %s PutIdleConn err=%v\n", label, err)
			} else {
				fmt.Fprintf(os.Stderr, "[httptrace] %s PutIdleConn ok\n", label)
			}
		},
		ConnectStart: func(network, addr string) {
			fmt.Fprintf(os.Stderr, "[httptrace] %s ConnectStart %s %s\n", label, network, addr)
		},
		ConnectDone: func(network, addr string, err error) {
			fmt.Fprintf(os.Stderr, "[httptrace] %s ConnectDone %s %s err=%v\n", label, network, addr, err)
		},
		TLSHandshakeStart: func() {
			fmt.Fprintf(os.Stderr, "[httptrace] %s TLSHandshakeStart\n", label)
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			fmt.Fprintf(os.Stderr, "[httptrace] %s TLSHandshakeDone resumed=%v err=%v\n",
				label, state.DidResume, err)
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			fmt.Fprintf(os.Stderr, "[httptrace] %s WroteRequest err=%v\n", label, info.Err)
		},
	}
	return httptrace.WithClientTrace(ctx, trace)
}

// dumpOutgoingRequest prints the wire-format request line and headers for
// req to stderr, prefixed with label. The body is not consumed (passes
// body=false to httputil.DumpRequestOut), but Transfer-Encoding and
// Content-Length will reflect what Go's transport would actually send.
// Useful when a server behaves unexpectedly on a POST and you need to
// see what the request looked like at the protocol level — the
// connection-level trace tells you which TCP/TLS connection was used
// but not what was written on it. Best-effort: dump errors are
// surfaced as a single line so a transient dump failure doesn't mask
// the underlying request.
func dumpOutgoingRequest(req *http.Request, label string) {
	dump, err := httputil.DumpRequestOut(req, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[httptrace] %s dump error: %v\n", label, err)
		return
	}
	fmt.Fprintf(os.Stderr, "[httptrace] %s outgoing request:\n%s\n", label, redactAuthorization(dump))
}

// redactAuthorization scrubs any Authorization header value from a dumped
// HTTP request so the credentials don't leak into stderr when
// GITSYNC_HTTP_TRACE is enabled in environments with shoulder-surfers,
// pasted-into-tickets logs, or shared shells.
func redactAuthorization(dump []byte) []byte {
	const header = "Authorization:"
	idx := bytes.Index(dump, []byte(header))
	if idx < 0 {
		return dump
	}
	end := bytes.IndexByte(dump[idx:], '\n')
	if end < 0 {
		end = len(dump) - idx
	}
	out := make([]byte, 0, len(dump))
	out = append(out, dump[:idx]...)
	out = append(out, []byte(header+" [REDACTED]")...)
	out = append(out, dump[idx+end:]...)
	return out
}

// AuthMethod authorizes outbound HTTP requests for a remote. It is satisfied
// by *transporthttp.BasicAuth and *transporthttp.TokenAuth, whose Authorizer
// methods replaced the AuthMethod interface that go-git removed in v6 alpha.2.
type AuthMethod interface {
	Authorizer(req *http.Request) error
}

// HTTPConn represents a connection to a remote Git HTTP endpoint.
type HTTPConn struct {
	Label       string
	EndpointURL *url.URL
	HTTP        *http.Client
	Auth        AuthMethod

	// FollowInfoRefsRedirect, when true, rewrites Endpoint.Scheme and
	// Endpoint.Host to the final URL returned by RequestInfoRefs after
	// HTTP redirects. Subsequent PostRPC* calls then target the
	// redirected host directly, matching vanilla git's smart-HTTP
	// behaviour for discovery-aware servers that 307 /info/refs to a
	// hosting replica. Endpoint.Path is never modified — it still
	// contains the repo path. Off by default to preserve behaviour for
	// callers that rely on Endpoint being stable.
	FollowInfoRefsRedirect bool

	// ProgressOut is the destination for verbose sideband progress
	// messages ("Enumerating objects: ...", "Resolving deltas: ..."
	// streamed by upload-pack and receive-pack). Nil falls back to
	// os.Stderr. Callers driving a live progress ticker can plug in a
	// coordinated writer here so server-side progress lines don't
	// clobber the in-place ticker frame.
	ProgressOut io.Writer
}

// NewHTTPConn creates a new connection to the given endpoint.
func NewHTTPConn(ep *url.URL, label string, auth AuthMethod, rt http.RoundTripper) *HTTPConn {
	httpClient := &http.Client{Transport: rt}
	return NewHTTPConnWithClient(ep, label, auth, httpClient)
}

// NewHTTPConnWithClient creates a new connection using the provided HTTP client.
// Passing nil falls back to a default client and is intended only for direct
// callers outside git-sync's normal instrumented session setup.
func NewHTTPConnWithClient(ep *url.URL, label string, auth AuthMethod, httpClient *http.Client) *HTTPConn {
	if httpClient == nil {
		httpClient = &http.Client{Transport: http.DefaultTransport}
	}
	normalizeEndpointPath(ep)
	return &HTTPConn{
		Label:       label,
		EndpointURL: ep,
		HTTP:        httpClient,
		Auth:        auth,
	}
}

func (c *HTTPConn) Endpoint() *url.URL { return c.EndpointURL }

func (c *HTTPConn) ProgressWriter() io.Writer { return c.ProgressOut }

func (c *HTTPConn) SetProgressWriter(w io.Writer) { c.ProgressOut = w }

func (c *HTTPConn) Close() error { return nil }

func normalizeEndpointPath(ep *url.URL) {
	if ep == nil {
		return
	}
	ep.Path = strings.TrimRight(ep.Path, "/")
	ep.RawPath = strings.TrimRight(ep.RawPath, "/")
}

// NewHTTPTransport returns the default git-sync HTTP transport. It clones
// http.DefaultTransport so config changes (TLS, keep-alive policy) don't
// leak into other code in the same process.
//
// Keep-alives are disabled. The git smart-HTTP workflow over the same host
// is coarse-grained — info/refs, then a single upload-pack or receive-pack
// POST — with real work in between (planning, source fetch, local object
// materialization). On the push side that gap is long enough for CDN
// edges and some hosted git providers to close their end of an idle TLS
// socket; the next POST then fails with "use of closed network connection"
// because the pooled connection is half-dead. Pool reuse would save at
// most one TLS handshake per sync, which is negligible against multi-MB
// to multi-GB transfers, so we prefer a fresh connection per request and
// avoid the race entirely.
//
// Library callers that need pool reuse (e.g. embedding git-sync in a
// long-running process that hits the same host repeatedly with short
// gaps) can pass their own RoundTripper to NewHTTPConn instead.
func NewHTTPTransport(skipTLS bool) http.RoundTripper {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	tc := base.Clone()
	tc.DisableKeepAlives = true
	if skipTLS {
		if tc.TLSClientConfig == nil {
			tc.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		tc.TLSClientConfig.InsecureSkipVerify = true
	}
	return tc
}

// RequestInfoRefs fetches /info/refs for the given service.
func RequestInfoRefs(ctx context.Context, conn Conn, service string, gitProtocol string) ([]byte, error) {
	data, err := conn.RequestInfoRefs(ctx, service, gitProtocol)
	if err != nil {
		return nil, fmt.Errorf("request info refs: %w", err)
	}
	return data, nil
}

// RequestInfoRefs fetches /info/refs for the given service.
func (c *HTTPConn) RequestInfoRefs(ctx context.Context, service string, gitProtocol string) ([]byte, error) {
	reqURL := fmt.Sprintf("%s/info/refs?service=%s", c.EndpointURL.String(), service)
	ctx = withHTTPTrace(ctx, "GET "+service+"/info/refs")
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
	ApplyAuth(req, c.Auth)

	res, err := c.HTTP.Do(req)
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
	if c.FollowInfoRefsRedirect && res.Request != nil && res.Request.URL != nil {
		final := res.Request.URL
		if final.Host != c.EndpointURL.Host || final.Scheme != c.EndpointURL.Scheme {
			c.EndpointURL.Scheme = final.Scheme
			c.EndpointURL.Host = final.Host
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
func PostRPC(ctx context.Context, conn Conn, service string, body []byte, v2 bool, phase string) ([]byte, error) {
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
func PostRPCStream(ctx context.Context, conn Conn, service string, body []byte, v2 bool, phase string) (io.ReadCloser, error) {
	return PostRPCStreamBody(ctx, conn, service, bytes.NewReader(body), v2, phase)
}

// PostRPCStreamBody sends a POST to the given service using a streaming request body.
// Caller must close the returned ReadCloser.
func PostRPCStreamBody(ctx context.Context, conn Conn, service string, body io.Reader, v2 bool, phase string) (io.ReadCloser, error) {
	reader, err := conn.PostRPCStreamBody(ctx, service, body, v2, phase)
	if err != nil {
		return nil, fmt.Errorf("post RPC stream body: %w", err)
	}
	return reader, nil
}

// PostRPCStreamBody sends a POST to the given service using a streaming request body.
// Caller must close the returned ReadCloser.
//
// The body is sent as-is — streaming readers produce a chunked request.
func (c *HTTPConn) PostRPCStreamBody(ctx context.Context, service string, body io.Reader, v2 bool, phase string) (io.ReadCloser, error) {
	reqURL := fmt.Sprintf("%s/%s", c.EndpointURL.String(), service)
	ctx = withHTTPTrace(ctx, "POST "+service)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, body)
	if err != nil {
		return nil, fmt.Errorf("create RPC request: %w", err)
	}
	req.Header.Set("Content-Type", fmt.Sprintf("application/x-%s-request", service))
	req.Header.Set("Accept", fmt.Sprintf("application/x-%s-result", service))
	req.Header.Set("User-Agent", capability.DefaultAgent())
	req.Header.Set(StatsPhaseHeader, phase)
	if v2 {
		req.Header.Set("Git-Protocol", GitProtocolV2)
	}
	ApplyAuth(req, c.Auth)

	if httpTraceEnabled() {
		dumpOutgoingRequest(req, "POST "+service)
	}

	res, err := c.HTTP.Do(req)
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
