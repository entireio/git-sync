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

	"entire.io/entire/git-sync/internal/useragent"
	transporthttp "github.com/go-git/go-git/v6/plumbing/transport/http"
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

// CredentialHelper provides on-demand credentials when an HTTP request is
// rejected with 401. Lookup may block on user interaction if the underlying
// helper falls through to a terminal prompt — that's vanilla git's
// behaviour and intentional for interactive users. Callers that must not
// block (CI, daemons, the syncer's background loop) set
// GIT_TERMINAL_PROMPT=0 in their environment, which the credential
// subprocess inherits. Lookup returns ok=false when no credentials could
// be obtained, so the caller can surface a clean 401.
//
// Approve/Reject are advisory and intentionally have no error return:
// failures must not poison the outer request flow.
type CredentialHelper interface {
	Lookup(ctx context.Context, ep *url.URL) (username, password string, ok bool, err error)
	Approve(ctx context.Context, ep *url.URL, username, password string)
	Reject(ctx context.Context, ep *url.URL, username, password string)
}

// HTTPConn represents a connection to a remote Git HTTP endpoint.
type HTTPConn struct {
	Label       string
	EndpointURL *url.URL
	HTTP        *http.Client
	Auth        AuthMethod

	// CredentialHelper, if set, is consulted on a 401 response when no
	// initial Auth was configured. The retry happens once on /info/refs;
	// on success the resolved credentials are stored in Auth for the
	// remaining requests on this connection. Setting Auth up front
	// disables the helper fallback — explicit auth wins.
	CredentialHelper CredentialHelper

	// FollowInfoRefsRedirect, when true, rewrites Endpoint.Scheme and
	// Endpoint.Host to the final URL returned by RequestInfoRefs after
	// HTTP redirects. Subsequent PostRPC* calls then target the
	// redirected host directly, matching vanilla git's smart-HTTP
	// behaviour for discovery-aware servers that 307 /info/refs to a
	// hosting replica. Endpoint.Path is never modified — it still
	// contains the repo path. Off by default to preserve behaviour for
	// callers that rely on Endpoint being stable.
	FollowInfoRefsRedirect bool

	// InsecureSkipTLSVerify mirrors the same-named transport setting and
	// must be set by callers whenever the HTTP client they pass in has
	// TLS verification disabled. The credential-helper retry path uses it
	// to refuse cross-host operations: with TLS verification off there's
	// no way to know whether a redirect's destination is the host the
	// user trusts or a MITM impersonating it, so sending the helper's
	// stored credentials there is unsafe. Same-host 401s (no redirect)
	// still retry — the user has accepted whatever host they configured.
	InsecureSkipTLSVerify bool

	// ProgressOut is the destination for verbose sideband progress
	// messages ("Enumerating objects: ...", "Resolving deltas: ..."
	// streamed by upload-pack and receive-pack). Nil falls back to
	// os.Stderr. Callers driving a live progress ticker can plug in a
	// coordinated writer here so server-side progress lines don't
	// clobber the in-place ticker frame.
	ProgressOut io.Writer

	// pendingHelperCreds tracks credentials supplied by the helper via
	// EnsureAuthForService but not yet validated against a real operation.
	// The next RequestInfoRefs/PostRPCStreamBody approves on 2xx or rejects
	// on 401/403, ensuring helper state reflects the actual outcome rather
	// than an ambiguous probe response.
	pendingHelperCreds *helperCreds

	// resolvedEndpoint, when non-nil, supersedes EndpointURL.Scheme/Host
	// for outgoing requests on this conn. Populated when one of two
	// redirect-following paths fires:
	//
	//   - The FollowInfoRefsRedirect block in RequestInfoRefs, after a
	//     successful /info/refs that landed on a different host.
	//   - adoptChallengeHost, after credential-helper auth resolves
	//     against a cross-host challenge.
	//
	// Both are gated on FollowInfoRefsRedirect, so resolvedEndpoint only
	// diverges from EndpointURL when the user has opted into following
	// redirects. EndpointURL itself is never mutated — display, logging,
	// telemetry, and the user-typed-URL accessors (Endpoint()) keep
	// returning what the caller passed in.
	//
	// Path/userinfo are copied from EndpointURL; only Scheme/Host differ.
	resolvedEndpoint *url.URL
}

