package main

import (
	"fmt"

	gitsync "entire.io/entire/git-sync"
	"entire.io/entire/git-sync/internal/validation"
	"entire.io/entire/git-sync/unstable"
	"github.com/spf13/cobra"
)

func newBootstrapCmd() *cobra.Command {
	var (
		mappings    []string
		jsonOutput  bool
		sourceAuth  gitsync.EndpointAuth
		targetAuth  gitsync.EndpointAuth
		branches    string
		protocolVal = newProtocolFlag()
		req         = unstable.BootstrapRequest{}
	)

	cmd := &cobra.Command{
		Use:           "bootstrap [flags] <source-url> <target-url>",
		Short:         "Seed an empty target by streaming the source pack",
		Args:          cobra.MaximumNArgs(2),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Protocol = gitsync.ProtocolMode(protocolVal)

			if req.Source.URL == "" && len(args) > 0 {
				req.Source.URL = args[0]
			}
			if req.Target.URL == "" && len(args) > 1 {
				req.Target.URL = args[1]
			}

			if branches != "" {
				req.Scope.Branches = splitCSV(branches)
			}
			for _, raw := range mappings {
				mapping, err := validation.ParseMapping(raw)
				if err != nil {
					return fmt.Errorf("parse mapping %q: %w", raw, err)
				}
				req.Scope.Mappings = append(req.Scope.Mappings, gitsync.RefMapping{
					Source: mapping.Source,
					Target: mapping.Target,
				})
			}

			if req.Source.URL == "" || req.Target.URL == "" {
				return fmt.Errorf("bootstrap requires source and target repository URLs")
			}

			result, err := unstable.New(unstable.Options{
				Auth: gitsync.StaticAuthProvider{Source: sourceAuth, Target: targetAuth},
			}).Bootstrap(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("bootstrap: %w", err)
			}
			printOutput(jsonOutput, result)
			return nil
		},
	}

	addSourceEndpoint(cmd, &req.Source)
	addTargetEndpoint(cmd, &req.Target)
	addSourceAuth(cmd, &sourceAuth)
	addTargetAuth(cmd, &targetAuth)

	cmd.Flags().StringVar(&branches, "branch", "", "comma-separated branch list; default is all source branches")
	cmd.Flags().StringArrayVar(&mappings, "map", nil, "ref mapping in src:dst form; short names map branches, full refs map exact refs")
	cmd.Flags().BoolVar(&req.IncludeTags, "tags", false, "mirror tags")
	cmd.Flags().BoolVar(&req.Options.CollectStats, "stats", false, "print transfer statistics")
	cmd.Flags().BoolVar(&req.Options.MeasureMemory, "measure-memory", false, "sample elapsed time and Go heap usage")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print JSON output")
	cmd.Flags().Int64Var(&req.Options.MaxPackBytes, "max-pack-bytes", 0, "abort bootstrap if the streamed source pack exceeds this many bytes")
	cmd.Flags().Int64Var(&req.Options.TargetMaxPackBytes, "target-max-pack-bytes", 0, "target receive-pack body size limit; batches are planned and auto-subdivided to fit")
	addProtocolFlag(cmd, &protocolVal)
	cmd.Flags().BoolVarP(&req.Options.Verbose, "verbose", "v", false, "verbose logging")

	return cmd
}
