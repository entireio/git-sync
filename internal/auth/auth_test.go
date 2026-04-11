package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	transporthttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/zalando/go-keyring"
)

func TestDecodeTokenWithExpiration(t *testing.T) {
	tests := []struct {
		name      string
		encoded   string
		wantToken string
		wantZero  bool   // if true, expect time.Time zero value
		wantUnix  int64  // checked only when wantZero is false
	}{
		{
			name:      "token with pipe-separated unix timestamp",
			encoded:   "mytoken|12345",
			wantToken: "mytoken",
			wantUnix:  12345,
		},
		{
			name:      "plain token without pipe",
			encoded:   "plain-token",
			wantToken: "plain-token",
			wantZero:  true,
		},
		{
			name:      "empty string",
			encoded:   "",
			wantToken: "",
			wantZero:  true,
		},
		{
			name:      "pipe with non-numeric suffix falls back to full string",
			encoded:   "tok|notanumber",
			wantToken: "tok|notanumber",
			wantZero:  true,
		},
		{
			name:      "multiple pipes uses last one",
			encoded:   "a|b|99999",
			wantToken: "a|b",
			wantUnix:  99999,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, ts := decodeTokenWithExpiration(tt.encoded)
			if token != tt.wantToken {
				t.Errorf("token = %q, want %q", token, tt.wantToken)
			}
			if tt.wantZero {
				if !ts.IsZero() {
					t.Errorf("expected zero time, got %v", ts)
				}
			} else {
				if ts.Unix() != tt.wantUnix {
					t.Errorf("timestamp = %d, want %d", ts.Unix(), tt.wantUnix)
				}
			}
		})
	}
}

func TestTokenExpiredOrExpiring(t *testing.T) {
	tests := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{
			name:      "zero time is treated as expired",
			expiresAt: time.Time{},
			want:      true,
		},
		{
			name:      "far future is not expired",
			expiresAt: time.Now().Add(1 * time.Hour),
			want:      false,
		},
		{
			name:      "past time is expired",
			expiresAt: time.Now().Add(-1 * time.Hour),
			want:      true,
		},
		{
			name:      "expiring within 5 minute window is treated as expired",
			expiresAt: time.Now().Add(2 * time.Minute),
			want:      true,
		},
		{
			name:      "just beyond 5 minute window is not expired",
			expiresAt: time.Now().Add(10 * time.Minute),
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tokenExpiredOrExpiring(tt.expiresAt)
			if got != tt.want {
				t.Errorf("tokenExpiredOrExpiring(%v) = %v, want %v", tt.expiresAt, got, tt.want)
			}
		})
	}
}

func TestCredentialFillInput(t *testing.T) {
	ep := &transport.Endpoint{
		Protocol: "https",
		Host:     "github.com",
		Path:     "/owner/repo.git",
		User:     "myuser",
	}

	got := credentialFillInput(ep)
	want := "protocol=https\nhost=github.com\npath=owner/repo.git\nusername=myuser\n\n"
	if got != want {
		t.Errorf("credentialFillInput returned:\n%q\nwant:\n%q", got, want)
	}
}

func TestCredentialFillInputNilEndpoint(t *testing.T) {
	got := credentialFillInput(nil)
	if got != "" {
		t.Errorf("expected empty string for nil endpoint, got %q", got)
	}
}

func TestCredentialFillInputEmptyHost(t *testing.T) {
	ep := &transport.Endpoint{Protocol: "https"}
	got := credentialFillInput(ep)
	if got != "" {
		t.Errorf("expected empty string for empty host, got %q", got)
	}
}

func TestCredentialFillInputNoUser(t *testing.T) {
	ep := &transport.Endpoint{
		Protocol: "https",
		Host:     "example.com",
		Path:     "/repo.git",
	}
	got := credentialFillInput(ep)
	want := "protocol=https\nhost=example.com\npath=repo.git\n\n"
	if got != want {
		t.Errorf("credentialFillInput returned:\n%q\nwant:\n%q", got, want)
	}
}

func TestParseCredentialOutput(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		wantUser   string
		wantPass   string
		wantLen    int
	}{
		{
			name:     "standard username and password",
			output:   "username=foo\npassword=bar\n",
			wantUser: "foo",
			wantPass: "bar",
			wantLen:  2,
		},
		{
			name:     "with extra fields",
			output:   "protocol=https\nhost=example.com\nusername=alice\npassword=secret\n",
			wantUser: "alice",
			wantPass: "secret",
			wantLen:  4,
		},
		{
			name:    "empty input",
			output:  "",
			wantLen: 0,
		},
		{
			name:    "blank lines only",
			output:  "\n\n\n",
			wantLen: 0,
		},
		{
			name:     "value with equals sign",
			output:   "password=tok=en\n",
			wantPass: "tok=en",
			wantLen:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCredentialOutput([]byte(tt.output))
			if len(got) != tt.wantLen {
				t.Errorf("len(result) = %d, want %d; result = %v", len(got), tt.wantLen, got)
			}
			if tt.wantUser != "" {
				if got["username"] != tt.wantUser {
					t.Errorf("username = %q, want %q", got["username"], tt.wantUser)
				}
			}
			if tt.wantPass != "" {
				if got["password"] != tt.wantPass {
					t.Errorf("password = %q, want %q", got["password"], tt.wantPass)
				}
			}
		})
	}
}

