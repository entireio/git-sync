package validation

import (
	"fmt"
	"net"
	"net/url"
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

// ValidateEndpoints rejects configurations where the source and target URLs
// point at the same repository. Empty URLs are ignored so the caller can
// surface a more specific "missing URL" error.
func ValidateEndpoints(sourceURL, targetURL string) error {
	src := normalizeEndpointURL(sourceURL)
	dst := normalizeEndpointURL(targetURL)
	if src == "" || dst == "" {
		return nil
	}
	if src == dst {
		return fmt.Errorf("source and target must not be the same repository: %s", src)
	}
	return nil
}

func normalizeEndpointURL(raw string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return ""
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return trimmed
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return trimmed
	}

	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if port != "" && !isDefaultPort(scheme, port) {
		host = net.JoinHostPort(host, port)
	}

	path := strings.TrimRight(parsed.EscapedPath(), "/")
	if path == "" {
		path = "/"
	}

	normalized := scheme + "://" + host + path
	if parsed.RawQuery != "" {
		normalized += "?" + parsed.RawQuery
	}
	if parsed.Fragment != "" {
		normalized += "#" + parsed.Fragment
	}
	return normalized
}

func isDefaultPort(scheme, port string) bool {
	return (scheme == "http" && port == "80") || (scheme == "https" && port == "443")
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

// NormalizeMapping validates a single mapping. allowOther accepts
// refs/* outside refs/heads/ and refs/tags/.
func NormalizeMapping(m RefMapping, allowOther bool) (NormalizedMapping, error) {
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
		if err := validateRefName("source", sourceRef); err != nil {
			return NormalizedMapping{}, err
		}
		if err := validateRefName("target", targetRef); err != nil {
			return NormalizedMapping{}, err
		}
		srcKind := refKind(sourceRef)
		dstKind := refKind(targetRef)
		if !allowOther && srcKind == kindOther {
			return NormalizedMapping{}, fmt.Errorf("unsupported source ref kind: %s (set --all-refs to allow arbitrary refs/* namespaces)", src)
		}
		if !allowOther && dstKind == kindOther {
			return NormalizedMapping{}, fmt.Errorf("unsupported target ref kind: %s (set --all-refs to allow arbitrary refs/* namespaces)", dst)
		}
		if srcKind != dstKind {
			return NormalizedMapping{}, fmt.Errorf("cross-kind mapping not allowed: %s (%s) -> %s (%s)", src, srcKind, dst, dstKind)
		}
		return NormalizedMapping{SourceRef: sourceRef, TargetRef: targetRef}, nil
	}

	if !srcFQ && !dstFQ {
		sourceRef := plumbing.NewBranchReferenceName(src)
		targetRef := plumbing.NewBranchReferenceName(dst)
		if err := validateRefName("source", sourceRef); err != nil {
			return NormalizedMapping{}, err
		}
		if err := validateRefName("target", targetRef); err != nil {
			return NormalizedMapping{}, err
		}
		return NormalizedMapping{SourceRef: sourceRef, TargetRef: targetRef}, nil
	}

	return NormalizedMapping{}, fmt.Errorf("ambiguous mapping: cannot mix fully-qualified and short ref names: %q -> %q", src, dst)
}

// ValidateMappings normalizes all mappings and rejects duplicate target refs.
func ValidateMappings(mappings []RefMapping, allowOther bool) ([]NormalizedMapping, error) {
	if len(mappings) == 0 {
		return nil, nil
	}

	normalized := make([]NormalizedMapping, 0, len(mappings))
	targetSeen := make(map[plumbing.ReferenceName]string, len(mappings))

	for _, m := range mappings {
		nm, err := NormalizeMapping(m, allowOther)
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

// validateRefName rejects a mapping ref name that git itself would not accept.
//
// A mapped target ref reaches the same places an advertised one does — written
// to disk by convert-sha256, embedded in receive-pack command lines — so a name
// containing "..", NUL, or control characters carries the same risk regardless
// of whether a remote or a caller supplied it. Unlike advertised names, which
// are skipped so one bad ref upstream cannot stop a mirror, a bad mapping is
// rejected outright: it is configuration, and failing at startup is more useful
// than silently not mirroring what was asked for.
func validateRefName(side string, name plumbing.ReferenceName) error {
	if err := name.Validate(); err != nil {
		return fmt.Errorf("invalid %s ref %q in mapping: %w", side, name.String(), err)
	}
	return nil
}

const (
	kindBranch = "branch"
	kindTag    = "tag"
	kindOther  = "other"
)

func refKind(name plumbing.ReferenceName) string {
	switch {
	case name.IsBranch():
		return kindBranch
	case name.IsTag():
		return kindTag
	case strings.HasPrefix(name.String(), "refs/"):
		return kindOther
	default:
		return ""
	}
}
