package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing/transport"
	transporthttp "github.com/go-git/go-git/v6/plumbing/transport/http"

	"github.com/entirehq/git-sync/internal/auth"
)

func TestResolveAuthMethodPrefersExplicitToken(t *testing.T) {
	ep, err := transport.NewEndpoint("https://github.com/entireio/cli.git")
	if err != nil {
		t.Fatalf("new endpoint: %v", err)
	}

	originalFill := auth.GitCredentialFillCommand
	t.Cleanup(func() { auth.GitCredentialFillCommand = originalFill })
	auth.GitCredentialFillCommand = func(ctx context.Context, input string) ([]byte, error) {
		t.Fatalf("unexpected git credential fill call with input %q", input)
		return nil, nil
	}

	resolved, err := auth.Resolve(auth.Endpoint{
		Username: "git",
		Token:    "explicit-token",
	}, ep)
	if err != nil {
		t.Fatalf("resolve auth: %v", err)
	}

	basic, ok := resolved.(*transporthttp.BasicAuth)
	if !ok {
		t.Fatalf("expected basic auth, got %T", resolved)
	}
	if basic.Username != "git" || basic.Password != "explicit-token" {
		t.Fatalf("unexpected auth: %+v", basic)
	}
}

func TestNewConnSkipTLSVerify(t *testing.T) {
	stats := newStats(false)
	conn, err := newConn(Endpoint{
		URL:           "https://example.com/repo.git",
		SkipTLSVerify: true,
	}, "source", stats, nil)
	if err != nil {
		t.Fatalf("new conn: %v", err)
	}
	rt, ok := conn.HTTP.Transport.(*countingRoundTripper)
	if !ok {
		t.Fatalf("expected countingRoundTripper, got %T", conn.HTTP.Transport)
	}
	base, ok := rt.base.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport base, got %T", rt.base)
	}
	if base.TLSClientConfig == nil || !base.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("expected InsecureSkipVerify transport")
	}
}

func TestNewConnUsesProvidedHTTPClient(t *testing.T) {
	stats := newStats(false)
	baseTransport := http.DefaultTransport
	baseClient := &http.Client{Transport: baseTransport}

	conn, err := newConn(Endpoint{URL: "https://example.com/repo.git"}, "source", stats, baseClient)
	if err != nil {
		t.Fatalf("new conn: %v", err)
	}
	if conn.HTTP == baseClient {
		t.Fatalf("expected cloned HTTP client, got original pointer")
	}
	rt, ok := conn.HTTP.Transport.(*countingRoundTripper)
	if !ok {
		t.Fatalf("expected countingRoundTripper, got %T", conn.HTTP.Transport)
	}
	if rt.base != baseTransport {
		t.Fatalf("wrapped base transport = %T, want %T", rt.base, baseTransport)
	}
}

func TestResolveAuthMethodUsesEntireDBStoredToken(t *testing.T) {
	configDir := t.TempDir()
	tokenStorePath := filepath.Join(t.TempDir(), "tokens.json")
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	t.Setenv("ENTIRE_TOKEN_STORE", "file")
	t.Setenv("ENTIRE_TOKEN_STORE_PATH", tokenStorePath)

	ep, err := transport.NewEndpoint("https://localhost:8080/git/test/repo")
	if err != nil {
		t.Fatalf("new endpoint: %v", err)
	}
	credHost := ep.Host
	writeEntireDBHostsFile(t, configDir, credHost, "test-user")
	// Store token with a future expiration so it's not treated as expired (issue #7).
	futureExpiry := time.Now().Unix() + 3600
	if err := auth.WriteStoredToken("entire:"+credHost, "test-user", fmt.Sprintf("stored-token|%d", futureExpiry)); err != nil {
		t.Fatalf("write token: %v", err)
	}

	originalFill := auth.GitCredentialFillCommand
	t.Cleanup(func() { auth.GitCredentialFillCommand = originalFill })
	auth.GitCredentialFillCommand = func(ctx context.Context, input string) ([]byte, error) {
		t.Fatalf("unexpected git credential fill call with input %q", input)
		return nil, nil
	}

	resolved, err := auth.Resolve(auth.Endpoint{}, ep)
	if err != nil {
		t.Fatalf("resolve auth: %v", err)
	}

	basic, ok := resolved.(*transporthttp.BasicAuth)
	if !ok {
		t.Fatalf("expected basic auth, got %T", resolved)
	}
	if basic.Username != "git" || basic.Password != "stored-token" {
		t.Fatalf("unexpected auth: %+v", basic)
	}
}

func TestResolveAuthMethodRefreshesExpiredEntireDBToken(t *testing.T) {
	configDir := t.TempDir()
	tokenStorePath := filepath.Join(t.TempDir(), "tokens.json")
	t.Setenv("ENTIRE_CONFIG_DIR", configDir)
	t.Setenv("ENTIRE_TOKEN_STORE", "file")
	t.Setenv("ENTIRE_TOKEN_STORE_PATH", tokenStorePath)

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if got := r.Form.Get("refresh_token"); got != "refresh-token" {
			t.Fatalf("unexpected refresh token: %s", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-token","refresh_token":"new-refresh","expires_in":3600}`))
	}))
	defer server.Close()

	ep, err := transport.NewEndpoint(server.URL + "/git/test/repo")
	if err != nil {
		t.Fatalf("new endpoint: %v", err)
	}
	credHost := ep.Host
	writeEntireDBHostsFile(t, configDir, credHost, "test-user")
	// Expired token: expiration in the past
	if err := auth.WriteStoredToken("entire:"+credHost, "test-user", "expired-token|1"); err != nil {
		t.Fatalf("write expired token: %v", err)
	}
	if err := auth.WriteStoredToken("entire:"+credHost+":refresh", "test-user", "refresh-token"); err != nil {
		t.Fatalf("write refresh token: %v", err)
	}

	resolved, err := auth.Resolve(auth.Endpoint{SkipTLSVerify: true}, ep)
	if err != nil {
		t.Fatalf("resolve auth: %v", err)
	}

	basic, ok := resolved.(*transporthttp.BasicAuth)
	if !ok {
		t.Fatalf("expected basic auth, got %T", resolved)
	}
	if basic.Password != "new-token" {
		t.Fatalf("unexpected password: %q", basic.Password)
	}
}

func writeEntireDBHostsFile(t *testing.T, configDir, host, username string) {
	t.Helper()
	hosts := map[string]map[string]any{
		host: {"activeUser": username, "users": []string{username}},
	}
	data, err := json.Marshal(hosts)
	if err != nil {
		t.Fatalf("marshal hosts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "hosts.json"), data, 0o600); err != nil {
		t.Fatalf("write hosts: %v", err)
	}
}
