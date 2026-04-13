package unstable

import (
	"context"
	"net/http"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/soph/git-sync/internal/syncer"
	"github.com/soph/git-sync/internal/validation"
	"github.com/soph/git-sync/pkg/gitsync"
	"github.com/soph/git-sync/pkg/gitsync/internalbridge"
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
	CollectStats           bool
	MeasureMemory          bool
	Verbose                bool
	MaxPackBytes           int64
	BatchMaxPackBytes      int64
	MaterializedMaxObjects int
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
	return syncer.Probe(ctx, cfg)
}

func (c *Client) Plan(ctx context.Context, req SyncRequest) (Result, error) {
	planReq := req
	planReq.DryRun = true
	cfg, err := c.buildSyncConfig(ctx, planReq)
	if err != nil {
		return Result{}, err
	}
	return syncer.Run(ctx, cfg)
}

func (c *Client) Sync(ctx context.Context, req SyncRequest) (Result, error) {
	cfg, err := c.buildSyncConfig(ctx, req)
	if err != nil {
		return Result{}, err
	}
	return syncer.Run(ctx, cfg)
}

func (c *Client) Bootstrap(ctx context.Context, req BootstrapRequest) (Result, error) {
	cfg, err := c.buildBootstrapConfig(ctx, req)
	if err != nil {
		return Result{}, err
	}
	return syncer.Bootstrap(ctx, cfg)
}

func (c *Client) Fetch(ctx context.Context, req FetchRequest) (FetchResult, error) {
	cfg, err := c.buildFetchConfig(ctx, req)
	if err != nil {
		return FetchResult{}, err
	}
	return syncer.Fetch(ctx, cfg, append([]string(nil), req.HaveRefs...), append([]plumbing.Hash(nil), req.HaveHashes...))
}

func (c *Client) buildProbeConfig(ctx context.Context, req ProbeRequest) (syncer.Config, error) {
	sourceAuth, err := c.authFor(ctx, req.Source, gitsync.SourceRole)
	if err != nil {
		return syncer.Config{}, err
	}
	cfg := syncer.Config{
		Source:        syncerEndpoint(req.Source, sourceAuth),
		HTTPClient:    c.httpClient,
		IncludeTags:   req.IncludeTags,
		ShowStats:     req.Options.CollectStats,
		MeasureMemory: req.Options.MeasureMemory,
		ProtocolMode:  protocolString(req.Protocol),
	}
	if req.Target != nil {
		targetAuth, err := c.authFor(ctx, *req.Target, gitsync.TargetRole)
		if err != nil {
			return syncer.Config{}, err
		}
		cfg.Target = syncerEndpoint(*req.Target, targetAuth)
	}
	return cfg, nil
}

func (c *Client) buildSyncConfig(ctx context.Context, req SyncRequest) (syncer.Config, error) {
	sourceAuth, err := c.authFor(ctx, req.Source, gitsync.SourceRole)
	if err != nil {
		return syncer.Config{}, err
	}
	targetAuth, err := c.authFor(ctx, req.Target, gitsync.TargetRole)
	if err != nil {
		return syncer.Config{}, err
	}
	maxObjects := req.Options.MaterializedMaxObjects
	if maxObjects == 0 {
		maxObjects = DefaultMaterializedMaxObjects
	}
	return syncer.Config{
		Source:                 syncerEndpoint(req.Source, sourceAuth),
		Target:                 syncerEndpoint(req.Target, targetAuth),
		HTTPClient:             c.httpClient,
		Branches:               append([]string(nil), req.Scope.Branches...),
		Mappings:               validationMappings(req.Scope.Mappings),
		IncludeTags:            req.Policy.IncludeTags,
		DryRun:                 req.DryRun,
		ShowStats:              req.Options.CollectStats,
		MeasureMemory:          req.Options.MeasureMemory,
		Force:                  req.Policy.Force,
		Prune:                  req.Policy.Prune,
		MaterializedMaxObjects: maxObjects,
		ProtocolMode:           protocolString(req.Policy.Protocol),
		Verbose:                req.Options.Verbose,
	}, nil
}

func (c *Client) buildBootstrapConfig(ctx context.Context, req BootstrapRequest) (syncer.Config, error) {
	sourceAuth, err := c.authFor(ctx, req.Source, gitsync.SourceRole)
	if err != nil {
		return syncer.Config{}, err
	}
	targetAuth, err := c.authFor(ctx, req.Target, gitsync.TargetRole)
	if err != nil {
		return syncer.Config{}, err
	}
	return syncer.Config{
		Source:            syncerEndpoint(req.Source, sourceAuth),
		Target:            syncerEndpoint(req.Target, targetAuth),
		HTTPClient:        c.httpClient,
		Branches:          append([]string(nil), req.Scope.Branches...),
		Mappings:          validationMappings(req.Scope.Mappings),
		IncludeTags:       req.IncludeTags,
		ShowStats:         req.Options.CollectStats,
		MeasureMemory:     req.Options.MeasureMemory,
		MaxPackBytes:      req.Options.MaxPackBytes,
		BatchMaxPackBytes: req.Options.BatchMaxPackBytes,
		ProtocolMode:      protocolString(req.Protocol),
		Verbose:           req.Options.Verbose,
	}, nil
}

func (c *Client) buildFetchConfig(ctx context.Context, req FetchRequest) (syncer.Config, error) {
	sourceAuth, err := c.authFor(ctx, req.Source, gitsync.SourceRole)
	if err != nil {
		return syncer.Config{}, err
	}
	return syncer.Config{
		Source:        syncerEndpoint(req.Source, sourceAuth),
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
	return c.auth.AuthFor(ctx, endpoint, role)
}

func protocolString(mode gitsync.ProtocolMode) string {
	if mode == "" {
		return string(gitsync.ProtocolAuto)
	}
	return string(mode)
}

func syncerEndpoint(endpoint gitsync.Endpoint, auth gitsync.EndpointAuth) syncer.Endpoint {
	return internalbridge.ToSyncerEndpoint(
		internalbridge.Endpoint{URL: endpoint.URL},
		internalbridge.EndpointAuth{
			Username:      auth.Username,
			Token:         auth.Token,
			BearerToken:   auth.BearerToken,
			SkipTLSVerify: auth.SkipTLSVerify,
		},
	)
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
