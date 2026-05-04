package main

import (
	"errors"
	"fmt"

	gitsync "entire.io/entire/git-sync"
	"entire.io/entire/git-sync/internal/validation"
	"entire.io/entire/git-sync/unstable"
	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	return newSyncLikeCmd("sync", "Mirror refs and objects from source to target", false, gitsync.ModeSync)
}

func newReplicateCmd() *cobra.Command {
	return newSyncLikeCmd("replicate", "Fast-forward-only mirror from source to target", false, gitsync.ModeReplicate)
}

func newPlanCmd() *cobra.Command {
	return newSyncLikeCmd("plan", "Show what a sync or replicate would do without pushing", true, "")
}

func newSyncLikeCmd(name, short string, dryRun bool, defaultMode gitsync.OperationMode) *cobra.Command {
	var (
		mappings    []string
		jsonOutput  bool
		sourceAuth  gitsync.EndpointAuth
		targetAuth  gitsync.EndpointAuth
		branches    string
		modeValue   = operationModeFlag(defaultOperationMode(defaultMode))
		protocolVal = newProtocolFlag()
		req         = unstable.SyncRequest{DryRun: dryRun}
	)

	cmd := &cobra.Command{
		Use:           name + " [flags] <source-url> <target-url>",
		Short:         short,
		Args:          cobra.MaximumNArgs(2),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Policy.Mode = gitsync.OperationMode(modeValue)
			req.Policy.Protocol = gitsync.ProtocolMode(protocolVal)

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
				return fmt.Errorf("%s requires source and target repository URLs", name)
			}

			client := unstable.New(unstable.Options{
				Auth: gitsync.StaticAuthProvider{Source: sourceAuth, Target: targetAuth},
			})

			var (
				result unstable.Result
				err    error
			)
			ctx := cmd.Context()
			switch {
			case dryRun:
				result, err = client.Plan(ctx, req)
			case req.Policy.Mode == gitsync.ModeReplicate:
				result, err = client.Replicate(ctx, req)
			default:
				result, err = client.Sync(ctx, req)
			}
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			printOutput(jsonOutput, result)

			if !dryRun && result.Blocked > 0 {
				return errors.New("one or more branches were skipped because the target was not fast-forwardable")
			}
			return nil
		},
	}

	addSourceEndpoint(cmd, &req.Source)
	addTargetEndpoint(cmd, &req.Target)
	addSourceAuth(cmd, &sourceAuth)
	addTargetAuth(cmd, &targetAuth)

	cmd.Flags().StringVar(&branches, "branch", "", "comma-separated branch list; default is all source branches")
	cmd.Flags().StringArrayVar(&mappings, "map", nil, "ref mapping in src:dst form; short names map branches, full refs map exact refs")
	if name == "plan" {
		cmd.Flags().Var(&modeValue, "mode", "operation mode: sync or replicate")
	}
	cmd.Flags().BoolVar(&req.Policy.IncludeTags, "tags", false, "mirror tags")
	cmd.Flags().BoolVar(&req.Policy.Force, "force", false, "allow non-fast-forward branch updates and retarget tags")
	cmd.Flags().BoolVar(&req.Policy.Prune, "prune", false, "delete managed target refs that no longer exist on source")
	cmd.Flags().BoolVar(&req.Options.CollectStats, "stats", false, "print transfer statistics")
	cmd.Flags().BoolVar(&req.Options.MeasureMemory, "measure-memory", false, "sample elapsed time and Go heap usage")
	cmd.Flags().BoolVar(&req.Options.Progress, "progress", false, "show live per-side throughput on stderr (TTY only)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print JSON output")
	cmd.Flags().IntVar(&req.Options.MaterializedMaxObjects, "materialized-max-objects", unstable.DefaultMaterializedMaxObjects, "abort non-relay materialized syncs above this many objects")
	cmd.Flags().Int64Var(&req.Options.MaxPackBytes, "max-pack-bytes", 0, "abort bootstrap-relay push if the streamed source pack exceeds this many bytes")
	cmd.Flags().Int64Var(&req.Options.TargetMaxPackBytes, "target-max-pack-bytes", 0, "target receive-pack body size limit; batches are planned and auto-subdivided to fit")
	addProtocolFlag(cmd, &protocolVal)
	cmd.Flags().BoolVarP(&req.Options.Verbose, "verbose", "v", false, "verbose logging")

	return cmd
}