func TestResolve(t *testing.T) {
	ep, err := transport.NewEndpoint("https://example.com/repo.git")
	if err != nil {
		t.Fatal(err)
	}

	sshEP := &transport.Endpoint{Protocol: "ssh", Host: "example.com", Path: "/repo.git"}

	tests := []struct {
		name      string
		raw       Endpoint
		ep        *transport.Endpoint
		mockCred  func(ctx context.Context, input string) ([]byte, error)
		wantType  string // "token", "basic", "nil"
		wantUser  string
		wantPass  string
		wantErr   bool
	}{
		{
			name:     "bearer token set returns TokenAuth",
			raw:      Endpoint{BearerToken: "my-bearer"},
			ep:       ep,
			wantType: "token",
			wantPass: "my-bearer",
		},
		{
			name:     "token with username returns BasicAuth",
			raw:      Endpoint{Token: "my-token", Username: "alice"},
			ep:       ep,
			wantType: "basic",
			wantUser: "alice",
			wantPass: "my-token",
		},
		{
			name:     "token without username returns BasicAuth with git",
			raw:      Endpoint{Token: "my-token"},
			ep:       ep,
			wantType: "basic",
			wantUser: "git",
			wantPass: "my-token",
		},
		{
			name:     "nothing set non-HTTP endpoint returns nil",
			raw:      Endpoint{},
			ep:       sshEP,
			wantType: "nil",
		},
		{
			name: "nothing set HTTP endpoint no credential helper returns nil",
			raw:  Endpoint{},
			ep:   ep,
			mockCred: func(ctx context.Context, input string) ([]byte, error) {
				return nil, fmt.Errorf("no helper")
			},
			wantType: "nil",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Save and restore GitCredentialFillCommand.
			origCmd := GitCredentialFillCommand
			defer func() { GitCredentialFillCommand = origCmd }()

			if tt.mockCred != nil {
				GitCredentialFillCommand = tt.mockCred
			} else {
				// Default mock: no credential helper.
				GitCredentialFillCommand = func(ctx context.Context, input string) ([]byte, error) {
					return nil, fmt.Errorf("no helper")
				}
			}

			// Also ensure ENTIRE_CONFIG_DIR points nowhere so EntireDB lookup
			// doesn't find anything.
			t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())

			got, err := Resolve(tt.raw, tt.ep)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			switch tt.wantType {
			case "nil":
				if got != nil {
					t.Errorf("expected nil auth, got %T", got)
				}
			case "token":
				ta, ok := got.(*transporthttp.TokenAuth)
				if !ok {
					t.Fatalf("expected *TokenAuth, got %T", got)
				}
				if ta.Token != tt.wantPass {
					t.Errorf("token = %q, want %q", ta.Token, tt.wantPass)
				}
			case "basic":
				ba, ok := got.(*transporthttp.BasicAuth)
				if !ok {
					t.Fatalf("expected *BasicAuth, got %T", got)
				}
				if ba.Username != tt.wantUser {
					t.Errorf("username = %q, want %q", ba.Username, tt.wantUser)
				}
				if ba.Password != tt.wantPass {
					t.Errorf("password = %q, want %q", ba.Password, tt.wantPass)
				}
			}
		})
	}
}

func TestExplicitAuth(t *testing.T) {
	tests := []struct {
		name     string
		raw      Endpoint
		wantType string // "token", "basic", "nil"
		wantUser string
		wantPass string
	}{
		{
			name:     "bearer token returns TokenAuth",
			raw:      Endpoint{BearerToken: "bearer-abc"},
			wantType: "token",
			wantPass: "bearer-abc",
		},
		{
			name:     "token with username returns BasicAuth",
			raw:      Endpoint{Token: "tok", Username: "bob"},
			wantType: "basic",
			wantUser: "bob",
			wantPass: "tok",
		},
		{
			name:     "token without username returns BasicAuth with git",
			raw:      Endpoint{Token: "tok"},
			wantType: "basic",
			wantUser: "git",
			wantPass: "tok",
		},
		{
			name:     "nothing set returns nil",
			raw:      Endpoint{},
			wantType: "nil",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := explicitAuth(tt.raw)

			switch tt.wantType {
			case "nil":
				if got != nil {
					t.Errorf("expected nil, got %T", got)
				}
			case "token":
				ta, ok := got.(*transporthttp.TokenAuth)
				if !ok {
					t.Fatalf("expected *TokenAuth, got %T", got)
				}
				if ta.Token != tt.wantPass {
					t.Errorf("token = %q, want %q", ta.Token, tt.wantPass)
				}
			case "basic":
				ba, ok := got.(*transporthttp.BasicAuth)
				if !ok {
					t.Fatalf("expected *BasicAuth, got %T", got)
				}
				if ba.Username != tt.wantUser {
					t.Errorf("username = %q, want %q", ba.Username, tt.wantUser)
				}
				if ba.Password != tt.wantPass {
					t.Errorf("password = %q, want %q", ba.Password, tt.wantPass)
				}
			}
		})
	}
}

