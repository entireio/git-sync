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
