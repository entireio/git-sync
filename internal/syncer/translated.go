package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/storage/memory"

	"entire.io/entire/git-sync/internal/convert"
	"entire.io/entire/git-sync/internal/gitproto"
	"entire.io/entire/git-sync/internal/planner"
	"entire.io/entire/git-sync/internal/translate"
)

const (
	// sourceOriginNotesRef is where convert-sha256 records, for each commit
	// it converted, the SHA1 commit it came from.
	sourceOriginNotesRef = plumbing.ReferenceName("refs/notes/sha1-origin")
	// targetOriginNotesRef is where a translated sync records, for each
	// commit it wrote to the SHA1 target, the SHA256 commit it came from.
	targetOriginNotesRef = plumbing.ReferenceName("refs/notes/sha256-origin")
	// convertedTagPrefix holds convert-sha256's attestation tags. They sign
	// the conversion itself and mean nothing on the SHA1 side.
	convertedTagPrefix = "refs/tags/converted/"
)

// translating reports whether objects must be translated rather than
// relayed: the source is SHA256 and the target SHA1. Every other pair of
// formats is left to checkObjectFormats.
func (s *syncSession) translating() bool {
	if s.target == nil {
		return false
	}
	return s.sourceService.ObjectFormat() == formatcfg.SHA256 &&
		effectiveObjectFormat(gitproto.AdvRefsObjectFormat(s.target.adv)) == formatcfg.SHA1
}

// effectiveObjectFormat maps a format that was not advertised to SHA1, the
// protocol's default.
func effectiveObjectFormat(f formatcfg.ObjectFormat) formatcfg.ObjectFormat {
	if f == formatcfg.UnsetObjectFormat {
		return formatcfg.SHA1
	}
	return f
}

// errTranslationMismatch wraps why an operation cannot translate a SHA256
// source into a SHA1 target, naming both formats as checkObjectFormats does.
func errTranslationMismatch(reason error) error {
	return fmt.Errorf("%w: source is %s, target is %s: %w",
		gitproto.ErrObjectFormatMismatch, formatcfg.SHA256, formatcfg.SHA1, reason)
}

// validateTranslation rejects what a translated sync does not support.
func validateTranslation(cfg Config) error {
	switch {
	case cfg.Mode == modeReplicate:
		return errors.New("replicate cannot translate objects; use sync")
	case cfg.ForceAny():
		return errors.New("force flags are not supported when translating objects")
	case cfg.Prune:
		return errors.New("--prune is not supported when translating objects")
	case cfg.AllRefs:
		return errors.New("--all-refs is not supported when translating objects; sync branches and tags")
	case cfg.BestEffort:
		// A rejected ref would be reported as a warning, and the result
		// never mentions a rejected refs/notes/sha256-origin at all.
		return errors.New("best-effort is not supported when translating objects")
	}
	return nil
}

// checkTranslationFlags rejects the translation-only options this run cannot
// honour.
func (s *syncSession) checkTranslationFlags() error {
	targetAtomic := s.target != nil && s.target.adv.Capabilities.Supports(capability.Atomic)
	return validateTranslationFlags(s.cfg, s.translating(), targetAtomic)
}

// validateTranslationFlags rejects --atomic and --retranslate-tags outside a
// translated sync, and --atomic against a target that does not advertise
// atomic.
func validateTranslationFlags(cfg Config, translating, targetAtomic bool) error {
	const only = " is only supported when syncing a sha256 source into a sha1 target"
	switch {
	case cfg.Atomic && !translating:
		return errors.New("--atomic" + only)
	case cfg.RetranslateTags && !translating:
		return errors.New("--retranslate-tags" + only)
	case cfg.Atomic && !targetAtomic:
		return fmt.Errorf("--atomic: %w", gitproto.ErrAtomicPushUnsupported)
	}
	return nil
}