func TestEndpointBaseURL(t *testing.T) {
	tests := []struct {
		name string
		ep   *transport.Endpoint
		want string
	}{
		{
			name: "https host",
			ep:   &transport.Endpoint{Protocol: "https", Host: "example.com"},
			want: "https://example.com",
		},
		{
			name: "http host with port",
			ep:   &transport.Endpoint{Protocol: "http", Host: "example.com", Port: 8080},
			want: "http://example.com:8080",
		},
		{
			name: "nil endpoint",
			ep:   nil,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := endpointBaseURL(tt.ep)
			if got != tt.want {
				t.Errorf("endpointBaseURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEndpointCredentialHost(t *testing.T) {
	tests := []struct {
		name string
		ep   *transport.Endpoint
		want string
	}{
		{
			name: "host without port",
			ep:   &transport.Endpoint{Host: "example.com"},
			want: "example.com",
		},
		{
			name: "host with port",
			ep:   &transport.Endpoint{Host: "example.com", Port: 8080},
			want: "example.com:8080",
		},
		{
			name: "nil endpoint",
			ep:   nil,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := endpointCredentialHost(tt.ep)
			if got != tt.want {
				t.Errorf("endpointCredentialHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadWriteFileToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")

	// Write a token and read it back.
	if err := writeFileToken(path, "svc1", "user1", "pass1"); err != nil {
		t.Fatalf("writeFileToken: %v", err)
	}
	got, err := readFileToken(path, "svc1", "user1")
	if err != nil {
		t.Fatalf("readFileToken: %v", err)
	}
	if got != "pass1" {
		t.Errorf("readFileToken = %q, want %q", got, "pass1")
	}

	// Read missing service returns ErrNotFound.
	_, err = readFileToken(path, "missing-svc", "user1")
	if !errors.Is(err, keyring.ErrNotFound) {
		t.Errorf("expected ErrNotFound for missing service, got %v", err)
	}

	// Write a second service and read each back.
	if err := writeFileToken(path, "svc2", "user2", "pass2"); err != nil {
		t.Fatalf("writeFileToken svc2: %v", err)
	}
	got1, err := readFileToken(path, "svc1", "user1")
	if err != nil {
		t.Fatalf("readFileToken svc1 after second write: %v", err)
	}
	if got1 != "pass1" {
		t.Errorf("svc1 token = %q, want %q", got1, "pass1")
	}
	got2, err := readFileToken(path, "svc2", "user2")
	if err != nil {
		t.Fatalf("readFileToken svc2: %v", err)
	}
	if got2 != "pass2" {
		t.Errorf("svc2 token = %q, want %q", got2, "pass2")
	}

	// Read file that doesn't exist returns ErrNotFound.
	_, err = readFileToken(filepath.Join(dir, "nonexistent.json"), "svc1", "user1")
	if !errors.Is(err, keyring.ErrNotFound) {
		t.Errorf("expected ErrNotFound for missing file, got %v", err)
	}
}

func TestIsNotFound(t *testing.T) {
	if !isNotFound(keyring.ErrNotFound) {
		t.Error("expected isNotFound(keyring.ErrNotFound) = true")
	}
	if isNotFound(fmt.Errorf("some other error")) {
		t.Error("expected isNotFound(other error) = false")
	}
	// Wrapped ErrNotFound should also be detected.
	wrapped := fmt.Errorf("wrapped: %w", keyring.ErrNotFound)
	if !isNotFound(wrapped) {
		t.Error("expected isNotFound(wrapped ErrNotFound) = true")
	}
}

func TestReadFileTokenEmptyPath(t *testing.T) {
	_, err := readFileToken("", "svc", "user")
	if !errors.Is(err, keyring.ErrNotFound) {
		t.Errorf("expected ErrNotFound for empty path, got %v", err)
	}
}

func TestWriteFileTokenEmptyPath(t *testing.T) {
	err := writeFileToken("", "svc", "user", "pass")
	if err == nil {
		t.Error("expected error for empty path, got nil")
	}
	if !errors.Is(err, os.ErrInvalid) {
		t.Errorf("expected os.ErrInvalid, got %v", err)
	}
}
