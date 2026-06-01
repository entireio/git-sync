// Package sha256convert implements a one-off SHA1 → SHA256 conversion for a
// single repository. It fetches a pack from a remote SHA1 HTTP endpoint into
// a temporary on-disk SHA1 bare repo, then walks every reachable object and
// re-emits it under SHA256 into a new bare repo at the user-supplied path.
//
// The tool is intentionally scoped: GPG signatures on commits and tags
// are dropped (they sign over the original SHA1 byte stream and would be
// invalid post-rewrite), and any submodule gitlink fails the run so the
// caller chooses which refs to exclude. The linked-to repository's URL
// still points at an upstream SHA1 store, which has no way to resolve a
// SHA256-rewritten gitlink, so rewriting would produce a tree that
// fsck-passes but breaks `git submodule update`.
//
// The SHA1 → SHA256 mapping is preserved, so the original hashes stay
// recoverable: by default as a refs/notes/sha1-origin notes ref in the
// converted repo (disable with --no-origin-notes), and optionally as a
// sidecar TSV via --write-mapping.
package sha256convert

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
	transporthttp "github.com/go-git/go-git/v6/plumbing/transport/http"
	"github.com/go-git/go-git/v6/storage/filesystem"

	gitsync "entire.io/entire/git-sync"
	"entire.io/entire/git-sync/internal/auth"
	"entire.io/entire/git-sync/internal/convert"
	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
)

// Request describes a single SHA1 → SHA256 conversion.
//
// Scope is intentionally fixed: every branch and every annotated/lightweight
// tag on the source is always converted. Partial scope risks stranding
// cross-branch references in commit messages, which defeats the point of a
// one-off cutover. AllRefs additionally pulls in refs/notes and other custom
// namespaces; ExcludeRefPrefixes subtracts from that. Server-internal
// pull/merge-request namespaces are excluded from AllRefs by default (see
// IncludePullRefs) because they carry unmerged foreign code.
type Request struct {
	SourceURL                    string
	SourceAuth                   gitsync.EndpointAuth
	SourceFollowInfoRefsRedirect bool
	TargetDir                    string

	AllRefs            bool
	ExcludeRefPrefixes []string

	// IncludePullRefs opts back into converting the server-internal
	// pull/merge-request namespaces (refs/pull/*, refs/pull-requests/*,
	// refs/merge-requests/*) that AllRefs would otherwise pull in. They
	// are excluded by default: those refs hold code proposed from forks
	// and other branches — foreign to the repository until merged — and
	// the converted repo is typically mirrored onward with
	// `git push --mirror`, where a destination forge may surface them as
	// ordinary refs and republish unreviewed code as repo content. No
	// effect without AllRefs (the namespaces are out of scope anyway).
	IncludePullRefs bool

	ProtocolMode gitsync.ProtocolMode
	Verbose      bool
	Progress     bool
	Check        bool

	// SignMode selects the post-conversion attestation strategy. ""
	// and SignModeNone sign nothing; SignModeTips runs
	// `git tag -s converted/<branch> <tip>` for every converted branch,
	// attesting the entire reachable history of each branch via its
	// tip's parent chain. (A future "all" mode could sign every commit
	// and tag.) SignKey is passed to git as `-u <SignKey>`; leave empty
	// to use the repo's default signing identity.
	SignMode string
	SignKey  string

	KeepSourceObjects bool

	// MappingFile, when non-empty, is a path to which a TSV of every
	// translated object's SHA1 → SHA256 mapping is written. Useful for
	// rewriting external systems that reference old commit hashes.
	MappingFile string

	// SkipMessageRewrite disables the inline rewrite of SHA1 hashes found
	// in commit and tag messages. Off by default (rewriting is on).
	SkipMessageRewrite bool

	// SkipOriginNotes disables the refs/notes/sha1-origin output that
	// records each translated commit's original SHA1. Off by default
	// (notes are written).
	SkipOriginNotes bool

	// Out receives human-readable status lines. Nil means os.Stderr.
	Out io.Writer
}

// Counts tallies converted objects by kind.
type Counts struct {
	Blobs   int `json:"blobs"`
	Trees   int `json:"trees"`
	Commits int `json:"commits"`
	Tags    int `json:"tags"`
}

// Result is the conversion summary, suitable for JSON output.
type Result struct {
	SourceURL            string   `json:"sourceUrl"`
	TargetDir            string   `json:"targetDir"`
	Protocol             string   `json:"protocol"`
	RefsConverted        int      `json:"refsConverted"`
	Counts               Counts   `json:"counts"`
	SignaturesStripped   int      `json:"signaturesStripped"`
	MessageRewrites      int      `json:"messageRewrites"`
	AmbiguousMessageRefs []string `json:"ambiguousMessageRefs,omitempty"`
	SkippedPullRefs      int      `json:"skippedPullRefs,omitempty"`
	OriginNotesRef       string   `json:"originNotesRef,omitempty"`
	MappingFile          string   `json:"mappingFile,omitempty"`
	SignedTags           []string `json:"signedTags,omitempty"`
	Checks               []Check  `json:"checks,omitempty"`
	TempDir              string   `json:"tempDir,omitempty"`
}

// Check is one named verification step from --check, with the result
// and a short detail string suitable for logging/JSON output.
//
// Skipped distinguishes "this check passed" from "this check did not
// run" — e.g. fsck when git is not on PATH, or HEAD on a tags-only
// conversion. Skipped implies OK so callers that only branch on OK
// still treat it as non-fatal; callers that need a stricter signal
// (CI gating, audit logs) should branch on Skipped first.
type Check struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// previewMax caps how many items from a potentially-long list (ambiguous
// prefixes, signed tags) are inlined into a Lines() summary before
// switching to a "(N more)" suffix.
const previewMax = 5

// previewJoin renders items as a comma-separated list, inlining at most
// previewMax of them and appending ", ... (N more<suffix>)" when the list
// is longer. suffix points at where the full list lives (e.g.
// "; full list in --json"); pass "" for none.
func previewJoin(items []string, suffix string) string {
	if len(items) <= previewMax {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s, ... (%d more%s)",
		strings.Join(items[:previewMax], ", "), len(items)-previewMax, suffix)
}

// Lines satisfies the human-readable output contract used by other git-sync subcommands.
func (r Result) Lines() []string {
	lines := []string{
		"sha256 bare repo: " + r.TargetDir,
		fmt.Sprintf("source: %s (%s)", r.SourceURL, r.Protocol),
		fmt.Sprintf("converted: %d blobs, %d trees, %d commits, %d tags",
			r.Counts.Blobs, r.Counts.Trees, r.Counts.Commits, r.Counts.Tags),
		fmt.Sprintf("refs written: %d", r.RefsConverted),
	}
	if r.SignaturesStripped > 0 {
		// Mixes commit/tag signatures (GPG/SSH/X.509) and embedded
		// mergetag headers — each counts as one signed artifact whose
		// signature became invalid post-rewrite.
		lines = append(lines, fmt.Sprintf("warning: stripped %d signature(s) / mergetag header(s); they no longer match the rewritten object content", r.SignaturesStripped))
	}
	if r.MessageRewrites > 0 {
		lines = append(lines, fmt.Sprintf("rewrote %d SHA1 hash reference(s) in commit/tag messages", r.MessageRewrites))
	}
	if n := len(r.AmbiguousMessageRefs); n > 0 {
		lines = append(lines, fmt.Sprintf("warning: %d ambiguous SHA1 hex prefix(es) in messages left unrewritten (look up via the mapping file): %s",
			n, previewJoin(r.AmbiguousMessageRefs, "")))
	}
	if r.SkippedPullRefs > 0 {
		lines = append(lines, fmt.Sprintf("excluded %d foreign pull/merge-request ref(s) (refs/pull/*, refs/pull-requests/*, refs/merge-requests/*) from --all-refs; pass --include-pull-refs to convert them", r.SkippedPullRefs))
	}
	if r.OriginNotesRef != "" {
		lines = append(lines, fmt.Sprintf("origin notes ref: %s (use `git notes --ref=%s show <sha256>` to recover old SHA1)",
			r.OriginNotesRef, strings.TrimPrefix(r.OriginNotesRef, "refs/notes/")))
	}
	if r.MappingFile != "" {
		lines = append(lines, "mapping written to: "+r.MappingFile)
	}
	if n := len(r.SignedTags); n > 0 {
		lines = append(lines, fmt.Sprintf("signed %d branch attestation tag(s): %s",
			n, previewJoin(r.SignedTags, "; full list in --json")))
	}
	if r.TempDir != "" {
		lines = append(lines, "kept source objects: "+r.TempDir)
	}
	return lines
}

