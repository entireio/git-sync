package main

import (
	"encoding/json"
	"fmt"
	"os"

	"entire.io/entire/git-sync/cmd/git-sync/internal/versioninfo"
	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "git-sync",
		Short: "Sync, replicate, or probe git repositories over the smart HTTP protocol",
		Long: `git-sync moves refs and objects between two git HTTP endpoints without
needing a working tree. It can mirror a source into a target (sync), do a
fast-forward-only mirror (replicate), preview the work to be done (plan),
seed an empty target (bootstrap), or inspect either side (probe, fetch).`,
		Version:       versioninfo.Version,
		SilenceErrors: true,
		SilenceUsage:  true,
		CompletionOptions: cobra.CompletionOptions{
			HiddenDefaultCmd: true,
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.SetVersionTemplate(fmt.Sprintf("git-sync %s (commit %s, built %s)\n",
		versioninfo.Version, versioninfo.Commit, versioninfo.Date))

	cmd.AddCommand(newSyncCmd())
	cmd.AddCommand(newReplicateCmd())
	cmd.AddCommand(newPlanCmd())
	cmd.AddCommand(newBootstrapCmd())
	cmd.AddCommand(newProbeCmd())
	cmd.AddCommand(newFetchCmd())
	cmd.AddCommand(newVersionCmd())

	return cmd
}

func printOutput(jsonOutput bool, value interface{ Lines() []string }) {
	if jsonOutput {
		data, err := marshalOutput(value)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: encode JSON output: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		return
	}

	for _, line := range value.Lines() {
		fmt.Println(line)
	}
}

func marshalOutput(value interface{}) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal JSON: %w", err)
	}
	return data, nil
}
