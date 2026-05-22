// Package sha256convert implements a one-off SHA1 → SHA256 conversion for a
// single repository. It fetches a pack from a remote SHA1 HTTP endpoint into
// a temporary on-disk SHA1 bare repo, then walks every reachable object and
// re-emits it under SHA256 into a new bare repo at the user-supplied path.
//
// The tool is intentionally scoped: no hash mapping is persisted, GPG
// signatures on commits and tags are dropped (they sign over the original
// SHA1 byte stream and would be invalid post-rewrite), and submodule
// gitlinks are left at their original SHA1 hash unless the referenced
// commit happens to live in the same repo. A run that encounters an
// unresolvable submodule entry fails so the caller can choose which refs
// to exclude.
package sha256convert

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
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
type Request struct {
	SourceURL                    string
	SourceAuth                   gitsync.EndpointAuth
	SourceFollowInfoRefsRedirect bool
	TargetDir                    string

	Branches           []string
	IncludeTags        bool
	AllRefs            bool
	ExcludeRefPrefixes []string
	Mappings           []gitsync.RefMapping

	ProtocolMode      gitsync.ProtocolMode
	Verbose           bool
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
	SourceURL          string `json:"sourceUrl"`
	TargetDir          string `json:"targetDir"`
	Protocol           string `json:"protocol"`
	RefsConverted      int    `json:"refsConverted"`
	Counts             Counts `json:"counts"`
	SignaturesStripped int    `json:"signaturesStripped"`
	MessageRewrites    int    `json:"messageRewrites"`
	OriginNotesRef     string `json:"originNotesRef,omitempty"`
	MappingFile        string `json:"mappingFile,omitempty"`
	TempDir            string `json:"tempDir,omitempty"`
}

// Lines satisfies the human-readable output contract used by other git-sync subcommands.
func (r Result) Lines() []string {
	lines := []string{
		fmt.Sprintf("sha256 bare repo: %s", r.TargetDir),
		fmt.Sprintf("source: %s (%s)", r.SourceURL, r.Protocol),
		fmt.Sprintf("converted: %d blobs, %d trees, %d commits, %d tags",
			r.Counts.Blobs, r.Counts.Trees, r.Counts.Commits, r.Counts.Tags),
		fmt.Sprintf("refs written: %d", r.RefsConverted),
	}
	if r.SignaturesStripped > 0 {
		lines = append(lines, fmt.Sprintf("warning: stripped %d GPG signature(s); they no longer match the rewritten object content", r.SignaturesStripped))
	}
	if r.MessageRewrites > 0 {
		lines = append(lines, fmt.Sprintf("rewrote %d SHA1 hash reference(s) in commit/tag messages", r.MessageRewrites))
	}
	if r.OriginNotesRef != "" {
		lines = append(lines, fmt.Sprintf("origin notes ref: %s (use `git notes --ref=%s show <sha256>` to recover old SHA1)",
			r.OriginNotesRef, strings.TrimPrefix(r.OriginNotesRef, "refs/notes/")))
	}
	if r.MappingFile != "" {
		lines = append(lines, fmt.Sprintf("mapping written to: %s", r.MappingFile))
	}
	if r.TempDir != "" {
		lines = append(lines, fmt.Sprintf("kept source objects: %s", r.TempDir))
	}
	return lines
}

