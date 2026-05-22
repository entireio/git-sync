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

// newAdvertisementResponse returns a 200 response shaped like a smart-HTTP
// /info/refs advertisement, suitable for round-tripper fakes.
func newAdvertisementResponse(req *http.Request, service string) *http.Response {
	res := &http.Response{
		StatusCode: http.StatusOK,
		Request:    req,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("0000")),
	}
	res.Header.Set("Content-Type", "application/x-"+service+"-advertisement")
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

// TestRequestInfoRefs_AnonymousSucceedsWithoutConsultingHelper verifies the
// happy path: when the server accepts an unauthenticated request, we never
// touch the credential helper.
func TestRequestInfoRefs_AnonymousSucceedsWithoutConsultingHelper(t *testing.T) {
	helper := &fakeCredentialHelper{lookupOK: true, lookupUser: "x", lookupPass: "y"}
	var authHeaders []string
	conn := NewHTTPConn(
		&url.URL{Scheme: "https", Host: "example.com", Path: "/repo.git"},
		"src", nil,
		roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			authHeaders = append(authHeaders, req.Header.Get("Authorization"))
			return newAdvertisementResponse(req, "git-upload-pack"), nil
		}),
	)
	conn.CredentialHelper = helper

	if _, err := conn.RequestInfoRefs(context.Background(), "git-upload-pack", ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}
	if helper.lookupCalls != 0 {
		t.Errorf("expected 0 helper lookups on anonymous success, got %d", helper.lookupCalls)
	}
	if len(authHeaders) != 1 || authHeaders[0] != "" {
		t.Errorf("expected exactly one anonymous request, got headers %v", authHeaders)
	}
}

// TestRequestInfoRefs_OnUnauthorizedRetriesWithHelperCredentials verifies
// the core fix: a 401 triggers a helper lookup, the request is retried with
// those credentials, the helper is told the creds worked, and the conn
// remembers the credentials for subsequent calls.
func TestRequestInfoRefs_OnUnauthorizedRetriesWithHelperCredentials(t *testing.T) {
	helper := &fakeCredentialHelper{lookupUser: "alice", lookupPass: "s3cret", lookupOK: true}

	var authHeaders []string
	attempts := 0
	conn := NewHTTPConn(
		&url.URL{Scheme: "https", Host: "example.com", Path: "/repo.git"},
		"src", nil,
		roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			authHeaders = append(authHeaders, req.Header.Get("Authorization"))
			attempts++
			if attempts == 1 {
				return newUnauthorizedResponse(req), nil
			}
			return newAdvertisementResponse(req, "git-upload-pack"), nil
		}),
	)
	conn.CredentialHelper = helper

	if _, err := conn.RequestInfoRefs(context.Background(), "git-upload-pack", ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}

	if helper.lookupCalls != 1 {
		t.Errorf("expected 1 helper lookup, got %d", helper.lookupCalls)
	}
	if helper.approveCalls != 1 {
		t.Errorf("expected 1 approve call on retry success, got %d", helper.approveCalls)
	}
	if helper.rejectCalls != 0 {
		t.Errorf("expected 0 reject calls on retry success, got %d", helper.rejectCalls)
	}
	if helper.lastApproveUser != "alice" || helper.lastApprovePass != "s3cret" {
		t.Errorf("approve called with wrong creds: user=%q pass=%q", helper.lastApproveUser, helper.lastApprovePass)
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

// TestRequestInfoRefs_OnUnauthorizedReusesStoredAuthOnNextCall confirms that
// once a 401 retry succeeds, subsequent requests on the same connection use
// the stored auth instead of going through anonymous → 401 → retry again.
func TestRequestInfoRefs_OnUnauthorizedReusesStoredAuthOnNextCall(t *testing.T) {
	helper := &fakeCredentialHelper{lookupUser: "alice", lookupPass: "s3cret", lookupOK: true}

	var authHeaders []string
	attempts := 0
	conn := NewHTTPConn(
		&url.URL{Scheme: "https", Host: "example.com", Path: "/repo.git"},
		"src", nil,
		roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			authHeaders = append(authHeaders, req.Header.Get("Authorization"))
			attempts++
			if attempts == 1 {
				return newUnauthorizedResponse(req), nil
			}
			if req.Method == http.MethodGet {
				return newAdvertisementResponse(req, "git-upload-pack"), nil
			}
			res := &http.Response{
				StatusCode: http.StatusOK,
				Request:    req,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
			}
			res.Header.Set("Content-Type", "application/x-git-upload-pack-result")
			return res, nil
		}),
	)
	conn.CredentialHelper = helper

	if _, err := conn.RequestInfoRefs(context.Background(), "git-upload-pack", ""); err != nil {
		t.Fatalf("RequestInfoRefs: %v", err)
	}
	if _, err := PostRPC(context.Background(), conn, "git-upload-pack", []byte("0000"), false, "phase"); err != nil {
		t.Fatalf("PostRPC: %v", err)
	}

	if helper.lookupCalls != 1 {
		t.Errorf("expected only 1 helper lookup across both requests, got %d", helper.lookupCalls)
	}
	if len(authHeaders) != 3 {
		t.Fatalf("expected 3 requests, got %d: %v", len(authHeaders), authHeaders)
	}
	if authHeaders[1] == "" || authHeaders[2] == "" {
		t.Errorf("retry GET and follow-up POST should both carry auth: %v", authHeaders)
	}
}

