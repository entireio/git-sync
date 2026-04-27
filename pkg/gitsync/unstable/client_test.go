package unstable

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entirehq/git-sync/pkg/gitsync"
)

func TestBuildSyncConfigCarriesAdvancedOptions(t *testing.T) {
	srcPin := &url.URL{Scheme: "https", Host: "src.pinned"}
	dstPin := &url.URL{Scheme: "https", Host: "dst.pinned"}
	srcHook := func(*http.Response) *url.URL { return srcPin }
	dstHook := func(*http.Response) *url.URL { return dstPin }

	cfg, err := New(Options{
		HTTPClient: &http.Client{},
		Auth: gitsync.StaticAuthProvider{
			Source: gitsync.EndpointAuth{Token: "src"},
			Target: gitsync.EndpointAuth{Token: "dst"},
		},
	}).buildSyncConfig(context.Background(), SyncRequest{
		Source: gitsync.Endpoint{URL: "https://source.example/repo.git", AfterInfoRefs: srcHook},
		Target: gitsync.Endpoint{URL: "https://target.example/repo.git", AfterInfoRefs: dstHook},
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
	if cfg.Source.AfterInfoRefs == nil || cfg.Target.AfterInfoRefs == nil {
		t.Fatalf("AfterInfoRefs hooks not propagated: %+v %+v", cfg.Source, cfg.Target)
	}
	if cfg.Source.AfterInfoRefs(nil) != srcPin {
		t.Errorf("source hook returned wrong URL")
	}
	if cfg.Target.AfterInfoRefs(nil) != dstPin {
		t.Errorf("target hook returned wrong URL")
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