// Run performs the conversion described by req.
//
//nolint:maintidx // Run is a linear orchestrator over distinct phases (fetch → discover → init → translate → refs → notes → mapping → sign → check); each phase is short and isolated. Splitting into helpers would obscure the pipeline rather than clarify it.
func Run(ctx context.Context, req Request) (Result, error) {
	if req.SourceURL == "" {
		return Result{}, errors.New("convert-sha256 requires --source-url")
	}
	if req.TargetDir == "" {
		return Result{}, errors.New("convert-sha256 requires a target directory")
	}
	// Enforce the documented invariant: every branch and every tag is
	// always converted. Otherwise the partial set could strand
	// cross-branch hash references in commit and tag messages, which
	// the message-rewrite pass is built to keep intact.
	if bad := protectedExcludePrefixes(req.ExcludeRefPrefixes); len(bad) > 0 {
		return Result{}, fmt.Errorf("convert-sha256 refuses --exclude-ref-prefix values that would drop branches or tags: %s (only namespaces outside refs/heads/ and refs/tags/ may be excluded)", strings.Join(bad, ", "))
	}
	switch req.SignMode {
	case "", SignModeNone, SignModeTips:
	default:
		return Result{}, fmt.Errorf("convert-sha256: unknown --sign-mode %q (valid: %s, %s)", req.SignMode, SignModeNone, SignModeTips)
	}
	out := req.Out
	if out == nil {
		out = os.Stderr
	}

	targetCreated, err := ensureEmptyTarget(req.TargetDir)
	if err != nil {
		return Result{}, err
	}

	tempDir, err := os.MkdirTemp("", "git-sync-sha256-src-")
	if err != nil {
		return Result{}, fmt.Errorf("create temp dir: %w", err)
	}
	cleanupTemp := true
	defer func() {
		if cleanupTemp {
			_ = os.RemoveAll(tempDir)
		}
	}()

	// cleanupTarget fires when set, undoing the SHA256 bare repo we
	// initialize below. Without it, any error after PlainInit leaves
	// config/objects/refs/HEAD behind, and the next retry hits
	// ensureEmptyTarget's "not empty" refusal with no indication of how
	// to recover. We restore the exact pre-run state: if this run created
	// the target directory, remove it entirely; if the user pre-created it
	// (empty — a mountpoint, or a dir whose ownership/ACLs they set up),
	// remove only the entries we added and leave the directory in place.
	// Suppressed by --keep-source-objects so users can inspect partial
	// state.
	cleanupTarget := false
	defer func() {
		if cleanupTarget && !req.KeepSourceObjects {
			cleanupConvertedTarget(req.TargetDir, targetCreated)
		}
	}()

	// Credentials embedded in the URL (https://user:token@host/...) must
	// never reach status output, the JSON result, or — worst of all — the
	// signed attestation tag message, which is permanent and gets pushed.
	// The fetch path keeps the original req.SourceURL; only these
	// human-facing / persisted copies are redacted.
	redactedSourceURL := redactSourceURL(req.SourceURL)

	// Build the result struct early so error paths can surface
	// what little ran successfully. In particular, --keep-source-objects
	// exists to debug failures, so cleanupTemp must flip and TempDir
	// must be in the result *before* any later error return; otherwise
	// the temp store gets wiped on exactly the runs that need it.
	res := Result{SourceURL: redactedSourceURL, TargetDir: req.TargetDir}
	if req.KeepSourceObjects {
		cleanupTemp = false
		res.TempDir = tempDir
	}

	srcRepo, err := git.PlainInit(tempDir, true)
	if err != nil {
		return res, fmt.Errorf("init temporary SHA1 store: %w", err)
	}

	// Source connection + ref discovery -----------------------------------
	// Scope is fixed: always include every branch and every tag. AllRefs
	// extends to refs/notes/* and other namespaces; ExcludeRefPrefixes
	// can subtract from that under AllRefs. Pull/merge-request namespaces
	// are excluded by default (foreign code) unless --include-pull-refs.
	planCfg := planner.PlanConfig{
		IncludeTags:        true,
		AllRefs:            req.AllRefs,
		ExcludeRefPrefixes: effectiveExcludePrefixes(req.ExcludeRefPrefixes, req.AllRefs, req.IncludePullRefs),
	}
	conn, refService, sourceRefList, err := openSource(ctx, req, planCfg)
	if err != nil {
		return res, err
	}
	defer conn.Close()
	refService.Verbose = req.Verbose

	sourceRefs := gitproto.RefHashMap(sourceRefList)
	desired, _, err := planner.BuildDesiredRefs(sourceRefs, planCfg)
	if err != nil {
		return res, fmt.Errorf("build desired refs: %w", err)
	}
	if len(desired) == 0 {
		return res, errors.New("no source refs matched the requested scope")
	}

	// Surface how many pull/merge-request refs the default exclusion
	// dropped, so an --all-refs run doesn't silently omit them. Only
	// meaningful when we actually excluded them.
	if req.AllRefs && !req.IncludePullRefs {
		if skipped := countForeignPullRefs(sourceRefs); skipped > 0 {
			res.SkippedPullRefs = skipped
			fmt.Fprintf(out, "excluding %d foreign pull/merge-request ref(s) from --all-refs (pass --include-pull-refs to convert them) ...\n", skipped)
		}
	}

	// Refuse before any further I/O if the source carries refs that
	// would collide with our side outputs. writeRefs runs before
	// writeOriginNotes / signBranchTips, so without this check the
	// later side-output write would silently clobber the source ref.
	if err := checkSideOutputCollision(desired, req.SkipOriginNotes, req.SignMode == SignModeTips); err != nil {
		return res, err
	}

	// Fetch into temp SHA1 store ------------------------------------------
	fmt.Fprintf(out, "fetching %d ref(s) from %s ...\n", len(desired), redactedSourceURL)
	gpDesired := convert.DesiredRefs(desired)
	if err := refService.FetchToStore(ctx, srcRepo.Storer, conn, gpDesired, nil); err != nil &&
		!errors.Is(err, git.NoErrAlreadyUpToDate) {
		return res, fmt.Errorf("fetch source pack: %w", err)
	}

	// Discover reachable set before initing the target. Submodule
	// errors surface here, so a failed run leaves the target dir
	// untouched (it was only ensured-empty so far) rather than half
	// converted.
	rootSHA1s := make([]plumbing.Hash, 0, len(desired))
	for _, d := range desired {
		rootSHA1s = append(rootSHA1s, d.SourceHash)
	}
	fmt.Fprintln(out, "discovering reachable objects ...")
	progressActive := req.Progress && isTTY(out)
	var discCounter *atomic.Int64
	var stopDisc func()
	if progressActive {
		c := new(atomic.Int64)
		discCounter = c
		stopDisc = startProgressTick(out, func() string {
			return fmt.Sprintf("  discovered %d objects", c.Load())
		})
	}
	reachable, err := discoverReachable(ctx, srcRepo.Storer, rootSHA1s, discCounter)
	if stopDisc != nil {
		stopDisc()
	}
	if err != nil {
		return res, fmt.Errorf("discover reachable: %w", err)
	}

	// Discovery succeeded — safe to materialize the SHA256 target.
	dstRepo, err := git.PlainInit(req.TargetDir, true, git.WithObjectFormat(formatcfg.SHA256))
	if err != nil {
		return res, fmt.Errorf("init SHA256 target at %s: %w", req.TargetDir, err)
	}
	// Anything that fails past here would leave the target dir
	// non-empty (config + HEAD + maybe objects/refs), blocking a
	// retry on ensureEmptyTarget; arm the deferred cleanup now.
	cleanupTarget = true

	tr, err := newTranslator(ctx, srcRepo.Storer, dstRepo.Storer, !req.SkipMessageRewrite, reachable)
	if err != nil {
		return res, err
	}
	fmt.Fprintln(out, "translating objects to sha256 ...")
	var stopTr func()
	if progressActive {
		stopTr = startProgressTick(out, func() string {
			return fmt.Sprintf("  translated %d blobs, %d trees, %d commits, %d tags",
				tr.blobs.Load(), tr.trees.Load(), tr.commitsCount.Load(), tr.tags.Load())
		})
	}
	for _, d := range desired {
		if _, err := tr.translate(d.SourceHash); err != nil {
			if stopTr != nil {
				stopTr()
			}
			return res, fmt.Errorf("translate %s: %w", d.SourceRef, err)
		}
	}
	if stopTr != nil {
		stopTr()
	}

	// Write refs ---------------------------------------------------------
	refsWritten, err := writeRefs(dstRepo.Storer, desired, tr.mapping)
	if err != nil {
		return res, fmt.Errorf("write target refs: %w", err)
	}

	// Point HEAD at a ref that actually exists in the target. PlainInit
	// defaults HEAD to refs/heads/master, which often doesn't exist
	// (e.g. repos using "main"), and would then fail the --check HEAD
	// step. See pickHEAD for the selection order.
	if headRef := pickHEAD(refService.HeadTarget, desired); headRef != "" {
		if err := dstRepo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, headRef)); err != nil {
			return res, fmt.Errorf("set HEAD: %w", err)
		}
	}

	// The converted repo is now complete: every reachable object is
	// written, all refs point at translated tips, and HEAD resolves.
	// Everything past here — origin notes, the mapping file, signing, and
	// --check — is optional enrichment or post-hoc verification. A failure
	// in any of those must surface the error but must NOT delete a
	// successful conversion: a multi-hour kernel-scale run, a --write-mapping
	// path typo, or a signing-key misconfig should never silently discard
	// the repo the user just built. Disarm the target cleanup here so those
	// steps leave the converted repo on disk for inspection or re-run.
	cleanupTarget = false

	res.Protocol = refService.Protocol
	res.RefsConverted = refsWritten
	res.Counts = tr.snapshotCounts()
	res.SignaturesStripped = tr.signaturesStripped
	res.MessageRewrites = tr.messageRewrites
	if len(tr.ambiguousMessageRefs) > 0 {
		amb := make([]string, 0, len(tr.ambiguousMessageRefs))
		for s := range tr.ambiguousMessageRefs {
			amb = append(amb, s)
		}
		sort.Strings(amb)
		res.AmbiguousMessageRefs = amb
	}

	if !req.SkipOriginNotes && len(tr.commits) > 0 {
		notesRef, err := tr.writeOriginNotes(originNotesRef)
		if err != nil {
			return res, fmt.Errorf("write origin notes: %w", err)
		}
		if err := dstRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(notesRef), tr.lastNotesCommit)); err != nil {
			return res, fmt.Errorf("set %s: %w", notesRef, err)
		}
		res.OriginNotesRef = notesRef
	}

	if req.MappingFile != "" {
		if err := tr.writeMappingFile(req.MappingFile); err != nil {
			return res, fmt.Errorf("write mapping file: %w", err)
		}
		res.MappingFile = req.MappingFile
	}

	if req.SignMode == SignModeTips {
		signed, err := signBranchTips(ctx, out, req.TargetDir, req.SignKey, redactedSourceURL, desired)
		// signBranchTips returns the tags it had already created
		// when it failed mid-iteration. Surface that partial list
		// even on error so the caller can clean up — without it,
		// signed converted/* tags would be left on disk with no
		// indication in either Result or the error.
		res.SignedTags = signed
		if err != nil {
			return res, fmt.Errorf("sign: %w", err)
		}
	}

	if req.Check {
		fmt.Fprintln(out, "verifying output ...")
		// Collect the side outputs this run actually wrote so the
		// refs check knows which target refs to ignore. Anything not
		// in here is assumed to be a translated source ref.
		sideOutputs := make(map[plumbing.ReferenceName]struct{}, 1+len(res.SignedTags))
		if res.OriginNotesRef != "" {
			sideOutputs[plumbing.ReferenceName(res.OriginNotesRef)] = struct{}{}
		}
		for _, tag := range res.SignedTags {
			sideOutputs[plumbing.ReferenceName(tag)] = struct{}{}
		}
		hasBranches := false
		for _, d := range desired {
			if d.TargetRef.IsBranch() {
				hasBranches = true
				break
			}
		}
		res.Checks = runChecks(ctx, req.TargetDir, dstRepo, refsWritten, sideOutputs, hasBranches)
		for _, c := range res.Checks {
			mark := "✓"
			switch {
			case !c.OK:
				mark = "✗"
			case c.Skipped:
				mark = "○"
			}
			fmt.Fprintf(out, "  %s %s: %s\n", mark, c.Name, c.Detail)
		}
		for _, c := range res.Checks {
			if !c.OK {
				// The conversion finished; a failed check is a
				// post-hoc verification miss, so the target stays
				// on disk (cleanup was already disarmed once the
				// conversion completed) for the user to inspect
				// exactly what failed.
				return res, fmt.Errorf("check %q failed: %s", c.Name, c.Detail)
			}
		}
	}

	// Run completed successfully; the target dir is kept (cleanup was
	// disarmed once the conversion completed above).
	return res, nil
}

