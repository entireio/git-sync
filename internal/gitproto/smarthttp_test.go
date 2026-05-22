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

func TestNewHTTPConn(t *testing.T) {
	ep, err := transport.ParseURL("https://github.com/user/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	auth := &transporthttp.BasicAuth{Username: "user", Password: "pass"}
	conn := NewHTTPConn(ep, "test-label", auth, http.DefaultTransport)

	if conn.Label != "test-label" {
		t.Errorf("Label = %q, want %q", conn.Label, "test-label")
	}
	if conn.EndpointURL != ep {
		t.Error("EndpointURL mismatch")
	}
	if conn.Auth != auth {
		t.Error("Auth mismatch")
	}
	if conn.HTTP == nil {
		t.Error("HTTP client should not be nil")
	}
}

func TestNewHTTPConnStripsTrailingEndpointSlash(t *testing.T) {
	ep, err := url.Parse("https://example.com/repo.git///")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	var gotURLs []string
	conn := NewHTTPConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		gotURLs = append(gotURLs, req.URL.String())
		res := &http.Response{
			StatusCode: http.StatusOK,
			Request:    req,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
		}
		if req.Method == http.MethodGet {
			res.Header.Set("Content-Type", "application/x-git-upload-pack-advertisement")
			res.Body = io.NopCloser(strings.NewReader("0000"))
		}
		return res, nil
	}))

	if got, want := conn.EndpointURL.Path, "/repo.git"; got != want {
		t.Fatalf("EndpointURL.Path = %q, want %q", got, want)
	}
	if _, err := RequestInfoRefs(t.Context(), conn, transport.UploadPackService, ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}
	if _, err := PostRPC(t.Context(), conn, transport.UploadPackService, []byte("0000"), false, "upload-pack test"); err != nil {
		t.Fatalf("PostRPC: %v", err)
	}

	wantURLs := []string{
		"https://example.com/repo.git/info/refs?service=git-upload-pack",
		"https://example.com/repo.git/git-upload-pack",
	}
	if len(gotURLs) != len(wantURLs) {
		t.Fatalf("got %d request URLs, want %d: %v", len(gotURLs), len(wantURLs), gotURLs)
	}
	for i := range wantURLs {
		if gotURLs[i] != wantURLs[i] {
			t.Fatalf("request URL %d = %q, want %q", i, gotURLs[i], wantURLs[i])
		}
	}
}

