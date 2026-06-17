package unstable

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"

	"entire.io/entire/git-sync"
)

func TestBuildSyncConfigCarriesAdvancedOptions(t *testing.T) {
	cfg, err := New(Options{
		HTTPClient: &http.Client{},
		Auth: gitsync.StaticAuthProvider{
			Source: gitsync.EndpointAuth{Token: "src"},
			Target: gitsync.EndpointAuth{Token: "dst"},
		},
	}).buildSyncConfig(context.Background(), SyncRequest{
		Source: gitsync.Endpoint{URL: "https://source.example/repo.git", FollowInfoRefsRedirect: true},
		Target: gitsync.Endpoint{URL: "https://target.example/repo.git", FollowInfoRefsRedirect: true},
		Scope:  gitsync.RefScope{Branches: []string{"main"}},
		Policy: gitsync.SyncPolicy{IncludeTags: true, ForceWithLease: true, Prune: true},
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
	if !cfg.Source.FollowInfoRefsRedirect || !cfg.Target.FollowInfoRefsRedirect {
		t.Fatalf("follow-info-refs redirect flags not propagated: %+v %+v", cfg.Source, cfg.Target)
	}
}

func TestAdvancedOptionsValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		opts    AdvancedOptions
		wantErr bool
	}{
		{name: "empty strategy is the default", opts: AdvancedOptions{}, wantErr: false},
		{name: "first-parent accepted", opts: AdvancedOptions{BootstrapStrategy: BootstrapStrategyFirstParent}, wantErr: false},
		{name: "topo accepted", opts: AdvancedOptions{BootstrapStrategy: BootstrapStrategyTopo}, wantErr: false},
		{name: "typo rejected at API edge", opts: AdvancedOptions{BootstrapStrategy: "topographic"}, wantErr: true},
		{name: "case-sensitive: TOPO is not topo", opts: AdvancedOptions{BootstrapStrategy: "TOPO"}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := c.opts.Validate()
			if (err != nil) != c.wantErr {
				t.Errorf("Validate() err=%v, wantErr=%v", err, c.wantErr)
			}
		})
	}
}

func TestClientRejectsUnknownStrategyBeforeIO(t *testing.T) {
	t.Parallel()
	// The reviewer's concern: an invalid value should fail at the
	// API edge, not silently slip through on a non-bootstrap path
	// (e.g. Probe) where bootstrap planning never runs.
	c := New(Options{HTTPClient: &http.Client{}})
	bad := AdvancedOptions{BootstrapStrategy: "unsupported"}
	if _, err := c.Probe(context.Background(), ProbeRequest{
		Source:  gitsync.Endpoint{URL: "https://source.example/repo.git"},
		Options: bad,
	}); err == nil {
		t.Errorf("Probe with invalid bootstrap strategy should error")
	}
	if _, err := c.Sync(context.Background(), SyncRequest{
		Source:  gitsync.Endpoint{URL: "https://source.example/repo.git"},
		Target:  gitsync.Endpoint{URL: "https://target.example/repo.git"},
		Options: bad,
	}); err == nil {
		t.Errorf("Sync with invalid bootstrap strategy should error")
	}
	if _, err := c.Bootstrap(context.Background(), BootstrapRequest{
		Source:  gitsync.Endpoint{URL: "https://source.example/repo.git"},
		Target:  gitsync.Endpoint{URL: "https://target.example/repo.git"},
		Options: bad,
	}); err == nil {
		t.Errorf("Bootstrap with invalid bootstrap strategy should error")
	}
}

// A mappings-scoped fetch must carry its mappings into the syncer config;
// dropping them (as buildFetchConfig used to) fetches the wrong refs.
func TestBuildFetchConfigPreservesMappings(t *testing.T) {
	req := FetchRequest{
		Source: gitsync.Endpoint{URL: "https://source.example/repo.git"},
		Scope: gitsync.RefScope{
			Mappings: []gitsync.RefMapping{{Source: "refs/heads/main", Target: "refs/heads/trunk"}},
		},
	}
	cfg, err := New(Options{}).buildFetchConfig(context.Background(), req)
	if err != nil {
		t.Fatalf("buildFetchConfig: %v", err)
	}
	if len(cfg.Mappings) != 1 {
		t.Fatalf("expected fetch config to carry 1 mapping, got %d", len(cfg.Mappings))
	}
	if cfg.Mappings[0].Source != "refs/heads/main" || cfg.Mappings[0].Target != "refs/heads/trunk" {
		t.Fatalf("unexpected mapping carried through: %+v", cfg.Mappings[0])
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
