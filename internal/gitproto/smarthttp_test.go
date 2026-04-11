package gitproto

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
	transporthttp "github.com/go-git/go-git/v5/plumbing/transport/http"
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
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	auth := &transporthttp.BasicAuth{Username: "user", Password: "pass"}
	ApplyAuth(req, auth)
	user, pass, ok := req.BasicAuth()
	if !ok || user != "user" || pass != "pass" {
		t.Errorf("BasicAuth not applied: ok=%v user=%q pass=%q", ok, user, pass)
	}

	// TokenAuth
	req, _ = http.NewRequest("GET", "https://example.com", nil)
	tokenAuth := &transporthttp.TokenAuth{Token: "my-token"}
	ApplyAuth(req, tokenAuth)
	got := req.Header.Get("Authorization")
	if got == "" {
		t.Error("TokenAuth not applied: Authorization header is empty")
	}

	// nil auth should not panic.
	req, _ = http.NewRequest("GET", "https://example.com", nil)
	ApplyAuth(req, nil)
}

func TestRequestInfoRefsContextCanceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ep, err := transport.NewEndpoint(server.URL + "/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	conn := NewConn(ep, "source", nil, http.DefaultTransport)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = RequestInfoRefs(ctx, conn, "git-upload-pack", "version=2")
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestPostRPCStreamContextCanceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ep, err := transport.NewEndpoint(server.URL + "/repo.git")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	conn := NewConn(ep, "source", nil, http.DefaultTransport)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = PostRPCStream(ctx, conn, "git-upload-pack", []byte("0000"), true, "upload-pack fetch")
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
