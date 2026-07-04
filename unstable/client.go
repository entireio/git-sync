package unstable

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-git/go-git/v6/plumbing"

	"entire.io/entire/git-sync"
	"entire.io/entire/git-sync/internal/syncer"
	"entire.io/entire/git-sync/internal/validation"
)

const DefaultMaterializedMaxObjects = syncer.DefaultMaterializedMaxObjects

type (
	Result      = syncer.Result
	ProbeResult = syncer.ProbeResult
	FetchResult = syncer.FetchResult
	RefInfo     = syncer.RefInfo
	Stats       = syncer.Stats
	Measurement = syncer.Measurement
)

type Options struct {
	HTTPClient *http.Client
	Auth       gitsync.AuthProvider
}

type Client struct {
	httpClient *http.Client
	auth       gitsync.AuthProvider
}

type AdvancedOptions struct {
	CollectStats           bool   `json:"collectStats"`
	MeasureMemory          bool   `json:"measureMemory"`
	Verbose                bool   `json:"verbose"`
	Progress               bool   `json:"progress"`
	MaxPackBytes           int64  `json:"maxPackBytes"`
	TargetMaxPackBytes     int64  `json:"targetMaxPackBytes"`
	TargetMaxRefUpdates    int    `json:"targetMaxRefUpdates"`
	MaterializedMaxObjects int    `json:"materializedMaxObjects"`
	BootstrapStrategy      string `json:"bootstrapStrategy,omitempty"`
}

// BootstrapStrategy values accepted by AdvancedOptions.BootstrapStrategy.
// Empty is treated as the default (first-parent).
//
// BootstrapStrategyTopo additionally requires the target to allow
// non-fast-forward updates under the refs/gitsync/ namespace, since
// successive checkpoints under topological ordering aren't guaranteed
// to be in an ancestor-descendant relationship and the internal temp
// ref may receive non-ff updates between batches. Major hosts allow
// this by default; only locked-down deployments need to be checked.
const (
	BootstrapStrategyFirstParent = "first-parent"
	BootstrapStrategyTopo        = "topo"
)

// Validate rejects unknown values up front so callers don't have to
// wait for the deep bootstrap planning path to surface them. It runs
// on every request method regardless of whether the value will end
// up being consulted, since silently accepting a typo on a
// non-bootstrap path is worse than failing fast.
func (o AdvancedOptions) Validate() error {
	switch o.BootstrapStrategy {
	case "", BootstrapStrategyFirstParent, BootstrapStrategyTopo:
		return nil
	default:
		return fmt.Errorf("unsupported bootstrap strategy %q (want %q or %q)",
			o.BootstrapStrategy, BootstrapStrategyFirstParent, BootstrapStrategyTopo)
	}
}

type ProbeRequest struct {
	Source             gitsync.Endpoint
	Target             *gitsync.Endpoint
	IncludeTags        bool
	AllRefs            bool
	ExcludeRefPrefixes []string
	Protocol           gitsync.ProtocolMode
	Options            AdvancedOptions
}

type SyncRequest struct {
	Source  gitsync.Endpoint
	Target  gitsync.Endpoint
	Scope   gitsync.RefScope
	Policy  gitsync.SyncPolicy
	DryRun  bool
	Options AdvancedOptions
}

type BootstrapRequest struct {
	Source      gitsync.Endpoint
	Target      gitsync.Endpoint
	Scope       gitsync.RefScope
	IncludeTags bool
	BestEffort  bool
	Protocol    gitsync.ProtocolMode
	Options     AdvancedOptions
}

type FetchRequest struct {
	Source      gitsync.Endpoint
	Scope       gitsync.RefScope
	IncludeTags bool
	Protocol    gitsync.ProtocolMode
	HaveRefs    []string
	HaveHashes  []plumbing.Hash
	Options     AdvancedOptions
}

func New(opts Options) *Client {
	return &Client{httpClient: opts.HTTPClient, auth: opts.Auth}
}

func (c *Client) Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error) {
	if err := req.Options.Validate(); err != nil {
		return ProbeResult{}, fmt.Errorf("probe: %w", err)
	}
	cfg, err := c.buildProbeConfig(ctx, req)
	if err != nil {
		return ProbeResult{}, err
	}
	result, err := syncer.Probe(ctx, cfg)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("probe: %w", err)
	}
	return result, nil
}

func (c *Client) Plan(ctx context.Context, req SyncRequest) (Result, error) {
	if err := req.Options.Validate(); err != nil {
		return Result{}, fmt.Errorf("plan: %w", err)
	}
	if err := req.Policy.Validate(); err != nil {
		return Result{}, fmt.Errorf("plan: %w", err)
	}
	planReq := req
	planReq.DryRun = true
	cfg, err := c.buildSyncConfig(ctx, planReq)
	if err != nil {
		return Result{}, err
	}
	result, err := syncer.Run(ctx, cfg)
	if err != nil {
		return Result{}, fmt.Errorf("plan: %w", err)
	}
	return result, nil
}

