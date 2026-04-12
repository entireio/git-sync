package validation

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
)

const (
	ProtocolAuto = "auto"
	ProtocolV1   = "v1"
	ProtocolV2   = "v2"
)

// RefMapping is a user-specified source:target mapping.
type RefMapping struct {
	Source string
	Target string
}

// NormalizedMapping is a validated mapping normalized to fully-qualified refs.
type NormalizedMapping struct {
	SourceRef plumbing.ReferenceName
	TargetRef plumbing.ReferenceName
}

// NormalizeProtocolMode validates the configured protocol mode and applies the
// default auto mode when the user did not specify one.
func NormalizeProtocolMode(mode string) (string, error) {
	if mode == "" {
		return ProtocolAuto, nil
	}
	switch mode {
	case ProtocolAuto, ProtocolV1, ProtocolV2:
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported protocol mode %q", mode)
	}
}

// ParseMapping parses a CLI --map value into a ref mapping.
func ParseMapping(raw string) (RefMapping, error) {
	parts := strings.SplitN(raw, ":", 2)
	if len(parts) != 2 {
		return RefMapping{}, fmt.Errorf("invalid --map %q, expected src:dst", raw)
	}
	source := strings.TrimSpace(parts[0])
	target := strings.TrimSpace(parts[1])
	if source == "" || target == "" {
		return RefMapping{}, fmt.Errorf("invalid --map %q, expected src:dst", raw)
	}
	return RefMapping{Source: source, Target: target}, nil
}

// ParseHaveRef normalizes a have-ref CLI value. Short names are treated as
// branch names for compatibility with other CLI ref selectors.
func ParseHaveRef(raw string) plumbing.ReferenceName {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "refs/") {
		return plumbing.ReferenceName(raw)
	}
	return plumbing.NewBranchReferenceName(raw)
}

// NormalizeMapping validates and normalizes a single ref mapping.
func NormalizeMapping(m RefMapping) (NormalizedMapping, error) {
	src := strings.TrimSpace(m.Source)
	dst := strings.TrimSpace(m.Target)
	if src == "" || dst == "" {
		return NormalizedMapping{}, fmt.Errorf("invalid mapping %q:%q: source and target must be non-empty", m.Source, m.Target)
	}

	srcFQ := strings.HasPrefix(src, "refs/")
	dstFQ := strings.HasPrefix(dst, "refs/")

	if srcFQ && dstFQ {
		sourceRef := plumbing.ReferenceName(src)
		targetRef := plumbing.ReferenceName(dst)
		srcKind := refKind(sourceRef)
		dstKind := refKind(targetRef)
		if srcKind == "" {
			return NormalizedMapping{}, fmt.Errorf("unsupported source ref kind: %s", src)
		}
		if dstKind == "" {
			return NormalizedMapping{}, fmt.Errorf("unsupported target ref kind: %s", dst)
		}
		if srcKind != dstKind {
			return NormalizedMapping{}, fmt.Errorf("cross-kind mapping not allowed: %s (%s) -> %s (%s)", src, srcKind, dst, dstKind)
		}
		return NormalizedMapping{SourceRef: sourceRef, TargetRef: targetRef}, nil
	}

	if !srcFQ && !dstFQ {
		return NormalizedMapping{
			SourceRef: plumbing.NewBranchReferenceName(src),
			TargetRef: plumbing.NewBranchReferenceName(dst),
		}, nil
	}

	return NormalizedMapping{}, fmt.Errorf("ambiguous mapping: cannot mix fully-qualified and short ref names: %q -> %q", src, dst)
}

// ValidateMappings normalizes all mappings and rejects duplicate target refs.
func ValidateMappings(mappings []RefMapping) ([]NormalizedMapping, error) {
	if len(mappings) == 0 {
		return nil, nil
	}

	normalized := make([]NormalizedMapping, 0, len(mappings))
	targetSeen := make(map[plumbing.ReferenceName]string, len(mappings))

	for _, m := range mappings {
		nm, err := NormalizeMapping(m)
		if err != nil {
			return nil, err
		}
		if prev, exists := targetSeen[nm.TargetRef]; exists {
			return nil, fmt.Errorf("duplicate target ref %s: mapped from both %q and %q", nm.TargetRef, prev, m.Source)
		}
		targetSeen[nm.TargetRef] = m.Source
		normalized = append(normalized, nm)
	}
	return normalized, nil
}

func refKind(name plumbing.ReferenceName) string {
	switch {
	case name.IsBranch():
		return "branch"
	case name.IsTag():
		return "tag"
	default:
		return ""
	}
}