// Run performs the conversion described by req.
func Run(ctx context.Context, req Request) (Result, error) {
	if req.SourceURL == "" {
		return Result{}, errors.New("convert-sha256 requires --source-url")
	}
	if req.TargetDir == "" {
		return Result{}, errors.New("convert-sha256 requires a target directory")
	}
	out := req.Out
	if out == nil {
		out = os.Stderr
	}

	if err := ensureEmptyTarget(req.TargetDir); err != nil {
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

	srcRepo, err := git.PlainInit(tempDir, true)
	if err != nil {
		return Result{}, fmt.Errorf("init temporary SHA1 store: %w", err)
	}

	dstRepo, err := git.PlainInit(req.TargetDir, true, git.WithObjectFormat(formatcfg.SHA256))
	if err != nil {
		return Result{}, fmt.Errorf("init SHA256 target at %s: %w", req.TargetDir, err)
	}

	// Source connection + ref discovery -----------------------------------
	planCfg := planner.PlanConfig{
		Branches:           append([]string(nil), req.Branches...),
		Mappings:           toPlannerMappings(req.Mappings),
		IncludeTags:        req.IncludeTags,
		AllRefs:            req.AllRefs,
		ExcludeRefPrefixes: append([]string(nil), req.ExcludeRefPrefixes...),
	}
	conn, refService, sourceRefList, err := openSource(ctx, req, planCfg)
	if err != nil {
		return Result{}, err
	}
	defer conn.Close()
	refService.Verbose = req.Verbose

	sourceRefs := gitproto.RefHashMap(sourceRefList)
	desired, _, err := planner.BuildDesiredRefs(sourceRefs, planCfg)
	if err != nil {
		return Result{}, fmt.Errorf("build desired refs: %w", err)
	}
	if len(desired) == 0 {
		return Result{}, errors.New("no source refs matched the requested scope")
	}

	// Fetch into temp SHA1 store ------------------------------------------
	fmt.Fprintf(out, "fetching %d ref(s) from %s ...\n", len(desired), req.SourceURL)
	gpDesired := convert.DesiredRefs(desired)
	if err := refService.FetchToStore(ctx, srcRepo.Storer, conn, gpDesired, nil); err != nil &&
		!errors.Is(err, git.NoErrAlreadyUpToDate) {
		return Result{}, fmt.Errorf("fetch source pack: %w", err)
	}

	// Discover + translate ------------------------------------------------
	tr, err := newTranslator(srcRepo.Storer, dstRepo.Storer, req.TargetDir, !req.SkipMessageRewrite)
	if err != nil {
		return Result{}, err
	}
	rootSHA1s := make([]plumbing.Hash, 0, len(desired))
	for _, d := range desired {
		rootSHA1s = append(rootSHA1s, d.SourceHash)
	}
	fmt.Fprintln(out, "discovering reachable objects ...")
	if err := tr.discover(rootSHA1s); err != nil {
		return Result{}, fmt.Errorf("discover reachable: %w", err)
	}
	fmt.Fprintln(out, "translating objects to sha256 ...")
	for _, d := range desired {
		if _, err := tr.translate(d.SourceHash); err != nil {
			return Result{}, fmt.Errorf("translate %s: %w", d.SourceRef, err)
		}
	}

	// Write refs ---------------------------------------------------------
	refsWritten, err := writeRefs(dstRepo.Storer, desired, tr.mapping)
	if err != nil {
		return Result{}, fmt.Errorf("write target refs: %w", err)
	}

	// Point HEAD at the source's symbolic HEAD if it landed in the
	// converted ref set. PlainInit defaults HEAD to refs/heads/master,
	// which often doesn't exist (e.g. repos using "main" as the default).
	if refService.HeadTarget != "" {
		if _, ok := desired[refService.HeadTarget]; ok {
			head := plumbing.NewSymbolicReference(plumbing.HEAD, refService.HeadTarget)
			if err := dstRepo.Storer.SetReference(head); err != nil {
				return Result{}, fmt.Errorf("set HEAD: %w", err)
			}
		}
	}

	res := Result{
		SourceURL:          req.SourceURL,
		TargetDir:          req.TargetDir,
		Protocol:           refService.Protocol,
		RefsConverted:      refsWritten,
		Counts:             tr.counts,
		SignaturesStripped: tr.signaturesStripped,
		MessageRewrites:    tr.messageRewrites,
	}

	if !req.SkipOriginNotes && len(tr.commits) > 0 {
		notesRef, err := tr.writeOriginNotes(originNotesRef)
		if err != nil {
			return Result{}, fmt.Errorf("write origin notes: %w", err)
		}
		if err := dstRepo.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(notesRef), tr.lastNotesCommit)); err != nil {
			return Result{}, fmt.Errorf("set %s: %w", notesRef, err)
		}
		res.OriginNotesRef = notesRef
	}

	if req.MappingFile != "" {
		if err := tr.writeMappingFile(req.MappingFile); err != nil {
			return Result{}, fmt.Errorf("write mapping file: %w", err)
		}
		res.MappingFile = req.MappingFile
	}

	if req.KeepSourceObjects {
		cleanupTemp = false
		res.TempDir = tempDir
	}
	return res, nil
}