func (c *Client) Sync(ctx context.Context, req SyncRequest) (Result, error) {
	if err := req.Options.Validate(); err != nil {
		return Result{}, fmt.Errorf("sync: %w", err)
	}
	if err := req.Policy.Validate(); err != nil {
		return Result{}, fmt.Errorf("sync: %w", err)
	}
	cfg, err := c.buildSyncConfig(ctx, req)
	if err != nil {
		return Result{}, err
	}
	result, err := syncer.Run(ctx, cfg)
	if err != nil {
		return Result{}, fmt.Errorf("sync: %w", err)
	}
	return result, nil
}

func (c *Client) Replicate(ctx context.Context, req SyncRequest) (Result, error) {
	if err := req.Options.Validate(); err != nil {
		return Result{}, fmt.Errorf("replicate: %w", err)
	}
	req.Policy.Mode = gitsync.ModeReplicate
	if err := req.Policy.Validate(); err != nil {
		return Result{}, fmt.Errorf("replicate: %w", err)
	}
	cfg, err := c.buildSyncConfig(ctx, req)
	if err != nil {
		return Result{}, err
	}
	result, err := syncer.Run(ctx, cfg)
	if err != nil {
		return Result{}, fmt.Errorf("replicate: %w", err)
	}
	return result, nil
}

func (c *Client) Bootstrap(ctx context.Context, req BootstrapRequest) (Result, error) {
	if err := req.Options.Validate(); err != nil {
		return Result{}, fmt.Errorf("bootstrap: %w", err)
	}
	cfg, err := c.buildBootstrapConfig(ctx, req)
	if err != nil {
		return Result{}, err
	}
	result, err := syncer.Bootstrap(ctx, cfg)
	if err != nil {
		return Result{}, fmt.Errorf("bootstrap: %w", err)
	}
	return result, nil
}

func (c *Client) Fetch(ctx context.Context, req FetchRequest) (FetchResult, error) {
	if err := req.Options.Validate(); err != nil {
		return FetchResult{}, fmt.Errorf("fetch: %w", err)
	}
	cfg, err := c.buildFetchConfig(ctx, req)
	if err != nil {
		return FetchResult{}, err
	}
	result, err := syncer.Fetch(ctx, cfg, append([]string(nil), req.HaveRefs...), append([]plumbing.Hash(nil), req.HaveHashes...))
	if err != nil {
		return FetchResult{}, fmt.Errorf("fetch: %w", err)
	}
	return result, nil
}

func (c *Client) buildProbeConfig(ctx context.Context, req ProbeRequest) (syncer.Config, error) {
	source, err := c.resolveEndpoint(ctx, req.Source, gitsync.SourceRole)
	if err != nil {
		return syncer.Config{}, err
	}
	cfg := syncer.Config{
		Source:             source,
		HTTPClient:         c.httpClient,
		IncludeTags:        req.IncludeTags,
		AllRefs:            req.AllRefs,
		ExcludeRefPrefixes: append([]string(nil), req.ExcludeRefPrefixes...),
		ShowStats:          req.Options.CollectStats,
		MeasureMemory:      req.Options.MeasureMemory,
		Progress:           req.Options.Progress,
		ProtocolMode:       string(req.Protocol),
		Verbose:            req.Options.Verbose,
	}
	if req.Target != nil {
		target, err := c.resolveEndpoint(ctx, *req.Target, gitsync.TargetRole)
		if err != nil {
			return syncer.Config{}, err
		}
		cfg.Target = target
	}
	return cfg, nil
}

func (c *Client) buildSyncConfig(ctx context.Context, req SyncRequest) (syncer.Config, error) {
	source, err := c.resolveEndpoint(ctx, req.Source, gitsync.SourceRole)
	if err != nil {
		return syncer.Config{}, err
	}
	target, err := c.resolveEndpoint(ctx, req.Target, gitsync.TargetRole)
	if err != nil {
		return syncer.Config{}, err
	}
	maxObjects := req.Options.MaterializedMaxObjects
	if maxObjects == 0 {
		maxObjects = DefaultMaterializedMaxObjects
	}
	return syncer.Config{
		Source:                 source,
		Target:                 target,
		HTTPClient:             c.httpClient,
		Branches:               append([]string(nil), req.Scope.Branches...),
		Mappings:               validationMappings(req.Scope.Mappings),
		AllRefs:                req.Scope.AllRefs,
		ExcludeRefPrefixes:     append([]string(nil), req.Scope.ExcludeRefPrefixes...),
		IncludeTags:            req.Policy.IncludeTags,
		DryRun:                 req.DryRun,
		ShowStats:              req.Options.CollectStats,
		MeasureMemory:          req.Options.MeasureMemory,
		Progress:               req.Options.Progress,
		Mode:                   string(req.Policy.Mode),
		ForceWithLease:         req.Policy.ForceWithLease,
		ForceBlind:             req.Policy.ForceBlind,
		Prune:                  req.Policy.Prune,
		BestEffort:             req.Policy.BestEffort,
		MaxPackBytes:           req.Options.MaxPackBytes,
		TargetMaxPackBytes:     req.Options.TargetMaxPackBytes,
		TargetMaxRefUpdates:    req.Options.TargetMaxRefUpdates,
		MaterializedMaxObjects: maxObjects,
		ProtocolMode:           string(req.Policy.Protocol),
		Verbose:                req.Options.Verbose,
		BootstrapStrategy:      req.Options.BootstrapStrategy,
	}, nil
}

