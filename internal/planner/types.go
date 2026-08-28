package planner

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"entire.io/entire/git-sync/internal/validation"
	"github.com/go-git/go-git/v6/plumbing"
)

// RefKind distinguishes ref namespaces: branch, tag, or other (notes/pulls/custom).
type RefKind string

const (
	RefKindBranch RefKind = "branch"
	RefKindTag    RefKind = "tag"
	RefKindOther  RefKind = "other"
)

// Action represents the planned operation on a ref.
type Action string

const (
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionDelete Action = "delete"
	ActionSkip   Action = "skip"
	ActionBlock  Action = "block"
	// ActionWarn is set when the target rejected an individual ref under
	// best-effort policy; the server's reason is carried in BranchPlan.Reason.
	ActionWarn Action = "warn"
)

// RefMapping is a user-specified source:target mapping.
type RefMapping = validation.RefMapping

// DesiredRef represents a source ref that should be mirrored to a target ref.
type DesiredRef struct {
	Kind       RefKind
	Label      string
	SourceRef  plumbing.ReferenceName
	TargetRef  plumbing.ReferenceName
	SourceHash plumbing.Hash
}

// ManagedTarget tracks which target refs are managed by git-sync.
type ManagedTarget struct {
	Kind  RefKind
	Label string
}

// BranchPlan describes the planned action for a single ref.
type BranchPlan struct {
	Branch     string                 `json:"branch"`
	SourceRef  plumbing.ReferenceName `json:"sourceRef"`
	TargetRef  plumbing.ReferenceName `json:"targetRef"`
	SourceHash plumbing.Hash          `json:"sourceHash"`
	TargetHash plumbing.Hash          `json:"targetHash"`
	Kind       RefKind                `json:"kind"`
	Action     Action                 `json:"action"`
	Reason     string                 `json:"reason"`
}