// requestURL returns the URL outgoing requests should build from on this
// conn. It is the resolved endpoint when one has been discovered (and the
// user opted into following redirects via FollowInfoRefsRedirect), and the
// user-typed EndpointURL otherwise.
func (c *HTTPConn) requestURL() *url.URL {
	if c.resolvedEndpoint != nil {
		return c.resolvedEndpoint
	}
	return c.EndpointURL
}

type helperCreds struct {
	user, pass string
	url        *url.URL
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
	res, err := c.doInfoRefsRequest(ctx, service, gitProtocol, c.Auth, nil)
	if err != nil {
		return nil, err
	}
	res, err = c.tryHelperRetry(ctx, res, func(auth AuthMethod, target *url.URL) (*http.Response, error) {
		return c.doInfoRefsRequest(ctx, service, gitProtocol, auth, target)
	})
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	data, err := c.readInfoRefsResponse(res, service)
	// Settle helper credentials on the fully-validated outcome: approve only
	// once the advertisement parsed and read within limits, reject on 401/403.
	// Running this after validation stops a misleading 2xx (wrong content-type
	// or oversized body) from persisting credentials for an operation that
	// ultimately failed.
	c.resolvePendingHelperCreds(ctx, res, err == nil)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// readInfoRefsResponse validates and reads an /info/refs response: it checks
// the HTTP status and advertisement content-type, applies any redirect to the
// endpoint, and reads the body under a size cap. The caller closes res.Body.
func (c *HTTPConn) readInfoRefsResponse(res *http.Response, service string) ([]byte, error) {
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
		current := c.requestURL()
		if final.Host != current.Host || final.Scheme != current.Scheme {
			// Don't mutate EndpointURL — record the resolved endpoint in
			// a separate field so the user-typed URL stays available for
			// display/logging/telemetry. Path/userinfo carry over from
			// EndpointURL (same assumption as before: the redirect target
			// serves the same repo path).
			resolved := *c.EndpointURL
			resolved.Scheme = final.Scheme
			resolved.Host = final.Host
			c.resolvedEndpoint = &resolved
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
//
// On a 401 we consult the credential helper and retry, mirroring git's
// own behaviour for servers that allow anonymous /info/refs but gate the
// actual upload-pack/receive-pack POST behind auth. Retry is only possible
// when body is an io.Seeker (so we can rewind it); callers that pass a raw
// non-seekable Reader will see the 401 surface as-is.
func (c *HTTPConn) PostRPCStreamBody(ctx context.Context, service string, body io.Reader, v2 bool, phase string) (io.ReadCloser, error) {
	res, err := c.doPostRPCRequest(ctx, service, body, v2, phase, c.Auth, nil)
	if err != nil {
		return nil, err
	}
	if seeker, ok := body.(io.Seeker); ok {
		res, err = c.tryHelperRetry(ctx, res, func(auth AuthMethod, target *url.URL) (*http.Response, error) {
			if _, seekErr := seeker.Seek(0, io.SeekStart); seekErr != nil {
				return nil, fmt.Errorf("rewind RPC body for credential-helper retry: %w", seekErr)
			}
			return c.doPostRPCRequest(ctx, service, body, v2, phase, auth, target)
		})
		if err != nil {
			return nil, err
		}
	}
	httpErr := httpError(res)
	// Settle helper credentials on the validated status: approve on a 2xx,
	// reject on 401/403. For the POST path the HTTP status is the whole
	// success signal — there's no advertisement body to validate further.
	c.resolvePendingHelperCreds(ctx, res, httpErr == nil)
	if httpErr != nil {
		_ = res.Body.Close()
		return nil, httpErr
	}
	return res.Body, nil
}

// doPostRPCRequest issues a single POST to /<service>. Caller closes res.Body.
//
// target is an optional override URL: when non-nil, the request is sent
// verbatim to that URL instead of building one from c.EndpointURL. See
// doInfoRefsRequest for why — same redirect-strip avoidance.
func (c *HTTPConn) doPostRPCRequest(ctx context.Context, service string, body io.Reader, v2 bool, phase string, auth AuthMethod, target *url.URL) (*http.Response, error) {
	var reqURL string
	if target != nil {
		reqURL = target.String()
	} else {
		reqURL = fmt.Sprintf("%s/%s", c.requestURL().String(), service)
	}
	ctx = withHTTPTrace(ctx, "POST "+service)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, body)
	if err != nil {
		return nil, fmt.Errorf("create RPC request: %w", err)
	}
	req.Header.Set("Content-Type", fmt.Sprintf("application/x-%s-request", service))
	req.Header.Set("Accept", fmt.Sprintf("application/x-%s-result", service))
	req.Header.Set("User-Agent", useragent.GoGit())
	req.Header.Set(StatsPhaseHeader, phase)
	if v2 {
		req.Header.Set("Git-Protocol", GitProtocolV2)
	}
	ApplyAuth(req, auth)

	if httpTraceEnabled() {
		dumpOutgoingRequest(req, "POST "+service)
	}

	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post RPC: %w", err)
	}
	return res, nil
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

// EnsureAuthForService tentatively attaches helper credentials before a
// non-rewindable request body is committed. It's a no-op when no helper
// is configured, when Auth is already set, or when the helper has no
// credentials to offer.
//
// Used from push.go (and other streaming-body POST paths) where the body
// is built from a live upstream stream (e.g. io.MultiReader over a pack
// reader) and can't be replayed on a mid-stream 401.
//
// The flow is:
//  1. Probe with a POST to /<service> using the smart-HTTP flush packet
//     "0000" as body — a valid no-op (zero ref updates, zero pack data)
//     by spec. We probe with POST rather than GET because the auth layer
//     may only gate the POST handler; a GET probe would slip past on
//     servers that 404/405 GET while requiring auth on POST.
//  2. If the probe gets 401, ask the helper for credentials keyed on the
//     actually-challenged host (which may differ from c.EndpointURL after a
//     cross-host redirect). Keying on the post-redirect host matters: that's
//     where the user stored their creds, and that's the key we'll later
//     Approve/Reject against.
//  3. Attach the credentials tentatively. The next real operation calls
//     resolvePendingHelperCreds, which Approves them only once that
//     operation fully succeeds or Rejects them on 401/403 — helper state
//     only changes based on the actual outcome, never on the probe response
//     alone.
//  4. If the challenge came from a cross-host redirect, rewrite
//     c.EndpointURL's scheme/host to the challenger so the real op skips the
//     redirect (which Go's http.Client would otherwise follow with the
//     Authorization header stripped, turning every push into a fresh 401).
//
// If the probe doesn't 401 (200, 404, 405, etc.) we don't attach; the
// server either accepts anonymous POSTs here or returns ambiguously,
// and either way attaching unvalidated credentials could leak them.
func (c *HTTPConn) EnsureAuthForService(ctx context.Context, service string) {
	if c.Auth != nil || c.CredentialHelper == nil {
		return
	}
	res, err := c.doServiceProbe(ctx, service)
	if err != nil {
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		return
	}
	challengeURL := challengeURLFor(c.EndpointURL, res)
	if c.InsecureSkipTLSVerify && challengeURL.Host != c.requestURL().Host {
		// See tryHelperRetry: with TLS verification off we can't tell a
		// real challenger from a MITM, so we won't attach helper creds
		// after a cross-host probe redirect. The real op will surface
		// the 401 and the user can resolve it.
		return
	}
	user, pass, ok, lookupErr := c.CredentialHelper.Lookup(ctx, challengeURL)
	if lookupErr != nil || !ok {
		return
	}
	c.Auth = &transporthttp.BasicAuth{Username: user, Password: pass}
	c.pendingHelperCreds = &helperCreds{user: user, pass: pass, url: challengeURL}
	c.adoptChallengeHost(challengeURL)
}

// resolvePendingHelperCreds settles credentials that were attached tentatively
// — either by EnsureAuthForService's probe or by a tryHelperRetry that got a
// 2xx — based on the fully-validated outcome of a real operation. Called from
// RequestInfoRefs and PostRPCStreamBody. No-op if nothing is pending.
//
// success reports whether the operation actually succeeded (a 2xx whose body
// also passed any service-specific validation), as opposed to merely returning
// a 2xx status. We approve only on success, so a misleading 2xx — e.g. an
// /info/refs response with the wrong content-type or an oversized body — can't
// persist credentials for an operation the caller still reports as failed.
func (c *HTTPConn) resolvePendingHelperCreds(ctx context.Context, res *http.Response, success bool) {
	if c.pendingHelperCreds == nil || c.CredentialHelper == nil {
		return
	}
	creds := c.pendingHelperCreds
	switch {
	case success:
		c.pendingHelperCreds = nil
		c.CredentialHelper.Approve(ctx, creds.url, creds.user, creds.pass)
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		c.pendingHelperCreds = nil
		c.Auth = nil
		c.CredentialHelper.Reject(ctx, creds.url, creds.user, creds.pass)
	}
	// Otherwise (e.g. a 2xx with a malformed/oversized body): leave the creds
	// pending and c.Auth in place. We must not approve credentials for an
	// operation that failed validation, but a non-auth failure isn't proof
	// they're bad either. The conn is short-lived (one sync), so leftover
	// pending state at end of life is harmless.
}

// flushPacket is the smart-HTTP pkt-line "flush" marker. A request body
// containing only a flush packet is a valid no-op for both upload-pack
// (no wants/haves) and receive-pack (no ref updates, no pack data).
var flushPacket = []byte("0000")

func (c *HTTPConn) doServiceProbe(ctx context.Context, service string) (*http.Response, error) {
	reqURL := fmt.Sprintf("%s/%s", c.requestURL().String(), service)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(flushPacket))
	if err != nil {
		return nil, fmt.Errorf("create auth-probe request: %w", err)
	}
	req.Header.Set("Content-Type", fmt.Sprintf("application/x-%s-request", service))
	req.Header.Set("Accept", fmt.Sprintf("application/x-%s-result", service))
	req.Header.Set("User-Agent", useragent.GoGit())
	req.Header.Set(StatsPhaseHeader, service+" auth-probe")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth-probe request: %w", err)
	}
	return res, nil
}

