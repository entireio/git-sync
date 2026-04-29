package gitproto

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing/transport"
	transporthttp "github.com/go-git/go-git/v6/plumbing/transport/http"
)

func TestNewConn(t *testing.T) {
	ep, err := transport.ParseURL("https://github.com/user/repo.git")
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
	ep, err := transport.ParseURL("https://example.com/repo.git")
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

func TestRequestInfoRefsRequiresAdvertisementContentType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		wantErr     bool
	}{
		{
			name:    "missing content type",
			wantErr: true,
		},
		{
			name:        "wrong content type",
			contentType: "text/plain",
			wantErr:     true,
		},
		{
			name:        "wrong service advertisement",
			contentType: "application/x-git-receive-pack-advertisement",
			wantErr:     true,
		},
		{
			name:        "expected content type",
			contentType: "application/x-git-upload-pack-advertisement",
		},
		{
			name:        "expected content type with parameter",
			contentType: "application/x-git-upload-pack-advertisement; charset=utf-8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep, err := transport.ParseURL("https://example.com/repo.git")
			if err != nil {
				t.Fatalf("parse endpoint: %v", err)
			}
			conn := NewConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				res := &http.Response{
					StatusCode: http.StatusOK,
					Request:    req,
					Body:       io.NopCloser(strings.NewReader("0000")),
					Header:     make(http.Header),
				}
				if tt.contentType != "" {
					res.Header.Set("Content-Type", tt.contentType)
				}
				return res, nil
			}))

			body, err := RequestInfoRefs(t.Context(), conn, transport.UploadPackService, "")
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected content type error")
				}
				if !strings.Contains(err.Error(), "unexpected info/refs content-type") {
					t.Fatalf("error = %v, want content type error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("RequestInfoRefs: %v", err)
			}
			if got, want := string(body), "0000"; got != want {
				t.Fatalf("body = %q, want %q", got, want)
			}
		})
	}
}

func TestPostRPCStreamContextCanceled(t *testing.T) {
	started := make(chan struct{}, 1)
	ep, err := transport.ParseURL("https://example.com/repo.git")
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

// TestRequestInfoRefs_FollowInfoRefsRedirect verifies that when the flag is
// set, a 307 on /info/refs rewrites Conn.Endpoint.Host so subsequent PostRPC
// calls target the redirected node. Matches vanilla git's smart-HTTP
// behaviour and lets clients use a cluster entry domain for info/refs while
// packs land on the hosting replica.
func TestRequestInfoRefs_FollowInfoRefsRedirect(t *testing.T) {
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

	ep, err := transport.ParseURL(entry.URL + "/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	conn := NewConn(ep, "test", nil, http.DefaultTransport)
	conn.FollowInfoRefsRedirect = true

	if _, err := RequestInfoRefs(t.Context(), conn, transport.UploadPackService, ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}

	nodeURL := strings.TrimPrefix(node.URL, "http://")
	if conn.Endpoint.Host != nodeURL {
		t.Errorf("Endpoint.Host = %q, want %q (endpoint should follow the 307)", conn.Endpoint.Host, nodeURL)
	}
}

// TestRequestInfoRefs_FollowInfoRefsRedirect_SubsequentPOSTHitsRedirectedHost
// is the reviewer-requested integration test: it runs the full sequence
// (GET /info/refs → 307 → 200 on hosting node → POST /git-upload-pack) and
// asserts the POST lands on the hosting node, not the entry domain. This is
// the property that makes the flag useful — the whole point is that packs
// follow info/refs.
func TestRequestInfoRefs_FollowInfoRefsRedirect_SubsequentPOSTHitsRedirectedHost(t *testing.T) {
	var nodeGotPOST bool
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs"):
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			if _, err := w.Write([]byte("001e# service=git-upload-pack\n0000")); err != nil {
				t.Errorf("node info/refs write: %v", err)
			}
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
		if r.Method == http.MethodPost {
			entryGotPOST = true
			http.Error(w, "LB rejects packs", http.StatusMethodNotAllowed)
			return
		}
		http.Redirect(w, r, node.URL+r.URL.Path+"?"+r.URL.RawQuery, http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	ep, err := transport.ParseURL(entry.URL + "/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	conn := NewConn(ep, "test", nil, http.DefaultTransport)
	conn.FollowInfoRefsRedirect = true

	if _, err := RequestInfoRefs(t.Context(), conn, transport.UploadPackService, ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}

	// Now do the follow-up upload-pack POST. Without the flag this hits the
	// entry domain (rejected with 405); with the flag it hits the node.
	body, err := PostRPC(t.Context(), conn, transport.UploadPackService, []byte("0000"), false, "upload-pack integration-test")
	if err != nil {
		t.Fatalf("PostRPC: %v", err)
	}
	if len(body) != 0 {
		// body shape is not what we're asserting; just demand it didn't fail
		_ = body
	}

	if entryGotPOST {
		t.Error("POST hit the entry domain instead of the redirected node")
	}
	if !nodeGotPOST {
		t.Error("POST did not hit the redirected node")
	}
}

// TestRequestInfoRefs_DoesNotFollowByDefault confirms the default behaviour
// is unchanged: Endpoint is stable even if the server 307s.
func TestRequestInfoRefs_DoesNotFollowByDefault(t *testing.T) {
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

	ep, err := transport.ParseURL(entry.URL + "/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	entryHost := ep.Host
	conn := NewConn(ep, "test", nil, http.DefaultTransport)
	// FollowInfoRefsRedirect intentionally not set.

	if _, err := RequestInfoRefs(t.Context(), conn, transport.UploadPackService, ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}

	if conn.Endpoint.Host != entryHost {
		t.Errorf("Endpoint.Host = %q, want %q (endpoint should be unchanged by default)", conn.Endpoint.Host, entryHost)
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
