package main

import (
	"errors"
	"fmt"
	"strings"

	gitsync "entire.io/entire/git-sync"
	"entire.io/entire/git-sync/unstable"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"
)

func newFetchCmd() *cobra.Command {
	var (
		haveRefs      []string
		haveHashesRaw []string
		jsonOutput    bool
		sourceAuth    gitsync.EndpointAuth
		branches      string
		protocolVal   = newProtocolFlag()
		req           = unstable.FetchRequest{}
	)

	cmd := &cobra.Command{
		Use:           "fetch [flags] <source-url>",
		Short:         "Negotiate a fetch against the source and report packed objects",
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Protocol = gitsync.ProtocolMode(protocolVal)

			if req.Source.URL == "" && len(args) > 0 {
				req.Source.URL = args[0]
			}
			if req.Source.URL == "" {
				return errors.New("fetch requires a source repository URL")
			}
			if branches != "" {
				req.Scope.Branches = splitCSV(branches)
			}

			haveHashes := make([]plumbing.Hash, 0, len(haveHashesRaw))
			for _, raw := range haveHashesRaw {
				hash := plumbing.NewHash(strings.TrimSpace(raw))
				if hash.IsZero() {
					return fmt.Errorf("invalid --have %q", raw)
				}
				haveHashes = append(haveHashes, hash)
			}

			req.HaveRefs = append(req.HaveRefs, haveRefs...)
			req.HaveHashes = append(req.HaveHashes, haveHashes...)

			result, err := unstable.New(unstable.Options{
				Auth: gitsync.StaticAuthProvider{Source: sourceAuth},
			}).Fetch(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("fetch: %w", err)
			}
			printOutput(jsonOutput, result)
			return nil
		},
	}

	addSourceEndpoint(cmd, &req.Source)
	addSourceAuth(cmd, &sourceAuth)

	cmd.Flags().StringVar(&branches, "branch", "", "comma-separated branch list; default is all source branches")
	cmd.Flags().BoolVar(&req.IncludeTags, "tags", false, "include tags in the fetch request")
	addProtocolFlag(cmd, &protocolVal)
	cmd.Flags().BoolVar(&req.Options.CollectStats, "stats", false, "print transfer statistics")
	cmd.Flags().BoolVar(&req.Options.MeasureMemory, "measure-memory", false, "sample elapsed time and Go heap usage")
	cmd.Flags().BoolVar(&req.Options.Progress, "progress", false, "show live per-side throughput on stderr (TTY only)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print JSON output")
	cmd.Flags().StringArrayVar(&haveRefs, "have-ref", nil, "source ref name to advertise as have; short names map to branches")
	cmd.Flags().StringArrayVar(&haveHashesRaw, "have", nil, "explicit object hash to advertise as have")

	return cmd
}