// tryHelperRetry handles the 401 → lookup → retry → approve/reject lifecycle
// when a CredentialHelper is configured and no explicit Auth was set up front
// (explicit auth must surface its own failures rather than be quietly papered
// over). retry attempts the same request with helper-supplied credentials,
// targeted at the URL that actually returned the 401 (which may differ from
// c.EndpointURL if Go's http.Client followed a cross-host redirect).
//
// On a 2xx retry the credentials are stored on c.Auth (so follow-up calls on
// the same connection reuse them) and recorded as pending — the caller then
// approves them via resolvePendingHelperCreds once the response passes full
// validation, never on the 2xx status alone. If the challenge was on a host
// different from c.EndpointURL we also rewrite c.EndpointURL's scheme/host to
// the challenger so subsequent ops on this conn don't redirect again (Go's
// http.Client strips Authorization on cross-host redirects, which would
// otherwise turn every follow-up into a fresh 401 → Reject of valid creds).
//
// On retry failure (401, 403, or transport error) the helper is told to
// reject the credentials immediately so a stale stored token self-heals on
// the next run.
//
// Caller is responsible for closing the returned response body.
func (c *HTTPConn) tryHelperRetry(ctx context.Context, res *http.Response, retry func(AuthMethod, *url.URL) (*http.Response, error)) (*http.Response, error) {
	if res.StatusCode != http.StatusUnauthorized || c.Auth != nil || c.CredentialHelper == nil {
		return res, nil
	}
	challengeURL := challengeURLFor(c.EndpointURL, res)
	if c.InsecureSkipTLSVerify && challengeURL.Host != c.requestURL().Host {
		// Refuse to hand helper-stored credentials to a redirect target we
		// can't authenticate. With TLS verification off, the post-redirect
		// host could be anyone presenting a self-signed cert for the host
		// our Lookup key would hand creds to. Let the 401 surface so the
		// user fixes their setup (don't combine SkipTLSVerify with a
		// credential helper on a redirecting endpoint).
		return res, nil
	}
	user, pass, ok, lookupErr := c.CredentialHelper.Lookup(ctx, challengeURL)
	if lookupErr != nil {
		_ = res.Body.Close()
		return nil, fmt.Errorf("look up credentials: %w", lookupErr)
	}
	if !ok {
		return res, nil
	}
	// Capture the actually-challenged URL — what http.Client landed on after
	// any redirects, including its path/query — so the retry hits it directly
	// instead of replaying through c.EndpointURL and getting Authorization
	// stripped on the cross-host hop.
	retryTarget := res.Request.URL
	_ = res.Body.Close()
	retryAuth := &transporthttp.BasicAuth{Username: user, Password: pass}
	res, err := retry(retryAuth, retryTarget)
	if err != nil {
		c.CredentialHelper.Reject(ctx, challengeURL, user, pass)
		return nil, err
	}
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		// 403 included because some token services (e.g. Cloudflare)
		// surface "Invalid or expired token" as 403 rather than 401.
		c.CredentialHelper.Reject(ctx, challengeURL, user, pass)
	case res.StatusCode >= http.StatusOK && res.StatusCode < http.StatusMultipleChoices:
		// Attach tentatively and defer approval to resolvePendingHelperCreds,
		// which runs after the caller validates the response body — a 2xx
		// status alone isn't proof the operation succeeded.
		c.Auth = retryAuth
		c.pendingHelperCreds = &helperCreds{user: user, pass: pass, url: challengeURL}
		c.adoptChallengeHost(challengeURL)
	}
	return res, nil
}