func TestNewHTTPTransport(t *testing.T) {
	// Default (no TLS skip) returns a cloned transport, not the shared
	// http.DefaultTransport — config must not leak into other code.
	rt := NewHTTPTransport(false)
	if rt == http.DefaultTransport {
		t.Error("expected a cloned transport, got shared http.DefaultTransport")
	}
	ht, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", rt)
	}
	if !ht.DisableKeepAlives {
		t.Error("expected DisableKeepAlives = true on the default transport")
	}

	// With TLS skip we still get a cloned transport with keep-alives off,
	// plus InsecureSkipVerify on the TLS config.
	rt = NewHTTPTransport(true)
	if rt == http.DefaultTransport {
		t.Error("expected a cloned transport when skipTLS is true")
	}
	ht, ok = rt.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", rt)
	}
	if !ht.DisableKeepAlives {
		t.Error("expected DisableKeepAlives = true when skipTLS is true")
	}
	if ht.TLSClientConfig == nil || !ht.TLSClientConfig.InsecureSkipVerify {
		t.Error("expected InsecureSkipVerify = true when skipTLS is true")
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
	conn := NewHTTPConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := RequestInfoRefs(ctx, conn, "git-upload-pack", GitProtocolV2)
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
			conn := NewHTTPConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
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
	conn := NewHTTPConn(ep, "source", nil, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
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
// set, a 307 on /info/refs rewrites HTTPConn.EndpointURL.Host so subsequent PostRPC
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
	conn := NewHTTPConn(ep, "test", nil, http.DefaultTransport)
	conn.FollowInfoRefsRedirect = true

	if _, err := RequestInfoRefs(t.Context(), conn, transport.UploadPackService, ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}

	nodeURL := strings.TrimPrefix(node.URL, "http://")
	if conn.EndpointURL.Host != nodeURL {
		t.Errorf("EndpointURL.Host = %q, want %q (endpoint should follow the 307)", conn.EndpointURL.Host, nodeURL)
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
	conn := NewHTTPConn(ep, "test", nil, http.DefaultTransport)
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
// is unchanged: EndpointURL is stable even if the server 307s.
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
	conn := NewHTTPConn(ep, "test", nil, http.DefaultTransport)
	// FollowInfoRefsRedirect intentionally not set.

	if _, err := RequestInfoRefs(t.Context(), conn, transport.UploadPackService, ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}

	if conn.EndpointURL.Host != entryHost {
		t.Errorf("EndpointURL.Host = %q, want %q (endpoint should be unchanged by default)", conn.EndpointURL.Host, entryHost)
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

func newAdvertisementResponse(req *http.Request) *http.Response {
	res := &http.Response{
		StatusCode: http.StatusOK,
		Request:    req,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("0000")),
	}
	res.Header.Set("Content-Type", "application/x-git-upload-pack-advertisement")
	return res
}

func newUnauthorizedResponse(req *http.Request) *http.Response {
	res := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Request:    req,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("authentication required")),
	}
	res.Header.Set("WWW-Authenticate", `Basic realm="git"`)
	return res
}

func newTestConn(_ *testing.T, rt http.RoundTripper) *HTTPConn {
	return NewHTTPConn(
		&url.URL{Scheme: "https", Host: "example.com", Path: "/repo.git"},
		"src", nil, rt,
	)
}

func TestRequestInfoRefs_AnonymousSucceedsWithoutConsultingHelper(t *testing.T) {
	helper := &fakeCredentialHelper{user: "x", pass: "y", ok: true}
	var authHeaders []string
	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		authHeaders = append(authHeaders, req.Header.Get("Authorization"))
		return newAdvertisementResponse(req), nil
	}))
	conn.CredentialHelper = helper

	if _, err := conn.RequestInfoRefs(context.Background(), "git-upload-pack", ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}
	if helper.count("lookup") != 0 {
		t.Errorf("expected 0 helper lookups on anonymous success, got %d", helper.count("lookup"))
	}
	if len(authHeaders) != 1 || authHeaders[0] != "" {
		t.Errorf("expected exactly one anonymous request, got headers %v", authHeaders)
	}
}

func TestRequestInfoRefs_OnUnauthorizedRetriesWithHelperCredentials(t *testing.T) {
	helper := &fakeCredentialHelper{user: "alice", pass: "s3cret", ok: true}

	var authHeaders []string
	attempts := 0
	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		authHeaders = append(authHeaders, req.Header.Get("Authorization"))
		attempts++
		if attempts == 1 {
			return newUnauthorizedResponse(req), nil
		}
		return newAdvertisementResponse(req), nil
	}))
	conn.CredentialHelper = helper

	if _, err := conn.RequestInfoRefs(context.Background(), "git-upload-pack", ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}

	if got := helper.count("lookup"); got != 1 {
		t.Errorf("expected 1 helper lookup, got %d", got)
	}
	if got := helper.count("approve"); got != 1 {
		t.Errorf("expected 1 approve call, got %d", got)
	}
	if got := helper.count("reject"); got != 0 {
		t.Errorf("expected 0 reject calls, got %d", got)
	}
	if last := helper.last("approve"); last == nil || last.user != "alice" || last.pass != "s3cret" {
		t.Errorf("approve called with wrong creds: %+v", last)
	}
	if len(authHeaders) != 2 {
		t.Fatalf("expected 2 requests (anon then auth retry), got %d: %v", len(authHeaders), authHeaders)
	}
	if authHeaders[0] != "" {
		t.Errorf("first request should be anonymous, got Authorization=%q", authHeaders[0])
	}
	if !strings.HasPrefix(authHeaders[1], "Basic ") {
		t.Errorf("retry should have Basic auth header, got %q", authHeaders[1])
	}
	if conn.Auth == nil {
		t.Error("expected conn.Auth to be stored after successful auth retry")
	}
}

