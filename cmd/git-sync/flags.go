package main

import (
	"fmt"
	"os"
	"strings"

	gitsync "entire.io/entire/git-sync"
	"entire.io/entire/git-sync/internal/validation"
	"github.com/spf13/cobra"
)

func addSourceEndpoint(cmd *cobra.Command, ep *gitsync.Endpoint) {
	cmd.Flags().StringVar(&ep.URL, "source-url", "", "source repository URL")
	cmd.Flags().BoolVar(&ep.FollowInfoRefsRedirect, "source-follow-info-refs-redirect",
		envBool("GITSYNC_SOURCE_FOLLOW_INFO_REFS_REDIRECT"),
		"send follow-up source RPCs to the final /info/refs redirect host")
}

func addTargetEndpoint(cmd *cobra.Command, ep *gitsync.Endpoint) {
	cmd.Flags().StringVar(&ep.URL, "target-url", "", "target repository URL")
	cmd.Flags().BoolVar(&ep.FollowInfoRefsRedirect, "target-follow-info-refs-redirect",
		envBool("GITSYNC_TARGET_FOLLOW_INFO_REFS_REDIRECT"),
		"send follow-up target RPCs to the final /info/refs redirect host")
}

func addSourceAuth(cmd *cobra.Command, auth *gitsync.EndpointAuth) {
	cmd.Flags().StringVar(&auth.Token, "source-token", envOr("GITSYNC_SOURCE_TOKEN", ""), "source token/password")
	cmd.Flags().StringVar(&auth.Username, "source-username", envOr("GITSYNC_SOURCE_USERNAME", "git"), "source basic auth username")
	cmd.Flags().StringVar(&auth.BearerToken, "source-bearer-token", envOr("GITSYNC_SOURCE_BEARER_TOKEN", ""), "source bearer token")
	cmd.Flags().BoolVar(&auth.SkipTLSVerify, "source-insecure-skip-tls-verify",
		envBool("GITSYNC_SOURCE_INSECURE_SKIP_TLS_VERIFY"),
		"skip TLS certificate verification for the source")
}

func addTargetAuth(cmd *cobra.Command, auth *gitsync.EndpointAuth) {
	cmd.Flags().StringVar(&auth.Token, "target-token", envOr("GITSYNC_TARGET_TOKEN", ""), "target token/password")
	cmd.Flags().StringVar(&auth.Username, "target-username", envOr("GITSYNC_TARGET_USERNAME", "git"), "target basic auth username")
	cmd.Flags().StringVar(&auth.BearerToken, "target-bearer-token", envOr("GITSYNC_TARGET_BEARER_TOKEN", ""), "target bearer token")
	cmd.Flags().BoolVar(&auth.SkipTLSVerify, "target-insecure-skip-tls-verify",
		envBool("GITSYNC_TARGET_INSECURE_SKIP_TLS_VERIFY"),
		"skip TLS certificate verification for the target")
}

func addProtocolFlag(cmd *cobra.Command, mode *protocolModeFlag) {
	cmd.Flags().Var(mode, "protocol", "protocol mode: auto, v1, or v2")
}

func newProtocolFlag() protocolModeFlag {
	return protocolModeFlag(protocolMode(envOr("GITSYNC_PROTOCOL", validation.ProtocolAuto)))
}

func envOr(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func envBool(key string) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

type protocolMode gitsync.ProtocolMode
type operationMode gitsync.OperationMode

type protocolModeFlag protocolMode
type operationModeFlag operationMode

func (p *protocolModeFlag) String() string { return string(*p) }
func (p *protocolModeFlag) Type() string   { return "string" }

func (p *protocolModeFlag) Set(value string) error {
	mode, err := validation.NormalizeProtocolMode(value)
	if err != nil {
		return fmt.Errorf("normalize protocol: %w", err)
	}
	*p = protocolModeFlag(protocolMode(gitsync.ProtocolMode(mode)))
	return nil
}

func (m *operationModeFlag) String() string { return string(*m) }
func (m *operationModeFlag) Type() string   { return "string" }

func (m *operationModeFlag) Set(value string) error {
	switch gitsync.OperationMode(value) {
	case gitsync.ModeSync, gitsync.ModeReplicate:
		*m = operationModeFlag(operationMode(value))
		return nil
	default:
		return fmt.Errorf("unsupported mode %q", value)
	}
}

// defaultOperationMode returns the starting value for the --mode flag.
// Subcommands that pin a mode (sync, replicate) pass it in; plan passes ""
// and gets sync as the default, letting --mode override it.
func defaultOperationMode(defaultMode gitsync.OperationMode) operationMode {
	if defaultMode != "" {
		return operationMode(defaultMode)
	}
	return operationMode(gitsync.ModeSync)
}