// adoptChallengeHost records the host that just successfully authenticated
// as this conn's resolved endpoint, so subsequent ops on the same conn go
// there directly instead of replaying through c.EndpointURL — which would
// redirect again and have its Authorization header stripped on the cross-
// host hop, turning every follow-up into a fresh 401.
//
// Gated on FollowInfoRefsRedirect: the user explicitly opting into redirect-
// following is the trigger for the conn's effective endpoint changing. With
// the flag off the immediate retry still hits the challenger directly (so
// the current op succeeds and creds get Approved on the right key), but the
// next op stays pointed at the user-typed URL. EndpointURL itself is never
// mutated; we set resolvedEndpoint instead, leaving EndpointURL as user
// input for display/logging/telemetry to read.
//
// Path/userinfo are copied from EndpointURL — same assumption
// FollowInfoRefsRedirect's /info/refs block makes about the redirect target
// serving the same repo path.
func (c *HTTPConn) adoptChallengeHost(challengeURL *url.URL) {
	if !c.FollowInfoRefsRedirect || challengeURL == nil {
		return
	}
	current := c.requestURL()
	if challengeURL.Host == current.Host && challengeURL.Scheme == current.Scheme {
		return
	}
	resolved := *c.EndpointURL
	resolved.Scheme = challengeURL.Scheme
	resolved.Host = challengeURL.Host
	c.resolvedEndpoint = &resolved
}