func (c *Client) buildBootstrapConfig(ctx context.Context, req BootstrapRequest) (syncer.Config, error) {
	source, err := c.resolveEndpoint(ctx, req.Source, gitsync.SourceRole)
	if err != nil {
		return syncer.Config{}, err
	}
	target, err := c.resolveEndpoint(ctx, req.Target, gitsync.TargetRole)
	if err != nil {
		return syncer.Config{}, err
	}
	return syncer.Config{
		Source:              source,
		Target:              target,
		HTTPClient:          c.httpClient,
		Branches:            append([]string(nil), req.Scope.Branches...),
		Mappings:            validationMappings(req.Scope.Mappings),
		AllRefs:             req.Scope.AllRefs,
		ExcludeRefPrefixes:  append([]string(nil), req.Scope.ExcludeRefPrefixes...),
		IncludeTags:         req.IncludeTags,
		BestEffort:          req.BestEffort,
		ShowStats:           req.Options.CollectStats,
		MeasureMemory:       req.Options.MeasureMemory,
		Progress:            req.Options.Progress,
		MaxPackBytes:        req.Options.MaxPackBytes,
		TargetMaxPackBytes:  req.Options.TargetMaxPackBytes,
		TargetMaxRefUpdates: req.Options.TargetMaxRefUpdates,
		ProtocolMode:        string(req.Protocol),
		Verbose:             req.Options.Verbose,
		BootstrapStrategy:   req.Options.BootstrapStrategy,
	}, nil
}

func (c *Client) buildFetchConfig(ctx context.Context, req FetchRequest) (syncer.Config, error) {
	source, err := c.resolveEndpoint(ctx, req.Source, gitsync.SourceRole)
	if err != nil {
		return syncer.Config{}, err
	}
	return syncer.Config{
		Source:             source,
		HTTPClient:         c.httpClient,
		Branches:           append([]string(nil), req.Scope.Branches...),
		Mappings:           validationMappings(req.Scope.Mappings),
		AllRefs:            req.Scope.AllRefs,
		ExcludeRefPrefixes: append([]string(nil), req.Scope.ExcludeRefPrefixes...),
		IncludeTags:        req.IncludeTags,
		ShowStats:          req.Options.CollectStats,
		MeasureMemory:      req.Options.MeasureMemory,
		Progress:           req.Options.Progress,
		ProtocolMode:       string(req.Protocol),
		Verbose:            req.Options.Verbose,
	}, nil
}

func (c *Client) authFor(ctx context.Context, endpoint gitsync.Endpoint, role gitsync.EndpointRole) (gitsync.EndpointAuth, error) {
	if c.auth == nil {
		return gitsync.EndpointAuth{}, nil
	}
	auth, err := c.auth.AuthFor(ctx, endpoint, role)
	if err != nil {
		return gitsync.EndpointAuth{}, fmt.Errorf("resolve auth for %s: %w", role, err)
	}
	return auth, nil
}

func (c *Client) resolveEndpoint(ctx context.Context, endpoint gitsync.Endpoint, role gitsync.EndpointRole) (syncer.Endpoint, error) {
	auth, err := c.authFor(ctx, endpoint, role)
	if err != nil {
		return syncer.Endpoint{}, err
	}
	return syncerEndpoint(endpoint, auth), nil
}

func syncerEndpoint(endpoint gitsync.Endpoint, auth gitsync.EndpointAuth) syncer.Endpoint {
	return syncer.Endpoint{
		URL:                    endpoint.URL,
		Username:               auth.Username,
		Token:                  auth.Token,
		BearerToken:            auth.BearerToken,
		SkipTLSVerify:          auth.SkipTLSVerify,
		FollowInfoRefsRedirect: endpoint.FollowInfoRefsRedirect,
	}
}

func validationMappings(mappings []gitsync.RefMapping) []validation.RefMapping {
	out := make([]validation.RefMapping, 0, len(mappings))
	for _, mapping := range mappings {
		out = append(out, validation.RefMapping{
			Source: mapping.Source,
			Target: mapping.Target,
		})
	}
	return out
}