func TestRequestInfoRefs_OnUnauthorizedReusesStoredAuthOnNextCall(t *testing.T) {
	helper := &fakeCredentialHelper{user: "alice", pass: "s3cret", ok: true}

	var authHeaders []string
	attempts := 0
	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		authHeaders = append(authHeaders, req.Header.Get("Authorization"))
		attempts++
		if attempts == 1 {
			return newUnauthorizedResponse(req), nil
		}
		if req.Method == http.MethodGet {
			return newAdvertisementResponse(req), nil
		}
		res := &http.Response{
			StatusCode: http.StatusOK,
			Request:    req,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}
		res.Header.Set("Content-Type", "application/x-git-upload-pack-result")
		return res, nil
	}))
	conn.CredentialHelper = helper

	if _, err := conn.RequestInfoRefs(context.Background(), "git-upload-pack", ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}
	if _, err := PostRPC(context.Background(), conn, "git-upload-pack", []byte("0000"), false, "phase"); err != nil {
		t.Fatalf("PostRPC: %v", err)
	}

	if got := helper.count("lookup"); got != 1 {
		t.Errorf("expected only 1 helper lookup across both requests, got %d", got)
	}
	if len(authHeaders) != 3 {
		t.Fatalf("expected 3 requests, got %d: %v", len(authHeaders), authHeaders)
	}
	if authHeaders[1] == "" || authHeaders[2] == "" {
		t.Errorf("retry GET and follow-up POST should both carry auth: %v", authHeaders)
	}
}

func TestRequestInfoRefs_OnUnauthorizedSurfaces401WithoutHelper(t *testing.T) {
	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return newUnauthorizedResponse(req), nil
	}))

	_, err := conn.RequestInfoRefs(context.Background(), "git-upload-pack", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 error, got %v", err)
	}
}

func TestRequestInfoRefs_OnUnauthorizedSurfaces401WhenHelperHasNoCredentials(t *testing.T) {
	helper := &fakeCredentialHelper{ok: false}
	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return newUnauthorizedResponse(req), nil
	}))
	conn.CredentialHelper = helper

	_, err := conn.RequestInfoRefs(context.Background(), "git-upload-pack", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 error, got %v", err)
	}
	if got := helper.count("lookup"); got != 1 {
		t.Errorf("expected 1 lookup attempt, got %d", got)
	}
	if got := helper.count("approve") + helper.count("reject"); got != 0 {
		t.Errorf("expected no approve/reject when helper had no creds, got %d", got)
	}
}

func TestRequestInfoRefs_OnUnauthorizedRetryStill401CallsReject(t *testing.T) {
	helper := &fakeCredentialHelper{user: "alice", pass: "bad", ok: true}
	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return newUnauthorizedResponse(req), nil
	}))
	conn.CredentialHelper = helper

	_, err := conn.RequestInfoRefs(context.Background(), "git-upload-pack", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := helper.count("reject"); got != 1 {
		t.Errorf("expected 1 reject call, got %d", got)
	}
	if got := helper.count("approve"); got != 0 {
		t.Errorf("expected 0 approve calls, got %d", got)
	}
	if last := helper.last("reject"); last == nil || last.user != "alice" || last.pass != "bad" {
		t.Errorf("reject called with wrong creds: %+v", last)
	}
}

// TestRequestInfoRefs_OnUnauthorizedRetry403CallsReject documents that some
// token services (notably Cloudflare) return 403 "Invalid or expired token"
// instead of 401 when stored credentials have expired.
func TestRequestInfoRefs_OnUnauthorizedRetry403CallsReject(t *testing.T) {
	helper := &fakeCredentialHelper{user: "user", pass: "expired-token", ok: true}
	attempts := 0
	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return newUnauthorizedResponse(req), nil
		}
		res := &http.Response{
			StatusCode: http.StatusForbidden,
			Request:    req,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("Invalid or expired token")),
		}
		return res, nil
	}))
	conn.CredentialHelper = helper

	_, err := conn.RequestInfoRefs(context.Background(), "git-upload-pack", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := helper.count("reject"); got != 1 {
		t.Errorf("expected 1 reject call on retry 403, got %d", got)
	}
	if got := helper.count("approve"); got != 0 {
		t.Errorf("expected 0 approve calls, got %d", got)
	}
	if last := helper.last("reject"); last == nil || last.user != "user" || last.pass != "expired-token" {
		t.Errorf("reject called with wrong creds: %+v", last)
	}
}

