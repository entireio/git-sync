package gitsync

import (
	"context"
	"fmt"
	"net/http"

	"github.com/soph/git-sync/internal/syncer"
)

// Options configures a Client. It is intentionally small in the first public cut.
type Options struct {
	HTTPClient *http.Client
	Auth       AuthProvider
}

// Client provides the public orchestration API for git-sync.
type Client struct {
	httpClient *http.Client
	auth       AuthProvider
}

// New constructs a new Client.
func New(opts Options) *Client {
	return &Client{httpClient: opts.HTTPClient, auth: opts.Auth}
}

// Probe inspects a source remote and optional target remote.
func (c *Client) Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error) {
	if err := req.Validate(); err != nil {
		return ProbeResult{}, err
	}
	cfg, err := c.buildProbeConfig(ctx, req)
	if err != nil {
		return ProbeResult{}, err
	}
	result, err := syncer.Probe(ctx, cfg)
	if err != nil {
		return ProbeResult{}, err
	}
	return fromSyncerProbeResult(result), nil
}

// Plan computes ref actions without pushing.
func (c *Client) Plan(ctx context.Context, req PlanRequest) (PlanResult, error) {
	if err := req.Validate(); err != nil {
		return PlanResult{}, err
	}
	cfg, err := c.buildSyncConfig(ctx, req.Source, req.Target, req.Scope, req.Policy, req.CollectStats, true)
	if err != nil {
		return PlanResult{}, err
	}
	result, err := syncer.Run(ctx, cfg)
	if err != nil {
		return PlanResult{}, err
	}
	return fromSyncerResult(result), nil
}

// Sync executes a sync between two remotes.
func (c *Client) Sync(ctx context.Context, req SyncRequest) (SyncResult, error) {
	if err := req.Validate(); err != nil {
		return SyncResult{}, err
	}
	cfg, err := c.buildSyncConfig(ctx, req.Source, req.Target, req.Scope, req.Policy, req.CollectStats, false)
	if err != nil {
		return SyncResult{}, err
	}
	result, err := syncer.Run(ctx, cfg)
	if err != nil {
		return SyncResult{}, err
	}
	return fromSyncerResult(result), nil
}

func (c *Client) buildProbeConfig(ctx context.Context, req ProbeRequest) (syncer.Config, error) {
	sourceAuth, err := c.authFor(ctx, req.Source, SourceRole)
	if err != nil {
		return syncer.Config{}, err
	}
	cfg := syncer.Config{
		Source:       syncer.Endpoint{URL: req.Source.URL, Username: sourceAuth.Username, Token: sourceAuth.Token, BearerToken: sourceAuth.BearerToken, SkipTLSVerify: sourceAuth.SkipTLSVerify},
		HTTPClient:   c.httpClient,
		IncludeTags:  req.IncludeTags,
		ShowStats:    req.CollectStats,
		ProtocolMode: string(req.Protocol),
	}
	if req.Target != nil {
		targetAuth, err := c.authFor(ctx, *req.Target, TargetRole)
		if err != nil {
			return syncer.Config{}, err
		}
		cfg.Target = syncer.Endpoint{URL: req.Target.URL, Username: targetAuth.Username, Token: targetAuth.Token, BearerToken: targetAuth.BearerToken, SkipTLSVerify: targetAuth.SkipTLSVerify}
	}
	return cfg, nil
}

func (c *Client) buildSyncConfig(ctx context.Context, source Endpoint, target Endpoint, scope RefScope, policy SyncPolicy, collectStats, dryRun bool) (syncer.Config, error) {
	sourceAuth, err := c.authFor(ctx, source, SourceRole)
	if err != nil {
		return syncer.Config{}, err
	}
	targetAuth, err := c.authFor(ctx, target, TargetRole)
	if err != nil {
		return syncer.Config{}, err
	}
	return syncer.Config{
		Source:                 syncer.Endpoint{URL: source.URL, Username: sourceAuth.Username, Token: sourceAuth.Token, BearerToken: sourceAuth.BearerToken, SkipTLSVerify: sourceAuth.SkipTLSVerify},
		Target:                 syncer.Endpoint{URL: target.URL, Username: targetAuth.Username, Token: targetAuth.Token, BearerToken: targetAuth.BearerToken, SkipTLSVerify: targetAuth.SkipTLSVerify},
		HTTPClient:             c.httpClient,
		Branches:               append([]string(nil), scope.Branches...),
		Mappings:               append([]RefMapping(nil), scope.Mappings...),
		IncludeTags:            policy.IncludeTags,
		DryRun:                 dryRun,
		ShowStats:              collectStats,
		Force:                  policy.Force,
		Prune:                  policy.Prune,
		ProtocolMode:           protocolString(policy.Protocol),
		MaterializedMaxObjects: syncer.DefaultMaterializedMaxObjects,
	}, nil
}

func (c *Client) authFor(ctx context.Context, endpoint Endpoint, role EndpointRole) (EndpointAuth, error) {
	if c.auth == nil {
		return EndpointAuth{}, nil
	}
	return c.auth.AuthFor(ctx, endpoint, role)
}

func protocolString(mode ProtocolMode) string {
	if mode == "" {
		return string(ProtocolAuto)
	}
	return string(mode)
}

func (r SyncRequest) Validate() error {
	if r.Source.URL == "" {
		return fmt.Errorf("source URL is required")
	}
	if r.Target.URL == "" {
		return fmt.Errorf("target URL is required")
	}
	return nil
}

func (r PlanRequest) Validate() error {
	if r.Source.URL == "" {
		return fmt.Errorf("source URL is required")
	}
	if r.Target.URL == "" {
		return fmt.Errorf("target URL is required")
	}
	return nil
}

func (r ProbeRequest) Validate() error {
	if r.Source.URL == "" {
		return fmt.Errorf("source URL is required")
	}
	return nil
}