const originNotesRef = "refs/notes/sha1-origin"

// ensureEmptyTarget refuses to init into a non-empty directory so the user
// doesn't quietly accumulate objects into an existing repo.
func ensureEmptyTarget(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			if mkErr := os.MkdirAll(path, 0o755); mkErr != nil {
				return fmt.Errorf("create target dir: %w", mkErr)
			}
			return nil
		}
		return fmt.Errorf("read target dir: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("target directory %s is not empty", path)
	}
	return nil
}

func openSource(ctx context.Context, req Request, planCfg planner.PlanConfig) (gitproto.Conn, *gitproto.RefService, []*plumbing.Reference, error) {
	ep, err := url.Parse(req.SourceURL)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse source URL: %w", err)
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

func (a authAdapter) Authorizer(req *http.Request) error { return a.m.Authorizer(req) }

func toPlannerMappings(in []gitsync.RefMapping) []planner.RefMapping {
	out := make([]planner.RefMapping, 0, len(in))
	for _, m := range in {
		out = append(out, planner.RefMapping{Source: m.Source, Target: m.Target})
	}
	return out
}

// translator walks the SHA1 source store, rewrites object content with
// SHA256-mapped hashes, and writes the result as loose objects under the
// target bare repo. Loose object writing is done by hand because go-git
// v6 alpha 3's objfile.Writer hardcodes SHA1 in prepareForWrite (see
// plumbing/format/objfile/writer.go:68), which would store every SHA256
// object at a SHA1-derived path.
type translator struct {
	src        *filesystem.Storage
	dst        *filesystem.Storage
	objectsDir string
	// reachable holds every in-scope SHA1 with its object type, built up
	// front by a discovery pass that walks tree/commit/tag dependencies
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
	commits            []plumbing.Hash
	counts             Counts
	signaturesStripped int
	messageRewrites    int
	rewriteMessages    bool
	lastNotesCommit    plumbing.Hash
}

func newTranslator(src, dst storer.Storer, targetDir string, rewriteMessages bool) (*translator, error) {
	srcFS, ok := src.(*filesystem.Storage)
	if !ok {
		return nil, fmt.Errorf("source storage is not filesystem-backed (%T)", src)
	}
	dstFS, ok := dst.(*filesystem.Storage)
	if !ok {
		return nil, fmt.Errorf("target storage is not filesystem-backed (%T)", dst)
	}
	return &translator{
		src:             srcFS,
		dst:             dstFS,
		objectsDir:      filepath.Join(targetDir, "objects"),
		reachable:       make(map[plumbing.Hash]plumbing.ObjectType),
		mapping:         make(map[plumbing.Hash]plumbing.Hash),
		inProgress:      make(map[plumbing.Hash]struct{}),
		rewriteMessages: rewriteMessages,
	}, nil
}

// discover walks every object reachable from roots (via tree entries,
// commit tree+parent links, and tag targets) and records each one in
// t.reachable with its object type. Submodule gitlinks are followed
// only when the referenced commit exists in the same source store, to
// stay consistent with translateTree's handling. Message-reference
// edges are not part of this pass — those are added during translation.
func (t *translator) discover(roots []plumbing.Hash) error {
	for _, root := range roots {
		if err := t.visit(root); err != nil {
			return err
		}
	}
	return nil
}

func (t *translator) visit(sha1 plumbing.Hash) error {
	if _, seen := t.reachable[sha1]; seen {
		return nil
	}
	obj, err := t.src.EncodedObject(plumbing.AnyObject, sha1)
	if err != nil {
		return fmt.Errorf("discover %s: %w", sha1, err)
	}
	t.reachable[sha1] = obj.Type()
	switch obj.Type() {
	case plumbing.BlobObject:
		return nil
	case plumbing.TreeObject:
		tree := &object.Tree{}
		if err := tree.Decode(obj); err != nil {
			return fmt.Errorf("discover decode tree %s: %w", sha1, err)
		}
		for _, e := range tree.Entries {
			if e.Mode == filemode.Submodule {
				if _, err := t.src.EncodedObject(plumbing.CommitObject, e.Hash); err == nil {
					if err := t.visit(e.Hash); err != nil {
						return err
					}
				}
				continue
			}
			if err := t.visit(e.Hash); err != nil {
				return err
			}
		}
	case plumbing.CommitObject:
		c := &object.Commit{}
		if err := c.Decode(obj); err != nil {
			return fmt.Errorf("discover decode commit %s: %w", sha1, err)
		}
		if err := t.visit(c.TreeHash); err != nil {
			return err
		}
		for _, p := range c.ParentHashes {
			if err := t.visit(p); err != nil {
				return err
			}
		}
	case plumbing.TagObject:
		tag := &object.Tag{}
		if err := tag.Decode(obj); err != nil {
			return fmt.Errorf("discover decode tag %s: %w", sha1, err)
		}
		if err := t.visit(tag.Target); err != nil {
			return err
		}
	}
	return nil
}

func (t *translator) translate(sha1 plumbing.Hash) (plumbing.Hash, error) {
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
	switch obj.Type() {
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
	newHash, err := t.writeLoose(plumbing.BlobObject, body)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("blob store: %w", err)
	}
	t.mapping[sha1] = newHash
	t.counts.Blobs++
	return newHash, nil
}

func (t *translator) translateTree(sha1 plumbing.Hash, src plumbing.EncodedObject) (plumbing.Hash, error) {
	tree := &object.Tree{}
	if err := tree.Decode(src); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("decode tree %s: %w", sha1, err)
	}
	for i, entry := range tree.Entries {
		if entry.Mode == filemode.Submodule {
			// Submodule gitlinks reference a commit in a different repo.
			// We can only translate if that commit happens to live in our
			// SHA1 store too (rare, e.g. vendored). Otherwise the SHA1
			// pointer can't be embedded in a SHA256 tree, so we error
			// out and let the caller scope around it.
			if _, ok := t.mapping[entry.Hash]; ok {
				tree.Entries[i].Hash = t.mapping[entry.Hash]
				continue
			}
			if _, err := t.src.EncodedObject(plumbing.CommitObject, entry.Hash); err == nil {
				newH, err := t.translate(entry.Hash)
				if err != nil {
					return plumbing.ZeroHash, err
				}
				tree.Entries[i].Hash = newH
				continue
			}
			return plumbing.ZeroHash, fmt.Errorf(
				"tree %s contains submodule gitlink %q at %s that is not present in the source repo; "+
					"exclude refs that reference it or convert the submodule repository first",
				sha1, entry.Name, entry.Hash)
		}
		newH, err := t.translate(entry.Hash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("tree %s entry %q: %w", sha1, entry.Name, err)
		}
		tree.Entries[i].Hash = newH
	}
	body, err := encodeBody(plumbing.TreeObject, tree.Encode)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode tree %s: %w", sha1, err)
	}
	newHash, err := t.writeLoose(plumbing.TreeObject, body)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store tree %s: %w", sha1, err)
	}
	t.mapping[sha1] = newHash
	t.counts.Trees++
	return newHash, nil
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
		// Translate every in-scope SHA1 mentioned in this commit's
		// message before rewriting it. This makes the message-reference
		// edge part of the translation DFS, so the mapping contains
		// each referenced object by the time we substitute. Without
		// it, sibling-branch references (cherry-picks, etc.) would
		// only resolve when ref iteration happened to process the
		// referenced commit's branch first.
		for _, ref := range t.extractMessageReferences(c.Message) {
			if _, err := t.translate(ref); err != nil {
				return plumbing.ZeroHash, fmt.Errorf("commit %s message ref %s: %w", sha1, ref, err)
			}
		}
		if rewritten, n := t.rewriteHashesInMessage(c.Message); n > 0 {
			c.Message = rewritten
			t.messageRewrites += n
		}
	}
	if c.Signature != "" {
		c.Signature = ""
		t.signaturesStripped++
	}
	if c.SignatureSHA256 != "" {
		c.SignatureSHA256 = ""
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
	body, err := encodeBody(plumbing.CommitObject, c.Encode)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode commit %s: %w", sha1, err)
	}
	newHash, err := t.writeLoose(plumbing.CommitObject, body)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store commit %s: %w", sha1, err)
	}
	t.mapping[sha1] = newHash
	t.commits = append(t.commits, sha1)
	t.counts.Commits++
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
		// Same as translateCommit: translate every in-scope message
		// reference before rewriting, so cross-branch references
		// always resolve regardless of ref iteration order.
		for _, ref := range t.extractMessageReferences(tag.Message) {
			if _, err := t.translate(ref); err != nil {
				return plumbing.ZeroHash, fmt.Errorf("tag %s message ref %s: %w", sha1, ref, err)
			}
		}
		if rewritten, n := t.rewriteHashesInMessage(tag.Message); n > 0 {
			tag.Message = rewritten
			t.messageRewrites += n
		}
	}
	if tag.Signature != "" {
		tag.Signature = ""
		t.signaturesStripped++
	}
	if tag.SignatureSHA256 != "" {
		tag.SignatureSHA256 = ""
		t.signaturesStripped++
	}
	body, err := encodeBody(plumbing.TagObject, tag.Encode)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode tag %s: %w", sha1, err)
	}
	newHash, err := t.writeLoose(plumbing.TagObject, body)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store tag %s: %w", sha1, err)
	}
	t.mapping[sha1] = newHash
	t.counts.Tags++
	return newHash, nil
}

