package main

import (
	"errors"
	"fmt"

	gitsync "entire.io/entire/git-sync"
	"entire.io/entire/git-sync/unstable"
	"github.com/spf13/cobra"
)

func newProbeCmd() *cobra.Command {
	var (
		jsonOutput                   bool
		sourceAuth                   gitsync.EndpointAuth
		targetAuth                   gitsync.EndpointAuth
		targetURL                    string
		targetFollowInfoRefsRedirect bool
		protocolVal                  = newProtocolFlag()
		req                          = unstable.ProbeRequest{}
	)

	cmd := &cobra.Command{
		Use:           "probe [flags] <source-url> [target-url]",
		Short:         "Inspect refs advertised by source (and optionally target)",
		Args:          cobra.MaximumNArgs(2),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Protocol = gitsync.ProtocolMode(protocolVal)

			if req.Source.URL == "" && len(args) > 0 {
				req.Source.URL = args[0]
			}
			if targetURL == "" && len(args) > 1 {
				targetURL = args[1]
			}
			if req.Source.URL == "" {
				return errors.New("probe requires a source repository URL")
			}
			if targetURL != "" {
				req.Target = &gitsync.Endpoint{
					URL:                    targetURL,
					FollowInfoRefsRedirect: targetFollowInfoRefsRedirect,
				}
			}

			result, err := unstable.New(unstable.Options{
				Auth: gitsync.StaticAuthProvider{Source: sourceAuth, Target: targetAuth},
			}).Probe(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("probe: %w", err)
			}
			printOutput(jsonOutput, result)
			return nil
		},
	}

	addSourceEndpoint(cmd, &req.Source)
	cmd.Flags().StringVar(&targetURL, "target-url", "", "optional target repository URL")
	cmd.Flags().BoolVar(&targetFollowInfoRefsRedirect, "target-follow-info-refs-redirect",
		envBool("GITSYNC_TARGET_FOLLOW_INFO_REFS_REDIRECT"),
		"send follow-up target RPCs to the final /info/refs redirect host")
	addSourceAuth(cmd, &sourceAuth)
	addTargetAuth(cmd, &targetAuth)

	cmd.Flags().BoolVar(&req.IncludeTags, "tags", false, "include tag ref prefixes in probe")
	addProtocolFlag(cmd, &protocolVal)
	cmd.Flags().BoolVar(&req.Options.CollectStats, "stats", false, "print transfer statistics")
	cmd.Flags().BoolVar(&req.Options.MeasureMemory, "measure-memory", false, "sample elapsed time and Go heap usage")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print JSON output")

	return cmd
}
