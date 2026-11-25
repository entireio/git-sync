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
re-hashed under SHA256 and tree/commit/tag references are rewritten, as
git converts objects for extensions.compatObjectFormat: commit gpgsig and
gpgsig-sha256 headers are kept, mergetag headers are converted, and every
converted object converts back to its SHA1 original byte for byte.

All branches and tags on the source are always converted, so the opt-in
--rewrite-messages never strands a cross-branch reference. Pass
--all-refs to also include refs/notes/* and other custom namespaces;
pass --exclude-ref-prefix to subtract specific namespaces from --all-refs.
Exclude prefixes that would drop any branch or tag (e.g. refs/heads/feature/,
refs/tags/, refs/) are rejected at run time to preserve the always-convert
invariant.

Server-internal pull/merge-request refs (refs/pull/*, refs/pull-requests/*,
refs/merge-requests/*) are NOT converted even under --all-refs: they hold
unmerged code foreign to the repository, and mirroring the result onward
would republish it. Pass --include-pull-refs to convert them anyway.

Any submodule gitlink fails the run — its .gitmodules upstream still
advertises SHA1 hashes, so a rewritten SHA256 gitlink would point at a
hash the upstream cannot resolve and break ` + "`git submodule update`" + ` in
every clone. Exclude refs that reference submodules, or convert the
submodule repository first and re-point .gitmodules.`,
		Args:          cobra.MaximumNArgs(2),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			req.ProtocolMode = gitsync.ProtocolMode(protocolVal)
			if err := resolveConvertSHA256Args(&req, args); err != nil {
				return err
			}

			result, err := sha256convert.Run(cmd.Context(), req)
			// Print whatever state Run produced even on error: signed
			// tags landed before signBranchTips failed, --check
			// findings, and the --keep-source-objects temp dir are
			// all things the user needs to see to clean up or debug.
			// Run zero-values fields it never touched, so this is
			// safe to call on a half-populated result.
			if result.SourceURL != "" || result.TargetDir != "" {
				printOutput(jsonOutput, result)
			}
			if err != nil {
				return fmt.Errorf("convert-sha256: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&req.SourceURL, "source-url", "", "source repository URL")
	cmd.Flags().BoolVar(&req.SourceFollowInfoRefsRedirect, "source-follow-info-refs-redirect",
		envBool("GITSYNC_SOURCE_FOLLOW_INFO_REFS_REDIRECT"),
		"send follow-up source RPCs to the final /info/refs redirect host")
	addSourceAuth(cmd, &req.SourceAuth)
	cmd.Flags().StringVar(&req.TargetDir, "target-dir", "", "directory to initialize as a SHA256 bare repository")

	allRefsFlag(cmd, allRefsUsageScopeOnly, &req.AllRefs)
	excludeRefPrefixFlag(cmd, &req.ExcludeRefPrefixes)
	cmd.Flags().BoolVar(&req.IncludePullRefs, "include-pull-refs", false,
		"with --all-refs, also convert server-internal pull/merge-request refs (refs/pull/*, refs/pull-requests/*, refs/merge-requests/*); off by default because they hold unmerged foreign code")
	addProtocolFlag(cmd, &protocolVal)
	cmd.Flags().BoolVarP(&req.Verbose, "verbose", "v", false, "verbose logging")
	cmd.Flags().BoolVar(&req.Progress, "progress", false,
		"show live per-phase object counts on stderr (TTY only)")
	cmd.Flags().BoolVar(&req.Check, "check", false,
		"verify the output after conversion (config, HEAD, refs, git fsck --full)")
	cmd.Flags().StringVar(&req.SignMode, "sign-mode", sha256convert.SignModeNone,
		"post-conversion signing: `none` (default), or `tips` to sign each branch tip as refs/tags/converted/<branch> via `git tag -s`")
	cmd.Flags().StringVar(&req.SignKey, "sign-key", "",
		"signing key id to pass to `git tag -s -u`; default uses the repo's user.signingkey")
	cmd.Flags().BoolVar(&req.KeepSourceObjects, "keep-source-objects", false,
		"keep the temporary SHA1 store on disk after conversion (for debugging)")
	cmd.Flags().StringVar(&req.MappingFile, "write-mapping", "",
		"write the full SHA1 → SHA256 mapping as a TSV to this path; useful for rewriting external references")
	cmd.Flags().BoolVar(&req.RewriteMessages, "rewrite-messages", false,
		"rewrite SHA1 hash references found in commit and tag messages to SHA256; rewritten objects no longer convert back to their SHA1 originals, so their signatures stop verifying")
	cmd.Flags().Bool("no-rewrite-messages", false, "no effect: messages are no longer rewritten unless --rewrite-messages is passed")
	if err := cmd.Flags().MarkDeprecated("no-rewrite-messages", "messages are no longer rewritten by default; pass --rewrite-messages to opt in"); err != nil {
		panic(err)
	}
	cmd.Flags().BoolVar(&req.SkipOriginNotes, "no-origin-notes", false,
		"do not write a refs/notes/sha1-origin ref recording each commit's original SHA1")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print JSON output")

	return cmd
}

// resolveConvertSHA256Args fills the source URL and target dir from flags
// and/or positional args (see resolvePositionalEndpoints) and requires both.
func resolveConvertSHA256Args(req *sha256convert.Request, args []string) error {
	if err := resolvePositionalEndpoints(&req.SourceURL, &req.TargetDir, args); err != nil {
		return err
	}
	if req.SourceURL == "" || req.TargetDir == "" {
		return errors.New("convert-sha256 requires a source URL and a target directory")
	}
	return nil
}