// TestRequestInfoRefs_DoesNotRetryWhenConnAlreadyAuthenticated: explicit auth
// must win over the helper, so users debugging a bad token they passed see
// the real error rather than a silent fallback.
func TestRequestInfoRefs_DoesNotRetryWhenConnAlreadyAuthenticated(t *testing.T) {
	helper := &fakeCredentialHelper{user: "alice", pass: "s3cret", ok: true}
	initialAuth := &transporthttp.BasicAuth{Username: "explicit", Password: "tok"}

	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return newUnauthorizedResponse(req), nil
	}))
	conn.Auth = initialAuth
	conn.CredentialHelper = helper

	_, err := conn.RequestInfoRefs(context.Background(), "git-upload-pack", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := helper.count("lookup"); got != 0 {
		t.Errorf("expected 0 helper lookups when auth was preconfigured, got %d", got)
	}
}

// TestRequestInfoRefs_OnUnauthorizedAfterRedirectKeysHelperOnFinalHost
// covers the case where /info/refs is 307'd to a different host and the
// replica returns 401: the helper must be queried for the host that
// actually challenged us, not the original endpoint.
func TestRequestInfoRefs_OnUnauthorizedAfterRedirectKeysHelperOnFinalHost(t *testing.T) {
	helper := &fakeCredentialHelper{user: "alice", pass: "s3cret", ok: true}
	attempts := 0
	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			// Simulate that Go's HTTP client followed a 3xx to replica.example
			// before getting the 401 — res.Request.URL is the post-redirect URL.
			res := newUnauthorizedResponse(req)
			res.Request = &http.Request{URL: &url.URL{
				Scheme: "https", Host: "replica.example", Path: "/repo.git/info/refs",
			}}
			return res, nil
		}
		return newAdvertisementResponse(req), nil
	}))
	conn.CredentialHelper = helper

	if _, err := conn.RequestInfoRefs(context.Background(), "git-upload-pack", ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}

	lookup := helper.last("lookup")
	if lookup == nil {
		t.Fatal("expected helper lookup")
	}
	if !strings.Contains(lookup.url, "replica.example") {
		t.Errorf("helper Lookup keyed on %q, want replica.example", lookup.url)
	}
	if strings.Contains(lookup.url, "/info/refs") {
		t.Errorf("helper Lookup URL should carry the repo path, not /info/refs: %q", lookup.url)
	}
	approve := helper.last("approve")
	if approve == nil || !strings.Contains(approve.url, "replica.example") {
		t.Errorf("helper Approve keyed on wrong URL: %+v", approve)
	}
}

func TestPostRPC_OnUnauthorizedRetriesWithHelperCredentials(t *testing.T) {
	helper := &fakeCredentialHelper{user: "alice", pass: "s3cret", ok: true}
	var authHeaders []string
	attempts := 0
	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		authHeaders = append(authHeaders, req.Header.Get("Authorization"))
		attempts++
		if attempts == 1 {
			return newUnauthorizedResponse(req), nil
		}
		res := &http.Response{
			StatusCode: http.StatusOK,
			Request:    req,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}
		res.Header.Set("Content-Type", "application/x-git-upload-pack-result")
		return res, nil
	}))
	conn.CredentialHelper = helper

	if _, err := PostRPC(context.Background(), conn, "git-upload-pack", []byte("0000"), false, "phase"); err != nil {
		t.Fatalf("PostRPC: %v", err)
	}

	if got := helper.count("lookup"); got != 1 {
		t.Errorf("expected 1 helper lookup, got %d", got)
	}
	if got := helper.count("approve"); got != 1 {
		t.Errorf("expected 1 approve call, got %d", got)
	}
	if len(authHeaders) != 2 {
		t.Fatalf("expected 2 requests (anon then auth), got %d: %v", len(authHeaders), authHeaders)
	}
	if authHeaders[0] != "" {
		t.Errorf("first POST should be anonymous, got %q", authHeaders[0])
	}
	if !strings.HasPrefix(authHeaders[1], "Basic ") {
		t.Errorf("retry POST should have Basic auth, got %q", authHeaders[1])
	}
	if conn.Auth == nil {
		t.Error("expected conn.Auth to be stored after successful POST retry")
	}
}

func TestPostRPC_OnUnauthorizedSurfaces401WithoutHelper(t *testing.T) {
	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return newUnauthorizedResponse(req), nil
	}))

	_, err := PostRPC(context.Background(), conn, "git-upload-pack", []byte("0000"), false, "phase")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 error, got %v", err)
	}
}