// TestRequestInfoRefs_OnUnauthorizedSurfaces401WithoutHelper verifies that
// a 401 without a configured credential helper just returns the error.
func TestRequestInfoRefs_OnUnauthorizedSurfaces401WithoutHelper(t *testing.T) {
	conn := NewHTTPConn(
		&url.URL{Scheme: "https", Host: "example.com", Path: "/repo.git"},
		"src", nil,
		roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return newUnauthorizedResponse(req), nil
		}),
	)

	_, err := conn.RequestInfoRefs(context.Background(), "git-upload-pack", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 error, got %v", err)
	}
}

// TestRequestInfoRefs_OnUnauthorizedSurfaces401WhenHelperHasNoCredentials
// verifies that an opaque "helper has nothing" response (ok=false) surfaces
// the 401 cleanly — no retry, no approve, no reject.
func TestRequestInfoRefs_OnUnauthorizedSurfaces401WhenHelperHasNoCredentials(t *testing.T) {
	helper := &fakeCredentialHelper{lookupOK: false}
	conn := NewHTTPConn(
		&url.URL{Scheme: "https", Host: "example.com", Path: "/repo.git"},
		"src", nil,
		roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return newUnauthorizedResponse(req), nil
		}),
	)
	conn.CredentialHelper = helper

	_, err := conn.RequestInfoRefs(context.Background(), "git-upload-pack", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 error, got %v", err)
	}
	if helper.lookupCalls != 1 {
		t.Errorf("expected 1 lookup attempt, got %d", helper.lookupCalls)
	}
	if helper.approveCalls != 0 || helper.rejectCalls != 0 {
		t.Errorf("expected no approve/reject when helper had no creds, got approve=%d reject=%d",
			helper.approveCalls, helper.rejectCalls)
	}
}

// TestRequestInfoRefs_OnUnauthorizedRetryStill401CallsReject verifies that
// when the helper-supplied credentials are themselves rejected by the
// server, we tell the helper so it can forget them.
func TestRequestInfoRefs_OnUnauthorizedRetryStill401CallsReject(t *testing.T) {
	helper := &fakeCredentialHelper{lookupUser: "alice", lookupPass: "bad", lookupOK: true}
	conn := NewHTTPConn(
		&url.URL{Scheme: "https", Host: "example.com", Path: "/repo.git"},
		"src", nil,
		roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return newUnauthorizedResponse(req), nil
		}),
	)
	conn.CredentialHelper = helper

	_, err := conn.RequestInfoRefs(context.Background(), "git-upload-pack", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if helper.rejectCalls != 1 {
		t.Errorf("expected 1 reject call, got %d", helper.rejectCalls)
	}
	if helper.approveCalls != 0 {
		t.Errorf("expected 0 approve calls, got %d", helper.approveCalls)
	}
	if helper.lastRejectUser != "alice" || helper.lastRejectPass != "bad" {
		t.Errorf("reject called with wrong creds: user=%q pass=%q", helper.lastRejectUser, helper.lastRejectPass)
	}
}

