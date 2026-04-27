package gitproto

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing/transport"
	transporthttp "github.com/go-git/go-git/v6/plumbing/transport/http"
)

func TestNewConn(t *testing.T) {
	ep, err := transport.NewEndpoint("https://github.com/user/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	auth := &transporthttp.BasicAuth{Username: "user", Password: "pass"}
	conn := NewConn(ep, "test-label", auth, http.DefaultTransport)

	if conn.Label != "test-label" {
		t.Errorf("Label = %q, want %q", conn.Label, "test-label")
	}
	if conn.Endpoint != ep {
		t.Error("Endpoint mismatch")
	}
	if conn.Auth != auth {
		t.Error("Auth mismatch")
	}
	if conn.HTTP == nil {
		t.Error("HTTP client should not be nil")
	}
	if conn.Transport == nil {
		t.Error("Transport should not be nil")
	}
}

func TestNewHTTPTransport(t *testing.T) {
	// Without TLS skip should return default transport.
	rt := NewHTTPTransport(false)
	if rt != http.DefaultTransport {
		t.Error("expected http.DefaultTransport when skipTLS is false")
	}

	// With TLS skip should return a transport with InsecureSkipVerify.
	rt = NewHTTPTransport(true)
	if rt == http.DefaultTransport {
		t.Error("expected a different transport when skipTLS is true")
	}
	// Verify the returned transport is an *http.Transport with skip verify.
	if ht, ok := rt.(*http.Transport); ok {
		if ht.TLSClientConfig == nil || !ht.TLSClientConfig.InsecureSkipVerify {
			t.Error("expected InsecureSkipVerify = true")
		}
	}
}

func TestApplyAuth(t *testing.T) {
	// BasicAuth
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	auth := &transporthttp.BasicAuth{Username: "user", Password: "pass"}
	ApplyAuth(req, auth)
	user, pass, ok := req.BasicAuth()
	if !ok || user != "user" || pass != "pass" {
		t.Errorf("BasicAuth not applied: ok=%v user=%q pass=%q", ok, user, pass)
	}

	// TokenAuth
	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	tokenAuth := &transporthttp.TokenAuth{Token: "my-token"}
	ApplyAuth(req, tokenAuth)
	got := req.Header.Get("Authorization")
	if got == "" {
		t.Error("TokenAuth not applied: Authorization header is empty")
	}

	// nil auth should not panic.
	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	ApplyAuth(req, nil)
}

func TestRequestInfoRefsContextCanceled(t *testing.T) {
	started := make(chan struct{}, 1)
	ep, err := transport.NewEndpoint("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	conn := NewConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := RequestInfoRefs(ctx, conn, "git-upload-pack", "version=2")
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach server before timeout")
	}
	cancel()

	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not return after cancellation")
	}
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestPostRPCStreamContextCanceled(t *testing.T) {
	started := make(chan struct{}, 1)
	ep, err := transport.NewEndpoint("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	conn := NewConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := PostRPCStream(ctx, conn, "git-upload-pack", []byte("0000"), true, "upload-pack fetch")
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach server before timeout")
	}
	cancel()

	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not return after cancellation")
	}
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestRequestInfoRefs_AfterInfoRefsHook_PinsToReturnedURL verifies that when
// AfterInfoRefs returns a non-nil URL, Conn.Endpoint.Scheme and .Host are
// rewritten to those of the returned URL so subsequent PostRPC calls target
// it. The hook is the generic mechanism callers use to implement
// discovery-aware redirection — header-based replica advertisement and the
// vanilla-git post-redirect case are both expressible as hook
// implementations.
func TestRequestInfoRefs_AfterInfoRefsHook_PinsToReturnedURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		if _, err := w.Write([]byte("001e# service=git-upload-pack\n0000")); err != nil {
			t.Errorf("server write: %v", err)
		}
	}))
	defer server.Close()

	ep, err := transport.NewEndpoint(server.URL + "/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	conn := NewConn(ep, "test", nil, http.DefaultTransport)
	conn.AfterInfoRefs = func(*http.Response) *url.URL {
		return &url.URL{Scheme: "https", Host: "node3.example:443"}
	}

	if _, err := RequestInfoRefs(t.Context(), conn, transport.UploadPackService, ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}

	if conn.Endpoint.Host != "node3.example:443" {
		t.Errorf("Endpoint.Host = %q, want %q (hook return should pin endpoint host)", conn.Endpoint.Host, "node3.example:443")
	}
	if conn.Endpoint.Scheme != "https" {
		t.Errorf("Endpoint.Scheme = %q, want %q", conn.Endpoint.Scheme, "https")
	}
}

// TestRequestInfoRefs_AfterInfoRefsHook_NilReturnLeavesEndpointUnchanged
// verifies that a hook returning nil is the explicit "no change" signal —
// the endpoint stays as-is even though the hook fired.
func TestRequestInfoRefs_AfterInfoRefsHook_NilReturnLeavesEndpointUnchanged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		if _, err := w.Write([]byte("001e# service=git-upload-pack\n0000")); err != nil {
			t.Errorf("server write: %v", err)
		}
	}))
	defer server.Close()

	ep, err := transport.NewEndpoint(server.URL + "/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	originalHost := ep.Host
	conn := NewConn(ep, "test", nil, http.DefaultTransport)
	conn.AfterInfoRefs = func(*http.Response) *url.URL { return nil }

	if _, err := RequestInfoRefs(t.Context(), conn, transport.UploadPackService, ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}

	if conn.Endpoint.Host != originalHost {
		t.Errorf("Endpoint.Host = %q, want %q (nil hook return should not pin)", conn.Endpoint.Host, originalHost)
	}
}

