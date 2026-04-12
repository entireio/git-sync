package planner

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
)

// NormalizeMapping validates and normalizes a single ref mapping.
// It rejects branch-to-tag and tag-to-branch cross-kind mappings (issue #3).
func NormalizeMapping(m RefMapping) (plumbing.ReferenceName, plumbing.ReferenceName, RefKind, error) {
	src := strings.TrimSpace(m.Source)
	dst := strings.TrimSpace(m.Target)
	if src == "" || dst == "" {
		return "", "", "", fmt.Errorf("invalid mapping %q:%q: source and target must be non-empty", m.Source, m.Target)
	}

	srcFQ := strings.HasPrefix(src, "refs/")
	dstFQ := strings.HasPrefix(dst, "refs/")

	// Both fully qualified
	if srcFQ && dstFQ {
		sourceRef := plumbing.ReferenceName(src)
		targetRef := plumbing.ReferenceName(dst)
		srcKind := RefKindFromName(sourceRef)
		dstKind := RefKindFromName(targetRef)
		if srcKind == "" {
			return "", "", "", fmt.Errorf("unsupported source ref kind: %s", src)
		}
		if dstKind == "" {
			return "", "", "", fmt.Errorf("unsupported target ref kind: %s", dst)
		}
		if srcKind != dstKind {
			return "", "", "", fmt.Errorf("cross-kind mapping not allowed: %s (%s) -> %s (%s)", src, srcKind, dst, dstKind)
		}
		return sourceRef, targetRef, dstKind, nil
	}

	// Both short names -> branch mapping
	if !srcFQ && !dstFQ {
		return plumbing.NewBranchReferenceName(src), plumbing.NewBranchReferenceName(dst), RefKindBranch, nil
	}

	// Mixed: one FQ and one short -> reject as ambiguous (issue #3)
	return "", "", "", fmt.Errorf("ambiguous mapping: cannot mix fully-qualified and short ref names: %q -> %q", src, dst)
}

// ValidateMappings normalizes all mappings and checks for duplicate targets (issue #2).
// Validation happens before any network activity.
func ValidateMappings(mappings []RefMapping) ([]NormalizedMapping, error) {
	if len(mappings) == 0 {
		return nil, nil
	}

	normalized := make([]NormalizedMapping, 0, len(mappings))
	targetSeen := make(map[plumbing.ReferenceName]string, len(mappings))

	for _, m := range mappings {
		srcRef, dstRef, kind, err := NormalizeMapping(m)
		if err != nil {
			return nil, err
		}

		// Check for duplicate target refs (issue #2)
		if prev, exists := targetSeen[dstRef]; exists {
			return nil, fmt.Errorf("duplicate target ref %s: mapped from both %q and %q", dstRef, prev, m.Source)
		}
		targetSeen[dstRef] = m.Source

		normalized = append(normalized, NormalizedMapping{
			SourceRef: srcRef,
			TargetRef: dstRef,
			Kind:      kind,
		})
	}
	return normalized, nil
}

// NormalizedMapping is a validated and normalized ref mapping.
type NormalizedMapping struct {
	SourceRef plumbing.ReferenceName
	TargetRef plumbing.ReferenceName
	Kind      RefKind
}