// signBranchTips runs `git tag -s converted/<branch> <branch>` for every
// branch in the desired set. The converter's signing identity (whatever
// `user.signingkey` / `gpg.format` is set to in the target repo, or the
// caller-supplied signKey) attests each branch's full reachable history
// via the parent chain encoded in the tip commit's bytes.
//
// stdin/stderr are inherited so gpg/ssh-agent prompts work
// interactively. A failure short-circuits the run; tags signed before
// the failure stay in the target repo.
func signBranchTips(ctx context.Context, out io.Writer, targetDir, signKey, sourceURL string, desired map[plumbing.ReferenceName]planner.DesiredRef) ([]string, error) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("git binary required to sign: %w", err)
	}
	// Iterate in a deterministic order so re-runs over the same source
	// produce the same sequence of tags (modulo the signature payload,
	// which carries the signer's timestamp).
	branchNames := make([]string, 0, len(desired))
	for name := range desired {
		if name.IsBranch() {
			branchNames = append(branchNames, string(name))
		}
	}
	sort.Strings(branchNames)

	var signed []string
	for _, refName := range branchNames {
		shortName := plumbing.ReferenceName(refName).Short()
		tagName := strings.TrimPrefix(attestationTagPrefix, "refs/tags/") + shortName
		fmt.Fprintf(out, "signing %s ...\n", "refs/tags/"+tagName)

		msg := fmt.Sprintf(
			"SHA1 → SHA256 conversion attestation for %s.\n\n"+
				"Source: %s\nProduced by git-sync convert-sha256.\n",
			refName, sourceURL)
		args := []string{"-C", targetDir, "tag", "-s", "-m", msg}
		if signKey != "" {
			args = append(args, "-u", signKey)
		}
		args = append(args, tagName, refName)

		cmd := exec.CommandContext(ctx, gitBin, args...)
		// Deliberate departure from the req.Out plumbing the rest of
		// Run uses: gpg/ssh-agent and pinentry need a real TTY for
		// passphrase prompts, so we inherit the parent's stdio
		// directly. The consequence is that callers passing
		// req.Out = io.Discard (e.g. tests) still see subprocess
		// output on real stderr — that's the cost of working
		// authentication.
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stderr // git tag -s is usually quiet on success
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return signed, fmt.Errorf("git tag -s %s: %w", tagName, err)
		}
		signed = append(signed, "refs/tags/"+tagName)
	}
	return signed, nil
}

// runChecks performs lightweight verification of the converted repo.
// Returns one Check per step. Callers print and/or fail-on-error based
// on these. No early return so users see the full picture even when an
// earlier check fails.
//
// sideOutputs holds the exact refs the run created on top of the
// source set (the origin-notes ref, any --sign-mode tips attestation tags), so
// the refs check can omit them from the resolved/expected fraction
// without false-positive-skipping a same-named source ref.
//
// hasBranches says whether any refs/heads/* landed in the target. If
// false, this is a tags-only conversion and HEAD is left at the
// PlainInit default (refs/heads/master, which won't exist); the HEAD
// check is then a no-op rather than a guaranteed failure.
func runChecks(ctx context.Context, targetDir string, repo *git.Repository, refsExpected int, sideOutputs map[plumbing.ReferenceName]struct{}, hasBranches bool) []Check {
	checks := []Check{}

	// 1. Config: extensions.objectformat = sha256. Parse the file
	// section-aware so we don't false-positive on a commented line or
	// a similarly-named key in another section.
	cfgFile, err := os.Open(filepath.Join(targetDir, "config"))
	switch {
	case err != nil:
		checks = append(checks, Check{Name: "config", OK: false, Detail: err.Error()})
	default:
		cfg := formatcfg.New()
		decodeErr := formatcfg.NewDecoder(cfgFile).Decode(cfg)
		_ = cfgFile.Close()
		switch {
		case decodeErr != nil:
			checks = append(checks, Check{Name: "config", OK: false, Detail: fmt.Sprintf("parse config: %v", decodeErr)})
		case !strings.EqualFold(cfg.Section("extensions").Option("objectformat"), "sha256"):
			checks = append(checks, Check{Name: "config", OK: false, Detail: "extensions.objectformat = sha256 not set"})
		default:
			checks = append(checks, Check{Name: "config", OK: true, Detail: "extensions.objectformat = sha256"})
		}
	}

	// 2. HEAD resolves to an existing object. Skipped on tags-only
	// conversions, where the target legitimately has no branch for
	// HEAD to symlink to.
	switch {
	case !hasBranches:
		checks = append(checks, Check{Name: "HEAD", OK: true, Skipped: true, Detail: "tags-only conversion; no branch to point at"})
	default:
		head, err := repo.Reference(plumbing.HEAD, true)
		switch {
		case err != nil:
			checks = append(checks, Check{Name: "HEAD", OK: false, Detail: err.Error()})
		case head.Hash().IsZero():
			checks = append(checks, Check{Name: "HEAD", OK: false, Detail: "resolves to zero hash"})
		default:
			if _, err := repo.Storer.EncodedObject(plumbing.AnyObject, head.Hash()); err != nil {
				checks = append(checks, Check{Name: "HEAD", OK: false, Detail: fmt.Sprintf("%s: %v", head.Hash(), err)})
			} else {
				checks = append(checks, Check{Name: "HEAD", OK: true, Detail: head.Hash().String()})
			}
		}
	}

	// 3. Every written ref resolves to an existing object. Skip the
	// specific refs this run created as side outputs — they're
	// accounted for in their own Result fields and would otherwise
	// make the displayed fraction misleading. Skipping by exact name
	// (not by prefix) avoids hiding a legitimate source ref that
	// happened to share a namespace.
	resolved := 0
	missing := ""
	refs, err := repo.References()
	if err != nil {
		checks = append(checks, Check{Name: "refs", OK: false, Detail: err.Error()})
	} else {
		walkErr := refs.ForEach(func(r *plumbing.Reference) error {
			if r.Type() != plumbing.HashReference {
				return nil
			}
			if _, skip := sideOutputs[r.Name()]; skip {
				return nil
			}
			if _, err := repo.Storer.EncodedObject(plumbing.AnyObject, r.Hash()); err != nil {
				if missing == "" {
					missing = fmt.Sprintf("%s → %s: %v", r.Name(), r.Hash(), err)
				}
				return nil
			}
			resolved++
			return nil
		})
		switch {
		case walkErr != nil:
			checks = append(checks, Check{Name: "refs", OK: false, Detail: walkErr.Error()})
		case missing != "":
			checks = append(checks, Check{Name: "refs", OK: false, Detail: missing})
		case resolved < refsExpected:
			checks = append(checks, Check{Name: "refs", OK: false, Detail: fmt.Sprintf("only %d / %d refs resolved", resolved, refsExpected)})
		default:
			checks = append(checks, Check{Name: "refs", OK: true, Detail: fmt.Sprintf("%d / %d resolve to objects", resolved, refsExpected)})
		}
	}

	// 4. git fsck --full (if git is on PATH).
	gitBin, err := exec.LookPath("git")
	if err != nil {
		checks = append(checks, Check{Name: "git fsck --full", OK: true, Skipped: true, Detail: "git not in PATH"})
		return checks
	}
	cmd := exec.CommandContext(ctx, gitBin, "-C", targetDir, "fsck", "--full")
	fsckOut, err := cmd.CombinedOutput()
	switch {
	case err != nil:
		checks = append(checks, Check{Name: "git fsck --full", OK: false, Detail: fmt.Sprintf("%v\n%s", err, fsckOut)})
	case fsckHasError(fsckOut):
		// Belt-and-braces against a hypothetical git version that prints
		// "error:" / "fatal:" lines but exits zero. Match line prefixes
		// rather than a substring so a branch or path containing "error"
		// in a benign dangling/warning line doesn't trip the check.
		checks = append(checks, Check{Name: "git fsck --full", OK: false, Detail: strings.TrimSpace(string(fsckOut))})
	default:
		checks = append(checks, Check{Name: "git fsck --full", OK: true, Detail: "clean"})
	}
	return checks
}

