package unstable

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-git/go-git/v6/plumbing"

	"entire.io/entire/git-sync/internal/syncer"
	"entire.io/entire/git-sync/internal/validation"
	"entire.io/entire/git-sync/pkg/gitsync"
	"entire.io/entire/git-sync/pkg/gitsync/internalbridge"
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
	CollectStats           bool  `json:"collectStats"`
	MeasureMemory          bool  `json:"measureMemory"`
	Verbose                bool  `json:"verbose"`
	MaxPackBytes           int64 `json:"maxPackBytes"`
	TargetMaxPackBytes     int64 `json:"targetMaxPackBytes"`
	MaterializedMaxObjects int   `json:"materializedMaxObjects"`
}

type ProbeRequest struct {
	Source      gitsync.Endpoint
	Target      *gitsync.Endpoint
	IncludeTags bool
	Protocol    gitsync.ProtocolMode
	Options     AdvancedOptions
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
	req.Policy.Mode = gitsync.ModeReplicate
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
		Source:        source,
		HTTPClient:    c.httpClient,
		IncludeTags:   req.IncludeTags,
		ShowStats:     req.Options.CollectStats,
		MeasureMemory: req.Options.MeasureMemory,
		ProtocolMode:  protocolString(req.Protocol),
		Verbose:       req.Options.Verbose,
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
		IncludeTags:            req.Policy.IncludeTags,
		DryRun:                 req.DryRun,
		ShowStats:              req.Options.CollectStats,
		MeasureMemory:          req.Options.MeasureMemory,
		Mode:                   operationModeString(req.Policy.Mode),
		Force:                  req.Policy.Force,
		Prune:                  req.Policy.Prune,
		MaxPackBytes:           req.Options.MaxPackBytes,
		TargetMaxPackBytes:     req.Options.TargetMaxPackBytes,
		MaterializedMaxObjects: maxObjects,
		ProtocolMode:           protocolString(req.Policy.Protocol),
		Verbose:                req.Options.Verbose,
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
		Source:             source,
		Target:             target,
		HTTPClient:         c.httpClient,
		Branches:           append([]string(nil), req.Scope.Branches...),
		Mappings:           validationMappings(req.Scope.Mappings),
		IncludeTags:        req.IncludeTags,
		ShowStats:          req.Options.CollectStats,
		MeasureMemory:      req.Options.MeasureMemory,
		MaxPackBytes:       req.Options.MaxPackBytes,
		TargetMaxPackBytes: req.Options.TargetMaxPackBytes,
		ProtocolMode:       protocolString(req.Protocol),
		Verbose:            req.Options.Verbose,
	}, nil
}

func (c *Client) buildFetchConfig(ctx context.Context, req FetchRequest) (syncer.Config, error) {
	source, err := c.resolveEndpoint(ctx, req.Source, gitsync.SourceRole)
	if err != nil {
		return syncer.Config{}, err
	}
	return syncer.Config{
		Source:        source,
		HTTPClient:    c.httpClient,
		Branches:      append([]string(nil), req.Scope.Branches...),
		IncludeTags:   req.IncludeTags,
		ShowStats:     req.Options.CollectStats,
		MeasureMemory: req.Options.MeasureMemory,
		ProtocolMode:  protocolString(req.Protocol),
		Verbose:       req.Options.Verbose,
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

func protocolString(mode gitsync.ProtocolMode) string {
	if mode == "" {
		return string(gitsync.ProtocolAuto)
	}
	return string(mode)
}

func operationModeString(mode gitsync.OperationMode) string {
	if mode == "" {
		return string(gitsync.ModeSync)
	}
	return string(mode)
}

func syncerEndpoint(endpoint gitsync.Endpoint, auth gitsync.EndpointAuth) syncer.Endpoint {
	return internalbridge.ToSyncerEndpoint(
		internalbridge.Endpoint{
			URL:                    endpoint.URL,
			FollowInfoRefsRedirect: endpoint.FollowInfoRefsRedirect,
		},
		internalbridge.EndpointAuth{
			Username:      auth.Username,
			Token:         auth.Token,
			BearerToken:   auth.BearerToken,
			SkipTLSVerify: auth.SkipTLSVerify,
		},
	)
}

func validationMappings(mappings []gitsync.RefMapping) []validation.RefMapping {
	bridgeMappings := make([]internalbridge.RefMapping, 0, len(mappings))
	for _, mapping := range mappings {
		bridgeMappings = append(bridgeMappings, internalbridge.RefMapping{
			Source: mapping.Source,
			Target: mapping.Target,
		})
	}
	return internalbridge.ToValidationMappings(bridgeMappings)
}