// TestRequestInfoRefs_OnUnauthorizedRetry403CallsReject verifies that some
// token services (notably Cloudflare) return 403 "Invalid or expired token"
// instead of 401 when stored credentials have expired. Since we only reach
// the retry path when the initial response was 401 (server required auth),
// a 403 on retry indicates the helper's credentials themselves didn't
// validate — reject them so the next run starts clean.
func TestRequestInfoRefs_OnUnauthorizedRetry403CallsReject(t *testing.T) {
	helper := &fakeCredentialHelper{lookupUser: "user", lookupPass: "expired-token", lookupOK: true}
	attempts := 0
	conn := NewHTTPConn(
		&url.URL{Scheme: "https", Host: "example.com", Path: "/repo.git"},
		"src", nil,
		roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			if attempts == 1 {
				return newUnauthorizedResponse(req), nil
			}
			// Retry with helper creds — server says 403, expired token.
			res := &http.Response{
				StatusCode: http.StatusForbidden,
				Request:    req,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("Invalid or expired token")),
			}
			return res, nil
		}),
	)
	conn.CredentialHelper = helper

	_, err := conn.RequestInfoRefs(context.Background(), "git-upload-pack", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if helper.rejectCalls != 1 {
		t.Errorf("expected 1 reject call on retry 403, got %d", helper.rejectCalls)
	}
	if helper.approveCalls != 0 {
		t.Errorf("expected 0 approve calls, got %d", helper.approveCalls)
	}
	if helper.lastRejectUser != "user" || helper.lastRejectPass != "expired-token" {
		t.Errorf("reject called with wrong creds: user=%q pass=%q", helper.lastRejectUser, helper.lastRejectPass)
	}
}

// TestRequestInfoRefs_DoesNotRetryWhenConnAlreadyAuthenticated verifies
// that if the caller explicitly configured auth (via Resolve), we don't
// override it with helper credentials on 401 — that's the caller's
// problem to debug, not ours to silently paper over.
func TestRequestInfoRefs_DoesNotRetryWhenConnAlreadyAuthenticated(t *testing.T) {
	helper := &fakeCredentialHelper{lookupUser: "alice", lookupPass: "s3cret", lookupOK: true}
	initialAuth := &transporthttp.BasicAuth{Username: "explicit", Password: "tok"}

	conn := NewHTTPConn(
		&url.URL{Scheme: "https", Host: "example.com", Path: "/repo.git"},
		"src", initialAuth,
		roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return newUnauthorizedResponse(req), nil
		}),
	)
	conn.CredentialHelper = helper

	_, err := conn.RequestInfoRefs(context.Background(), "git-upload-pack", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if helper.lookupCalls != 0 {
		t.Errorf("expected 0 helper lookups when auth was preconfigured, got %d", helper.lookupCalls)
	}
}

// fakeCredentialHelper is a CredentialHelper used in tests. Configure lookup
// behaviour via lookupUser/lookupPass/lookupOK/lookupErr; the call counters
// let tests assert the helper's Approve/Reject lifecycle was driven correctly.
type fakeCredentialHelper struct {
	lookupUser string
	lookupPass string
	lookupOK   bool
	lookupErr  error

	lookupCalls  int
	approveCalls int
	rejectCalls  int

	lastApproveUser, lastApprovePass string
	lastRejectUser, lastRejectPass   string
}

func (h *fakeCredentialHelper) Lookup(_ context.Context, _ *url.URL) (string, string, bool, error) {
	h.lookupCalls++
	return h.lookupUser, h.lookupPass, h.lookupOK, h.lookupErr
}

func (h *fakeCredentialHelper) Approve(_ context.Context, _ *url.URL, user, pass string) {
	h.approveCalls++
	h.lastApproveUser, h.lastApprovePass = user, pass
}

func (h *fakeCredentialHelper) Reject(_ context.Context, _ *url.URL, user, pass string) {
	h.rejectCalls++
	h.lastRejectUser, h.lastRejectPass = user, pass
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