// fsckHasError reports whether git-fsck output contains a line that
// signals a real problem. We match (case-insensitively) any line whose
// first token starts with "error" or "fatal" — covering "error:",
// "fatal:", and the rare "errorInX:" variants — plus the
// "missing <type> <sha>" / "broken link" / "bad <thing>" object reports
// emitted by older git. Dangling and warning lines are intentionally
// ignored.
//
// Splits on raw newlines rather than using bufio.Scanner so a single
// very long line (some fsck reports include long paths) is not
// silently truncated at the scanner's 64 KiB default.
func fsckHasError(out []byte) bool {
	for _, raw := range bytes.Split(out, []byte("\n")) {
		line := strings.TrimSpace(string(raw))
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "error") || strings.HasPrefix(lower, "fatal") {
			return true
		}
		if strings.HasPrefix(lower, "missing ") || strings.HasPrefix(lower, "broken link") || strings.HasPrefix(lower, "bad ") {
			return true
		}
	}
	return false
}

const (
	originNotesRef       = "refs/notes/sha1-origin"
	attestationTagPrefix = "refs/tags/converted/"
)

// --sign-mode values. SignModeNone (and the empty string) sign nothing;
// SignModeTips mints one signed attestation tag per branch tip. The set
// is deliberately small and forward-compatible: an "all" mode that signs
// every commit/tag could be added without changing the flag's shape.
const (
	SignModeNone = "none"
	SignModeTips = "tips"
)

// foreignPullRefPrefixes are the server-internal pull/merge-request
// namespaces that hold code proposed from forks and other branches —
// content foreign to the repository's own history until merged. They are
// excluded from --all-refs by default: the converted repo is usually
// mirrored onward with `git push --mirror`, and a destination forge may
// surface these refs as ordinary refs, republishing unreviewed code as if
// it were part of the repo. --include-pull-refs opts back in.
var foreignPullRefPrefixes = []string{
	"refs/pull/",           // GitHub, Gitea, Forgejo
	"refs/pull-requests/",  // Bitbucket Server / Data Center
	"refs/merge-requests/", // GitLab
}

// effectiveExcludePrefixes combines the user's --exclude-ref-prefix values
// with the default pull/merge-request exclusions. The latter only apply
// under --all-refs (the namespaces are out of scope otherwise) and only
// when the user did not pass --include-pull-refs.
func effectiveExcludePrefixes(userPrefixes []string, allRefs, includePullRefs bool) []string {
	out := append([]string(nil), userPrefixes...)
	if allRefs && !includePullRefs {
		out = append(out, foreignPullRefPrefixes...)
	}
	return out
}

// countForeignPullRefs reports how many source refs fall under a
// pull/merge-request namespace, so the run can tell the user exactly how
// many refs the default exclusion dropped rather than silently omitting
// them from an --all-refs conversion.
func countForeignPullRefs(refs map[plumbing.ReferenceName]plumbing.Hash) int {
	n := 0
	for name := range refs {
		for _, p := range foreignPullRefPrefixes {
			if strings.HasPrefix(string(name), p) {
				n++
				break
			}
		}
	}
	return n
}

// protectedExcludePrefixes returns the subset of prefixes that, under
// planner.IsRefExcluded's string-prefix semantics, would knock out at
// least one branch or tag. A prefix matches a branch if either side
// is a string-prefix of the other against "refs/heads/" (and likewise
// for "refs/tags/"). That covers:
//
//   - bare "" (excludes every ref)
//   - "refs/" or "refs/h", "refs/heads/" (whole branch namespace)
//   - "refs/heads/feature/" (some branches)
//   - "refs/tags/" and any narrower suffix
//
// Returned in input order, with duplicates removed, so the error
// message shows the user exactly which flag values to drop.
func protectedExcludePrefixes(prefixes []string) []string {
	protected := []string{"refs/heads/", "refs/tags/"}
	var bad []string
	seen := map[string]struct{}{}
	for _, raw := range prefixes {
		p := strings.TrimSpace(raw)
		if _, dup := seen[p]; dup {
			continue
		}
		for _, prot := range protected {
			if strings.HasPrefix(p, prot) || strings.HasPrefix(prot, p) {
				bad = append(bad, raw)
				seen[p] = struct{}{}
				break
			}
		}
	}
	return bad
}

// checkSideOutputCollision refuses the conversion when the source set
// already contains a ref name this run would later write as a side
// output. Without this guard, writeRefs would publish the source's
// value first and writeOriginNotes / signBranchTips would silently
// overwrite it — losing the source ref and hiding the conflict.
func checkSideOutputCollision(desired map[plumbing.ReferenceName]planner.DesiredRef, skipOriginNotes, signTips bool) error {
	if !skipOriginNotes {
		if _, conflict := desired[plumbing.ReferenceName(originNotesRef)]; conflict {
			return fmt.Errorf("source already advertises %s; pass --no-origin-notes to keep that source ref, or --exclude-ref-prefix %s to drop it from the conversion", originNotesRef, originNotesRef)
		}
	}
	if signTips {
		var clashes []string
		for name := range desired {
			if strings.HasPrefix(string(name), attestationTagPrefix) {
				clashes = append(clashes, string(name))
			}
		}
		if len(clashes) > 0 {
			sort.Strings(clashes)
			return fmt.Errorf("source has %s under %s, which collides with the attestation tags --sign-mode tips would create; use --sign-mode none or rename the source tag(s)", strings.Join(clashes, ", "), attestationTagPrefix)
		}
	}
	return nil
}

// ensureEmptyTarget refuses to init into a non-empty directory so the user
// doesn't quietly accumulate objects into an existing repo. It reports
// created=true when it had to make the directory, so the caller's failure
// cleanup can tell "remove the whole tree we created" apart from "remove
// only the contents we added to a directory the user pre-created".
func ensureEmptyTarget(path string) (created bool, err error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			if mkErr := os.MkdirAll(path, 0o755); mkErr != nil {
				return false, fmt.Errorf("create target dir: %w", mkErr)
			}
			return true, nil
		}
		return false, fmt.Errorf("read target dir: %w", err)
	}
	if len(entries) > 0 {
		return false, fmt.Errorf("target directory %s is not empty", path)
	}
	return false, nil
}