// runTranslatedSync syncs a SHA256 source into a SHA1 target.
//
// It fetches the full closure of the desired refs, translates it into SHA1
// objects and pushes what the target lacks. Translation stops at commits
// whose SHA1 counterpart is already recorded: refs/notes/sha1-origin on the
// source covers history from before the conversion, and
// refs/notes/sha256-origin on the target covers commits earlier syncs wrote.
// The recorded hashes win over translating again: convert-sha256 before
// this release dropped signatures and rewrote messages, so translating a
// commit it converted does not reproduce the original SHA1 commit. The push
// replaces refs/notes/sha256-origin with notes that add every translated
// commit the target will hold.
//
// Tags are immutable: a tag the target already has is left alone whatever it
// points at. RetranslateTags still translates it, to note its commits when
// the target holds its translation.
func (s *syncSession) runTranslatedSync(ctx context.Context) (Result, error) {
	if err := validateTranslation(s.cfg); err != nil {
		return Result{}, errTranslationMismatch(err)
	}

	desiredRefs, _, err := planner.BuildDesiredRefs(s.sourceRefMap, planConfig(s.cfg))
	if err != nil {
		return Result{}, fmt.Errorf("build desired refs: %w", err)
	}
	dropConvertedTags(desiredRefs)
	if len(desiredRefs) == 0 {
		return Result{}, errors.New("no source refs matched")
	}

	sourceStore := newBoundedStorer(memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA256)), effectiveMaxObjects(s.cfg))
	fetch := convert.DesiredRefs(desiredRefs)
	// A tag the target already has is left alone, so its history is not
	// needed unless RetranslateTags translates it.
	maps.DeleteFunc(fetch, func(name plumbing.ReferenceName, _ gitproto.DesiredRef) bool {
		want := desiredRefs[name]
		return s.skipsExistingTag(want)
	})
	originNotes, err := s.sourceRefHash(ctx, sourceOriginNotesRef)
	if err != nil {
		return Result{}, err
	}
	if !originNotes.IsZero() {
		fetch[sourceOriginNotesRef] = gitproto.DesiredRef{
			SourceRef: sourceOriginNotesRef, TargetRef: sourceOriginNotesRef, SourceHash: originNotes,
		}
	}
	if err := s.sourceService.FetchToStore(ctx, sourceStore, s.sourceConn, fetch, nil); err != nil &&
		!errors.Is(err, git.NoErrAlreadyUpToDate) {
		return Result{}, fmt.Errorf("fetch source objects: %w", err)
	}

	seed := make(map[plumbing.Hash]plumbing.Hash)
	if !originNotes.IsZero() {
		converted, _, err := translate.ReadHashNotes(sourceStore, originNotes)
		if err != nil {
			return Result{}, fmt.Errorf("read %s: %w", sourceOriginNotesRef, err)
		}
		maps.Copy(seed, converted)
	}
	targetNotes := s.target.refMap[targetOriginNotesRef]
	var targetNoteEntries []translate.NoteEntry
	var fromTargetNotes map[plumbing.Hash]struct{}
	if !targetNotes.IsZero() {
		synced, entries, err := s.readTargetNotes(ctx, targetNotes)
		if err != nil {
			return Result{}, err
		}
		fromTargetNotes = addTargetNoteSeeds(seed, synced)
		targetNoteEntries = entries
	}

	// A non-atomic push can land the notes without the commits they record,
	// which the target then drops. Such a seed is dropped too, and the
	// commit translated again.
	var (
		refs       translatedRefs
		result     Result
		pushPlans  []BranchPlan
		newCommits []translate.Pair
	)
	for {
		refs, err = s.translateRefs(sourceStore, desiredRefs, seed)
		if err != nil {
			return Result{}, err
		}
		result = Result{
			Plans: refs.plans, DryRun: s.cfg.DryRun, OperationMode: modeSync, Protocol: s.sourceService.Protocol,
			RelayReason: "source objects are sha256 and target objects are sha1; translated before push",
			SourceHEAD:  s.sourceService.HeadTarget,
		}
		pushPlans = classifyPlans(refs.plans, s.cfg.DryRun, &result)
		if !s.cfg.DryRun && result.Blocked > 0 {
			return result, fmt.Errorf("blocked %d ref update(s): the target has commits that are not translations of the source's", result.Blocked)
		}
		newCommits, err = landedCommits(sourceStore, refs.tr.Commits(), refs.plans, refs.translated, seed)
		if err != nil {
			return result, err
		}
		if !changesRefs(refs.plans) && len(newCommits) == 0 {
			break
		}
		lost, err := s.checkTargetHas(ctx, refs.tr.SeedsUsed(), fromTargetNotes)
		if err != nil {
			return result, err
		}
		if len(lost) == 0 {
			break
		}
		for _, h := range lost {
			delete(seed, h)
			delete(fromTargetNotes, h)
		}
	}

	if !s.cfg.DryRun && (len(pushPlans) > 0 || len(newCommits) > 0) {
		onTarget := make(map[plumbing.Hash]struct{}, len(seed))
		for sha256 := range seed {
			onTarget[sha256] = struct{}{}
		}
		for _, h := range s.target.refMap {
			if sha256, ok := refs.reverse[h]; ok {
				onTarget[sha256] = struct{}{}
			}
		}
		if err := s.pushTranslated(ctx, sourceStore, refs.out, refs.tr, pushPlans, refs.translated, onTarget, targetNotes, targetNoteEntries, newCommits); err != nil {
			return result, err
		}
	}

	s.finalizeCounts(pushPlans, &result)
	result.Stats = s.stats.snapshot()
	result.Measurement = s.measurementDone()
	return result, nil
}

