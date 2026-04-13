package internalbridge

import (
	"context"
	"net/http"

	"github.com/soph/git-sync/internal/syncer"
	"github.com/soph/git-sync/internal/validation"
)

type ProtocolMode string

type Config = syncer.Config

const ProtocolAuto ProtocolMode = validation.ProtocolAuto

type RefMapping = validation.RefMapping

type Endpoint struct {
	URL string
}

type EndpointAuth struct {
	Username      string
	Token         string
	BearerToken   string
	SkipTLSVerify bool
}

type RefScope struct {
	Branches []string
	Mappings []RefMapping
}

type SyncPolicy struct {
	IncludeTags bool
	Force       bool
	Prune       bool
	Protocol    ProtocolMode
}

func ProbeConfig(source Endpoint, sourceAuth EndpointAuth, target *Endpoint, targetAuth EndpointAuth, protocol ProtocolMode, includeTags, collectStats bool, httpClient *http.Client) syncer.Config {
	cfg := syncer.Config{
		Source:       syncer.Endpoint{URL: source.URL, Username: sourceAuth.Username, Token: sourceAuth.Token, BearerToken: sourceAuth.BearerToken, SkipTLSVerify: sourceAuth.SkipTLSVerify},
		HTTPClient:   httpClient,
		IncludeTags:  includeTags,
		ShowStats:    collectStats,
		ProtocolMode: protocolString(protocol),
	}
	if target != nil {
		cfg.Target = syncer.Endpoint{URL: target.URL, Username: targetAuth.Username, Token: targetAuth.Token, BearerToken: targetAuth.BearerToken, SkipTLSVerify: targetAuth.SkipTLSVerify}
	}
	return cfg
}

func SyncConfig(source Endpoint, sourceAuth EndpointAuth, target Endpoint, targetAuth EndpointAuth, scope RefScope, policy SyncPolicy, collectStats, dryRun bool, httpClient *http.Client) syncer.Config {
	return syncer.Config{
		Source:                 syncer.Endpoint{URL: source.URL, Username: sourceAuth.Username, Token: sourceAuth.Token, BearerToken: sourceAuth.BearerToken, SkipTLSVerify: sourceAuth.SkipTLSVerify},
		Target:                 syncer.Endpoint{URL: target.URL, Username: targetAuth.Username, Token: targetAuth.Token, BearerToken: targetAuth.BearerToken, SkipTLSVerify: targetAuth.SkipTLSVerify},
		HTTPClient:             httpClient,
		Branches:               append([]string(nil), scope.Branches...),
		Mappings:               append([]RefMapping(nil), scope.Mappings...),
		IncludeTags:            policy.IncludeTags,
		DryRun:                 dryRun,
		ShowStats:              collectStats,
		Force:                  policy.Force,
		Prune:                  policy.Prune,
		ProtocolMode:           protocolString(policy.Protocol),
		MaterializedMaxObjects: syncer.DefaultMaterializedMaxObjects,
	}
}

func Probe(ctx context.Context, cfg syncer.Config) (syncer.ProbeResult, error) {
	return syncer.Probe(ctx, cfg)
}

func Run(ctx context.Context, cfg syncer.Config) (syncer.Result, error) {
	return syncer.Run(ctx, cfg)
}

func protocolString(mode ProtocolMode) string {
	if mode == "" {
		return string(ProtocolAuto)
	}
	return string(mode)
}