// cleanupConvertedTarget restores the target directory to the state it had
// before the run, for use on a failure path. When the run created the
// directory (created=true) it is removed outright. When the user
// pre-created it — ensureEmptyTarget accepts an existing empty directory —
// only the entries the run added are removed, leaving the directory and
// any ownership, permissions, ACLs, or mount the user set up intact.
//
// Best-effort, like the temp-dir cleanup defer: a removal error is not
// surfaced because the run is already failing for another reason.
func cleanupConvertedTarget(path string, created bool) {
	if created {
		_ = os.RemoveAll(path)
		return
	}
	removeDirContents(path)
}

// removeDirContents removes every entry inside dir but leaves dir itself
// in place. Best-effort (see cleanupConvertedTarget).
func removeDirContents(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// redactSourceURL removes any credentials embedded in a source URL so
// they never reach status output, the JSON result, or the signed
// attestation tag message. The entire userinfo component is stripped,
// not just the password: token auth commonly carries the secret in the
// username position (https://<token>@host/...), which url.URL.Redacted()
// would leave intact. The fetch path keeps the original req.SourceURL,
// so stripping here does not affect authentication.
//
// If the URL cannot be parsed (openSource parses the same string and
// fails the run otherwise), we return a placeholder rather than risk
// echoing credentials we could not locate.
func redactSourceURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<source url redacted>"
	}
	u.User = nil
	return u.String()
}

func openSource(ctx context.Context, req Request, planCfg planner.PlanConfig) (gitproto.Conn, *gitproto.RefService, []*plumbing.Reference, error) {
	ep, err := url.Parse(req.SourceURL)
	if err != nil {
		// url.Parse wraps the raw URL in its error (`parse "<url>": ...`),
		// which would leak embedded credentials (https://user:token@host)
		// into status output, logs, and CI. Surface only the underlying
		// reason — *url.Error.Err carries the cause without the URL string.
		reason := err
		var ue *url.Error
		if errors.As(err, &ue) {
			reason = ue.Err
		}
		return nil, nil, nil, fmt.Errorf("parse source URL: %w", reason)
	}
	if ep.Scheme != "http" && ep.Scheme != "https" {
		return nil, nil, nil, fmt.Errorf("convert-sha256 currently supports HTTP/HTTPS sources only; got %q", ep.Scheme)
	}
	authMethod, err := auth.Resolve(auth.Endpoint{
		Username:      req.SourceAuth.Username,
		Token:         req.SourceAuth.Token,
		BearerToken:   req.SourceAuth.BearerToken,
		SkipTLSVerify: req.SourceAuth.SkipTLSVerify,
	}, ep)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve source auth: %w", err)
	}
	httpClient := &http.Client{Transport: gitproto.NewHTTPTransport(req.SourceAuth.SkipTLSVerify)}
	conn := gitproto.NewHTTPConnWithClient(ep, "source", normalizeAuth(authMethod), httpClient)
	conn.FollowInfoRefsRedirect = req.SourceFollowInfoRefsRedirect

	mode := string(req.ProtocolMode)
	if mode == "" {
		mode = string(gitsync.ProtocolAuto)
	}

	refs, svc, err := gitproto.ListSourceRefs(ctx, conn, mode, planner.RefPrefixes(planCfg))
	if err != nil {
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("list source refs: %w", err)
	}
	return conn, svc, refs, nil
}

func normalizeAuth(m auth.Method) gitproto.AuthMethod {
	if m == nil {
		return nil
	}
	// auth.Method and gitproto.AuthMethod share the same Authorizer signature.
	// Wrap so we can pass either *transporthttp.BasicAuth or *transporthttp.TokenAuth.
	if a, ok := m.(*transporthttp.BasicAuth); ok {
		return a
	}
	if a, ok := m.(*transporthttp.TokenAuth); ok {
		return a
	}
	return authAdapter{m: m}
}

type authAdapter struct{ m auth.Method }

func (a authAdapter) Authorizer(req *http.Request) error {
	if err := a.m.Authorizer(req); err != nil {
		return fmt.Errorf("authorize request: %w", err)
	}
	return nil
}

// translator walks the SHA1 source store, rewrites object content with
// SHA256-mapped hashes, and writes the result into the target bare repo
// via SetEncodedObject. The target storer is configured for SHA256 (see
// the PlainInit in Run), so go-git hashes and names every loose object
// under SHA256.
type translator struct {
	// ctx is checked at the top of every translate() call so a Ctrl-C
	// during a million-object conversion is responsive. It is the same
	// context passed to Run() and is not stored to outlive its caller.
	ctx context.Context //nolint:containedctx // translate() is recursive and not directly called by Run; threading ctx through every signature is noisier than a single field used for cancellation only.
	src *filesystem.Storage
	// dst is the SHA256-configured target store. Each translated object
	// is built with dst.NewEncodedObject (which binds it to the target's
	// SHA256 hasher) and persisted with dst.SetEncodedObject, so both the
	// returned hash and the on-disk loose path are computed under SHA256.
	dst storer.EncodedObjectStorer
	// reachable holds every in-scope SHA1 with its object type, built up
	// front by discoverReachable, which walks tree/commit/tag dependencies
	// from the desired ref tips. It is the authoritative "what's in
	// scope" set: abbreviated SHA1 prefixes in commit/tag messages are
	// resolved against this set so a unique match is fixed before any
	// encoding starts, and so message-reference edges can be added to
	// the translation DFS in topological order.
	reachable map[plumbing.Hash]plumbing.ObjectType
	mapping   map[plumbing.Hash]plumbing.Hash
	// inProgress detects cycles in the translation DFS. Real Git
	// histories cannot form cycles (the parent/tree/tag-target edges
	// are a DAG by construction, and SHA1 message-reference cycles are
	// cryptographically infeasible), but a defensive guard turns
	// surprising input into a clear error instead of a stack overflow.
	inProgress map[plumbing.Hash]struct{}
	// commits records every translated commit's old SHA1, in DFS order,
	// for use by writeOriginNotes. We track separately rather than walking
	// the full mapping because notes only attach meaningfully to commits.
	commits []plumbing.Hash
	// ambiguousMessageRefs collects every hex prefix in a commit/tag
	// message that matched more than one in-scope SHA1 and was
	// therefore left unrewritten. Surfaced to the user as a warning
	// so they know which references to investigate via the mapping
	// file.
	ambiguousMessageRefs map[string]struct{}
	// resolveCache memoizes resolveMessageRef results. reachable is
	// frozen before translation starts, so the (prefix → matchResult)
	// mapping is stable for the lifetime of the translator. The
	// abbreviated-hash path costs O(len(reachable)) per distinct prefix;
	// caching collapses repeats — the same hash cited across many
	// messages, or more than once in one — to a single scan.
	resolveCache map[string]resolveCacheEntry
	// Live counts updated atomically so the --progress ticker goroutine
	// can sample them without racing against translation. Snapshot into
	// a Counts struct at the end of the run.
	blobs              atomic.Int64
	trees              atomic.Int64
	commitsCount       atomic.Int64
	tags               atomic.Int64
	signaturesStripped int
	messageRewrites    int
	rewriteMessages    bool
	lastNotesCommit    plumbing.Hash
}

func (t *translator) snapshotCounts() Counts {
	return Counts{
		Blobs:   int(t.blobs.Load()),
		Trees:   int(t.trees.Load()),
		Commits: int(t.commitsCount.Load()),
		Tags:    int(t.tags.Load()),
	}
}

func newTranslator(ctx context.Context, src storer.Storer, dst storer.EncodedObjectStorer, rewriteMessages bool, reachable map[plumbing.Hash]plumbing.ObjectType) (*translator, error) {
	srcFS, ok := src.(*filesystem.Storage)
	if !ok {
		return nil, fmt.Errorf("source storage is not filesystem-backed (%T)", src)
	}
	if reachable == nil {
		reachable = make(map[plumbing.Hash]plumbing.ObjectType)
	}
	return &translator{
		ctx:                  ctx,
		src:                  srcFS,
		dst:                  dst,
		reachable:            reachable,
		mapping:              make(map[plumbing.Hash]plumbing.Hash),
		inProgress:           make(map[plumbing.Hash]struct{}),
		ambiguousMessageRefs: make(map[string]struct{}),
		resolveCache:         make(map[string]resolveCacheEntry),
		rewriteMessages:      rewriteMessages,
	}, nil
}

