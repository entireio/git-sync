package internalbridge

import (
	"context"
	"net/http"

	"github.com/soph/git-sync/internal/syncer"
	"github.com/soph/git-sync/internal/validation"
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
	return syncer.Probe(ctx, cfg.raw)
}

func Run(ctx context.Context, cfg Config) (syncer.Result, error) {
	return syncer.Run(ctx, cfg.raw)
}

func ToSyncerEndpoint(endpoint Endpoint, auth EndpointAuth) syncer.Endpoint {
	return syncer.Endpoint{
		URL:           endpoint.URL,
		Username:      auth.Username,
		Token:         auth.Token,
		BearerToken:   auth.BearerToken,
		SkipTLSVerify: auth.SkipTLSVerify,
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
