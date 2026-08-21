package unstable

import (
	"context"
	"net/http"
	"reflect"
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

// AdvancedOptions carries MaterializedMaxObjects, and the fetch path is the one
// place it can be set — so silently dropping it left callers with no way to
// bound a fetch at all.
func TestBuildFetchConfigThreadsMaterializedMaxObjects(t *testing.T) {
	cfg, err := New(Options{}).buildFetchConfig(context.Background(), FetchRequest{
		Source:  gitsync.Endpoint{URL: "https://example.test/r.git"},
		Options: AdvancedOptions{MaterializedMaxObjects: 1234},
	})
	if err != nil {
		t.Fatalf("buildFetchConfig: %v", err)
	}
	if cfg.MaterializedMaxObjects != 1234 {
		t.Errorf("MaterializedMaxObjects = %d, want 1234", cfg.MaterializedMaxObjects)
	}
}

// Every bool on gitsync.SyncPolicy must reach the syncer config, checked by
// reflection rather than by an enumerated list.
//
// This exists because the hand-written test above did not catch three policy
// fields that buildSyncConfig silently dropped: a list of fields only covers
// what someone remembered to add to it, so the failure mode is a new policy
// field that is accepted by the API and then ignored, with no test going red.
// Reflection inverts that — a new bool is covered the moment it is declared,
// and this fails until it is threaded.
//
// Matching is by identical field name in syncer.Config, which is the
// convention every policy bool follows today. A future policy field whose
// config counterpart is deliberately named differently (or deliberately
// absent) will fail here and should be added to skip with a reason, not
// renamed to satisfy the test.
func TestBuildSyncConfigThreadsEveryPolicyBool(t *testing.T) {
	skip := map[string]string{}

	policyType := reflect.TypeOf(gitsync.SyncPolicy{})
	for i := range policyType.NumField() {
		field := policyType.Field(i)
		if field.Type.Kind() != reflect.Bool {
			continue
		}
		if reason, ok := skip[field.Name]; ok {
			t.Logf("skipping %s: %s", field.Name, reason)
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			// One field at a time, so a failure names the culprit and no
			// mutually-exclusive pair is ever set together.
			policy := gitsync.SyncPolicy{}
			reflect.ValueOf(&policy).Elem().FieldByName(field.Name).SetBool(true)

			cfg, err := New(Options{HTTPClient: &http.Client{}}).buildSyncConfig(context.Background(), SyncRequest{
				Source: gitsync.Endpoint{URL: "https://source.example/repo.git"},
				Target: gitsync.Endpoint{URL: "https://target.example/repo.git"},
				Policy: policy,
			})
			if err != nil {
				t.Fatalf("buildSyncConfig: %v", err)
			}

			got := reflect.ValueOf(cfg).FieldByName(field.Name)
			if !got.IsValid() {
				t.Fatalf("syncer.Config has no %s field; thread it, or add it to skip with a reason", field.Name)
			}
			if !got.Bool() {
				t.Errorf("SyncPolicy.%s = true was dropped by buildSyncConfig", field.Name)
			}
		})
	}
}