// discoverReachable walks every object reachable from roots (via tree
// entries, commit tree+parent links, and tag targets) and returns a
// (SHA1 → object type) map covering the full in-scope set.
//
// Submodule gitlinks: any submodule entry (mode 160000) fails the run
// here, before the target bare repo is initialized — failing fast
// keeps half-converted state off disk. Rewriting the gitlink to SHA256
// would produce a tree the upstream .gitmodules repo can never
// resolve, since it advertises only SHA1.
//
// Message-reference edges are not part of this pass; those are added
// during translation, where the partial mapping is updated as we go.
//
// If progress is non-nil, it is incremented once per object visited.
// The --progress ticker samples this counter from another goroutine.
func discoverReachable(ctx context.Context, src storer.Storer, roots []plumbing.Hash, progress *atomic.Int64) (map[plumbing.Hash]plumbing.ObjectType, error) {
	srcFS, ok := src.(*filesystem.Storage)
	if !ok {
		return nil, fmt.Errorf("source storage is not filesystem-backed (%T)", src)
	}
	reachable := make(map[plumbing.Hash]plumbing.ObjectType)

	// Iterative DFS with an explicit stack. The previous recursive
	// implementation walked deep linear histories (50k–100k commits
	// is not unheard of) one Go stack frame deep per parent edge,
	// growing the goroutine stack by tens of MiB on kernel-scale
	// runs. The explicit stack keeps memory usage proportional to
	// the in-flight frontier, not the longest chain.
	stack := make([]plumbing.Hash, 0, len(roots))
	stack = append(stack, roots...)
	for len(stack) > 0 {
		// Per-object cancellation check. Discovery on a kernel-scale
		// repo runs for several minutes before translate() takes
		// over, so without this Ctrl-C would not interrupt the run
		// until the discovery phase finished on its own.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("discover: %w", err)
		}
		sha1 := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, seen := reachable[sha1]; seen {
			continue
		}
		obj, err := srcFS.EncodedObject(plumbing.AnyObject, sha1)
		if err != nil {
			return nil, fmt.Errorf("discover %s: %w", sha1, err)
		}
		reachable[sha1] = obj.Type()
		if progress != nil {
			progress.Add(1)
		}
		switch obj.Type() { //nolint:exhaustive // OFSDelta/REFDelta/AnyObject/InvalidObject cannot reach a resolved storage.
		case plumbing.BlobObject:
			// No outgoing edges.
		case plumbing.TreeObject:
			tree := &object.Tree{}
			if err := tree.Decode(obj); err != nil {
				return nil, fmt.Errorf("discover decode tree %s: %w", sha1, err)
			}
			for _, e := range tree.Entries {
				if e.Mode == filemode.Submodule {
					// A submodule gitlink stores a hash that refers to a
					// commit in a *different* repository — the one named
					// by the matching .gitmodules URL. Even when that
					// commit happens to be in our source store, the URL
					// still points at an upstream SHA1 repo, so rewriting
					// the gitlink to SHA256 produces a tree that fsck-
					// passes but breaks `git submodule update` forever:
					// the upstream advertises only SHA1 hashes. The only
					// safe answer is to refuse and let the caller scope
					// the offending ref out (or convert the submodule
					// upstream first and re-point .gitmodules).
					return nil, fmt.Errorf(
						"tree %s contains a submodule gitlink %q at %s; convert-sha256 cannot rewrite submodule pointers "+
							"because the linked-to repository would still advertise SHA1 hashes — "+
							"exclude refs that reference it or convert the submodule repository first",
						sha1, e.Name, e.Hash)
				}
				stack = append(stack, e.Hash)
			}
		case plumbing.CommitObject:
			c := &object.Commit{}
			if err := c.Decode(obj); err != nil {
				return nil, fmt.Errorf("discover decode commit %s: %w", sha1, err)
			}
			stack = append(stack, c.TreeHash)
			stack = append(stack, c.ParentHashes...)
		case plumbing.TagObject:
			tag := &object.Tag{}
			if err := tag.Decode(obj); err != nil {
				return nil, fmt.Errorf("discover decode tag %s: %w", sha1, err)
			}
			stack = append(stack, tag.Target)
		default:
			return nil, fmt.Errorf("unexpected object type %v for %s during discovery", obj.Type(), sha1)
		}
	}
	return reachable, nil
}

// translate is intentionally recursive. Unlike discoverReachable's
// purely-structural DFS, translate's edges are dynamic: tree entries,
// commit parents, tag targets, *and* message-reference edges resolved
// against the partial mapping built so far. Converting that to an
// explicit work stack would require an "after-children" callback per
// object type and is easy to get subtly wrong (re-encoding before all
// referenced hashes are placed silently corrupts the message rewrite).
//
// Recursion depth is bounded by the longest dependency chain in the
// source DAG — in practice the longest commit-parent chain, since
// trees and tags add at most one frame each. Linux kernel history is
// O(70k) commits along its deepest single-parent path; Go's growable
// stacks comfortably absorb that (~tens of MiB). Cycle detection above
// turns any unexpected graph shape into a clear error rather than a
// stack-overflow crash.
func (t *translator) translate(sha1 plumbing.Hash) (plumbing.Hash, error) {
	// Cheap per-object cancellation check so Ctrl-C during a long
	// conversion (kernel-scale: ~10M objects) returns promptly rather
	// than running the whole DFS to completion.
	if err := t.ctx.Err(); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("translate %s: %w", sha1, err)
	}
	if newH, ok := t.mapping[sha1]; ok {
		return newH, nil
	}
	if _, busy := t.inProgress[sha1]; busy {
		// Real Git histories cannot form cycles via parent, tree, or
		// tag-target edges (those are a DAG by construction), and
		// SHA1 message-reference cycles are cryptographically
		// infeasible (each commit's hash depends on its content,
		// including any hash it embeds). A trip here would mean an
		// unexpected graph shape; surface it instead of overflowing
		// the stack.
		return plumbing.ZeroHash, fmt.Errorf("translation cycle detected at %s", sha1)
	}
	t.inProgress[sha1] = struct{}{}
	defer delete(t.inProgress, sha1)

	obj, err := t.src.EncodedObject(plumbing.AnyObject, sha1)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("lookup %s: %w", sha1, err)
	}
	switch obj.Type() { //nolint:exhaustive // OFSDelta/REFDelta/AnyObject/InvalidObject cannot reach a resolved storage.
	case plumbing.BlobObject:
		return t.translateBlob(sha1, obj)
	case plumbing.TreeObject:
		return t.translateTree(sha1, obj)
	case plumbing.CommitObject:
		return t.translateCommit(sha1, obj)
	case plumbing.TagObject:
		return t.translateTag(sha1, obj)
	default:
		return plumbing.ZeroHash, fmt.Errorf("unexpected object type %v for %s", obj.Type(), sha1)
	}
}

func (t *translator) translateBlob(sha1 plumbing.Hash, src plumbing.EncodedObject) (plumbing.Hash, error) {
	r, err := src.Reader()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("blob reader: %w", err)
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("blob read: %w", err)
	}
	newHash, err := t.storeBlob(body)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("blob store: %w", err)
	}
	t.mapping[sha1] = newHash
	t.blobs.Add(1)
	return newHash, nil
}

func (t *translator) translateTree(sha1 plumbing.Hash, src plumbing.EncodedObject) (plumbing.Hash, error) {
	tree := &object.Tree{}
	if err := tree.Decode(src); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("decode tree %s: %w", sha1, err)
	}
	for i, entry := range tree.Entries {
		if entry.Mode == filemode.Submodule {
			// Should not be reachable: discoverReachable refuses any
			// submodule gitlink up-front. Keep this as a defensive
			// guard so the rewrite path never silently produces a
			// SHA256 tree whose gitlink points at a hash the
			// .gitmodules upstream repo cannot resolve.
			return plumbing.ZeroHash, fmt.Errorf(
				"tree %s contains submodule gitlink %q at %s; convert-sha256 refuses to rewrite submodule pointers",
				sha1, entry.Name, entry.Hash)
		}
		newH, err := t.translate(entry.Hash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("tree %s entry %q: %w", sha1, entry.Name, err)
		}
		tree.Entries[i].Hash = newH
	}
	newHash, err := t.store(tree.Encode)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store tree %s: %w", sha1, err)
	}
	t.mapping[sha1] = newHash
	t.trees.Add(1)
	return newHash, nil
}

// stripSignatures clears a commit's or tag's signature fields, returning
// true if either was set. Signatures sign over the original SHA1 byte
// stream and cannot survive the rewrite. A transitional dual-hash object
// can carry both the SHA1-form "gpgsig" (Signature) and "gpgsig-sha256"
// (SignatureSHA256) for the same logical signature, so we clear both and
// the caller counts the artifact once.
func stripSignatures(sig, sigSHA256 *string) bool {
	if *sig == "" && *sigSHA256 == "" {
		return false
	}
	*sig = ""
	*sigSHA256 = ""
	return true
}

func (t *translator) translateCommit(sha1 plumbing.Hash, src plumbing.EncodedObject) (plumbing.Hash, error) {
	c := &object.Commit{}
	if err := c.Decode(src); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("decode commit %s: %w", sha1, err)
	}
	newTree, err := t.translate(c.TreeHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("commit %s tree: %w", sha1, err)
	}
	c.TreeHash = newTree
	for i, p := range c.ParentHashes {
		newP, err := t.translate(p)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("commit %s parent %s: %w", sha1, p, err)
		}
		c.ParentHashes[i] = newP
	}
	if t.rewriteMessages {
		rewritten, n, err := t.rewriteMessageRefs(c.Message, "commit", sha1)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if n > 0 {
			c.Message = rewritten
			t.messageRewrites += n
		}
	}
	if stripSignatures(&c.Signature, &c.SignatureSHA256) {
		t.signaturesStripped++
	}
	// "mergetag" extra headers embed a copy of a signed annotated tag with
	// its own signature. Drop them too — they reference the pre-rewrite
	// commit/tag content and cannot be re-signed here.
	if len(c.ExtraHeaders) > 0 {
		filtered := c.ExtraHeaders[:0]
		for _, h := range c.ExtraHeaders {
			if h.Key == "mergetag" {
				t.signaturesStripped++
				continue
			}
			filtered = append(filtered, h)
		}
		c.ExtraHeaders = filtered
	}
	newHash, err := t.store(c.Encode)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store commit %s: %w", sha1, err)
	}
	t.mapping[sha1] = newHash
	t.commits = append(t.commits, sha1)
	t.commitsCount.Add(1)
	return newHash, nil
}

