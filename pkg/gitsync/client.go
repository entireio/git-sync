package gitsync

import (
	"context"
	"fmt"
	"net/http"

	"github.com/soph/git-sync/internal/validation"
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
	return internalbridge.FromProbeResult(result), nil
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
	return internalbridge.FromSyncResult(result), nil
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
	return internalbridge.FromSyncResult(result), nil
}

func (c *Client) buildProbeConfig(ctx context.Context, req ProbeRequest) (internalbridge.Config, error) {
	sourceAuth, err := c.authFor(ctx, req.Source, SourceRole)
	if err != nil {
		return internalbridge.Config{}, err
	}
	var target *internalbridge.Endpoint
	targetAuth := internalbridge.EndpointAuth{}
	if req.Target != nil {
		resolvedTargetAuth, err := c.authFor(ctx, *req.Target, TargetRole)
		if err != nil {
			return internalbridge.Config{}, err
		}
		target = ptr(bridgeEndpoint(*req.Target))
		targetAuth = bridgeEndpointAuth(resolvedTargetAuth)
	}
	return internalbridge.ProbeConfig(
		bridgeEndpoint(req.Source),
		bridgeEndpointAuth(sourceAuth),
		target,
		targetAuth,
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
		bridgeEndpoint(source),
		bridgeEndpointAuth(sourceAuth),
		bridgeEndpoint(target),
		bridgeEndpointAuth(targetAuth),
		bridgeScope(scope),
		bridgePolicy(policy),
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
	if _, err := validation.NormalizeProtocolMode(string(r.Policy.Protocol)); err != nil {
		return err
	}
	if _, err := validation.ValidateMappings(validationMappings(r.Scope.Mappings)); err != nil {
		return err
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
	if _, err := validation.NormalizeProtocolMode(string(r.Policy.Protocol)); err != nil {
		return err
	}
	if _, err := validation.ValidateMappings(validationMappings(r.Scope.Mappings)); err != nil {
		return err
	}
	return nil
}

func (r ProbeRequest) Validate() error {
	if r.Source.URL == "" {
		return fmt.Errorf("source URL is required")
	}
	if _, err := validation.NormalizeProtocolMode(string(r.Protocol)); err != nil {
		return err
	}
	return nil
}

func bridgeEndpoint(ep Endpoint) internalbridge.Endpoint {
	return internalbridge.Endpoint{URL: ep.URL}
}

func bridgeEndpointAuth(auth EndpointAuth) internalbridge.EndpointAuth {
	return internalbridge.EndpointAuth{
		Username:      auth.Username,
		Token:         auth.Token,
		BearerToken:   auth.BearerToken,
		SkipTLSVerify: auth.SkipTLSVerify,
	}
}

func bridgeScope(scope RefScope) internalbridge.RefScope {
	mappings := make([]internalbridge.RefMapping, 0, len(scope.Mappings))
	for _, mapping := range scope.Mappings {
		mappings = append(mappings, internalbridge.RefMapping{
			Source: mapping.Source,
			Target: mapping.Target,
		})
	}
	return internalbridge.RefScope{
		Branches: append([]string(nil), scope.Branches...),
		Mappings: mappings,
	}
}

func bridgePolicy(policy SyncPolicy) internalbridge.SyncPolicy {
	return internalbridge.SyncPolicy{
		IncludeTags: policy.IncludeTags,
		Force:       policy.Force,
		Prune:       policy.Prune,
		Protocol:    internalbridge.ProtocolMode(policy.Protocol),
	}
}

func ptr[T any](v T) *T {
	return &v
}

func validationMappings(mappings []RefMapping) []validation.RefMapping {
	out := make([]validation.RefMapping, 0, len(mappings))
	for _, mapping := range mappings {
		out = append(out, validation.RefMapping{
			Source: mapping.Source,
			Target: mapping.Target,
		})
	}
	return out
}
