package main

import (
	"errors"
	"fmt"

	gitsync "entire.io/entire/git-sync"
	"entire.io/entire/git-sync/cmd/git-sync/internal/sha256convert"
	"github.com/spf13/cobra"
)

func newConvertSHA256Cmd() *cobra.Command {
	var (
		req         = sha256convert.Request{}
		jsonOutput  bool
		protocolVal = newProtocolFlag()
	)

	cmd := &cobra.Command{
		Use:   "convert-sha256 [flags] <source-url> <target-dir>",
		Short: "One-off SHA1 → SHA256 conversion of a remote repo into a local bare repo",
		Long: `convert-sha256 fetches a pack from a SHA1 HTTP source and writes a new
SHA256 bare repository on disk at <target-dir>. Every reachable object is
re-hashed under SHA256 and tree/commit/tag references are rewritten.

All branches and tags on the source are always converted — partial scope
risks stranding cross-branch references in commit messages. Pass
--all-refs to also include refs/notes/*, refs/pull/*, and other custom
namespaces; pass --exclude-ref-prefix to subtract specific namespaces
from --all-refs. Exclude prefixes that would drop any branch or tag
(e.g. refs/heads/feature/, refs/tags/, refs/) are rejected at run time
to preserve the always-convert invariant.

The conversion is destructive in two ways the caller should be aware of:
GPG signatures on commits and tags are dropped (they sign over the
original SHA1 content and would be invalid post-rewrite), and submodule
gitlinks that point at a commit outside this repository cannot be
embedded in a SHA256 tree — the command exits with an error if it finds
any so the caller can convert the submodule repository first.`,
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

	allRefsFlag(cmd, allRefsUsageScopeOnly, &req.AllRefs)
	excludeRefPrefixFlag(cmd, &req.ExcludeRefPrefixes)
	addProtocolFlag(cmd, &protocolVal)
	cmd.Flags().BoolVarP(&req.Verbose, "verbose", "v", false, "verbose logging")
	cmd.Flags().BoolVar(&req.Progress, "progress", false,
		"show live per-phase object counts on stderr (TTY only)")
	cmd.Flags().BoolVar(&req.Check, "check", false,
		"verify the output after conversion (config, HEAD, refs, git fsck --full)")
	cmd.Flags().BoolVar(&req.Sign, "sign", false,
		"after conversion, sign each branch tip as refs/tags/converted/<branch> via `git tag -s`")
	cmd.Flags().StringVar(&req.SignKey, "sign-key", "",
		"signing key id to pass to `git tag -s -u`; default uses the repo's user.signingkey")
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