// encodeBody runs an object's go-git Encode method into a SHA1-hasher
// MemoryObject (the hasher we use to capture bytes is irrelevant; we only
// read the body back out) and returns just the payload bytes — without the
// "<type> <size>\x00" header. writeLoose adds the SHA256-correct header.
func encodeBody(typ plumbing.ObjectType, encode func(plumbing.EncodedObject) error) ([]byte, error) {
	scratch := plumbing.NewMemoryObject(plumbing.FromObjectFormat(formatcfg.SHA1))
	scratch.SetType(typ)
	if err := encode(scratch); err != nil {
		return nil, err
	}
	r, err := scratch.Reader()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// writeLoose writes a single object as a SHA256-named loose object under
// objects/<aa>/<rest>. Bypasses go-git's objfile.Writer, which would hash
// with SHA1. Atomic via tempfile+rename, idempotent on duplicate hashes.
func (t *translator) writeLoose(typ plumbing.ObjectType, body []byte) (plumbing.Hash, error) {
	h := sha256.New()
	header := append(typ.Bytes(), ' ')
	header = strconv.AppendInt(header, int64(len(body)), 10)
	header = append(header, 0)
	h.Write(header)
	h.Write(body)
	sum := h.Sum(nil)
	hexSum := hex.EncodeToString(sum)

	dir := filepath.Join(t.objectsDir, hexSum[:2])
	file := filepath.Join(dir, hexSum[2:])

	hashID, ok := plumbing.FromBytes(sum)
	if !ok {
		return plumbing.ZeroHash, fmt.Errorf("internal: bad sha256 sum length %d", len(sum))
	}

	if _, err := os.Stat(file); err == nil {
		return hashID, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(header); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("zlib write header: %w", err)
	}
	if _, err := zw.Write(body); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("zlib write body: %w", err)
	}
	if err := zw.Close(); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("zlib close: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "tmp_obj_")
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("create temp object: %w", err)
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return plumbing.ZeroHash, fmt.Errorf("write temp object: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return plumbing.ZeroHash, fmt.Errorf("close temp object: %w", err)
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		_ = os.Remove(tmp.Name())
		return plumbing.ZeroHash, fmt.Errorf("rename %s: %w", file, err)
	}
	return hashID, nil
}

// hashPattern matches hex runs that could be a git object hash. Git's
// default abbreviation is 7 chars; 40 is a full SHA1. We only rewrite a
// match if the prefix uniquely identifies a commit or tag in the
// reachable set, so false positives on incidental hex strings are
// essentially impossible (a random hex would have to collide with a
// real source SHA1).
var hashPattern = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)

