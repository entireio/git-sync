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
	addSecretFlag(cmd, &auth.Token, "source-token", "GITSYNC_SOURCE_TOKEN", "source token/password")
	cmd.Flags().StringVar(&auth.Username, "source-username", envOr("GITSYNC_SOURCE_USERNAME", "git"), "source basic auth username")
	addSecretFlag(cmd, &auth.BearerToken, "source-bearer-token", "GITSYNC_SOURCE_BEARER_TOKEN", "source bearer token")
	cmd.Flags().BoolVar(&auth.SkipTLSVerify, "source-insecure-skip-tls-verify",
		envBool("GITSYNC_SOURCE_INSECURE_SKIP_TLS_VERIFY"),
		"skip TLS certificate verification for the source")
}

func addTargetAuth(cmd *cobra.Command, auth *gitsync.EndpointAuth) {
	addSecretFlag(cmd, &auth.Token, "target-token", "GITSYNC_TARGET_TOKEN", "target token/password")
	cmd.Flags().StringVar(&auth.Username, "target-username", envOr("GITSYNC_TARGET_USERNAME", "git"), "target basic auth username")
	addSecretFlag(cmd, &auth.BearerToken, "target-bearer-token", "GITSYNC_TARGET_BEARER_TOKEN", "target bearer token")
	cmd.Flags().BoolVar(&auth.SkipTLSVerify, "target-insecure-skip-tls-verify",
		envBool("GITSYNC_TARGET_INSECURE_SKIP_TLS_VERIFY"),
		"skip TLS certificate verification for the target")
}

func addProtocolFlag(cmd *cobra.Command, mode *protocolModeFlag) {
	cmd.Flags().Var(mode, "protocol", "protocol mode: auto, v1, or v2")
}

const (
	allRefsUsageBestEffort = "mirror every refs/* on the source (branches, tags, notes, pulls, custom namespaces) on a best-effort basis; per-ref server rejections become warnings rather than failing the sync"
	allRefsUsageStrict     = "mirror every refs/* on the source (branches, tags, notes, pulls, custom namespaces); per-ref rejections fail the run, since replicate's contract is target == source"
	allRefsUsageScopeOnly  = "include every refs/* on the source (notes, pulls, custom namespaces) — scope only, no failure-handling effect"
)

// excludeRefPrefixFlag registers --exclude-ref-prefix. Repeatable; each
// prefix is matched as a string prefix against ref names (e.g.
// "refs/pull/" trims GitHub PR refs under --all-refs).
func excludeRefPrefixFlag(cmd *cobra.Command, prefixes *[]string) {
	cmd.Flags().StringArrayVar(prefixes, "exclude-ref-prefix", nil,
		"exclude refs whose names start with this prefix; repeatable. "+
			"Subtracts from auto-discovery (branches/tags/--all-refs); explicit --map values are not subject to this filter")
}

// allRefsFlag registers --all-refs with the supplied usage string and
// bundles its implications. Each pointer in implies is set to true when
// --all-refs is set, via a PreRunE hook that fires after flag parsing.
//
// Not idempotent: calling twice on the same command stacks two PreRunE
// hooks on the same flag pointer. Call once per command.
func allRefsFlag(cmd *cobra.Command, usage string, allRefs *bool, implies ...*bool) {
	cmd.Flags().BoolVar(allRefs, "all-refs", false, usage)
	if len(implies) == 0 {
		return
	}
	chainPreRunE(cmd, func(*cobra.Command, []string) error {
		if *allRefs {
			for _, p := range implies {
				if p != nil {
					*p = true
				}
			}
		}
		return nil
	})
}

// chainPreRunE appends fn to cmd's PreRunE, preserving any hook already
// registered (which runs first). Lets independent flag helpers each attach
// post-parse logic to the same command without clobbering one another.
func chainPreRunE(cmd *cobra.Command, fn func(*cobra.Command, []string) error) {
	prev := cmd.PreRunE
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		if prev != nil {
			if err := prev(cmd, args); err != nil {
				return err
			}
		}
		return fn(cmd, args)
	}
}