func TestPostRPC_OnUnauthorizedRetryStill401CallsReject(t *testing.T) {
	helper := &fakeCredentialHelper{user: "alice", pass: "bad", ok: true}
	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return newUnauthorizedResponse(req), nil
	}))
	conn.CredentialHelper = helper

	_, err := PostRPC(context.Background(), conn, "git-upload-pack", []byte("0000"), false, "phase")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := helper.count("reject"); got != 1 {
		t.Errorf("expected 1 reject call, got %d", got)
	}
	if got := helper.count("approve"); got != 0 {
		t.Errorf("expected 0 approve calls, got %d", got)
	}
}

// TestEnsureAuthForService_TentativelyAttachesHelperCredsOnAnonymous401:
// the anonymous probe gets 401, the helper supplies credentials, and they
// are attached to the conn for the upcoming streaming POST — but NOT
// approved yet. Approval is deferred until the real operation validates
// them; otherwise a server that returns 405 to GET /git-receive-pack
// without checking auth would bless stale creds.
func TestEnsureAuthForService_TentativelyAttachesHelperCredsOnAnonymous401(t *testing.T) {
	helper := &fakeCredentialHelper{user: "alice", pass: "s3cret", ok: true}
	probes := 0
	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		probes++
		return newUnauthorizedResponse(req), nil
	}))
	conn.CredentialHelper = helper

	conn.EnsureAuthForService(context.Background(), "git-receive-pack")

	if conn.Auth == nil {
		t.Fatal("expected conn.Auth to be tentatively set from helper")
	}
	if got := helper.count("lookup"); got != 1 {
		t.Errorf("expected 1 helper lookup, got %d", got)
	}
	if got := helper.count("approve"); got != 0 {
		t.Errorf("probe must not Approve — let the real operation validate. got %d", got)
	}
	if got := helper.count("reject"); got != 0 {
		t.Errorf("probe must not Reject either. got %d", got)
	}
	if probes != 1 {
		t.Errorf("expected exactly 1 probe (no retry), got %d", probes)
	}
}

func TestEnsureAuthForService_NoHelperIsNoOp(t *testing.T) {
	called := false
	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return newAdvertisementResponse(req), nil
	}))

	conn.EnsureAuthForService(context.Background(), "git-receive-pack")

	if called {
		t.Error("expected EnsureAuthForService to be a no-op without a helper")
	}
	if conn.Auth != nil {
		t.Error("expected conn.Auth to remain nil")
	}
}

func TestEnsureAuthForService_AnonymousServiceLeavesAuthNil(t *testing.T) {
	helper := &fakeCredentialHelper{user: "alice", pass: "s3cret", ok: true}
	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusMethodNotAllowed,
			Request:    req,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	}))
	conn.CredentialHelper = helper

	conn.EnsureAuthForService(context.Background(), "git-receive-pack")

	if conn.Auth != nil {
		t.Error("expected conn.Auth to remain nil when probe gets non-401")
	}
	if got := helper.count("lookup"); got != 0 {
		t.Errorf("expected 0 helper lookups when probe didn't 401, got %d", got)
	}
}

// TestEnsureAuthForService_RealPostApprovesTentativeCreds covers the
// production push shape: probe attaches helper creds tentatively; the
// real POST succeeds, which is the actual proof creds are valid. Only
// then do we Approve in the helper.
func TestEnsureAuthForService_RealPostApprovesTentativeCreds(t *testing.T) {
	helper := &fakeCredentialHelper{user: "alice", pass: "s3cret", ok: true}
	var authHeaders []string
	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		authHeaders = append(authHeaders, req.Header.Get("Authorization"))
		if req.Header.Get("Authorization") == "" {
			return newUnauthorizedResponse(req), nil
		}
		res := &http.Response{
			StatusCode: http.StatusOK, Request: req, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("")),
		}
		res.Header.Set("Content-Type", "application/x-git-receive-pack-result")
		return res, nil
	}))
	conn.CredentialHelper = helper

	conn.EnsureAuthForService(context.Background(), "git-receive-pack")
	body := io.MultiReader(strings.NewReader("0000"), strings.NewReader(""))
	reader, err := PostRPCStreamBody(context.Background(), conn, "git-receive-pack", body, false, "phase")
	if err != nil {
		t.Fatalf("PostRPCStreamBody: %v", err)
	}
	_ = reader.Close()

	// 2 requests: probe-anon (401) + POST-authed (200). No second probe.
	if len(authHeaders) != 2 {
		t.Fatalf("expected 2 requests, got %d: %v", len(authHeaders), authHeaders)
	}
	if authHeaders[0] != "" {
		t.Errorf("probe should be anonymous, got %q", authHeaders[0])
	}
	if !strings.HasPrefix(authHeaders[1], "Basic ") {
		t.Errorf("real POST should carry the helper creds, got %q", authHeaders[1])
	}
	if got := helper.count("approve"); got != 1 {
		t.Errorf("expected 1 Approve after the real POST succeeded, got %d", got)
	}
}