// dropConvertedTags removes the refs under refs/tags/converted/, the
// attestation tags convert-sha256 signs, whether a mapping names one as the
// source or the target.
func dropConvertedTags(desired map[plumbing.ReferenceName]planner.DesiredRef) {
	maps.DeleteFunc(desired, func(name plumbing.ReferenceName, ref planner.DesiredRef) bool {
		return strings.HasPrefix(name.String(), convertedTagPrefix) ||
			strings.HasPrefix(ref.SourceRef.String(), convertedTagPrefix)
	})
}

// translatedRefs is the translation of the desired refs from one seed.
type translatedRefs struct {
	tr  *translate.Translator
	out storer.Storer
	// translated maps each desired ref to its translation. An existing tag
	// is left out unless RetranslateTags is set.
	translated map[plumbing.ReferenceName]plumbing.Hash
	// reverse maps each SHA1 commit the seed or the translation produced to
	// its SHA256 source.
	reverse map[plumbing.Hash]plumbing.Hash
	plans   []BranchPlan
}

func (s *syncSession) translateRefs(sourceStore storer.EncodedObjectStorer, desiredRefs map[plumbing.ReferenceName]planner.DesiredRef, seed map[plumbing.Hash]plumbing.Hash) (translatedRefs, error) {
	out := memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA1))
	tr := translate.New(sourceStore, out, formatcfg.SHA256, formatcfg.SHA1, seed)
	translated := make(map[plumbing.ReferenceName]plumbing.Hash, len(desiredRefs))
	for _, name := range sortedRefNames(desiredRefs) {
		want := desiredRefs[name]
		if s.skipsExistingTag(want) {
			continue
		}
		h, err := tr.Translate(want.SourceHash)
		if err != nil {
			return translatedRefs{}, fmt.Errorf("translate %s: %w", want.SourceRef, err)
		}
		translated[name] = h
	}

	reverse := make(map[plumbing.Hash]plumbing.Hash, len(seed))
	for sha256, sha1 := range seed {
		reverse[sha1] = sha256
	}
	for _, p := range tr.Commits() {
		reverse[p.Target] = p.Source
	}

	plans := make([]BranchPlan, 0, len(desiredRefs))
	for _, name := range sortedRefNames(desiredRefs) {
		want := desiredRefs[name]
		plans = append(plans, planTranslatedRef(sourceStore, want, s.target.refMap[want.TargetRef], translated[name], reverse))
	}
	return translatedRefs{tr: tr, out: out, translated: translated, reverse: reverse, plans: plans}, nil
}

// skipsExistingTag reports whether want is a tag the target already has,
// which is left alone without RetranslateTags.
func (s *syncSession) skipsExistingTag(want planner.DesiredRef) bool {
	return want.Kind == planner.RefKindTag && !s.target.refMap[want.TargetRef].IsZero() && !s.cfg.RetranslateTags
}

// changesRefs reports whether any plan would change a ref on the target.
func changesRefs(plans []BranchPlan) bool {
	return slices.ContainsFunc(plans, func(p BranchPlan) bool {
		return p.Action == ActionCreate || p.Action == ActionUpdate || p.Action == ActionDelete
	})
}