// addSecretFlag registers a string flag whose value may also come from an
// environment variable. Unlike non-secret flags, it must NOT register the env
// value as the pflag default: pflag prints non-empty defaults in --help and in
// the usage block dumped on a flag error, which would leak the secret (e.g.
// into CI logs). Registration and the post-parse env fallback are paired in
// one call so a new secret flag cannot accidentally re-introduce the leak by
// reaching for envOr.
func addSecretFlag(cmd *cobra.Command, dst *string, name, env, usage string) {
	cmd.Flags().StringVar(dst, name, "", fmt.Sprintf("%s (env: %s)", usage, env))
	addSecretEnvFallback(cmd, name, dst, env)
}

// addSecretEnvFallback fills *dst from the named environment variable after
// parsing, but only when the flag was not given explicitly.
func addSecretEnvFallback(cmd *cobra.Command, flag string, dst *string, env string) {
	chainPreRunE(cmd, func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed(flag) {
			return nil
		}
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
		return nil
	})
}

func newProtocolFlag() protocolModeFlag {
	return protocolModeFlag(envOr("GITSYNC_PROTOCOL", validation.ProtocolAuto))
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

// resolvePositionalEndpoints fills the source and target from positional args
// left-to-right, skipping whichever was already supplied via a flag. Consuming
// by fixed index instead (args[0]→source, args[1]→target) breaks the mixed
// form `--source-url URL <target>`: the lone positional lands in args[0] and
// the target slot stays empty.
//
// A positional left over after both slots are filled means the user
// over-specified an endpoint (e.g. `--source-url URL a b`); reject it rather
// than silently pick one. Callers still validate that both ended up set.
func resolvePositionalEndpoints(source, target *string, args []string) error {
	positional := args
	if *source == "" && len(positional) > 0 {
		*source = positional[0]
		positional = positional[1:]
	}
	if *target == "" && len(positional) > 0 {
		*target = positional[0]
		positional = positional[1:]
	}
	if len(positional) > 0 {
		return fmt.Errorf("unexpected argument %q: source and target are already set via flags or earlier positionals", positional[0])
	}
	return nil
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

// parseMappings converts raw --map values (src:dst form) into ref mappings.
// Returns nil (not an empty slice) when no mappings were given, matching the
// zero value the request structs start with.
func parseMappings(raw []string) ([]gitsync.RefMapping, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]gitsync.RefMapping, 0, len(raw))
	for _, value := range raw {
		mapping, err := validation.ParseMapping(value)
		if err != nil {
			return nil, fmt.Errorf("parse mapping %q: %w", value, err)
		}
		out = append(out, gitsync.RefMapping{Source: mapping.Source, Target: mapping.Target})
	}
	return out, nil
}

type protocolModeFlag gitsync.ProtocolMode
type operationModeFlag gitsync.OperationMode

func (p *protocolModeFlag) String() string { return string(*p) }
func (p *protocolModeFlag) Type() string   { return "string" }

func (p *protocolModeFlag) Set(value string) error {
	mode, err := validation.NormalizeProtocolMode(value)
	if err != nil {
		return fmt.Errorf("normalize protocol: %w", err)
	}
	*p = protocolModeFlag(mode)
	return nil
}

func (m *operationModeFlag) String() string { return string(*m) }
func (m *operationModeFlag) Type() string   { return "string" }

func (m *operationModeFlag) Set(value string) error {
	switch gitsync.OperationMode(value) {
	case gitsync.ModeSync, gitsync.ModeReplicate:
		*m = operationModeFlag(value)
		return nil
	default:
		return fmt.Errorf("unsupported mode %q", value)
	}
}

// defaultOperationMode returns the starting value for the --mode flag.
// Subcommands that pin a mode (sync, replicate) pass it in; plan passes ""
// and gets sync as the default, letting --mode override it.
func defaultOperationMode(defaultMode gitsync.OperationMode) operationModeFlag {
	if defaultMode != "" {
		return operationModeFlag(defaultMode)
	}
	return operationModeFlag(gitsync.ModeSync)
}
