package gitsync

import (
	"context"
	"fmt"
	"net/http"

	"github.com/soph/git-sync/pkg/gitsync/internalbridge"
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
	result, err := internalbridge.Probe(ctx, cfg)
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
	result, err := internalbridge.Run(ctx, cfg)
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
	result, err := internalbridge.Run(ctx, cfg)
	if err != nil {
		return SyncResult{}, err
	}
	return fromSyncerResult(result), nil
}

func (c *Client) buildProbeConfig(ctx context.Context, req ProbeRequest) (internalbridge.Config, error) {
	sourceAuth, err := c.authFor(ctx, req.Source, SourceRole)
	if err != nil {
		return internalbridge.Config{}, err
	}
	if req.Target != nil {
		targetAuth, err := c.authFor(ctx, *req.Target, TargetRole)
		if err != nil {
			return internalbridge.Config{}, err
		}
		return internalbridge.ProbeConfig(
			internalbridge.Endpoint{URL: req.Source.URL},
			internalbridge.EndpointAuth(sourceAuth),
			&internalbridge.Endpoint{URL: req.Target.URL},
			internalbridge.EndpointAuth(targetAuth),
			internalbridge.ProtocolMode(req.Protocol),
			req.IncludeTags,
			req.CollectStats,
			c.httpClient,
		), nil
	}
	return internalbridge.ProbeConfig(
		internalbridge.Endpoint{URL: req.Source.URL},
		internalbridge.EndpointAuth(sourceAuth),
		nil,
		internalbridge.EndpointAuth{},
		internalbridge.ProtocolMode(req.Protocol),
		req.IncludeTags,
		req.CollectStats,
		c.httpClient,
	), nil
}

func (c *Client) buildSyncConfig(ctx context.Context, source Endpoint, target Endpoint, scope RefScope, policy SyncPolicy, collectStats, dryRun bool) (internalbridge.Config, error) {
	sourceAuth, err := c.authFor(ctx, source, SourceRole)
	if err != nil {
		return internalbridge.Config{}, err
	}
	targetAuth, err := c.authFor(ctx, target, TargetRole)
	if err != nil {
		return internalbridge.Config{}, err
	}
	return internalbridge.SyncConfig(
		internalbridge.Endpoint{URL: source.URL},
		internalbridge.EndpointAuth(sourceAuth),
		internalbridge.Endpoint{URL: target.URL},
		internalbridge.EndpointAuth(targetAuth),
		internalbridge.RefScope{Branches: append([]string(nil), scope.Branches...), Mappings: append([]internalbridge.RefMapping(nil), scope.Mappings...)},
		internalbridge.SyncPolicy{
			IncludeTags: policy.IncludeTags,
			Force:       policy.Force,
			Prune:       policy.Prune,
			Protocol:    internalbridge.ProtocolMode(policy.Protocol),
		},
		collectStats,
		dryRun,
		c.httpClient,
	), nil
}

func (c *Client) authFor(ctx context.Context, endpoint Endpoint, role EndpointRole) (EndpointAuth, error) {
	if c.auth == nil {
		return EndpointAuth{}, nil
	}
	return c.auth.AuthFor(ctx, endpoint, role)
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