// landedCommits keeps the commits in translated whose SHA1 counterparts the
// target will hold once the push lands: those reachable from a ref it
// creates or updates, or from a ref whose translation it already has. Only
// those get origin notes. A retranslated tag that differs from the target's
// leaves out the commits only it reaches. seed holds the commits whose
// counterparts are already recorded, where the walk stops.
func landedCommits(
	sourceStore storer.EncodedObjectStorer,
	commits []translate.Pair,
	plans []BranchPlan,
	translated map[plumbing.ReferenceName]plumbing.Hash,
	seed map[plumbing.Hash]plumbing.Hash,
) ([]translate.Pair, error) {
	var roots []plumbing.Hash
	for _, p := range plans {
		out := translated[p.TargetRef]
		switch {
		case p.Action == planner.ActionCreate, p.Action == planner.ActionUpdate:
			roots = append(roots, p.SourceHash)
		case !out.IsZero() && out == p.TargetHash:
			roots = append(roots, p.SourceHash)
		}
	}
	reachable, err := reachableCommits(sourceStore, formatcfg.SHA256, roots, seed)
	if err != nil {
		return nil, fmt.Errorf("find commits the push lands: %w", err)
	}
	landed := make([]translate.Pair, 0, len(commits))
	for _, c := range commits {
		if _, ok := reachable[c.Source]; ok {
			landed = append(landed, c)
		}
	}
	return landed, nil
}

// reachableCommits returns the commits reachable from roots, peeling tags,
// without walking past the commits in stop.
func reachableCommits(store storer.EncodedObjectStorer, format formatcfg.ObjectFormat, roots []plumbing.Hash, stop map[plumbing.Hash]plumbing.Hash) (map[plumbing.Hash]struct{}, error) {
	out := make(map[plumbing.Hash]struct{})
	seen := make(map[plumbing.Hash]struct{})
	stack := slices.Clone(roots)
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		if _, ok := stop[h]; ok {
			continue
		}
		obj, err := store.EncodedObject(plumbing.AnyObject, h)
		if err != nil {
			return nil, fmt.Errorf("load object %s: %w", h, err)
		}
		switch obj.Type() {
		case plumbing.CommitObject:
			c, err := object.DecodeCommit(store, obj)
			if err != nil {
				return nil, fmt.Errorf("decode commit %s: %w", h, err)
			}
			out[h] = struct{}{}
			stack = append(stack, c.ParentHashes...)
		case plumbing.TagObject:
			raw, err := readEncoded(obj)
			if err != nil {
				return nil, fmt.Errorf("read tag %s: %w", h, err)
			}
			target, err := translate.TagObject(raw, format)
			if err != nil {
				return nil, fmt.Errorf("tag %s: %w", h, err)
			}
			stack = append(stack, target)
		case plumbing.TreeObject, plumbing.BlobObject:
			// A tag of a tree or blob reaches no commit.
		case plumbing.InvalidObject, plumbing.OFSDeltaObject, plumbing.REFDeltaObject, plumbing.AnyObject:
			return nil, fmt.Errorf("object %s has unexpected type %s", h, obj.Type())
		}
	}
	return out, nil
}