func (p BranchPlan) MarshalJSON() ([]byte, error) {
	type bp struct {
		Branch     string  `json:"branch"`
		SourceRef  string  `json:"sourceRef"`
		TargetRef  string  `json:"targetRef"`
		SourceHash string  `json:"sourceHash"`
		TargetHash string  `json:"targetHash"`
		Kind       RefKind `json:"kind"`
		Action     Action  `json:"action"`
		Reason     string  `json:"reason"`
	}
	data, err := json.Marshal(bp{
		Branch:     p.Branch,
		SourceRef:  p.SourceRef.String(),
		TargetRef:  p.TargetRef.String(),
		SourceHash: p.SourceHash.String(),
		TargetHash: p.TargetHash.String(),
		Kind:       p.Kind,
		Action:     p.Action,
		Reason:     p.Reason,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal branch plan: %w", err)
	}
	return data, nil
}

// ShortHash returns the first 8 characters of a hash, or "<zero>" for zero hashes.
func ShortHash(hash plumbing.Hash) string {
	if hash.IsZero() {
		return "<zero>"
	}
	s := hash.String()
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// IsRefExcluded reports whether name matches any of the exclude prefixes.
// Empty prefixes are ignored. Used to subtract specific namespaces from
// auto-discovery (e.g. refs/pull/* under --all-refs against GitHub).
func IsRefExcluded(name plumbing.ReferenceName, excludePrefixes, excludeExact []string) bool {
	if len(excludePrefixes) == 0 && len(excludeExact) == 0 {
		return false
	}
	s := name.String()
	for _, p := range excludePrefixes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	// Exact names match the whole ref, so a caller can reserve
	// refs/heads/entire without also excluding refs/heads/entire/foo — which
	// a prefix cannot express (it would also catch refs/heads/entirely).
	for _, e := range excludeExact {
		if e = strings.TrimSpace(e); e != "" && s == e {
			return true
		}
	}
	return false
}

// RefKindFromName infers the ref kind from a fully qualified ref name.
// Returns RefKindOther for any refs/* outside refs/heads/ and refs/tags/,
// and "" for names that don't start with refs/ at all.
func RefKindFromName(name plumbing.ReferenceName) RefKind {
	switch {
	case name.IsBranch():
		return RefKindBranch
	case name.IsTag():
		return RefKindTag
	case strings.HasPrefix(name.String(), "refs/"):
		return RefKindOther
	default:
		return ""
	}
}

// ActionForTargetHash returns ActionCreate for zero hashes, ActionUpdate otherwise.
func ActionForTargetHash(hash plumbing.Hash) Action {
	if hash.IsZero() {
		return ActionCreate
	}
	return ActionUpdate
}

// BranchMapFromRefHashMap extracts short branch names from a ref hash map.
func BranchMapFromRefHashMap(refs map[plumbing.ReferenceName]plumbing.Hash) map[string]plumbing.Hash {
	branches := make(map[string]plumbing.Hash)
	for name, hash := range refs {
		if name.IsBranch() {
			branches[name.Short()] = hash
		}
	}
	return branches
}

// SelectBranches filters branches to only the requested ones.
// If requested is empty, returns all.
func SelectBranches(source map[string]plumbing.Hash, requested []string) map[string]plumbing.Hash {
	if len(requested) == 0 {
		return source
	}
	selected := make(map[string]plumbing.Hash, len(requested))
	for _, branch := range requested {
		if hash, ok := source[branch]; ok {
			selected[branch] = hash
		}
	}
	return selected
}

// RefPrefixes computes the ref-prefix arguments for v2 ls-refs from cfg.
// Under AllRefs the result collapses to a single "refs/" prefix.
func RefPrefixes(cfg PlanConfig) []string {
	if cfg.AllRefs {
		return []string{"refs/"}
	}
	prefixSet := map[string]struct{}{}
	if len(cfg.Mappings) > 0 {
		for _, m := range cfg.Mappings {
			src := strings.TrimSpace(m.Source)
			switch {
			case strings.HasPrefix(src, "refs/tags/"):
				prefixSet["refs/tags/"] = struct{}{}
			case strings.HasPrefix(src, "refs/heads/"):
				prefixSet["refs/heads/"] = struct{}{}
			case !strings.HasPrefix(src, "refs/"):
				prefixSet["refs/heads/"] = struct{}{}
			}
		}
	} else {
		prefixSet["refs/heads/"] = struct{}{}
	}
	if cfg.IncludeTags {
		prefixSet["refs/tags/"] = struct{}{}
	}
	prefixes := make([]string, 0, len(prefixSet))
	for p := range prefixSet {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	return prefixes
}

// CopyRefHashMap returns a shallow copy of a ref hash map.
func CopyRefHashMap(input map[plumbing.ReferenceName]plumbing.Hash) map[plumbing.ReferenceName]plumbing.Hash {
	out := make(map[plumbing.ReferenceName]plumbing.Hash, len(input))
	for k, v := range input {
		out[k] = v
	}
	return out
}

// gitsyncRefPrefix is git-sync's own scaffolding namespace on a TARGET.
// Source-side discovery never mirrors it (see BuildDesiredRefs): a source
// carrying refs/gitsync/* has leftover scaffolding from being someone else's
// target, and replicating it would collide with — and under overwrite
// semantics, clobber — this target's own resume state. The namespace is NOT
// excluded from target-side prune scope, though: prune is the only cleaner a
// stale marker has (isLiveBootstrapMarker carves out the live ones).
const gitsyncRefPrefix = "refs/gitsync/"

// bootstrapTempRefPrefix is the namespace holding batched-bootstrap temp refs.
// git-sync is its only writer: the bootstrap strategy creates a marker per
// branch and deletes it when the branch is finalized (or when the marker is
// stale), so a surviving ref here always means a bootstrap that did not finish.
const bootstrapTempRefPrefix = gitsyncRefPrefix + "bootstrap/heads/"

// isGitSyncScaffoldingRef reports whether name lies anywhere in git-sync's
// own scaffolding namespace.
func isGitSyncScaffoldingRef(name plumbing.ReferenceName) bool {
	return strings.HasPrefix(name.String(), gitsyncRefPrefix)
}

// BootstrapTempRef returns the temporary ref name used during batched bootstrap.
func BootstrapTempRef(targetRef plumbing.ReferenceName) plumbing.ReferenceName {
	return plumbing.ReferenceName(bootstrapTempRefPrefix + targetRef.Short())
}

// BootstrapTempRefTarget maps a batched-bootstrap temp ref back to the branch
// it checkpoints — the inverse of BootstrapTempRef. ok is false for names
// outside the temp-ref namespace.
func BootstrapTempRefTarget(name plumbing.ReferenceName) (target plumbing.ReferenceName, ok bool) {
	rest, found := strings.CutPrefix(name.String(), bootstrapTempRefPrefix)
	if !found || rest == "" {
		return "", false
	}
	return plumbing.NewBranchReferenceName(rest), true
}

// FormatPlanLine formats a single plan entry for human-readable output.
func FormatPlanLine(plan BranchPlan) string {
	label := plan.Branch
	if plan.TargetRef != "" {
		label = plan.TargetRef.String()
	}
	line := fmt.Sprintf("%s %s", strings.ToUpper(string(plan.Action)), label)
	if plan.Reason != "" {
		line += " - " + plan.Reason
	}
	return line
}
