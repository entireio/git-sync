package main

import (
	"fmt"

	"entire.io/entire/git-sync/cmd/git-sync/internal/versioninfo"
	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show build information",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), versioninfo.String())
		},
	}
}
