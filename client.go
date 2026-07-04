package gitsync

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"entire.io/entire/git-sync/internal/syncer"
	"entire.io/entire/git-sync/internal/validation"
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
		return ProbeResult{}, fmt.Errorf("probe: %w", err)
	}
	return fromProbeResult(result), nil
}

// Plan computes ref actions without pushing.
func (c *Client) Plan(ctx context.Context, req PlanRequest) (PlanResult, error) {
	if err := req.Validate(); err != nil {
		return PlanResult{}, err
	}
	cfg, err := c.buildSyncConfig(ctx, SyncRequest(req), true)
	if err != nil {
		return PlanResult{}, err
	}
	result, err := syncer.Run(ctx, cfg)
	if err != nil {
		return PlanResult{}, fmt.Errorf("plan: %w", err)
	}
	return fromSyncResult(result), nil
}

// Sync executes a sync between two remotes.
func (c *Client) Sync(ctx context.Context, req SyncRequest) (SyncResult, error) {
	if err := req.Validate(); err != nil {
		return SyncResult{}, err
	}
	cfg, err := c.buildSyncConfig(ctx, req, false)
	if err != nil {
		return SyncResult{}, err
	}
	result, err := syncer.Run(ctx, cfg)
	if err != nil {
		return SyncResult{}, fmt.Errorf("sync: %w", err)
	}
	return fromSyncResult(result), nil
}

// Replicate executes source-authoritative relay-only replication between two remotes.
func (c *Client) Replicate(ctx context.Context, req SyncRequest) (SyncResult, error) {
	req.Policy.Mode = ModeReplicate
	return c.Sync(ctx, req)
}

func (c *Client) buildProbeConfig(ctx context.Context, req ProbeRequest) (syncer.Config, error) {
	sourceAuth, err := c.authFor(ctx, req.Source, SourceRole)
	if err != nil {
		return syncer.Config{}, err
	}
	cfg := syncer.Config{
		Source:             syncerEndpoint(req.Source, sourceAuth),
		HTTPClient:         c.httpClient,
		IncludeTags:        req.IncludeTags,
		AllRefs:            req.AllRefs,
		ExcludeRefPrefixes: append([]string(nil), req.ExcludeRefPrefixes...),
		ShowStats:          req.CollectStats,
		ProtocolMode:       string(req.Protocol),
	}
	if req.Target != nil {
		targetAuth, err := c.authFor(ctx, *req.Target, TargetRole)
		if err != nil {
			return syncer.Config{}, err
		}
		cfg.Target = syncerEndpoint(*req.Target, targetAuth)
	}
	return cfg, nil
}

func (c *Client) buildSyncConfig(ctx context.Context, req SyncRequest, dryRun bool) (syncer.Config, error) {
	sourceAuth, err := c.authFor(ctx, req.Source, SourceRole)
	if err != nil {
		return syncer.Config{}, err
	}
	targetAuth, err := c.authFor(ctx, req.Target, TargetRole)
	if err != nil {
		return syncer.Config{}, err
	}
	return syncer.Config{
		Source:                 syncerEndpoint(req.Source, sourceAuth),
		Target:                 syncerEndpoint(req.Target, targetAuth),
		HTTPClient:             c.httpClient,
		Branches:               append([]string(nil), req.Scope.Branches...),
		Mappings:               validationMappings(req.Scope.Mappings),
		AllRefs:                req.Scope.AllRefs,
		ExcludeRefPrefixes:     append([]string(nil), req.Scope.ExcludeRefPrefixes...),
		ExcludeRefs:            append([]string(nil), req.Scope.ExcludeRefs...),
		IncludeTags:            req.Policy.IncludeTags,
		DryRun:                 dryRun,
		ShowStats:              req.CollectStats,
		Mode:                   string(req.Policy.Mode),
		ForceWithLease:         req.Policy.ForceWithLease,
		ForceBlind:             req.Policy.ForceBlind,
		Prune:                  req.Policy.Prune,
		BestEffort:             req.Policy.BestEffort,
		ProtocolMode:           string(req.Policy.Protocol),
		MaterializedMaxObjects: syncer.DefaultMaterializedMaxObjects,
	}, nil
}

func (c *Client) authFor(ctx context.Context, endpoint Endpoint, role EndpointRole) (EndpointAuth, error) {
	if c.auth == nil {
		return EndpointAuth{}, nil
	}
	auth, err := c.auth.AuthFor(ctx, endpoint, role)
	if err != nil {
		return EndpointAuth{}, fmt.Errorf("resolve auth for %s: %w", role, err)
	}
	return auth, nil
}

func (r SyncRequest) Validate() error {
	return validateSyncFields(r.Source, r.Target, r.Scope, r.Policy)
}

func (r PlanRequest) Validate() error {
	return validateSyncFields(r.Source, r.Target, r.Scope, r.Policy)
}

// validateSyncFields validates the fields shared by SyncRequest and
// PlanRequest, whose Validate methods are otherwise identical.
func validateSyncFields(source, target Endpoint, scope RefScope, policy SyncPolicy) error {
	if source.URL == "" {
		return errors.New("source URL is required")
	}
	if target.URL == "" {
		return errors.New("target URL is required")
	}
	if err := validateOperationMode(policy.Mode); err != nil {
		return err
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	if _, err := validation.NormalizeProtocolMode(string(policy.Protocol)); err != nil {
		return fmt.Errorf("normalize protocol: %w", err)
	}
	if _, err := validation.ValidateMappings(validationMappings(scope.Mappings), scope.AllRefs); err != nil {
		return fmt.Errorf("validate mappings: %w", err)
	}
	return nil
}

func (r ProbeRequest) Validate() error {
	if r.Source.URL == "" {
		return errors.New("source URL is required")
	}
	if r.Target != nil && r.Target.URL == "" {
		return errors.New("target URL is required when target endpoint is provided")
	}
	if _, err := validation.NormalizeProtocolMode(string(r.Protocol)); err != nil {
		return fmt.Errorf("normalize protocol: %w", err)
	}
	return nil
}

func syncerEndpoint(ep Endpoint, auth EndpointAuth) syncer.Endpoint {
	return syncer.Endpoint{
		URL:                    ep.URL,
		Username:               auth.Username,
		Token:                  auth.Token,
		BearerToken:            auth.BearerToken,
		SkipTLSVerify:          auth.SkipTLSVerify,
		FollowInfoRefsRedirect: ep.FollowInfoRefsRedirect,
	}
}

func validateOperationMode(mode OperationMode) error {
	switch mode {
	case "", ModeSync, ModeReplicate:
		return nil
	default:
		return fmt.Errorf("unsupported operation mode %q", mode)
	}
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