func (t *translator) translateTag(sha1 plumbing.Hash, src plumbing.EncodedObject) (plumbing.Hash, error) {
	tag := &object.Tag{}
	if err := tag.Decode(src); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("decode tag %s: %w", sha1, err)
	}
	newTarget, err := t.translate(tag.Target)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("tag %s target: %w", sha1, err)
	}
	tag.Target = newTarget
	if t.rewriteMessages {
		rewritten, n, err := t.rewriteMessageRefs(tag.Message, "tag", sha1)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if n > 0 {
			tag.Message = rewritten
			t.messageRewrites += n
		}
	}
	if stripSignatures(&tag.Signature, &tag.SignatureSHA256) {
		t.signaturesStripped++
	}
	newHash, err := t.store(tag.Encode)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store tag %s: %w", sha1, err)
	}
	t.mapping[sha1] = newHash
	t.tags.Add(1)
	return newHash, nil
}

// store encodes an object into a fresh EncodedObject bound to the target
// store's SHA256 hasher and persists it as a loose object, returning the
// new SHA256 hash. Building the object via dst.NewEncodedObject is what
// guarantees the returned hash and the on-disk filename are both computed
// under SHA256: NewEncodedObject binds the object to the store's object
// format, and SetEncodedObject writes (and returns) it under that format.
//
// This path could not be used on go-git v6 alpha.3 — its objfile.Writer
// hardcoded SHA1, so SetEncodedObject placed every translated object at a
// SHA1-derived path even on a SHA256 store. alpha.4 derives the hash
// format from the store config (go-git commit 5cab3a7), so the manual
// loose-object writer this code used to carry is no longer needed.
//
// go-git's loose writer is atomic (tempfile + rename) and idempotent on
// duplicate hashes (it lstats the destination and drops the temp file if
// it already exists); duplicate source objects never reach here anyway,
// since translate() memoizes through t.mapping before encoding.
func (t *translator) store(encode func(plumbing.EncodedObject) error) (plumbing.Hash, error) {
	obj := t.dst.NewEncodedObject()
	if err := encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	h, err := t.dst.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("set encoded object: %w", err)
	}
	return h, nil
}

// storeBlob persists raw bytes as a blob. Blob content is never rewritten
// during conversion (blobs carry no hash references), so the bytes are
// copied verbatim.
func (t *translator) storeBlob(content []byte) (plumbing.Hash, error) {
	return t.store(func(o plumbing.EncodedObject) error {
		o.SetType(plumbing.BlobObject)
		o.SetSize(int64(len(content)))
		w, err := o.Writer()
		if err != nil {
			return fmt.Errorf("blob writer: %w", err)
		}
		if _, err := w.Write(content); err != nil {
			_ = w.Close()
			return fmt.Errorf("write blob content: %w", err)
		}
		if err := w.Close(); err != nil {
			return fmt.Errorf("close blob writer: %w", err)
		}
		return nil
	})
}

// hashPattern matches hex runs that could be a git object hash. Git's
// default abbreviation is 7 chars; 40 is a full SHA1. Case-insensitive
// so messages that paste an uppercase or mixed-case hash (e.g. from
// some commit graph viewers) still resolve — the lookup canonicalizes
// to lowercase before checking the reachable set. We only rewrite a
// match if the prefix uniquely identifies a commit or tag in the
// reachable set, so false positives on incidental hex strings are
// essentially impossible (a random hex would have to collide with a
// real source SHA1).
var hashPattern = regexp.MustCompile(`(?i)\b[0-9a-f]{7,40}\b`)

// matchResult is the 3-state outcome of resolving a hex prefix in a
// commit/tag message against the reachable set. We distinguish
// "ambiguous" from "no match" so the caller can warn the user about
// prefixes that *could* be rewritten if they were a couple of chars
// longer.
type matchResult int

const (
	matchNone matchResult = iota
	matchUnique
	matchAmbiguous
)

// rewriteMessageRefs resolves the SHA1 hash references in a commit or tag
// message and rewrites the unique in-scope ones to their full SHA256 hex,
// in a single regex pass over msg. It first translates every referenced
// object — adding it as an edge in the translation DFS so t.mapping holds
// the object by the time we substitute. That ordering is what lets a
// cross-branch reference (a cherry-pick, or a revert of a sibling branch)
// resolve regardless of which branch ref iteration processed first; it is
// the subtlest invariant in this file, which is why both translateCommit
// and translateTag funnel through here instead of copying it.
//
// kind ("commit"/"tag") and sha1 only frame a translate error. Returns the
// rewritten message and the number of substitutions made. Ambiguous
// prefixes are left in place and recorded in t.ambiguousMessageRefs so the
// caller can surface a warning at the end of the run.
//
// Uniqueness is decided against t.reachable rather than t.mapping so that
// abbreviated prefixes get the same verdict during translation as they
// would after every object has been translated — the answer cannot flip
// depending on what has been processed so far.
//
// Performance: the abbreviated-hash path scans the reachable set linearly
// for each distinct prefix (memoized by resolveCache). Fine for repos up
// to ~100k commits; slower past that. If this ever matters, build a
// sorted-prefix index over reachable SHA1 hex strings once and binary
// search.
func (t *translator) rewriteMessageRefs(msg, kind string, sha1 plumbing.Hash) (string, int, error) {
	spans := hashPattern.FindAllStringIndex(msg, -1)
	if len(spans) == 0 {
		return msg, 0, nil
	}
	// Resolve every match once (resolveMessageRef is memoized), recording
	// the unique in-scope refs to translate before any substitution.
	type resolvedMatch struct {
		lo, hi int
		hash   plumbing.Hash
		result matchResult
	}
	matches := make([]resolvedMatch, 0, len(spans))
	seen := make(map[plumbing.Hash]struct{})
	var refs []plumbing.Hash
	for _, s := range spans {
		hash, result := t.resolveMessageRef(msg[s[0]:s[1]])
		matches = append(matches, resolvedMatch{lo: s[0], hi: s[1], hash: hash, result: result})
		if result == matchUnique {
			if _, dup := seen[hash]; !dup {
				seen[hash] = struct{}{}
				refs = append(refs, hash)
			}
		}
	}
	for _, ref := range refs {
		if _, err := t.translate(ref); err != nil {
			return "", 0, fmt.Errorf("%s %s message ref %s: %w", kind, sha1, ref, err)
		}
	}
	// Rebuild msg, substituting each unique ref now present in the mapping.
	var b strings.Builder
	b.Grow(len(msg))
	prev, count := 0, 0
	for _, m := range matches {
		b.WriteString(msg[prev:m.lo])
		tok := msg[m.lo:m.hi]
		switch m.result {
		case matchUnique:
			if newHash, ok := t.mapping[m.hash]; ok {
				b.WriteString(newHash.String())
				count++
			} else {
				// reachable says this SHA1 is in scope but the DFS hasn't
				// placed it. Shouldn't happen — we translated every ref
				// above — so leave the original hex if it somehow does.
				b.WriteString(tok)
			}
		case matchAmbiguous:
			t.ambiguousMessageRefs[tok] = struct{}{}
			b.WriteString(tok)
		case matchNone:
			b.WriteString(tok)
		}
		prev = m.hi
	}
	b.WriteString(msg[prev:])
	return b.String(), count, nil
}

// resolveMessageRef classifies a hex prefix against the reachable set.
// Returns matchUnique with the resolved SHA1 when exactly one commit
// or tag in scope matches; matchAmbiguous when more than one does;
// matchNone otherwise (no match, or the match is a blob/tree — those
// are filtered so incidental hex collisions on content hashes aren't
// rewritten).
// resolveCacheEntry holds a memoized (Hash, matchResult) pair from
// resolveMessageRef. Stored in t.resolveCache keyed by lowercased prefix.
type resolveCacheEntry struct {
	hash   plumbing.Hash
	result matchResult
}

func (t *translator) resolveMessageRef(prefix string) (plumbing.Hash, matchResult) {
	// Canonicalize to lowercase: hashPattern is case-insensitive so
	// the caller can match `ABCD1234` in a message, but reachable
	// keys and plumbing.Hash.String() are always lowercase hex.
	prefix = strings.ToLower(prefix)
	if cached, ok := t.resolveCache[prefix]; ok {
		return cached.hash, cached.result
	}
	hash, result := t.resolveMessageRefUncached(prefix)
	t.resolveCache[prefix] = resolveCacheEntry{hash: hash, result: result}
	return hash, result
}

