package syncer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
	transporthttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

func TestResolveAuthMethodPrefersExplicitToken(t *testing.T) {
	ep, err := transport.NewEndpoint("https://github.com/entireio/cli.git")
	if err != nil {
		t.Fatalf("new endpoint: %v", err)
	}

	originalFill := gitCredentialFillCommand
	t.Cleanup(func() {
		gitCredentialFillCommand = originalFill
	})
	gitCredentialFillCommand = func(ctx context.Context, input string) ([]byte, error) {
		t.Fatalf("unexpected git credential fill call with input %q", input)
		return nil, nil
	}

	auth, err := resolveAuthMethod(Endpoint{
		Username: "git",
		Token:    "explicit-token",
	}, ep)
	if err != nil {
		t.Fatalf("resolve auth: %v", err)
	}

	basic, ok := auth.(*transporthttp.BasicAuth)
	if !ok {
		t.Fatalf("expected basic auth, got %T", auth)
	}
	if basic.Username != "git" || basic.Password != "explicit-token" {
		t.Fatalf("unexpected auth: %+v", basic)
	}
}

func TestNewTransportConnSkipTLSVerify(t *testing.T) {
	conn, err := newTransportConn(Endpoint{
		URL:           "https://example.com/repo.git",
		SkipTLSVerify: true,
	}, "source", newStats(false))
	if err != nil {
		t.Fatalf("new transport conn: %v", err)
	}

	roundTripper, ok := conn.http.Transport.(*countingRoundTripper)
	if !ok {
		t.Fatalf("expected countingRoundTripper, got %T", conn.http.Transport)
	}
	base, ok := roundTripper.base.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport base, got %T", roundTripper.base)
	}
	if base.TLSClientConfig == nil || !base.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("expected InsecureSkipVerify transport, got %#v", base.TLSClientConfig)
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
	credHost := endpointCredentialHost(ep)
	writeEntireDBHostsFile(t, configDir, credHost, "test-user")
	if err := writeEntireDBStoredToken("entire:"+credHost, "test-user", "stored-token"); err != nil {
		t.Fatalf("write token: %v", err)
	}

	originalFill := gitCredentialFillCommand
	t.Cleanup(func() {
		gitCredentialFillCommand = originalFill
	})
	gitCredentialFillCommand = func(ctx context.Context, input string) ([]byte, error) {
		t.Fatalf("unexpected git credential fill call with input %q", input)
		return nil, nil
	}

	auth, err := resolveAuthMethod(Endpoint{}, ep)
	if err != nil {
		t.Fatalf("resolve auth: %v", err)
	}

	basic, ok := auth.(*transporthttp.BasicAuth)
	if !ok {
		t.Fatalf("expected basic auth, got %T", auth)
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
	credHost := endpointCredentialHost(ep)
	writeEntireDBHostsFile(t, configDir, credHost, "test-user")
	if err := writeEntireDBStoredToken("entire:"+credHost, "test-user", encodeTokenWithExpiration("expired-token", -3600)); err != nil {
		t.Fatalf("write expired token: %v", err)
	}
	if err := writeEntireDBStoredToken("entire:"+credHost+":refresh", "test-user", "refresh-token"); err != nil {
		t.Fatalf("write refresh token: %v", err)
	}

	auth, err := resolveAuthMethod(Endpoint{SkipTLSVerify: true}, ep)
	if err != nil {
		t.Fatalf("resolve auth: %v", err)
	}

	basic, ok := auth.(*transporthttp.BasicAuth)
	if !ok {
		t.Fatalf("expected basic auth, got %T", auth)
	}
	if basic.Password != "new-token" {
		t.Fatalf("unexpected password: %q", basic.Password)
	}

	stored, err := readEntireDBStoredToken("entire:"+credHost, "test-user")
	if err != nil {
		t.Fatalf("read stored token: %v", err)
	}
	token, _ := decodeTokenWithExpiration(stored)
	if token != "new-token" {
		t.Fatalf("unexpected refreshed token: %q", token)
	}
}

func writeEntireDBHostsFile(t *testing.T, configDir, host, username string) {
	t.Helper()
	hosts := map[string]map[string]any{
		host: {
			"activeUser": username,
			"users":      []string{username},
		},
	}
	data, err := json.Marshal(hosts)
	if err != nil {
		t.Fatalf("marshal hosts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "hosts.json"), data, 0o600); err != nil {
		t.Fatalf("write hosts: %v", err)
	}
}