// TestRequestInfoRefs_AfterInfoRefsHook_BodyIsReadable pins the contract
// that the hook can safely read res.Body without affecting RequestInfoRefs's
// return value. The body is buffered before the hook fires and presented
// via a fresh reader, so caller order doesn't matter.
func TestRequestInfoRefs_AfterInfoRefsHook_BodyIsReadable(t *testing.T) {
	const advertisement = "001e# service=git-upload-pack\n0000"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		if _, err := w.Write([]byte(advertisement)); err != nil {
			t.Errorf("server write: %v", err)
		}
	}))
	defer server.Close()

	ep, err := transport.NewEndpoint(server.URL + "/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	var hookBody []byte
	conn := NewConn(ep, "test", nil, http.DefaultTransport)
	conn.AfterInfoRefs = func(res *http.Response) *url.URL {
		data, err := io.ReadAll(res.Body)
		if err != nil {
			t.Errorf("hook read body: %v", err)
		}
		hookBody = data
		return nil
	}

	data, err := RequestInfoRefs(t.Context(), conn, transport.UploadPackService, "")
	if err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}
	if string(hookBody) != advertisement {
		t.Errorf("hook body = %q, want %q", hookBody, advertisement)
	}
	if string(data) != advertisement {
		t.Errorf("RequestInfoRefs body = %q, want %q", data, advertisement)
	}
}

// TestRequestInfoRefs_AfterInfoRefsHook_SubsequentPOSTHitsHookHost is the
// reviewer-requested integration test: GET /info/refs → hook returns the
// hosting node's URL → subsequent POST /git-upload-pack lands on that node,
// not on the originally-configured entry domain.
func TestRequestInfoRefs_AfterInfoRefsHook_SubsequentPOSTHitsHookHost(t *testing.T) {
	var nodeGotPOST bool
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
			nodeGotPOST = true
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("node: unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer node.Close()

	var entryGotPOST bool
	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs"):
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			if _, err := w.Write([]byte("001e# service=git-upload-pack\n0000")); err != nil {
				t.Errorf("entry info/refs write: %v", err)
			}
		case r.Method == http.MethodPost:
			entryGotPOST = true
			http.Error(w, "LB rejects packs", http.StatusMethodNotAllowed)
		default:
			http.NotFound(w, r)
		}
	}))
	defer entry.Close()

	ep, err := transport.NewEndpoint(entry.URL + "/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	nodeParsed, err := url.Parse(node.URL)
	if err != nil {
		t.Fatalf("parse node URL: %v", err)
	}
	conn := NewConn(ep, "test", nil, http.DefaultTransport)
	conn.AfterInfoRefs = func(*http.Response) *url.URL { return nodeParsed }

	if _, err := RequestInfoRefs(t.Context(), conn, transport.UploadPackService, ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}

	if _, err := PostRPC(t.Context(), conn, transport.UploadPackService, []byte("0000"), false, "upload-pack integration-test"); err != nil {
		t.Fatalf("PostRPC: %v", err)
	}
	if entryGotPOST {
		t.Error("POST hit the entry domain instead of the hook-returned host")
	}
	if !nodeGotPOST {
		t.Error("POST did not hit the hook-returned host")
	}
}

// TestRequestInfoRefs_NoHookByDefault confirms the default behaviour:
// without an AfterInfoRefs hook the endpoint is stable even if the server
// 307s. http.Client still follows the redirect for the GET itself, but
// subsequent RPCs target the originally-configured host.
func TestRequestInfoRefs_NoHookByDefault(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		if _, err := w.Write([]byte("001e# service=git-upload-pack\n0000")); err != nil {
			t.Errorf("node write: %v", err)
		}
	}))
	defer node.Close()

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, node.URL+r.URL.Path+"?"+r.URL.RawQuery, http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	ep, err := transport.NewEndpoint(entry.URL + "/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	entryHost := ep.Host
	conn := NewConn(ep, "test", nil, http.DefaultTransport)
	// AfterInfoRefs intentionally not set.

	if _, err := RequestInfoRefs(t.Context(), conn, transport.UploadPackService, ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}

	if conn.Endpoint.Host != entryHost {
		t.Errorf("Endpoint.Host = %q, want %q (endpoint should be unchanged when no hook)", conn.Endpoint.Host, entryHost)
	}
}

func TestHTTPErrorBoundsBodyRead(t *testing.T) {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/repo.git", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}

	body := &roundTripReader{remaining: maxHTTPErrorBody + 4096}
	res := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Request:    req,
		Body:       body,
	}

	err = httpError(res)
	if err == nil {
		t.Fatal("expected error")
	}
	if len(err.Error()) > maxHTTPErrorBody+128 {
		t.Fatalf("error body was not bounded, len=%d", len(err.Error()))
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type roundTripReader struct {
	remaining int
}

func (r *roundTripReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	for i := range n {
		p[i] = 'x'
	}
	r.remaining -= n
	return n, nil
}

func (r *roundTripReader) Close() error { return nil }