// rewriteHashesInMessage scans msg for short and full SHA1 hashes,
// replacing any that uniquely identify a commit or tag in t.reachable
// with the corresponding full SHA256 hex from t.mapping. Returns the
// rewritten message and the number of substitutions made.
//
// Uniqueness is decided against t.reachable rather than t.mapping so
// that abbreviated prefixes get the same verdict during translation as
// they would after every object has been translated — the answer cannot
// flip depending on what has been processed so far.
//
// Performance: the abbreviated-hash path scans the reachable set
// linearly for each match. Fine for repos up to ~100k commits; slower
// past that. If this ever matters, build a sorted-prefix index over
// reachable SHA1 hex strings once and binary-search.
func (t *translator) rewriteHashesInMessage(msg string) (string, int) {
	count := 0
	out := hashPattern.ReplaceAllStringFunc(msg, func(s string) string {
		sha1, ok := t.resolveMessageRef(s)
		if !ok {
			return s
		}
		newHash, ok := t.mapping[sha1]
		if !ok {
			// The reachable set says this SHA1 is in scope, but the
			// translation DFS hasn't placed it yet. Shouldn't happen
			// because translateCommit/translateTag add message-reference
			// edges before encoding — leave the hex untouched if it
			// somehow does.
			return s
		}
		count++
		return newHash.String()
	})
	return out, count
}