// planTranslatedRef plans one ref. sourceStore holds the source objects,
// targetHash is the target's current value, translated is the source tip
// translated into the target's format (zero for a tag left alone), and
// reverse maps target commits to the source commits they translate.
func planTranslatedRef(
	sourceStore storer.EncodedObjectStorer,
	want planner.DesiredRef,
	targetHash, translated plumbing.Hash,
	reverse map[plumbing.Hash]plumbing.Hash,
) BranchPlan {
	plan := BranchPlan{
		Branch: want.Label, SourceRef: want.SourceRef, TargetRef: want.TargetRef,
		SourceHash: want.SourceHash, TargetHash: targetHash, Kind: want.Kind,
	}
	switch {
	case want.Kind == planner.RefKindTag && !targetHash.IsZero():
		plan.Action = planner.ActionSkip
		plan.Reason = "tag already exists on target"
		if !translated.IsZero() && translated != targetHash {
			plan.Reason += " as " + planner.ShortHash(targetHash) + ", not its translation " + planner.ShortHash(translated)
		}
		return plan
	case targetHash.IsZero():
		plan.Action = planner.ActionCreate
		plan.Reason = "create as " + planner.ShortHash(translated)
		return plan
	case translated == targetHash:
		plan.Action = planner.ActionSkip
		plan.Reason = planner.ShortHash(targetHash) + " already current"
		return plan
	}

	sourceOfTarget, ok := reverse[targetHash]
	if !ok {
		plan.Action = planner.ActionBlock
		plan.Reason = planner.ShortHash(targetHash) + " is not a translation of any source commit; was the target written to directly?"
		return plan
	}
	ancestry, err := planner.CheckAncestry(sourceStore, want.SourceHash, sourceOfTarget)
	switch {
	case err != nil:
		plan.Action = planner.ActionBlock
		plan.Reason = "cannot verify fast-forward: " + err.Error()
	case ancestry == planner.AncestryReachable:
		plan.Action = planner.ActionUpdate
		plan.Reason = planner.ShortHash(targetHash) + " -> " + planner.ShortHash(translated)
	default:
		plan.Action = planner.ActionBlock
		plan.Reason = planner.ShortHash(targetHash) + " (source " + planner.ShortHash(sourceOfTarget) + ") is not an ancestor of " + planner.ShortHash(want.SourceHash)
	}
	return plan
}

// pushTranslated pushes the translated objects for pushPlans and records
// newCommits in the target's notes. onTarget holds the source commits whose
// translations the target already has.
func (s *syncSession) pushTranslated(
	ctx context.Context,
	sourceStore storer.EncodedObjectStorer,
	outStore storer.Storer,
	tr *translate.Translator,
	pushPlans []BranchPlan,
	translated map[plumbing.ReferenceName]plumbing.Hash,
	onTarget map[plumbing.Hash]struct{},
	targetNotes plumbing.Hash,
	targetNoteEntries []translate.NoteEntry,
	newCommits []translate.Pair,
) error {
	wants := make([]plumbing.Hash, 0, len(pushPlans))
	cmds := make([]gitproto.PushCommand, 0, len(pushPlans)+1)
	for _, p := range pushPlans {
		wants = append(wants, p.SourceHash)
		cmds = append(cmds, gitproto.PushCommand{Name: p.TargetRef, Old: p.TargetHash, New: translated[p.TargetRef]})
	}
	treeOnTarget := func(c plumbing.Hash) (bool, error) {
		return roundTrips(sourceStore, formatcfg.SHA256, formatcfg.SHA1, tr, c)
	}
	sourceObjects, err := objectsMissingFromTarget(sourceStore, formatcfg.SHA256, wants, onTarget, treeOnTarget)
	if err != nil {
		return fmt.Errorf("compute objects to push: %w", err)
	}
	hashes := make([]plumbing.Hash, 0, len(sourceObjects)+3)
	for _, h := range sourceObjects {
		out, ok := tr.Lookup(h)
		if !ok {
			return fmt.Errorf("object %s to push was never translated", h)
		}
		hashes = append(hashes, out)
	}

	if len(newCommits) > 0 {
		sig := object.Signature{Name: "git-sync", Email: "noreply@entire.io", When: time.Now().UTC()}
		notes, written, err := translate.WriteHashNotes(outStore, targetNoteEntries, newCommits, sig,
			"git-sync: SHA256 origin of translated commits\n")
		if err != nil {
			return fmt.Errorf("write %s: %w", targetOriginNotesRef, err)
		}
		hashes = append(hashes, written...)
		cmds = append(cmds, gitproto.PushCommand{Name: targetOriginNotesRef, Old: targetNotes, New: notes})
	}

	// With Atomic, the refs and the notes recording their origin land
	// together or not at all. Without it, the target may take a ref without
	// its notes. The next run then translates those commits again, gets the
	// same hashes and writes the notes it missed.
	push := s.target.pusher.PushObjects
	if s.cfg.Atomic {
		push = s.target.pusher.PushObjectsAtomic
	}
	if err := push(ctx, cmds, outStore, hashes); err != nil {
		return fmt.Errorf("push translated objects: %w", err)
	}
	return nil
}

