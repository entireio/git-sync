package main

import (
	"errors"
	"fmt"

	gitsync "entire.io/entire/git-sync"
	"entire.io/entire/git-sync/cmd/git-sync/internal/sha256convert"
	"entire.io/entire/git-sync/internal/validation"
	"github.com/spf13/cobra"
)

func newConvertSHA256Cmd() *cobra.Command {
	var (
		req         = sha256convert.Request{}
		mappings    []string
		branches    string
		jsonOutput  bool
		protocolVal = newProtocolFlag()
	)

	cmd := &cobra.Command{
		Use:   "convert-sha256 [flags] <source-url> <target-dir>",
		Short: "One-off SHA1 → SHA256 conversion of a remote repo into a local bare repo",
		Long: `convert-sha256 fetches a pack from a SHA1 HTTP source and writes a new
SHA256 bare repository on disk at <target-dir>. Every reachable object is
re-hashed under SHA256 and tree/commit/tag references are rewritten.

The conversion is destructive in two ways the caller should be aware of:
no SHA1↔SHA256 mapping is persisted, and any GPG signatures on commits or
tags are dropped (they sign over the original SHA1 content and would be
invalid post-rewrite). Submodule gitlinks that point at a commit outside
this repository cannot be embedded in a SHA256 tree; if the source repo
contains any, the command exits with an error so the caller can scope
around the offending refs.`,
		Args:          cobra.MaximumNArgs(2),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			req.ProtocolMode = gitsync.ProtocolMode(protocolVal)
			if req.SourceURL == "" && len(args) > 0 {
				req.SourceURL = args[0]
			}
			if req.TargetDir == "" && len(args) > 1 {
				req.TargetDir = args[1]
			}
			if req.SourceURL == "" || req.TargetDir == "" {
				return errors.New("convert-sha256 requires a source URL and a target directory")
			}
			if branches != "" {
				req.Branches = splitCSV(branches)
			}
			for _, raw := range mappings {
				mapping, err := validation.ParseMapping(raw)
				if err != nil {
					return fmt.Errorf("parse mapping %q: %w", raw, err)
				}
				req.Mappings = append(req.Mappings, gitsync.RefMapping{
					Source: mapping.Source,
					Target: mapping.Target,
				})
			}

			result, err := sha256convert.Run(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("convert-sha256: %w", err)
			}
			printOutput(jsonOutput, result)
			return nil
		},
	}

	cmd.Flags().StringVar(&req.SourceURL, "source-url", "", "source repository URL")
	cmd.Flags().BoolVar(&req.SourceFollowInfoRefsRedirect, "source-follow-info-refs-redirect",
		envBool("GITSYNC_SOURCE_FOLLOW_INFO_REFS_REDIRECT"),
		"send follow-up source RPCs to the final /info/refs redirect host")
	cmd.Flags().StringVar(&req.SourceAuth.Token, "source-token",
		envOr("GITSYNC_SOURCE_TOKEN", ""), "source token/password")
	cmd.Flags().StringVar(&req.SourceAuth.Username, "source-username",
		envOr("GITSYNC_SOURCE_USERNAME", "git"), "source basic auth username")
	cmd.Flags().StringVar(&req.SourceAuth.BearerToken, "source-bearer-token",
		envOr("GITSYNC_SOURCE_BEARER_TOKEN", ""), "source bearer token")
	cmd.Flags().BoolVar(&req.SourceAuth.SkipTLSVerify, "source-insecure-skip-tls-verify",
		envBool("GITSYNC_SOURCE_INSECURE_SKIP_TLS_VERIFY"),
		"skip TLS certificate verification for the source")
	cmd.Flags().StringVar(&req.TargetDir, "target-dir", "", "directory to initialize as a SHA256 bare repository")

	cmd.Flags().StringVar(&branches, "branch", "", "comma-separated branch list; default is all source branches")
	cmd.Flags().StringArrayVar(&mappings, "map", nil, "ref mapping in src:dst form; short names map branches, full refs map exact refs")
	cmd.Flags().BoolVar(&req.IncludeTags, "tags", false, "include annotated and lightweight tags")
	allRefsFlag(cmd, allRefsUsageScopeOnly, &req.AllRefs)
	excludeRefPrefixFlag(cmd, &req.ExcludeRefPrefixes)
	addProtocolFlag(cmd, &protocolVal)
	cmd.Flags().BoolVarP(&req.Verbose, "verbose", "v", false, "verbose logging")
	cmd.Flags().BoolVar(&req.KeepSourceObjects, "keep-source-objects", false,
		"keep the temporary SHA1 store on disk after conversion (for debugging)")
	cmd.Flags().StringVar(&req.MappingFile, "write-mapping", "",
		"write the full SHA1 → SHA256 mapping as a TSV to this path; useful for rewriting external references")
	cmd.Flags().BoolVar(&req.SkipMessageRewrite, "no-rewrite-messages", false,
		"do not rewrite SHA1 hash references found in commit and tag messages")
	cmd.Flags().BoolVar(&req.SkipOriginNotes, "no-origin-notes", false,
		"do not write a refs/notes/sha1-origin ref recording each commit's original SHA1")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print JSON output")

	return cmd
}
