package planner

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"entire.io/entire/gitsync/internal/validation"
	"github.com/go-git/go-git/v6/plumbing"
)

// RefKind distinguishes branch refs from tag refs.
type RefKind string

const (
	RefKindBranch RefKind = "branch"
	RefKindTag    RefKind = "tag"
)

// Action represents the planned operation on a ref.
type Action string

const (
	ActionCreate Action = "create"
	ActionUpdate Action = "update"
	ActionDelete Action = "delete"
	ActionSkip   Action = "skip"
	ActionBlock  Action = "block"
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

// RefKindFromName infers the ref kind from a fully qualified ref name.
func RefKindFromName(name plumbing.ReferenceName) RefKind {
	switch {
	case name.IsBranch():
		return RefKindBranch
	case name.IsTag():
		return RefKindTag
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

// RefPrefixes computes the ref-prefix arguments for v2 ls-refs based on
// the user's configuration.
func RefPrefixes(mappings []RefMapping, includeTags bool) []string {
	prefixSet := map[string]struct{}{}
	if len(mappings) > 0 {
		for _, m := range mappings {
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
	if includeTags {
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

// DesiredSubset returns the subset of desired refs that match the given plans.
func DesiredSubset(
	desired map[plumbing.ReferenceName]DesiredRef,
	plans []BranchPlan,
) map[plumbing.ReferenceName]DesiredRef {
	out := make(map[plumbing.ReferenceName]DesiredRef, len(plans))
	for _, plan := range plans {
		if ref, ok := desired[plan.TargetRef]; ok {
			out[plan.TargetRef] = ref
		}
	}
	return out
}

// SingleDesired builds a single-entry desired ref map.
func SingleDesired(sourceRef, targetRef plumbing.ReferenceName, hash plumbing.Hash) map[plumbing.ReferenceName]DesiredRef {
	return map[plumbing.ReferenceName]DesiredRef{
		targetRef: {
			Kind:       RefKindBranch,
			Label:      targetRef.Short(),
			SourceRef:  sourceRef,
			TargetRef:  targetRef,
			SourceHash: hash,
		},
	}
}

// SingleHaveMap builds a single-entry have map for fetch negotiation.
func SingleHaveMap(hash plumbing.Hash) map[plumbing.ReferenceName]plumbing.Hash {
	if hash.IsZero() {
		return nil
	}
	return map[plumbing.ReferenceName]plumbing.Hash{
		plumbing.ReferenceName("refs/gitsync/have"): hash,
	}
}

// BootstrapTempRef returns the temporary ref name used during batched bootstrap.
func BootstrapTempRef(targetRef plumbing.ReferenceName) plumbing.ReferenceName {
	return plumbing.ReferenceName("refs/gitsync/bootstrap/heads/" + targetRef.Short())
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