// TestEnsureAuthForService_RealPostRejectsTentativeCreds: helper supplied
// stale credentials, the real POST 401s, which is the definitive signal
// to reject them and clear c.Auth.
func TestEnsureAuthForService_RealPostRejectsTentativeCreds(t *testing.T) {
	helper := &fakeCredentialHelper{user: "alice", pass: "stale", ok: true}
	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		// Every request 401s (helper creds are stale).
		return newUnauthorizedResponse(req), nil
	}))
	conn.CredentialHelper = helper

	conn.EnsureAuthForService(context.Background(), "git-receive-pack")
	if conn.Auth == nil {
		t.Fatal("expected conn.Auth to be tentatively set after probe 401")
	}
	body := io.MultiReader(strings.NewReader("0000"), strings.NewReader(""))
	_, err := PostRPCStreamBody(context.Background(), conn, "git-receive-pack", body, false, "phase")
	if err == nil {
		t.Fatal("expected error from POST with stale creds")
	}
	if got := helper.count("reject"); got != 1 {
		t.Errorf("expected 1 Reject after POST 401, got %d", got)
	}
	if got := helper.count("approve"); got != 0 {
		t.Errorf("expected 0 Approve calls, got %d", got)
	}
	if conn.Auth != nil {
		t.Error("expected conn.Auth to be cleared after rejecting bad creds")
	}
}

// TestEnsureAuthForService_405ProbeWithCredsDoesNotPoisonHelper is the
// specific regression: previously, a 405 to the authenticated probe was
// interpreted as "creds accepted" and Approve was called — even though
// the server may have rejected the method before reading Authorization.
// The new contract never approves from a probe response at all.
func TestEnsureAuthForService_405ProbeWithCredsDoesNotPoisonHelper(t *testing.T) {
	helper := &fakeCredentialHelper{user: "alice", pass: "stale", ok: true}
	conn := newTestConn(t, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") == "" {
			return newUnauthorizedResponse(req), nil
		}
		// Server returns 405 without checking auth.
		return &http.Response{
			StatusCode: http.StatusMethodNotAllowed,
			Request:    req, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("")),
		}, nil
	}))
	conn.CredentialHelper = helper

	conn.EnsureAuthForService(context.Background(), "git-receive-pack")

	if got := helper.count("approve"); got != 0 {
		t.Errorf("405 probe response must not Approve stale creds (got %d Approve calls)", got)
	}
}

type credCall struct {
	op   string // "lookup", "approve", "reject"
	user string
	pass string
	url  string // the *url.URL passed to the helper, stringified
}

// fakeCredentialHelper is a test CredentialHelper. Set user/pass/ok/err to
// configure Lookup; inspect calls (via count/last) to assert lifecycle.
type fakeCredentialHelper struct {
	user, pass string
	ok         bool
	err        error

	calls []credCall
}

func (h *fakeCredentialHelper) Lookup(_ context.Context, ep *url.URL) (string, string, bool, error) {
	h.calls = append(h.calls, credCall{op: "lookup", url: ep.String()})
	return h.user, h.pass, h.ok, h.err
}

func (h *fakeCredentialHelper) Approve(_ context.Context, ep *url.URL, user, pass string) {
	h.calls = append(h.calls, credCall{op: "approve", user: user, pass: pass, url: ep.String()})
}

func (h *fakeCredentialHelper) Reject(_ context.Context, ep *url.URL, user, pass string) {
	h.calls = append(h.calls, credCall{op: "reject", user: user, pass: pass, url: ep.String()})
}

func (h *fakeCredentialHelper) count(op string) int {
	n := 0
	for _, c := range h.calls {
		if c.op == op {
			n++
		}
	}
	return n
}

func (h *fakeCredentialHelper) last(op string) *credCall {
	for i := len(h.calls) - 1; i >= 0; i-- {
		if h.calls[i].op == op {
			return &h.calls[i]
		}
	}
	return nil
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