// challengeURLFor returns the URL key used to query the credential helper
// for an auth challenge. After a 3xx the actually-challenged host is in
// res.Request.URL, which may differ from c.EndpointURL — using the wrong
// one would query (and possibly approve/reject) credentials under the
// wrong helper key. The original repo path is preserved so the key still
// matches what the user configured.
//
// We deliberately key on orig.Path rather than res.Request.URL.Path so
// credentials the user stored against the URL they typed remain findable
// when a redirect rewrites paths (e.g. github.com/owner/repo redirected
// to cdn.example/mirror/owner/repo). Trade-off: for path-aware helpers
// (credential.useHttpPath=true) the Approve/Reject key may be less
// precise than the actual challenge URL. Not a credential leak — creds
// only ever reach hosts the user already trusted enough to store them
// against — just a helper-audit-trail imprecision.
func challengeURLFor(orig *url.URL, res *http.Response) *url.URL {
	if res == nil || res.Request == nil || res.Request.URL == nil {
		return orig
	}
	final := res.Request.URL
	if final.Host == orig.Host && final.Scheme == orig.Scheme {
		return orig
	}
	out := *orig
	out.Scheme = final.Scheme
	out.Host = final.Host
	return &out
}

// doInfoRefsRequest issues a single /info/refs GET. Caller closes res.Body.
//
// target is an optional override URL: when non-nil, the request is sent
// verbatim to that URL instead of building one from c.EndpointURL. Used by
// the credential-helper retry path to hit a redirected challenge host
// directly, skipping the redirect that would otherwise cause Go's
// http.Client to strip the Authorization header on the cross-host hop.
func (c *HTTPConn) doInfoRefsRequest(ctx context.Context, service, gitProtocol string, auth AuthMethod, target *url.URL) (*http.Response, error) {
	var reqURL string
	if target != nil {
		reqURL = target.String()
	} else {
		reqURL = fmt.Sprintf("%s/info/refs?service=%s", c.requestURL().String(), service)
	}
	ctx = withHTTPTrace(ctx, "GET "+service+"/info/refs")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create info-refs request: %w", err)
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", useragent.GoGit())
	req.Header.Set(StatsPhaseHeader, service+" info-refs")
	if gitProtocol != "" {
		req.Header.Set("Git-Protocol", gitProtocol)
	}
	ApplyAuth(req, auth)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request info-refs: %w", err)
	}
	return res, nil
}
