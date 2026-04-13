package gitsync

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

func TestBuildSyncConfigUsesDefaultProtocolAndMaterializedLimit(t *testing.T) {
	cfg, err := New(Options{Auth: StaticAuthProvider{
		Source: EndpointAuth{Token: "src"},
		Target: EndpointAuth{Token: "dst"},
	}}).buildSyncConfig(
		context.Background(),
		Endpoint{URL: "https://source.example/repo.git"},
		Endpoint{URL: "https://target.example/repo.git"},
		RefScope{Branches: []string{"main"}},
		SyncPolicy{},
		true,
		false,
	)
	if err != nil {
		t.Fatalf("buildSyncConfig: %v", err)
	}

	if cfg.ProtocolMode != string(ProtocolAuto) {
		t.Fatalf("protocol mode = %q, want %q", cfg.ProtocolMode, ProtocolAuto)
	}
	if cfg.MaterializedMaxObjects <= 0 {
		t.Fatalf("materialized max objects = %d, want positive default", cfg.MaterializedMaxObjects)
	}
	if !cfg.ShowStats {
		t.Fatalf("show stats = false, want true")
	}
	if cfg.DryRun {
		t.Fatalf("dry run = true, want false")
	}
	if cfg.Source.Token != "src" || cfg.Target.Token != "dst" {
		t.Fatalf("unexpected token mapping: %+v %+v", cfg.Source, cfg.Target)
	}
}

func TestClientCarriesHTTPClientIntoSyncerConfig(t *testing.T) {
	base := &http.Client{}
	cfg, err := New(Options{
		HTTPClient: base,
		Auth:       StaticAuthProvider{},
	}).buildSyncConfig(
		context.Background(),
		Endpoint{URL: "https://source.example/repo.git"},
		Endpoint{URL: "https://target.example/repo.git"},
		RefScope{},
		SyncPolicy{},
		false,
		false,
	)
	if err != nil {
		t.Fatalf("buildSyncConfig: %v", err)
	}
	if cfg.HTTPClient != base {
		t.Fatalf("http client = %p, want %p", cfg.HTTPClient, base)
	}
}

type errAuthProvider struct{}

func (errAuthProvider) AuthFor(_ context.Context, _ Endpoint, _ EndpointRole) (EndpointAuth, error) {
	return EndpointAuth{}, fmt.Errorf("boom")
}

func TestValidateRequests(t *testing.T) {
	if err := (ProbeRequest{}).Validate(); err == nil {
		t.Fatalf("expected probe validation error")
	}
	if err := (PlanRequest{}).Validate(); err == nil {
		t.Fatalf("expected plan validation error")
	}
	if err := (SyncRequest{}).Validate(); err == nil {
		t.Fatalf("expected sync validation error")
	}
}

func TestFromSyncerResultZeroHashesAreEmptyStrings(t *testing.T) {
	got := hashString(plumbing.ZeroHash)
	if got != "" {
		t.Fatalf("hashString(zero) = %q, want empty string", got)
	}
}

func TestClientReturnsAuthProviderErrors(t *testing.T) {
	_, err := New(Options{Auth: errAuthProvider{}}).buildProbeConfig(context.Background(), ProbeRequest{
		Source: Endpoint{URL: "https://source.example/repo.git"},
	})
	if err == nil {
		t.Fatalf("expected auth provider error")
	}
}