// objectsMissingFromTarget lists the objects reachable from wants that the
// target lacks, given onTarget, the source commits whose translations the
// target holds. The walk stops at those commits, and leaves out the objects
// in their trees, which is what a commit built on them shares with them.
//
// treeOnTarget reports whether the target holds the translation of such a
// commit's tree. When it does not, only the commit's blobs are left out.
// Blobs translate unchanged, while the tree the target holds may differ
// from the translation of the source's.
func objectsMissingFromTarget(
	store storer.EncodedObjectStorer,
	format formatcfg.ObjectFormat,
	wants []plumbing.Hash,
	onTarget map[plumbing.Hash]struct{},
	treeOnTarget func(commit plumbing.Hash) (bool, error),
) ([]plumbing.Hash, error) {
	var boundary []plumbing.Hash
	seen := make(map[plumbing.Hash]struct{})
	var reachable []plumbing.Hash
	stack := slices.Clone(wants)
	for len(stack) > 0 {
		h := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		if _, ok := onTarget[h]; ok {
			boundary = append(boundary, h)
			continue
		}
		children, err := objectChildren(store, format, h)
		if err != nil {
			return nil, err
		}
		reachable = append(reachable, h)
		stack = append(stack, children...)
	}

	shared := make(map[plumbing.Hash]struct{})
	blobsOnly := make(map[plumbing.Hash]struct{})
	for _, c := range boundary {
		commit, err := object.GetCommit(store, c)
		if err != nil {
			if errors.Is(err, plumbing.ErrObjectNotFound) {
				continue
			}
			return nil, fmt.Errorf("load commit %s: %w", c, err)
		}
		known, err := treeOnTarget(c)
		if err != nil {
			return nil, err
		}
		into := shared
		if !known {
			into = blobsOnly
		}
		if err := collectTree(store, commit.TreeHash, into); err != nil {
			return nil, err
		}
	}
	for h := range blobsOnly {
		if _, ok := shared[h]; ok {
			continue
		}
		obj, err := store.EncodedObject(plumbing.AnyObject, h)
		if err != nil {
			return nil, fmt.Errorf("load object %s: %w", h, err)
		}
		if obj.Type() == plumbing.BlobObject {
			shared[h] = struct{}{}
		}
	}
	out := reachable[:0]
	for _, h := range reachable {
		if _, ok := shared[h]; !ok {
			out = append(out, h)
		}
	}
	return out, nil
}

// roundTrips reports whether source commit c converts back to the
// translation tr holds for it, a seeded one included. A commit that does
// embeds the translation of its tree, so a target holding the commit holds
// that tree. One converted by a convert-sha256 release that re-encoded
// trees, or with --rewrite-messages, may not. A commit that cannot be
// converted, such as one with an unknown gitlink, counts as not round-tripping.
func roundTrips(store storer.EncodedObjectStorer, from, to formatcfg.ObjectFormat, tr *translate.Translator, c plumbing.Hash) (bool, error) {
	want, ok := tr.Lookup(c)
	if !ok {
		return false, nil
	}
	obj, err := store.EncodedObject(plumbing.CommitObject, c)
	if err != nil {
		return false, fmt.Errorf("load commit %s: %w", c, err)
	}
	raw, err := readEncoded(obj)
	if err != nil {
		return false, fmt.Errorf("read commit %s: %w", c, err)
	}
	// Lookup first, so the parents' seeds are not reported as used.
	content, err := translate.ConvertCommit(raw, from, to, func(oid plumbing.Hash, gitlink bool) (plumbing.Hash, error) {
		if out, ok := tr.Lookup(oid); ok {
			return out, nil
		}
		if gitlink {
			return plumbing.ZeroHash, translate.ErrGitlink
		}
		return tr.Translate(oid)
	})
	if err != nil {
		return false, nil //nolint:nilerr // an object that does not convert does not round-trip
	}
	hasher := plumbing.NewHasher(to, plumbing.CommitObject, int64(len(content)))
	if _, err := hasher.Write(content); err != nil {
		return false, fmt.Errorf("hash commit %s: %w", c, err)
	}
	return hasher.Sum() == want, nil
}

