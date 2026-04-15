package unstable

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entirehq/git-sync/pkg/gitsync"
)

func TestBuildSyncConfigCarriesAdvancedOptions(t *testing.T) {
	cfg, err := New(Options{
		HTTPClient: &http.Client{},
		Auth: gitsync.StaticAuthProvider{
			Source: gitsync.EndpointAuth{Token: "src"},
			Target: gitsync.EndpointAuth{Token: "dst"},
		},
	}).buildSyncConfig(context.Background(), SyncRequest{
		Source: gitsync.Endpoint{URL: "https://source.example/repo.git"},
		Target: gitsync.Endpoint{URL: "https://target.example/repo.git"},
		Scope:  gitsync.RefScope{Branches: []string{"main"}},
		Policy: gitsync.SyncPolicy{IncludeTags: true, Force: true, Prune: true},
		DryRun: true,
		Options: AdvancedOptions{
			CollectStats:           true,
			MeasureMemory:          true,
			Verbose:                true,
			MaterializedMaxObjects: 123,
		},
	})
	if err != nil {
		t.Fatalf("buildSyncConfig: %v", err)
	}
	if !cfg.DryRun || !cfg.ShowStats || !cfg.MeasureMemory || !cfg.Verbose {
		t.Fatalf("advanced booleans not propagated: %+v", cfg)
	}
	if cfg.MaterializedMaxObjects != 123 {
		t.Fatalf("materialized max objects = %d, want 123", cfg.MaterializedMaxObjects)
	}
	if cfg.Source.Token != "src" || cfg.Target.Token != "dst" {
		t.Fatalf("auth not propagated: %+v %+v", cfg.Source, cfg.Target)
	}
}

func TestBuildFetchConfigCopiesHaveHashesAtCallSite(t *testing.T) {
	req := FetchRequest{
		Source:     gitsync.Endpoint{URL: "https://source.example/repo.git"},
		HaveHashes: []plumbing.Hash{plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
	}
	cfg, err := New(Options{}).buildFetchConfig(context.Background(), req)
	if err != nil {
		t.Fatalf("buildFetchConfig: %v", err)
	}
	if cfg.Source.URL == "" {
		t.Fatalf("source URL not set")
	}
}
