package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func main() {
	err := run(context.Background(), os.Args[1:])
	if err == nil {
		return
	}
	if !errors.Is(err, errSilent) {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	os.Exit(1)
}

func run(ctx context.Context, args []string) error {
	rootCmd := newRootCmd()
	rootCmd.SetArgs(args)
	err := rootCmd.ExecuteContext(ctx)
	if err == nil {
		return nil
	}

	// On unknown commands or unknown flags, fall back to printing usage so
	// the user sees what's available instead of a one-line cryptic error.
	// Use the deepest subcommand that matched the args so flag errors show
	// the relevant subcommand's usage, not the root.
	msg := err.Error()
	if strings.Contains(msg, "unknown command") || strings.Contains(msg, "unknown flag") || strings.Contains(msg, "unknown shorthand flag") {
		target := rootCmd
		if found, _, ferr := rootCmd.Find(args); ferr == nil && found != nil {
			target = found
		}
		showUsage(target, err)
		return errSilent
	}
	//nolint:wrapcheck // cobra surfaces errors that are already user-facing (RunE-prefixed or cobra arg validation); main prints them with an "error:" prefix
	return err
}

// errSilent signals that main should exit non-zero without re-printing the
// error (it has already been printed alongside the usage block).
var errSilent = errors.New("")

func showUsage(cmd *cobra.Command, err error) {
	fmt.Fprint(cmd.OutOrStderr(), cmd.UsageString())
	fmt.Fprintf(cmd.OutOrStderr(), "\nError: %v\n", err)
}
