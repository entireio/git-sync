package gitsync

import (
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

func TestBuildSyncConfigUsesDefaultProtocolAndMaterializedLimit(t *testing.T) {
	cfg := buildSyncConfig(
		Endpoint{URL: "https://source.example/repo.git"},
		EndpointAuth{Token: "src"},
		Endpoint{URL: "https://target.example/repo.git"},
		EndpointAuth{Token: "dst"},
		RefScope{Branches: []string{"main"}},
		SyncPolicy{},
		true,
		false,
	)

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