func objectChildren(store storer.EncodedObjectStorer, format formatcfg.ObjectFormat, h plumbing.Hash) ([]plumbing.Hash, error) {
	obj, err := store.EncodedObject(plumbing.AnyObject, h)
	if err != nil {
		return nil, fmt.Errorf("load object %s: %w", h, err)
	}
	switch obj.Type() {
	case plumbing.CommitObject:
		c, err := object.DecodeCommit(store, obj)
		if err != nil {
			return nil, fmt.Errorf("decode commit %s: %w", h, err)
		}
		return append([]plumbing.Hash{c.TreeHash}, c.ParentHashes...), nil
	case plumbing.TreeObject:
		t, err := object.DecodeTree(store, obj)
		if err != nil {
			return nil, fmt.Errorf("decode tree %s: %w", h, err)
		}
		children := make([]plumbing.Hash, 0, len(t.Entries))
		for _, e := range t.Entries {
			children = append(children, e.Hash)
		}
		return children, nil
	case plumbing.TagObject:
		raw, err := readEncoded(obj)
		if err != nil {
			return nil, fmt.Errorf("read tag %s: %w", h, err)
		}
		target, err := translate.TagObject(raw, format)
		if err != nil {
			return nil, fmt.Errorf("tag %s: %w", h, err)
		}
		return []plumbing.Hash{target}, nil
	case plumbing.BlobObject:
		return nil, nil
	case plumbing.InvalidObject, plumbing.OFSDeltaObject, plumbing.REFDeltaObject, plumbing.AnyObject:
		return nil, fmt.Errorf("object %s has unexpected type %s", h, obj.Type())
	}
	return nil, fmt.Errorf("object %s has unknown type %s", h, obj.Type())
}

func readEncoded(obj plumbing.EncodedObject) ([]byte, error) {
	r, err := obj.Reader()
	if err != nil {
		return nil, fmt.Errorf("open reader: %w", err)
	}
	raw, err := io.ReadAll(r)
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return raw, nil
}

func collectTree(store storer.EncodedObjectStorer, h plumbing.Hash, into map[plumbing.Hash]struct{}) error {
	if _, ok := into[h]; ok {
		return nil
	}
	into[h] = struct{}{}
	tree, err := object.GetTree(store, h)
	if err != nil {
		return fmt.Errorf("load tree %s: %w", h, err)
	}
	for _, e := range tree.Entries {
		if e.Mode.IsFile() {
			into[e.Hash] = struct{}{}
			continue
		}
		if err := collectTree(store, e.Hash, into); err != nil {
			return err
		}
	}
	return nil
}

// sourceRefHash returns the source's value for name, or zero when it has
// none. A v2 listing is filtered to the requested scope, so a ref outside it
// takes another ls-refs.
func (s *syncSession) sourceRefHash(ctx context.Context, name plumbing.ReferenceName) (plumbing.Hash, error) {
	if h, ok := s.sourceRefMap[name]; ok || s.sourceService.Protocol != "v2" {
		return h, nil
	}
	refs, _, err := gitproto.ListSourceRefs(ctx, s.sourceConn, "v2", []string{name.String()})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("list source %s: %w", name, err)
	}
	return gitproto.RefHashMap(refs)[name], nil
}

// addTargetNoteSeeds adds to seed the SHA256 to SHA1 pairs the target's
// notes record, mapped SHA1 to SHA256 in synced. The source's notes record
// the original SHA1 commits, so a commit they map keeps that mapping. The
// returned seeds are those only the target's notes record. They name
// commits an earlier sync translated, which translating again reproduces.
func addTargetNoteSeeds(seed, synced map[plumbing.Hash]plumbing.Hash) map[plumbing.Hash]struct{} {
	fromTarget := make(map[plumbing.Hash]struct{})
	for sha1, sha256 := range synced {
		if _, converted := seed[sha256]; converted {
			continue
		}
		seed[sha256] = sha1
		fromTarget[sha256] = struct{}{}
	}
	return fromTarget
}

