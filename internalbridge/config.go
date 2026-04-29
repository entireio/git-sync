package internalbridge

import (
	"context"
	"net/http"

	"entire.io/entire/gitsync/internal/syncer"
	"entire.io/entire/gitsync/internal/validation"
)

type ProtocolMode string
type OperationMode string

type Config struct {
	raw syncer.Config
}

const ProtocolAuto ProtocolMode = validation.ProtocolAuto
const ProtocolV1 ProtocolMode = validation.ProtocolV1
const ProtocolV2 ProtocolMode = validation.ProtocolV2

const ModeSync OperationMode = "sync"
const ModeReplicate OperationMode = "replicate"

type RefMapping struct {
	Source string
	Target string
}

type Endpoint struct {
	URL                    string
	FollowInfoRefsRedirect bool
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
	Mode        OperationMode
	IncludeTags bool
	Force       bool
	Prune       bool
	Protocol    ProtocolMode
}

func ProbeConfig(source Endpoint, sourceAuth EndpointAuth, target *Endpoint, targetAuth EndpointAuth, protocol ProtocolMode, includeTags, collectStats bool, httpClient *http.Client) Config {
	cfg := syncer.Config{
		Source:       ToSyncerEndpoint(source, sourceAuth),
		HTTPClient:   httpClient,
		IncludeTags:  includeTags,
		ShowStats:    collectStats,
		ProtocolMode: protocolString(protocol),
	}
	if target != nil {
		cfg.Target = ToSyncerEndpoint(*target, targetAuth)
	}
	return Config{raw: cfg}
}

func SyncConfig(source Endpoint, sourceAuth EndpointAuth, target Endpoint, targetAuth EndpointAuth, scope RefScope, policy SyncPolicy, collectStats, dryRun bool, httpClient *http.Client) Config {
	return Config{raw: syncer.Config{
		Source:                 ToSyncerEndpoint(source, sourceAuth),
		Target:                 ToSyncerEndpoint(target, targetAuth),
		HTTPClient:             httpClient,
		Branches:               append([]string(nil), scope.Branches...),
		Mappings:               ToValidationMappings(scope.Mappings),
		IncludeTags:            policy.IncludeTags,
		DryRun:                 dryRun,
		ShowStats:              collectStats,
		Mode:                   operationModeString(policy.Mode),
		Force:                  policy.Force,
		Prune:                  policy.Prune,
		ProtocolMode:           protocolString(policy.Protocol),
		MaterializedMaxObjects: syncer.DefaultMaterializedMaxObjects,
	}}
}

func Probe(ctx context.Context, cfg Config) (syncer.ProbeResult, error) {
	result, err := syncer.Probe(ctx, cfg.raw)
	if err != nil {
		return syncer.ProbeResult{}, err //nolint:wrapcheck // pass-through layer, caller wraps with context
	}
	return result, nil
}

func Run(ctx context.Context, cfg Config) (syncer.Result, error) {
	result, err := syncer.Run(ctx, cfg.raw)
	if err != nil {
		return syncer.Result{}, err //nolint:wrapcheck // pass-through layer, caller wraps with context
	}
	return result, nil
}

func ToSyncerEndpoint(endpoint Endpoint, auth EndpointAuth) syncer.Endpoint {
	return syncer.Endpoint{
		URL:                    endpoint.URL,
		Username:               auth.Username,
		Token:                  auth.Token,
		BearerToken:            auth.BearerToken,
		SkipTLSVerify:          auth.SkipTLSVerify,
		FollowInfoRefsRedirect: endpoint.FollowInfoRefsRedirect,
	}
}

func protocolString(mode ProtocolMode) string {
	if mode == "" {
		return string(ProtocolAuto)
	}
	return string(mode)
}

func operationModeString(mode OperationMode) string {
	if mode == "" {
		return string(ModeSync)
	}
	return string(mode)
}

func ToValidationMappings(mappings []RefMapping) []validation.RefMapping {
	out := make([]validation.RefMapping, 0, len(mappings))
	for _, mapping := range mappings {
		out = append(out, validation.RefMapping{
			Source: mapping.Source,
			Target: mapping.Target,
		})
	}
	return out
}