// resolveMessageRef returns the unique commit/tag SHA1 in t.reachable
// that matches the given hex prefix. Returns (zero, false) for no
// match, an ambiguous prefix, or a match that is not a commit or tag
// (incidental hex strings that happen to collide with a blob or tree
// hash are not rewritten).
func (t *translator) resolveMessageRef(prefix string) (plumbing.Hash, bool) {
	if len(prefix) == 40 {
		sha1, ok := plumbing.FromHex(prefix)
		if !ok {
			return plumbing.ZeroHash, false
		}
		typ, in := t.reachable[sha1]
		if !in {
			return plumbing.ZeroHash, false
		}
		if typ != plumbing.CommitObject && typ != plumbing.TagObject {
			return plumbing.ZeroHash, false
		}
		return sha1, true
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
				return plumbing.ZeroHash, false
			}
			match = sha1
		}
	}
	if matches != 1 {
		return plumbing.ZeroHash, false
	}
	return match, true
}

// extractMessageReferences returns the unique commit/tag SHA1s mentioned
// by hex prefix in msg. Used by translateCommit/translateTag to add
// message-reference edges to the translation DFS so the mapping is
// fully populated by the time the message is rewritten.
func (t *translator) extractMessageReferences(msg string) []plumbing.Hash {
	seen := make(map[plumbing.Hash]struct{})
	var out []plumbing.Hash
	for _, match := range hashPattern.FindAllString(msg, -1) {
		sha1, ok := t.resolveMessageRef(match)
		if !ok {
			continue
		}
		if _, dup := seen[sha1]; dup {
			continue
		}
		seen[sha1] = struct{}{}
		out = append(out, sha1)
	}
	return out
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
		key   plumbing.Hash
		blob  plumbing.Hash
	}
	entries := make([]entry, 0, len(t.commits))
	for _, oldSHA1 := range t.commits {
		newCommit, ok := t.mapping[oldSHA1]
		if !ok {
			continue
		}
		blobHash, err := t.writeLoose(plumbing.BlobObject, []byte(oldSHA1.String()+"\n"))
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
	treeBody, err := encodeBody(plumbing.TreeObject, tree.Encode)
	if err != nil {
		return "", fmt.Errorf("encode notes tree: %w", err)
	}
	treeHash, err := t.writeLoose(plumbing.TreeObject, treeBody)
	if err != nil {
		return "", fmt.Errorf("store notes tree: %w", err)
	}

	now := time.Now().UTC()
	sig := object.Signature{Name: "git-sync", Email: "noreply@entire.io", When: now}
	commit := &object.Commit{
		Author:    sig,
		Committer: sig,
		Message:   "git-sync convert-sha256: SHA1 origin notes\n",
		TreeHash:  treeHash,
	}
	commitBody, err := encodeBody(plumbing.CommitObject, commit.Encode)
	if err != nil {
		return "", fmt.Errorf("encode notes commit: %w", err)
	}
	commitHash, err := t.writeLoose(plumbing.CommitObject, commitBody)
	if err != nil {
		return "", fmt.Errorf("store notes commit: %w", err)
	}
	t.lastNotesCommit = commitHash
	return refName, nil
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
	defer f.Close()
	w := bufio.NewWriter(f)
	if _, err := fmt.Fprintln(w, "# sha1\tsha256"); err != nil {
		return err
	}
	for _, p := range pairs {
		if _, err := fmt.Fprintf(w, "%s\t%s\n", p.sha1, p.sha256); err != nil {
			return err
		}
	}
	return w.Flush()
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