func (t *translator) resolveMessageRefUncached(prefix string) (plumbing.Hash, matchResult) {
	if len(prefix) == 40 {
		sha1, ok := plumbing.FromHex(prefix)
		if !ok {
			return plumbing.ZeroHash, matchNone
		}
		typ, in := t.reachable[sha1]
		if !in {
			return plumbing.ZeroHash, matchNone
		}
		if typ != plumbing.CommitObject && typ != plumbing.TagObject {
			return plumbing.ZeroHash, matchNone
		}
		return sha1, matchUnique
	}
	var match plumbing.Hash
	matches := 0
	for sha1, typ := range t.reachable {
		if typ != plumbing.CommitObject && typ != plumbing.TagObject {
			continue
		}
		if strings.HasPrefix(sha1.String(), prefix) {
			matches++
			if matches > 1 {
				return plumbing.ZeroHash, matchAmbiguous
			}
			match = sha1
		}
	}
	if matches == 1 {
		return match, matchUnique
	}
	return plumbing.ZeroHash, matchNone
}

// notesCommitTime returns the committer/author timestamp for the
// synthetic notes wrapper commit. Reads SOURCE_DATE_EPOCH (the
// reproducible-builds convention) when set, falling back to the Unix
// epoch so two runs over identical source state always produce the
// same notes-ref hash.
func notesCommitTime() time.Time {
	if raw := os.Getenv("SOURCE_DATE_EPOCH"); raw != "" {
		if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return time.Unix(secs, 0).UTC()
		}
	}
	return time.Unix(0, 0).UTC()
}

// writeOriginNotes writes a `git notes` ref to dst that records each
// translated commit's original SHA1, keyed by its new SHA256. Standard
// git tooling (`git log --notes=<ref>`, `git notes --ref=<ref> show
// <commit>`) can then surface the old hash to anyone with the repo.
//
// The notes tree is flat (no fanout). Git supports either layout, and a
// flat layout keeps this code small; on repos with millions of commits
// lookups slow down to a linear tree scan, but the data is preserved.
func (t *translator) writeOriginNotes(refName string) (string, error) {
	if len(t.commits) == 0 {
		return "", nil
	}
	// Note for each commit: a blob containing the original SHA1 hex + newline.
	// We collect (sha256-of-new-commit → blob hash) pairs so the tree entry
	// path is the commit's new hash.
	type entry struct {
		key  plumbing.Hash
		blob plumbing.Hash
	}
	entries := make([]entry, 0, len(t.commits))
	for _, oldSHA1 := range t.commits {
		newCommit, ok := t.mapping[oldSHA1]
		if !ok {
			continue
		}
		blobHash, err := t.storeBlob([]byte(oldSHA1.String() + "\n"))
		if err != nil {
			return "", fmt.Errorf("note blob for %s: %w", oldSHA1, err)
		}
		entries = append(entries, entry{key: newCommit, blob: blobHash})
	}
	if len(entries) == 0 {
		return "", nil
	}

	treeEntries := make([]object.TreeEntry, 0, len(entries))
	for _, e := range entries {
		treeEntries = append(treeEntries, object.TreeEntry{
			Name: e.key.String(),
			Mode: filemode.Regular,
			Hash: e.blob,
		})
	}
	sort.Slice(treeEntries, func(i, j int) bool {
		return treeEntries[i].Name < treeEntries[j].Name
	})
	tree := &object.Tree{Entries: treeEntries}
	treeHash, err := t.store(tree.Encode)
	if err != nil {
		return "", fmt.Errorf("store notes tree: %w", err)
	}

	// Honor SOURCE_DATE_EPOCH for reproducible builds; otherwise pin to
	// the Unix epoch so the notes-ref hash is identical across runs over
	// the same source state. The notes commit is bookkeeping — its
	// timestamp carries no meaningful information about when the
	// underlying SHA1 history was created.
	sig := object.Signature{Name: "git-sync", Email: "noreply@entire.io", When: notesCommitTime()}
	commit := &object.Commit{
		Author:    sig,
		Committer: sig,
		Message:   "git-sync convert-sha256: SHA1 origin notes\n",
		TreeHash:  treeHash,
	}
	commitHash, err := t.store(commit.Encode)
	if err != nil {
		return "", fmt.Errorf("store notes commit: %w", err)
	}
	t.lastNotesCommit = commitHash
	return refName, nil
}

// startProgressTick spawns a goroutine that, every 500 ms, rewrites a
// single line in place on out with the string returned by render. The
// returned stop function halts the goroutine and emits a trailing
// newline so subsequent prints start on a fresh row.
//
// Only intended for TTY output: the rendered line uses '\r\x1b[K' to
// overwrite itself, which looks fine on a terminal and ugly anywhere
// else. Callers gate on isTTY before calling.
func startProgressTick(out io.Writer, render func() string) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				fmt.Fprintf(out, "\r\x1b[K%s", render())
			}
		}
	}()
	stopOnce := false
	return func() {
		if stopOnce {
			return
		}
		stopOnce = true
		close(stop)
		<-done
		// Last frame + newline so subsequent output is on a clean row.
		fmt.Fprintf(out, "\r\x1b[K%s\n", render())
	}
}

// isTTY reports whether w is a writable terminal. The --progress
// ticker is suppressed on non-TTY destinations because the '\r'-style
// in-place updates would otherwise show up as literal control
// characters in log files and pipes.
func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// writeMappingFile dumps the SHA1 → SHA256 mapping as a TSV. Lines are
// sorted by SHA1 so diffs across runs are stable. Includes every
// translated object (blob/tree/commit/tag), so external tooling can use
// it for content-addressed lookups regardless of object kind.
func (t *translator) writeMappingFile(path string) error {
	type pair struct{ sha1, sha256 string }
	pairs := make([]pair, 0, len(t.mapping))
	for old, newH := range t.mapping {
		pairs = append(pairs, pair{sha1: old.String(), sha256: newH.String()})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].sha1 < pairs[j].sha1 })

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	// Close is best-effort on the failure path (the underlying issue
	// will already have surfaced via Flush). On the success path the
	// explicit Close below propagates its error — networked / quota'd
	// filesystems can defer write failures until close.
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()
	w := bufio.NewWriter(f)
	if _, err := fmt.Fprintln(w, "# sha1\tsha256"); err != nil {
		return fmt.Errorf("write mapping header: %w", err)
	}
	for _, p := range pairs {
		if _, err := fmt.Fprintf(w, "%s\t%s\n", p.sha1, p.sha256); err != nil {
			return fmt.Errorf("write mapping line: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush mapping file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close mapping file: %w", err)
	}
	closed = true
	return nil
}

// pickHEAD chooses which target-side ref the bare repo's HEAD should
// symlink to. It returns "" when no suitable branch exists (e.g. a
// tags-only conversion), in which case the caller leaves HEAD at the
// PlainInit default.
//
// Selection order:
//  1. The source's advertised HEAD, if it landed in the converted set.
//     Resolved via the desired entry's TargetRef so a user-supplied ref
//     mapping is honored.
//  2. refs/heads/main, then refs/heads/master, if either is present in
//     the converted target refs. Some HTTP v1 servers do not advertise
//     HEAD, so we pattern-match on conventional defaults.
//  3. The lexicographically first refs/heads/* in the target set, for
//     a deterministic fallback when neither convention is present.
func pickHEAD(advertised plumbing.ReferenceName, desired map[plumbing.ReferenceName]planner.DesiredRef) plumbing.ReferenceName {
	if advertised != "" {
		if d, ok := desired[advertised]; ok {
			return d.TargetRef
		}
	}
	branches := make(map[plumbing.ReferenceName]struct{}, len(desired))
	for _, d := range desired {
		if d.TargetRef.IsBranch() {
			branches[d.TargetRef] = struct{}{}
		}
	}
	for _, candidate := range []plumbing.ReferenceName{"refs/heads/main", "refs/heads/master"} {
		if _, ok := branches[candidate]; ok {
			return candidate
		}
	}
	if len(branches) == 0 {
		return ""
	}
	names := make([]string, 0, len(branches))
	for name := range branches {
		names = append(names, string(name))
	}
	sort.Strings(names)
	return plumbing.ReferenceName(names[0])
}

func writeRefs(
	dst storer.Storer,
	desired map[plumbing.ReferenceName]planner.DesiredRef,
	mapping map[plumbing.Hash]plumbing.Hash,
) (int, error) {
	written := 0
	for _, d := range desired {
		newHash, ok := mapping[d.SourceHash]
		if !ok {
			return written, fmt.Errorf("ref %s tip %s missing from translation map", d.TargetRef, d.SourceHash)
		}
		if err := dst.SetReference(plumbing.NewHashReference(d.TargetRef, newHash)); err != nil {
			return written, fmt.Errorf("set ref %s: %w", d.TargetRef, err)
		}
		written++
	}
	return written, nil
}