// readTargetNotes fetches the target's refs/notes/sha256-origin and reads it.
// Each sync replaces the notes with one parentless commit, so the fetch
// holds the current notes alone. Notes written by an earlier release still
// carry their history, which the next sync that adds notes drops.
func (s *syncSession) readTargetNotes(ctx context.Context, tip plumbing.Hash) (map[plumbing.Hash]plumbing.Hash, []translate.NoteEntry, error) {
	_, service, err := gitproto.ListSourceRefs(ctx, s.target.conn, s.cfg.ProtocolMode, []string{targetOriginNotesRef.String()})
	if err != nil {
		return nil, nil, fmt.Errorf("list target %s: %w", targetOriginNotesRef, err)
	}
	store := newBoundedStorer(memory.NewStorage(memory.WithObjectFormat(formatcfg.SHA1)), effectiveMaxObjects(s.cfg))
	desired := map[plumbing.ReferenceName]gitproto.DesiredRef{
		targetOriginNotesRef: {SourceRef: targetOriginNotesRef, TargetRef: targetOriginNotesRef, SourceHash: tip},
	}
	if err := service.FetchToStore(ctx, store, s.target.conn, desired, nil); err != nil {
		return nil, nil, fmt.Errorf("fetch target %s: %w", targetOriginNotesRef, err)
	}
	values, entries, err := translate.ReadHashNotes(store, tip)
	if err != nil {
		return nil, nil, fmt.Errorf("read target %s: %w", targetOriginNotesRef, err)
	}
	return values, entries, nil
}

// checkTargetHas fails unless the target has every seeded commit the
// translated objects build on. The source's notes name the SHA1 commits
// convert-sha256 started from, which only the original repository holds.
// Pushing to another repository would fail on missing objects after the
// whole pack was sent.
//
// A missing commit that only the target's notes record, listed in
// fromTargetNotes, does not fail the check. Its SHA256 source is returned
// instead, to be translated again.
//
// It negotiates with the target's upload-pack as a fetch would, offering
// those commits as haves, and upload-pack acknowledges the ones it holds.
func (s *syncSession) checkTargetHas(ctx context.Context, seeds []translate.Pair, fromTargetNotes map[plumbing.Hash]struct{}) ([]plumbing.Hash, error) {
	tips := make(map[plumbing.Hash]struct{}, len(s.target.refMap))
	for _, h := range s.target.refMap {
		tips[h] = struct{}{}
	}
	sourceOf := make(map[plumbing.Hash]plumbing.Hash)
	for _, p := range seeds {
		if _, ok := tips[p.Target]; !ok {
			sourceOf[p.Target] = p.Source
		}
	}
	if len(sourceOf) == 0 {
		return nil, nil
	}
	// Any advertised ref gives the negotiation something to want. The notes
	// ref counts too: a non-atomic push can land it while refusing every
	// branch and tag.
	prefixes := []string{"refs/heads/", "refs/tags/", targetOriginNotesRef.String()}
	refs, service, err := gitproto.ListSourceRefs(ctx, s.target.conn, s.cfg.ProtocolMode, prefixes)
	if err != nil {
		return nil, fmt.Errorf("list target refs: %w", err)
	}
	advertised := make([]plumbing.Hash, 0, len(refs))
	for _, r := range refs {
		advertised = append(advertised, r.Hash())
	}
	commits := slices.SortedFunc(maps.Keys(sourceOf), func(a, b plumbing.Hash) int { return a.Compare(b.Bytes()) })
	held, err := service.HasCommits(ctx, s.target.conn, advertised, commits)
	if err != nil {
		return nil, fmt.Errorf("check the target has the %d SHA1 commit(s) the translated history builds on: %w", len(commits), err)
	}
	var missing, lost []plumbing.Hash
	for _, h := range commits {
		if _, ok := held[h]; ok {
			continue
		}
		if _, ok := fromTargetNotes[sourceOf[h]]; ok {
			lost = append(lost, sourceOf[h])
			continue
		}
		missing = append(missing, h)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the target does not have %d SHA1 commit(s) the translated history builds on, such as %s (SHA256 %s). "+
			"Sync to the repository the source was converted from", len(missing), missing[0], sourceOf[missing[0]])
	}
	return lost, nil
}

func sortedRefNames(refs map[plumbing.ReferenceName]planner.DesiredRef) []plumbing.ReferenceName {
	names := make([]plumbing.ReferenceName, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
